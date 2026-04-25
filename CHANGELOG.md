# Changelog

All notable changes to the NetBird HA fork are documented here.

## [Unreleased]

## [2026-04-25] - Security & Reliability Fixes

### Security Fixes
- **Redis TLS Support**: Added TLS configuration and secure connection support for Redis connections
- **Redis Pool Limits**: Added MaxIdleConns, ConnMaxLifetime, and ConnMaxIdleTime configuration to prevent connection exhaustion
- **Redis Reconnection**: Added proper reconnection handling with exponential backoff when Redis becomes unavailable
- **Distributed Lock Fixes**:
  - Added heartbeat ownership validation to prevent lock stealing
  - Fixed goroutine leak in lock release
  - release() now returns error instead of swallowing failures
- **Signal Server HMAC-SHA256**: Replaced unauthenticated Redis pub/sub with HMAC-SHA256 signed messages
- **Login Filter TOCTOU Fix**: Added UnderLock functions to prevent time-of-check-time-of-use races
- **Login Filter Counter**: Changed from int to uint64 to prevent overflow
- **Login Filter Memory Leak**: Added cleanup goroutine for stale entries
- **Ephemeral Peer Atomic ZRem**: Replaced non-atomic ZRem with WATCH/MULTI/EXEC transactions
- **Ephemeral Deadline Precision**: Changed from Unix to UnixMilli for sub-second precision
- **DSN Password Masking**: Enhanced maskDSNPassword() to handle URI, key=value, and pass= formats
- **TURN Credentials HMAC**: Replaced CRC32 checksum with HMAC-SHA256 for TURN/relay credentials
- **Env Var Rename**: NB_REDIS_PASSWORD renamed to NB_HA_REDIS_PASSWORD for clarity

### Reliability Improvements
- Signal peer TTL reduced from 60s/30s to 30s/10s for faster failover detection
- Added timeout upper bound validation (30s cap) for Redis operations
- Added local registry fallback when Redis lookup fails
- Signal reconnect handler includes backoff and re-subscribe backstop
- Signal liveness check for local registration
- Signal zombie cleanup on peer disconnect

### Files Changed
- `shared/distributed/redis.go` - TLS, pool limits, reconnection
- `shared/distributed/config.go` - HAConfig extensions, SanitizeRedisKey()
- `management/server/distributed/lock.go` - ownership check, error returns, goroutine cleanup
- `signal/server/signal.go` - HMAC signing, reconnect, liveness, zombie cleanup
- `signal/server/config.go` - PeerTTL adjustments
- `management/internals/shared/grpc/loginfilter.go` - uint64, TOCTOU, cleanup goroutine
- `management/internals/modules/peers/ephemeral/manager/ephemeral.go` - atomic ZRem
- `management/server/types/proxy_access_token.go` - HMAC-SHA256 checksum
- `combined/cmd/root.go` - enhanced DSN masking
- `.env.example` - NB_HA_REDIS_PASSWORD

## [2026-04-23] - Initial HA Implementation

### Added
- Active-active horizontal scaling for Signal and Management servers
- Redis-based distributed state for cross-instance coordination
- Distributed locks with TTL and heartbeat
- Pub/sub forwarding between Signal instances
- Login filter with Redis-backed rate limiting
- Ephemeral peer management with TTL-based cleanup
- Account update broadcast to all Management instances
- Peer registry with Redis Hash and TTL
- TURN/relay stateless credential refresh
- Docker Compose HA test environment
- Integration test suite (14 tests)

### Architecture
- Signal instances share peer registry via Redis HSET
- Management instances share state via PostgreSQL and coordinate via Redis locks
- Graceful degradation when Redis is unavailable
- Backward compatible with single-instance deployments
