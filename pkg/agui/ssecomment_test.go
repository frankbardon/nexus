package agui

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestWriteCommentIsInvisibleToAReader proves a keepalive changes nothing a
// client observes: the reader skips comment records, so a stream carrying them
// decodes to exactly the events it would have without them.
func TestWriteCommentIsInvisibleToAReader(t *testing.T) {
	var buf bytes.Buffer
	w := NewSSEWriter(&buf)

	if err := w.WriteComment("heartbeat"); err != nil {
		t.Fatalf("WriteComment: %v", err)
	}
	if err := w.Write(NewRunStarted("t", "r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.WriteComment("heartbeat"); err != nil {
		t.Fatalf("WriteComment: %v", err)
	}
	if err := w.Write(NewRunFinished("t", "r")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !strings.HasPrefix(buf.String(), ": heartbeat\n\n") {
		t.Errorf("a comment record must be a %q line; got %q", ":", buf.String()[:20])
	}

	r := NewSSEReader(bytes.NewReader(buf.Bytes()))
	var got []string
	for {
		e, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, string(e.EventType()))
	}
	if len(got) != 2 || got[0] != "RUN_STARTED" || got[1] != "RUN_FINISHED" {
		t.Errorf("decoded %v, want [RUN_STARTED RUN_FINISHED] — comments must be skipped", got)
	}
}

// TestWriteCommentFlushes pins the property the keepalive exists for: a
// heartbeat buffered behind a proxy is not a heartbeat.
func TestWriteCommentFlushes(t *testing.T) {
	f := &countingFlusher{}
	if err := NewSSEWriter(f).WriteComment("heartbeat"); err != nil {
		t.Fatalf("WriteComment: %v", err)
	}
	if f.flushes != 1 {
		t.Errorf("flushes = %d, want 1", f.flushes)
	}
}

type countingFlusher struct {
	bytes.Buffer
	flushes int
}

func (c *countingFlusher) Flush() { c.flushes++ }
