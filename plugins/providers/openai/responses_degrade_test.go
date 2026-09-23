package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the two known degradations of the Responses path and, more
// importantly, the fact that they are treated as opposites: a rejected
// reasoning replay fails the request naming the cause, while a `prediction`
// with no counterpart on this surface is dropped with one warning and the turn
// carries on.

// capturingLogger returns a JSON logger and the buffer behind it, for the
// warn-once assertions.
func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// warnLines returns the decoded WARN records a capturing buffer collected.
func warnLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["level"] == "WARN" {
			out = append(out, rec)
		}
	}
	return out
}

// --- classification ---------------------------------------------------------

// The matcher must fire on the shapes a rejected Item actually arrives in, and
// must NOT fire on the unrelated failures of the request's `reasoning` object —
// misattributing an effort-value rejection to replay would send an operator
// hunting the wrong thing.
func TestIsReasoningReplayRejection(t *testing.T) {
	cases := []struct {
		name          string
		code, msg, pm string
		want          bool
	}{
		{
			name: "item named by id and type",
			msg:  "Item 'rs_68a1' of type 'reasoning' was provided without its required following item.",
			want: true,
		},
		{
			name: "encrypted blob failed verification",
			msg:  "The encrypted content for reasoning item rs_68a1 could not be verified.",
			want: true,
		},
		{
			name: "named only in param",
			code: "invalid_value",
			msg:  "Invalid value.",
			pm:   "input[0].encrypted_content",
			want: true,
		},
		{
			name: "double-quoted item spelling",
			msg:  `Item "rs_68a1" is not valid for this model.`,
			want: true,
		},
		{
			name: "reasoning effort rejected is NOT a replay rejection",
			code: "invalid_value",
			msg:  "Invalid value: 'xhigh'. Supported values are: 'low', 'medium' and 'high'.",
			pm:   "reasoning.effort",
			want: false,
		},
		{
			name: "an ordinary bad request is not one either",
			code: "context_length_exceeded",
			msg:  "This model's maximum context length is 128000 tokens.",
			want: false,
		},
		{
			name: "empty everything",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReasoningReplayRejection(tc.code, tc.msg, tc.pm); got != tc.want {
				t.Errorf("isReasoningReplayRejection(%q, %q, %q) = %v, want %v", tc.code, tc.msg, tc.pm, got, tc.want)
			}
		})
	}
}

// A 400 naming the Item is classified; every other status is left alone, because
// a 401, a 429 or a 5xx is about the deployment or the service and has nothing
// to do with the conversation's reasoning.
func TestReplayRejectionFromStatus(t *testing.T) {
	envelope := `{"error":{"message":"Item 'rs_1' of type 'reasoning' was provided without its required following item.","type":"invalid_request_error","param":"input[0]","code":"invalid_value"}}`

	if err := replayRejectionFromStatus(http.StatusBadRequest, []byte(envelope)); err == nil {
		t.Error("a 400 naming a reasoning Item must classify as a replay rejection")
	}
	if err := replayRejectionFromStatus(http.StatusTooManyRequests, []byte(envelope)); err != nil {
		t.Errorf("a 429 must not be classified as a replay rejection: %v", err)
	}
	if err := replayRejectionFromStatus(http.StatusBadRequest, []byte(`{"error":{"message":"Unknown parameter: 'foo'."}}`)); err != nil {
		t.Errorf("an unrelated 400 must not be classified: %v", err)
	}
	// A proxy that rewrote the envelope but kept the message still gets the
	// explanation.
	if err := replayRejectionFromStatus(http.StatusBadRequest, []byte("encrypted_content could not be verified")); err == nil {
		t.Error("a non-JSON 400 naming the blob must still classify")
	}
}

// The message has to stand in for a detection Nexus deliberately does not do,
// so it must name both causes an operator could be hitting.
func TestReasoningReplayError_NamesBothCauses(t *testing.T) {
	msg := reasoningReplayError("invalid_value", "could not be verified").Error()
	for _, want := range []string{
		"could not be verified", // the API's own words survive
		"encrypted_content",
		"model families",
		"rather than retried without it",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
}

// --- surface 1: a run that fails inside an HTTP 200 -------------------------

func TestResponsesSync_ReplayRejectionFailsTheTurn(t *testing.T) {
	r := newStreamRecorder()
	p := quietPlugin()
	p.bus = r.bus

	body := `{"id":"resp_1","model":"gpt-5.1","status":"failed","error":{"code":"invalid_value","message":"The encrypted content for reasoning item rs_1 could not be verified.","param":"input[0].encrypted_content"}}`
	p.handleResponsesSyncResponse(strings.NewReader(body), "req-1", nil, nil, reasoningConfig{})

	if len(r.responses) != 0 {
		t.Fatalf("published %d llm.response events, want 0 — a rejected replay must not be answered", len(r.responses))
	}
	if len(r.errors) != 1 {
		t.Fatalf("published %d core.error events, want 1", len(r.errors))
	}
	got := r.errors[0].Err.Error()
	if !strings.Contains(got, "rejected a replayed reasoning item") {
		t.Errorf("core.error does not name the cause:\n%s", got)
	}
	if r.errors[0].Retryable {
		t.Error("a rejected replay is not retryable — the same Items would be rejected the same way")
	}
}

// The generic run failure keeps its generic message: only the replay case is
// re-described.
func TestResponsesSync_UnrelatedFailureKeepsItsOwnMessage(t *testing.T) {
	r := newStreamRecorder()
	p := quietPlugin()
	p.bus = r.bus

	body := `{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"The model is overloaded."}}`
	p.handleResponsesSyncResponse(strings.NewReader(body), "req-1", nil, nil, reasoningConfig{})

	if len(r.errors) != 1 {
		t.Fatalf("published %d core.error events, want 1", len(r.errors))
	}
	got := r.errors[0].Err.Error()
	if strings.Contains(got, "replayed reasoning item") {
		t.Errorf("an overload must not be reported as a replay rejection:\n%s", got)
	}
	if !strings.Contains(got, "The model is overloaded.") {
		t.Errorf("core.error lost the API's own message:\n%s", got)
	}
}

// --- surface 2: a run that dies mid-stream ----------------------------------

// Half a turn already rendered changes nothing about the verdict: the rest of
// it would have been produced with none of the model's own reasoning behind it.
func TestResponsesStream_ReplayRejectionFailsTheTurn(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"Let me check"}`,
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"invalid_value","message":"Item 'rs_1' of type 'reasoning' could not be verified.","param":"input[0]"}}}`,
	))

	if len(r.responses) != 0 {
		t.Fatalf("published %d llm.response events, want 0", len(r.responses))
	}
	if len(r.errors) != 1 {
		t.Fatalf("published %d core.error events, want 1", len(r.errors))
	}
	if got := r.errors[0].Err.Error(); !strings.Contains(got, "rejected a replayed reasoning item") {
		t.Errorf("core.error does not name the cause:\n%s", got)
	}
	// The transport still gets its terminator.
	if len(r.ends) != 1 || r.ends[0].FinishReason != "error" {
		t.Errorf("llm.stream.end = %+v, want exactly one with FinishReason error", r.ends)
	}
}

// The flat `error` event carries the same classification, param included.
func TestResponsesStream_FlatErrorEventIsClassified(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"error","code":"invalid_value","message":"Invalid value.","param":"input[2].encrypted_content"}`,
	))

	if len(r.errors) != 1 {
		t.Fatalf("published %d core.error events, want 1", len(r.errors))
	}
	if got := r.errors[0].Err.Error(); !strings.Contains(got, "rejected a replayed reasoning item") {
		t.Errorf("a flat error event naming the blob in param must classify:\n%s", got)
	}
}

// --- surface 3: the request is refused outright -----------------------------

// The commonest shape: the API never accepts the request, so neither reply
// parser ever runs. The assertion that matters beyond the message is the
// request count — nothing retries the turn without its reasoning Items.
func TestResponsesHandler_Rejected400FailsLoudlyAndDoesNotRetry(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Item 'rs_1' of type 'reasoning' was provided without its required following item.","type":"invalid_request_error","param":"input[0]","code":"invalid_value"}}`)
	}))
	t.Cleanup(srv.Close)

	r := newStreamRecorder()
	p := &Plugin{}
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"api_key":  "sk-not-used",
			"api":      "responses",
			"base_url": srv.URL + "/v1/chat/completions",
		},
		Bus:    r.bus,
		Logger: silentLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	p.handleRequest(events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "reasoning",
		Model:         "gpt-5.1",
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
	})

	if requests != 1 {
		t.Errorf("server saw %d requests, want 1 — a rejected replay must not be retried", requests)
	}
	if len(r.responses) != 0 {
		t.Errorf("published %d llm.response events, want 0", len(r.responses))
	}
	if len(r.errors) != 1 {
		t.Fatalf("published %d core.error events, want 1", len(r.errors))
	}
	if got := r.errors[0].Err.Error(); !strings.Contains(got, "rejected a replayed reasoning item") {
		t.Errorf("core.error does not name the cause:\n%s", got)
	}
}

// An unrelated 400 on the same surface keeps the status message it always had.
func TestResponsesHandler_Unrelated400KeepsTheStatusMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Unknown parameter: 'foo'."}}`)
	}))
	t.Cleanup(srv.Close)

	r := newStreamRecorder()
	p := &Plugin{}
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"api_key":  "sk-not-used",
			"api":      "responses",
			"base_url": srv.URL + "/v1/chat/completions",
		},
		Bus:    r.bus,
		Logger: silentLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	p.handleRequest(events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5.1",
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
	})

	if len(r.errors) != 1 {
		t.Fatalf("published %d core.error events, want 1", len(r.errors))
	}
	got := r.errors[0].Err.Error()
	if strings.Contains(got, "replayed reasoning item") {
		t.Errorf("an unrelated 400 must not be reported as a replay rejection:\n%s", got)
	}
	if !strings.Contains(got, "status 400") {
		t.Errorf("core.error lost the status:\n%s", got)
	}
}

// --- the other half: prediction degrades quietly but audibly ----------------

// The field is dropped and the turn proceeds — the body is otherwise complete.
func TestResponsesBody_PredictionIsDroppedAndTheTurnProceeds(t *testing.T) {
	logger, buf := capturingLogger()
	p := &Plugin{logger: logger}

	body := p.buildResponsesBody("gpt-5.1", 2048, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "coder",
		Prediction:    "the known target text",
		Messages:      []events.Message{{Role: "user", Content: "rewrite this"}},
	})

	if _, ok := body["prediction"]; ok {
		t.Errorf("prediction must not reach the Responses wire, got %v", body["prediction"])
	}
	if body["input"] == nil {
		t.Error("the turn must still be serialized — dropping a prediction costs latency, not the turn")
	}

	warns := warnLines(t, buf)
	if len(warns) != 1 {
		t.Fatalf("logged %d WARN records, want exactly 1: %v", len(warns), warns)
	}
	if warns[0]["role"] != "coder" {
		t.Errorf("the warning must name the role, got %v", warns[0]["role"])
	}
	if !strings.Contains(warns[0]["msg"].(string), "prediction") {
		t.Errorf("the warning must name the dropped field: %v", warns[0]["msg"])
	}
}

// Once means once. An agent that sets a prediction sets one every turn, so a
// per-request warning would bury the role's log.
func TestResponsesBody_PredictionWarnsOncePerRole(t *testing.T) {
	logger, buf := capturingLogger()
	p := &Plugin{logger: logger}

	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "coder",
		Prediction:    "the known target text",
		Messages:      []events.Message{{Role: "user", Content: "rewrite this"}},
	}
	for i := 0; i < 5; i++ {
		p.buildResponsesBody("gpt-5.1", 2048, req)
	}
	if got := len(warnLines(t, buf)); got != 1 {
		t.Fatalf("logged %d WARN records across 5 requests, want 1", got)
	}

	// A second role is a second fact, so it gets its own line.
	req.Role = "editor"
	p.buildResponsesBody("gpt-5.1", 2048, req)
	warns := warnLines(t, buf)
	if len(warns) != 2 {
		t.Fatalf("logged %d WARN records, want 2 — one per degrading role", len(warns))
	}
	if warns[1]["role"] != "editor" {
		t.Errorf("the second warning must name the second role, got %v", warns[1]["role"])
	}
}

// A turn with no prediction says nothing at all.
func TestResponsesBody_NoPredictionNoWarning(t *testing.T) {
	logger, buf := capturingLogger()
	p := &Plugin{logger: logger}

	p.buildResponsesBody("gpt-5.1", 2048, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "coder",
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
	})
	if got := len(warnLines(t, buf)); got != 0 {
		t.Errorf("logged %d WARN records, want 0", got)
	}
}
