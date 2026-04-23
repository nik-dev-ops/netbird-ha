#!/bin/bash
set -euo pipefail

# Build script for NetBird HA test environment
# Builds all required binaries for Docker images
#
# Usage:
#   ./tests/integration/scripts/build.sh

cd "$(dirname "$0")/../.."
PROJECT_ROOT="$(pwd)"

echo "=== Building NetBird HA Binaries ==="
echo "Project root: ${PROJECT_ROOT}"

# Determine version info from git if available
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo 'dev')}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

LDFLAGS="-s -w \
  -X github.com/netbirdio/netbird/version.version=${VERSION} \
  -X main.commit=${COMMIT} \
  -X main.date=${DATE} \
  -X main.builtBy=build.sh"

echo "Version: ${VERSION}"
echo "Commit: ${COMMIT}"
echo ""

# Build management server (requires CGO for SQLite support)
echo "Building management server (netbird-mgmt)..."
CGO_ENABLED=1 go build -ldflags "${LDFLAGS}" -o netbird-mgmt ./management/

# Build signal server (CGO disabled for static binary)
echo "Building signal server (netbird-signal)..."
CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o netbird-signal ./signal/

# Build relay server (CGO disabled for static binary)
echo "Building relay server (netbird-relay)..."
CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o netbird-relay ./relay/

# Build combined server (requires CGO)
echo "Building combined server (netbird-server)..."
CGO_ENABLED=1 go build -ldflags "${LDFLAGS}" -o netbird-server ./combined/

# Build client (CGO disabled for static binary)
echo "Building netbird client..."
CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o netbird ./client/

echo ""
echo "=== All binaries built successfully ==="
echo "Binaries:"
ls -la netbird-mgmt netbird-signal netbird-relay netbird-server netbird 2>/dev/null || true
echo ""
echo "Next steps:"
echo "  1. cp .env.example .env"
echo "  2. Edit .env with your desired values"
echo "  3. docker compose -f docker-compose.ha-test.yml up --build"
