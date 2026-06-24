# Tavily Smart Router — API Reference

Complete reference for every HTTP endpoint exposed by tavily-smart-router.

**Base URL**: `http://<listen_addr>` (default `0.0.0.0:8082`)

**Content-Type**: All request bodies and responses are `application/json`, except `/metrics` (Prometheus text format).

---

## Table of Contents

1. [Authentication](#authentication)
2. [Proxy Endpoints](#proxy-endpoints)
3. [Health Endpoint](#health-endpoint)
4. [Admin: View Key Stats](#admin-view-key-stats)
5. [Admin: Enable / Disable / Reset Keys](#admin-enable--disable--reset-keys)
6. [Metrics Endpoint](#metrics-endpoint)
7. [Error Response Format](#error-response-format)
8. [Key State Machine](#key-state-machine)
9. [HTTP Status Code Classification](#http-status-code-classification)

---

## Authentication

### Caller (proxy) endpoints

**No authentication required.** The router injects the rotated Tavily API key as `Authorization: Bearer <key>` on every upstream request. Do **not** send your own `Authorization` header — it will be overwritten.

### Admin endpoints

All `/admin/*` endpoints require HTTP Basic Auth.

| Header | Value |
|---|---|
| `Authorization` | `Basic <base64(admin_user:admin_pass)>` |

```bash
curl -u admin:your-password http://localhost:8082/admin/stats
```

**If `admin_pass` is empty** in the config, all admin endpoints return `403 Forbidden`:

```
admin endpoints disabled
```

**If credentials are wrong or missing**, the router returns `401 Unauthorized` with a `WWW-Authenticate` challenge:

```
WWW-Authenticate: Basic realm="tavily-router"
```

---

## Proxy Endpoints

All Tavily API paths are transparently proxied with automatic key rotation and retry.

### `POST /search`

Web search.

```bash
curl -X POST http://localhost:8082/search \
  -H "Content-Type: application/json" \
  -d '{
    "query": "latest AI news",
    "max_results": 5,
    "search_depth": "advanced"
  }'
```

### `POST /extract`

Content extraction.

```bash
curl -X POST http://localhost:8082/extract \
  -H "Content-Type: application/json" \
  -d '{
    "urls": ["https://example.com/article"],
    "extract_depth": "advanced"
  }'
```

### `POST /crawl`

Web crawling.

```bash
curl -X POST http://localhost:8082/crawl \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://example.com",
    "max_depth": 2
  }'
```

### `POST /map`

Site mapping.

```bash
curl -X POST http://localhost:8082/map \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com"}'
```

### `POST /*` (catch-all)

Any other path is proxied to the Tavily API. This covers endpoints Tavily may add in the future without requiring router updates.

### Proxy behavior

| Behavior | Description |
|---|---|
| Key injection | Router sets `Authorization: Bearer <rotated_key>` |
| Transparent retry | On 429 / 401 / 403 / 432 / 433, the router retries with the next available key. The caller never sees the retry. |
| Max retries | Equal to the total number of keys. Each retry uses a different key. |
| Body buffering | The request body is buffered in memory so it can be replayed across retries. Avoid extremely large payloads. |
| Hop-by-hop stripping | `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Transfer-Encoding`, `Upgrade` headers are stripped. |

### Successful response

The upstream Tavily response is forwarded as-is, including status code, headers, and body.

### All-keys-exhausted response

If every key is exhausted after retries, the router returns an error (see [Error Response Format](#error-response-format)):

| Last upstream status | Router response status | Error code |
|---|---|---|
| 429 / 432 / 433 | `429 Too Many Requests` | `all_keys_exhausted` |
| 401 / 403 | `401` or `403` | `auth_failed` |

---

## Health Endpoint

### `GET /health`

Verifies both key validity and upstream connectivity by sending a real `POST /search` request to Tavily with a minimal payload.

**Success** — HTTP `200`:

```json
{
  "status": "healthy",
  "upstream": "reachable",
  "healthy_keys": 3,
  "total_keys": 4,
  "disabled_keys": 1,
  "key": "tvly-a...xyz"
}
```

**Unhealthy** — HTTP `503`:

```json
{
  "status": "unhealthy",
  "upstream": "no_healthy_keys",
  "healthy_keys": 0,
  "total_keys": 4,
  "disabled_keys": 1
}
```

| `upstream` value | Meaning |
|---|---|
| `reachable` | Upstream responded 2xx |
| `no_healthy_keys` | All keys are in cooldown or disabled |
| `unreachable` | Tavily API did not respond (network error) |
| `auth_failed` | Key returned 401/403 — key is invalid |
| `quota_exhausted` | Key returned 432/433 — quota limit exceeded |
| `rate_limited` | Key returned 429 — rate limited |
| `marshal_error` | Failed to build health check body |
| `url_error` | Invalid upstream URL |
| `http_<code>` | Other upstream status code (e.g. `http_500`) |

The health check sends this request upstream:

```json
POST https://api.tavily.com/search
Authorization: Bearer <healthy_key>
Content-Type: application/json

{"query": "health_check", "max_results": 1, "search_depth": "basic"}
```

This costs one API call per health check. Use it at a reasonable interval (e.g. every 30–60 seconds).

---

## Admin: View Key Stats

### `GET /admin/stats`

Returns a real-time snapshot of all keys, their states, usage, and distribution fairness.

**Request**:

```bash
curl -u admin:your-password http://localhost:8082/admin/stats
```

**Response** (HTTP `200`):

```json
{
  "keys": [
    {
      "index": 0,
      "masked_key": "tvly-Z...AM",
      "state": "healthy",
      "usage_count": 142,
      "fail_count": 0,
      "last_used": "2026-06-24T08:30:00Z",
      "usage_pct": 38.04
    },
    {
      "index": 1,
      "masked_key": "tvly-4...BO",
      "state": "cooldown",
      "usage_count": 98,
      "fail_count": 0,
      "last_used": "2026-06-24T08:29:45Z",
      "usage_pct": 26.27
    },
    {
      "index": 2,
      "masked_key": "tvly-1...zE",
      "state": "disabled",
      "usage_count": 87,
      "fail_count": 3,
      "last_used": "2026-06-24T07:00:00Z",
      "usage_pct": 23.32
    },
    {
      "index": 3,
      "masked_key": "tvly-1...RG",
      "state": "healthy",
      "usage_count": 46,
      "fail_count": 0,
      "last_used": "2026-06-24T08:30:15Z",
      "usage_pct": 12.33
    }
  ],
  "total_requests": 373,
  "strategy": "least_used",
  "distribution": {
    "ideal_per_key": 93.25,
    "std_dev": 37.84,
    "coefficient_of_var": 0.4058,
    "fairness_ratio": 0.3239
  }
}
```

#### Key fields

| Field | Type | Description |
|---|---|---|
| `index` | int | Zero-based position in the key list (used by enable/disable/reset) |
| `masked_key` | string | Masked key: first 5 + last 3 chars (e.g. `tvly-Z...AM`) |
| `state` | string | `healthy`, `cooldown`, or `disabled` (see [Key State Machine](#key-state-machine)) |
| `usage_count` | int | Total times this key was selected by the rotator |
| `fail_count` | int | Consecutive failures since last success (resets on success or pick) |
| `last_used` | string\|null | RFC 3339 timestamp of last selection, or `null` if never used |
| `usage_pct` | float | This key's share of total traffic, in percent (only present if `total_requests > 0`) |

#### Distribution fields

Present only when `total_requests > 0`:

| Field | Type | Description |
|---|---|---|
| `ideal_per_key` | float | `total_requests / num_keys` — perfect even distribution |
| `std_dev` | float | Standard deviation of usage across keys |
| `coefficient_of_var` | float | `std_dev / ideal_per_key` — 0.0 = perfectly even, higher = more skewed |
| `fairness_ratio` | float | `min_usage / max_usage` — 1.0 = perfectly even, 0.0 = completely skewed |

**Interpreting fairness**: A `fairness_ratio` above 0.8 and `coefficient_of_var` below 0.2 indicate good distribution. If `fairness_ratio` drops below 0.5, one key is handling significantly more traffic than others — check for keys stuck in cooldown or disabled.

---

## Admin: Enable / Disable / Reset Keys

### `POST /admin/keys/{index}/{action}`

Manually control an individual key. Requires Basic Auth.

**Path parameters**:

| Parameter | Type | Description |
|---|---|---|
| `index` | int | Zero-based key index (from `GET /admin/stats`) |
| `action` | string | One of: `enable`, `disable`, `reset` |

### `POST /admin/keys/{index}/disable`

Removes a key from rotation permanently (until explicitly re-enabled). The key will not be selected by `PickKey` and cannot be resurrected by any automatic state transition.

```bash
curl -u admin:your-password -X POST http://localhost:8082/admin/keys/0/disable
```

**Response** (HTTP `200`):

```json
{
  "ok": true,
  "index": 0,
  "action": "disable",
  "key": "tvly-Z...AM",
  "state": "disabled",
  "usage_count": 142
}
```

Use this when:
- You want to take a key offline without restarting the router
- A key is compromised and you want to stop using it immediately
- You are rotating keys and want to phase out an old one

### `POST /admin/keys/{index}/enable`

Returns a disabled or cooldown key to healthy state, making it available for rotation again.

```bash
curl -u admin:your-password -X POST http://localhost:8082/admin/keys/0/enable
```

**Response** (HTTP `200`):

```json
{
  "ok": true,
  "index": 0,
  "action": "enable",
  "key": "tvly-Z...AM",
  "state": "healthy",
  "usage_count": 142
}
```

This is the **only** way to recover a disabled key. There is no automatic recovery from `disabled` state. The enable action also clears cooldown timers and resets the fail counter.

### `POST /admin/keys/{index}/reset`

Resets a key's usage and fail counters to zero without changing its state. Useful for recalibrating fairness after manual intervention.

```bash
curl -u admin:your-password -X POST http://localhost:8082/admin/keys/0/reset
```

**Response** (HTTP `200`):

```json
{
  "ok": true,
  "index": 0,
  "action": "reset",
  "key": "tvly-Z...AM",
  "state": "healthy",
  "usage_count": 0
}
```

After reset, the `least_used` strategy will prefer this key until its usage count catches up with the others. This can cause a temporary burst of traffic to the reset key.

### Error responses

| HTTP status | Body | Cause |
|---|---|---|
| `400 Bad Request` | `invalid path, expected /admin/keys/{index}/{action}` | Malformed URL |
| `400 Bad Request` | `invalid key index` | `index` is not an integer |
| `400 Bad Request` | `key index 5 out of range (0-3)` | Index exceeds the number of configured keys |
| `400 Bad Request` | `unknown action, expected enable\|disable\|reset` | Action is not one of the three valid values |
| `403 Forbidden` | `admin endpoints disabled` | `admin_pass` is empty in config |
| `405 Method Not Allowed` | `method not allowed` | Request was not `POST` |

### Common workflows

**Disable a key that is being rate-limited heavily**:

```bash
# 1. Check current states
curl -u admin:pass http://localhost:8082/admin/stats | jq '.keys[] | {index, state, usage_count}'

# 2. Disable the problematic key (e.g. index 0)
curl -u admin:pass -X POST http://localhost:8082/admin/keys/0/disable

# 3. Verify it's disabled
curl -u admin:pass http://localhost:8082/admin/stats | jq '.keys[0].state'
# "disabled"
```

**Re-enable a key after fixing it**:

```bash
curl -u admin:pass -X POST http://localhost:8082/admin/keys/0/enable
```

**Reset all counters for a clean fairness baseline**:

```bash
# Reset each key
for i in 0 1 2 3; do
  curl -u admin:pass -X POST "http://localhost:8082/admin/keys/$i/reset"
done
```

> **Note**: Key state is held in memory only. Restarting the router resets all keys to `healthy` with zeroed counters. To permanently remove a key, remove it from `config.json` (or `TAVILY_KEYS`) and restart.

---

## Metrics Endpoint

### `GET /metrics`

Prometheus-format metrics. Only available when `enable_prometheus: true` in config.

```bash
curl http://localhost:8082/metrics
```

### Metrics reference

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tavily_router_requests_total` | Counter | `key`, `status_group` | Total requests proxied, grouped by status class (`2xx`, `3xx`, `4xx`, `5xx`) |
| `tavily_router_key_usage_total` | Counter | `key` | Number of times each key was selected by the rotator |
| `tavily_router_key_healthy` | Gauge | `key` | `1` if healthy, `0` if cooldown or disabled |
| `tavily_router_request_duration_seconds` | Histogram | `key` | Upstream request latency in seconds |
| `tavily_router_key_cooldown_total` | Counter | `key` | Number of times each key entered cooldown |
| `tavily_router_upstream_errors_total` | Counter | `key`, `error_type` | Upstream errors by type |
| `tavily_router_key_usage_pct` | Gauge | `key` | Each key's share of total traffic (`0.0`–`1.0`). Updated every 10 seconds. |
| `tavily_router_key_distribution_fairness` | GaugeFunc | — | Fairness ratio: `min_usage / max_usage` across all keys. `1.0` = perfectly even. |

### Error types

| `error_type` | Meaning |
|---|---|
| `rate_limit` | Upstream returned 429 (rate limit) |
| `auth_error` | Upstream returned 401/403 (invalid key), 432/433 (quota exceeded), or 429 with quota message |
| `server_error` | Upstream returned 5xx |
| `timeout` | Request timed out |
| `unknown` | Other 4xx errors not classified above |

### Useful PromQL queries

```promql
# Request rate by key
rate(tavily_router_requests_total[5m])

# Error rate by type
rate(tavily_router_upstream_errors_total[5m])

# P95 latency
histogram_quantile(0.95, rate(tavily_router_request_duration_seconds_bucket[5m]))

# Keys currently healthy
tavily_router_key_healthy

# Fairness ratio (1.0 = perfectly even distribution)
tavily_router_key_distribution_fairness

# Traffic share per key (updated every 10s)
tavily_router_key_usage_pct

# Auth errors (keys being disabled by 401/403)
tavily_router_upstream_errors_total{error_type="auth_error"}
```

---

## Error Response Format

When the router itself returns an error (not a forwarded upstream error), it uses this shape:

```json
{
  "error": {
    "message": "all API keys exhausted after retries",
    "type": "server_error",
    "code": "all_keys_exhausted"
  }
}
```

| Field | Description |
|---|---|
| `message` | Human-readable description |
| `type` | Error category: `server_error` or `authentication_error` |
| `code` | Machine-readable code (see below) |

### Error codes

| Code | HTTP status | Type | Meaning |
|---|---|---|---|
| `all_keys_exhausted` | `429` | `server_error` | All keys are in cooldown after retries |
| `auth_failed` | `401` or `403` | `authentication_error` | All keys returned 401/403 |
| `upstream_error` | `502` | `server_error` | Reverse proxy error (e.g. connection failure) |
| `request_body_read_error` | `500` | `server_error` | Failed to read the incoming request body |

---

## Key State Machine

Each key is always in one of three states. After the in-flight-resurrection fix, **disabled is truly terminal** — no automatic transition can leave it.

```
                     ┌──────────────────────────────────┐
                     │            KeyHealthy             │
                     └──────────────────────────────────┘
                        │          │           │
              429/432/  │  401/403 │  5xx x N  │  2xx
              433/timeout│          │ (≥ max_)  │
                        ▼          ▼   fails   │
                  ┌──────────┐  ┌──────────┐   │
                  │KeyCooldown│  │KeyDisabled│  │
                  └──────────┘  └──────────┘   │
                      │              │          │
           cooldown   │              │ ADMIN    │
           expires or │              │ enable   │
           2xx early  │              │ only     │
           recovery   │              │          │
                      ▼              ▼          │
                  KeyHealthy    KeyHealthy      │
                                               │
                      (stays KeyHealthy) ◄─────┘
```

### State transition rules

| Current state | Event | New state | Recovery |
|---|---|---|---|
| `healthy` | 2xx response | `healthy` | — |
| `healthy` | 429 (rate limit) | `cooldown` | Auto after `cooldown_sec` or `Retry-After` |
| `healthy` | 429 (quota) | `cooldown` | Auto after `quota_cooldown_sec` (default 24h) |
| `healthy` | 432 (plan limit) | `cooldown` | Auto after `quota_cooldown_sec` |
| `healthy` | 433 (PayGo limit) | `cooldown` | Auto after `quota_cooldown_sec` |
| `healthy` | 401 / 403 | `disabled` | **Never (manual enable only)** |
| `healthy` | 5xx (N consecutive) | `cooldown` | Auto after `cooldown_sec` |
| `healthy` | 5xx (below threshold) | `healthy` | — (fail count increments) |
| `healthy` | Timeout | `cooldown` | Auto after 10 seconds |
| `healthy` | Admin disable | `disabled` | **Manual enable only** |
| `cooldown` | Cooldown expires + picked | `healthy` | Automatic on next `PickKey` |
| `cooldown` | 2xx response | `healthy` | Early recovery |
| `cooldown` | Admin disable | `disabled` | **Manual enable only** |
| `cooldown` | Admin enable | `healthy` | Immediate |
| `disabled` | **Any automatic event** | `disabled` | **No automatic recovery** |
| `disabled` | Admin enable | `healthy` | **The only exit from disabled** |
| `disabled` | Admin reset | `disabled` | State unchanged, counters zeroed |

### Why disabled is terminal

A disabled key is excluded from rotation by `PickKey` in both `round_robin` and `least_used` strategies. All three state-mutation functions guard against touching a disabled key:

| Function | Guard | Effect |
|---|---|---|
| `MarkSuccess` | `if State == KeyDisabled { return }` | 2xx on in-flight request does not re-enable |
| `MarkFail` | `if State == KeyDisabled { return }` | 5xx on in-flight request does not change state |
| `MarkCooldown` | `if State == KeyDisabled { return }` | 429/432/433/timeout on in-flight request does not resurrect |

This means: if you disable a key while requests are in flight, and those in-flight requests later return a retryable error, the key stays disabled. Only `POST /admin/keys/{index}/enable` can bring it back.

---

## HTTP Status Code Classification

How the router classifies upstream responses and what action it takes:

| Upstream status | Classification | Key action | Retry? | Router behavior |
|---|---|---|---|---|
| 2xx | Success | `MarkSuccess` | No | Forward response to client |
| 401 / 403 | Auth failure | `MarkDisabled` | Yes | Disable key, retry with next key |
| 429 + quota in body | Quota exhausted | `MarkCooldown` (quota) | Yes | Cooldown for `quota_cooldown_sec`, retry |
| 429 (other) | Rate limited | `MarkCooldown` (rate) | Yes | Cooldown for `Retry-After` or `cooldown_sec`, retry |
| 432 | Plan limit | `MarkCooldown` (quota) | Yes | Cooldown for `quota_cooldown_sec`, retry |
| 433 | PayGo limit | `MarkCooldown` (quota) | Yes | Cooldown for `quota_cooldown_sec`, retry |
| 5xx | Server error | `MarkFail` | No | Track fail count; forward response to client |
| Other 4xx | Client error | `MarkFail` | No | Track fail count; forward response to client |
| Timeout | Network error | `MarkCooldown` (10s) | No | Short cooldown; forward 502 to client |

**Retry budget**: Up to `N` attempts per request, where `N` is the total number of configured keys. Each retry selects a different key.
