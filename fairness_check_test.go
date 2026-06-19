package main

import (
	"sync"
	"testing"
)

// Proves the first key in the slice does NOT get disproportionate traffic
// under least_used strategy with 4 keys (the user's exact scenario).
func TestFairness_FirstKeyDoesNotDominate(t *testing.T) {
	keys := []string{"tvly-dev-AAAA", "tvly-dev-BBBB", "tvly-dev-CCCC", "tvly-dev-DDDD"}
	setupTestGlobals(keys, "least_used")

	picks := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Simulate 1000 concurrent requests
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				key, err := rotator.PickKey()
				if err != nil {
					t.Errorf("PickKey() error: %v", err)
					return
				}
				mu.Lock()
				picks[key.RawKey]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, k := range keys {
		total += picks[k]
	}

	t.Logf("Distribution over %d requests:", total)
	for _, k := range keys {
		pct := float64(picks[k]) / float64(total) * 100
		t.Logf("  %s: %d (%.1f%%)", k[:12]+"...", picks[k], pct)
	}

	// With 4 keys, each should get ~25%. Allow 15%-35% range for concurrency variance.
	for _, k := range keys {
		pct := float64(picks[k]) / float64(total) * 100
		if pct < 15 {
			t.Errorf("key %q got %.1f%% — UNDER-UTILIZED (want >= 15%%)", k[:12], pct)
		}
		if pct > 35 {
			t.Errorf("key %q got %.1f%% — OVER-UTILIZED (want <= 35%%)", k[:12], pct)
		}
	}

	// Specifically: the first key must NOT dominate
	firstKeyPct := float64(picks[keys[0]]) / float64(total) * 100
	if firstKeyPct > 35 {
		t.Errorf("FIRST KEY got %.1f%% — this is the bug the user reported! Want <= 35%%", firstKeyPct)
	}
}

// Proves health checks also rotate (not always first key)
func TestFairness_HealthChecksRotate(t *testing.T) {
	keys := []string{"tvly-dev-AAAA", "tvly-dev-BBBB", "tvly-dev-CCCC", "tvly-dev-DDDD"}
	setupTestGlobals(keys, "least_used")

	picks := make(map[string]int)
	for i := 0; i < 40; i++ {
		key, err := rotator.PickKey()
		if err != nil {
			t.Fatalf("PickKey() error: %v", err)
		}
		picks[key.RawKey]++
	}

	t.Logf("Health check rotation over 40 calls:")
	for _, k := range keys {
		t.Logf("  %s: %d", k[:12]+"...", picks[k])
	}

	// Each key should get ~10 picks (40/4). The first key should NOT get all 40.
	if picks[keys[0]] == 40 {
		t.Error("FIRST KEY got ALL 40 health checks — health check rotation is broken!")
	}
	if picks[keys[0]] > 15 {
		t.Errorf("FIRST KEY got %d/40 health checks — should be ~10, want <= 15", picks[keys[0]])
	}
	if picks[keys[0]] < 5 {
		t.Errorf("FIRST KEY got %d/40 health checks — should be ~10, want >= 5", picks[keys[0]])
	}
}
