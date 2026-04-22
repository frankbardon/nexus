package anthropic

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
		t.Fatalf("expected exponential_jitter backoff, got %s", rc.Backoff)
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
		t.Fatal("expected retry enabled")
	}
	if rc.MaxRetries != 5 {
		t.Fatalf("expected 5, got %d", rc.MaxRetries)
	}
	if rc.InitialDelay != 500*time.Millisecond {
		t.Fatalf("expected 500ms, got %v", rc.InitialDelay)
	}
	if rc.MaxDelay != 30*time.Second {
		t.Fatalf("expected 30s, got %v", rc.MaxDelay)
	}
	if rc.Backoff != BackoffLinear {
		t.Fatalf("expected linear, got %s", rc.Backoff)
	}
	if rc.Multiplier != 1.5 {
		t.Fatalf("expected 1.5, got %f", rc.Multiplier)
	}
	if len(rc.RetryableStatuses) != 2 || !rc.RetryableStatuses[429] || !rc.RetryableStatuses[503] {
		t.Fatalf("unexpected statuses: %v", rc.RetryableStatuses)
	}
}

func TestBackoffDelay_Constant(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Backoff: BackoffConstant}

	for i := 0; i < 5; i++ {
		d := p.backoffDelay(i, rc)
		if d != 100*time.Millisecond {
			t.Fatalf("attempt %d: expected 100ms, got %v", i, d)
		}
	}
}

func TestBackoffDelay_Linear(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Backoff: BackoffLinear, Multiplier: 2.0}

	expected := []time.Duration{
		100 * time.Millisecond, // 100 + 0*2*100
		300 * time.Millisecond, // 100 + 1*2*100
		500 * time.Millisecond, // 100 + 2*2*100
	}
	for i, exp := range expected {
		d := p.backoffDelay(i, rc)
		if d != exp {
			t.Fatalf("attempt %d: expected %v, got %v", i, exp, d)
		}
	}
}

func TestBackoffDelay_Exponential(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Backoff: BackoffExponential, Multiplier: 2.0}

	expected := []time.Duration{
		100 * time.Millisecond, // 100 * 2^0
		200 * time.Millisecond, // 100 * 2^1
		400 * time.Millisecond, // 100 * 2^2
	}
	for i, exp := range expected {
		d := p.backoffDelay(i, rc)
		if d != exp {
			t.Fatalf("attempt %d: expected %v, got %v", i, exp, d)
		}
	}
}

func TestBackoffDelay_MaxDelayCap(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 1 * time.Second, MaxDelay: 2 * time.Second, Backoff: BackoffExponential, Multiplier: 10.0}

	d := p.backoffDelay(3, rc)
	if d != 2*time.Second {
		t.Fatalf("expected max delay 2s, got %v", d)
	}
}

func TestBackoffDelay_Jitter(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	rc := retryConfig{InitialDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Backoff: BackoffJitter, Multiplier: 2.0}

	// Jitter adds [0, base) on top of base, so delay is in [base, 2*base).
	for i := 0; i < 20; i++ {
		d := p.backoffDelay(0, rc)
		if d < 100*time.Millisecond || d >= 200*time.Millisecond {
			t.Fatalf("jitter delay out of range: %v", d)
		}
	}
}

func TestDoWithRetry_NoRetryPassthrough(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry:  retryConfig{Enabled: false},
	}

	resp, err := p.doWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("POST", srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", calls.Load())
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
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:      true,
			MaxRetries:   3,
			InitialDelay: 1 * time.Millisecond,
			MaxDelay:     10 * time.Millisecond,
			Backoff:      BackoffConstant,
			Multiplier:   1.0,
			RetryableStatuses: map[int]bool{
				503: true,
			},
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
		t.Fatalf("expected 3 calls (2 retries + success), got %d", calls.Load())
	}
}

func TestDoWithRetry_ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:      true,
			MaxRetries:   2,
			InitialDelay: 1 * time.Millisecond,
			MaxDelay:     10 * time.Millisecond,
			Backoff:      BackoffConstant,
			Multiplier:   1.0,
			RetryableStatuses: map[int]bool{
				503: true,
			},
		},
	}

	_, err := p.doWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("POST", srv.URL, nil)
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	// 1 initial + 2 retries = 3 total
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoWithRetry_RespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:      true,
			MaxRetries:   10,
			InitialDelay: 1 * time.Second, // long delay so cancel triggers
			MaxDelay:     10 * time.Second,
			Backoff:      BackoffConstant,
			Multiplier:   1.0,
			RetryableStatuses: map[int]bool{
				503: true,
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately after first attempt delay starts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := p.doWithRetry(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "POST", srv.URL, nil)
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestDoWithRetry_NonRetryableStatusPassesThrough(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:      true,
			MaxRetries:   3,
			InitialDelay: 1 * time.Millisecond,
			MaxDelay:     10 * time.Millisecond,
			Backoff:      BackoffConstant,
			Multiplier:   1.0,
			RetryableStatuses: map[int]bool{
				503: true,
			},
		},
	}

	resp, err := p.doWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("POST", srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call (no retry for 400), got %d", calls.Load())
	}
}

func TestDoWithRetry_RespectsRetryAfterHeader(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &Plugin{
		logger: slog.Default(),
		client: srv.Client(),
		retry: retryConfig{
			Enabled:      true,
			MaxRetries:   3,
			InitialDelay: 1 * time.Millisecond,
			MaxDelay:     10 * time.Second,
			Backoff:      BackoffConstant,
			Multiplier:   1.0,
			RetryableStatuses: map[int]bool{
				429: true,
			},
		},
	}

	resp, err := p.doWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("POST", srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
}
