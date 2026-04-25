// NETBIRD HA FORK - NEW FILE
// management/server/distributed/lock.go
// Distributed locking primitives for the management server HA mode

package distributed

import (
	"context"
	"fmt"
	"sync"
	"time"

	ha "github.com/netbirdio/netbird/shared/distributed"
	log "github.com/sirupsen/logrus"
)

// Lock provides distributed mutual exclusion.
type Lock interface {
	Acquire(ctx context.Context, resource string, ttl time.Duration) (release func(), err error)
}

// RedisLock implements Lock using Redis SET ... NX EX with a background heartbeat.
type RedisLock struct {
	client     *ha.Client
	instanceID string
}

// NewRedisLock creates a lock backed by the given Redis client.
func NewRedisLock(client *ha.Client) *RedisLock {
	return &RedisLock{
		client:     client,
		instanceID: client.InstanceID(),
	}
}

// Lua script for atomic check-and-delete unlock.
// Returns 1 if the lock was deleted, 0 if it wasn't held by this instance.
const unlockScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
else
    return 0
end
`

// Acquire attempts to acquire a distributed lock for the given resource.
// On success it returns a release function that MUST be called to free the lock.
// A background goroutine extends the lock TTL every ttl/3 until released.
func (l *RedisLock) Acquire(ctx context.Context, resource string, ttl time.Duration) (release func(), err error) {
	key := fmt.Sprintf("lock:%s", resource)
	value := l.instanceID

	ok, err := l.client.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redis lock acquire failed for %s: %w", resource, err)
	}
	if !ok {
		return nil, fmt.Errorf("lock already held: %s", resource)
	}

	// Use context-based cancellation instead of channel-based stop to avoid goroutine leak.
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := l.client.Expire(bgCtx, key, ttl).Err()
				cancel()
				if err != nil {
					// Heartbeat failed; lock will eventually expire.
					return
				}
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()

	released := false
	var releaseMu sync.Mutex

	release = func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true

		cancelHeartbeat()
		wg.Wait()

		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Use Lua script for atomic check-and-delete to prevent race condition
		// where another instance acquires the lock between GET and DEL.
		result, err := l.client.Eval(bgCtx, unlockScript, []string{key}, value).Int()
		if err != nil {
			log.Errorf("failed to release distributed lock %s: %v", resource, err)
			return
		}
		if result == 0 {
			log.Warnf("distributed lock %s was not held by this instance", resource)
		}
	}

	return release, nil
}
