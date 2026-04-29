package gemini

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// BackoffStrategy defines how delays increase between retries.
type BackoffStrategy string

const (
	BackoffConstant    BackoffStrategy = "constant"
	BackoffLinear      BackoffStrategy = "linear"
	BackoffExponential BackoffStrategy = "exponential"
	BackoffJitter      BackoffStrategy = "exponential_jitter"
)

// retryConfig controls automatic retry behavior for API requests.
type retryConfig struct {
	Enabled           bool
	MaxRetries        int
	InitialDelay      time.Duration
	MaxDelay          time.Duration
	Backoff           BackoffStrategy
	Multiplier        float64
	RetryableStatuses map[int]bool
}

func defaultRetryConfig() retryConfig {
	return retryConfig{
		Enabled:      false,
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     60 * time.Second,
		Backoff:      BackoffJitter,
		Multiplier:   2.0,
		RetryableStatuses: map[int]bool{
			429: true,
			500: true,
			502: true,
			503: true,
			504: true,
		},
	}
}

// parseRetryConfig reads retry settings from the plugin config map.
func parseRetryConfig(cfg map[string]any) retryConfig {
	rc := defaultRetryConfig()

	retryCfg, ok := cfg["retry"].(map[string]any)
	if !ok {
		return rc
	}

	rc.Enabled = true

	if v, ok := retryCfg["max_retries"].(int); ok {
		rc.MaxRetries = v
	}
	if v, ok := retryCfg["initial_delay"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			rc.InitialDelay = d
		}
	}
	if v, ok := retryCfg["max_delay"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			rc.MaxDelay = d
		}
	}
	if v, ok := retryCfg["backoff"].(string); ok {
		switch BackoffStrategy(v) {
		case BackoffConstant, BackoffLinear, BackoffExponential, BackoffJitter:
			rc.Backoff = BackoffStrategy(v)
		}
	}
	if v, ok := retryCfg["multiplier"].(float64); ok && v > 0 {
		rc.Multiplier = v
	}
	if v, ok := retryCfg["statuses"].([]any); ok {
		rc.RetryableStatuses = make(map[int]bool, len(v))
		for _, s := range v {
			if code, ok := s.(int); ok {
				rc.RetryableStatuses[code] = true
			}
		}
	}

	return rc
}

// doWithRetry executes an HTTP request with retry logic. The caller must close
// the response body on success. The makeFn creates a fresh *http.Request for
// each attempt (request bodies are consumed on send).
func (p *Plugin) doWithRetry(ctx context.Context, makeFn func() (*http.Request, error)) (*http.Response, error) {
	rc := p.retry

	if !rc.Enabled {
		req, err := makeFn()
		if err != nil {
			return nil, err
		}
		return p.client.Do(req)
	}

	var lastErr error

	for attempt := 0; attempt <= rc.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := p.backoffDelay(attempt-1, rc)
			p.logger.Info("retrying API request",
				"attempt", attempt,
				"max_retries", rc.MaxRetries,
				"delay", delay,
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := makeFn()
		if err != nil {
			return nil, fmt.Errorf("gemini: failed to create HTTP request: %w", err)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}

		if !rc.RetryableStatuses[resp.StatusCode] {
			return resp, nil
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil {
					delay := time.Duration(secs) * time.Second
					if delay > rc.MaxDelay {
						delay = rc.MaxDelay
					}
					p.logger.Info("respecting Retry-After header",
						"delay", delay,
						"attempt", attempt,
					)
					resp.Body.Close()
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(delay):
					}
					continue
				}
			}
		}

		lastErr = fmt.Errorf("API returned status %d", resp.StatusCode)
		resp.Body.Close()
	}

	return nil, fmt.Errorf("gemini: max retries (%d) exceeded: %w", rc.MaxRetries, lastErr)
}

// backoffDelay computes the wait duration for a given retry attempt.
func (p *Plugin) backoffDelay(attempt int, rc retryConfig) time.Duration {
	var delay time.Duration

	switch rc.Backoff {
	case BackoffConstant:
		delay = rc.InitialDelay
	case BackoffLinear:
		delay = rc.InitialDelay + time.Duration(float64(attempt)*rc.Multiplier*float64(rc.InitialDelay))
	case BackoffExponential:
		delay = time.Duration(float64(rc.InitialDelay) * math.Pow(rc.Multiplier, float64(attempt)))
	case BackoffJitter:
		base := float64(rc.InitialDelay) * math.Pow(rc.Multiplier, float64(attempt))
		jitter := rand.Float64() * base
		delay = time.Duration(base + jitter)
	default:
		delay = rc.InitialDelay
	}

	if delay > rc.MaxDelay {
		delay = rc.MaxDelay
	}

	return delay
}
