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

	stopHeartbeat := make(chan struct{})
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
			case <-stopHeartbeat:
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

		close(stopHeartbeat)
		wg.Wait()

		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		val, err := l.client.Get(bgCtx, key).Result()
		if err == nil && val == value {
			l.client.Del(bgCtx, key)
		}
	}

	return release, nil
}
