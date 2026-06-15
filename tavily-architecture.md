Tavily Smart Router - Architecture & Design Specification
Markdown# Tavily Smart Router - Architecture & Design Specification

**Project Goal**  
A lightweight, deterministic, single-binary HTTP proxy written in Go for rotating multiple Tavily API keys.  
It is designed to serve AI agents (such as Hermes Agent) by providing a stable local endpoint while handling key rotation, automatic failover, and observability.

This router follows the same philosophy as the existing `opencode-smart-router`: prioritize simplicity, low resource usage, and predictability — especially for resource-constrained environments like Raspberry Pi 4.

---

## 1. Core Component Design

| Component              | Responsibility                                      | Implementation Suggestion              | Notes |
|------------------------|-----------------------------------------------------|----------------------------------------|-------|
| **KeyRotator / KeyPool** | Manage multiple API keys, rotation strategy, and health state | Reuse/adapt existing `KeyPool` logic   | Core of the router |
| **Proxy Handler**      | Receive request → select key → forward request → return response | `http.HandlerFunc` + manual forwarding | Simpler than `ReverseProxy` |
| **Error Classifier**   | Classify upstream errors and decide next action     | Map status code + body to action       | Critical for Tavily |
| **Health Checker**     | Actively test if upstream + at least one key is usable | Lightweight `/search` call             | Similar to existing `/health` |
| **Metrics Collector**  | Expose Prometheus metrics                           | `prometheus/client_golang`             | Optional but recommended |
| **Logger**             | Structured or simple logging                        | `log/slog` or standard `log` (optional)| Consider RPi 4 resource impact |

---

## 2. Error Handling & State Machine Suggestions

### Recommended State Machine

Each Tavily API key maintains one of the following states:

- **HEALTHY**: Key is available for use
- **COOLDOWN**: Key is temporarily unavailable after failure (auto-recovery after cooldown period)
- **DISABLED**: Key is permanently disabled (requires manual intervention)

### Error Classification & Recommended Actions

| HTTP Status | Error Type          | Recommended Action     | State Transition     | Error Label (for metrics) |
|-------------|---------------------|------------------------|----------------------|---------------------------|
| 200         | Success             | Mark as healthy        | → HEALTHY            | -                         |
| 429         | Rate Limit          | Enter cooldown         | → COOLDOWN           | `rate_limit`              |
| 401 / 403   | Authentication / Quota exhausted | Disable key     | → DISABLED           | `auth_error`              |
| 5xx         | Server Error        | Short cooldown         | → COOLDOWN           | `server_error`            |
| Timeout     | Connection Timeout  | Short cooldown         | → COOLDOWN           | `timeout`                 |
| Other       | Unknown             | Short cooldown         | → COOLDOWN           | `unknown`                 |

**Cooldown Logic Recommendation**:
- Use `Retry-After` header when available (Tavily sometimes returns it)
- Fall back to configurable `cooldown_sec` (default 300 seconds)
- Allow early recovery if a key succeeds again

---

## 3. Recommended Project Structure
tavily-smart-router/
├── main.go                      # All logic in one file (recommended)
├── Makefile
├── go.mod
├── config.example.json
├── Dockerfile
├── docker-compose.yml
├── .dockerignore
│
├── deploy/
│   └── systemd/
│       └── tavily-router.service
│
├── docs/
│   └── architecture.md
│
└── examples/
└── config.json
text**Design Note**: Keep everything in `main.go` for now to maintain simplicity and determinism, consistent with the existing `opencode-smart-router`.

---

## 4. Config Structure Suggestions

```go
type Config struct {
	// Core settings
	Keys         []string `json:"keys"`
	UpstreamBase string   `json:"upstream_base"`
	ListenAddr   string   `json:"listen_addr"`

	// Rotation & reliability
	Strategy               string `json:"strategy"` // "round_robin" or "least_used"
	CooldownSec            int    `json:"cooldown_sec"`
	MaxFailsBeforeCooldown int    `json:"max_fails_before_cooldown"`

	// Logging & Observability
	EnableRequestLog bool   `json:"enable_request_log"`
	LogFile          string `json:"log_file"`          // Empty = stdout only
	EnablePrometheus bool   `json:"enable_prometheus"`

	// Optional admin protection
	AdminUser string `json:"admin_user"`
	AdminPass string `json:"admin_pass"`
}
Resource Consideration for Raspberry Pi 4

File logging can be I/O intensive on SD cards.
→ Recommendation: Make enable_request_log and log_file optional and disabled by default.
Prometheus metrics have very low overhead when disabled.
Keep the router as lightweight as possible when running on RPi 4.


5. Environment Variable Support Suggestions
Support the following environment variables (priority over config file):
Goif keys := os.Getenv("TAVILY_KEYS"); keys != "" {
    cfg.Keys = strings.Split(keys, ",")
    for i := range cfg.Keys {
        cfg.Keys[i] = strings.TrimSpace(cfg.Keys[i])
    }
}
Recommended environment variables:

TAVILY_KEYS — Comma-separated list of API keys
TAVILY_UPSTREAM_BASE
TAVILY_LISTEN_ADDR
TAVILY_STRATEGY
TAVILY_COOLDOWN_SEC

This makes Docker deployment cleaner and avoids storing secrets in config files.

6. Default Value Suggestions
Goif cfg.ListenAddr == "" {
    cfg.ListenAddr = ":8082"
}
if cfg.UpstreamBase == "" {
    cfg.UpstreamBase = "https://api.tavily.com"
}
if cfg.Strategy == "" {
    cfg.Strategy = "least_used"
}
if cfg.CooldownSec == 0 {
    cfg.CooldownSec = 300
}
if cfg.MaxFailsBeforeCooldown == 0 {
    cfg.MaxFailsBeforeCooldown = 3
}
if cfg.EnablePrometheus == false {
    // Default: disabled on low-resource devices
}

7. Metrics Usage Suggestions
Recommended Prometheus Metrics
Gotavily_router_requests_total{key, status}
tavily_router_key_usage_total{key}
tavily_router_key_healthy{key}
tavily_router_request_duration_seconds{key}
tavily_router_key_cooldown_total{key}
tavily_router_upstream_errors_total{key, error_type}
Suggested Usage Points in Code



































WhenMetric to UpdateLabelsKey is selectedtavily_router_key_usage_totalkeyRequest completestavily_router_requests_total + durationkey, statusKey enters cooldowntavily_router_key_cooldown_totalkeyKey health status changestavily_router_key_healthykeySpecific upstream error occurstavily_router_upstream_errors_totalkey, error_type

8. Suggested Error Type Classification
Goconst (
	ErrorTypeRateLimit   = "rate_limit"
	ErrorTypeAuth        = "auth_error"
	ErrorTypeServerError = "server_error"
	ErrorTypeTimeout     = "timeout"
	ErrorTypeUnknown     = "unknown"
)
These labels make it easy to create Grafana alerts such as:

“Too many rate limit errors on Tavily keys”
“Authentication failure detected”


9. Suggested Config Validation Logic
Gofunc validateConfig(cfg *Config) error {
	if len(cfg.Keys) == 0 {
		return fmt.Errorf("at least one Tavily API key is required")
	}
	if cfg.MaxFailsBeforeCooldown < 1 {
		cfg.MaxFailsBeforeCooldown = 3
	}
	if cfg.CooldownSec < 30 {
		cfg.CooldownSec = 300 // prevent too aggressive cooldown
	}
	return nil
}
Call this function after loading config (from file or environment variables).

Summary
This design keeps the router:

Lightweight and suitable for Raspberry Pi 4
Consistent in style with the existing opencode-smart-router
Focused on reliability (state machine + proper error classification)
Observable (Prometheus metrics)
Easy to configure via environment variables or config file

The architecture prioritizes determinism and low resource consumption while remaining extensible.