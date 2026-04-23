// NETBIRD HA FORK - NEW FILE
// shared/distributed/redis.go
// Shared Redis client wrapper with health checks

package distributed

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Client wraps go-redis with health checks and configuration.
type Client struct {
	*redis.Client
	config HAConfig
}

// NewClient creates a Redis client from HA config.
// Returns error if HA is not enabled or Redis is unreachable.
func NewClient(cfg HAConfig) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("HA is not enabled")
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid HA config: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddress,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		PoolSize:     cfg.PoolSize,
	})

	ctx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed (%s): %w", cfg.RedisAddress, err)
	}

	return &Client{Client: rdb, config: cfg}, nil
}

// HealthCheck returns nil if Redis is reachable.
func (c *Client) HealthCheck(ctx context.Context) error {
	return c.Ping(ctx).Err()
}

// Close gracefully shuts down the client.
func (c *Client) Close() error {
	return c.Client.Close()
}

// InstanceID returns the configured instance ID.
func (c *Client) InstanceID() string {
	return c.config.InstanceID
}

// Config returns the HA configuration.
func (c *Client) Config() HAConfig {
	return c.config
}

// WithTimeout creates a context with the configured dial timeout.
func (c *Client) WithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.config.DialTimeout)
}