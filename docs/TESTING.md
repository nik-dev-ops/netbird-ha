# NetBird HA Integration Tests

All integration tests are in `tests/integration/` and validate the HA behavior of Signal and Management servers running in Docker.

## Test Suite Summary

| Test File | Tests | Focus |
|-----------|-------|-------|
| `signal_ha_test.go` | 7 | Signal server HA (registry, pub/sub, failover, load balancing) |
| `management_ha_test.go` | 7 | Management server HA (sync, locks, policies, failover) |
| `helper_test.go` | — | Shared utilities (Redis, gRPC, Docker, PAT, WireGuard keys) |

**Total: 14 tests, all passing**

## Running the Tests

### Prerequisites

- Docker 29+ with Docker Compose
- Go 1.25.5+
- `wireguard-tools` package (for `wg genkey` / `wg pubkey`)

### Start the Test Environment

```bash
# From project root
docker compose -f docker-compose.ha-test.yml up -d

# Wait for all services to be healthy (30-60s)
docker compose -f docker-compose.ha-test.yml ps
```

### Run All Tests

```bash
cd tests/integration
go test -v -count=1 -timeout 300s
```

### Run Individual Tests

```bash
# Signal tests only
go test -v -count=1 -run TestSignal -timeout 120s

# Management tests only
go test -v -count=1 -run TestManagement -timeout 120s

# Single test
go test -v -count=1 -run TestSignalTraefikFailover -timeout 120s
```

### Environment Variables

The tests use `localhost` with exposed host ports by default. Override via env vars:

| Variable | Default | Description |
|----------|---------|-------------|
| `SIGNAL1_ADDR` | `localhost:10000` | Signal-1 gRPC endpoint |
| `SIGNAL2_ADDR` | `localhost:10001` | Signal-2 gRPC endpoint |
| `SIGNAL_TRAEFIK_ADDR` | `localhost:8088` | Signal via Traefik LB |
| `MGMT1_ADDR` | `localhost:33073` | Management-1 gRPC/HTTP |
| `MGMT2_ADDR` | `localhost:33074` | Management-2 gRPC/HTTP |
| `MGMT_TRAEFIK_ADDR` | `localhost:8088` | Management via Traefik LB |
| `MGMT1_METRICS` | `localhost:9091` | Management-1 metrics |
| `MGMT2_METRICS` | `localhost:9092` | Management-2 metrics |
| `REDIS_ADDR` | `localhost:6379` | Redis endpoint |

---

## Signal HA Tests (`signal_ha_test.go`)

### TestSignalCrossInstanceMessaging

**What it tests**: A peer connected to signal-1 can send messages to a peer connected to signal-2 via Redis pub/sub.

**How it works**:
1. Connects `peer-a` to signal-1 and `peer-b` to signal-2
2. Waits for both peers to be registered in Redis (`nb:signal:registry`)
3. signal-1 looks up peer-b's instance (signal-2), publishes message to `nb:signal:instance:signal-2`
4. signal-2 receives the pub/sub message and delivers it to peer-b
5. Asserts the message body matches

**Expected**: `PASS` with message delivery verified

---

### TestSignalRegistryPopulation

**What it tests**: The Redis HSET registry correctly stores peer -> instance mappings with TTL.

**How it works**:
1. Connects a peer to signal-1
2. Polls Redis for the peer key in `nb:signal:registry`
3. Verifies the value is `signal-1`
4. Verifies TTL is set (> 0)

**Expected**: `PASS` with correct instance mapping and TTL

---

### TestSignalInstanceFailover

**What it tests**: When a signal instance is killed, peers can reconnect to the surviving instance and continue receiving messages.

**How it works**:
1. Connects peer-a to signal-1, peer-b to signal-2
2. Stops signal-1 container (`docker stop nb-signal-1`)
3. Peer-a reconnects to signal-2 (direct connection, not via Traefik)
4. Peer-a sends message to peer-b
5. Asserts peer-b receives it (now both on signal-2)
6. Restarts signal-1 (deferred cleanup)

**Expected**: `PASS` with message delivery after failover

---

### TestSignalGracefulDegradation

**What it tests**: When Redis is unavailable, the signal server continues operating in local-only mode.

**How it works**:
1. Connects peer-a and peer-b to signal-1
2. Stops Redis container (`docker stop nb-redis`)
3. Peer-a sends message to peer-b
4. Asserts peer-b receives it (both on same instance, no Redis needed)
5. Restarts Redis (deferred cleanup)

**Expected**: `PASS` — local peers still communicate without Redis

---

### TestSignalRedisChannelIsolation

**What it tests**: Each signal instance has its own Redis pub/sub channel, and messages are not broadcast to all instances.

**How it works**:
1. Verifies `nb:signal:instance:signal-1` and `nb:signal:instance:signal-2` channels exist
2. Publishes a test message to each channel
3. Verifies each channel has at least one subscriber (the respective signal instance)

**Expected**: `PASS` with both channels having subscribers

---

### TestSignalTraefikLoadBalancing

**What it tests**: Peers connecting through the Traefik load balancer are distributed across both signal instances.

**How it works**:
1. Connects 4 peers through Traefik (`localhost:8088`)
2. Polls Redis registry to find which instance each peer landed on
3. Asserts at least one peer is on signal-1 and at least one on signal-2

**Expected**: `PASS` with peers distributed across both instances (load balanced)

---

### TestSignalTraefikFailover

**What it tests**: When the signal instance serving a peer dies, the peer reconnects through Traefik to the surviving instance and cross-instance messaging continues.

**How it works**:
1. Connects peer-a and peer-b through Traefik
2. Determines which instance each peer landed on
3. Ensures peers are on different instances (reconnects peer-b if needed)
4. Stops the instance serving peer-a
5. Peer-a reconnects through Traefik (should land on the survivor)
6. Peer-a sends message to peer-b (still on original instance)
7. Asserts peer-b receives the message via Redis pub/sub

**Expected**: `PASS` with successful reconnection and message delivery

---

## Management HA Tests (`management_ha_test.go`)

### TestManagementUpdatePropagation

**What it tests**: Peers connected to different management instances can communicate through the WireGuard tunnel.

**How it works**:
1. Assumes `nb-agent-a` and `nb-agent-b` are already running and connected
2. Extracts NetBird IPs from `netbird status` output
3. Runs `ping -c 3` from agent-a to agent-b and vice versa
4. Asserts 0% packet loss in both directions

**Expected**: `PASS` with bidirectional ping success

**Note**: This is the highest-level integration test — it validates the entire end-to-end data path.

---

### TestManagementPeerRegistry

**What it tests**: The Redis peer registry for management stores peer -> instance mappings with TTL.

**How it works**:
1. Simulates peer registration: `HSET nb:mgmt:peers peer-key mgmt-1`
2. Sets TTL with `EXPIRE`
3. Verifies the value and TTL are correct
4. Simulates deregistration with `HDEL`

**Expected**: `PASS` with correct registry behavior

---

### TestManagementDistributedLocks

**What it tests**: Redis-based distributed locks work correctly with `SET NX EX`.

**How it works**:
1. Acquires lock from mgmt-1: `SET nb:mgmt:lock:test-lock mgmt-1 NX EX 5`
2. Verifies lock value is `mgmt-1`
3. Attempts to acquire same lock from mgmt-2 — should fail
4. Releases lock with `DEL`
5. Re-acquires from mgmt-2 — should succeed

**Expected**: `PASS` with exclusive lock semantics

---

### TestManagementInstanceFailover

**What it tests**: When a management instance is stopped, the other instance remains reachable, and a peer can perform full login + sync.

**How it works**:
1. Generates a real WireGuard key pair for the test peer
2. **Login + Sync via mgmt-1**:
   - Gets server public key via `GetServerKey`
   - Encrypts a `LoginRequest` with the setup key
   - Calls `Login` gRPC
   - Decrypts the `LoginResponse`
   - Encrypts a `SyncRequest`
   - Opens `Sync` stream
   - Decrypts the `SyncResponse`
3. Stops mgmt-1 container
4. Updates Redis registry to point peer to mgmt-2
5. **Login + Sync via mgmt-2** with the same peer key
6. Asserts valid `SyncResponse` received

**Expected**: `PASS` with successful sync from both instances

---

### TestManagementHealthConsistency

**What it tests**: Both management instances report healthy status via their metrics endpoints.

**How it works**:
1. Queries `http://localhost:9091/metrics` (mgmt-1) with retries
2. Queries `http://localhost:9092/metrics` (mgmt-2) with retries
3. Asserts HTTP 200 for both

**Expected**: `PASS` with both instances healthy

---

### TestManagementPolicyPropagation

**What it tests**: Policies and groups created via one management instance are immediately visible via the other (shared database consistency).

**How it works**:
1. Gets the owner user ID from PostgreSQL
2. Creates a Personal Access Token (PAT) in the database
3. **Creates a group** via mgmt-1 REST API (`POST /api/groups`)
4. **Lists groups** via mgmt-2 REST API (`GET /api/groups`)
5. Asserts the new group ID appears in the mgmt-2 response
6. **Creates a policy** via mgmt-1 REST API (`POST /api/policies`)
7. **Lists policies** via mgmt-2 REST API (`GET /api/policies`)
8. Asserts the new policy ID appears in the mgmt-2 response
9. Cleans up (deletes policy and group)

**Expected**: `PASS` with cross-instance visibility of groups and policies

**Authentication**: Uses PAT (Personal Access Token) generated in the test, inserted directly into PostgreSQL. The PAT follows NetBird's format (`nbp_<secret><checksum>`) with proper base62 encoding.

---

### TestManagementFailoverWithSync

**What it tests**: When a management instance fails, a peer can reconnect via Traefik to the surviving instance and continue receiving valid network map updates.

**How it works**:
1. Generates a real WireGuard key pair for the test peer
2. **Full login + sync via mgmt-1**:
   - Gets server key, encrypts login request, decrypts response
   - Encrypts sync request, opens sync stream
   - Decrypts sync response
   - Asserts response contains valid `NetbirdConfig` with signal URI
3. Stops mgmt-1 container
4. **Full login + sync via Traefik** (routes to mgmt-2):
   - Same encryption/decryption flow
   - Asserts valid sync response after failover

**Expected**: `PASS` with successful sync from mgmt-1 and from Traefik after failover

---

## Test Helpers (`helper_test.go`)

### Redis Client

```go
newRedisClient(t) *redis.Client
```

Creates a Redis client using `redis.ParseURL` with the address from `REDIS_ADDR` env var (default: `localhost:6379`).

### Signal gRPC Client

```go
signalClient(t, addr string) signalproto.SignalExchangeClient
signalClientTraefik(t) signalproto.SignalExchangeClient
```

Connects to a signal server via gRPC with insecure credentials and 5-15s timeout. The Traefik variant connects through the load balancer.

### Management gRPC Client

```go
mgmtGRPCClient(t, addr string) mgmtproto.ManagementServiceClient
mgmtClientTraefik(t) mgmtproto.ManagementServiceClient
```

Connects to a management server via gRPC. The Traefik variant routes through the load balancer.

### connectMgmtSync

```go
connectMgmtSync(t, client, peerKey string) mgmtproto.ManagementService_SyncClient
```

Opens a `Sync` stream with the peer's WireGuard public key in the gRPC metadata.

### Docker Control

```go
dockerStop(t, container string)
dockerStart(t, container string)
```

Stops/starts Docker containers by name. Used for failover tests.

### Personal Access Token (PAT)

```go
createTestPAT(t, userID string) string
```

Generates a valid NetBird PAT (`nbp_<30-char-secret><6-char-checksum>`) and inserts it into PostgreSQL. Uses proper base62 encoding with the NetBird alphabet (`0-9A-Za-z`).

```go
getOwnerUserID(t) string
```

Queries PostgreSQL for the first owner user ID.

### WireGuard Key Generation

Tests that need valid peer keys use `wgtypes.GenerateKey()` from the `golang.zx2c4.com/wireguard/wgctrl/wgtypes` package.

### mgmtHTTPClientWithToken

```go
mgmtHTTPClientWithToken(t, method, baseURL, path, body, token) *http.Response
```

Performs authenticated HTTP requests against the management REST API using a PAT in the `Authorization: Token <pat>` header.

---

## Test Environment Files

| File | Purpose |
|------|---------|
| `Dockerfile.test` | Test runner image: Go 1.25, Docker CLI, Redis client, psql, full project copy |
| `Dockerfile.agent` | NetBird agent image for real peer connectivity tests |
| `config/management.json` | Management config for mgmt-1 (Signal: signal-1.nb-ha.local) |
| `config/management-2.json` | Management config for mgmt-2 (Signal: signal-2.nb-ha.local) |
| `scripts/agent-setup.sh` | Agent bootstrap: login, up, status |
| `go.mod` / `go.sum` | Test dependencies: redis, testify, grpc, wireguard |

### Management Server Configs

Each management server uses a separate config file:

| File | Signal URI | Used By |
|------|-----------|---------|
| `management.json` | `signal-1.nb-ha.local:10000` | mgmt-1 |
| `management-2.json` | `signal-2.nb-ha.local:10000` | mgmt-2 |

This ensures agents connecting to different management servers get assigned to different signal servers, enabling proper HA testing.

### PostgreSQL Databases

The test environment creates 3 databases automatically:

```bash
# Main database
docker exec nb-postgres psql -U netbird -d netbird -c "SELECT 1;"

# Auth database (embedded IdP / Dex)
docker exec nb-postgres psql -U netbird -d netbird_auth -c "SELECT 1;"

# Events database (activity logs)
docker exec nb-postgres psql -U netbird -d netbird_events -c "SELECT 1;"
```

---

## Debugging Failed Tests

### Check container health

```bash
docker compose -f docker-compose.ha-test.yml ps
docker logs nb-signal-1 --tail 30
docker logs nb-mgmt-1 --tail 30
```

### Check Redis state

```bash
docker exec nb-redis redis-cli HGETALL nb:signal:registry
docker exec nb-redis redis-cli PUBLISH nb:signal:instance:signal-1 ping
```

### Check PostgreSQL

```bash
# Main database
docker exec nb-postgres psql -U netbird -d netbird -c "SELECT id, email, role FROM users;"
docker exec nb-postgres psql -U netbird -d netbird -c "SELECT id, name FROM setup_keys;"

# All databases
docker exec nb-postgres psql -U netbird -l
```

### Check Traefik routing

```bash
curl -s http://localhost:8089/api/http/services | python3 -m json.tool
curl -s http://localhost:8089/api/http/routers | python3 -m json.tool
```

### Run with verbose logging

```bash
go test -v -count=1 -run TestSignalTraefikLoadBalancing -timeout 120s
```

---

## CI/CD Integration

The tests are designed to run in CI with the Docker Compose stack:

```yaml
# Example GitHub Actions workflow
- name: Start HA test environment
  run: docker compose -f docker-compose.ha-test.yml up -d

- name: Wait for services
  run: |
    for i in {1..30}; do
      curl -sf http://localhost:9091/metrics && break
      sleep 2
    done

- name: Run integration tests
  run: cd tests/integration && go test -v -count=1 -timeout 300s
```
