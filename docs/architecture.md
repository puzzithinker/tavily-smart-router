# Architecture: tavily-smart-router

## Project Overview

tavily-smart-router is a lightweight, deterministic HTTP proxy written in Go. It sits between AI agents (such as Hermes Agent) and the Tavily API, rotating multiple API keys automatically so the agent only needs to talk to a single local endpoint.

When a key fails, hits a rate limit, or returns an authentication error, the router transparently retries with the next available key. The caller never sees the retry, only the final success or a clean error response.

The project inherits the philosophy and architecture of `opencode-smart-router`: single binary, no complex abstractions, minimal resource usage, and deterministic behavior. It is designed to run on resource-constrained hardware like a Raspberry Pi 4.

---

## Architecture Overview

### Single-Binary Design

All logic lives in `main.go`. The file is organized into clearly marked sections:

1. **Config** — JSON parsing, defaults, environment overrides, validation
2. **Key State Machine** — `KeyEntry`, `KeyRotator`, state transitions, `MarkFail` for consecutive failure tracking
3. **Error Classification** — Error type constants, Tavily-specific error body parsing
4. **Proxy** — `httputil.ReverseProxy`, buffered response writer, transparent retry loop
5. **Middleware** — Basic auth for admin endpoints
6. **Handlers** — `/health` (POST to `/search`), `/admin/stats`
7. **Metrics** — Prometheus counters, gauges, and histograms (6 metrics)
8. **Logging** — Structured logging with `log/slog`
9. **Router Error Format** — Standard error response shape
10. **Main** — Wiring, signal handling, graceful shutdown

### Key Differences from opencode-smart-router

| Aspect | opencode-smart-router | tavily-smart-router |
|--------|----------------------|---------------------|
| Upstream | `https://opencode.ai/zen/go` | `https://api.tavily.com` |
| Default port | `8080` | `8082` |
| Default strategy | `round_robin` | `least_used` |
| Default cooldown | `60s` | `300s` |
| Env prefix | `OPENCODE_` | `TAVILY_` |
| Health check | `GET /v1/models` | `POST /search` (minimal body) |
| Error codes | Standard HTTP | Standard + 432 (plan limit), 433 (PayGo limit) |
| Consecutive fail tracking | N/A | `max_fails_before_cooldown` (default 3) |
| Metric prefix | `opencode_router_` | `tavily_router_` |
| Extra metrics | 4 | 6 (+key_cooldown_total, upstream_errors_total) |
| Health auth check | Standard | Detects 401/403 as `auth_failed`, 432/433 as `quota_exhausted` |

---

## Request Flow

```
Client (Hermes Agent / AI Agent)
    |
    v
/search, /extract, /crawl, /map, /*  -->  proxyHandler
                                            |
                                            v
                                    Pick key from KeyRotator
                                            |
                                            v
                                    httputil.ReverseProxy
                                            |
                                            v
                                    classifyResponse (ModifyResponse)
                                            |
                                            +-- ShouldRetry? --> Pick next key, loop
                                            |
                                            +-- Success/Non-retryable? --> Write buffered response to client
```

---

## Key Rotation

### KeyState Machine

Each API key is wrapped in a `KeyEntry` with three possible states:

| State | Meaning | Transition Trigger |
|-------|---------|-------------------|
| `HEALTHY` | Key is available for use | Default state; entered after cooldown expires or on success |
| `COOLDOWN` | Key is temporarily paused | Entered on 429 rate limit, N consecutive 5xx failures, timeout, or quota exhaustion (432/433/429-quota) |
| `DISABLED` | Key is permanently removed from rotation | Entered on 401/403 only |

Transitions:

- `HEALTHY --> COOLDOWN`: Rate limit (429), N consecutive 5xx (via `MarkFail`), timeout, or quota exhaustion (432/433/429-quota via `MarkCooldown` with `quota_cooldown_sec`)
- `COOLDOWN --> HEALTHY`: Cooldown period expires (checked at pick time)
- `HEALTHY --> DISABLED`: Authentication failure (401/403) only
- `COOLDOWN --> DISABLED`: Never happens directly; only via HEALTHY
- `DISABLED` is permanent — `MarkSuccess()` is a no-op for disabled keys

### Consecutive Failure Tracking (max_fails_before_cooldown)

The `MarkFail()` method tracks consecutive failures without triggering immediate cooldown. A key enters cooldown only after `max_fails_before_cooldown` (default: 3) consecutive failures. This provides resilience against transient 5xx errors.

- `FailCount` is reset on successful `PickKey()` or when entering cooldown/disabled
- `FailCount` is reset on `MarkSuccess()`
- When `FailCount >= MaxFailsBeforeCooldown`, the key transitions to COOLDOWN

### Selection Strategies

Two strategies are implemented in `PickKey()`:

**round_robin**

An atomic counter increments on every call. The rotator starts at the counter position and scans forward, skipping `DISABLED` keys and keys still in `COOLDOWN`. The first available key is selected.

**least_used** (default)

The rotator scans all keys and picks the one with the lowest `UsageCount` that is not `DISABLED` or in active `COOLDOWN`.

---

## Error Classification

The `classifyResponse` function maps upstream status codes to actions:

| Status Code | Action | Retry? | Key State Change |
|-------------|--------|--------|-----------------|
| 2xx | Forward to client | No | Mark `HEALTHY` |
| 401 / 403 | Auth failure | Yes (next key) | Mark `DISABLED` |
| 429 (rate limit) | Rate limited | Yes (next key) | Mark `COOLDOWN` with `Retry-After` |
| 429 (quota/plan) | Quota exhausted | Yes (next key) | Mark `COOLDOWN` with `quota_cooldown_sec` |
| 432 | Plan limit exceeded | Yes (next key) | Mark `COOLDOWN` with `quota_cooldown_sec` |
| 433 | PayGo limit exceeded | Yes (next key) | Mark `COOLDOWN` with `quota_cooldown_sec` |
| 5xx | Upstream error | No | Track fail (MarkFail) |
| Timeout | Network issue | No | Mark `COOLDOWN` for 10s |
| Other 4xx | Client error | No | Track fail (MarkFail) |

### 429 Body Parsing

For 429 responses, the router parses the response body to distinguish:
- Regular rate limit → `COOLDOWN`
- Quota/plan exhaustion (`"quota"`, `"plan limit"`, `"usage limit"` in body) → `COOLDOWN` with `quota_cooldown_sec`

### Tavily-Specific Error Codes

- **432**: Key or plan limit exceeded — the API key has reached its plan's usage limit. Mapped to `COOLDOWN` with `quota_cooldown_sec` (default 24h). Quotas reset monthly, so keys auto-recover.
- **433**: PayGo limit exceeded — the pay-as-you-go budget is exhausted. Mapped to `COOLDOWN` with `quota_cooldown_sec` (default 24h). Quotas reset monthly, so keys auto-recover.

### Error Type Constants

```go
const (
    ErrorTypeRateLimit   = "rate_limit"
    ErrorTypeAuth        = "auth_error"
    ErrorTypeServerError = "server_error"
    ErrorTypeTimeout     = "timeout"
    ErrorTypeUnknown     = "unknown"
)
```

---

## Reverse Proxy

The router uses Go's standard `httputil.ReverseProxy` with the modern `Rewrite` API.

### Rewrite Function

For every outgoing request, the rewrite handler:
1. Sets the upstream URL to `https://api.tavily.com`
2. Sets `X-Forwarded-*` headers
3. Strips hop-by-hop headers
4. Injects the selected API key as `Authorization: Bearer <raw_key>`

Tavily authenticates via the `Authorization: Bearer tvly-...` header. The proxy overrides any existing Authorization header from the client with the rotated key.

### Transparent Retry Mechanism

Identical to opencode-smart-router:
1. Buffer the entire request body for replay
2. Try each key in sequence (up to total key count)
3. If `ShouldRetry` is true, discard buffer and try next key
4. If `ShouldRetry` is false, forward buffered response to client

---

## Health Check

The `/health` endpoint performs a real `POST /search` request to Tavily with a minimal body:

```json
{"query": "health_check", "max_results": 1, "search_depth": "basic"}
```

This validates both key validity and upstream connectivity. Response codes:
- 200 → `healthy` + `reachable`
- 401/403 → `unhealthy` + `auth_failed`
- 432/433 → `unhealthy` + `quota_exhausted`
- Other errors → `unhealthy` + status code

---

## Configuration

### Config File

Default path is `config.json`. Override with `TAVILY_CONFIG` environment variable.

### Config Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `0.0.0.0:8082` | HTTP bind address |
| `upstream_base` | string | `https://api.tavily.com` | Tavily API base URL |
| `keys` | []string | (required) | Tavily API keys to rotate |
| `strategy` | string | `least_used` | `round_robin` or `least_used` |
| `cooldown_sec` | int | `300` | Default cooldown duration (min 30) |
| `max_fails_before_cooldown` | int | `3` | Consecutive failures before cooldown (min 1) |
| `quota_cooldown_sec` | int | `86400` | Cooldown for quota-exhausted keys (432/433/429-quota). Min 60. Quotas reset monthly. |
| `health_check_timeout_seconds` | int | `10` | Timeout for upstream health probe |
| `admin_user` | string | `admin` | Basic auth username for admin endpoints |
| `admin_pass` | string | `""` | Basic auth password. Empty = admin disabled |
| `enable_prometheus` | bool | `false` | Enable `/metrics` endpoint |
| `enable_request_log` | bool | `false` | Enable file logging |
| `log_file` | string | `""` | Log file path (stdout if empty) |

### Environment Overrides

- `TAVILY_KEYS`: Comma-separated list of keys (overrides config file)
- `TAVILY_UPSTREAM_BASE`: Override upstream URL
- `TAVILY_LISTEN_ADDR`: Override listen address
- `TAVILY_STRATEGY`: Override rotation strategy
- `TAVILY_COOLDOWN_SEC`: Override cooldown seconds
- `TAVILY_QUOTA_COOLDOWN_SEC`: Override quota cooldown seconds
- `TAVILY_CONFIG`: Override config file path

---

## Prometheus Metrics

Six metrics are registered when `enable_prometheus` is true:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `tavily_router_requests_total` | Counter | `key`, `status_group` | Total requests proxied |
| `tavily_router_key_usage_total` | Counter | `key` | Times each key was selected |
| `tavily_router_key_healthy` | Gauge | `key` | 1 if healthy, 0 if cooldown or disabled |
| `tavily_router_request_duration_seconds` | Histogram | `key` | Request latency distribution |
| `tavily_router_key_cooldown_total` | Counter | `key` | Times each key entered cooldown |
| `tavily_router_upstream_errors_total` | Counter | `key`, `error_type` | Upstream errors by type |

---

## Deployment

### Docker

Multi-stage build: `golang:1.23-alpine` → `distroless/static-debian12`. Runs as `nonroot` (UID 65534). Exposes port 8082.

### Systemd

`deploy/systemd/tavily-router.service` registers Docker Compose as a systemd unit.

### Resource Limits

- 256 MB memory limit in Docker Compose
- Static binary, no CGO
- GOGC=50 recommended for RPi 4
- Optional features (Prometheus, file logging) disabled by default