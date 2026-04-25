// NETBIRD HA FORK - NEW FILE
// shared/distributed/redis.go
// Shared Redis client wrapper with health checks

package distributed

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

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

	var tlsConfig *tls.Config
	if cfg.TLSEnabled {
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:           cfg.RedisAddress,
		Password:       cfg.RedisPassword,
		DB:             cfg.RedisDB,
		DialTimeout:    cfg.DialTimeout,
		ReadTimeout:    cfg.ReadTimeout,
		WriteTimeout:   cfg.WriteTimeout,
		PoolSize:       cfg.PoolSize,
		MaxIdleConns:   cfg.MaxIdleConns,
		MinIdleConns:   cfg.MinIdleConns,
		ConnMaxLifetime: cfg.ConnMaxLifetime,
		TLSConfig:      tlsConfig,
	})

	ctx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed (%s): %w", cfg.RedisAddress, err)
	}

	return &Client{Client: rdb, config: cfg}, nil
}

// HealthCheck validates Redis connectivity by writing and reading back data.
func (c *Client) HealthCheck(ctx context.Context) error {
	return c.Echo(ctx, "health").Err()
}

// IsConnected provides a quick connectivity check with a 2-second timeout.
func (c *Client) IsConnected() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.Echo(ctx, "ping").Err() == nil
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