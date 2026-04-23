# NetBird HA Fork

**⚠️ This is a fork of [netbirdio/netbird](https://github.com/netbirdio/netbird) with added horizontal scaling support.**

## Changes from Upstream

- **Signal Server**: Active-active scaling via Redis distributed registry and pub/sub
- **Management Server**: Active-active scaling via Redis update broadcast and distributed locks
- **Configuration**: All HA parameters externally configurable (env vars + YAML)

## Rebase Strategy

See [docs/REBASE_GUIDE.md](docs/REBASE_GUIDE.md) for per-file conflict guidance and step-by-step rebase instructions.

## Original README

See [original_readme.md](original_readme.md) for the upstream NetBird project documentation.
