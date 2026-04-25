package manager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/management/internals/modules/peers"
	"github.com/netbirdio/netbird/management/internals/modules/peers/ephemeral"
	"github.com/netbirdio/netbird/management/server/activity"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"

	"github.com/netbirdio/netbird/management/server/store"
	"github.com/netbirdio/netbird/shared/distributed"
)

const (
	cleanupWindow    = 1 * time.Minute
	redisPollInterval = 1 * time.Minute
	memberSeparator  = "|"
)

var (
	timeNow = time.Now
)

type ephemeralPeer struct {
	id        string
	accountID string
	deadline  time.Time
	next      *ephemeralPeer
}

// todo: consider to remove peer from ephemeral list when the peer has been deleted via API. If we do not do it
// in worst case we will get invalid error message in this manager.

// EphemeralManager keep a list of ephemeral peers. After EphemeralLifeTime inactivity the peer will be deleted
// automatically. Inactivity means the peer disconnected from the Management server.
type EphemeralManager struct {
	store        store.Store
	peersManager peers.Manager

	headPeer  *ephemeralPeer
	tailPeer  *ephemeralPeer
	peersLock sync.Mutex
	timer     *time.Timer

	lifeTime      time.Duration
	cleanupWindow time.Duration

	// HA mode fields
	redisClient  *distributed.Client
	ephemeralKey string
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// NewEphemeralManager instantiate new EphemeralManager
func NewEphemeralManager(store store.Store, peersManager peers.Manager) *EphemeralManager {
	return &EphemeralManager{
		store:        store,
		peersManager: peersManager,

		lifeTime:      ephemeral.EphemeralLifeTime,
		cleanupWindow: cleanupWindow,
	}
}

// WithRedis enables Redis-backed ephemeral peer tracking for HA mode.
func (e *EphemeralManager) WithRedis(client *distributed.Client, ephemeralKey string) *EphemeralManager {
	e.redisClient = client
	e.ephemeralKey = ephemeralKey
	return e
}

func (e *EphemeralManager) haEnabled() bool {
	return e.redisClient != nil && e.ephemeralKey != ""
}

// LoadInitialPeers load from the database the ephemeral type of peers and schedule a cleanup procedure to the head
// of the linked list (to the most deprecated peer). At the end of cleanup it schedules the next cleanup to the new
// head.
func (e *EphemeralManager) LoadInitialPeers(ctx context.Context) {
	e.peersLock.Lock()
	defer e.peersLock.Unlock()

	e.loadEphemeralPeers(ctx)
	if e.haEnabled() {
		// Sync loaded peers to Redis ZSET
		for p := e.headPeer; p != nil; p = p.next {
			e.redisZAdd(ctx, p.id, p.accountID, p.deadline)
		}
		// Start background polling goroutine for Redis-backed cleanup
		pollCtx, cancel := context.WithCancel(context.Background())
		e.cancel = cancel
		e.wg.Add(1)
		go e.redisPollLoop(pollCtx)
	} else if e.headPeer != nil {
		e.timer = time.AfterFunc(e.lifeTime, func() {
			e.cleanup(ctx)
		})
	}
}

// Stop timer and background goroutines
func (e *EphemeralManager) Stop() {
	e.peersLock.Lock()
	if e.timer != nil {
		e.timer.Stop()
	}
	e.peersLock.Unlock()

	if e.cancel != nil {
		e.cancel()
		e.wg.Wait()
	}
}

// OnPeerConnected remove the peer from the linked list of ephemeral peers. Because it has been called when the peer
// is active the manager will not delete it while it is active.
func (e *EphemeralManager) OnPeerConnected(ctx context.Context, peer *nbpeer.Peer) {
	if !peer.Ephemeral {
		return
	}

	log.WithContext(ctx).Tracef("remove peer from ephemeral list: %s", peer.ID)

	e.peersLock.Lock()
	defer e.peersLock.Unlock()

	e.removePeer(peer.ID)

	if e.haEnabled() {
		e.redisZRem(ctx, peer.ID, peer.AccountID)
	}

	// stop the unnecessary timer
	if e.headPeer == nil && e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
}

// OnPeerDisconnected add the peer to the linked list of ephemeral peers. Because of the peer
// is inactive it will be deleted after the EphemeralLifeTime period.
func (e *EphemeralManager) OnPeerDisconnected(ctx context.Context, peer *nbpeer.Peer) {
	if !peer.Ephemeral {
		return
	}

	log.WithContext(ctx).Tracef("add peer to ephemeral list: %s", peer.ID)

	e.peersLock.Lock()
	defer e.peersLock.Unlock()

	if e.isPeerOnList(peer.ID) {
		return
	}

	deadline := e.newDeadLine()
	e.addPeer(peer.AccountID, peer.ID, deadline)
	if e.haEnabled() {
		e.redisZAdd(ctx, peer.ID, peer.AccountID, deadline)
	}

	if !e.haEnabled() && e.timer == nil {
		delay := e.headPeer.deadline.Sub(timeNow()) + e.cleanupWindow
		if delay < 0 {
			delay = 0
		}
		e.timer = time.AfterFunc(delay, func() {
			e.cleanup(ctx)
		})
	}
}

func (e *EphemeralManager) loadEphemeralPeers(ctx context.Context) {
	peers, err := e.store.GetAllEphemeralPeers(ctx, store.LockingStrengthNone)
	if err != nil {
		log.WithContext(ctx).Debugf("failed to load ephemeral peers: %s", err)
		return
	}

	t := e.newDeadLine()
	for _, p := range peers {
		e.addPeer(p.AccountID, p.ID, t)
	}

	log.WithContext(ctx).Debugf("loaded ephemeral peer(s): %d", len(peers))
}

func (e *EphemeralManager) cleanup(ctx context.Context) {
	log.Tracef("on ephemeral cleanup")
	deletePeers := make(map[string]*ephemeralPeer)

	e.peersLock.Lock()
	now := timeNow()
	for p := e.headPeer; p != nil; p = p.next {
		if now.Before(p.deadline) {
			break
		}

		deletePeers[p.id] = p
		e.headPeer = p.next
		if p.next == nil {
			e.tailPeer = nil
		}
	}

	if e.headPeer != nil {
		delay := e.headPeer.deadline.Sub(timeNow()) + e.cleanupWindow
		if delay < 0 {
			delay = 0
		}
		e.timer = time.AfterFunc(delay, func() {
			e.cleanup(ctx)
		})
	} else {
		e.timer = nil
	}

	e.peersLock.Unlock()

	peerIDsPerAccount := make(map[string][]string)
	for id, p := range deletePeers {
		peerIDsPerAccount[p.accountID] = append(peerIDsPerAccount[p.accountID], id)
	}

	for accountID, peerIDs := range peerIDsPerAccount {
		log.WithContext(ctx).Tracef("cleanup: deleting %d ephemeral peers for account %s", len(peerIDs), accountID)
		err := e.peersManager.DeletePeers(ctx, accountID, peerIDs, activity.SystemInitiator, true)
		if err != nil {
			log.WithContext(ctx).Errorf("failed to delete ephemeral peers: %s", err)
		}
	}
}

func (e *EphemeralManager) addPeer(accountID string, peerID string, deadline time.Time) {
	ep := &ephemeralPeer{
		id:        peerID,
		accountID: accountID,
		deadline:  deadline,
	}

	if e.headPeer == nil {
		e.headPeer = ep
	}
	if e.tailPeer != nil {
		e.tailPeer.next = ep
	}
	e.tailPeer = ep
}

func (e *EphemeralManager) removePeer(id string) {
	if e.headPeer == nil {
		return
	}

	if e.headPeer.id == id {
		e.headPeer = e.headPeer.next
		if e.tailPeer.id == id {
			e.tailPeer = nil
		}
		return
	}

	for p := e.headPeer; p.next != nil; p = p.next {
		if p.next.id == id {
			// if we remove the last element from the chain then set the last-1 as tail
			if e.tailPeer.id == id {
				e.tailPeer = p
			}
			p.next = p.next.next
			return
		}
	}
}

func (e *EphemeralManager) isPeerOnList(id string) bool {
	for p := e.headPeer; p != nil; p = p.next {
		if p.id == id {
			return true
		}
	}
	return false
}

func (e *EphemeralManager) newDeadLine() time.Time {
	return timeNow().Add(e.lifeTime)
}

func (e *EphemeralManager) redisZAdd(ctx context.Context, peerID, accountID string, deadline time.Time) {
	member := peerID + memberSeparator + accountID
	err := e.redisClient.ZAdd(ctx, e.ephemeralKey, redis.Z{Score: float64(deadline.UnixMilli()), Member: member}).Err()
	if err != nil {
		log.WithContext(ctx).Errorf("failed to ZADD ephemeral peer %s: %v", member, err)
	}
}

func (e *EphemeralManager) redisZRem(ctx context.Context, peerID, accountID string) {
	member := peerID + memberSeparator + accountID
	err := e.redisClient.ZRem(ctx, e.ephemeralKey, member).Err()
	if err != nil {
		log.WithContext(ctx).Errorf("failed to ZREM ephemeral peer %s: %v", member, err)
	}
}

func (e *EphemeralManager) redisPollLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(redisPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.redisCleanup(ctx)
		}
	}
}

func (e *EphemeralManager) redisCleanup(ctx context.Context) {
	const maxRetries = 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		now := timeNow().UnixMilli()

		err := e.redisClient.Watch(ctx, func(tx *redis.Tx) error {
			members, err := tx.ZRangeByScore(ctx, e.ephemeralKey, &redis.ZRangeBy{
				Min: "0",
				Max: fmt.Sprintf("%d", now),
			}).Result()
			if err != nil {
				return err
			}

			if len(members) == 0 {
				return nil
			}

			peerIDsPerAccount := make(map[string][]string)
			validMembers := make([]string, 0, len(members))
			for _, member := range members {
				parts := strings.SplitN(member, memberSeparator, 2)
				if len(parts) != 2 {
					log.WithContext(ctx).Warnf("invalid ephemeral peer member format: %s", member)
					continue
				}
				peerID, accountID := parts[0], parts[1]
				peerIDsPerAccount[accountID] = append(peerIDsPerAccount[accountID], peerID)
				validMembers = append(validMembers, member)
			}

			if len(validMembers) == 0 {
				return nil
			}

			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, member := range validMembers {
					pipe.ZRem(ctx, e.ephemeralKey, member)
				}
				return nil
			})
			if err != nil {
				return err
			}

			e.peersLock.Lock()
			for _, member := range validMembers {
				parts := strings.SplitN(member, memberSeparator, 2)
				if len(parts) == 2 {
					e.removePeer(parts[0])
				}
			}
			e.peersLock.Unlock()

			for accountID, peerIDs := range peerIDsPerAccount {
				log.WithContext(ctx).Tracef("cleanup: deleting %d ephemeral peers for account %s", len(peerIDs), accountID)
				if err := e.peersManager.DeletePeers(ctx, accountID, peerIDs, activity.SystemInitiator, true); err != nil {
					log.WithContext(ctx).Errorf("failed to delete ephemeral peers: %s", err)
				}
			}

			return nil
		}, e.ephemeralKey)

		if err == nil {
			return
		}

		if attempt == maxRetries-1 {
			log.WithContext(ctx).Errorf("failed to cleanup ephemeral peers after %d attempts: %v", maxRetries, err)
			return
		}

		log.WithContext(ctx).Debugf("ephemeral peers cleanup watch triggered, retrying (attempt %d/%d)", attempt+1, maxRetries)
	}
}
