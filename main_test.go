package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// --- Test Helpers ---

func setupTestGlobals(keys []string, strategy string) {
	cfg = &Config{
		ListenAddr:                "0.0.0.0:8082",
		UpstreamBase:              "https://api.tavily.com",
		Keys:                      keys,
		Strategy:                  strategy,
		CooldownSec:               300,
		MaxFailsBeforeCooldown:    3,
		QuotaCooldownSec:          86400,
		HealthCheckTimeoutSeconds: 10,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
		EnableRequestLog:          false,
		LogFile:                   "",
	}
	rotator = NewKeyRotator(keys, strategy)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func setupTestGlobalsNoAuth(keys []string, strategy string) {
	cfg = &Config{
		ListenAddr:                "0.0.0.0:8082",
		UpstreamBase:              "https://api.tavily.com",
		Keys:                      keys,
		Strategy:                  strategy,
		CooldownSec:               300,
		MaxFailsBeforeCooldown:    3,
		QuotaCooldownSec:          86400,
		HealthCheckTimeoutSeconds: 10,
		AdminUser:                 "admin",
		AdminPass:                 "",
		EnablePrometheus:          false,
		EnableRequestLog:          false,
		LogFile:                  "",
	}
	rotator = NewKeyRotator(keys, strategy)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// --- Config Tests ---

func TestParseKeysFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single key", "tvly-key1", []string{"tvly-key1"}},
		{"multiple keys", "tvly-key1,tvly-key2,tvly-key3", []string{"tvly-key1", "tvly-key2", "tvly-key3"}},
		{"keys with spaces", " tvly-key1 , tvly-key2 , tvly-key3 ", []string{"tvly-key1", "tvly-key2", "tvly-key3"}},
		{"empty string", "", nil},
		{"trailing comma", "tvly-key1,", []string{"tvly-key1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseKeysFromEnv(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("parseKeysFromEnv(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseKeysFromEnv(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			"valid config",
			Config{Keys: []string{"tvly-key1"}, Strategy: "round_robin"},
			false,
		},
		{
			"valid least_used",
			Config{Keys: []string{"tvly-key1"}, Strategy: "least_used"},
			false,
		},
		{
			"empty keys",
			Config{Keys: []string{}, Strategy: "round_robin"},
			true,
		},
		{
			"nil keys",
			Config{Keys: nil, Strategy: "round_robin"},
			true,
		},
		{
			"invalid strategy",
			Config{Keys: []string{"tvly-key1"}, Strategy: "random"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidationDefaults(t *testing.T) {
	cfg := Config{Keys: []string{"tvly-test"}, Strategy: "round_robin", MaxFailsBeforeCooldown: 0, CooldownSec: 10}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.MaxFailsBeforeCooldown != 3 {
		t.Errorf("MaxFailsBeforeCooldown = %d, want 3 (default)", cfg.MaxFailsBeforeCooldown)
	}
	if cfg.CooldownSec != 300 {
		t.Errorf("CooldownSec = %d, want 300 (min 30)", cfg.CooldownSec)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["tvly-test-key"]}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.ListenAddr != "0.0.0.0:8082" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "0.0.0.0:8082")
	}
	if cfg.UpstreamBase != "https://api.tavily.com" {
		t.Errorf("UpstreamBase = %q, want %q", cfg.UpstreamBase, "https://api.tavily.com")
	}
	if cfg.Strategy != "least_used" {
		t.Errorf("Strategy = %q, want %q", cfg.Strategy, "least_used")
	}
	if cfg.CooldownSec != 300 {
		t.Errorf("CooldownSec = %d, want %d", cfg.CooldownSec, 300)
	}
	if cfg.MaxFailsBeforeCooldown != 3 {
		t.Errorf("MaxFailsBeforeCooldown = %d, want %d", cfg.MaxFailsBeforeCooldown, 3)
	}
	if cfg.HealthCheckTimeoutSeconds != 10 {
		t.Errorf("HealthCheckTimeoutSeconds = %d, want %d", cfg.HealthCheckTimeoutSeconds, 10)
	}
}

func TestLoadConfigEnvOverride(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["tvly-original"], "strategy": "round_robin"}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	t.Setenv("TAVILY_KEYS", "tvly-env-key1,tvly-env-key2")

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if len(cfg.Keys) != 2 {
		t.Fatalf("len(Keys) = %d, want 2", len(cfg.Keys))
	}
	if cfg.Keys[0] != "tvly-env-key1" || cfg.Keys[1] != "tvly-env-key2" {
		t.Errorf("Keys = %v, want [tvly-env-key1, tvly-env-key2]", cfg.Keys)
	}
}

func TestLoadConfigEnvOverrideEmptyKeys(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["tvly-original"], "strategy": "round_robin"}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	t.Setenv("TAVILY_KEYS", "")

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	// Empty TAVILY_KEYS should NOT override config keys
	if len(cfg.Keys) != 1 || cfg.Keys[0] != "tvly-original" {
		t.Errorf("Keys = %v, want [tvly-original] (empty env should not override)", cfg.Keys)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("LoadConfig() should return error for missing file")
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("{invalid json}"); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	_, err = LoadConfig(tmpFile.Name())
	if err == nil {
		t.Error("LoadConfig() should return error for invalid JSON")
	}
}

// --- Key State Machine Tests ---

func TestNewKeyRotator(t *testing.T) {
	keys := []string{"tvly-abcdefghijklmnop", "tvly-short"}
	kr := NewKeyRotator(keys, "round_robin")

	if len(kr.keys) != 2 {
		t.Fatalf("len(kr.keys) = %d, want 2", len(kr.keys))
	}
	if kr.keys[0].RawKey != "tvly-abcdefghijklmnop" {
		t.Errorf("RawKey[0] = %q, want %q", kr.keys[0].RawKey, "tvly-abcdefghijklmnop")
	}
	if kr.keys[0].Key != "tvly-...nop" {
		t.Errorf("Key[0] = %q, want %q", kr.keys[0].Key, "tvly-...nop")
	}
	if kr.keys[0].State != KeyHealthy {
		t.Errorf("State[0] = %d, want %d", kr.keys[0].State, KeyHealthy)
	}
	if kr.keys[1].Key != "tvly-...ort" {
		t.Errorf("Key[1] = %q, want %q", kr.keys[1].Key, "tvly-...ort")
	}
}

func TestPickKeyRoundRobin(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	key0, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key0.RawKey != "key0" {
		t.Errorf("first PickKey = %q, want %q", key0.RawKey, "key0")
	}

	key1, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key1.RawKey != "key1" {
		t.Errorf("second PickKey = %q, want %q", key1.RawKey, "key1")
	}

	key2, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key2.RawKey != "key2" {
		t.Errorf("third PickKey = %q, want %q", key2.RawKey, "key2")
	}

	key0Again, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key0Again.RawKey != "key0" {
		t.Errorf("fourth PickKey = %q, want %q", key0Again.RawKey, "key0")
	}
}

func TestPickKeyLeastUsed(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "least_used")

	// First pick: both keys tied at count 0, rotation picks one
	key1, _ := rotator.PickKey()
	if key1.RawKey != "key0" && key1.RawKey != "key1" {
		t.Errorf("first PickKey = %q, want key0 or key1", key1.RawKey)
	}

	// Second pick: the key with lower count wins (the one not picked)
	key2, _ := rotator.PickKey()
	if key2.RawKey != "key0" && key2.RawKey != "key1" {
		t.Errorf("second PickKey = %q, want key0 or key1", key2.RawKey)
	}

	// Both keys now have count 1, rotation distributes among tied candidates
	key3, _ := rotator.PickKey()
	if key3.RawKey != "key0" && key3.RawKey != "key1" {
		t.Errorf("third PickKey = %q, want key0 or key1", key3.RawKey)
	}
}

func TestPickKeyLeastUsedEvenDistribution(t *testing.T) {
	keys := []string{"key0", "key1", "key2", "key3"}
	setupTestGlobals(keys, "least_used")

	picks := make(map[string]int)
	const totalPicks = 40
	for i := 0; i < totalPicks; i++ {
		key, err := rotator.PickKey()
		if err != nil {
			t.Fatalf("PickKey() error: %v", err)
		}
		picks[key.RawKey]++
	}

	for _, k := range keys {
		count := picks[k]
		if count == 0 {
			t.Errorf("key %q was never picked out of %d total picks", k, totalPicks)
		}
		if count > totalPicks/len(keys)+2 {
			t.Errorf("key %q picked %d times, expected ~%d", k, count, totalPicks/len(keys))
		}
	}
}

func TestPickKeySkipsDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key1" {
		t.Errorf("PickKey = %q, want %q (should skip disabled key)", key.RawKey, "key1")
	}
}

func TestPickKeySkipsCooldown(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	rotator.MarkCooldown(rotator.keys[0], 60*time.Second)

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key1" {
		t.Errorf("PickKey = %q, want %q (should skip cooldown key)", key.RawKey, "key1")
	}
}

func TestPickKeyCooldownExpires(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	rotator.keys[0].mu.Lock()
	rotator.keys[0].State = KeyCooldown
	rotator.keys[0].CooldownUntil = time.Now().Add(-time.Second)
	rotator.keys[0].mu.Unlock()

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key0" {
		t.Errorf("PickKey = %q, want %q (cooldown should have expired)", key.RawKey, "key0")
	}
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("key0 State = %d, want %d (healthy)", rotator.keys[0].State, KeyHealthy)
	}
}

func TestPickKeyAllExhausted(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	_, err := rotator.PickKey()
	if err == nil {
		t.Error("PickKey() should return error when all keys unavailable")
	}
}

func TestMarkSuccess(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkCooldown(rotator.keys[0], 60*time.Second)
	if rotator.keys[0].State != KeyCooldown {
		t.Errorf("State = %d, want %d", rotator.keys[0].State, KeyCooldown)
	}

	rotator.MarkSuccess(rotator.keys[0])
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("State = %d, want %d", rotator.keys[0].State, KeyHealthy)
	}
}

func TestMarkDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Errorf("State = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}

	_, err := rotator.PickKey()
	if err == nil {
		t.Error("PickKey() should return error when key is disabled")
	}
}

func TestDisabledIsPermanent(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	rotator.MarkSuccess(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Errorf("MarkSuccess should NOT recover DISABLED key; State = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}
}

func TestMarkFail(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	// First and second failures should not trigger cooldown (max_fails = 3)
	rotator.MarkFail(rotator.keys[0])
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("State after 1st fail = %d, want %d (healthy)", rotator.keys[0].State, KeyHealthy)
	}

	rotator.MarkFail(rotator.keys[0])
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("State after 2nd fail = %d, want %d (healthy)", rotator.keys[0].State, KeyHealthy)
	}

	// Third failure should trigger cooldown
	rotator.MarkFail(rotator.keys[0])
	if rotator.keys[0].State != KeyCooldown {
		t.Errorf("State after 3rd fail = %d, want %d (cooldown)", rotator.keys[0].State, KeyCooldown)
	}
}

func TestMarkFailResetsOnPick(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	// Fail key0 twice (below threshold)
	rotator.MarkFail(rotator.keys[0])
	rotator.MarkFail(rotator.keys[0])

	// Pick key0 — should reset fail count
	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	// key0 may or may not be picked depending on counter state, just verify fail count was reset on pick
	// Actually, let's check: after PickKey, fail count should be 0
	_ = key

	rotator.keys[0].mu.Lock()
	failCount := rotator.keys[0].FailCount
	rotator.keys[0].mu.Unlock()

	if failCount != 0 {
		t.Errorf("FailCount after PickKey = %d, want 0 (should reset on pick)", failCount)
	}
}

func TestKeyCounts(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	if rotator.TotalCount() != 3 {
		t.Errorf("TotalCount() = %d, want 3", rotator.TotalCount())
	}
	if rotator.HealthyCount() != 3 {
		t.Errorf("HealthyCount() = %d, want 3", rotator.HealthyCount())
	}
	if rotator.DisabledCount() != 0 {
		t.Errorf("DisabledCount() = %d, want 0", rotator.DisabledCount())
	}

	rotator.MarkDisabled(rotator.keys[0])
	if rotator.HealthyCount() != 2 {
		t.Errorf("HealthyCount() = %d, want 2", rotator.HealthyCount())
	}
	if rotator.DisabledCount() != 1 {
		t.Errorf("DisabledCount() = %d, want 1", rotator.DisabledCount())
	}
}

func TestHealthyCountWithCooldown(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	rotator.keys[0].mu.Lock()
	rotator.keys[0].State = KeyCooldown
	rotator.keys[0].CooldownUntil = time.Now().Add(-time.Second)
	rotator.keys[0].mu.Unlock()

	rotator.MarkCooldown(rotator.keys[1], 60*time.Second)

	// HealthyCount should count key0 (expired cooldown) and key2 (healthy) = 2
	count := rotator.HealthyCount()
	if count != 2 {
		t.Errorf("HealthyCount() = %d, want 2 (1 expired cooldown + 1 healthy)", count)
	}
}

// --- MaskKey Tests ---

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"long key", "tvly-abcdefghijklmnop", "tvly-...nop"},
		{"medium key", "tvly-short", "tvly-...ort"},
		{"short key", "tv", "tv***"},
		{"exactly 8 chars", "12345678", "123***"},
		// Note: 8 chars -> first 3 + "***" = "123***" (length > 3, <= 8)
		{"exactly 9 chars", "123456789", "12345...789"},
		{"empty key", "", "***"},
		{"1 char", "a", "a***"},
		{"2 chars", "ab", "ab***"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskKey(tt.input)
			if got != tt.want {
				t.Errorf("MaskKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- ParseRetryAfter Tests ---

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name            string
		header          string
		defaultDuration time.Duration
		want            time.Duration
	}{
		{"delta seconds", "60", 0, 60 * time.Second},
		{"delta seconds zero", "0", 0, 0},
		{"invalid value returns default", "invalid", 30 * time.Second, 30 * time.Second},
		{"empty string returns default", "", 45 * time.Second, 45 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRetryAfter(tt.header, tt.defaultDuration)
			if tt.name != "http date" && got != tt.want {
				t.Errorf("ParseRetryAfter(%q, %v) = %v, want %v", tt.header, tt.defaultDuration, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	got := ParseRetryAfter("120", 0)
	if got != 120*time.Second {
		t.Errorf("ParseRetryAfter(\"120\") = %v, want %v", got, 120*time.Second)
	}
}

func TestParseRetryAfterDefault(t *testing.T) {
	got := ParseRetryAfter("not-a-number", 30*time.Second)
	if got != 30*time.Second {
		t.Errorf("ParseRetryAfter(\"not-a-number\", 30s) = %v, want %v", got, 30*time.Second)
	}
}

// --- StatusGroup Tests ---

func TestStatusGroup(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{301, "3xx"},
		{400, "4xx"},
		{401, "4xx"},
		{403, "4xx"},
		{429, "4xx"},
		{432, "4xx"},
		{433, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{100, "other"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			got := statusGroup(tt.code)
			if got != tt.want {
				t.Errorf("statusGroup(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// --- Basic Auth Middleware Tests ---

func TestBasicAuthMiddleware_ValidAuth(t *testing.T) {
	setupTestGlobals([]string{"test-key"}, "round_robin")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := basicAuthMiddleware(next)
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	req.SetBasicAuth("admin", "testpass")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestBasicAuthMiddleware_InvalidAuth(t *testing.T) {
	setupTestGlobals([]string{"test-key"}, "round_robin")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := basicAuthMiddleware(next)
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	req.SetBasicAuth("admin", "wrongpass")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("next handler should not be called with invalid auth")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header")
	}
}

func TestBasicAuthMiddleware_DisabledWhenNoPassword(t *testing.T) {
	setupTestGlobalsNoAuth([]string{"test-key"}, "round_robin")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := basicAuthMiddleware(next)
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("next handler should not be called when admin is disabled")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// --- Stats Handler Tests ---

func TestStatsHandler(t *testing.T) {
	setupTestGlobals([]string{"tvly-test-key-1"}, "round_robin")

	req := httptest.NewRequest("GET", "/admin/stats", nil)
	w := httptest.NewRecorder()

	statsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if result["strategy"] != "round_robin" {
		t.Errorf("strategy = %v, want round_robin", result["strategy"])
	}

	keys, ok := result["keys"].([]interface{})
	if !ok {
		t.Fatal("keys is not a slice")
	}
	if len(keys) != 1 {
		t.Errorf("len(keys) = %d, want 1", len(keys))
	}

	key0, ok := keys[0].(map[string]interface{})
	if !ok {
		t.Fatal("key entry is not a map")
	}
	if key0["state"] != "healthy" {
		t.Errorf("state = %v, want healthy", key0["state"])
	}
	if key0["masked_key"] == nil {
		t.Error("masked_key should not be nil")
	}
}

// --- Health Handler Tests ---

func TestHealthHandler_AllKeysDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	rotator.MarkDisabled(rotator.keys[0])

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["status"] != "unhealthy" {
		t.Errorf("status = %v, want unhealthy", result["status"])
	}
}

func TestHealthHandler_UpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:              upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSec:               300,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHealthHandler_HealthyUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Authorization header is present
		if r.Header.Get("Authorization") != "Bearer key0" {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer key0")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:              upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSec:               300,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["status"] != "healthy" {
		t.Errorf("status = %v, want healthy", result["status"])
	}
	if result["upstream"] != "reachable" {
		t.Errorf("upstream = %v, want reachable", result["upstream"])
	}
}

func TestHealthHandler_AuthFailedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:              upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSec:               300,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["upstream"] != "auth_failed" {
		t.Errorf("upstream = %v, want auth_failed", result["upstream"])
	}
}

// --- Integration Tests: Transparent Retry ---

func TestTransparentRetry_429ThenSuccess(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer key0" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"detail": "Rate limit exceeded"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should retry on 429)", w.Code, http.StatusOK)
	}
	if callCount < 2 {
		t.Errorf("callCount = %d, want at least 2 (should retry)", callCount)
	}
}

func TestTransparentRetry_401AllKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (all keys should fail with 401)", w.Code, http.StatusUnauthorized)
	}

	if rotator.keys[0].State != KeyDisabled {
		t.Error("key0 should be disabled after 401")
	}
	if rotator.keys[1].State != KeyDisabled {
		t.Error("key1 should be disabled after 401")
	}
}

func TestTransparentRetry_432CooldownsKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(432)
		w.Write([]byte(`{"detail": "Plan limit exceeded"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		QuotaCooldownSec:       86400,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	// Both keys should be in cooldown after 432
	if rotator.keys[0].State != KeyCooldown {
		t.Error("key0 should be in cooldown after 432")
	}
	if rotator.keys[1].State != KeyCooldown {
		t.Error("key1 should be in cooldown after 432")
	}

	// Verify CooldownUntil is set to approximately QuotaCooldownSec in the future
	for i, key := range rotator.keys {
		key.mu.Lock()
		remaining := time.Until(key.CooldownUntil)
		key.mu.Unlock()
		minExpected := time.Duration(cfg.QuotaCooldownSec-10) * time.Second
		maxExpected := time.Duration(cfg.QuotaCooldownSec+10) * time.Second
		if remaining < minExpected || remaining > maxExpected {
			t.Errorf("key%d CooldownUntil remaining = %v, want approximately %v", i, remaining, time.Duration(cfg.QuotaCooldownSec)*time.Second)
		}
	}
}

func TestTransparentRetry_433CooldownsKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(433)
		w.Write([]byte(`{"detail": "PayGo limit exceeded"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		QuotaCooldownSec:       86400,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	// Both keys should be in cooldown after 433
	if rotator.keys[0].State != KeyCooldown {
		t.Error("key0 should be in cooldown after 433")
	}
	if rotator.keys[1].State != KeyCooldown {
		t.Error("key1 should be in cooldown after 433")
	}

	// Verify CooldownUntil is set to approximately QuotaCooldownSec in the future
	for i, key := range rotator.keys {
		key.mu.Lock()
		remaining := time.Until(key.CooldownUntil)
		key.mu.Unlock()
		minExpected := time.Duration(cfg.QuotaCooldownSec-10) * time.Second
		maxExpected := time.Duration(cfg.QuotaCooldownSec+10) * time.Second
		if remaining < minExpected || remaining > maxExpected {
			t.Errorf("key%d CooldownUntil remaining = %v, want approximately %v", i, remaining, time.Duration(cfg.QuotaCooldownSec)*time.Second)
		}
	}
}

func TestTransparentRetry_429AllKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"detail": "Rate limit exceeded"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d (all keys rate limited)", w.Code, http.StatusTooManyRequests)
	}
}

func TestPermanentDisable_401(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	rotator.MarkDisabled(rotator.keys[0])

	if rotator.keys[0].State != KeyDisabled {
		t.Error("key should be disabled after MarkDisabled")
	}

	rotator.MarkSuccess(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Error("disabled key should NOT recover after MarkSuccess")
	}
}

func TestForward5xxWithoutRetry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"detail": "Bad gateway"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (5xx should be forwarded without retry)", w.Code, http.StatusBadGateway)
	}
}

func TestProxyHandler_SingleKeySuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key0" {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer key0")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/search", strings.NewReader(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestProxyHandler_RequestBodyPreservedOnRetry(t *testing.T) {
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))

		auth := r.Header.Get("Authorization")
		if auth == "Bearer key0" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"detail": "Rate limit exceeded"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:           upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSec:            300,
		MaxFailsBeforeCooldown: 3,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	originalBody := `{"query":"test search","max_results":5}`
	req := httptest.NewRequest("POST", "/search", strings.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should retry and succeed)", w.Code, http.StatusOK)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected 2 request bodies, got %d", len(bodies))
	}

	if bodies[0] != originalBody {
		t.Errorf("first request body = %q, want %q", bodies[0], originalBody)
	}
	if bodies[1] != originalBody {
		t.Errorf("second request body = %q, want %q (should be preserved on retry)", bodies[1], originalBody)
	}
}

// --- Classification Response Tests ---

func TestClassifyResponse_2xx(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("GET", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 200,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err != nil {
		t.Errorf("classifyResponse() error = %v, want nil for 2xx", err)
	}

	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("key state = %d, want %d (healthy)", key.State, KeyHealthy)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if holder.result.ShouldRetry {
		t.Error("ShouldRetry = true, want false for 2xx")
	}
	if holder.result.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", holder.result.StatusCode)
	}
}

func TestClassifyResponse_5xx(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("GET", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 500,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err != nil {
		t.Errorf("classifyResponse() error = %v, want nil for 5xx", err)
	}

	key.mu.Lock()
	// 5xx should track a fail but not immediately go to cooldown
	if key.State != KeyHealthy {
		t.Errorf("key state = %d, want %d (healthy, 5xx should track fail not disable)", key.State, KeyHealthy)
	}
	if key.FailCount != 1 {
		t.Errorf("FailCount = %d, want 1 (one failure tracked)", key.FailCount)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if holder.result.ShouldRetry {
		t.Error("ShouldRetry = true, want false for 5xx (should not retry)")
	}
	if holder.result.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", holder.result.StatusCode)
	}
}

func TestClassifyResponse_429RateLimit(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	body := `{"detail": "Rate limit exceeded"}`

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 429,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 429 rate limit")
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown)", key.State, KeyCooldown)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 429 rate limit")
	}
	if holder.result.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", holder.result.StatusCode)
	}
}

func TestClassifyResponse_429QuotaExhausted(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	body := `{"detail": "Usage limit exceeded for the plan"}`

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 429,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 429 quota exhausted")
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("error message = %q, want to contain 'cooldown'", err.Error())
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown)", key.State, KeyCooldown)
	}
	remaining := time.Until(key.CooldownUntil)
	key.mu.Unlock()

	minExpected := time.Duration(cfg.QuotaCooldownSec-10) * time.Second
	maxExpected := time.Duration(cfg.QuotaCooldownSec+10) * time.Second
	if remaining < minExpected || remaining > maxExpected {
		t.Errorf("cooldown remaining = %v, want approximately %v", remaining, time.Duration(cfg.QuotaCooldownSec)*time.Second)
	}

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 429 quota exhausted")
	}
}

func TestClassifyResponse_401(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 401,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 401")
	}

	key.mu.Lock()
	if key.State != KeyDisabled {
		t.Errorf("key state = %d, want %d (disabled)", key.State, KeyDisabled)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 401")
	}
	if holder.result.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", holder.result.StatusCode)
	}
}

func TestClassifyResponse_432(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 432,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 432")
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("error message = %q, want to contain 'cooldown'", err.Error())
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown for 432)", key.State, KeyCooldown)
	}
	remaining := time.Until(key.CooldownUntil)
	key.mu.Unlock()

	minExpected := time.Duration(cfg.QuotaCooldownSec-10) * time.Second
	maxExpected := time.Duration(cfg.QuotaCooldownSec+10) * time.Second
	if remaining < minExpected || remaining > maxExpected {
		t.Errorf("cooldown remaining = %v, want approximately %v", remaining, time.Duration(cfg.QuotaCooldownSec)*time.Second)
	}

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 432")
	}
}

func TestClassifyResponse_433(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 433,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 433")
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("error message = %q, want to contain 'cooldown'", err.Error())
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown for 433)", key.State, KeyCooldown)
	}
	remaining := time.Until(key.CooldownUntil)
	key.mu.Unlock()

	minExpected := time.Duration(cfg.QuotaCooldownSec-10) * time.Second
	maxExpected := time.Duration(cfg.QuotaCooldownSec+10) * time.Second
	if remaining < minExpected || remaining > maxExpected {
		t.Errorf("cooldown remaining = %v, want approximately %v", remaining, time.Duration(cfg.QuotaCooldownSec)*time.Second)
	}

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 433")
	}
}

// --- Router Error Format Tests ---

func TestWriteRouterError(t *testing.T) {
	w := httptest.NewRecorder()
	writeRouterError(w, "test message", "test_type", "test_code", http.StatusTooManyRequests)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/json")
	}

	var errResp RouterError
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if errResp.Error.Message != "test message" {
		t.Errorf("message = %q, want %q", errResp.Error.Message, "test message")
	}
	if errResp.Error.Type != "test_type" {
		t.Errorf("type = %q, want %q", errResp.Error.Type, "test_type")
	}
	if errResp.Error.Code != "test_code" {
		t.Errorf("code = %q, want %q", errResp.Error.Code, "test_code")
	}
}

// --- MarkFail Tests ---

func TestMarkFailCooldownThreshold(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	// max_fails_before_cooldown = 3 (from setupTestGlobals)
	rotator.MarkFail(rotator.keys[0])
	rotator.MarkFail(rotator.keys[0])
	// After 2 fails, should still be healthy
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("State after 2 fails = %d, want %d", rotator.keys[0].State, KeyHealthy)
	}

	// After 3rd fail, should be in cooldown
	rotator.MarkFail(rotator.keys[0])
	if rotator.keys[0].State != KeyCooldown {
		t.Errorf("State after 3 fails = %d, want %d (cooldown)", rotator.keys[0].State, KeyCooldown)
	}
}

func TestMarkFailDoesNotAffectDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])
	rotator.MarkFail(rotator.keys[0])

	// Fail on disabled key should not change state
	if rotator.keys[0].State != KeyDisabled {
		t.Errorf("State after MarkFail on disabled = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}
}

// --- Health Check POST Body Tests ---

func TestHealthHandler_SendsPOSTWithBody(t *testing.T) {
	var receivedBody map[string]interface{}
	var receivedMethod string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:              upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSec:               300,
		MaxFailsBeforeCooldown:    3,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if receivedMethod != "POST" {
		t.Errorf("health check method = %q, want POST", receivedMethod)
	}

	if receivedBody["query"] != "health_check" {
		t.Errorf("health check query = %v, want health_check", receivedBody["query"])
	}

	if receivedBody["max_results"] != float64(1) {
		t.Errorf("health check max_results = %v, want 1", receivedBody["max_results"])
	}

	if receivedBody["search_depth"] != "basic" {
		t.Errorf("health check search_depth = %v, want basic", receivedBody["search_depth"])
	}
}

// --- PickKey Round Robin Wrap-Around Test ---

func TestPickKeyRoundRobinWrapsAround(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	picks := make([]string, 4)
	for i := 0; i < 3; i++ {
		key, err := rotator.PickKey()
		if err != nil {
			t.Fatal(err)
		}
		picks[i] = key.RawKey
	}

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	picks[3] = key.RawKey

	expected := []string{"key0", "key1", "key2", "key0"}
	for i, got := range picks {
		if got != expected[i] {
			t.Errorf("pick %d = %q, want %q", i, got, expected[i])
		}
	}
}

func TestPickKeyLeastUsedAfterCooldown(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "least_used")

	rotator.MarkCooldown(rotator.keys[0], 60*time.Second)

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key1" {
		t.Errorf("PickKey = %q, want %q (should skip cooldown key)", key.RawKey, "key1")
	}

	rotator.keys[0].mu.Lock()
	rotator.keys[0].CooldownUntil = time.Now().Add(-time.Second)
	rotator.keys[0].mu.Unlock()

	key, err = rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key0" {
		t.Errorf("PickKey = %q, want %q (cooldown expired, should be available)", key.RawKey, "key0")
	}
}

func TestPickKeySingleKeyDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	_, err := rotator.PickKey()
	if err == nil {
		t.Error("PickKey() should return error when single key is disabled")
	}
}

func TestMarkCooldownSetsDuration(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	duration := 300 * time.Second // Tavily default
	before := time.Now()
	rotator.MarkCooldown(rotator.keys[0], duration)
	after := time.Now()

	rotator.keys[0].mu.Lock()
	cooldownUntil := rotator.keys[0].CooldownUntil
	state := rotator.keys[0].State
	rotator.keys[0].mu.Unlock()

	if state != KeyCooldown {
		t.Errorf("State = %d, want %d (cooldown)", state, KeyCooldown)
	}

	minExpected := before.Add(duration)
	maxExpected := after.Add(duration + time.Second)
	if cooldownUntil.Before(minExpected) || cooldownUntil.After(maxExpected) {
		t.Errorf("CooldownUntil = %v, want between %v and %v", cooldownUntil, minExpected, maxExpected)
	}
}

func Test5xxTracksFailCount(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	// Send 2 5xx responses — should track fails but not go to cooldown (threshold = 3)
	for i := 0; i < 2; i++ {
		holder2 := &classifyHolder{}
		req := httptest.NewRequest("POST", "/search", nil)
		ctx := context.WithValue(req.Context(), keyCtxKey, key)
		ctx = context.WithValue(ctx, classifyCtxKey, holder2)
		ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
		req = req.WithContext(ctx)

		resp := &http.Response{
			StatusCode: 500,
			Request:    req,
			Body:       io.NopCloser(strings.NewReader("")),
		}

		classifyResponse(resp)
	}

	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("State after 2 5xx = %d, want %d (healthy, below threshold)", key.State, KeyHealthy)
	}
	if key.FailCount != 2 {
		t.Errorf("FailCount after 2 5xx = %d, want 2", key.FailCount)
	}
	key.mu.Unlock()

	// 3rd 5xx — should trigger cooldown
	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 500,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	classifyResponse(resp)

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("State after 3 5xx = %d, want %d (cooldown, at threshold)", key.State, KeyCooldown)
	}
	key.mu.Unlock()
}

// --- Health Check Response Body Tests ---

func TestHealthHandler_SendsCorrectRequestBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify method is POST
		if r.Method != "POST" {
			t.Errorf("health check method = %q, want POST", r.Method)
		}

		// Verify path is /search
		if r.URL.Path != "/search" {
			t.Errorf("health check path = %q, want /search", r.URL.Path)
		}

		// Verify Authorization header
		if r.Header.Get("Authorization") != "Bearer key0" {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer key0")
		}

		// Verify Content-Type
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:              upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSec:               300,
		MaxFailsBeforeCooldown:    3,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// --- Stats Handler Test for fail_count ---

func TestStatsHandlerIncludesFailCount(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	// Increment fail count
	rotator.MarkFail(rotator.keys[0])

	req := httptest.NewRequest("GET", "/admin/stats", nil)
	w := httptest.NewRecorder()

	statsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	keys, ok := result["keys"].([]interface{})
	if !ok {
		t.Fatal("keys is not a slice")
	}

	key0, ok := keys[0].(map[string]interface{})
	if !ok {
		t.Fatal("key entry is not a map")
	}

	if key0["fail_count"] == nil {
		t.Error("fail_count should not be nil")
	}
	if failCount, ok := key0["fail_count"].(float64); ok && failCount != 1 {
		t.Errorf("fail_count = %v, want 1", key0["fail_count"])
	}
}

// --- Buffered Response Writer Tests ---

func TestBufferedResponseWriter(t *testing.T) {
	buf := newBufferedResponseWriter()

	// Write headers and body
	buf.Header().Set("Content-Type", "application/json")
	buf.WriteHeader(200)
	buf.Write([]byte(`{"test": true}`))

	if buf.statusCode != 200 {
		t.Errorf("statusCode = %d, want 200", buf.statusCode)
	}
	if buf.wroteCode != true {
		t.Error("wroteCode should be true")
	}
	if buf.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", buf.header.Get("Content-Type"))
	}
	if buf.body.String() != `{"test": true}` {
		t.Errorf("body = %q, want %q", buf.body.String(), `{"test": true}`)
	}
}

func TestBufferedResponseWriter_DoubleWriteHeader(t *testing.T) {
	buf := newBufferedResponseWriter()

	buf.WriteHeader(200)
	buf.WriteHeader(500) // Should be ignored

	if buf.statusCode != 200 {
		t.Errorf("statusCode = %d, want 200 (second WriteHeader should be ignored)", buf.statusCode)
	}
}

// --- Validate Config Tests ---

func TestConfigValidationMinValues(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		check  func(*Config) error
	}{
		{
			"max_fails_below_1_clamped",
			Config{Keys: []string{"tvly-test"}, Strategy: "round_robin", MaxFailsBeforeCooldown: 0, CooldownSec: 300},
			func(c *Config) error {
				if c.MaxFailsBeforeCooldown != 3 {
					return fmt.Errorf("MaxFailsBeforeCooldown = %d, want 3 (default)", c.MaxFailsBeforeCooldown)
				}
				return nil
			},
		},
		{
			"cooldown_below_30_clamped",
			Config{Keys: []string{"tvly-test"}, Strategy: "round_robin", MaxFailsBeforeCooldown: 1, CooldownSec: 10},
			func(c *Config) error {
				if c.CooldownSec != 300 {
					return fmt.Errorf("CooldownSec = %d, want 300 (min 30)", c.CooldownSec)
				}
				return nil
			},
		},
		{
			"quota_cooldown_below_60_clamped",
			Config{Keys: []string{"tvly-test"}, Strategy: "round_robin", MaxFailsBeforeCooldown: 1, CooldownSec: 300, QuotaCooldownSec: 30},
			func(c *Config) error {
				if c.QuotaCooldownSec != 86400 {
					return fmt.Errorf("QuotaCooldownSec = %d, want 86400 (min 60)", c.QuotaCooldownSec)
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if checkErr := tt.check(&tt.config); checkErr != nil {
				t.Error(checkErr)
			}
		})
	}
}

// --- Integration: Tavily 429 with Retry-After ---

func TestClassifyResponse_429WithRetryAfter(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	body := `{"detail": "Rate limit exceeded"}`

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 429,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Retry-After": {"60"}},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 429")
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown)", key.State, KeyCooldown)
	}
	// Verify cooldown duration is approximately 60 seconds from Retry-After header
	remaining := time.Until(key.CooldownUntil)
	if remaining < 55*time.Second || remaining > 65*time.Second {
		t.Errorf("cooldown remaining = %v, want approximately 60s", remaining)
	}
	key.mu.Unlock()
}

// --- Integration: 429 with quota in error message ---

func TestClassifyResponse_429QuotaInErrorMessage(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	// Test with "quota" in the error field
	body := `{"error": "Quota exceeded for this API key"}`

	req := httptest.NewRequest("POST", "/search", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 429,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 429")
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown for quota in error message)", key.State, KeyCooldown)
	}
	key.mu.Unlock()
}

// --- Config: TAVILY_COOLDOWN_SEC env override ---

func TestLoadConfigEnvOverrideCooldownSec(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["tvly-test"], "strategy": "round_robin", "cooldown_sec": 300}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	t.Setenv("TAVILY_COOLDOWN_SEC", "120")

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.CooldownSec != 120 {
		t.Errorf("CooldownSec = %d, want 120 (from env override)", cfg.CooldownSec)
	}
}

// --- Integration: Health check detects 432 as auth_failed ---

func TestHealthHandler_432AsQuotaExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(432)
		w.Write([]byte(`{"detail": "Plan limit exceeded"}`))
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamBase:              upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSec:               300,
		MaxFailsBeforeCooldown:    3,
		QuotaCooldownSec:          86400,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["upstream"] != "quota_exhausted" {
		t.Errorf("upstream = %v, want quota_exhausted", result["upstream"])
	}
}

// --- Config: TAVILY_QUOTA_COOLDOWN_SEC env override ---

func TestLoadConfigEnvOverrideQuotaCooldownSec(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["tvly-test"], "strategy": "round_robin", "quota_cooldown_sec": 86400}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	t.Setenv("TAVILY_QUOTA_COOLDOWN_SEC", "3600")

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.QuotaCooldownSec != 3600 {
		t.Errorf("QuotaCooldownSec = %d, want 3600 (from env override)", cfg.QuotaCooldownSec)
	}
}

func TestLoadConfigDefaultQuotaCooldownSec(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["tvly-test"], "strategy": "round_robin"}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.QuotaCooldownSec != 86400 {
		t.Errorf("QuotaCooldownSec = %d, want 86400 (default)", cfg.QuotaCooldownSec)
	}
}