# NetBird HA Integration Tests

This directory contains integration tests for the NetBird High-Availability fork.
The tests verify cross-instance messaging, state propagation, failover, and graceful
degradation across Signal and Management servers.

## Test Environment

The tests assume a Docker Compose environment with the following services:

| Service | Address | Purpose |
|---------|---------|---------|
| Redis | `redis.nb-ha.local:6379` | Distributed state and pub/sub |
| PostgreSQL | `postgres.nb-ha.local:5432` | 3 databases: netbird, netbird_auth, netbird_events |
| Signal-1 | `signal-1.nb-ha.local:10000` | Signal server instance 1 |
| Signal-2 | `signal-2.nb-ha.local:10000` | Signal server instance 2 |
| Mgmt-1 | `mgmt-1.nb-ha.local:33073` | Management server instance 1 |
| Mgmt-2 | `mgmt-2.nb-ha.local:33073` | Management server instance 2 |

### PostgreSQL Databases

The HA deployment uses 3 PostgreSQL databases:

| Database | Purpose |
|----------|---------|
| `netbird` | Main store: users, accounts, peers, setup_keys, rules, policies |
| `netbird_auth` | Embedded IdP (Dex): OAuth tokens, connectors |
| `netbird_events` | Activity store: audit logs, events |

## Files

- `signal_ha_test.go` — Signal server HA tests
- `management_ha_test.go` — Management server HA tests
- `helper_test.go` — Shared test utilities
- `scripts/init-test-data.sh` — Idempotent database initialization
- `go.mod` — Go module for integration tests

## Running Tests

### Prerequisites

1. Start the test environment:
   ```bash
   cp .env.example .env
   docker compose -f docker-compose.ha-test.yml up --build -d
   ```

2. Initialize test data:
   ```bash
   docker exec nb-test-runner /tests/scripts/init-test-data.sh
   ```

3. Run the tests:
   ```bash
   docker exec -e MGMT_TOKEN=<token> nb-test-runner go test -v ./...
   ```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SIGNAL1_ADDR` | `localhost:10000` | Signal-1 gRPC endpoint |
| `SIGNAL2_ADDR` | `localhost:10000` | Signal-2 gRPC endpoint |
| `MGMT1_ADDR` | `localhost:33073` | Mgmt-1 HTTP/gRPC endpoint |
| `MGMT2_ADDR` | `localhost:33073` | Mgmt-2 HTTP/gRPC endpoint |
| `REDIS_ADDR` | `localhost:6379` | Redis endpoint |
| `MGMT_TOKEN` | *(none)* | PAT for management HTTP API |
| `POSTGRES_DSN` | *(see script)* | Postgres connection string |

### Short Mode

Tests that require running infrastructure are skipped when `-short` is passed:

```bash
go test -short ./...
```

### Container-Based Tests

Tests that stop/start containers (`TestSignalInstanceFailover`,
`TestSignalGracefulDegradation`, `TestManagementInstanceFailover`) require the
Docker CLI to be available inside the test runner. Mount the Docker socket if
you want to run these:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
```

## Test Coverage

### Signal HA Tests (`signal_ha_test.go`)

1. **`TestSignalCrossInstanceMessaging`** — Peer on signal-1 sends a message to
   peer on signal-2 via Redis pub/sub.
2. **`TestSignalRegistryPopulation`** — Connected peers appear in the Redis HSET
   `nb:signal:registry` with correct instance mappings.
3. **`TestSignalInstanceFailover`** — Stop signal-1, peer reconnects to signal-2,
   messaging continues.
4. **`TestSignalGracefulDegradation`** — Stop Redis, verify local-only mode still
   works for peers on the same instance.
5. **`TestSignalRedisChannelIsolation`** — Each signal instance has its own Redis
   pub/sub channel and messages are not broadcast to all instances.
6. **`TestSignalTraefikLoadBalancing`** — Peers connecting through Traefik are
   distributed across both signal instances.
7. **`TestSignalTraefikFailover`** — When a signal instance dies, peer reconnects
   through Traefik to the surviving instance and messaging continues.

### Management HA Tests (`management_ha_test.go`)

1. **`TestManagementUpdatePropagation`** — Create a setup key via mgmt-1 HTTP API,
   verify it is readable via mgmt-2 HTTP API (shared database consistency).
2. **`TestManagementPeerRegistry`** — Peer connected to mgmt-1 via Sync has its
   `peer->instance` mapping stored in Redis `nb:mgmt:peers`.
3. **`TestManagementDistributedLocks`** — Verify Redis-based lock acquisition
   (`SET NX EX`) and release (`DEL`) using the management lock prefix.
4. **`TestManagementInstanceFailover`** — Stop mgmt-1, peer reconnects to mgmt-2,
   Sync stream resumes.
5. **`TestManagementHealthConsistency`** — Both management instances report
   healthy status via their metrics endpoints.
6. **`TestManagementPolicyPropagation`** — Policies and groups created via one
   management instance are visible via the other (shared database consistency).
7. **`TestManagementFailoverWithSync`** — Full login + sync from both management
   instances using the same peer key after failover.

## Idempotent Initialization

`scripts/init-test-data.sh` is safe to run multiple times. It will:

- Ensure the `netbird_auth` and `netbird_events` databases exist
- Create the owner user (if instance setup is required)
- Create a reusable setup key for integration tests
- Create a Personal Access Token (PAT) for HTTP API access
- Create test peers in the database

**Note**: The `postgres-init.sh` script (run automatically on PostgreSQL container startup) creates the `netbird_auth` and `netbird_events` databases if they don't exist.

All operations check for existing data before inserting.

## Architecture Notes

- Signal instances share peer registry via Redis HSET and forward messages via
  Redis pub/sub on per-instance channels (`nb:signal:instance:<instance-id>`).
- Management instances share state via Postgres and coordinate via Redis
  distributed locks (`nb:mgmt:lock:*`).
- Both signal and management write peer->instance mappings to Redis for
  cross-instance routing and failover detection.
