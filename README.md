# NetBird High Availability (HA) Fork

**A horizontally-scalable, active-active fork of [netbirdio/netbird](https://github.com/netbirdio/netbird).**

This fork adds Redis-based distributed state to enable multiple Signal and Management server instances to operate concurrently behind a load balancer. All changes are backward-compatible: when HA is disabled, the system behaves exactly like upstream NetBird.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [What Changed (File-by-File)](#what-changed-file-by-file)
3. [Technologies Used](#technologies-used)
4. [Key Design Decisions](#key-design-decisions)
5. [Configuration Reference](#configuration-reference)
6. [Quick Start](#quick-start)
7. [Integration Tests](#integration-tests)
8. [Build & Deploy](#build--deploy)
9. [Maintaining After Upstream Updates](#maintaining-after-upstream-updates)
10. [Project Structure](#project-structure)

---

## Architecture Overview

```
                                Traefik LB (localhost:8088)
                                       |
            +--------------------------+--------------------------+
            |                          |                          |
     +------v------+            +------v------+           +-----v-------+
     |  signal-1   |            |  signal-2   |           |  dashboard  |
     |  :10000     |            |  :10000     |           |   :80       |
     +------+------+            +------+------+           +-------------+
            |                          |
            +------------+-------------+
                         |
                +--------v---------+
                |     Redis        |
                | nb:signal:registry |
                | nb:signal:instance: |
                +--------+---------+
                         |
                +--------v---------+
                |    PostgreSQL    |
                |  (shared state)  |
                +--------+---------+
                         |
            +------------+-------------+
            |                          |
     +------v------+            +------v------+
     |   mgmt-1    |            |   mgmt-2    |
     |   :33073    |            |   :33073    |
     +------+------+            +------+------+
            |                          |
            +------------+-------------+
                         |
                +--------v---------+
                |     Redis        |
                | nb:mgmt:account: |
                | nb:mgmt:lock:    |
                | nb:mgmt:ephemeral|
                +------------------+
```

### Components

| Component | Role | Count |
|-----------|------|-------|
| **Traefik** | Reverse proxy & load balancer for HTTP/gRPC | 1 |
| **Signal Server** | WebRTC signaling, peer message relay | 2+ |
| **Management Server** | Peer auth, network maps, policies | 2+ |
| **Redis** | Distributed state, pub/sub, locks | 1 (or Sentinel/Cluster) |
| **PostgreSQL** | Persistent account, peer, policy data | 1 |
| **Relay** | Fallback peer relay (self-hosted) | 1 |
| **coturn** | STUN/TURN for NAT traversal | 1 |
| **Dashboard** | Web UI (Next.js via Traefik) | 1 |

### How HA Works

#### Signal Server HA
- Each peer is registered in Redis under `nb:signal:registry` (HSET: peerPubKey -> instanceID)
- Each signal instance subscribes to a Redis channel `nb:signal:instance:<id>`
- When a peer sends a message to another peer, the server looks up the recipient's instance in Redis
- If the recipient is on a different instance, the message is forwarded via Redis pub/sub
- Heartbeat goroutines refresh the Redis TTL every 30 seconds
- If Redis is unavailable, signal degrades to local-only mode (no cross-instance routing)

#### Management Server HA
- **Account Updates**: When a management instance changes account state, it publishes to `nb:mgmt:account:<id>` on Redis. All instances receive the event and push updates to connected peers.
- **Distributed Locks**: Critical operations (peer registration, account creation) use Redis `SET NX EX` locks with TTL and heartbeat refresh.
- **Peer Registry**: Maps peer -> management instance in Redis Hash with TTL.
- **Login Filter**: Tracks in-progress logins in Redis Hash to prevent duplicate registration attempts.
- **Ephemeral Peers**: Uses Redis ZSET with TTL deadlines; a background goroutine polls and cleans up expired entries.
- **TURN/Relay Credentials**: Stateless credential refresh using HMAC (no in-memory timers), safe for any instance to generate.

---

## What Changed (File-by-File)

### New Files

| File | Purpose |
|------|---------|
| `shared/distributed/config.go` | `HAConfig` struct with env var bindings for all HA services |
| `shared/distributed/redis.go` | Redis client wrapper with health checks and reconnection |
| `management/server/distributed/config.go` | `ManagementHAConfig` extending HAConfig with mgmt-specific keys |
| `management/server/distributed/lock.go` | Distributed lock implementation using `SET NX EX` + heartbeat |
| `management/server/distributed/registry.go` | Peer-to-instance registry wrapper around Redis Hash |
| `signal/server/config.go` | `SignalHAConfig` with signal-specific env vars |
| `signal/metrics/app.go` | HA-specific metrics (cross-instance forwards, Redis errors) |
| `.env.example` | All configuration values externalized |
| `docker-compose.ha-test.yml` | Full test stack with Traefik, 2x signal, 2x mgmt, agents |
| `tests/integration/**` | 14 integration tests + helper utilities |

### Modified Files (Signal Server)

| File | Change |
|------|--------|
| `signal/server/signal.go` | Added Redis registry, cross-instance pub/sub forwarding, heartbeat goroutines, graceful degradation when Redis unavailable |
| `signal/cmd/run.go` | Parse HA CLI flags (`--ha-enabled`, `--ha-redis-address`) |
| `signal/cmd/root.go` | Wire HA config into signal server initialization |
| `signal/metrics/app.go` | Added cross-instance forward count, Redis error count, registry hit/miss metrics |

### Modified Files (Management Server)

| File | Change |
|------|--------|
| `management/internals/shared/grpc/server.go` | Added distributed peer locks (`NoopLock` fallback when HA disabled) |
| `management/internals/shared/grpc/loginfilter.go` | Redis Hash + TTL for in-progress login tracking |
| `management/internals/shared/grpc/token_mgr.go` | Stateless TURN/Relay credential refresh (removed in-memory timers) |
| `management/internals/modules/peers/ephemeral/manager/ephemeral.go` | Redis ZSET for ephemeral peer deadlines with polling cleanup |
| `management/internals/controllers/network_map/update_channel/updatechannel.go` | Account update pub/sub via Redis |
| `management/internals/controllers/network_map/controller/controller.go` | Broadcast account updates to all connected peers |
| `management/internals/server/server.go` | Wire Redis client into boot sequence |
| `management/internals/server/boot.go` | Initialize Redis client and HA components |
| `management/internals/server/controllers.go` | Pass Redis client to controllers |
| `management/internals/server/config/config.go` | Added `HAConfig` field |
| `management/cmd/management.go` | Parse HA flags from env vars |

### Modified Files (Combined Mode)

| File | Change |
|------|--------|
| `combined/cmd/root.go` | Pass HA config when running in combined mode |
| `combined/cmd/config.go` | Wire HA config into combined server |

### Modified Files (Test Environment)

| File | Change |
|------|--------|
| `management/Dockerfile` | Added `wget` for healthchecks |
| `tests/integration/config/management.json` | Self-hosted config with embedded IdP, STUN/TURN, relay |
| `tests/integration/Dockerfile.test` | Full project copy + Docker CLI for container stop/start tests |
| `tests/integration/Dockerfile.agent` | NetBird agent image for peer connectivity tests |

---

## Technologies Used

| Technology | Version | Purpose |
|------------|---------|---------|
| Go | 1.25.5 | Primary language |
| Redis | 7.x (via Docker) | Distributed state, pub/sub, locks |
| PostgreSQL | 15+ (via Docker) | Persistent data store |
| go-redis/v9 | 9.7.3 | Redis client library |
| WireGuard | kernel module | VPN tunneling |
| gRPC | 1.80.0 | Signal/Management RPC |
| Traefik | v3.6 | Reverse proxy / load balancer |
| Docker & Docker Compose | 29.x | Container orchestration |
| coturn | latest | STUN/TURN server |
| Next.js | latest (dashboard) | Web UI |

---

## Key Design Decisions

1. **Redis-first approach**: Local memory is a cache; Redis is the source of truth for cross-instance routing.
2. **Backward compatibility**: When `NB_HA_ENABLED=false` (or unset), the system uses `NoopLock` and nil Redis checks -- behavior is identical to upstream.
3. **Env var auto-mapping**: Signal CLI flags are automatically populated from env vars via `setFlagsFromEnvVars()`.
4. **Zero hardcoded values**: All URLs, endpoints, secrets are configurable via `.env` file.
5. **Instance ID auto-detection**: Falls back from config -> env var -> hostname -> UUID.
6. **Graceful degradation**: If Redis is unavailable, Signal continues in local-only mode; Management uses nil checks to skip HA features.
7. **Traefik for same-origin**: Dashboard and embedded IdP are served on the same origin (`localhost:8088`) to avoid CORS issues.
8. **Self-hosted everything**: No external dependencies -- STUN, TURN, relay, signal, management, dashboard all run in Docker.

---

## Configuration Reference

All configuration is in `.env` (copy from `.env.example`):

```bash
# Enable HA
NB_HA_ENABLED=true

# Redis
NB_REDIS_ADDRESS=redis.nb-ha.local:6379

# Signal HA
NB_SIGNAL_REGISTRY_KEY=nb:signal:registry
NB_SIGNAL_CHANNEL_PREFIX=nb:signal:instance:
NB_SIGNAL_PEER_TTL=60s
NB_SIGNAL_HEARTBEAT_INTERVAL=30s

# Management HA
NB_MGMT_PEERS_REGISTRY_KEY=nb:mgmt:peers
NB_MGMT_ACCOUNT_CHANNEL_PREFIX=nb:mgmt:account:
NB_MGMT_LOCK_PREFIX=nb:mgmt:lock:
NB_MGMT_LOGIN_FILTER_KEY=nb:mgmt:loginfilter
NB_MGMT_EPHEMERAL_KEY=nb:mgmt:ephemeral
NB_MGMT_PEER_TTL=60s
NB_MGMT_HEARTBEAT_INTERVAL=30s
NB_MGMT_LOCK_TTL=30s
```

---

## Quick Start

```bash
# 1. Clone and checkout HA branch
git clone https://github.com/netbirdio/netbird.git netbird_ha
cd netbird_ha
git checkout ha/main

# 2. Copy env file
cp .env.example .env

# 3. Build binaries
CGO_ENABLED=1 go build -o netbird-mgmt ./management/
CGO_ENABLED=1 go build -o netbird-signal ./signal/
CGO_ENABLED=1 go build -o netbird-server ./combined/
CGO_ENABLED=1 go build -o netbird ./client/
CGO_ENABLED=1 go build -o netbird-relay ./relay/

# 4. Build Docker images
docker compose -f docker-compose.ha-test.yml build

# 5. Start the stack
docker compose -f docker-compose.ha-test.yml up -d

# 6. Verify health
curl http://localhost:8088/api/users/current
curl http://localhost:9091/metrics  # mgmt-1 metrics
curl http://localhost:9093/metrics  # signal-1 metrics

# 7. Run integration tests
cd tests/integration
go test -v -count=1 -timeout 300s
```

---

## Integration Tests

See [docs/TESTING.md](docs/TESTING.md) for the full test suite documentation.

---

## Build & Deploy

See [docs/BUILD_DEPLOY.md](docs/BUILD_DEPLOY.md) for detailed build and deployment instructions.

---

## Maintaining After Upstream Updates

See [docs/REBASE_GUIDE.md](docs/REBASE_GUIDE.md) for step-by-step rebase instructions.

---

## Project Structure

```
netbird_ha/
├── .env.example                    # All configuration
├── docker-compose.ha-test.yml      # Full test stack
├── README_HA.md                    # This file
├── README_FORK.md                  # Fork summary
├── combined/                       # Combined signal+mgmt binary
├── client/                         # NetBird client (unchanged)
├── encryption/                     # WireGuard encryption utils
├── idp/                            # Identity provider (Dex embedded)
├── management/                     # Management server
│   ├── cmd/management.go           # HA flag parsing
│   ├── internals/
│   │   ├── controllers/network_map/  # Account update broadcast
│   │   ├── modules/peers/ephemeral/  # Ephemeral peer cleanup
│   │   ├── server/                   # Boot + config
│   │   └── shared/grpc/              # Locks, login filter, tokens
│   └── server/distributed/         # Lock, registry, config
├── relay/                          # Relay server (unchanged)
├── shared/
│   ├── distributed/                # HAConfig, Redis client
│   ├── management/proto/           # gRPC protobufs
│   └── signal/proto/               # gRPC protobufs
├── signal/                         # Signal server
│   ├── cmd/                        # HA flag parsing
│   ├── metrics/app.go              # HA metrics
│   └── server/                     # Registry, pub/sub
└── tests/integration/              # 14 integration tests
    ├── config/management.json      # Self-hosted mgmt config
    ├── Dockerfile.agent            # Test peer image
    ├── Dockerfile.test             # Test runner image
    ├── helper_test.go              # Redis, gRPC, Docker helpers
    ├── management_ha_test.go       # 7 management tests
    └── signal_ha_test.go           # 7 signal tests
```

---

## License

Same as upstream NetBird. See upstream repository for license details.
