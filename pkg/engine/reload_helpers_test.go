package engine

import (
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/configwatch"
)

// newConfigwatchWatcher wraps configwatch.New for the reload tests. Kept
// in a *_test.go file so the production engine package never imports
// configwatch (the CLI in cmd/nexus is the only production consumer).
func newConfigwatchWatcher(t *testing.T, path string, debounce time.Duration, logger *slog.Logger, onChange func()) (*configwatch.Watcher, error) {
	t.Helper()
	return configwatch.New(path, debounce, logger, onChange)
}
