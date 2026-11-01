package downloader

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLivePackDialTiming(t *testing.T) {
	if os.Getenv("RUN_LIVE_SPEED_TEST") != "1" {
		t.Skip("set RUN_LIVE_SPEED_TEST=1 to compare live pack-only timing")
	}
	ensureLiveTokenJSON(t)

	target := os.Getenv("LIVE_PACK_SPEED_TARGET")
	if target == "" {
		target = "play.enchanted.gg"
	}
	trials := envInt("LIVE_PACK_SPEED_TRIALS", 3)
	trialTimeout := time.Duration(envInt("LIVE_PACK_SPEED_TIMEOUT_SECONDS", 20)) * time.Second

	result := timeFetchPacks(t, target, trials, trialTimeout)
	t.Logf("pack dial total=%s avg=%s timeouts=%d/%d", result.total, result.total/time.Duration(trials), result.timeouts, trials)
}

type timingResult struct {
	total    time.Duration
	timeouts int
}

func timeFetchPacks(t *testing.T, target string, trials int, trialTimeout time.Duration) timingResult {
	t.Helper()
	var result timingResult
	for i := 0; i < trials; i++ {
		elapsed, timedOut, err := fetchPacksTimed(target, trialTimeout)
		if err != nil {
			t.Fatalf("fetch packs trial=%d: %v", i+1, err)
		}
		if timedOut {
			result.timeouts++
		}
		t.Logf("trial=%d duration=%s timeout=%v", i+1, elapsed, timedOut)
		result.total += elapsed
	}
	return result
}

func fetchPacksTimed(target string, timeout time.Duration) (time.Duration, bool, error) {
	type result struct {
		elapsed time.Duration
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		start := time.Now()
		packs, err := fetchPacks(target, 19132, nil)
		if err == nil && len(packs) == 0 {
			err = fmt.Errorf("no packs")
		}
		ch <- result{elapsed: time.Since(start), err: err}
	}()

	select {
	case result := <-ch:
		return result.elapsed, false, result.err
	case <-time.After(timeout):
		return timeout, true, nil
	}
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func ensureLiveTokenJSON(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("token.json"); err == nil {
		return
	}
	for _, candidate := range []string{
		filepath.Join("..", "..", "..", "token.json"),
		filepath.Join("..", "..", "..", "..", "token.json"),
	} {
		body, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if err := os.WriteFile("token.json", body, 0600); err != nil {
			t.Fatalf("copy token.json for live speed test: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove("token.json") })
		return
	}
	t.Skip("token.json not found")
}
