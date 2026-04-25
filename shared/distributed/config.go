// NETBIRD HA FORK - NEW FILE
// shared/distributed/config.go
// Shared HA configuration for Signal and Management servers

package distributed

import (
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"os"
	"time"
)

// HAConfig holds common configuration for distributed HA mode.
// All fields can be set via environment variables or YAML.
// No hardcoded values - everything is externally configurable.
type HAConfig struct {
	Enabled           bool          `yaml:"enabled" env:"NB_HA_ENABLED"`
	RedisAddress      string        `yaml:"redis_address" env:"NB_HA_REDIS_ADDRESS"`
	RedisPassword     string        `yaml:"redis_password" env:"NB_HA_REDIS_PASSWORD"`
	RedisDB           int           `yaml:"redis_db" env:"NB_HA_REDIS_DB"`
	DialTimeout       time.Duration `yaml:"dial_timeout" env:"NB_HA_REDIS_DIAL_TIMEOUT"`
	ReadTimeout       time.Duration `yaml:"read_timeout" env:"NB_HA_REDIS_READ_TIMEOUT"`
	WriteTimeout      time.Duration `yaml:"write_timeout" env:"NB_HA_REDIS_WRITE_TIMEOUT"`
	PoolSize          int           `yaml:"pool_size" env:"NB_HA_REDIS_POOL_SIZE"`
	TLSConfig         *tls.Config   `yaml:"-" env:"-"`
	InstanceID        string        `yaml:"instance_id" env:"NB_HA_INSTANCE_ID"`
}

// DefaultHAConfig returns sensible defaults.
// HA is disabled by default to maintain backward compatibility.
func DefaultHAConfig() HAConfig {
	return HAConfig{
		Enabled:      false,
		RedisAddress: "localhost:6379",
		RedisDB:      0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     10,
		InstanceID:   "",
	}
}

// DetectInstanceID returns a unique instance identifier.
// Priority: config value > NB_HA_INSTANCE_ID env var > generated UUID.
func DetectInstanceID(cfgValue string) string {
	if cfgValue != "" {
		return cfgValue
	}
	if v := os.Getenv("NB_HA_INSTANCE_ID"); v != "" {
		return v
	}
	return generateUUID()
}

func generateUUID() string {
	// Use timestamp-based fallback if uuid generation fails
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return fmt.Sprintf("auto-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant is 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Validate checks that the config is coherent.
// When HA is disabled, validation always passes.
func (c *HAConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.RedisAddress == "" {
		return fmt.Errorf("redis_address is required when HA is enabled")
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 3 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 3 * time.Second
	}
	if c.PoolSize <= 0 {
		c.PoolSize = 10
	}
	c.InstanceID = DetectInstanceID(c.InstanceID)
	return nil
}

// IsEnabled returns true if HA mode is enabled.
func (c *HAConfig) IsEnabled() bool {
	return c != nil && c.Enabled
}