# Tavily Smart Router — Troubleshooting Guide

Real-world issues encountered during production deployment, their root causes, and fixes.

---

## Table of Contents

1. [Disabled Key Still Used (MarkCooldown Resurrection)](#1-disabled-key-still-used-markcooldown-resurrection)
2. [Uneven Key Distribution (First Key Heavily Used)](#2-uneven-key-distribution-first-key-heavily-used)
3. [Disabled Key Reset on Restart (No State Persistence)](#3-disabled-key-reset-on-restart-no-state-persistence)
4. [State File Permission Denied in Docker](#4-state-file-permission-denied-in-docker)
5. [Log Spam from Persistent Save Failures](#5-log-spam-from-persistent-save-failures)
6. [432 Response with Low Credit Usage](#6-432-response-with-low-credit-usage)
7. [Useful Diagnostic Commands](#useful-diagnostic-commands)

---

## 1. Disabled Key Still Used (MarkCooldown Resurrection)

**Commit**: `78f2c66`

### Symptom

A key disabled via `POST /admin/keys/{index}/disable` continued to receive traffic. The Tavily dashboard showed the disabled key's quota being consumed, while other keys appeared unused.

### Root Cause

`MarkCooldown` was the only state-transition function that lacked a `KeyDisabled` guard. When a key was disabled while requests were in flight, and those in-flight requests returned a retryable error (429/432/433/timeout), `MarkCooldown` overwrote `KeyDisabled` with `KeyCooldown`. The `PickKey` cooldown-recovery logic then resurrected the key as `KeyHealthy`, silently undoing the manual disable.

| Function | Had guard before fix? |
|---|---|
| `MarkSuccess` | Yes |
| `MarkFail` | Yes |
| `MarkCooldown` | **No — fixed in `78f2c66`** |

### Fix

Added the missing guard to `MarkCooldown`:

```go
if key.State == KeyDisabled {
    return
}
```

This covers all 7 call sites that can trigger `MarkCooldown` (4 in `classifyResponse`, 1 in `proxyErrorHandler`, 2 in `healthHandler`).

### Verification

`TestMarkCooldownDoesNotAffectDisabled` regression test verifies that `MarkCooldown` on a disabled key leaves `State`, `CooldownUntil`, and `FailCount` untouched.

---

## 2. Uneven Key Distribution (First Key Heavily Used)

**Commit**: `2e55eff` (health-check bias fix, was in a prior release but binary was stale)

### Symptom

The first key in the config received disproportionately heavy traffic, while other keys appeared unused on the Tavily dashboard.

### Root Cause

Three bugs compounded:

1. **Health-check key bias**: `healthHandler` used a manual loop that always picked the first healthy key in slice order (always key 0). Health checks send real API calls to Tavily, draining key 0's quota without incrementing `UsageCount` (invisible to `least_used`).

2. **Missing MarkCooldown on 432 in health handler**: The old health handler returned "unhealthy" for 432 responses but did not call `MarkCooldown`, so key 0 stayed "healthy" and kept getting all health checks.

3. **MarkCooldown resurrection** (Bug #1 above): Key 0 kept coming back from manual disable.

### Fix

- `healthHandler` now uses `PickKey()` for even rotation across all keys
- `healthHandler` now calls `MarkCooldown`/`MarkDisabled`/`MarkSuccess` based on response status
- `MarkCooldown` guard prevents resurrection of disabled keys

### Verification

`TestFairness_FirstKeyDoesNotDominate` confirms 4 keys each get ~25% of 1000 concurrent requests. `TestFairness_HealthChecksRotate` confirms health checks rotate evenly.

---

## 3. Disabled Key Reset on Restart (No State Persistence)

**Commit**: `99c2543`

### Symptom

After a Docker container restart or Pi4 reboot, all keys reset to `healthy` with zeroed counters. Manual disable actions were lost, and disabled keys re-entered rotation.

### Root Cause

Key state was held entirely in memory. `NewKeyRotator` always created keys with `State: KeyHealthy`. There was no save/load mechanism — no `os.WriteFile`, no persistence layer, nothing.

### Fix

Added JSON file-based persistence:

- **`StateFile` config option** (default: `state.json`, env: `TAVILY_STATE_FILE`)
- **`SaveState()`**: Serializes key states to JSON with atomic write (temp file + rename)
- **`LoadState()`**: Restores states on startup, matches by index with masked-key verification
- **Debounced auto-save**: 500ms coalescing on every state mutation + 5s periodic safety net
- **Final save on graceful shutdown** via `StopPersistence()`
- **Security**: State file contains masked keys only (`tvly-Z...AM`), never raw API keys

### Verification

7 tests: round-trip, missing file, corrupt file, restart restores disabled, atomic write, no-raw-key leak, disabled-when-empty.

---

## 4. State File Permission Denied in Docker

**Commit**: `16740b1`

### Symptom

```
state_save_failed error="creating temp file: open /state/.state-*.json.tmp: permission denied"
```

This error repeated every 5 seconds, flooding the logs.

### Root Cause

The Docker image uses `gcr.io/distroless/static-debian12` with `USER nonroot` (UID 65534). Docker named volumes are created owned by `root`. The `nonroot` user cannot write to a root-owned volume.

### Fix

- Switched `docker-compose.yml` from a named volume to a **bind mount** (`./state:/state`)
- The user creates the directory with correct ownership: `mkdir -p state && sudo chown 65534:65534 state`
- Added `user: "65534:65534"` to `docker-compose.yml` for explicitness

---

## 5. Log Spam from Persistent Save Failures

**Commit**: `16740b1`

### Symptom

The 5-second periodic save loop logged the same `state_save_failed` error every cycle, drowning out useful logs and making it impossible to see the actual cooldown events.

### Root Cause

The save loop called `slog.Error` on every save failure with no deduplication or backoff.

### Fix

- **Backoff**: Log once on first failure (`saveFailed = true`), suppress subsequent identical errors
- **Recovery log**: Log `state_save_recovered` when save succeeds after a failure
- **First-success log**: Log `state_save_ok` once on first successful save so users can verify persistence is working

---

## 6. 432 Response with Low Credit Usage

**Commit**: `d348cc5` (added body logging for diagnosis)

### Symptom

Tavily returns HTTP 432 with body:

```json
{"detail":{"error":"This request exceeds your plan's set usage limit. Please upgrade your plan or contact support@tavily.com"}}
```

But the Tavily dashboard shows only 45/1000 credits used. The router puts all keys into 24-hour cooldown, making the entire router unavailable.

### Investigation

According to [Tavily's error code documentation](https://help.tavily.com/articles/8645538886-understanding-http-errors):

| Status | Meaning |
|---|---|
| 432 | Plan limit exceeded (monthly credits) |
| 429 | Rate limit exceeded (too many requests in short period) |
| 433 | Pay-as-you-go limit exceeded |

The 432 error message says "exceeds your plan's set usage limit," which Tavily documents as the monthly credit limit. However, the user's dashboard shows only 45/1000 credits used, contradicting this.

### Confirmed Cause: IP-Based Rate Limiting

**Confirmed in production**: All 3 keys are on **separate Tavily accounts** with low credit usage (45/1000, not exhausted). Yet all 3 keys return 432 simultaneously within the same second.

The log timeline confirms this:

```
16:11:21 — key JRG, status 200 ✓ (1st success)
16:13:01 — key uBO, status 200 ✓ (2nd success, different key, different account)
16:13:07 — key OzE, status 432 ✗ (3rd request — ALL keys return 432)
16:13:07 — key JRG, status 432 ✗ (retry with different key, same account as 1st success)
16:13:07 — key uBO, status 432 ✗ (retry with different key, same account as 2nd success)
```

3 successful requests in ~2 minutes from the same Pi4 IP, then ALL keys on ALL accounts return 432 simultaneously. This is **IP-based rate limiting** — Tavily throttles by the client's public IP address, not per-key or per-account.

**Tavily is returning 432 (documented as "plan limit") for what is actually a rate limit.** This is a Tavily API behavior issue, not a router bug. The router correctly follows the documented semantics (432 → 24h cooldown), but the 24h cooldown is far too long for what is actually a short-term rate limit.

### Workaround

**Manually re-enable keys** when you know the 432 is a rate limit, not quota exhaustion:

```bash
curl -u admin:password -X POST http://localhost:8082/admin/keys/1/enable
curl -u admin:password -X POST http://localhost:8082/admin/keys/2/enable
curl -u admin:password -X POST http://localhost:8082/admin/keys/3/enable
```

**Reduce request frequency** — the free Dev plan allows 100 RPM per key, but IP-based throttling may trigger earlier when multiple keys fire from the same IP in rapid succession (as happens during transparent retry).

**Reduce `quota_cooldown_sec`** — if you frequently hit this issue, set a shorter cooldown in `config.json`:

```json
{
  "quota_cooldown_sec": 300
}
```

This treats 432 as a 5-minute cooldown instead of 24 hours, which is more appropriate for rate limiting.

### Diagnostic Steps

1. **Check the 432 response body** (now logged by the router):
   ```bash
   docker compose logs tavily-router | grep tavily_432
   ```

2. **Test the key directly** (bypassing the router):
   ```bash
   curl -s -X POST https://api.tavily.com/search \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer tvly-dev-YOUR-KEY" \
     -d '{"query": "test", "max_results": 1}'
   ```

3. **Check Tavily dashboard**: Look at account-level credit usage, not per-key. If all accounts show low usage but 432 persists, it's IP-based rate limiting.

4. **Contact Tavily support**: If credits are low but 432 persists and the issue is not IP-based, contact support@tavily.com — there may be an account-level issue.

### Router Behavior

The router treats all 432 responses as quota exhaustion and applies `quota_cooldown_sec` (default: 24h) cooldown. This is correct per Tavily's documentation. If 432 is being returned for a non-quota reason, the keys will be unnecessarily cooled down for 24 hours. To manually recover:

```bash
curl -u admin:password -X POST http://localhost:8082/admin/keys/1/enable
curl -u admin:password -X POST http://localhost:8082/admin/keys/2/enable
curl -u admin:password -X POST http://localhost:8082/admin/keys/3/enable
```

### Improvement: Body Logging

**Commit**: `d348cc5`

The 432 and 433 handlers previously did not read the response body, making diagnosis impossible. Now they read and log the body:

```
tavily_432_response key=tvly-...uBO body="This request exceeds your plan's set usage limit..."
```

---

## Useful Diagnostic Commands

### Check key states

```bash
curl -u admin:password http://localhost:8082/admin/stats | jq .
```

### Check health

```bash
curl http://localhost:8082/health | jq .
```

### View recent logs

```bash
docker compose logs --tail=30 tavily-router
```

### Follow logs in real-time

```bash
docker compose logs -f tavily-router
```

### Filter for 432 errors

```bash
docker compose logs tavily-router | grep "tavily_432"
```

### Filter for cooldown events

```bash
docker compose logs tavily-router | grep "key_cooldown"
```

### Manually enable a cooldown key

```bash
curl -u admin:password -X POST http://localhost:8082/admin/keys/{index}/enable
```

### Manually disable a key

```bash
curl -u admin:password -X POST http://localhost:8082/admin/keys/{index}/disable
```

### Test a key directly (bypass router)

```bash
curl -s -X POST https://api.tavily.com/search \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer tvly-dev-YOUR-KEY" \
  -d '{"query": "test", "max_results": 1}' | jq .
```

### Check if persistence is working

```bash
docker compose logs tavily-router | grep "state_save_ok"
# Should show: state_save_ok file=/state/state.json
```

### Check state file contents

```bash
cat state/state.json | jq .
```

### Verify binary version

```bash
docker compose logs tavily-router | grep "version="
# Should show a recent commit hash, not "dev" from an old build
```
