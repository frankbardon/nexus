// Package retry provides shared retry-with-LLM logic for gates that need to
// re-ask the LLM when output fails validation.
package retry

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// Config configures retry behavior.
type Config struct {
	MaxRetries  int    // max retry attempts (0 = no retries, just veto)
	RetryPrompt string // template with {error}, {content}, {schema}, {length}, {limit} placeholders
	Source      string // plugin ID for _source tagging
	ModelRole   string // LLM model role (default "balanced")
}

// state tracks an in-flight retry for a single turn.
type state struct {
	retries   int
	lastError string
	doneCh    chan events.LLMResponse
}

// Handler manages retry loops for a gate. Thread-safe.
type Handler struct {
	bus    engine.EventBus
	logger *slog.Logger
	config Config

	mu      sync.Mutex
	pending map[string]*state // keyed by source tag (unique per retry attempt)
	unsub   func()
}

// New creates a retry handler. Call Shutdown when done.
func New(bus engine.EventBus, logger *slog.Logger, config Config) *Handler {
	if config.ModelRole == "" {
		config.ModelRole = "balanced"
	}
	h := &Handler{
		bus:     bus,
		logger:  logger,
		config:  config,
		pending: make(map[string]*state),
	}

	// Subscribe to LLM responses — filter by _source metadata in handler.
	h.unsub = bus.Subscribe("llm.response", h.handleLLMResponse,
		engine.WithPriority(5))

	return h
}

// Shutdown unsubscribes from the bus.
func (h *Handler) Shutdown() {
	if h.unsub != nil {
		h.unsub()
	}
}

// ValidateFunc checks content and returns an error description if invalid, or "" if valid.
type ValidateFunc func(content string) string

// RetryResult is the outcome of a retry attempt.
type RetryResult struct {
	Valid   bool
	Content string // the (possibly corrected) content
	Error   string // last validation error if not valid
}

// AttemptRetry validates content and retries via LLM if invalid.
// Blocks until valid content is produced or retries are exhausted.
// templateVars are additional key=value pairs for the retry prompt template.
func (h *Handler) AttemptRetry(
	originalContent string,
	originalMessages []events.Message,
	validate ValidateFunc,
	templateVars map[string]string,
) RetryResult {
	// First validation.
	validationErr := validate(originalContent)
	if validationErr == "" {
		return RetryResult{Valid: true, Content: originalContent}
	}

	if h.config.MaxRetries <= 0 {
		return RetryResult{Valid: false, Content: originalContent, Error: validationErr}
	}

	content := originalContent
	for attempt := range h.config.MaxRetries {
		h.logger.Info("retrying LLM request",
			"attempt", attempt+1,
			"max_retries", h.config.MaxRetries,
			"error", validationErr)

		// Build retry prompt from template.
		prompt := h.config.RetryPrompt
		prompt = strings.ReplaceAll(prompt, "{error}", validationErr)
		prompt = strings.ReplaceAll(prompt, "{content}", content)
		for k, v := range templateVars {
			prompt = strings.ReplaceAll(prompt, "{"+k+"}", v)
		}

		// Build messages: original context + failed response + correction instruction.
		messages := make([]events.Message, len(originalMessages))
		copy(messages, originalMessages)
		messages = append(messages,
			events.Message{Role: "assistant", Content: content},
			events.Message{Role: "user", Content: prompt},
		)

		// Create unique tag for this retry.
		tag := fmt.Sprintf("%s/retry-%d-%d", h.config.Source, attempt, time.Now().UnixNano())

		doneCh := make(chan events.LLMResponse, 1)
		h.mu.Lock()
		h.pending[tag] = &state{
			retries:   attempt,
			lastError: validationErr,
			doneCh:    doneCh,
		}
		h.mu.Unlock()

		// Emit LLM request tagged with our source.
		_ = h.bus.Emit("llm.request", events.LLMRequest{
			Role:     h.config.ModelRole,
			Messages: messages,
			Stream:   false,
			Metadata: map[string]any{
				"_source":   tag,
				"task_kind": "gate_retry",
			},
			Tags: map[string]string{"source_plugin": tag},
		})

		// Wait for response (blocking — bus is synchronous, response arrives on another handler).
		// Use a timeout to prevent deadlock.
		select {
		case resp := <-doneCh:
			content = resp.Content
		case <-time.After(60 * time.Second):
			h.logger.Error("retry timed out", "attempt", attempt+1)
			h.mu.Lock()
			delete(h.pending, tag)
			h.mu.Unlock()
			return RetryResult{Valid: false, Content: content, Error: "retry timed out"}
		}

		h.mu.Lock()
		delete(h.pending, tag)
		h.mu.Unlock()

		// Validate the new response.
		validationErr = validate(content)
		if validationErr == "" {
			return RetryResult{Valid: true, Content: content}
		}
	}

	return RetryResult{Valid: false, Content: content, Error: validationErr}
}

func (h *Handler) handleLLMResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}

	// Check if this response is for one of our retries.
	src, _ := resp.Metadata["_source"].(string)
	if src == "" {
		return
	}

	h.mu.Lock()
	s, ok := h.pending[src]
	h.mu.Unlock()

	if ok && s != nil {
		select {
		case s.doneCh <- resp:
		default:
		}
	}
}
