package a2aclient

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRetryPolicyBackoffSchedule(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 400 * time.Millisecond}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond, // capped
	}
	for i, expected := range want {
		if got := p.backoff(i+1, 0); got != expected {
			t.Fatalf("backoff(%d) = %s, want %s", i+1, got, expected)
		}
	}
	// A server's Retry-After always wins, cap included: it is an instruction,
	// not a hint.
	if got := p.backoff(1, 9*time.Second); got != 9*time.Second {
		t.Fatalf("backoff with Retry-After = %s, want 9s", got)
	}
}

func TestRetryPolicyAttempts(t *testing.T) {
	if got := NoRetry().attempts(); got != 1 {
		t.Fatalf("NoRetry attempts = %d, want 1", got)
	}
	if got := (RetryPolicy{MaxAttempts: 0}).attempts(); got != 1 {
		t.Fatalf("zero attempts = %d, want 1", got)
	}
	if got := DefaultRetryPolicy().attempts(); got != 3 {
		t.Fatalf("default attempts = %d, want 3", got)
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := []struct {
		status              int
		idempotent, retried bool
	}{
		{http.StatusTooManyRequests, false, true},
		{http.StatusServiceUnavailable, false, true},
		{http.StatusBadGateway, false, false},
		{http.StatusBadGateway, true, true},
		{http.StatusGatewayTimeout, false, false},
		{http.StatusGatewayTimeout, true, true},
		{http.StatusInternalServerError, true, false},
		{http.StatusUnauthorized, true, false},
		{http.StatusNotFound, true, false},
	}
	for _, tc := range cases {
		if got := retryableStatus(tc.status, tc.idempotent); got != tc.retried {
			t.Fatalf("retryableStatus(%d, idempotent=%v) = %v, want %v",
				tc.status, tc.idempotent, got, tc.retried)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	header := http.Header{}
	if got := parseRetryAfter(header, now); got != 0 {
		t.Fatalf("absent Retry-After = %s, want 0", got)
	}

	header.Set("Retry-After", "3")
	if got := parseRetryAfter(header, now); got != 3*time.Second {
		t.Fatalf("delta-seconds = %s, want 3s", got)
	}

	header.Set("Retry-After", "-3")
	if got := parseRetryAfter(header, now); got != 0 {
		t.Fatalf("negative delta = %s, want 0", got)
	}

	header.Set("Retry-After", now.Add(5*time.Second).Format(http.TimeFormat))
	if got := parseRetryAfter(header, now); got != 5*time.Second {
		t.Fatalf("http-date = %s, want 5s", got)
	}

	header.Set("Retry-After", now.Add(-time.Hour).Format(http.TimeFormat))
	if got := parseRetryAfter(header, now); got != 0 {
		t.Fatalf("past http-date = %s, want 0", got)
	}

	header.Set("Retry-After", "soon-ish")
	if got := parseRetryAfter(header, now); got != 0 {
		t.Fatalf("unparseable = %s, want 0", got)
	}
}

func TestSleepRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); err == nil {
		t.Fatal("sleep on a cancelled context should return its error immediately")
	}
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleep: %v", err)
	}
}
