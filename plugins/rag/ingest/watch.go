package ingest

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watcher wraps fsnotify with per-entry namespace routing, glob filtering,
// and a small debounce so bursts of Write events on the same file coalesce.
type watcher struct {
	logger  *slog.Logger
	fsw     *fsnotify.Watcher
	entries []watchEntry
	onEvent func(path, namespace string, removed bool)

	mu      sync.Mutex
	pending map[string]*time.Timer // path -> debounce timer

	done chan struct{}
}

const debounceWindow = 250 * time.Millisecond

func newWatcher(logger *slog.Logger, entries []watchEntry, onEvent func(path, ns string, removed bool)) (*watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &watcher{
		logger:  logger,
		fsw:     fsw,
		entries: entries,
		onEvent: onEvent,
		pending: make(map[string]*time.Timer),
		done:    make(chan struct{}),
	}
	for _, e := range entries {
		if err := fsw.Add(e.Path); err != nil {
			logger.Warn("watcher: failed to add path", "path", e.Path, "err", err)
		}
	}
	go w.run()
	return w, nil
}

func (w *watcher) run() {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.dispatch(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logger.Warn("watcher: fsnotify error", "err", err)
		}
	}
}

// dispatch matches the event against each configured watch entry and either
// fires immediately (removals) or debounces (writes/creates).
func (w *watcher) dispatch(ev fsnotify.Event) {
	for _, e := range w.entries {
		if !isUnder(ev.Name, e.Path) {
			continue
		}
		if e.Glob != "" {
			rel, err := filepath.Rel(e.Path, ev.Name)
			if err != nil {
				rel = ev.Name
			}
			ok1, _ := filepath.Match(e.Glob, rel)
			ok2, _ := filepath.Match(e.Glob, filepath.Base(ev.Name))
			if !ok1 && !ok2 {
				continue
			}
		}
		if ev.Op&fsnotify.Remove != 0 || ev.Op&fsnotify.Rename != 0 {
			w.onEvent(ev.Name, e.Namespace, true)
			return
		}
		if ev.Op&fsnotify.Write != 0 || ev.Op&fsnotify.Create != 0 {
			w.debounce(ev.Name, e.Namespace)
			return
		}
	}
}

// debounce coalesces bursts of Write events on the same path into a single
// ingest call fired debounceWindow after the last write.
func (w *watcher) debounce(path, ns string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.pending[path]; ok {
		t.Stop()
	}
	w.pending[path] = time.AfterFunc(debounceWindow, func() {
		w.mu.Lock()
		delete(w.pending, path)
		w.mu.Unlock()
		w.onEvent(path, ns, false)
	})
}

func (w *watcher) close() {
	close(w.done)
	_ = w.fsw.Close()
	w.mu.Lock()
	for _, t := range w.pending {
		t.Stop()
	}
	w.pending = nil
	w.mu.Unlock()
}

// isUnder reports whether p is equal to or nested under root. Used to match
// fsnotify events (which report absolute paths) against configured roots.
func isUnder(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
