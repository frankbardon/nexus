package agui

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ContentType is the media type of an AG-UI SSE stream.
const ContentType = "text/event-stream"

// SSEWriter serializes AG-UI events to an io.Writer as Server-Sent Events. Each
// event is written as an SSE "data:" record (JSON payload) terminated by a blank
// line, and the underlying writer is flushed after every event when it supports
// http.Flusher so clients receive events as they are produced.
type SSEWriter struct {
	w       io.Writer
	flusher http.Flusher
}

// NewSSEWriter wraps w. If w implements http.Flusher, each event is flushed.
func NewSSEWriter(w io.Writer) *SSEWriter {
	sw := &SSEWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}
	return sw
}

// WriteHeaders sets the SSE response headers on an http.ResponseWriter. It is a
// convenience for server handlers and is optional.
func WriteHeaders(h http.Header) {
	h.Set("Content-Type", ContentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// Write encodes and emits a single AG-UI event as one SSE record.
func (sw *SSEWriter) Write(e Event) error {
	data, err := EncodeEvent(e)
	if err != nil {
		return err
	}
	return sw.writeData(string(data))
}

// writeData emits a single "data:" SSE record. Multi-line payloads are split
// across multiple data lines per the SSE spec; JSON marshalling never emits raw
// newlines, but this keeps the writer correct for any payload.
func (sw *SSEWriter) writeData(payload string) error {
	var buf bytes.Buffer
	for _, line := range strings.Split(payload, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := sw.w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("agui: write sse event: %w", err)
	}
	sw.Flush()
	return nil
}

// Flush flushes the underlying writer if it supports flushing.
func (sw *SSEWriter) Flush() {
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
}

// SSEReader decodes an AG-UI SSE stream into events. It handles multi-line
// "data:" records, comments (":" lines), and blank-line record boundaries.
type SSEReader struct {
	sc *bufio.Scanner
}

// NewSSEReader reads an AG-UI SSE stream from r.
func NewSSEReader(r io.Reader) *SSEReader {
	sc := bufio.NewScanner(r)
	// Allow large single-event payloads (default 64KiB is too small for
	// snapshots). Grow the buffer up to 8 MiB.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &SSEReader{sc: sc}
}

// Next reads and decodes the next AG-UI event from the stream. It returns
// io.EOF when the stream is exhausted.
func (sr *SSEReader) Next() (Event, error) {
	var data strings.Builder
	haveData := false

	for sr.sc.Scan() {
		line := sr.sc.Text()

		if line == "" {
			// Record boundary.
			if !haveData {
				continue
			}
			return DecodeEvent([]byte(data.String()))
		}
		if strings.HasPrefix(line, ":") {
			// Comment / heartbeat line.
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		}
		// A single leading space after the colon is stripped per the SSE spec.
		value = strings.TrimPrefix(value, " ")

		if field == "data" {
			if haveData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			haveData = true
		}
		// Other fields (event, id, retry) are ignored: AG-UI carries its
		// discriminator inside the JSON payload's "type".
	}

	if err := sr.sc.Err(); err != nil {
		return nil, fmt.Errorf("agui: read sse stream: %w", err)
	}
	if haveData {
		// Final record without a trailing blank line.
		return DecodeEvent([]byte(data.String()))
	}
	return nil, io.EOF
}

// ReadAll drains the SSE stream into a slice of events. It stops at io.EOF.
func (sr *SSEReader) ReadAll() ([]Event, error) {
	var events []Event
	for {
		e, err := sr.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, e)
	}
}
