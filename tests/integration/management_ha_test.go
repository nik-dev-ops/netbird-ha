package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/encryption"
	mgmtproto "github.com/netbirdio/netbird/shared/management/proto"
)

var (
	mgmtToken  = getEnv("MGMT_TOKEN", "")
	mgmtDomain = getEnv("NB_DOMAIN", "nb-ha.local")
)

const (
	mgmtPeersRegistryKey    = "nb:mgmt:peers"
	mgmtAccountChannelPrefix = "nb:mgmt:account:"
	mgmtLockPrefix          = "nb:mgmt:lock:"
)

// mgmtHTTPClient performs an HTTP request against a management server.
func mgmtHTTPClient(t *testing.T, method, baseURL, path string, body []byte) *http.Response {
	t.Helper()
	url := fmt.Sprintf("http://%s/api%s", baseURL, path)
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if mgmtToken != "" {
		req.Header.Set("Authorization", "Token "+mgmtToken)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// mgmtGRPCClient connects to a management gRPC endpoint.
func mgmtGRPCClient(t *testing.T, addr string) mgmtproto.ManagementServiceClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return mgmtproto.NewManagementServiceClient(conn)
}

// connectMgmtSync opens a Sync stream to a management server for a peer.
func connectMgmtSync(t *testing.T, client mgmtproto.ManagementServiceClient, peerKey string) mgmtproto.ManagementService_SyncClient {
	t.Helper()
	// The Sync RPC requires an EncryptedMessage. For integration testing we
	// send a minimal payload; the server will respond with encrypted updates.
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("wg-pub-key", peerKey))
	stream, err := client.Sync(ctx, &mgmtproto.EncryptedMessage{
		WgPubKey: peerKey,
		Version:  1,
	})
	require.NoError(t, err)
	return stream
}

// TestManagementUpdatePropagation verifies that peers connected to different
// management instances (mgmt-1 and mgmt-2) can communicate with each other,
// demonstrating shared database consistency and network map propagation.
func TestManagementUpdatePropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This test assumes agents are already running and connected via docker-compose.
	// We verify cross-instance peer reachability through the WireGuard tunnel.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get agent IPs from netbird status.
	agentAIP := getAgentIP(t, "nb-agent-a")
	agentBIP := getAgentIP(t, "nb-agent-b")

	t.Logf("agent-a IP: %s, agent-b IP: %s", agentAIP, agentBIP)
	require.NotEmpty(t, agentAIP, "agent-a should have a NetBird IP")
	require.NotEmpty(t, agentBIP, "agent-b should have a NetBird IP")

	// Verify agent-a can ping agent-b (cross-instance connectivity).
	out, err := exec.CommandContext(ctx, "docker", "exec", "nb-agent-a", "ping", "-c", "3", agentBIP).CombinedOutput()
	require.NoError(t, err, "agent-a should reach agent-b: %s", string(out))
	assert.Contains(t, string(out), "0% packet loss", "ping should succeed without packet loss")

	// Verify agent-b can ping agent-a (bidirectional connectivity).
	out, err = exec.CommandContext(ctx, "docker", "exec", "nb-agent-b", "ping", "-c", "3", agentAIP).CombinedOutput()
	require.NoError(t, err, "agent-b should reach agent-a: %s", string(out))
	assert.Contains(t, string(out), "0% packet loss", "ping should succeed without packet loss")
}

// getAgentIP extracts the NetBird IP address from a running agent container.
func getAgentIP(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "netbird", "status").CombinedOutput()
	if err != nil {
		t.Logf("netbird status output: %s", string(out))
		return ""
	}
	// Parse "NetBird IP: 100.x.x.x/16" from status output.
	re := regexp.MustCompile(`NetBird IP:\s+(\d+\.\d+\.\d+\.\d+)`)
	matches := re.FindStringSubmatch(string(out))
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

// TestManagementPeerRegistry verifies that the Redis peer registry mechanism
// works correctly (HSET/HGET/TTL/Expire), which is the foundation for HA
// peer-to-instance routing.
func TestManagementPeerRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	peerKey := "peer-mgmt-registry-test"
	_ = rdb.HDel(ctx, mgmtPeersRegistryKey, peerKey)

	// Simulate what the management server registry does on peer connect.
	err := rdb.HSet(ctx, mgmtPeersRegistryKey, peerKey, "mgmt-1").Err()
	require.NoError(t, err)
	err = rdb.Expire(ctx, mgmtPeersRegistryKey, 30*time.Second).Err()
	require.NoError(t, err)

	// Verify peer is registered.
	waitForRedisHashField(t, rdb, mgmtPeersRegistryKey, peerKey, "mgmt-1", 10*time.Second)

	val, err := rdb.HGet(ctx, mgmtPeersRegistryKey, peerKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "mgmt-1", val)

	// Verify TTL is set.
	ttl, err := rdb.TTL(ctx, mgmtPeersRegistryKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))

	// Simulate deregistration.
	err = rdb.HDel(ctx, mgmtPeersRegistryKey, peerKey).Err()
	require.NoError(t, err)
}

// TestManagementDistributedLocks verifies Redis-based distributed lock
// acquisition and release using the management lock prefix.
func TestManagementDistributedLocks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	lockKey := mgmtLockPrefix + "test-lock"
	_ = rdb.Del(ctx, lockKey)

	// Acquire lock with NX (only if not exists) and EX (expiry).
	acquired, err := rdb.SetNX(ctx, lockKey, "mgmt-1", 5*time.Second).Result()
	require.NoError(t, err)
	require.True(t, acquired, "lock should be acquired")

	// Verify lock value.
	val, err := rdb.Get(ctx, lockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "mgmt-1", val)

	// Second acquisition from same or different instance should fail.
	acquired2, err := rdb.SetNX(ctx, lockKey, "mgmt-2", 5*time.Second).Result()
	require.NoError(t, err)
	assert.False(t, acquired2, "lock should not be re-acquired")

	// Release lock.
	delCount, err := rdb.Del(ctx, lockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), delCount)

	// Re-acquire after release.
	acquired3, err := rdb.SetNX(ctx, lockKey, "mgmt-2", 5*time.Second).Result()
	require.NoError(t, err)
	assert.True(t, acquired3, "lock should be re-acquired after release")
}

// TestManagementInstanceFailover verifies that when a management instance is
// stopped, the other instance remains reachable via gRPC, and the Redis
// registry can be updated to reflect the new instance assignment.
func TestManagementInstanceFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	requireDocker(t)

	rdb := newRedisClient(t)
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	// Generate a real WireGuard key pair for this peer.
	peerKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)
	peerPubKeyStr := peerKey.PublicKey().String()

	_ = rdb.HDel(ctx, mgmtPeersRegistryKey, peerPubKeyStr)

	// Helper to login and sync with a management instance.
	loginAndSync := func(client mgmtproto.ManagementServiceClient, serverAddr string) *mgmtproto.SyncResponse {
		serverKeyResp, err := client.GetServerKey(context.Background(), &mgmtproto.Empty{})
		require.NoError(t, err, "GetServerKey failed for %s", serverAddr)
		serverPubKey, err := wgtypes.ParseKey(serverKeyResp.Key)
		require.NoError(t, err, "invalid server key from %s", serverAddr)

		meta := &mgmtproto.PeerSystemMeta{
			Hostname: peerPubKeyStr, GoOS: runtime.GOOS, OS: runtime.GOOS,
			Core: "core", Platform: "platform", Kernel: "kernel",
		}
		loginReq := &mgmtproto.LoginRequest{SetupKey: "E2808C99-E7FA-4841-845E-07CE633E50A1", Meta: meta}
		encLogin, err := encryption.EncryptMessage(serverPubKey, peerKey, loginReq)
		require.NoError(t, err, "failed to encrypt login request")

		loginResp, err := client.Login(context.Background(), &mgmtproto.EncryptedMessage{
			WgPubKey: peerPubKeyStr, Body: encLogin,
		})
		require.NoError(t, err, "login failed for %s", serverAddr)

		decryptedLogin := &mgmtproto.LoginResponse{}
		err = encryption.DecryptMessage(serverPubKey, peerKey, loginResp.Body, decryptedLogin)
		require.NoError(t, err, "failed to decrypt login response")

		syncReq := &mgmtproto.SyncRequest{Meta: meta}
		encSync, err := encryption.EncryptMessage(serverPubKey, peerKey, syncReq)
		require.NoError(t, err, "failed to encrypt sync request")

		stream, err := client.Sync(context.Background(), &mgmtproto.EncryptedMessage{
			WgPubKey: peerPubKeyStr, Body: encSync,
		})
		require.NoError(t, err, "sync call failed for %s", serverAddr)

		encResp, err := stream.Recv()
		require.NoError(t, err, "failed to receive sync response from %s", serverAddr)

		resp := &mgmtproto.SyncResponse{}
		err = encryption.DecryptMessage(serverPubKey, peerKey, encResp.Body, resp)
		require.NoError(t, err, "failed to decrypt sync response from %s", serverAddr)
		return resp
	}

	// Simulate peer registered to mgmt-1.
	err = rdb.HSet(ctx, mgmtPeersRegistryKey, peerPubKeyStr, "mgmt-1").Err()
	require.NoError(t, err)
	waitForRedisHashField(t, rdb, mgmtPeersRegistryKey, peerPubKeyStr, "mgmt-1", 10*time.Second)

	// Verify mgmt-1 is reachable with a real peer login+sync.
	client1 := mgmtGRPCClient(t, mgmt1Addr)
	syncResp1 := loginAndSync(client1, mgmt1Addr)
	require.NotNil(t, syncResp1, "should receive valid sync response from mgmt-1")
	t.Logf("sync from mgmt-1 OK")

	// Stop mgmt-1.
	dockerStop(t, "nb-mgmt-1")
	defer dockerStart(t, "nb-mgmt-1")

	time.Sleep(3 * time.Second)

	// Update registry to reflect failover to mgmt-2.
	err = rdb.HSet(ctx, mgmtPeersRegistryKey, peerPubKeyStr, "mgmt-2").Err()
	require.NoError(t, err)
	waitForRedisHashField(t, rdb, mgmtPeersRegistryKey, peerPubKeyStr, "mgmt-2", 10*time.Second)

	// Verify mgmt-2 is still reachable with a real peer login+sync.
	client2 := mgmtGRPCClient(t, mgmt2Addr)
	syncResp2 := loginAndSync(client2, mgmt2Addr)
	require.NotNil(t, syncResp2, "should receive valid sync response from mgmt-2 after failover")
	t.Logf("sync from mgmt-2 OK after failover")
}

// TestManagementHealthConsistency verifies that both management instances
// report healthy status, confirming independent operation.
func TestManagementHealthConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	for _, metricsAddr := range []string{mgmt1MetricsAddr, mgmt2MetricsAddr} {
		// Management exposes metrics on its dedicated metrics port; use it as health indicator.
		// The main API port serves gRPC+HTTP multiplexed and has no dedicated /healthz.
		url := fmt.Sprintf("http://%s/metrics", metricsAddr)

		// Retry with backoff in case a previous test restarted the container.
		var lastErr error
		var resp *http.Response
		for i := 0; i < 10; i++ {
			resp, lastErr = http.Get(url)
			if lastErr == nil && resp.StatusCode == http.StatusOK {
				break
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			time.Sleep(500 * time.Millisecond)
		}
		require.NoError(t, lastErr, "health check failed for %s", metricsAddr)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "%s not healthy", metricsAddr)
	}
}

// TestManagementPolicyPropagation verifies that policies and groups created via
// one management instance are immediately visible via the other instance,
// demonstrating shared database consistency.
func TestManagementPolicyPropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Get owner user and create a PAT for API auth.
	ownerID := getOwnerUserID(t)
	require.NotEmpty(t, ownerID, "owner user should exist")
	token := createTestPAT(t, ownerID)

	// Create a test group via mgmt-1.
	groupName := "test-ha-group-" + fmt.Sprintf("%d", time.Now().Unix())
	groupBody, _ := json.Marshal(map[string]interface{}{
		"name": groupName,
	})
	resp := mgmtHTTPClientWithToken(t, "POST", mgmt1Addr, "/groups", groupBody, token)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "should create group on mgmt-1")
	groupBodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var groupResp map[string]interface{}
	_ = json.Unmarshal(groupBodyBytes, &groupResp)
	groupID, _ := groupResp["id"].(string)
	t.Logf("created group %s with id %s", groupName, groupID)
	require.NotEmpty(t, groupID, "group should have an ID")

	// Verify the group is visible on mgmt-2 (shared database).
	resp2 := mgmtHTTPClientWithToken(t, "GET", mgmt2Addr, "/groups", nil, token)
	assert.Equal(t, http.StatusOK, resp2.StatusCode, "should list groups on mgmt-2")
	groupsBody, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	assert.Contains(t, string(groupsBody), groupID, "group should be visible on mgmt-2")

	// Create a policy on mgmt-1 using the group.
	policyName := "test-ha-policy-" + fmt.Sprintf("%d", time.Now().Unix())
	policyBody, _ := json.Marshal(map[string]interface{}{
		"name": policyName,
		"description": "HA test policy",
		"enabled": true,
		"rules": []map[string]interface{}{{
			"name": "allow-all",
			"enabled": true,
			"action": "accept",
			"sources": []string{groupID},
			"destinations": []string{groupID},
			"protocol": "all",
			"bidirectional": true,
		}},
	})
	resp3 := mgmtHTTPClientWithToken(t, "POST", mgmt1Addr, "/policies", policyBody, token)
	assert.Equal(t, http.StatusOK, resp3.StatusCode, "should create policy on mgmt-1")
	policyBodyBytes, _ := io.ReadAll(resp3.Body)
	_ = resp3.Body.Close()

	var policyResp map[string]interface{}
	_ = json.Unmarshal(policyBodyBytes, &policyResp)
	policyID, _ := policyResp["id"].(string)
	t.Logf("created policy %s with id %s", policyName, policyID)
	require.NotEmpty(t, policyID, "policy should have an ID")

	// Verify the policy is visible on mgmt-2.
	resp4 := mgmtHTTPClientWithToken(t, "GET", mgmt2Addr, "/policies", nil, token)
	assert.Equal(t, http.StatusOK, resp4.StatusCode, "should list policies on mgmt-2")
	policiesBody, _ := io.ReadAll(resp4.Body)
	_ = resp4.Body.Close()
	assert.Contains(t, string(policiesBody), policyID, "policy should be visible on mgmt-2")

	// Cleanup: delete policy and group.
	_ = mgmtHTTPClientWithToken(t, "DELETE", mgmt1Addr, "/policies/"+policyID, nil, token)
	_ = mgmtHTTPClientWithToken(t, "DELETE", mgmt1Addr, "/groups/"+groupID, nil, token)
}

// TestManagementFailoverWithSync verifies that when a management instance fails,
// a peer can reconnect to the surviving instance via Traefik and continue
// receiving network map updates.
func TestManagementFailoverWithSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	requireDocker(t)

	// Generate a real WireGuard key pair for this peer.
	peerKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	// Helper to perform full login + sync against a management client.
	loginAndSync := func(client mgmtproto.ManagementServiceClient, serverAddr string) *mgmtproto.SyncResponse {
		// 1. Get the server's public key.
		serverKeyResp, err := client.GetServerKey(context.Background(), &mgmtproto.Empty{})
		require.NoError(t, err, "GetServerKey failed for %s", serverAddr)
		serverPubKey, err := wgtypes.ParseKey(serverKeyResp.Key)
		require.NoError(t, err, "invalid server key from %s", serverAddr)

		// 2. Login with the setup key to register the peer.
		meta := &mgmtproto.PeerSystemMeta{
			Hostname:       peerKey.PublicKey().String(),
			GoOS:           runtime.GOOS,
			OS:             runtime.GOOS,
			Core:           "core",
			Platform:       "platform",
			Kernel:         "kernel",
			NetbirdVersion: "",
		}
		loginReq := &mgmtproto.LoginRequest{SetupKey: "E2808C99-E7FA-4841-845E-07CE633E50A1", Meta: meta}
		encLogin, err := encryption.EncryptMessage(serverPubKey, peerKey, loginReq)
		require.NoError(t, err, "failed to encrypt login request")

		loginResp, err := client.Login(context.Background(), &mgmtproto.EncryptedMessage{
			WgPubKey: peerKey.PublicKey().String(),
			Body:     encLogin,
		})
		require.NoError(t, err, "login failed for %s", serverAddr)

		decryptedLogin := &mgmtproto.LoginResponse{}
		err = encryption.DecryptMessage(serverPubKey, peerKey, loginResp.Body, decryptedLogin)
		require.NoError(t, err, "failed to decrypt login response")
		t.Logf("login to %s succeeded, peer registered", serverAddr)

		// 3. Open Sync stream with a properly encrypted SyncRequest.
		syncReq := &mgmtproto.SyncRequest{Meta: meta}
		encSync, err := encryption.EncryptMessage(serverPubKey, peerKey, syncReq)
		require.NoError(t, err, "failed to encrypt sync request")

		stream, err := client.Sync(context.Background(), &mgmtproto.EncryptedMessage{
			WgPubKey: peerKey.PublicKey().String(),
			Body:     encSync,
		})
		require.NoError(t, err, "sync call failed for %s", serverAddr)

		// Read the first encrypted SyncResponse.
		encResp, err := stream.Recv()
		require.NoError(t, err, "failed to receive sync response from %s", serverAddr)

		resp := &mgmtproto.SyncResponse{}
		err = encryption.DecryptMessage(serverPubKey, peerKey, encResp.Body, resp)
		require.NoError(t, err, "failed to decrypt sync response from %s", serverAddr)
		return resp
	}

	// Verify peer can sync via mgmt-1.
	client1 := mgmtGRPCClient(t, mgmt1Addr)
	syncResp1 := loginAndSync(client1, mgmt1Addr)
	require.NotNil(t, syncResp1, "should receive valid sync response from mgmt-1")
	require.NotNil(t, syncResp1.NetbirdConfig, "sync response should contain netbird config")
	t.Logf("sync from mgmt-1 OK, signal URI: %s", syncResp1.NetbirdConfig.GetSignal().GetUri())

	// Stop mgmt-1.
	dockerStop(t, "nb-mgmt-1")
	defer dockerStart(t, "nb-mgmt-1")

	time.Sleep(3 * time.Second)

	// Verify peer can sync via Traefik (should route to mgmt-2).
	client2 := mgmtClientTraefik(t)
	syncResp2 := loginAndSync(client2, mgmtTraefikAddr)
	require.NotNil(t, syncResp2, "should receive valid sync response via Traefik after failover")
	require.NotNil(t, syncResp2.NetbirdConfig, "sync response should contain netbird config after failover")
	t.Logf("sync via Traefik OK after failover, signal URI: %s", syncResp2.NetbirdConfig.GetSignal().GetUri())
}

// mgmtHTTPClientWithToken performs an authenticated HTTP request against a management server.
func mgmtHTTPClientWithToken(t *testing.T, method, baseURL, path string, body []byte, token string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("http://%s/api%s", baseURL, path)
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}
