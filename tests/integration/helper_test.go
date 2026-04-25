package integration

import (
	"context"
	"crypto/sha256"
	b64 "encoding/base64"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-secure-stdlib/base62"
	"github.com/redis/go-redis/v9"
	"github.com/rs/xid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	mgmtproto "github.com/netbirdio/netbird/shared/management/proto"
	signalproto "github.com/netbirdio/netbird/shared/signal/proto"
)

// Environment-based configuration for the HA test environment.
// Defaults use localhost with exposed host ports so tests can run from the host.
// When running inside the test-runner container, set env vars to use Docker hostnames.
var (
	signal1Addr       = getEnv("SIGNAL1_ADDR", "localhost:10000")
	signal2Addr       = getEnv("SIGNAL2_ADDR", "localhost:10001")
	signalTraefikAddr = getEnv("SIGNAL_TRAEFIK_ADDR", "localhost:8088")
	mgmt1Addr         = getEnv("MGMT1_ADDR", "localhost:33073")
	mgmt2Addr         = getEnv("MGMT2_ADDR", "localhost:33074")
	mgmt1MetricsAddr  = getEnv("MGMT1_METRICS", "localhost:9091")
	mgmt2MetricsAddr  = getEnv("MGMT2_METRICS", "localhost:9092")
	mgmtTraefikAddr   = getEnv("MGMT_TRAEFIK_ADDR", "localhost:8088")
	redisAddr         = getEnv("REDIS_ADDR", "localhost:6379")
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newRedisClient creates a Redis client for verification.
func newRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	opt, err := redis.ParseURL(fmt.Sprintf("redis://%s", redisAddr))
	require.NoError(t, err)
	return redis.NewClient(opt)
}

// signalClient connects to a signal server gRPC endpoint.
func signalClient(t *testing.T, addr string) signalproto.SignalExchangeClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return signalproto.NewSignalExchangeClient(conn)
}

// signalClientTraefik connects to the signal service through the Traefik load balancer.
func signalClientTraefik(t *testing.T) signalproto.SignalExchangeClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Traefik routes gRPC based on path prefix; use insecure credentials since it's internal.
	conn, err := grpc.DialContext(ctx, signalTraefikAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithAuthority("signal.nb-ha.local"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return signalproto.NewSignalExchangeClient(conn)
}

// mgmtClientTraefik connects to the management gRPC service through Traefik.
func mgmtClientTraefik(t *testing.T) mgmtproto.ManagementServiceClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, mgmtTraefikAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithAuthority("mgmt.nb-ha.local"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return mgmtproto.NewManagementServiceClient(conn)
}

// connectSignalStream registers a peer on a signal server and returns the stream.
// The stream must be consumed in a goroutine to avoid blocking.
func connectSignalStream(t *testing.T, client signalproto.SignalExchangeClient, peerID string) signalproto.SignalExchange_ConnectStreamClient {
	t.Helper()
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(signalproto.HeaderId, peerID))
	stream, err := client.ConnectStream(ctx)
	require.NoError(t, err)
	// Wait for registration header confirmation.
	_, err = stream.Header()
	require.NoError(t, err)
	return stream
}

// waitForRedisHashField waits until a Redis hash field has the expected value.
func waitForRedisHashField(t *testing.T, rdb *redis.Client, key, field, expected string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		val, err := rdb.HGet(ctx, key, field).Result()
		if err == nil && val == expected {
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
			continue
		case <-ctx.Done():
			t.Fatalf("timeout waiting for Redis HSET %s[%s] = %s", key, field, expected)
		}
	}
}

// requireDocker skips the test if the Docker CLI is not available.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI not available; skipping container-based test")
	}
}

// dockerStop stops a container by name.
func dockerStop(t *testing.T, container string) {
	t.Helper()
	requireDocker(t)
	out, err := exec.Command("docker", "stop", container).CombinedOutput()
	if err != nil {
		t.Logf("docker stop output: %s", string(out))
	}
	require.NoError(t, err)
}

// dockerStart starts a container by name.
func dockerStart(t *testing.T, container string) {
	t.Helper()
	requireDocker(t)
	out, err := exec.Command("docker", "start", container).CombinedOutput()
	if err != nil {
		t.Logf("docker start output: %s", string(out))
	}
	require.NoError(t, err)
}

// createTestPAT generates a valid personal access token and inserts it into
// the database for the given user, returning the plain token for API auth.
func createTestPAT(t *testing.T, userID string) string {
	t.Helper()

	const patPrefix = "nbp_"
	const patSecretLength = 30
	const patChecksumLength = 6
	const patLength = 40

	// Generate random base62 secret.
	secret, err := base62.Random(patSecretLength)
	require.NoError(t, err)

	// Compute CRC32 checksum and encode as base62.
	checksum := crc32.ChecksumIEEE([]byte(secret))
	encodedChecksum := encodeBase62(checksum)
	paddedChecksum := fmt.Sprintf("%06s", encodedChecksum)

	plainToken := patPrefix + secret + paddedChecksum
	require.Len(t, plainToken, patLength, "generated PAT should be exactly %d chars", patLength)

	hash := sha256.Sum256([]byte(plainToken))
	hashedToken := b64.StdEncoding.EncodeToString(hash[:])

	t.Logf("Generated PAT: %s (len=%d), hash: %s", plainToken, len(plainToken), hashedToken)

	// Insert directly into Postgres using psql.
	query := fmt.Sprintf(
		`INSERT INTO personal_access_tokens (id, user_id, name, hashed_token, expiration_date, created_by, created_at, last_used)
		 VALUES ('%s', '%s', 'integration-test', '%s', NOW() + INTERVAL '1 day', '%s', NOW(), NOW())
		 ON CONFLICT (id) DO UPDATE SET hashed_token = EXCLUDED.hashed_token;`,
		xid.New().String(), userID, hashedToken, userID,
	)
	out, err := exec.Command("docker", "exec", "nb-postgres", "psql", "-U", "netbird", "-d", "netbird", "-c", query).CombinedOutput()
	if err != nil {
		t.Logf("PAT insert output: %s", string(out))
	}
	require.NoError(t, err, "failed to insert test PAT")
	return plainToken
}

// encodeBase62 encodes a uint32 as a base62 string using the same alphabet as NetBird.
func encodeBase62(n uint32) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%62]
		n /= 62
	}
	return string(buf[i:])
}

// getOwnerUserID returns the ID of the first owner user in the database.
func getOwnerUserID(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", "nb-postgres", "psql", "-U", "netbird", "-d", "netbird", "-t", "-c",
		"SELECT id FROM users WHERE role = 'owner' LIMIT 1;").CombinedOutput()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}
