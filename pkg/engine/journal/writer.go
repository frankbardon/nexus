package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Writer is the durable JSONL appender for a single session's journal.
//
// Writes are decoupled from the bus dispatch goroutine via a buffered channel:
// the bus's wildcard handler builds an Envelope (with seq + parent_seq
// supplied by the bus) and pushes it on the channel; a drain goroutine
// orders envelopes by seq (since wildcard order is child-before-parent for
// nested emits) and appends them to events.jsonl. Fsync policy is honored
// per-envelope.
type Writer struct {
	dir         string
	mode        FsyncMode
	rotateBytes int64
	initialSeq  uint64

	ch    chan *Envelope
	doneC chan struct{}

	// closed flips to true under sendMu's write lock when Close runs. Append
	// observes it under sendMu's read lock and returns without sending,
	// preventing a "send on closed channel" race.
	closed    atomic.Bool
	sendMu    sync.RWMutex
	closeOnce sync.Once

	mu          sync.Mutex // guards activeFile + activeBytes during writes
	activeFile  *os.File
	activeBuf   *bufio.Writer
	activeBytes int64

	// rotateCb is invoked from the drain goroutine when an envelope's type
	// indicates a turn boundary and the active segment is over rotateBytes.
	// Stubbed — the real rotation logic lives in rotate.go.
	rotateCb func(w *Writer) error
}

// WriterOptions tune the writer.
type WriterOptions struct {
	FsyncMode     FsyncMode
	RotateBytes   int64
	BufferSize    int
	SchemaVersion string
	SessionID     string
	// InitialSeq is the lowest seq the drain expects to flush. Defaults
	// to 1 for fresh sessions; the engine sets this to LastSeq+1 on
	// recall so the reorder buffer does not stall waiting for a seq the
	// new run will never produce.
	InitialSeq uint64
}

const (
	activeSegmentName = "events.jsonl"
	headerName        = "header.json"
	defaultRotate     = int64(4 << 20)
	defaultBuffer     = 1024
)

// NewWriter creates the journal directory if absent, writes (or validates)
// the header, opens events.jsonl for append, and starts the drain goroutine.
//
// Multiple writers must not target the same dir; the engine creates exactly
// one writer per session and that constraint is enforced at the call site.
func NewWriter(dir string, opts WriterOptions) (*Writer, error) {
	if dir == "" {
		return nil, fmt.Errorf("journal: empty dir")
	}
	if opts.FsyncMode == "" {
		opts.FsyncMode = FsyncTurnBoundary
	}
	if opts.RotateBytes <= 0 {
		opts.RotateBytes = defaultRotate
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = defaultBuffer
	}
	if opts.SchemaVersion == "" {
		opts.SchemaVersion = SchemaVersion
	}
	if opts.InitialSeq == 0 {
		opts.InitialSeq = 1
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating journal dir: %w", err)
	}

	if err := ensureHeader(dir, opts); err != nil {
		return nil, err
	}

	activePath := filepath.Join(dir, activeSegmentName)
	f, err := os.OpenFile(activePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening active segment: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat active segment: %w", err)
	}

	w := &Writer{
		dir:         dir,
		mode:        opts.FsyncMode,
		rotateBytes: opts.RotateBytes,
		ch:          make(chan *Envelope, opts.BufferSize),
		doneC:       make(chan struct{}),
		activeFile:  f,
		activeBuf:   bufio.NewWriter(f),
		activeBytes: info.Size(),
		rotateCb:    rotateActiveSegment,
		initialSeq:  opts.InitialSeq,
	}

	go w.drain()
	return w, nil
}

// ensureHeader writes header.json on first construction, or validates the
// existing header on subsequent opens. Mismatch on schema_version aborts.
func ensureHeader(dir string, opts WriterOptions) error {
	path := filepath.Join(dir, headerName)
	if _, err := os.Stat(path); err == nil {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("reading journal header: %w", rerr)
		}
		var h Header
		if jerr := json.Unmarshal(data, &h); jerr != nil {
			return fmt.Errorf("parsing journal header: %w", jerr)
		}
		if h.SchemaVersion != opts.SchemaVersion {
			return fmt.Errorf("journal schema mismatch: header=%q want=%q (delete %s to start fresh)",
				h.SchemaVersion, opts.SchemaVersion, dir)
		}
		return nil
	}

	h := Header{
		SchemaVersion: opts.SchemaVersion,
		CreatedAt:     time.Now().UTC(),
		FsyncMode:     string(opts.FsyncMode),
		SessionID:     opts.SessionID,
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling header: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}
	return nil
}

// Append queues an envelope for asynchronous write. Blocks if the channel is
// full so callers feel back-pressure rather than silently losing events. Safe
// to call concurrently with Close — once closed, subsequent Appends drop.
func (w *Writer) Append(env *Envelope) {
	if env == nil {
		return
	}
	w.sendMu.RLock()
	defer w.sendMu.RUnlock()
	if w.closed.Load() {
		return
	}
	w.ch <- env
}

// Close drains in-flight envelopes, fsyncs, and shuts the file. Safe to call
// multiple times; subsequent calls return nil.
func (w *Writer) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.sendMu.Lock()
		w.closed.Store(true)
		close(w.ch)
		w.sendMu.Unlock()
	})

	select {
	case <-w.doneC:
	case <-ctx.Done():
		return ctx.Err()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activeBuf != nil {
		_ = w.activeBuf.Flush()
		w.activeBuf = nil
	}
	if w.activeFile != nil {
		_ = w.activeFile.Sync()
		err := w.activeFile.Close()
		w.activeFile = nil
		if err != nil {
			return fmt.Errorf("closing active segment: %w", err)
		}
	}
	return nil
}

// drain is the writer's single I/O goroutine. It reorders envelopes by seq
// (because the bus's wildcard dispatch order is child-before-parent for
// nested emits — see envelope.go for the causal-order rationale) and writes
// them in monotonic seq order to events.jsonl.
func (w *Writer) drain() {
	defer close(w.doneC)

	pending := make(map[uint64]*Envelope, 16)
	nextSeq := w.initialSeq
	if nextSeq == 0 {
		nextSeq = 1
	}

	flush := func() {
		for {
			env, ok := pending[nextSeq]
			if !ok {
				return
			}
			delete(pending, nextSeq)
			nextSeq++
			if err := w.writeOne(env); err != nil {
				// Logging is intentionally avoided here — the engine logger
				// may itself be writing to a sink that races our shutdown.
				// A failed write is rare; the loop continues so later
				// envelopes still land.
				_ = err
			}
		}
	}

	for env := range w.ch {
		pending[env.Seq] = env
		flush()
	}

	// Channel closed: drain whatever remains. Out-of-order tails imply a
	// gap (seq lost between pending push and channel send — should not
	// happen in practice). Write what we have in seq order anyway.
	if len(pending) > 0 {
		seqs := make([]uint64, 0, len(pending))
		for s := range pending {
			seqs = append(seqs, s)
		}
		// simple insertion sort — pending tail is tiny
		for i := 1; i < len(seqs); i++ {
			for j := i; j > 0 && seqs[j-1] > seqs[j]; j-- {
				seqs[j-1], seqs[j] = seqs[j], seqs[j-1]
			}
		}
		for _, s := range seqs {
			if env, ok := pending[s]; ok {
				_ = w.writeOne(env)
			}
		}
	}
}

// writeOne marshals one envelope and appends it. Honors fsync policy and
// triggers rotation when an agent.turn.end pushes the segment past its cap.
func (w *Writer) writeOne(env *Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		// Replace payload with a marker so the slot is still recorded.
		fallback := *env
		fallback.Payload = map[string]string{"__journal_error": err.Error()}
		data, err = json.Marshal(fallback)
		if err != nil {
			return err
		}
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activeBuf == nil {
		return fmt.Errorf("journal closed")
	}
	n, werr := w.activeBuf.Write(data)
	if werr != nil {
		return fmt.Errorf("writing envelope: %w", werr)
	}
	w.activeBytes += int64(n)

	switch w.mode {
	case FsyncEveryEvent:
		_ = w.activeBuf.Flush()
		_ = w.activeFile.Sync()
	case FsyncTurnBoundary:
		if env.Type == "agent.turn.end" {
			_ = w.activeBuf.Flush()
			_ = w.activeFile.Sync()
		}
	}

	if env.Type == "agent.turn.end" && w.activeBytes >= w.rotateBytes && w.rotateCb != nil {
		if rerr := w.rotateCb(w); rerr != nil {
			// Log-and-continue: rotation failure does not lose data because
			// the active segment remains valid; it just keeps growing.
			_ = rerr
		}
	}

	return nil
}

// JournalDir returns the directory passed to NewWriter. Used by Coordinator
// (Phase 2) and by tests.
func (w *Writer) JournalDir() string { return w.dir }
