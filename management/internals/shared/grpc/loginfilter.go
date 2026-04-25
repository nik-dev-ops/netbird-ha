package grpc

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/shared/distributed"
	mgmtdistributed "github.com/netbirdio/netbird/management/server/distributed"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"
)

const (
	reconnThreshold   = 5 * time.Minute
	baseBlockDuration = 10 * time.Minute // Duration for which a peer is banned after exceeding the reconnection limit
	reconnLimitForBan = 30               // Number of reconnections within the reconnTreshold that triggers a ban
	metaChangeLimit   = 3                // Number of reconnections with different metadata that triggers a ban of one peer
)

type lfConfig struct {
	reconnThreshold   time.Duration
	baseBlockDuration time.Duration
	reconnLimitForBan int
	metaChangeLimit   int
}

func initCfg() *lfConfig {
	return &lfConfig{
		reconnThreshold:   reconnThreshold,
		baseBlockDuration: baseBlockDuration,
		reconnLimitForBan: reconnLimitForBan,
		metaChangeLimit:   metaChangeLimit,
	}
}

type loginFilter struct {
	mu        sync.RWMutex
	cfg       *lfConfig
	logged    map[string]*peerState
	redis     *distributed.Client
	key       string
	loginLock mgmtdistributed.Lock
	stopCh    chan struct{}
}

type peerState struct {
	currentHash           uint64
	sessionCounter        uint64
	sessionStart          time.Time
	lastSeen              time.Time
	isBanned              bool
	banLevel              int
	banExpiresAt          time.Time
	metaChangeCounter     int
	metaChangeWindowStart time.Time
}

func newLoginFilter() *loginFilter {
	return newLoginFilterWithCfg(initCfg())
}

func newLoginFilterWithCfg(cfg *lfConfig) *loginFilter {
	return &loginFilter{
		logged: make(map[string]*peerState),
		cfg:    cfg,
	}
}

func newLoginFilterWithRedis(redis *distributed.Client, key string, loginLock ...mgmtdistributed.Lock) *loginFilter {
	lf := &loginFilter{
		logged:    make(map[string]*peerState),
		cfg:       initCfg(),
		redis:     redis,
		key:       key,
		stopCh:    make(chan struct{}),
	}
	if len(loginLock) > 0 && loginLock[0] != nil {
		lf.loginLock = loginLock[0]
	}
	go lf.cleanupStaleEntries()
	return lf
}

// allowLoginUnderLock checks if login is allowed. Caller must hold l.mu.
func (l *loginFilter) allowLoginUnderLock(wgPubKey string, metaHash uint64) bool {
	state, ok := l.logged[wgPubKey]

	if !ok && l.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		data, err := l.redis.HGet(ctx, l.key, wgPubKey).Result()
		if err != nil {
			// Redis unavailable - fail closed for security
			log.Warnf("login filter: redis error, denying login for security: %v", err)
			return false
		}
		if data != "" {
			var redisState peerState
			if json.Unmarshal([]byte(data), &redisState) == nil {
				state = &redisState
				ok = true
				l.logged[wgPubKey] = state
			}
		}
	}

	if !ok {
		return true
	}
	if state.isBanned && time.Now().Before(state.banExpiresAt) {
		return false
	}
	if metaHash != state.currentHash {
		if time.Now().Before(state.metaChangeWindowStart.Add(l.cfg.reconnThreshold)) && state.metaChangeCounter >= l.cfg.metaChangeLimit {
			return false
		}
	}
	return true
}

// allowLogin is the public wrapper that acquires RLock. For HA mode, use CheckAndAddLogin instead.
func (l *loginFilter) allowLogin(wgPubKey string, metaHash uint64) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.allowLoginUnderLock(wgPubKey, metaHash)
}

// addLoginUnderLock adds a login record. Caller must hold l.mu.
func (l *loginFilter) addLoginUnderLock(wgPubKey string, metaHash uint64) {
	now := time.Now()

	state, ok := l.logged[wgPubKey]

	if !ok {
		state = &peerState{
			currentHash:           metaHash,
			sessionCounter:        1,
			sessionStart:          now,
			lastSeen:              now,
			metaChangeWindowStart: now,
			metaChangeCounter:     1,
		}
		l.logged[wgPubKey] = state
		l.syncToRedis(wgPubKey, state)
		return
	}

	if state.isBanned && now.After(state.banExpiresAt) {
		state.isBanned = false
	}

	if state.banLevel > 0 && now.Sub(state.lastSeen) > (2*l.cfg.baseBlockDuration) {
		state.banLevel = 0
	}

	if metaHash != state.currentHash {
		if now.After(state.metaChangeWindowStart.Add(l.cfg.reconnThreshold)) {
			state.metaChangeWindowStart = now
			state.metaChangeCounter = 1
		} else {
			state.metaChangeCounter++
		}
		state.currentHash = metaHash
		state.sessionCounter = 1
		state.sessionStart = now
		state.lastSeen = now
		l.syncToRedis(wgPubKey, state)
		return
	}

	if state.sessionCounter > math.MaxUint64-1000 {
		state.sessionCounter = 0
	}
	state.sessionCounter++
	if state.sessionCounter > uint64(l.cfg.reconnLimitForBan) && now.Sub(state.sessionStart) < l.cfg.reconnThreshold {
		state.isBanned = true
		state.banLevel++

		backoffFactor := math.Pow(2, float64(state.banLevel-1))
		duration := time.Duration(float64(l.cfg.baseBlockDuration) * backoffFactor)
		state.banExpiresAt = now.Add(duration)

		state.sessionCounter = 0
		state.sessionStart = now
	}
	state.lastSeen = now
	l.syncToRedis(wgPubKey, state)
}

// addLogin is the public wrapper that acquires Lock. For HA mode, use CheckAndAddLogin instead.
func (l *loginFilter) addLogin(wgPubKey string, metaHash uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addLoginUnderLock(wgPubKey, metaHash)
}

// CheckAndAddLogin acquires a distributed lock, then holds the write lock while calling
// allowLoginUnderLock and addLoginUnderLock atomically. This prevents TOCTOU races where
// concurrent logins for the same peer could bypass limits. Returns true if login is allowed,
// false otherwise. If the lock cannot be acquired, returns false (fail closed for security).
func (l *loginFilter) CheckAndAddLogin(ctx context.Context, wgPubKey string, metaHash uint64) bool {
	if l.loginLock == nil {
		// H4: In production HA deployments with Redis but no loginLock configured,
		// login filtering may not be safe. Warn but allow for backward compatibility.
		if l.redis != nil {
			log.Warnf("login filter: loginLock not configured in HA mode with Redis - "+
				"login filtering may not be safe for peer %s. Consider configuring a distributed lock.", wgPubKey)
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.allowLoginUnderLock(wgPubKey, metaHash) {
			return false
		}
		l.addLoginUnderLock(wgPubKey, metaHash)
		return true
	}

	unlock, err := l.loginLock.Acquire(ctx, "loginfilter:"+wgPubKey, 5*time.Second)
	if err != nil {
		// Couldn't acquire lock - fail closed for security
		log.Warnf("login filter: lock unavailable for %s, denying login: %v", wgPubKey, err)
		return false
	}
	defer func() {
		if err := unlock(); err != nil {
			log.Warnf("login filter: failed to release lock for %s: %v", wgPubKey, err)
		}
	}()

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.allowLoginUnderLock(wgPubKey, metaHash) {
		return false
	}
	l.addLoginUnderLock(wgPubKey, metaHash)
	return true
}

func (l *loginFilter) syncToRedis(wgPubKey string, state *peerState) {
	if l.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	l.redis.HSet(ctx, l.key, wgPubKey, string(data))
	l.redis.Expire(ctx, l.key, 2*l.cfg.baseBlockDuration)
}

func (l *loginFilter) cleanupStaleEntries() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.mu.Lock()
			now := time.Now()
			staleThreshold := 2 * l.cfg.baseBlockDuration
			for wgPubKey, state := range l.logged {
				if state.isBanned && now.Before(state.banExpiresAt) {
					continue
				}
				if now.Sub(state.lastSeen) > staleThreshold {
					delete(l.logged, wgPubKey)
				}
			}
			l.mu.Unlock()
		case <-l.stopCh:
			return
		}
	}
}

func metaHash(meta nbpeer.PeerSystemMeta, pubip string) uint64 {
	h := fnv.New64a()

	h.Write([]byte(meta.WtVersion))
	h.Write([]byte(meta.OSVersion))
	h.Write([]byte(meta.KernelVersion))
	h.Write([]byte(meta.Hostname))
	h.Write([]byte(meta.SystemSerialNumber))
	h.Write([]byte(pubip))

	macs := uint64(0)
	for _, na := range meta.NetworkAddresses {
		for _, r := range na.Mac {
			macs += uint64(r)
		}
	}

	return h.Sum64() + macs
}
