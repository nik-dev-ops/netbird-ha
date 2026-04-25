// NETBIRD HA FORK - NEW FILE
// management/server/distributed/registry.go
// Peer-to-instance registry for routing requests in HA mode

package distributed

import (
	"context"
	"fmt"
	"time"

	ha "github.com/netbirdio/netbird/shared/distributed"
)

// Registry maps peers to the management instance that currently handles them.
type Registry interface {
	RegisterPeer(ctx context.Context, peerID, instanceID string, ttl time.Duration) error
	DeregisterPeer(ctx context.Context, peerID string) error
	GetPeerInstance(ctx context.Context, peerID string) (string, error)
}

// RedisRegistry implements Registry using Redis hashes.
type RedisRegistry struct {
	client *ha.Client
	config ManagementHAConfig
}

// NewRedisRegistry creates a new registry backed by Redis.
func NewRedisRegistry(client *ha.Client, config ManagementHAConfig) *RedisRegistry {
	return &RedisRegistry{
		client: client,
		config: config,
	}
}

// peersRegistryKey returns the Redis key used for the peer registry hash.
func (r *RedisRegistry) peersRegistryKey() string {
	if r.config.PeersRegistryKey != "" {
		return r.config.PeersRegistryKey
	}
	return "netbird:management:peers:registry"
}

// RegisterPeer records that the given peer is handled by instanceID.
func (r *RedisRegistry) RegisterPeer(ctx context.Context, peerID, instanceID string, ttl time.Duration) error {
	key := r.peersRegistryKey()

	err := r.client.HSet(ctx, key, peerID, instanceID).Err()
	if err != nil {
		return fmt.Errorf("failed to register peer %s: %w", peerID, err)
	}

	// Refresh TTL so the whole registry expires if no heartbeats occur.
	err = r.client.Expire(ctx, key, ttl).Err()
	if err != nil {
		return fmt.Errorf("failed to set peer registry TTL: %w", err)
	}

	return nil
}

// DeregisterPeer removes the peer from the registry.
func (r *RedisRegistry) DeregisterPeer(ctx context.Context, peerID string) error {
	key := r.peersRegistryKey()

	err := r.client.HDel(ctx, key, peerID).Err()
	if err != nil {
		return fmt.Errorf("failed to deregister peer %s: %w", peerID, err)
	}

	return nil
}

// GetPeerInstance returns the instance ID that currently handles the peer.
func (r *RedisRegistry) GetPeerInstance(ctx context.Context, peerID string) (string, error) {
	key := r.peersRegistryKey()

	instanceID, err := r.client.HGet(ctx, key, peerID).Result()
	if err != nil {
		return "", fmt.Errorf("failed to get peer instance for %s: %w", peerID, err)
	}

	return instanceID, nil
}
