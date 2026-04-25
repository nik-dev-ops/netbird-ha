#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# init-test-data.sh
# Idempotent initialization of NetBird HA integration test data.
# ------------------------------------------------------------------------------
set -euo pipefail

: "${POSTGRES_DSN:=postgres://netbird:netbird@postgres.nb-ha.local:5432/netbird?sslmode=disable}"
: "${MGMT1_ADDR:=mgmt-1.nb-ha.local:33073}"
: "${MGMT_TOKEN:=}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_KEY_NAME="ha-integration-test-key"
OWNER_EMAIL="admin@nb-ha.local"
OWNER_PASSWORD="Admin123!"
OWNER_NAME="Test Admin"

log() {
    echo "[init-test-data] $*"
}

# Wait for Postgres to be ready.
wait_for_postgres() {
    log "Waiting for PostgreSQL..."
    for i in {1..30}; do
        if pg_isready -d "$POSTGRES_DSN" >/dev/null 2>&1; then
            log "PostgreSQL is ready"
            return 0
        fi
        sleep 2
    done
    log "ERROR: PostgreSQL did not become ready in time"
    return 1
}

# Wait for management server to be ready.
wait_for_mgmt() {
    log "Waiting for management server at $MGMT1_ADDR..."
    for i in {1..30}; do
        if curl -sf "http://$MGMT1_ADDR/healthz" >/dev/null 2>&1; then
            log "Management server is ready"
            return 0
        fi
        sleep 2
    done
    log "ERROR: Management server did not become ready in time"
    return 1
}

# Check if setup is required and create owner if so.
setup_instance() {
    local status_json
    status_json=$(curl -sf "http://$MGMT1_ADDR/api/instance" 2>/dev/null || echo '{"setup_required":false}')
    local setup_required
    setup_required=$(echo "$status_json" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("setup_required", False))' 2>/dev/null || echo "false")

    if [[ "$setup_required" == "True" || "$setup_required" == "true" ]]; then
        log "Instance setup required; creating owner user $OWNER_EMAIL"
        curl -sf "http://$MGMT1_ADDR/api/setup" \
            -X POST \
            -H "Content-Type: application/json" \
            -d "{\"email\":\"$OWNER_EMAIL\",\"password\":\"$OWNER_PASSWORD\",\"name\":\"$OWNER_NAME\"}" \
            >/dev/null
        log "Owner user created"
    else
        log "Instance already set up"
    fi
}

# Fetch the owner user ID from the database.
get_owner_user_id() {
    psql "$POSTGRES_DSN" -tA -c "
        SELECT id FROM users WHERE email = '$OWNER_EMAIL' LIMIT 1;
    " 2>/dev/null || true
}

# Idempotent setup key creation via direct SQL.
# We ensure at least one reusable setup key exists for integration tests.
ensure_setup_key() {
    local account_id
    account_id=$(psql "$POSTGRES_DSN" -tA -c "
        SELECT id FROM accounts LIMIT 1;
    " 2>/dev/null || true)

    if [[ -z "$account_id" ]]; then
        log "WARNING: No account found in database"
        return 0
    fi

    local existing_key
    existing_key=$(psql "$POSTGRES_DSN" -tA -c "
        SELECT key FROM setup_keys WHERE name = '$SETUP_KEY_NAME' LIMIT 1;
    " 2>/dev/null || true)

    if [[ -n "$existing_key" ]]; then
        log "Setup key '$SETUP_KEY_NAME' already exists"
        return 0
    fi

    local key_id key_value key_secret
    key_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
    key_value=$(python3 -c 'import uuid; print(str(uuid.uuid4()).upper())')
    key_secret=$(echo -n "$key_value" | sha256sum | awk '{print $1}')

    psql "$POSTGRES_DSN" -c "
        INSERT INTO setup_keys (
            id, account_id, key, key_secret, name, type,
            created_at, updated_at, revoked, used_times,
            auto_groups, usage_limit, ephemeral, allow_extra_dns_labels
        ) VALUES (
            '$key_id', '$account_id', '$key_value', '$key_secret', '$SETUP_KEY_NAME', 'reusable',
            NOW(), NOW(), false, 0,
            '[]'::jsonb, 0, false, false
        )
        ON CONFLICT (id) DO NOTHING;
    " 2>/dev/null || true

    log "Setup key '$SETUP_KEY_NAME' created ($key_value)"
}

# Idempotent PAT creation for HTTP API access in tests.
ensure_pat() {
    local user_id
    user_id=$(get_owner_user_id)
    if [[ -z "$user_id" ]]; then
        log "WARNING: Owner user not found; skipping PAT creation"
        return 0
    fi

    local existing_pat
    existing_pat=$(psql "$POSTGRES_DSN" -tA -c "
        SELECT hashed_token FROM personal_access_tokens WHERE name = 'ha-test-pat' AND user_id = '$user_id' LIMIT 1;
    " 2>/dev/null || true)

    if [[ -n "$existing_pat" ]]; then
        log "PAT already exists for user $user_id"
        return 0
    fi

    local pat_id pat_plain pat_hashed
    pat_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
    pat_plain="nbp_$(python3 -c 'import secrets; print(secrets.token_urlsafe(30))')"
    pat_hashed=$(echo -n "$pat_plain" | sha256sum | awk '{print $1}')

    psql "$POSTGRES_DSN" -c "
        INSERT INTO personal_access_tokens (
            id, user_id, name, hashed_token,
            expiration_date, created_by, created_at, last_used
        ) VALUES (
            '$pat_id', '$user_id', 'ha-test-pat', '$pat_hashed',
            NOW() + INTERVAL '7 days', '$user_id', NOW(), NULL
        )
        ON CONFLICT (id) DO NOTHING;
    " 2>/dev/null || true

    # Export for test runner.
    echo "MGMT_TOKEN=$pat_plain"
    log "PAT created for user $user_id"
}

# Idempotent test peer creation in database.
# Creates peers that can be used by integration tests without full client setup.
ensure_test_peers() {
    local account_id
    account_id=$(psql "$POSTGRES_DSN" -tA -c "
        SELECT id FROM accounts LIMIT 1;
    " 2>/dev/null || true)

    if [[ -z "$account_id" ]]; then
        log "WARNING: No account found; skipping test peer creation"
        return 0
    fi

    for peer_name in "test-peer-1" "test-peer-2" "test-peer-3"; do
        local existing_peer
        existing_peer=$(psql "$POSTGRES_DSN" -tA -c "
            SELECT id FROM peers WHERE name = '$peer_name' AND account_id = '$account_id' LIMIT 1;
        " 2>/dev/null || true)

        if [[ -n "$existing_peer" ]]; then
            continue
        fi

        local peer_id peer_key peer_ip
        peer_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
        peer_key="$(echo -n "$peer_name-$account_id" | sha256sum | awk '{print $1}')"
        peer_ip="100.64.$(shuf -i 1-254 -n 1).$(shuf -i 1-254 -n 1)"

        psql "$POSTGRES_DSN" -c "
            INSERT INTO peers (
                id, account_id, key, ip, name, dns_label,
                ssh_key, ssh_enabled, login_expiration_enabled,
                inactivity_expiration_enabled, ephemeral, created_at,
                meta_hostname, meta_goos, meta_goarch, meta_netbird_version,
                meta_kernel_version, meta_ui_version, meta_extra_dns_labels,
                peer_status_last_seen, peer_status_connected, peer_status_login_expired, peer_status_requires_approval,
                proxy_meta_embedded, proxy_meta_cluster
            ) VALUES (
                '$peer_id', '$account_id', '$peer_key', '$peer_ip'::jsonb, '$peer_name', '$peer_name',
                '', false, false,
                false, false, NOW(),
                '', '', '', '', '', '', '[]'::jsonb,
                NOW(), false, false, false,
                false, ''
            )
            ON CONFLICT (id) DO NOTHING;
        " 2>/dev/null || true

        log "Test peer '$peer_name' created"
    done
}

# Main flow.
main() {
    log "Starting idempotent test data initialization"
    wait_for_postgres
    wait_for_mgmt
    setup_instance
    ensure_setup_key
    ensure_pat
    ensure_test_peers
    log "Initialization complete"
}

main "$@"
