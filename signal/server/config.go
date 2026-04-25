// NETBIRD HA FORK - NEW FILE
// signal/server/config.go
// Signal-specific HA configuration

package server

import (
	"time"

	"github.com/netbirdio/netbird/shared/distributed"
)

// SignalHAConfig extends HAConfig with signal-specific parameters.
// All fields can be set via environment variables or YAML.
type SignalHAConfig struct {
	distributed.HAConfig `yaml:",inline"`

	RegistryKey       string        `yaml:"registry_key" env:"NB_SIGNAL_REGISTRY_KEY"`
	ChannelPrefix     string        `yaml:"channel_prefix" env:"NB_SIGNAL_CHANNEL_PREFIX"`
	PeerTTL           time.Duration `yaml:"peer_ttl" env:"NB_SIGNAL_PEER_TTL"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval" env:"NB_SIGNAL_HEARTBEAT_INTERVAL"`
	SendTimeout       time.Duration `yaml:"send_timeout" env:"NB_SIGNAL_SEND_TIMEOUT"`
}

// DefaultSignalHAConfig returns signal-specific defaults.
// Inherits defaults from distributed.HAConfig.
func DefaultSignalHAConfig() SignalHAConfig {
	return SignalHAConfig{
		HAConfig:          distributed.DefaultHAConfig(),
		RegistryKey:       "nb:signal:registry",
		ChannelPrefix:     "nb:signal:instance:",
		PeerTTL:           30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		SendTimeout:       10 * time.Second,
	}
}

// Validate checks signal HA config and applies defaults.
// When HA is disabled, validation always passes.
func (c *SignalHAConfig) Validate() error {
	if err := c.HAConfig.Validate(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.RegistryKey == "" {
		c.RegistryKey = "nb:signal:registry"
	}
	if c.ChannelPrefix == "" {
		c.ChannelPrefix = "nb:signal:instance:"
	}
	if c.PeerTTL <= 0 {
		c.PeerTTL = 30 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = 10 * time.Second
	}
	return nil
}