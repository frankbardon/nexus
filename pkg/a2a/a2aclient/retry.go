package a2aclient

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// RetryPolicy governs re-attempts of a failed HTTP call.
//
// # What is retried, and why the answer depends on the operation
//
// A2A operations are not uniformly safe to repeat. GetTask and ListTasks are
// reads: repeating one costs a round trip and nothing else. SendMessage and
// CancelTask are not reads — a SendMessage that reached the agent and then lost
// its response would, on a blind retry, run the agent's work twice and put two
// messages in the task history. A2A defines no idempotency key, so the client
// cannot make that safe; it can only decline to make it unsafe.
//
// The policy therefore splits by what the failure proves:
//
//   - A TRANSPORT failure (connection refused, reset, TLS failure) proves
//     nothing about whether the server processed the request. It is retried for
//     reads only.
//   - HTTP 429 and 503 are the two statuses that say, by definition, that the
//     server did NOT process the request — it is rate-limiting or unavailable.
//     Those are retried for every operation, honoring Retry-After.
//   - HTTP 502 and 504 mean an intermediary gave up, which does not prove the
//     origin did not process the request. Those are retried for reads only.
//   - Every other status is an answer, not a failure of delivery, and is never
//     retried. A 500 from the agent is the agent's answer.
//
// Backoff is exponential from BaseDelay, doubling, capped at MaxDelay, with no
// jitter: a single client's retries do not need decorrelating, and a
// deterministic schedule is one a test can assert. A Retry-After header always
// wins over the computed delay.
//
// The retry loop covers establishing a stream (the request and its response
// headers) but never a stream already in progress: replaying a partially
// consumed stream would duplicate frames a caller has already acted on.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first. Values
	// below 2 disable retrying.
	MaxAttempts int
	// BaseDelay is the delay before the second attempt.
	BaseDelay time.Duration
	// MaxDelay caps the computed backoff. A Retry-After longer than this is
	// still honored in full — the server's instruction is not a suggestion.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is the policy a client uses when none is configured.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second}
}

// NoRetry is a policy that attempts each call exactly once.
func NoRetry() RetryPolicy { return RetryPolicy{MaxAttempts: 1} }

// attempts returns the effective attempt count, never below one.
func (p RetryPolicy) attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// backoff returns the delay before the given attempt number, where attempt 1 is
// the first retry. A non-zero retryAfter from the server overrides it.
func (p RetryPolicy) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	base := p.BaseDelay
	if base <= 0 {
		base = DefaultRetryPolicy().BaseDelay
	}
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if p.MaxDelay > 0 && delay >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		return p.MaxDelay
	}
	return delay
}

// retryableStatus reports whether an HTTP status justifies another attempt.
// idempotent widens the set to the statuses that only PROBABLY mean the request
// was not processed.
func retryableStatus(status int, idempotent bool) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return idempotent
	default:
		return false
	}
}

// parseRetryAfter reads a Retry-After header in either of its two forms:
// delta-seconds, or an HTTP-date. It returns zero when absent or unparseable,
// and clamps a date in the past to zero.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	raw := h.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// sleep waits for d, or returns the context's error if it is cancelled first.
// It allocates a timer per call and always stops it, so a cancelled wait leaves
// nothing behind.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
