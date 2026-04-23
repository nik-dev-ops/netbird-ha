package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/netbirdio/netbird/version"
)

const (
	// ExitSetupFailed defines exit code
	ExitSetupFailed = 1
)

var (
	logLevel       string
	defaultLogFile string
	logFile        string

	// HA configuration flags
	haEnabled           bool
	haRedisAddress      string
	haRedisPassword     string
	haRedisDB           int
	haRedisDialTimeout  time.Duration
	haRedisReadTimeout  time.Duration
	haRedisWriteTimeout time.Duration
	haRedisPoolSize     int
	haInstanceID        string
	haRegistryKey       string
	haChannelPrefix     string
	haPeerTTL           time.Duration
	haHeartbeatInterval time.Duration
	haSendTimeout       time.Duration

	rootCmd = &cobra.Command{
		Use:     "netbird-signal",
		Short:   "",
		Long:    "",
		Version: version.NetbirdVersion(),
	}

	// Execution control channel for stopCh signal
	stopCh chan int
)

// Execute executes the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	stopCh = make(chan int)
	defaultLogFile = "/var/log/netbird/signal.log"

	if runtime.GOOS == "windows" {
		defaultLogFile = os.Getenv("PROGRAMDATA") + "\\Netbird\\" + "signal.log"
	}

	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", defaultLogFile, "sets Netbird log path. If console is specified the log will be output to stdout")

	// HA configuration flags
	rootCmd.PersistentFlags().BoolVar(&haEnabled, "ha-enabled", false, "enable high-availability mode for signal server")
	rootCmd.PersistentFlags().StringVar(&haRedisAddress, "ha-redis-address", "localhost:6379", "redis address for HA coordination")
	rootCmd.PersistentFlags().StringVar(&haRedisPassword, "ha-redis-password", "", "redis password for HA coordination")
	rootCmd.PersistentFlags().IntVar(&haRedisDB, "ha-redis-db", 0, "redis database number for HA coordination")
	rootCmd.PersistentFlags().DurationVar(&haRedisDialTimeout, "ha-redis-dial-timeout", 5*time.Second, "redis dial timeout")
	rootCmd.PersistentFlags().DurationVar(&haRedisReadTimeout, "ha-redis-read-timeout", 3*time.Second, "redis read timeout")
	rootCmd.PersistentFlags().DurationVar(&haRedisWriteTimeout, "ha-redis-write-timeout", 3*time.Second, "redis write timeout")
	rootCmd.PersistentFlags().IntVar(&haRedisPoolSize, "ha-redis-pool-size", 10, "redis connection pool size")
	rootCmd.PersistentFlags().StringVar(&haInstanceID, "ha-instance-id", "", "unique instance ID for HA mode (auto-detected if empty)")
	rootCmd.PersistentFlags().StringVar(&haRegistryKey, "signal-registry-key", "nb:signal:registry", "redis key for peer registry")
	rootCmd.PersistentFlags().StringVar(&haChannelPrefix, "signal-channel-prefix", "nb:signal:instance:", "redis channel prefix for instance messaging")
	rootCmd.PersistentFlags().DurationVar(&haPeerTTL, "signal-peer-ttl", 60*time.Second, "peer registry TTL in redis")
	rootCmd.PersistentFlags().DurationVar(&haHeartbeatInterval, "signal-heartbeat-interval", 30*time.Second, "peer heartbeat interval for redis registry")
	rootCmd.PersistentFlags().DurationVar(&haSendTimeout, "signal-send-timeout", 10*time.Second, "message send timeout")

	setFlagsFromEnvVars(rootCmd)
	rootCmd.AddCommand(runCmd)
}

// SetupCloseHandler handles SIGTERM signal and exits with success
func SetupCloseHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			fmt.Println("\r- Ctrl+C pressed in Terminal")
			stopCh <- 0
		}
	}()
}
