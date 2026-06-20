# Tavily Smart Router

A lightweight, deterministic, single-binary HTTP proxy written in Go for rotating multiple Tavily API keys. Designed for AI agents that need reliable Tavily API access with automatic key rotation, failover, and observability.

Built on the same philosophy as [opencode-smart-router](https://github.com/puzzithinker/opencode-smart-router): simplicity, low resource usage, and predictability — especially for resource-constrained environments like Raspberry Pi 4.

## Features

- **Transparent Key Rotation** — Automatically cycles through multiple Tavily API keys. Callers see a single endpoint, the router handles the rest.
- **Automatic Failover** — When a key hits a rate limit (429), the router retries with the next available key. Callers never see the retry.
- **Smart Error Classification** — Distinguishes rate limits, auth failures, quota exhaustion, and upstream errors. Each error type triggers the appropriate key state transition.
- **Tavily-Specific Error Handling** — Handles Tavily-unique status codes 432 (plan limit) and 433 (PayGo limit) as temporary cooldown (quotas reset monthly).
- **Consecutive Failure Tracking** — Keys enter cooldown only after N consecutive failures (configurable), preventing transient 5xx errors from disabling keys prematurely.
- **Two Rotation Strategies** — `least_used` (default) balances load across keys; `round_robin` cycles evenly.
- **Health Check** — `POST /search` with a minimal payload to verify both key validity and upstream connectivity.
- **Prometheus Metrics** — 6 metrics for monitoring key health, request rates, and error types.
- **Admin Dashboard** — `/admin/stats` endpoint with basic auth for real-time key state inspection.
- **Graceful Shutdown** — Drains in-flight requests on SIGTERM.
- **RPi 4 Friendly** — Static binary, distroless Docker image, optional logging/metrics for minimal resource usage.

## Quick Start

### Binary

```bash
# Build
make build

# Configure
cp config.example.json config.json
# Edit config.json — add your Tavily API keys

# Run
./bin/tavily-router
```

### Docker

```bash
# Build image
make docker

# Run with environment variables
docker run -d \
  -p 8082:8082 \
  -e TAVILY_KEYS=tvly-key1,tvly-key2,tvly-key3 \
  tavily-router
```

### Docker Compose

```bash
# Copy and edit config
cp config.example.json config.json

# Set your keys
export TAVILY_KEYS="tvly-key1,tvly-key2,tvly-key3"

# Start
docker compose up -d
```

## Configuration

### Config File (`config.json`)

```json
{
  "listen_addr": "0.0.0.0:8082",
  "upstream_base": "https://api.tavily.com",
  "keys": ["tvly-YOUR-API-KEY-HERE"],
  "strategy": "least_used",
  "cooldown_sec": 300,
  "max_fails_before_cooldown": 3,
  "quota_cooldown_sec": 86400,
  "health_check_timeout_seconds": 10,
  "admin_user": "admin",
  "admin_pass": "",
  "enable_prometheus": false,
  "enable_request_log": false,
  "log_file": ""
}
```

### Environment Variables (override config file)

| Variable | Overrides |
|---|---|
| `TAVILY_KEYS` | `keys` (comma-separated) |
| `TAVILY_UPSTREAM_BASE` | `upstream_base` |
| `TAVILY_LISTEN_ADDR` | `listen_addr` |
| `TAVILY_STRATEGY` | `strategy` |
| `TAVILY_COOLDOWN_SEC` | `cooldown_sec` |
| `TAVILY_QUOTA_COOLDOWN_SEC` | `quota_cooldown_sec` |
| `TAVILY_CONFIG` | Config file path (default: `config.json`) |

### Config Reference

| Field | Type | Default | Description |
|---|---|---|---|
| `listen_addr` | string | `0.0.0.0:8082` | HTTP bind address. `127.0.0.1` for localhost-only |
| `upstream_base` | string | `https://api.tavily.com` | Tavily API base URL |
| `keys` | []string | *(required)* | Tavily API keys to rotate |
| `strategy` | string | `least_used` | `round_robin` or `least_used` |
| `cooldown_sec` | int | `300` | Cooldown duration in seconds (min 30) |
| `max_fails_before_cooldown` | int | `3` | Consecutive failures before cooldown (min 1) |
| `quota_cooldown_sec` | int | `86400` | Cooldown duration in seconds for quota-exhausted keys. Keys are retried after this period. Quotas reset monthly, so keys auto-recover. Minimum 60. |
| `health_check_timeout_seconds` | int | `10` | Timeout for health check requests |
| `admin_user` | string | `admin` | Basic auth username for `/admin/stats` |
| `admin_pass` | string | `""` | Basic auth password. Empty = admin disabled |
| `enable_prometheus` | bool | `false` | Enable `/metrics` endpoint |
| `enable_request_log` | bool | `false` | Enable structured request logging |
| `log_file` | string | `""` | Log file path (stdout if empty) |

## Usage

### Pointing Your Agent

Configure your AI agent or application to use the router as its Tavily API endpoint:

```bash
# Instead of: https://api.tavily.com/search
# Use: http://localhost:8082/search

# Example with curl
curl -X POST http://localhost:8082/search \
  -H "Content-Type: application/json" \
  -d '{"query": "latest AI news", "max_results": 5}'
```

The router injects the rotated API key as `Authorization: Bearer <key>`. Your agent does **not** need to provide an API key — the router handles authentication.

### Supported Endpoints

All Tavily API paths are proxied:

| Path | Method | Description |
|---|---|---|
| `/search` | POST | Web search |
| `/extract` | POST | Content extraction |
| `/crawl` | POST | Web crawling |
| `/map` | POST | Site mapping |
| `/*` | * | Catch-all for any other Tavily API path |

### Key Rotation Behavior

1. Each request picks the next available key (based on strategy)
2. If the key returns a retryable error (429, 401, 403, 432, 433), the router **transparently retries** with the next key
3. The caller sees only the final result — retries are invisible
4. If all keys are exhausted, a clear error response is returned

### Error Classification

| Status Code | Key State | Retry? | Notes |
|---|---|---|---|
| 2xx | HEALTHY | No | Success — key is marked healthy |
| 401 / 403 | DISABLED | Yes | Invalid key — permanently disabled |
| 429 (rate limit) | COOLDOWN | Yes | Rate limited — cooldown for `Retry-After` or default |
| 429 (quota) | COOLDOWN (quota_cooldown_sec) | Yes | Quota exhausted — cooldown, auto-recovers. Quotas reset monthly |
| 432 | COOLDOWN (quota_cooldown_sec) | Yes | Tavily plan limit — cooldown, auto-recovers. Quotas reset monthly |
| 433 | COOLDOWN (quota_cooldown_sec) | Yes | Tavily PayGo limit — cooldown, auto-recovers. Quotas reset monthly |
| 5xx | Fail tracked | No | Upstream issue — tracked for consecutive failure threshold |
| Timeout | COOLDOWN (10s) | No | Connection issue — short cooldown |

### Consecutive Failure Threshold

`max_fails_before_cooldown` (default: 3) controls how many consecutive 5xx errors before a key enters cooldown. This prevents transient upstream issues from unnecessarily disabling keys.

- 1-2 fails: key stays HEALTHY (fail count increments)
- 3rd fail: key enters COOLDOWN for `cooldown_sec`
- Success or pick: fail count resets to 0

## Endpoints

### `POST /search`, `/extract`, `/crawl`, `/map`, `/*`

Proxied to Tavily API with key rotation and transparent retry.

### `GET /health`

Returns router health and upstream connectivity status.

```json
{
  "status": "healthy",
  "upstream": "reachable",
  "healthy_keys": 2,
  "total_keys": 3,
  "disabled_keys": 1
}
```

Status codes: 200 (healthy), 503 (unhealthy).

The health check sends a real `POST /search` request with a minimal payload to verify both key validity and upstream connectivity.

### `GET /admin/stats`

Protected by basic auth (disabled if `admin_pass` is empty). Returns key state snapshot.

```json
{
  "keys": [
    {
      "masked_key": "tvly-a...xyz",
      "state": "healthy",
      "usage_count": 142,
      "fail_count": 0,
      "last_used": "2026-06-14T08:30:00Z"
    },
    {
      "masked_key": "tvly-b...abc",
      "state": "cooldown",
      "usage_count": 98,
      "fail_count": 0,
      "last_used": "2026-06-14T08:29:45Z"
    }
  ],
  "total_requests": 240,
  "strategy": "least_used"
}
```

### `GET /metrics`

Prometheus metrics (when `enable_prometheus` is `true`).

## Prometheus Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tavily_router_requests_total` | Counter | `key`, `status_group` | Total requests proxied |
| `tavily_router_key_usage_total` | Counter | `key` | Times each key was selected |
| `tavily_router_key_healthy` | Gauge | `key` | 1 if healthy, 0 if cooldown or disabled |
| `tavily_router_request_duration_seconds` | Histogram | `key` | Request latency distribution |
| `tavily_router_key_cooldown_total` | Counter | `key` | Times each key entered cooldown |
| `tavily_router_upstream_errors_total` | Counter | `key`, `error_type` | Upstream errors by type |
| `tavily_router_key_usage_pct` | Gauge | `key` | Each key's share of total traffic (0.0–1.0). Updated every 10s |

Error types: `rate_limit`, `auth_error`, `server_error`, `timeout`, `unknown`.

## Deployment

### Raspberry Pi 4

```bash
# Cross-compile for ARM64
make build-arm64

# Copy to Pi
scp bin/tavily-router-linux-arm64 pi@raspberry:~/tavily-router

# On Pi
ssh pi@raspberry
chmod +x ~/tavily-router
./tavily-router
```

For minimal memory usage, set `GOGC=50` and disable Prometheus and file logging:

```json
{
  "enable_prometheus": false,
  "enable_request_log": false,
  "log_file": ""
}
```

### Systemd (Docker Compose)

```bash
# Install service
sudo cp deploy/systemd/tavily-router.service /etc/systemd/system/
sudo systemctl enable --now tavily-router

# Check status
systemctl status tavily-router

# View logs
journalctl -u tavily-router -f
```

### Docker Compose with Monitoring

```bash
# Start with Prometheus and Grafana
docker compose up -d

# Access
# Router:   http://localhost:8082
# Grafana:  http://localhost:3000 (admin/admin)
# Prometheus: http://localhost:9090
```

## Building

```bash
# Development build
make build

# Production build with version
VERSION=v1.0.0 make build

# ARM64 for Raspberry Pi
make build-arm64

# Run tests
make test

# Full CI check
make ci
```

## Project Structure

```
tavily-smart-router/
├── main.go                      # All business logic
├── main_test.go                 # 69 tests with race detector
├── go.mod                       # Go 1.22, Prometheus client
├── Makefile                     # Build, test, cross-compile targets
├── Dockerfile                   # Multi-stage, distroless, non-root
├── docker-compose.yml           # With Prometheus + Grafana
├── config.example.json          # Minimal config template
├── deploy/
│   └── systemd/
│       └── tavily-router.service
├── docs/
│   ├── architecture.md          # Full architecture documentation
│   └── usage.md                 # Detailed usage guide
└── examples/
    └── config.json              # Realistic 3-key example
```

## Security Considerations

- **Key masking**: Raw keys never appear in logs or API responses. Only `tvly-a...xyz` format.
- **Admin auth**: `/admin/stats` requires basic auth. If `admin_pass` is empty, admin endpoints return 403.
- **Container security**: Distroless image, non-root user (UID 65534), no shell.
- **Key injection**: Use `TAVILY_KEYS` environment variable in Docker/Kubernetes to avoid committing secrets.
- **Network**: Default `listen_addr` is `0.0.0.0:8082`. Use `127.0.0.1:8082` for localhost-only.
- **No TLS**: The router does not terminate TLS. Use a reverse proxy (nginx, traefik) for HTTPS.

## License

MIT