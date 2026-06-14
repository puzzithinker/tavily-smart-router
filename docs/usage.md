# Tavily Smart Router — Detailed Usage Guide

This guide covers everything you need to know to deploy, configure, and operate tavily-smart-router in production.

---

## Table of Contents

1. [Installation](#installation)
2. [Configuration Deep Dive](#configuration-deep-dive)
3. [Running the Router](#running-the-router)
4. [Connecting Your Agent](#connecting-your-agent)
5. [Key Rotation Strategies](#key-rotation-strategies)
6. [Error Handling in Detail](#error-handling-in-detail)
7. [Health Check Endpoints](#health-check-endpoints)
8. [Admin Dashboard](#admin-dashboard)
9. [Prometheus Monitoring](#prometheus-monitoring)
10. [Deployment Scenarios](#deployment-scenarios)
11. [Troubleshooting](#troubleshooting)
12. [Migration from Direct Tavily API](#migration-from-direct-tavily-api)

---

## Installation

### Option 1: Pre-built Binary

```bash
# Build from source (requires Go 1.22+)
git clone https://github.com/puzzithinker/tavily-smart-router.git
cd tavily-smart-router
make build
./bin/tavily-router
```

### Option 2: Docker

```bash
docker build -t tavily-router .
docker run -d -p 8082:8082 \
  -e TAVILY_KEYS="tvly-key1,tvly-key2,tvly-key3" \
  tavily-router
```

### Option 3: Docker Compose

```bash
cp config.example.json config.json
# Edit config.json with your keys
export TAVILY_KEYS="tvly-key1,tvly-key2,tvly-key3"
docker compose up -d
```

### Option 4: Cross-compile for ARM64 (Raspberry Pi)

```bash
make build-arm64
# Copy to Pi
scp bin/tavily-router-linux-arm64 pi@raspberry:~/tavily-router
```

---

## Configuration Deep Dive

### Priority Order

Configuration values are loaded in this priority order (highest wins):

1. **Environment variables** (`TAVILY_KEYS`, `TAVILY_UPSTREAM_BASE`, etc.)
2. **Config file** (`config.json` or path from `TAVILY_CONFIG`)
3. **Built-in defaults**

### Minimal Config

The absolute minimum to get running:

```json
{
  "keys": ["tvly-your-api-key-here"]
}
```

All other values use sensible defaults:
- Listens on `0.0.0.0:8082`
- Upstream: `https://api.tavily.com`
- Strategy: `least_used`
- Cooldown: 300 seconds
- Max fails before cooldown: 3

### Production Config with Monitoring

```json
{
  "listen_addr": "0.0.0.0:8082",
  "upstream_base": "https://api.tavily.com",
  "keys": ["tvly-key1", "tvly-key2", "tvly-key3"],
  "strategy": "least_used",
  "cooldown_sec": 300,
  "max_fails_before_cooldown": 3,
  "health_check_timeout_seconds": 10,
  "admin_user": "admin",
  "admin_pass": "change-me-in-production",
  "enable_prometheus": true,
  "enable_request_log": true,
  "log_file": "/var/log/tavily-router.log"
}
```

### Docker Secrets (Recommended)

Never put API keys in config files. Use environment variables:

```bash
docker run -d \
  -p 8082:8082 \
  -e TAVILY_KEYS="tvly-key1,tvly-key2,tvly-key3" \
  -e TAVILY_STRATEGY="round_robin" \
  -e TAVILY_COOLDOWN_SEC="120" \
  tavily-router
```

### Key Count

You can use 1 to N keys. The router handles any number:

- **1 key**: Simple proxy with health checking and error reporting
- **2-3 keys**: Basic failover for rate limit resilience
- **5+ keys**: High-throughput with load distribution

> **Note**: With only 1 key, there's no failover on rate limits. The error will be forwarded to the caller.

---

## Running the Router

### Foreground (Development)

```bash
./bin/tavily-router
```

Output:
```
time=2026-06-14T08:00:00.000Z level=INFO msg=startup keys=3 strategy=least_used listen=0.0.0.0:8082 upstream=https://api.tavily.com
time=2026-06-14T08:00:00.000Z level=INFO msg=startup version=v1.0.0
time=2026-06-14T08:00:00.000Z level=INFO msg=listening addr=0.0.0.0:8082
```

### Custom Config Path

```bash
TAVILY_CONFIG=/etc/tavily-router/config.json ./bin/tavily-router
```

### With Environment Variables

```bash
export TAVILY_KEYS="tvly-key1,tvly-key2,tvly-key3"
export TAVILY_LISTEN_ADDR="127.0.0.1:8082"
export TAVILY_STRATEGY="round_robin"
./bin/tavily-router
```

### Docker Compose with Restart

```bash
docker compose up -d
docker compose logs -f tavily-router  # Follow logs
docker compose restart tavily-router    # Restart
docker compose down                     # Stop
```

### Graceful Shutdown

Send `SIGINT` (Ctrl+C) or `SIGTERM`:

```bash
kill -SIGTERM <pid>
```

The router:
1. Stops accepting new connections
2. Waits up to 10 seconds for in-flight requests to complete
3. Closes log file if enabled
4. Exits cleanly

---

## Connecting Your Agent

### Python (Tavily SDK)

Replace the Tavily API base URL:

```python
from tavily import TavilyClient

# Direct Tavily API (before)
# client = TavilyClient(api_key="tvly-your-key")

# Through router (after)
import httpx

# Option 1: Use the router as a proxy by setting the base URL
# This requires modifying how your code makes requests,
# since the Tavily SDK doesn't expose a base_url parameter.
# See Option 2 for the recommended approach.

# Option 2: Use the router as a direct HTTP proxy
import requests

response = requests.post(
    "http://localhost:8082/search",
    json={
        "query": "What is quantum computing?",
        "search_depth": "advanced",
        "max_results": 5,
    }
    # No Authorization header needed — the router injects the key
)
results = response.json()
```

### LangChain Integration

```python
from langchain_community.tools.tavily import TavilySearchResults

# Configure LangChain to use the router
import os
os.environ["TAVILY_API_URL"] = "http://localhost:8082"

# But note: if the Tavily tool sends the key in the body,
# the router will override it with the rotated key via the Authorization header.
```

### Direct HTTP (Any Language)

```bash
# Search
curl -X POST http://localhost:8082/search \
  -H "Content-Type: application/json" \
  -d '{"query": "latest AI news", "max_results": 5, "search_depth": "advanced"}'

# Extract
curl -X POST http://localhost:8082/extract \
  -H "Content-Type: application/json" \
  -d '{"urls": ["https://example.com/article"], "extract_depth": "advanced"}'

# Crawl
curl -X POST http://localhost:8082/crawl \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com", "max_depth": 2}'
```

> **Important**: Do NOT include an `Authorization` header in your requests. The router injects the rotated API key automatically. If you do include one, it will be overwritten.

---

## Key Rotation Strategies

### least_used (Default)

Selects the key with the lowest `usage_count` that is not in cooldown or disabled. This naturally balances load toward underutilized keys, which is useful when keys have different rate limits.

Best for: Keys with varying rate limits or when you want even distribution.

### round_robin

Cycles through keys in order using an atomic counter. Skips disabled and cooldown keys.

Best for: Keys with identical rate limits where you want predictable ordering.

### Switching Strategies

```bash
# Via config file
{
  "strategy": "round_robin"
}

# Via environment variable
export TAVILY_STRATEGY="round_robin"
```

---

## Error Handling in Detail

### Key State Machine

Each key has three possible states:

```
HEALTHY ──→ COOLDOWN ──→ (auto-recover after cooldown_sec)
   │              │
   │              └──→ HEALTHY (on successful request)
   │
   └──→ DISABLED (permanent, requires manual intervention)
```

### State Transition Rules

| Current State | Event | New State | Recovery |
|---|---|---|---|
| HEALTHY | 2xx response | HEALTHY | — |
| HEALTHY | 429 rate limit | COOLDOWN | Auto after cooldown |
| HEALTHY | 401/403 | DISABLED | Never |
| HEALTHY | 432/433 | DISABLED | Never |
| HEALTHY | 5xx (Nth consecutive) | COOLDOWN | Auto after cooldown |
| HEALTHY | 5xx (below threshold) | HEALTHY | — (fail count increments) |
| COOLDOWN | Cooldown expires | HEALTHY | Available on next pick |
| COOLDOWN | Successful request | HEALTHY | Early recovery |
| DISABLED | Any | DISABLED | Permanent |

### Consecutive Failure Threshold

The `max_fails_before_cooldown` setting (default: 3) prevents transient issues from triggering cooldown:

```
Request 1: 5xx → fail_count=1, key stays HEALTHY
Request 2: 5xx → fail_count=2, key stays HEALTHY
Request 3: 5xx → fail_count=3 ≥ threshold → key enters COOLDOWN
```

A successful request resets `fail_count` to 0.

### Transparent Retry Flow

When a key returns a retryable error:

```
1. Client sends request to router
2. Router picks key A
3. Key A returns 429 (rate limit)
4. Router marks key A as COOLDOWN
5. Router retries request with key B
6. Key B returns 200 (success)
7. Client receives successful response — never sees the 429
```

If all keys fail:
- Last 429 → HTTP 429 with error body
- Last 401/403 → HTTP 401/403 with error body

### Error Response Format

When the router itself returns an error (all keys exhausted):

```json
{
  "error": {
    "message": "all API keys exhausted after retries",
    "type": "server_error",
    "code": "all_keys_exhausted"
  }
}
```

---

## Health Check Endpoints

### GET /health

Performs a real Tavily search request to verify both key validity and upstream connectivity.

**Success response** (HTTP 200):

```json
{
  "status": "healthy",
  "upstream": "reachable",
  "healthy_keys": 3,
  "total_keys": 3,
  "disabled_keys": 0
}
```

**Unhealthy responses** (HTTP 503):

| `upstream` field | Meaning |
|---|---|
| `no_healthy_keys` | All keys are in cooldown or disabled |
| `unreachable` | Tavily API is not responding |
| `auth_failed` | Key returned 401/403/432/433 |
| `url_error` | Invalid upstream URL |

**Health check request details**:

The router sends:
```json
POST https://api.tavily.com/search
Authorization: Bearer <healthy_key>
Content-Type: application/json

{"query": "health_check", "max_results": 1, "search_depth": "basic"}
```

This minimizes cost while verifying both key and connectivity.

**For monitoring systems**:

```bash
# Simple check
curl -s http://localhost:8082/health | jq '.status'
# Outputs: "healthy"

# Detailed check
curl -s http://localhost:8082/health | jq .
```

---

## Admin Dashboard

### GET /admin/stats

Protected by HTTP Basic Auth. Returns real-time key state.

**Prerequisites**: Set `admin_pass` in config. If `admin_pass` is empty, admin endpoints return 403.

**Request**:

```bash
curl -u admin:change-me-in-production http://localhost:8082/admin/stats | jq .
```

**Response**:

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
    },
    {
      "masked_key": "tvly-c...def",
      "state": "disabled",
      "usage_count": 500,
      "fail_count": 3,
      "last_used": "2026-06-14T07:00:00Z"
    }
  ],
  "total_requests": 740,
  "strategy": "least_used"
}
```

### Key States in Stats

| State | Meaning |
|---|---|
| `healthy` | Available for use |
| `cooldown` | Temporarily paused (will auto-recover) |
| `disabled` | Permanently removed from rotation |

---

## Prometheus Monitoring

### Enabling Metrics

Set `enable_prometheus: true` in config:

```json
{
  "enable_prometheus": true
}
```

Then access metrics at:

```bash
curl http://localhost:8082/metrics
```

### Metrics Reference

| Metric | Type | Description |
|---|---|---|
| `tavily_router_requests_total` | Counter | Total requests by key and status group |
| `tavily_router_key_usage_total` | Counter | Times each key was selected |
| `tavily_router_key_healthy` | Gauge | 1=healthy, 0=cooldown/disabled |
| `tavily_router_request_duration_seconds` | Histogram | Request latency by key |
| `tavily_router_key_cooldown_total` | Counter | Times each key entered cooldown |
| `tavily_router_upstream_errors_total` | Counter | Upstream errors by key and type |

### Useful PromQL Queries

```promql
# Request rate by key
rate(tavily_router_requests_total[5m])

# Error rate by type
rate(tavily_router_upstream_errors_total[5m])

# P95 latency
histogram_quantile(0.95, rate(tavily_router_request_duration_seconds_bucket[5m]))

# Keys currently healthy
tavily_router_key_healthy

# Keys in cooldown (total entries ever)
tavily_router_key_cooldown_total

# Rate limit errors per key
tavily_router_upstream_errors_total{error_type="rate_limit"}

# Auth errors (keys being disabled)
tavily_router_upstream_errors_total{error_type="auth_error"}
```

### Grafana Dashboard

The docker-compose.yml includes Grafana. Access at `http://localhost:3000` (admin/admin).

Add Prometheus as a data source: `http://prometheus:9090`

---

## Deployment Scenarios

### Scenario 1: Single Instance (RPi 4)

```json
{
  "listen_addr": "0.0.0.0:8082",
  "upstream_base": "https://api.tavily.com",
  "keys": ["tvly-key1", "tvly-key2", "tvly-key3"],
  "strategy": "least_used",
  "cooldown_sec": 300,
  "max_fails_before_cooldown": 3,
  "admin_user": "admin",
  "admin_pass": "",
  "enable_prometheus": false,
  "enable_request_log": false,
  "log_file": ""
}
```

Run with `GOGC=50` for reduced GC pressure:

```bash
GOGC=50 ./tavily-router
```

### Scenario 2: Docker with Monitoring

```json
{
  "listen_addr": "0.0.0.0:8082",
  "upstream_base": "https://api.tavily.com",
  "keys": ["tvly-key1", "tvly-key2", "tvly-key3", "tvly-key4", "tvly-key5"],
  "strategy": "least_used",
  "cooldown_sec": 120,
  "max_fails_before_cooldown": 3,
  "admin_user": "admin",
  "admin_pass": "secure-password",
  "enable_prometheus": true,
  "enable_request_log": true,
  "log_file": ""
}
```

### Scenario 3: Behind Nginx with HTTPS

```nginx
server {
    listen 443 ssl;
    server_name tavily.example.com;

    ssl_certificate /etc/nginx/ssl/tavily.crt;
    ssl_certificate_key /etc/nginx/ssl/tavily.key;

    location / {
        proxy_pass http://127.0.0.1:8082;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### Scenario 4: Load-Balanced with Multiple Instances

```nginx
upstream tavily_router {
    server 10.0.0.1:8082;
    server 10.0.0.2:8082;
    server 10.0.0.3:8082;
}

server {
    listen 8082;
    location / {
        proxy_pass http://tavily_router;
    }
}
```

> **Note**: When load-balancing multiple router instances, each instance maintains its own key state. Keys disabled on one instance may still be healthy on another. Use identical configs and accept that eventually all instances will converge on disabled states.

---

## Troubleshooting

### All Keys Show as Disabled

This means every key received a 401/403/432/433 response:

```bash
# Check stats
curl -u admin:password http://localhost:8082/admin/stats
```

Common causes:
- Invalid API keys
- Expired keys
- Plan limits exceeded

**Fix**: Replace keys in config and restart, or update `TAVILY_KEYS` environment variable.

### Keys Keep Entering Cooldown

You may be hitting Tavily's rate limits. Solutions:
- Add more keys
- Increase `cooldown_sec` (default: 300)
- Switch to `least_used` strategy for better distribution
- Check Tavily rate limits for your plan (Dev: 100 RPM, Prod: 1000 RPM)

### Health Check Returns 503

```bash
curl http://localhost:8082/health | jq .

# Check which component failed:
# "upstream": "no_healthy_keys" → all keys cooldown/disabled
# "upstream": "unreachable" → Tavily API down
# "upstream": "auth_failed" → key is invalid
```

### Request Body Too Large

The router buffers the entire request body for retries. Extremely large requests may cause memory issues. This is intentional — transparent retry requires body replay.

### High Memory Usage on RPi 4

Set `GOGC=50` in your environment to reduce GC pressure:

```bash
export GOGC=50
./tavily-router
```

Also disable Prometheus and file logging if not needed.

---

## Migration from Direct Tavily API

### Step 1: Deploy the Router

```bash
# Copy config
cp config.example.json config.json
# Add your keys
# Start the router
./bin/tavily-router
```

### Step 2: Update Your Application

Replace `https://api.tavily.com` with `http://localhost:8082`:

```python
# Before
response = requests.post(
    "https://api.tavily.com/search",
    headers={"Authorization": f"Bearer {API_KEY}"},
    json={"query": "AI news"}
)

# After — no Authorization header needed, router handles it
response = requests.post(
    "http://localhost:8082/search",
    json={"query": "AI news"}
)
```

### Step 3: Add More Keys for Scale

Get additional Tavily API keys and add them to your config:

```json
{
  "keys": ["tvly-key1", "tvly-key2", "tvly-key3"]
}
```

Restart the router to pick up the new keys.

### Step 4: Monitor

Enable Prometheus metrics and set up alerts for:

- `tavily_router_key_healthy == 0` — Key is unhealthy
- `tavily_router_upstream_errors_total{error_type="auth_error"}` — Keys being disabled
- `tavily_router_key_cooldown_total` — Rate limiting frequency