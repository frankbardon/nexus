package gemini

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryConfig_Defaults(t *testing.T) {
	rc := parseRetryConfig(map[string]any{})
	if rc.Enabled {
		t.Fatal("expected retry disabled by default")
	}
	if rc.MaxRetries != 3 {
		t.Fatalf("expected 3 max retries, got %d", rc.MaxRetries)
	}
	if rc.Backoff != BackoffJitter {
		t.Fatalf("expected exponential_jitter, got %s", rc.Backoff)
	}
}

func TestParseRetryConfig_Full(t *testing.T) {
	rc := parseRetryConfig(map[string]any{
		"retry": map[string]any{
			"max_retries":   5,
			"initial_delay": "500ms",
			"max_delay":     "30s",
			"backoff":       "linear",
			"multiplier":    1.5,
			"statuses":      []any{429, 503},
		},
	})
	if !rc.Enabled {
		t.Fatal("expected enabled")
	}
	if rc.MaxRetries != 5 || rc.Backoff != BackoffLinear || rc.Multiplier != 1.5 {
		t.Fatalf("unexpected: %+v", rc)
	}
	if rc.InitialDelay != 500*time.Millisecond || rc.MaxDelay != 30*time.Second {
		t.Fatalf("unexpected delays: %+v", rc)
	}
	if !rc.RetryableStatuses[429] || !rc.RetryableStatuses[503] || len(rc.RetryableStatuses) != 2 {
		t.Fatalf("unexpected statuses: %+v", rc.RetryableStatuses)
	}
}

func TestBackoffDelay_Constant(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Backoff: BackoffConstant}
	for i := 0; i < 5; i++ {
		if d := p.backoffDelay(i, rc); d != 100*time.Millisecond {
			t.Fatalf("attempt %d: %v", i, d)
		}
	}
}

func TestBackoffDelay_Exponential(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Backoff: BackoffExponential, Multiplier: 2.0}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	for i, exp := range want {
		if d := p.backoffDelay(i, rc); d != exp {
			t.Fatalf("attempt %d: got %v want %v", i, d, exp)
		}
	}
}

func TestBackoffDelay_MaxDelayCap(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 1 * time.Second, MaxDelay: 2 * time.Second, Backoff: BackoffExponential, Multiplier: 10.0}
	if d := p.backoffDelay(3, rc); d != 2*time.Second {
		t.Fatalf("expected 2s cap, got %v", d)
	}
}

func TestDoWithRetry_RetriesOnRetryableStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:           true,
			MaxRetries:        3,
			InitialDelay:      1 * time.Millisecond,
			MaxDelay:          10 * time.Millisecond,
			Backoff:           BackoffConstant,
			Multiplier:        1.0,
			RetryableStatuses: map[int]bool{503: true},
		},
	}

	resp, err := p.doWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("POST", srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoWithRetry_NonRetryableStatusPassesThrough(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:           true,
			MaxRetries:        3,
			InitialDelay:      1 * time.Millisecond,
			MaxDelay:          10 * time.Millisecond,
			Backoff:           BackoffConstant,
			Multiplier:        1.0,
			RetryableStatuses: map[int]bool{503: true},
		},
	}

	resp, err := p.doWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("POST", srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("expected 1 call with 400, got %d / %d", calls.Load(), resp.StatusCode)
	}
}
