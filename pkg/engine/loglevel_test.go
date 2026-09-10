package engine

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

// TestParseLogLevel covers every recognized level string, including the new
// "trace" case, plus the default fallback to slog.LevelInfo for unrecognized
// input (matching the pre-existing parseLogLevel behavior).
func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"trace", LevelTrace},
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"bogus", slog.LevelInfo},
		{"", slog.LevelInfo},
	}
	for _, tc := range cases {
		if got := ParseLogLevel(tc.in); got != tc.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestLevelTrace_BelowDebug pins the ordering the whole rubric depends on:
// Trace must sort strictly below Debug so a "debug" config never leaks
// Trace-level records.
func TestLevelTrace_BelowDebug(t *testing.T) {
	if LevelTrace >= slog.LevelDebug {
		t.Fatalf("LevelTrace (%v) must be strictly below slog.LevelDebug (%v)", LevelTrace, slog.LevelDebug)
	}
}

// TestNewFromBytes_TraceLogLevel_EnablesFanoutAtTrace verifies core.log_level:
// trace actually reaches the fanout handler's minimum level at engine
// construction time, not just that ParseLogLevel maps the string correctly.
func TestNewFromBytes_TraceLogLevel_EnablesFanoutAtTrace(t *testing.T) {
	eng, err := NewFromBytes([]byte("core:\n  log_level: trace\n"))
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	if !eng.Logger.Enabled(context.Background(), LevelTrace) {
		t.Error("expected engine.Logger to be Enabled at LevelTrace when core.log_level is \"trace\"")
	}
}

// TestNewFromBytes_DebugLogLevel_DisablesTrace confirms Trace stays off under
// the existing "debug" level — Trace is a distinct, lower rung, not folded
// into Debug.
func TestNewFromBytes_DebugLogLevel_DisablesTrace(t *testing.T) {
	eng, err := NewFromBytes([]byte("core:\n  log_level: debug\n"))
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	if eng.Logger.Enabled(context.Background(), LevelTrace) {
		t.Error("expected engine.Logger to be disabled at LevelTrace when core.log_level is \"debug\"")
	}
}

// TestFanoutHandler_PassesThroughTraceRecords is the acceptance-criteria
// "confirm, don't just assume" check: a record emitted at LevelTrace via the
// generic Logger.Log method (the documented convention for future
// reclassification stories, since *slog.Logger has no .Trace() helper) must
// reach a registered sink unmodified, with no FanoutHandler code change
// needed to support the new level.
func TestFanoutHandler_PassesThroughTraceRecords(t *testing.T) {
	fanout := NewFanoutHandler(16, LevelTrace)
	logger := slog.New(fanout)

	sink := &recordingHandler{}
	remove := fanout.AddLogSink(sink)
	defer remove()

	logger.Log(context.Background(), LevelTrace, "trace message", "key", "val")

	if len(sink.records) != 1 {
		t.Fatalf("sink received %d records, want 1", len(sink.records))
	}
	if got := sink.records[0].Level; got != LevelTrace {
		t.Errorf("record level = %v, want %v", got, LevelTrace)
	}
	if got := sink.records[0].Message; got != "trace message" {
		t.Errorf("record message = %q, want %q", got, "trace message")
	}
}

// TestStderrTextHandler_EnabledAtTrace confirms the other sink named in the
// acceptance criteria — the stderr slog.NewTextHandler used for
// core.logging.bootstrap_stderr — also accepts LevelTrace via its ordinary
// HandlerOptions.Level gate, with no code change.
func TestStderrTextHandler_EnabledAtTrace(t *testing.T) {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: LevelTrace})
	if !h.Enabled(context.Background(), LevelTrace) {
		t.Error("expected stderr text handler configured at LevelTrace to be Enabled for LevelTrace records")
	}
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected stderr text handler configured at LevelTrace to still be Enabled for Debug records")
	}
}
