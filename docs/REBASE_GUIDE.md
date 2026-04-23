# Rebase & Maintenance Guide

This document explains how to keep the HA fork in sync with upstream NetBird releases.

## Fork Strategy

The HA changes are **isolated to specific files** with detailed inline comments. Each significant modification (signal state sharing, management scaling, configuration options) is its own commit with descriptive messages following conventional commit format.

### Commit History on `ha/main`

```
1ef78d3 feat(ha): implement active-active horizontal scaling for Signal and Management servers
f732b01 [management] unify peer-update test timeout via constant (#5952)  <-- upstream base
```

All HA changes are contained in a single squashed commit to simplify rebasing.

## Files That WILL Conflict on Rebase

These files are modified by both upstream and the HA fork. Expect conflicts:

### High Conflict Risk (Modified by HA fork + frequently changed upstream)

| File | Why It Conflicts | Resolution Strategy |
|------|-----------------|---------------------|
| `signal/server/signal.go` | Core signal logic extended with Redis registry, pub/sub, heartbeats | Keep upstream changes, re-apply HA hooks (search for `// HA:` comments) |
| `management/internals/shared/grpc/server.go` | gRPC server extended with distributed locks | Keep upstream changes, re-apply `WithLock()` calls |
| `management/internals/shared/grpc/loginfilter.go` | Login filter extended with Redis Hash state | Keep upstream changes, re-apply Redis-backed filter |
| `management/internals/shared/grpc/token_mgr.go` | Timer-based state removed for stateless credentials | Keep upstream changes, verify stateless approach still works |
| `management/internals/modules/peers/ephemeral/manager/ephemeral.go` | ZSET-based ephemeral tracking | Keep upstream changes, re-apply Redis ZSET logic |
| `signal/cmd/run.go` | CLI flags added for HA | Append HA flags after upstream flags |
| `signal/cmd/root.go` | HA config wired into signal init | Re-apply HA config injection |
| `management/cmd/management.go` | CLI flags added for HA | Append HA flags after upstream flags |
| `combined/cmd/root.go` | HA config wired into combined mode | Re-apply HA config injection |

### Medium Conflict Risk

| File | Why It Conflicts | Resolution Strategy |
|------|-----------------|---------------------|
| `management/internals/server/server.go` | Redis client wired into lifecycle | Re-apply `RedisClient` field and initialization |
| `management/internals/server/boot.go` | Redis client initialized during boot | Re-apply `initRedis()` call |
| `management/internals/server/controllers.go` | Redis client passed to controllers | Re-apply `WithRedisClient()` calls |
| `management/internals/server/config/config.go` | `HAConfig` field added | Re-apply `HAConfig` field |
| `management/internals/controllers/network_map/update_channel/updatechannel.go` | Pub/sub publisher added | Re-apply `PublishAccountUpdate()` call |
| `management/internals/controllers/network_map/controller/controller.go` | Subscriber added | Re-apply `SubscribeAccountUpdates()` call |
| `signal/metrics/app.go` | HA metrics added | Append HA metrics after upstream metrics |

### Low Conflict Risk (New files, unlikely to conflict)

These files are **new** and won't conflict unless upstream adds files with the same name:

- `shared/distributed/config.go`
- `shared/distributed/redis.go`
- `management/server/distributed/config.go`
- `management/server/distributed/lock.go`
- `management/server/distributed/registry.go`
- `signal/server/config.go`
- `.env.example`
- `docker-compose.ha-test.yml`
- `tests/integration/**`
- `docs/**`

## Rebase Procedure

### Step 1: Prepare

```bash
# Add upstream remote if not already added
git remote add upstream https://github.com/netbirdio/netbird.git
git fetch upstream

# Create a backup branch
git branch ha/main-backup-$(date +%Y%m%d)
```

### Step 2: Start Rebase

```bash
# Start interactive rebase onto latest upstream
git rebase -i upstream/main
```

### Step 3: Handle Conflicts (Expected)

When conflicts occur, identify which file is conflicting:

```bash
git status  # Shows conflicted files
```

For each conflicted file:

#### Strategy A: Upstream changes are minor (most common)

1. Accept upstream version: `git checkout --theirs <file>`
2. Re-apply HA changes manually
3. Mark resolved: `git add <file>`

#### Strategy B: Upstream changes are significant

1. Open the file and examine the conflict markers
2. Merge upstream changes with HA changes
3. Look for `// HA:` comments in the file to identify HA-specific sections
4. Mark resolved: `git add <file>`

### Step 4: Continue Rebase

```bash
git rebase --continue
```

Repeat Steps 3-4 until rebase completes.

### Step 5: Validate

```bash
# Build all binaries
go build ./signal/...
go build ./management/...
go build ./shared/...
go build ./combined/...

# Run integration tests
cd tests/integration
go test -v -count=1 -timeout 300s
```

### Step 6: Push

```bash
# Force push the rebased branch
git push --force-with-lease origin ha/main
```

## Alternative: Cherry-Pick Approach

If rebase becomes too complex, cherry-pick individual HA commits onto a fresh upstream branch:

```bash
# Create fresh branch from upstream
git checkout -b ha/main-v0.70 upstream/main

# Cherry-pick the single HA commit
git cherry-pick 1ef78d3

# Resolve conflicts, then continue
git cherry-pick --continue
```

## Key Patterns to Preserve During Rebase

### 1. Nil Redis Client Checks

Every HA feature must check if the Redis client is nil before using it:

```go
if s.redisClient != nil {
    // HA behavior
} else {
    // Non-HA behavior (same as upstream)
}
```

### 2. NoopLock Fallback

When HA is disabled, use `NoopLock` instead of Redis locks:

```go
lock := NewNoopLock() // or redis-based lock when HA enabled
```

### 3. Env Var Auto-Mapping

Signal CLI flags are auto-populated from env vars:

```go
// In signal/cmd/run.go
setFlagsFromEnvVars(rootCmd)
```

### 4. HA Comments

All HA-specific code sections are marked with `// HA:` comments:

```go
// HA: Register peer in distributed registry
if s.haConfig.Enabled && s.redisClient != nil {
    s.redisClient.HSet(ctx, s.haConfig.RegistryKey, peerKey, s.instanceID)
}
```

## Testing After Rebase

Always run the full integration test suite after rebasing:

```bash
# Start fresh environment
docker compose -f docker-compose.ha-test.yml down -v
docker compose -f docker-compose.ha-test.yml up -d --build

# Wait for health
sleep 30

# Run all tests
cd tests/integration
go test -v -count=1 -timeout 300s
```

## Common Rebase Issues

### Issue: Upstream changed signal/server/signal.go structure

**Symptom**: Conflicts in `ConnectStream` or `Send` methods.

**Fix**: Keep upstream method signatures. Re-apply HA hooks inside the methods:

```go
func (s *Server) ConnectStream(stream proto.SignalExchange_ConnectStreamServer) error {
    // upstream code...
    
    // HA: Register peer in Redis
    if s.haConfig.Enabled && s.redisClient != nil {
        s.registerPeer(peerKey)
    }
    
    // upstream code continues...
}
```

### Issue: Upstream changed management gRPC server initialization

**Symptom**: Conflicts in `NewServer()` constructor.

**Fix**: Keep upstream constructor. Re-apply `WithRedisClient()` and `WithLock()` options.

### Issue: Upstream added new CLI flags

**Symptom**: Conflicts in `signal/cmd/run.go` or `management/cmd/management.go`.

**Fix**: Keep upstream flags. Append HA flags at the end.

### Issue: Tests fail after rebase

**Symptom**: Integration tests fail with connection errors or missing data.

**Fix**:
1. Check if upstream changed gRPC protobuf definitions
2. Check if upstream changed management config format
3. Check if upstream changed peer authentication flow
4. Rebuild Docker images: `docker compose -f docker-compose.ha-test.yml build --no-cache`

## Version Tracking

Keep a `REBASE_LOG.md` file to track each rebase:

```markdown
# Rebase Log

## 2026-04-24: Rebased onto v0.69.0
- Upstream changes: peer status refactor, new metrics
- Conflicts: signal/server/signal.go, management/internals/shared/grpc/server.go
- Resolution: Accepted upstream, re-applied HA hooks
- Tests: All 14 pass
- Commits cherry-picked: 1ef78d3 (single squashed commit)

## 2026-05-15: Rebased onto v0.70.0
- ...
```

## Automated Rebase Script

```bash
#!/bin/bash
# scripts/rebase-upstream.sh

set -e

UPSTREAM_TAG="${1:-main}"
BACKUP_BRANCH="ha/main-backup-$(date +%Y%m%d)"

echo "Creating backup branch: $BACKUP_BRANCH"
git branch "$BACKUP_BRANCH"

echo "Fetching upstream..."
git fetch upstream "$UPSTREAM_TAG"

echo "Starting rebase..."
if git rebase upstream/$UPSTREAM_TAG; then
    echo "Rebase successful!"
    echo "Building..."
    go build ./signal/...
    go build ./management/...
    go build ./shared/...
    go build ./combined/...
    echo "Build OK. Run integration tests manually."
else
    echo "Rebase has conflicts. Resolve them and run:"
    echo "  git rebase --continue"
    echo "If rebase is too complex, abort and use cherry-pick:"
    echo "  git rebase --abort"
    echo "  git checkout -b ha/main-new upstream/$UPSTREAM_TAG"
    echo "  git cherry-pick <ha-commits>"
fi
```

## Summary

- **Rebase frequency**: After each upstream release (monthly)
- **Expected conflicts**: 5-10 files per rebase
- **Time estimate**: 30-60 minutes per rebase
- **Critical files**: `signal/server/signal.go`, `management/internals/shared/grpc/*.go`
- **Safety**: Always create a backup branch before rebasing
- **Validation**: Always run integration tests after rebasing
