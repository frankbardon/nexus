package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
)

// RenderTranscript renders a live session's full, observed event stream as
// legible plain text, suitable as protocol.Request.UserInput for an LLM
// judge alongside a rubric (E2-S3).
//
// v1 is deliberately minimal, per the interview's own resolved decision:
// every event in events is rendered, in order, with its full payload — no
// event-type filtering, no payload truncation. Narrowing that (dropping
// high-frequency noise, trimming huge payloads) is explicitly deferred
// until real usage shows what's actually noise; do not add it here.
//
// Each event renders as a numbered "[n] <RFC3339 timestamp> <type>" header
// line, followed by its payload as indented JSON (omitted when the payload
// is nil). This is a rendering of the ObservedEvent projection's own
// Type/Timestamp/Payload fields, not a raw dump of engine- or
// journal-internal bookkeeping (seq, event_id, parent_seq, etc. never
// reach ObservedEvent in the first place).
func RenderTranscript(events []evalcase.ObservedEvent) string {
	var b strings.Builder
	for i, e := range events {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%d] %s %s\n", i+1, e.Timestamp.UTC().Format(time.RFC3339Nano), e.Type)
		if body := renderPayload(e.Payload); body != "" {
			b.WriteString(body)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// renderPayload pretty-prints an event payload as indented JSON, two-space
// indented under its header line. Handles both shapes ObservedEvent.Payload
// arrives in across the runner's two observed-stream sources: a
// map[string]any (the common case — payloads decoded off the live
// journal's own JSONL) and a typed event struct (the wildcard-collector
// fallback, used when the live journal itself could not be read). Returns
// "" for a nil payload so the caller can skip the body entirely.
func renderPayload(payload any) string {
	if payload == nil {
		return ""
	}
	raw, err := json.MarshalIndent(payload, "  ", "  ")
	if err != nil || string(raw) == "null" {
		return ""
	}
	return "  " + string(raw)
}
