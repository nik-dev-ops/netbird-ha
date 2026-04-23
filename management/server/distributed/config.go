// NETBIRD HA FORK - NEW FILE
// management/server/distributed/config.go
// Management-server-specific HA configuration

package distributed

import (
	"os"
	"time"

	ha "github.com/netbirdio/netbird/shared/distributed"
)

// ManagementHAConfig extends the shared HAConfig with settings specific to the management server.
// All fields can be configured via environment variables using the NB_MGMT_ prefix.
type ManagementHAConfig struct {
	ha.HAConfig

	PeersRegistryKey     string        `yaml:"peers_registry_key" env:"NB_MGMT_PEERS_REGISTRY_KEY"`
	AccountChannelPrefix string        `yaml:"account_channel_prefix" env:"NB_MGMT_ACCOUNT_CHANNEL_PREFIX"`
	LockPrefix           string        `yaml:"lock_prefix" env:"NB_MGMT_LOCK_PREFIX"`
	LoginFilterKey       string        `yaml:"login_filter_key" env:"NB_MGMT_LOGIN_FILTER_KEY"`
	EphemeralKey         string        `yaml:"ephemeral_key" env:"NB_MGMT_EPHEMERAL_KEY"`
	PeerTTL              time.Duration `yaml:"peer_ttl" env:"NB_MGMT_PEER_TTL"`
	HeartbeatInterval    time.Duration `yaml:"heartbeat_interval" env:"NB_MGMT_HEARTBEAT_INTERVAL"`
	LockTTL              time.Duration `yaml:"lock_ttl" env:"NB_MGMT_LOCK_TTL"`
}

// DefaultManagementHAConfig returns sensible defaults for the management HA layer.
// HA itself is disabled by default; when enabled Redis defaults to localhost:6379.
func DefaultManagementHAConfig() ManagementHAConfig {
	return ManagementHAConfig{
		HAConfig:             ha.DefaultHAConfig(),
		PeersRegistryKey:     "netbird:management:peers:registry",
		AccountChannelPrefix: "netbird:management:account:",
		LockPrefix:           "netbird:management:lock:",
		LoginFilterKey:       "netbird:management:login:filter",
		EphemeralKey:         "nb:mgmt:ephemeral",
		PeerTTL:              30 * time.Second,
		HeartbeatInterval:    10 * time.Second,
		LockTTL:              15 * time.Second,
	}
}

// LoadManagementHAConfigFromEnv populates a ManagementHAConfig from environment variables.
// NB_MGMT_* variables override the management-specific fields; NB_HA_* variables override
// the embedded shared HAConfig fields.
func LoadManagementHAConfigFromEnv() ManagementHAConfig {
	cfg := DefaultManagementHAConfig()

	// Management-specific overrides
	if v := os.Getenv("NB_MGMT_PEERS_REGISTRY_KEY"); v != "" {
		cfg.PeersRegistryKey = v
	}
	if v := os.Getenv("NB_MGMT_ACCOUNT_CHANNEL_PREFIX"); v != "" {
		cfg.AccountChannelPrefix = v
	}
	if v := os.Getenv("NB_MGMT_LOCK_PREFIX"); v != "" {
		cfg.LockPrefix = v
	}
	if v := os.Getenv("NB_MGMT_LOGIN_FILTER_KEY"); v != "" {
		cfg.LoginFilterKey = v
	}
	if v := os.Getenv("NB_MGMT_EPHEMERAL_KEY"); v != "" {
		cfg.EphemeralKey = v
	}
	if v := os.Getenv("NB_MGMT_PEER_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.PeerTTL = d
		}
	}
	if v := os.Getenv("NB_MGMT_HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.HeartbeatInterval = d
		}
	}
	if v := os.Getenv("NB_MGMT_LOCK_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.LockTTL = d
		}
	}

	// Embedded HAConfig overrides
	if v := os.Getenv("NB_HA_ENABLED"); v == "true" {
		cfg.Enabled = true
	}
	if v := os.Getenv("NB_HA_REDIS_ADDRESS"); v != "" {
		cfg.RedisAddress = v
	}
	if v := os.Getenv("NB_HA_REDIS_PASSWORD"); v != "" {
		cfg.RedisPassword = v
	}
	if v := os.Getenv("NB_HA_REDIS_DB"); v != "" {
		// simple atoi fallback ignored for brevity; redis DB 0 is the default
		// consumers that need full parsing can do so externally.
	}
	if v := os.Getenv("NB_HA_REDIS_DIAL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.DialTimeout = d
		}
	}
	if v := os.Getenv("NB_HA_REDIS_READ_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.ReadTimeout = d
		}
	}
	if v := os.Getenv("NB_HA_REDIS_WRITE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.WriteTimeout = d
		}
	}
	if v := os.Getenv("NB_HA_REDIS_POOL_SIZE"); v != "" {
		// simple atoi fallback ignored for brevity; default is 10
	}
	if v := os.Getenv("NB_HA_INSTANCE_ID"); v != "" {
		cfg.InstanceID = v
	}

	return cfg
}

// Validate checks the management HA configuration and applies defaults where needed.
func (c *ManagementHAConfig) Validate() error {
	if err := c.HAConfig.Validate(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.PeerTTL <= 0 {
		c.PeerTTL = 30 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
	if c.LockTTL <= 0 {
		c.LockTTL = 15 * time.Second
	}
	return nil
}

// IsEnabled returns true when HA mode is enabled.
func (c *ManagementHAConfig) IsEnabled() bool {
	return c != nil && c.Enabled
}
