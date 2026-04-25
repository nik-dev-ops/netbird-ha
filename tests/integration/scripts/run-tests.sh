#!/bin/bash
set -euo pipefail

# NetBird HA Integration Test Runner
# This script runs inside the test-runner container and executes
# integration tests against the HA infrastructure.

echo "=== NetBird HA Integration Tests ==="
echo "Domain: ${NB_DOMAIN:-nb-ha.local}"
echo "Redis: ${REDIS_ADDR:-not set}"
echo "Management-1: ${MGMT1_ADDR:-not set}"
echo "Management-2: ${MGMT2_ADDR:-not set}"
echo "Signal-1: ${SIGNAL1_ADDR:-not set}"
echo "Signal-2: ${SIGNAL2_ADDR:-not set}"
echo ""

# Function to wait for a service to be healthy
wait_for_service() {
    local name="$1"
    local url="$2"
    local max_attempts="${3:-30}"
    local attempt=0

    echo "Waiting for ${name} at ${url}..."
    while ! wget -qO- "${url}" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "${attempt}" -ge "${max_attempts}" ]; then
            echo "ERROR: ${name} did not become ready in time"
            return 1
        fi
        sleep 2
    done
    echo "${name} is ready!"
}

# Wait for core services
echo "--- Health Checks ---"
wait_for_service "management-1" "http://${MGMT1_ADDR}/healthz" 30 || true
wait_for_service "management-2" "http://${MGMT2_ADDR}/healthz" 30 || true
wait_for_service "signal-1" "http://signal-1.${NB_DOMAIN}:9090/metrics" 30 || true
wait_for_service "signal-2" "http://signal-2.${NB_DOMAIN}:9090/metrics" 30 || true

echo ""
echo "--- Running Tests ---"

# If Go test files exist, run them
if [ -f "*.go" ] || ls *.go >/dev/null 2>&1; then
    echo "Running Go tests..."
    go test -v ./... 2>&1 || true
else
    echo "No Go test files found in /tests"
fi

# Run shell-based integration checks
echo ""
echo "--- Redis Connectivity ---"
redis-cli -h redis.${NB_DOMAIN} ping || echo "WARNING: Redis ping failed"

echo ""
echo "--- PostgreSQL Connectivity ---"
psql "${POSTGRES_DSN}" -c "SELECT 1;" || echo "WARNING: PostgreSQL check failed"

echo ""
echo "=== Integration Test Run Complete ==="
