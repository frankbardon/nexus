// Package configwatch is an internal helper that fires a callback whenever
// the on-disk YAML config changes. Used by cmd/nexus to drive
// engine.ReloadConfig from fsnotify edits when operators opt in via
// engine.config_watch.enabled.
//
// The watcher debounces bursts of fsnotify events on the same file because
// editors commonly fire two or three Write events when saving. Reading a
// half-written YAML through the validator would surface a confusing
// schema error, so we wait for the burst to settle before invoking the
// callback. The default debounce (1s) is well above the typical Vim/VSCode
// save storm but short enough that the operator perceives the reload as
// instant.
package configwatch

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a single YAML file and invokes onChange after each
// debounced edit. Methods are goroutine-safe.
type Watcher struct {
	path     string
	debounce time.Duration
	logger   *slog.Logger
	onChange func()

	fsw *fsnotify.Watcher

	mu      sync.Mutex
	pending *time.Timer
	closed  bool

	done chan struct{}
}

// New constructs a Watcher rooted at path with the given debounce and a
// callback to invoke after debounced changes. fsnotify watches the parent
// directory rather than the file itself because editors that swap on save
// (Vim's :w default) replace the file's inode — a watcher on the original
// path would not see the swap.
func New(path string, debounce time.Duration, logger *slog.Logger, onChange func()) (*Watcher, error) {
	if path == "" {
		return nil, fmt.Errorf("configwatch: empty path")
	}
	if onChange == nil {
		return nil, fmt.Errorf("configwatch: nil onChange callback")
	}
	if debounce <= 0 {
		debounce = time.Second
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("configwatch: abs path: %w", err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("configwatch: new watcher: %w", err)
	}
	dir := filepath.Dir(abs)
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("configwatch: add %q: %w", dir, err)
	}
	w := &Watcher{
		path:     abs,
		debounce: debounce,
		logger:   logger,
		onChange: onChange,
		fsw:      fsw,
		done:     make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// run is the watcher's main loop. It fires onChange after debounce ticks
// past the most recent qualifying event for the watched path.
func (w *Watcher) run() {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != w.path {
				continue
			}
			// Editors writing through tmpfile + rename produce a Create
			// event; in-place writes produce Write. Both should trigger a
			// reload. Remove/Rename without a follow-up Create means the
			// file vanished — skip rather than try to read a missing file.
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			w.armDebounce()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			if w.logger != nil {
				w.logger.Warn("configwatch: fsnotify error", "err", err)
			}
		}
	}
}

func (w *Watcher) armDebounce() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if w.pending != nil {
		w.pending.Stop()
	}
	w.pending = time.AfterFunc(w.debounce, w.fire)
}

func (w *Watcher) fire() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.pending = nil
	w.mu.Unlock()
	w.onChange()
}

// Close stops the watcher. Safe to call multiple times.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	if w.pending != nil {
		w.pending.Stop()
		w.pending = nil
	}
	w.mu.Unlock()
	close(w.done)
	return w.fsw.Close()
}
