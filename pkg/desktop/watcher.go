package desktop

import (
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fileWatcher watches a single directory for changes and calls back
// when files are created, removed, or renamed. Only one directory is
// watched at a time — calling Watch replaces the previous target.
//
// The watcher debounces rapid filesystem events (editors often write
// temp files then rename) so the callback fires at most once per
// debounce interval.
type fileWatcher struct {
	mu       sync.Mutex
	watcher  *fsnotify.Watcher
	dir      string
	onChange func(dir string)
	stopCh   chan struct{}
}

// newFileWatcher creates a watcher. onChange is called (at most once
// per debounce window) when the watched directory's contents change.
func newFileWatcher(onChange func(dir string)) (*fileWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fileWatcher{
		watcher:  w,
		onChange: onChange,
	}, nil
}

// Watch starts watching the given directory. If a different directory
// was previously watched, it is unwatched first. Passing "" stops
// watching without starting a new watch.
func (fw *fileWatcher) Watch(dir string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	// Stop previous watch loop.
	if fw.stopCh != nil {
		close(fw.stopCh)
		fw.stopCh = nil
	}

	// Remove old directory.
	if fw.dir != "" {
		_ = fw.watcher.Remove(fw.dir)
		fw.dir = ""
	}

	if dir == "" {
		return
	}

	if err := fw.watcher.Add(dir); err != nil {
		log.Printf("file watcher: cannot watch %q: %v", dir, err)
		return
	}
	fw.dir = dir
	fw.stopCh = make(chan struct{})
	go fw.loop(dir, fw.stopCh)
}

// Close tears down the underlying fsnotify watcher.
func (fw *fileWatcher) Close() {
	fw.mu.Lock()
	if fw.stopCh != nil {
		close(fw.stopCh)
		fw.stopCh = nil
	}
	fw.mu.Unlock()
	_ = fw.watcher.Close()
}

const debounceInterval = 200 * time.Millisecond

func (fw *fileWatcher) loop(dir string, stop chan struct{}) {
	var timer *time.Timer
	for {
		select {
		case <-stop:
			if timer != nil {
				timer.Stop()
			}
			return

		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}
			// Only fire on meaningful operations.
			if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			// Debounce: reset timer on each event.
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounceInterval, func() {
				if fw.onChange != nil {
					fw.onChange(dir)
				}
			})

		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("file watcher error: %v", err)
		}
	}
}
