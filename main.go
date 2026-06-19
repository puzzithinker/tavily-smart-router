package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var version = "dev"

// --- Config ---

type Config struct {
	ListenAddr                string   `json:"listen_addr"`
	UpstreamBase              string   `json:"upstream_base"`
	Keys                      []string `json:"keys"`
	Strategy                  string   `json:"strategy"`
	CooldownSec               int      `json:"cooldown_sec"`
	MaxFailsBeforeCooldown    int      `json:"max_fails_before_cooldown"`
	QuotaCooldownSec          int      `json:"quota_cooldown_sec"`
	HealthCheckTimeoutSeconds int      `json:"health_check_timeout_seconds"`
	AdminUser                 string   `json:"admin_user"`
	AdminPass                 string   `json:"admin_pass"`
	EnablePrometheus          bool     `json:"enable_prometheus"`
	EnableRequestLog          bool     `json:"enable_request_log"`
	LogFile                   string   `json:"log_file"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Apply defaults
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "0.0.0.0:8082"
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
	if cfg.QuotaCooldownSec == 0 {
		cfg.QuotaCooldownSec = 86400
	}
	if cfg.HealthCheckTimeoutSeconds == 0 {
		cfg.HealthCheckTimeoutSeconds = 10
	}

	// Env overrides (priority over config file)
	if envKeys := os.Getenv("TAVILY_KEYS"); envKeys != "" {
		cfg.Keys = parseKeysFromEnv(envKeys)
	}
	if v := os.Getenv("TAVILY_UPSTREAM_BASE"); v != "" {
		cfg.UpstreamBase = v
	}
	if v := os.Getenv("TAVILY_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("TAVILY_STRATEGY"); v != "" {
		cfg.Strategy = v
	}
	if v := os.Getenv("TAVILY_COOLDOWN_SEC"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			cfg.CooldownSec = sec
		}
	}
	if v := os.Getenv("TAVILY_QUOTA_COOLDOWN_SEC"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			cfg.QuotaCooldownSec = sec
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func parseKeysFromEnv(envValue string) []string {
	parts := strings.Split(envValue, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			keys = append(keys, p)
		}
	}
	return keys
}

func (c *Config) Validate() error {
	if len(c.Keys) == 0 {
		return fmt.Errorf("at least one Tavily API key is required (set in config or TAVILY_KEYS env)")
	}
	if c.Strategy != "round_robin" && c.Strategy != "least_used" {
		return fmt.Errorf("strategy must be 'round_robin' or 'least_used', got: %s", c.Strategy)
	}
	if c.MaxFailsBeforeCooldown < 1 {
		c.MaxFailsBeforeCooldown = 3
	}
	if c.CooldownSec < 30 {
		c.CooldownSec = 300 // prevent too aggressive cooldown
	}
	if c.QuotaCooldownSec < 60 {
		c.QuotaCooldownSec = 86400 // minimum 60 seconds for quota cooldown
	}
	return nil
}

// --- Key State Machine ---

type KeyState int

const (
	KeyHealthy  KeyState = iota
	KeyCooldown
	KeyDisabled
)

func (s KeyState) String() string {
	switch s {
	case KeyHealthy:
		return "healthy"
	case KeyCooldown:
		return "cooldown"
	case KeyDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

type KeyEntry struct {
	Key           string
	RawKey        string
	State         KeyState
	CooldownUntil time.Time
	UsageCount    int64
	FailCount    int64
	LastUsed      time.Time
	mu            sync.Mutex
}

type KeyRotator struct {
	keys     []*KeyEntry
	strategy string
	counter  atomic.Int64
	mu       sync.Mutex
}

func NewKeyRotator(keys []string, strategy string) *KeyRotator {
	entries := make([]*KeyEntry, len(keys))
	for i, k := range keys {
		entries[i] = &KeyEntry{
			Key:    MaskKey(k),
			RawKey: k,
			State:  KeyHealthy,
		}
	}
	return &KeyRotator{
		keys:     entries,
		strategy: strategy,
	}
}

func (kr *KeyRotator) PickKey() (*KeyEntry, error) {
	now := time.Now()

	switch kr.strategy {
	case "round_robin":
		n := len(kr.keys)
		start := int(kr.counter.Add(1) - 1)
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			entry := kr.keys[idx]
			entry.mu.Lock()
			if entry.State == KeyDisabled {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown && now.Before(entry.CooldownUntil) {
				entry.mu.Unlock()
				continue
			}
			// Key is available (healthy or cooldown expired)
			if entry.State == KeyCooldown {
				entry.State = KeyHealthy
				entry.CooldownUntil = time.Time{}
			}
			entry.UsageCount++
			entry.FailCount = 0 // reset fail count on successful pick
			entry.LastUsed = now
			entry.mu.Unlock()

			if cfg.EnablePrometheus {
				keyUsageTotal.WithLabelValues(entry.Key).Inc()
				keyHealthy.WithLabelValues(entry.Key).Set(1)
			}
			return entry, nil
		}
		return nil, fmt.Errorf("all keys are unavailable")

	case "least_used":
		kr.mu.Lock()

		// First pass: compute baseline UsageCount for normalizing recovering keys.
		// When a key recovers from cooldown, its UsageCount is much lower than keys
		// that handled traffic during the cooldown. Without normalization, ALL
		// requests would flood the recovering key (thundering herd), potentially
		// triggering another rate limit and creating oscillation.
		var baselineUsage int64
		for _, entry := range kr.keys {
			entry.mu.Lock()
			if entry.State == KeyDisabled {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown && now.Before(entry.CooldownUntil) {
				entry.mu.Unlock()
				continue
			}
			if entry.UsageCount > baselineUsage {
				baselineUsage = entry.UsageCount
			}
			entry.mu.Unlock()
		}

		// Second pass: build candidates with cooldown normalization
		var candidates []*KeyEntry
		var bestCount int64 = -1
		for _, entry := range kr.keys {
			entry.mu.Lock()
			if entry.State == KeyDisabled {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown && now.Before(entry.CooldownUntil) {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown {
				entry.State = KeyHealthy
				entry.CooldownUntil = time.Time{}
				// Normalize usage to prevent thundering herd on recovery
				entry.UsageCount = baselineUsage
			}
			if bestCount < 0 || entry.UsageCount < bestCount {
				bestCount = entry.UsageCount
				candidates = []*KeyEntry{entry}
			} else if entry.UsageCount == bestCount {
				candidates = append(candidates, entry)
			}
			entry.mu.Unlock()
		}
		if len(candidates) == 0 {
			kr.mu.Unlock()
			return nil, fmt.Errorf("all keys are unavailable")
		}
		// Rotate among tied candidates for even distribution
		best := candidates[int(kr.counter.Add(1))%len(candidates)]
		best.mu.Lock()
		best.UsageCount++
		best.FailCount = 0
		best.LastUsed = now
		best.mu.Unlock()
		kr.mu.Unlock()

		if cfg.EnablePrometheus {
			keyUsageTotal.WithLabelValues(best.Key).Inc()
			keyHealthy.WithLabelValues(best.Key).Set(1)
		}
		return best, nil

	default:
		return nil, fmt.Errorf("unknown strategy: %s", kr.strategy)
	}
}

func (kr *KeyRotator) MarkSuccess(key *KeyEntry) {
	key.mu.Lock()
	defer key.mu.Unlock()
	if key.State == KeyDisabled {
		return
	}
	key.State = KeyHealthy
	key.CooldownUntil = time.Time{}
	key.FailCount = 0

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(key.Key).Set(1)
	}

	slog.Info("key_recovered", "key", key.Key)
}

func (kr *KeyRotator) MarkCooldown(key *KeyEntry, duration time.Duration) {
	key.mu.Lock()
	defer key.mu.Unlock()
	key.State = KeyCooldown
	key.CooldownUntil = time.Now().Add(duration)
	key.FailCount = 0

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(key.Key).Set(0)
		keyCooldownTotal.WithLabelValues(key.Key).Inc()
	}

	slog.Info("key_cooldown", "key", key.Key, "duration", duration)
}

func (kr *KeyRotator) MarkFail(key *KeyEntry) {
	key.mu.Lock()
	defer key.mu.Unlock()

	if key.State == KeyDisabled {
		return
	}

	key.FailCount++
	if key.FailCount >= int64(cfg.MaxFailsBeforeCooldown) {
		// Transition to cooldown after N consecutive failures
		key.State = KeyCooldown
		key.CooldownUntil = time.Now().Add(time.Duration(cfg.CooldownSec) * time.Second)
		key.FailCount = 0

		if cfg.EnablePrometheus {
			keyHealthy.WithLabelValues(key.Key).Set(0)
			keyCooldownTotal.WithLabelValues(key.Key).Inc()
		}

		slog.Info("key_cooldown", "key", key.Key, "duration", fmt.Sprintf("%ds", cfg.CooldownSec))
	}
}

func (kr *KeyRotator) MarkDisabled(key *KeyEntry) {
	key.mu.Lock()
	defer key.mu.Unlock()
	key.State = KeyDisabled

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(key.Key).Set(0)
	}

	slog.Info("key_disabled", "key", key.Key)
}

func (kr *KeyRotator) HealthyCount() int {
	now := time.Now()
	count := 0
	for _, entry := range kr.keys {
		entry.mu.Lock()
		if entry.State == KeyHealthy || (entry.State == KeyCooldown && !now.Before(entry.CooldownUntil)) {
			count++
		}
		entry.mu.Unlock()
	}
	return count
}

func (kr *KeyRotator) TotalCount() int {
	return len(kr.keys)
}

func (kr *KeyRotator) DisabledCount() int {
	count := 0
	for _, entry := range kr.keys {
		entry.mu.Lock()
		if entry.State == KeyDisabled {
			count++
		}
		entry.mu.Unlock()
	}
	return count
}

func (kr *KeyRotator) EnableKey(index int) error {
	if index < 0 || index >= len(kr.keys) {
		return fmt.Errorf("key index %d out of range (0-%d)", index, len(kr.keys)-1)
	}
	entry := kr.keys[index]
	entry.mu.Lock()
	entry.State = KeyHealthy
	entry.CooldownUntil = time.Time{}
	entry.FailCount = 0
	entry.mu.Unlock()

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(entry.Key).Set(1)
	}

	slog.Info("key_manually_enabled", "key", entry.Key, "index", index)
	return nil
}

func (kr *KeyRotator) DisableKey(index int) error {
	if index < 0 || index >= len(kr.keys) {
		return fmt.Errorf("key index %d out of range (0-%d)", index, len(kr.keys)-1)
	}
	entry := kr.keys[index]
	entry.mu.Lock()
	entry.State = KeyDisabled
	entry.mu.Unlock()

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(entry.Key).Set(0)
	}

	slog.Info("key_manually_disabled", "key", entry.Key, "index", index)
	return nil
}

func (kr *KeyRotator) ResetKeyStats(index int) error {
	if index < 0 || index >= len(kr.keys) {
		return fmt.Errorf("key index %d out of range (0-%d)", index, len(kr.keys)-1)
	}
	entry := kr.keys[index]
	entry.mu.Lock()
	entry.UsageCount = 0
	entry.FailCount = 0
	entry.mu.Unlock()

	slog.Info("key_stats_reset", "key", entry.Key, "index", index)
	return nil
}

func MaskKey(key string) string {
	l := len(key)
	if l > 8 {
		return key[:5] + "..." + key[l-3:]
	}
	if l > 3 {
		return key[:3] + "***"
	}
	return key + "***"
}

func ParseRetryAfter(header string, defaultDuration time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if seconds, err := strconv.Atoi(header); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return defaultDuration
}

// --- Error Classification ---

// Error type constants for metrics
const (
	ErrorTypeRateLimit   = "rate_limit"
	ErrorTypeAuth        = "auth_error"
	ErrorTypeServerError = "server_error"
	ErrorTypeTimeout     = "timeout"
	ErrorTypeUnknown     = "unknown"
)

// tavilyErrorBody represents a Tavily error response
type tavilyErrorBody struct {
	Detail string `json:"detail"`
	Error  string `json:"error"`
}

// --- Proxy ---

type contextKey string

const (
	keyCtxKey       contextKey = "proxy_key"
	classifyCtxKey  contextKey = "classify_result"
	startTimeCtxKey contextKey = "start_time"
)

// ClassificationResult stores whether a response should trigger a retry.
type ClassificationResult struct {
	ShouldRetry bool
	StatusCode  int
}

// classifyHolder wraps a ClassificationResult pointer so ModifyResponse can
// write to it through the request context.
type classifyHolder struct {
	result *ClassificationResult
}

// bufferedResponseWriter captures the full response so we can decide
// whether to forward it to the real client or discard and retry.
type bufferedResponseWriter struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
	wroteCode  bool
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{
		header: make(http.Header),
	}
}

func (b *bufferedResponseWriter) Header() http.Header {
	return b.header
}

func (b *bufferedResponseWriter) Write(data []byte) (int, error) {
	if !b.wroteCode {
		b.statusCode = http.StatusOK
		b.wroteCode = true
	}
	return b.body.Write(data)
}

func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.wroteCode {
		return
	}
	b.statusCode = code
	b.wroteCode = true
}

func (b *bufferedResponseWriter) writeTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	w.WriteHeader(b.statusCode)
	if _, err := b.body.WriteTo(w); err != nil {
		slog.Error("failed to write buffered response", "error", err)
	}
}

func newReverseProxy(upstreamURL *url.URL) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstreamURL)
			r.SetXForwarded()

			// Strip hop-by-hop headers
			hopByHop := []string{
				"Connection",
				"Keep-Alive",
				"Proxy-Authenticate",
				"Proxy-Authorization",
				"Transfer-Encoding",
				"Upgrade",
			}
			for _, h := range hopByHop {
				r.Out.Header.Del(h)
			}

			// Set Authorization from context key
			key, _ := r.In.Context().Value(keyCtxKey).(*KeyEntry)
			if key != nil {
				r.Out.Header.Set("Authorization", "Bearer "+key.RawKey)
			}
		},
		ModifyResponse: classifyResponse,
		ErrorHandler:   proxyErrorHandler,
	}
	return rp
}

func proxyHandler(rp *httputil.ReverseProxy, rotator *KeyRotator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Buffer the request body for potential retries
		var bodyBytes []byte
		if r.Body != nil {
			var err error
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				writeRouterError(w, "failed to read request body", "server_error", "request_body_read_error", http.StatusInternalServerError)
				return
			}
			r.Body.Close()
		}

		maxRetries := rotator.TotalCount()
		lastStatusCode := 0

		for attempt := 0; attempt < maxRetries; attempt++ {
			key, err := rotator.PickKey()
			if err != nil {
				// All keys exhausted
				msg := "all API keys are unavailable"
				errType := "server_error"
				code := "all_keys_exhausted"
				statusCode := http.StatusTooManyRequests
				if lastStatusCode == 401 || lastStatusCode == 403 {
					statusCode = lastStatusCode
					msg = "authentication failed with all API keys"
					errType = "authentication_error"
					code = "auth_failed"
				}
				writeRouterError(w, msg, errType, code, statusCode)
				return
			}

			slog.Info("key_selected", "key", key.Key, "strategy", rotator.strategy, "attempt", attempt+1)

			// Create a fresh request for each attempt
			newReq := r.Clone(r.Context())
			if bodyBytes != nil {
				newReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				newReq.ContentLength = int64(len(bodyBytes))
			}

			// Store key and classification holder in context
			holder := &classifyHolder{}
			ctx := context.WithValue(newReq.Context(), keyCtxKey, key)
			ctx = context.WithValue(ctx, classifyCtxKey, holder)
			ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
			newReq = newReq.WithContext(ctx)

			// Use a buffered response writer so we can retry if needed
			buf := newBufferedResponseWriter()

			rp.ServeHTTP(buf, newReq)

			// Check classification result
			if holder.result != nil && holder.result.ShouldRetry {
				lastStatusCode = holder.result.StatusCode
				slog.Info("transparent_retry", "key", key.Key, "status", holder.result.StatusCode, "attempt", attempt+1)
				// Discard buffer and try next key
				continue
			}

			// Success or non-retryable error — forward buffered response to client
			buf.writeTo(w)
			return
		}

		// All retries exhausted
		allExhaustedMsg := "all API keys exhausted after retries"
		allExhaustedType := "server_error"
		allExhaustedCode := "all_keys_exhausted"
		allExhaustedStatus := http.StatusTooManyRequests
		if lastStatusCode == http.StatusUnauthorized || lastStatusCode == http.StatusForbidden {
			allExhaustedMsg = "authentication failed with all API keys"
			allExhaustedType = "authentication_error"
			allExhaustedCode = "auth_failed"
			allExhaustedStatus = lastStatusCode
		}
		writeRouterError(w, allExhaustedMsg, allExhaustedType, allExhaustedCode, allExhaustedStatus)
	}
}

func classifyResponse(resp *http.Response) error {
	key, _ := resp.Request.Context().Value(keyCtxKey).(*KeyEntry)
	holder, _ := resp.Request.Context().Value(classifyCtxKey).(*classifyHolder)
	startTime, _ := resp.Request.Context().Value(startTimeCtxKey).(time.Time)

	if key == nil {
		return nil
	}

	duration := time.Since(startTime)
	statusCode := resp.StatusCode

	// Record metrics helper
	recordMetrics := func(errorType string) {
		if cfg.EnablePrometheus {
			requestsTotal.WithLabelValues(key.Key, statusGroup(statusCode)).Inc()
			requestDuration.WithLabelValues(key.Key).Observe(duration.Seconds())
			if errorType != "" {
				upstreamErrorsTotal.WithLabelValues(key.Key, errorType).Inc()
			}
		}
	}

	// Success (2xx)
	if statusCode >= 200 && statusCode < 300 {
		rotator.MarkSuccess(key)
		recordMetrics("")
		slog.Info("request_forwarded", "key", key.Key, "status", statusCode)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
		}
		return nil
	}

	// 401/403 — authentication/quota failure, permanently disable key
	if statusCode == 401 || statusCode == 403 {
		rotator.MarkDisabled(key)
		recordMetrics(ErrorTypeAuth)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
		}
		return fmt.Errorf("key disabled: status %d", statusCode)
	}

	// 432 — Tavily key/plan limit exceeded, cooldown key (quotas reset monthly)
	if statusCode == 432 {
		rotator.MarkCooldown(key, time.Duration(cfg.QuotaCooldownSec)*time.Second)
		recordMetrics(ErrorTypeAuth)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
		}
		return fmt.Errorf("key cooldown: plan limit exceeded (432)")
	}

	// 433 — Tavily PayGo limit exceeded, cooldown key (quotas reset monthly)
	if statusCode == 433 {
		rotator.MarkCooldown(key, time.Duration(cfg.QuotaCooldownSec)*time.Second)
		recordMetrics(ErrorTypeAuth)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
		}
		return fmt.Errorf("key cooldown: paygo limit exceeded (433)")
	}

	// 429 — rate limit or insufficient quota
	if statusCode == 429 {
		// Parse body to check for quota exhaustion
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Check if it's a quota/plan limit error (instead of just rate limit)
		var errBody tavilyErrorBody
		if json.Unmarshal(bodyBytes, &errBody) == nil {
			detail := strings.ToLower(errBody.Detail)
			errMsg := strings.ToLower(errBody.Error)
			if strings.Contains(detail, "quota") || strings.Contains(detail, "plan limit") ||
				strings.Contains(detail, "usage limit") || strings.Contains(errMsg, "quota") ||
				strings.Contains(errMsg, "plan limit") || strings.Contains(errMsg, "usage limit") {
				rotator.MarkCooldown(key, time.Duration(cfg.QuotaCooldownSec)*time.Second)
				recordMetrics(ErrorTypeAuth)
				if holder != nil {
					holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
				}
				return fmt.Errorf("key cooldown: quota exhausted (429)")
			}
		}

		// Regular rate limit — use MarkFail for consecutive failure tracking
		retryAfter := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Duration(cfg.CooldownSec)*time.Second)
		rotator.MarkCooldown(key, retryAfter)
		recordMetrics(ErrorTypeRateLimit)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
		}
		return fmt.Errorf("key in cooldown: rate limited")
	}

	// 5xx — upstream server error, don't retry, mark short fail
	if statusCode >= 500 {
		rotator.MarkFail(key)
		recordMetrics(ErrorTypeServerError)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
		}
		return nil
	}

	// Other 4xx — client error, don't retry, mark short fail
	rotator.MarkFail(key)
	recordMetrics(ErrorTypeUnknown)
	if holder != nil {
		holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
	}
	return nil
}

func proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	key, _ := r.Context().Value(keyCtxKey).(*KeyEntry)
	holder, _ := r.Context().Value(classifyCtxKey).(*classifyHolder)

	if key != nil {
		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
			rotator.MarkCooldown(key, 10*time.Second)
			if cfg.EnablePrometheus {
				upstreamErrorsTotal.WithLabelValues(key.Key, ErrorTypeTimeout).Inc()
			}
		}
	}

	if holder != nil && holder.result == nil {
		holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: http.StatusBadGateway}
	}

	writeRouterError(w, fmt.Sprintf("upstream error: %s", err.Error()), "server_error", "upstream_error", http.StatusBadGateway)
}

// --- Middleware ---

func basicAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If admin_pass is empty, admin endpoints are disabled
		if cfg.AdminPass == "" {
			http.Error(w, "admin endpoints disabled", http.StatusForbidden)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != cfg.AdminUser || pass != cfg.AdminPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="tavily-router"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// --- Handlers ---

func healthHandler(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{
		Timeout: time.Duration(cfg.HealthCheckTimeoutSeconds) * time.Second,
	}

	// Use PickKey() to rotate health checks across keys — avoids
	// always hitting the first key in the slice and biasing its quota usage.
	key, err := rotator.PickKey()

	result := map[string]interface{}{
		"healthy_keys":  rotator.HealthyCount(),
		"total_keys":    rotator.TotalCount(),
		"disabled_keys": rotator.DisabledCount(),
	}

	if err != nil {
		result["status"] = "unhealthy"
		result["upstream"] = "no_healthy_keys"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}

	result["key"] = key.Key

	// Perform a lightweight Tavily search to verify upstream connectivity
	upstreamURL, _ := url.Parse(cfg.UpstreamBase)
	checkURL := upstreamURL.JoinPath("/search")

	// Minimal request body for health check
	healthBody := map[string]interface{}{
		"query":         "health_check",
		"max_results":   1,
		"search_depth":  "basic",
	}
	bodyBytes, err := json.Marshal(healthBody)
	if err != nil {
		result["status"] = "unhealthy"
		result["upstream"] = "marshal_error"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, checkURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		result["status"] = "unhealthy"
		result["upstream"] = "url_error"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}
	req.Header.Set("Authorization", "Bearer "+key.RawKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		rotator.MarkFail(key)
		result["status"] = "unhealthy"
		result["upstream"] = "unreachable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		rotator.MarkSuccess(key)
		result["status"] = "healthy"
		result["upstream"] = "reachable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	} else if resp.StatusCode == 401 || resp.StatusCode == 403 {
		rotator.MarkDisabled(key)
		result["status"] = "unhealthy"
		result["upstream"] = "auth_failed"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if resp.StatusCode == 432 || resp.StatusCode == 433 {
		rotator.MarkCooldown(key, time.Duration(cfg.QuotaCooldownSec)*time.Second)
		result["status"] = "unhealthy"
		result["upstream"] = "quota_exhausted"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if resp.StatusCode == 429 {
		retryAfter := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Duration(cfg.CooldownSec)*time.Second)
		rotator.MarkCooldown(key, retryAfter)
		result["status"] = "unhealthy"
		result["upstream"] = "rate_limited"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		rotator.MarkFail(key)
		result["status"] = "unhealthy"
		result["upstream"] = fmt.Sprintf("http_%d", resp.StatusCode)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	keys := make([]map[string]interface{}, 0, len(rotator.keys))
	var totalRequests int64

	for i, entry := range rotator.keys {
		entry.mu.Lock()
		keyStat := map[string]interface{}{
			"index":       i,
			"masked_key":  entry.Key,
			"state":       entry.State.String(),
			"usage_count": entry.UsageCount,
			"fail_count":  entry.FailCount,
		}
		if !entry.LastUsed.IsZero() {
			keyStat["last_used"] = entry.LastUsed.Format(time.RFC3339)
		} else {
			keyStat["last_used"] = nil
		}
		totalRequests += entry.UsageCount
		entry.mu.Unlock()
		keys = append(keys, keyStat)
	}

	result := map[string]interface{}{
		"keys":           keys,
		"total_requests": totalRequests,
		"strategy":       rotator.strategy,
	}

	if totalRequests > 0 {
		numKeys := int64(len(rotator.keys))
		if numKeys > 0 {
			idealPerKey := float64(totalRequests) / float64(numKeys)
			var sumSquaredDiff float64
			for _, entry := range rotator.keys {
				entry.mu.Lock()
				dev := float64(entry.UsageCount) - idealPerKey
				sumSquaredDiff += dev * dev
				if totalRequests > 0 {
					keyStat := findKeyStat(keys, entry.Key)
					if keyStat != nil {
						keyStat["usage_pct"] = float64(entry.UsageCount) / float64(totalRequests) * 100
					}
				}
				entry.mu.Unlock()
			}
			stdDev := math.Sqrt(sumSquaredDiff / float64(numKeys))
			var coeffVar float64
			if idealPerKey > 0 {
				coeffVar = stdDev / idealPerKey
			}
			result["distribution"] = map[string]interface{}{
				"ideal_per_key":      idealPerKey,
				"std_dev":           stdDev,
				"coefficient_of_var": coeffVar,
				"fairness_ratio":     fairnessRatio(rotator.keys, totalRequests),
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

func findKeyStat(keys []map[string]interface{}, maskedKey string) map[string]interface{} {
	for _, ks := range keys {
		if ks["masked_key"] == maskedKey {
			return ks
		}
	}
	return nil
}

func fairnessRatio(entries []*KeyEntry, totalRequests int64) float64 {
	if totalRequests == 0 || len(entries) <= 1 {
		return 1.0
	}
	var minUsage, maxUsage int64
	minUsage = math.MaxInt64
	for _, entry := range entries {
		entry.mu.Lock()
		if entry.UsageCount < minUsage {
			minUsage = entry.UsageCount
		}
		if entry.UsageCount > maxUsage {
			maxUsage = entry.UsageCount
		}
		entry.mu.Unlock()
	}
	if maxUsage == 0 {
		return 1.0
	}
	return float64(minUsage) / float64(maxUsage)
}

func totalUsageCount(entries []*KeyEntry) int64 {
	var total int64
	for _, entry := range entries {
		entry.mu.Lock()
		total += entry.UsageCount
		entry.mu.Unlock()
	}
	return total
}

func keyControlHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	if len(parts) != 4 || parts[0] != "admin" || parts[1] != "keys" {
		http.Error(w, "invalid path, expected /admin/keys/{index}/{action}", http.StatusBadRequest)
		return
	}

	index, err := strconv.Atoi(parts[2])
	if err != nil {
		http.Error(w, "invalid key index", http.StatusBadRequest)
		return
	}

	action := parts[3]
	var actionErr error
	switch action {
	case "enable":
		actionErr = rotator.EnableKey(index)
	case "disable":
		actionErr = rotator.DisableKey(index)
	case "reset":
		actionErr = rotator.ResetKeyStats(index)
	default:
		http.Error(w, "unknown action, expected enable|disable|reset", http.StatusBadRequest)
		return
	}

	if actionErr != nil {
		http.Error(w, actionErr.Error(), http.StatusBadRequest)
		return
	}

	result := map[string]interface{}{
		"ok":      true,
		"index":   index,
		"action":  action,
	}
	entry := rotator.keys[index]
	entry.mu.Lock()
	result["key"] = entry.Key
	result["state"] = entry.State.String()
	result["usage_count"] = entry.UsageCount
	entry.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

// --- Metrics ---

var (
	requestsTotal      *prometheus.CounterVec
	keyUsageTotal      *prometheus.CounterVec
	keyHealthy         *prometheus.GaugeVec
	requestDuration    *prometheus.HistogramVec
	keyCooldownTotal   *prometheus.CounterVec
	upstreamErrorsTotal *prometheus.CounterVec
	keyFairness        prometheus.GaugeFunc
)

func initMetrics() {
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tavily_router_requests_total",
			Help: "Total number of requests proxied",
		},
		[]string{"key", "status_group"},
	)

	keyUsageTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tavily_router_key_usage_total",
			Help: "Number of times each key was selected",
		},
		[]string{"key"},
	)

	keyHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tavily_router_key_healthy",
			Help: "Whether a key is healthy (1) or not (0)",
		},
		[]string{"key"},
	)

	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "tavily_router_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"key"},
	)

	keyCooldownTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tavily_router_key_cooldown_total",
			Help: "Number of times each key entered cooldown",
		},
		[]string{"key"},
	)

	upstreamErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tavily_router_upstream_errors_total",
			Help: "Number of upstream errors by key and error type",
		},
		[]string{"key", "error_type"},
	)

	keyFairness = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "tavily_router_key_distribution_fairness",
			Help: "Fairness ratio: min_usage / max_usage across keys (1.0 = perfectly even, 0.0 = completely skewed)",
		},
		func() float64 {
			return fairnessRatio(rotator.keys, totalUsageCount(rotator.keys))
		},
	)

	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(keyUsageTotal)
	prometheus.MustRegister(keyHealthy)
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(keyCooldownTotal)
	prometheus.MustRegister(upstreamErrorsTotal)
	prometheus.MustRegister(keyFairness)

	// Initialize gauges for all keys
	for _, entry := range rotator.keys {
		keyHealthy.WithLabelValues(entry.Key).Set(1)
	}
}

func statusGroup(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "other"
	}
}

// --- Logging ---

var logFile *os.File

func setupLogging() {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))

	if cfg.EnableRequestLog && cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			slog.Warn("failed to open log file, logging to stdout", "path", cfg.LogFile, "error", err)
		} else {
			logFile = f
			handler = slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})
			slog.SetDefault(slog.New(handler))
		}
	}
}

// --- Router Error Format ---

type RouterError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeRouterError(w http.ResponseWriter, message, errType, code string, statusCode int) {
	errResp := RouterError{}
	errResp.Error.Message = message
	errResp.Error.Type = errType
	errResp.Error.Code = code

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errResp) //nolint:errcheck
}

// --- Main ---

var (
	cfg     *Config
	rotator *KeyRotator
)

func main() {
	configPath := "config.json"
	if envPath := os.Getenv("TAVILY_CONFIG"); envPath != "" {
		configPath = envPath
	}

	var err error
	cfg, err = LoadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	setupLogging()

	rotator = NewKeyRotator(cfg.Keys, cfg.Strategy)

	slog.Info("startup", "keys", rotator.TotalCount(), "strategy", cfg.Strategy, "listen", cfg.ListenAddr, "upstream", cfg.UpstreamBase)
	slog.Info("startup", "version", version)

	upstreamURL, err := url.Parse(cfg.UpstreamBase)
	if err != nil {
		slog.Error("failed to parse upstream URL", "error", err)
		os.Exit(1)
	}

	rp := newReverseProxy(upstreamURL)

	mux := http.NewServeMux()

	// Proxy handler with transparent retry — handle all Tavily API paths
	mux.HandleFunc("/search", proxyHandler(rp, rotator))
	mux.HandleFunc("/extract", proxyHandler(rp, rotator))
	mux.HandleFunc("/crawl", proxyHandler(rp, rotator))
	mux.HandleFunc("/map", proxyHandler(rp, rotator))
	// Catch-all for any other API paths
	mux.HandleFunc("/", proxyHandler(rp, rotator))

	// Health endpoint
	mux.HandleFunc("/health", healthHandler)

	// Admin endpoints with basic auth
	mux.HandleFunc("/admin/stats", basicAuthMiddleware(statsHandler))
	mux.HandleFunc("/admin/keys/", basicAuthMiddleware(keyControlHandler))

	// Prometheus metrics
	if cfg.EnablePrometheus {
		initMetrics()
		mux.Handle("/metrics", promhttp.Handler())
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutdown", "message", "received signal, shutting down gracefully")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
		os.Exit(1)
	}

	if logFile != nil {
		logFile.Close()
	}

	slog.Info("shutdown", "message", "server stopped")
}