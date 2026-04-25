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
}

type peerState struct {
	currentHash           uint64
	sessionCounter        int
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
	}
	if len(loginLock) > 0 && loginLock[0] != nil {
		lf.loginLock = loginLock[0]
	}
	return lf
}

func (l *loginFilter) allowLogin(wgPubKey string, metaHash uint64) bool {
	l.mu.RLock()
	state, ok := l.logged[wgPubKey]
	l.mu.RUnlock()

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
				l.mu.Lock()
				l.logged[wgPubKey] = state
				l.mu.Unlock()
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

func (l *loginFilter) addLogin(wgPubKey string, metaHash uint64) {
	now := time.Now()
	l.mu.Lock()
	defer func() {
		l.mu.Unlock()
	}()

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

	if state.sessionCounter > math.MaxInt-1000 {
		state.sessionCounter = 0
	}
	state.sessionCounter++
	if state.sessionCounter > l.cfg.reconnLimitForBan && now.Sub(state.sessionStart) < l.cfg.reconnThreshold {
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

// CheckAndAddLogin acquires a distributed lock, calls allowLogin, then addLogin atomically.
// This prevents TOCTOU races where concurrent logins for the same peer could bypass limits.
// Returns true if login is allowed (allowLogin returned true), false otherwise.
// If the lock cannot be acquired, returns false (fail closed for security).
func (l *loginFilter) CheckAndAddLogin(ctx context.Context, wgPubKey string, metaHash uint64) bool {
	if l.loginLock == nil {
		// No lock configured - fall back to original behavior (not HA-safe)
		if !l.allowLogin(wgPubKey, metaHash) {
			return false
		}
		l.addLogin(wgPubKey, metaHash)
		return true
	}

	unlock, err := l.loginLock.Acquire(ctx, "loginfilter:"+wgPubKey, 5*time.Second)
	if err != nil {
		// Couldn't acquire lock - fail closed for security
		log.Warnf("login filter: lock unavailable for %s, denying login: %v", wgPubKey, err)
		return false
	}
	defer unlock()

	// Now do the allowLogin + addLogin under lock
	if !l.allowLogin(wgPubKey, metaHash) {
		return false
	}
	l.addLogin(wgPubKey, metaHash)
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
