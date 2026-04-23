# Build & Deployment Guide

This document covers building NetBird HA binaries and deploying the full stack with Docker Compose.

## Prerequisites

| Requirement | Version | Notes |
|------------|---------|-------|
| Go | 1.25.5 | `CGO_ENABLED=1` required for SQLite (embedded IdP) |
| Docker | 29.x+ | With BuildKit enabled |
| Docker Compose | v2+ | Plugin or standalone |
| Linux kernel | 5.6+ | For WireGuard (or wireguard-dkms) |
| make | any | Optional, for convenience |

## Building Binaries

### All Binaries

```bash
# From project root
cd /home/nino/git/netbird_ha

# Management server
CGO_ENABLED=1 go build -o netbird-mgmt ./management/

# Signal server
CGO_ENABLED=1 go build -o netbird-signal ./signal/

# Combined server (signal + management in one binary)
CGO_ENABLED=1 go build -o netbird-server ./combined/

# Relay server
CGO_ENABLED=1 go build -o netbird-relay ./relay/

# Client (for agent images)
CGO_ENABLED=1 go build -o netbird ./client/
```

### Individual Components

```bash
# Management only
go build -o bin/netbird-mgmt ./management/

# Signal only
go build -o bin/netbird-signal ./signal/

# Client only
go build -o bin/netbird ./client/
```

### Verify Builds

```bash
# All packages should compile without errors
go build ./signal/...
go build ./management/...
go build ./shared/...
go build ./combined/...
```

## Building Docker Images

### Full Stack

```bash
# Build all images defined in docker-compose.ha-test.yml
docker compose -f docker-compose.ha-test.yml build

# Or build specific services
docker compose -f docker-compose.ha-test.yml build signal-1 signal-2
docker compose -f docker-compose.ha-test.yml build mgmt-1 mgmt-2
```

### Individual Images

```bash
# Management
docker build -f management/Dockerfile -t netbird-mgmt:ha .

# Signal
docker build -f signal/Dockerfile -t netbird-signal:ha .

# Test runner
docker build -f tests/integration/Dockerfile.test -t netbird-test-runner:ha .

# Agent
docker build -f tests/integration/Dockerfile.agent -t netbird-agent:ha .
```

## Deploying with Docker Compose

### Step 1: Environment Setup

```bash
# Copy the example environment file
cp .env.example .env

# Edit .env to match your environment
# At minimum, verify these:
# - NB_DOMAIN (default: nb-ha.local)
# - NB_REDIS_ADDRESS
# - NB_POSTGRES_HOST
# - All STUN/TURN/relay URLs
```

### Step 2: Start Infrastructure

```bash
# Start Redis and PostgreSQL first
docker compose -f docker-compose.ha-test.yml up -d redis postgres

# Wait for them to be healthy (10-20s)
docker compose -f docker-compose.ha-test.yml ps
```

### Step 3: Start All Services

```bash
# Start the full stack
docker compose -f docker-compose.ha-test.yml up -d

# Or start in foreground to see logs
docker compose -f docker-compose.ha-test.yml up
```

### Step 4: Verify Deployment

```bash
# Check all containers are running and healthy
docker compose -f docker-compose.ha-test.yml ps

# Expected output:
# NAME           IMAGE                 STATUS
# nb-redis       redis:7-alpine        Up 30s (healthy)
# nb-postgres    postgres:15-alpine    Up 30s (healthy)
# nb-coturn      coturn/coturn         Up 30s
# nb-relay       netbird-relay:ha      Up 30s (healthy)
# nb-signal-1    netbird-signal:ha     Up 30s (healthy)
# nb-signal-2    netbird-signal:ha     Up 30s (healthy)
# nb-mgmt-1      netbird-mgmt:ha       Up 30s (healthy)
# nb-mgmt-2      netbird-mgmt:ha       Up 30s (healthy)
# nb-dashboard   netbirdio/dashboard   Up 30s
# nb-traefik     traefik:v3.6          Up 30s
```

### Step 5: Health Checks

```bash
# Signal-1 metrics
curl http://localhost:9093/metrics

# Signal-2 metrics
curl http://localhost:9094/metrics

# Management-1 metrics
curl http://localhost:9091/metrics

# Management-2 metrics
curl http://localhost:9092/metrics

# Traefik dashboard (API)
curl http://localhost:8089/api/http/services

# NetBird dashboard (UI)
curl -I http://localhost:8088
```

### Step 6: Access the Dashboard

1. Open `http://localhost:8088` in a browser
2. Log in with the embedded IdP:
   - Email: `admin@nb-ha.local`
   - Password: `testadmin123`
3. If you see "User Approval Pending", run:
   ```bash
   docker exec nb-postgres psql -U netbird -d netbird \
     -c "UPDATE users SET pending_approval=false, blocked=false WHERE email='admin@nb-ha.local';"
   ```
4. Refresh the page

## Service Endpoints

### Exposed Ports

| Service | Host Port | Container Port | Purpose |
|---------|-----------|----------------|---------|
| Traefik HTTP | 8088 | 80 | Dashboard, API, IdP, gRPC |
| Traefik HTTPS | 8443 | 443 | TLS termination |
| Traefik API | 8089 | 8080 | Traefik internal API |
| Management-1 | 33073 | 33073 | gRPC + HTTP API |
| Management-2 | 33074 | 33073 | gRPC + HTTP API |
| Signal-1 | 10000 | 10000 | gRPC signaling |
| Signal-2 | 10001 | 10000 | gRPC signaling |
| Redis | 6379 | 6379 | Cache & pub/sub |
| PostgreSQL | 5432 | 5432 | Database |
| TURN | 3478 | 3478 | STUN/TURN |
| Relay | 443 | 443 | Relay fallback |
| Mgmt-1 metrics | 9091 | 9090 | Prometheus metrics |
| Mgmt-2 metrics | 9092 | 9090 | Prometheus metrics |
| Signal-1 metrics | 9093 | 9090 | Prometheus metrics |
| Signal-2 metrics | 9094 | 9090 | Prometheus metrics |
| Relay metrics | 9095 | 9090 | Prometheus metrics |

### Internal Docker DNS

Inside the Docker network, services resolve by hostname:

| Hostname | Service |
|----------|---------|
| `redis.nb-ha.local` | Redis |
| `postgres.nb-ha.local` | PostgreSQL |
| `signal-1.nb-ha.local` | Signal-1 |
| `signal-2.nb-ha.local` | Signal-2 |
| `mgmt-1.nb-ha.local` | Management-1 |
| `mgmt-2.nb-ha.local` | Management-2 |
| `relay.nb-ha.local` | Relay |
| `turn.nb-ha.local` | coturn |
| `traefik.nb-ha.local` | Traefik |

## Scaling

### Add More Signal Instances

Edit `docker-compose.ha-test.yml` and add:

```yaml
  signal-3:
    extends:
      service: signal-1
    container_name: nb-signal-3
    hostname: signal-3.${NB_DOMAIN}
    environment:
      NB_HA_INSTANCE_ID: signal-3
    ports:
      - "10002:10000"
      - "9096:9090"
    labels:
      - traefik.enable=true
      - traefik.http.services.signal-h2c.loadbalancer.server.port=10000
      - traefik.http.services.signal-h2c.loadbalancer.server.scheme=h2c
```

### Add More Management Instances

```yaml
  mgmt-3:
    extends:
      service: mgmt-1
    container_name: nb-mgmt-3
    hostname: mgmt-3.${NB_DOMAIN}
    environment:
      NB_HA_INSTANCE_ID: mgmt-3
    ports:
      - "33075:33073"
      - "9096:9090"
    labels:
      - traefik.enable=true
      - traefik.http.services.mgmt-api.loadbalancer.server.port=33073
      - traefik.http.services.mgmt-grpc.loadbalancer.server.port=33073
      - traefik.http.services.mgmt-grpc.loadbalancer.server.scheme=h2c
```

## Production Deployment Considerations

### Redis

- Use **Redis Sentinel** or **Redis Cluster** for production (not single instance)
- Enable persistence (`appendonly yes`)
- Set appropriate maxmemory policy (`allkeys-lru`)

### PostgreSQL

- Use PostgreSQL 15+ with streaming replication for HA
- Enable connection pooling (PgBouncer)
- Regular backups

### Traefik

- Enable TLS with Let's Encrypt or custom certificates
- Configure rate limiting
- Enable access logs

### Monitoring

All services expose Prometheus metrics:

```yaml
# prometheus.yml scrape config
scrape_configs:
  - job_name: 'netbird-mgmt'
    static_configs:
      - targets: ['localhost:9091', 'localhost:9092']
  - job_name: 'netbird-signal'
    static_configs:
      - targets: ['localhost:9093', 'localhost:9094']
  - job_name: 'netbird-relay'
    static_configs:
      - targets: ['localhost:9095']
```

## Troubleshooting

### Container fails to start

```bash
# Check logs
docker logs nb-mgmt-1 --tail 50
docker logs nb-signal-1 --tail 50

# Check for port conflicts
sudo lsof -i :33073
sudo lsof -i :10000
```

### Redis connection errors

```bash
# Verify Redis is running
docker exec nb-redis redis-cli ping

# Check signal can reach Redis
docker exec nb-signal-1 nslookup redis.nb-ha.local
```

### PostgreSQL connection errors

```bash
# Verify PostgreSQL
docker exec nb-postgres psql -U netbird -d netbird -c "SELECT 1;"

# Check management can reach PostgreSQL
docker exec nb-mgmt-1 wget -qO- postgres.nb-ha.local:5432
```

### Traefik routing issues

```bash
# Check Traefik services
curl -s http://localhost:8089/api/http/services | python3 -m json.tool

# Check Traefik routers
curl -s http://localhost:8089/api/http/routers | python3 -m json.tool

# Test direct signal connection
curl -v http://localhost:10000/signalexchange.SignalExchange/ConnectStream
```

### Test agent connection issues

```bash
# Check agent status
docker exec nb-agent-a netbird status

# Check agent logs
docker logs nb-agent-a --tail 30

# Verify agent can reach management
docker exec nb-agent-a wget -qO- http://traefik.nb-ha.local:80/api/users/current
```

## Cleanup

```bash
# Stop all services
docker compose -f docker-compose.ha-test.yml down

# Stop and remove volumes (WARNING: deletes PostgreSQL data!)
docker compose -f docker-compose.ha-test.yml down -v

# Remove all images
docker compose -f docker-compose.ha-test.yml down --rmi all
```
