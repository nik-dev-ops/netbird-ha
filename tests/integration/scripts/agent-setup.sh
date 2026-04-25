#!/bin/bash
set -e

# Agent setup script for NetBird HA testing
# This script configures the netbird agent and connects it to the management server

echo "=== NetBird Agent Setup ==="
echo "Management URL: $NB_MANAGEMENT_URL"
echo "Setup Key: $NB_SETUP_KEY"

# Enable IP forwarding
sysctl -w net.ipv4.ip_forward=1 2>/dev/null || true
sysctl -w net.ipv6.conf.all.forwarding=1 2>/dev/null || true

# Create netbird config directory
mkdir -p /etc/netbird

# Install and start netbird service if not already done
if ! netbird service status 2>/dev/null; then
    echo "Installing netbird service..."
    netbird service install
    echo "Starting netbird service..."
    netbird service start
    sleep 2
fi

# If setup key is provided, run netbird up
if [ -n "$NB_SETUP_KEY" ]; then
    echo "Connecting to management server..."
    netbird up --management-url "$NB_MANAGEMENT_URL" --setup-key "$NB_SETUP_KEY" || true
else
    echo "No setup key provided. Waiting for manual registration..."
    echo "Run: netbird up --management-url $NB_MANAGEMENT_URL"
fi

# Keep container running
exec "$@"
