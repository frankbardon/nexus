package engine

import "log/slog"

// ContextManager manages agent context. This is a minimal v1 implementation
// that will be expanded as context windowing and summarization are added.
type ContextManager struct {
	bus    EventBus
	logger *slog.Logger
}

// NewContextManager creates a new ContextManager.
func NewContextManager(bus EventBus, logger *slog.Logger) *ContextManager {
	return &ContextManager{
		bus:    bus,
		logger: logger,
	}
}
