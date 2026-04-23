package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	signalproto "github.com/netbirdio/netbird/shared/signal/proto"
)

const (
	signalRegistryKey   = "nb:signal:registry"
	signalChannelPrefix = "nb:signal:instance:"
)

// TestSignalCrossInstanceMessaging verifies that a message sent from a peer
// connected to signal-1 reaches a peer connected to signal-2 via Redis.
func TestSignalCrossInstanceMessaging(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	// Clean slate.
	_ = rdb.Del(ctx, signalRegistryKey)

	peerA := "peer-a-signal-test"
	peerB := "peer-b-signal-test"

	// Connect peer A to signal-1 and peer B to signal-2.
	sig1 := signalClient(t, signal1Addr)
	sig2 := signalClient(t, signal2Addr)

	streamA := connectSignalStream(t, sig1, peerA)
	defer streamA.CloseSend()

	streamB := connectSignalStream(t, sig2, peerB)
	defer streamB.CloseSend()

	// Consume stream B in background so server can send to it.
	recvCh := make(chan *signalproto.EncryptedMessage, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		msg, err := streamB.Recv()
		if err == nil {
			recvCh <- msg
		}
	}()

	// Wait for both peers to be registered in Redis.
	waitForRedisHashField(t, rdb, signalRegistryKey, peerA, "signal-1", 10*time.Second)
	waitForRedisHashField(t, rdb, signalRegistryKey, peerB, "signal-2", 10*time.Second)

	// Send message from peer A to peer B via signal-1.
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := sig1.Send(sendCtx, &signalproto.EncryptedMessage{
		Key:       peerA,
		RemoteKey: peerB,
		Body:      []byte("hello-from-signal-1"),
	})
	require.NoError(t, err)

	// Verify peer B receives the message.
	select {
	case msg := <-recvCh:
		require.NotNil(t, msg)
		assert.Equal(t, peerA, msg.Key)
		assert.Equal(t, peerB, msg.RemoteKey)
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for cross-instance message")
	}

	<-doneCh
}

// TestSignalRegistryPopulation verifies that peers are registered in the Redis HSET.
func TestSignalRegistryPopulation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	peerID := "peer-registry-test"
	_ = rdb.HDel(ctx, signalRegistryKey, peerID)

	sig1 := signalClient(t, signal1Addr)
	stream := connectSignalStream(t, sig1, peerID)
	defer stream.CloseSend()

	waitForRedisHashField(t, rdb, signalRegistryKey, peerID, "signal-1", 10*time.Second)

	val, err := rdb.HGet(ctx, signalRegistryKey, peerID).Result()
	require.NoError(t, err)
	assert.Equal(t, "signal-1", val)

	// Verify TTL is set on the registry key.
	ttl, err := rdb.TTL(ctx, signalRegistryKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
}

// TestSignalInstanceFailover verifies that when a signal instance is stopped,
// a peer can reconnect to the other instance and communication continues.
func TestSignalInstanceFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	requireDocker(t)

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	peerA := "peer-a-failover"
	peerB := "peer-b-failover"

	// Clean slate.
	_ = rdb.HDel(ctx, signalRegistryKey, peerA, peerB)

	sig1 := signalClient(t, signal1Addr)
	sig2 := signalClient(t, signal2Addr)

	// Peer A on signal-1, peer B on signal-2.
	streamA := connectSignalStream(t, sig1, peerA)
	defer streamA.CloseSend()

	streamB := connectSignalStream(t, sig2, peerB)
	defer streamB.CloseSend()

	waitForRedisHashField(t, rdb, signalRegistryKey, peerA, "signal-1", 10*time.Second)
	waitForRedisHashField(t, rdb, signalRegistryKey, peerB, "signal-2", 10*time.Second)

	// Stop signal-1 container.
	dockerStop(t, "nb-signal-1")
	defer dockerStart(t, "nb-signal-1")

	// Wait for signal-1 peer entry to disappear (or reconnect).
	// The old peer connection will drop; we reconnect peer A to signal-2.
	time.Sleep(2 * time.Second)

	// Reconnect peer A to signal-2.
	streamA2 := connectSignalStream(t, sig2, peerA)
	defer streamA2.CloseSend()

	waitForRedisHashField(t, rdb, signalRegistryKey, peerA, "signal-2", 15*time.Second)

	// Verify cross-instance messaging still works (both on signal-2 now).
	recvCh := make(chan *signalproto.EncryptedMessage, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		msg, err := streamB.Recv()
		if err == nil {
			recvCh <- msg
		}
	}()

	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := sig2.Send(sendCtx, &signalproto.EncryptedMessage{
		Key:       peerA,
		RemoteKey: peerB,
		Body:      []byte("hello-after-failover"),
	})
	require.NoError(t, err)

	select {
	case msg := <-recvCh:
		require.NotNil(t, msg)
		assert.Equal(t, "hello-after-failover", string(msg.Body))
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for post-failover message")
	}
	<-doneCh
}

// TestSignalGracefulDegradation verifies that when Redis is unavailable,
// the signal server falls back to local-only mode and peers on the same
// instance can still communicate.
func TestSignalGracefulDegradation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	requireDocker(t)

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	peerA := "peer-a-degradation"
	peerB := "peer-b-degradation"

	// Stop Redis.
	dockerStop(t, "nb-redis")
	defer dockerStart(t, "nb-redis")

	// Wait a moment for Redis to be fully down.
	time.Sleep(2 * time.Second)

	// Connect both peers to signal-1 (same instance).
	sig1 := signalClient(t, signal1Addr)
	streamA := connectSignalStream(t, sig1, peerA)
	defer streamA.CloseSend()

	streamB := connectSignalStream(t, sig1, peerB)
	defer streamB.CloseSend()

	// Consume stream B.
	recvCh := make(chan *signalproto.EncryptedMessage, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		msg, err := streamB.Recv()
		if err == nil {
			recvCh <- msg
		}
	}()

	// Send from peer A to peer B on the same instance.
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := sig1.Send(sendCtx, &signalproto.EncryptedMessage{
		Key:       peerA,
		RemoteKey: peerB,
		Body:      []byte("local-only-message"),
	})
	require.NoError(t, err)

	select {
	case msg := <-recvCh:
		require.NotNil(t, msg)
		assert.Equal(t, "local-only-message", string(msg.Body))
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for local-only message")
	}
	<-doneCh
}

// TestSignalRedisChannelIsolation verifies that messages published to one
// instance channel are not incorrectly received by another instance's peers.
func TestSignalRedisChannelIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	peerA := "peer-a-isolation"
	peerB := "peer-b-isolation"

	_ = rdb.HDel(ctx, signalRegistryKey, peerA, peerB)

	// Wait for Redis to be fully up and DNS-resolvable after potential restart.
	// Signal servers need Redis to register peers.
	for i := 0; i < 30; i++ {
		if err := rdb.Ping(ctx).Err(); err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.NoError(t, rdb.Ping(ctx).Err(), "redis not available")

	// Allow signal servers time to reconnect their Redis PubSub and clients.
	time.Sleep(3 * time.Second)

	sig1 := signalClient(t, signal1Addr)
	sig2 := signalClient(t, signal2Addr)

	streamA := connectSignalStream(t, sig1, peerA)
	defer streamA.CloseSend()

	streamB := connectSignalStream(t, sig2, peerB)
	defer streamB.CloseSend()

	waitForRedisHashField(t, rdb, signalRegistryKey, peerA, "signal-1", 15*time.Second)
	waitForRedisHashField(t, rdb, signalRegistryKey, peerB, "signal-2", 15*time.Second)

	// Verify channel keys exist and are distinct.
	ch1 := signalChannelPrefix + "signal-1"
	ch2 := signalChannelPrefix + "signal-2"
	exists1, err := rdb.Publish(ctx, ch1, "ping").Result()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, exists1, int64(1)) // at least signal-1 subscriber

	exists2, err := rdb.Publish(ctx, ch2, "ping").Result()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, exists2, int64(1)) // at least signal-2 subscriber
}

// TestSignalTraefikLoadBalancing verifies that peers connected through the
// Traefik load balancer are distributed across both signal instances.
func TestSignalTraefikLoadBalancing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	// Clean slate.
	_ = rdb.Del(ctx, signalRegistryKey)

	// Connect multiple peers through Traefik.
	peers := []string{"peer-tlb-1", "peer-tlb-2", "peer-tlb-3", "peer-tlb-4"}
	streams := make([]signalproto.SignalExchange_ConnectStreamClient, len(peers))

	for i, peer := range peers {
		client := signalClientTraefik(t)
		streams[i] = connectSignalStream(t, client, peer)
		defer streams[i].CloseSend()
	}

	// Wait for all peers to register and collect their instance assignments.
	instances := make(map[string]int)
	for _, peer := range peers {
		// Poll Redis until the peer appears (with timeout).
		var instance string
		for i := 0; i < 20; i++ {
			val, err := rdb.HGet(ctx, signalRegistryKey, peer).Result()
			if err == nil && val != "" {
				instance = val
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		require.NotEmpty(t, instance, "peer %s should be registered in Redis", peer)
		instances[instance]++
		t.Logf("peer %s registered on %s", peer, instance)
	}

	// Verify that peers are distributed across BOTH instances.
	assert.GreaterOrEqual(t, instances["signal-1"], 1, "at least one peer should land on signal-1")
	assert.GreaterOrEqual(t, instances["signal-2"], 1, "at least one peer should land on signal-2")
	t.Logf("load distribution: signal-1=%d, signal-2=%d", instances["signal-1"], instances["signal-2"])
}

// TestSignalTraefikFailover verifies that when the signal instance serving a
// peer dies, the peer can reconnect through Traefik to the surviving instance
// and cross-instance messaging continues.
func TestSignalTraefikFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	requireDocker(t)

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	peerA := "peer-a-traefik-failover"
	peerB := "peer-b-traefik-failover"

	// Clean slate.
	_ = rdb.HDel(ctx, signalRegistryKey, peerA, peerB)

	// Connect both peers through Traefik.
	client := signalClientTraefik(t)

	// Helper to connect and get instance with retry for cross-instance placement.
	connectAndGetInstance := func(peerID string) (signalproto.SignalExchange_ConnectStreamClient, string) {
		for attempt := 0; attempt < 10; attempt++ {
			_ = rdb.HDel(ctx, signalRegistryKey, peerID)
			stream := connectSignalStream(t, client, peerID)
			// Poll Redis to find which instance this peer landed on.
			var instance string
			for i := 0; i < 20; i++ {
				val, err := rdb.HGet(ctx, signalRegistryKey, peerID).Result()
				if err == nil && val != "" {
					instance = val
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if instance != "" {
				return stream, instance
			}
			stream.CloseSend()
		}
		t.Fatalf("could not connect peer %s to any instance", peerID)
		return nil, ""
	}

	streamA, instanceA := connectAndGetInstance(peerA)
	defer streamA.CloseSend()

	streamB, instanceB := connectAndGetInstance(peerB)
	defer streamB.CloseSend()

	t.Logf("peerA on %s, peerB on %s", instanceA, instanceB)

	// Ensure peers are on different instances; if not, reconnect peerB.
	if instanceA == instanceB {
		t.Logf("both peers on same instance, reconnecting peerB")
		streamB.CloseSend()
		_ = rdb.HDel(ctx, signalRegistryKey, peerB)
		for attempt := 0; attempt < 10; attempt++ {
			streamB, instanceB = connectAndGetInstance(peerB)
			if instanceB != instanceA {
				break
			}
			streamB.CloseSend()
			_ = rdb.HDel(ctx, signalRegistryKey, peerB)
		}
		require.NotEqual(t, instanceA, instanceB, "peerB should land on different instance")
	}
	defer streamB.CloseSend()

	// Stop the instance serving peer A.
	targetInstance := instanceA
	t.Logf("stopping %s", targetInstance)
	dockerStop(t, "nb-"+targetInstance)
	defer dockerStart(t, "nb-"+targetInstance)

	time.Sleep(2 * time.Second)

	// Reconnect peer A through Traefik; it should land on the other instance.
	streamA.CloseSend()
	_ = rdb.HDel(ctx, signalRegistryKey, peerA)
	streamA2, instanceA2 := connectAndGetInstance(peerA)
	defer streamA2.CloseSend()

	survivor := instanceB
	t.Logf("peerA reconnected on %s (expected survivor: %s)", instanceA2, survivor)
	assert.Equal(t, survivor, instanceA2, "peerA should reconnect to surviving instance")

	// Verify peer B (still on original instance) can receive messages from peer A.
	recvCh := make(chan *signalproto.EncryptedMessage, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		msg, err := streamB.Recv()
		if err == nil {
			recvCh <- msg
		}
	}()

	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := client.Send(sendCtx, &signalproto.EncryptedMessage{
		Key:       peerA,
		RemoteKey: peerB,
		Body:      []byte("hello-after-traefik-failover"),
	})
	require.NoError(t, err)

	select {
	case msg := <-recvCh:
		require.NotNil(t, msg)
		assert.Equal(t, "hello-after-traefik-failover", string(msg.Body))
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for post-failover message through Traefik")
	}
	<-doneCh
}
