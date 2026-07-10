package aguiclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
)

// fullStreamHandler emits the full AG-UI event set (lifecycle, reasoning, text,
// tool, state) so the streaming client is exercised across every family. It
// optionally enforces a bearer token.
func fullStreamHandler(bearer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bearer != "" && r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		in, err := agui.DecodeRunAgentInput(mustReadAll(r))
		if err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		agui.WriteHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		sse := agui.NewSSEWriter(w)
		_ = sse.Write(agui.NewRunStarted(in.ThreadID, in.RunID))
		_ = sse.Write(agui.NewReasoningStart())
		_ = sse.Write(agui.NewReasoningMessageContent("thinking"))
		_ = sse.Write(agui.NewReasoningEnd())
		_ = sse.Write(agui.NewToolCallStart("tc1", "search"))
		_ = sse.Write(agui.NewToolCallArgs("tc1", `{"q":"x"}`))
		_ = sse.Write(agui.NewToolCallEnd("tc1"))
		_ = sse.Write(agui.NewToolCallResult("m0", "tc1", "found"))
		_ = sse.Write(agui.NewStateSnapshot([]byte(`{"k":"v"}`)))
		_ = sse.Write(agui.NewTextMessageStart("m1", "assistant"))
		_ = sse.Write(agui.NewTextMessageContent("m1", "hi"))
		_ = sse.Write(agui.NewTextMessageEnd("m1"))
		_ = sse.Write(agui.NewRunFinished(in.ThreadID, in.RunID))
	}
}

// interruptStreamHandler emits a run that ends on an interrupt outcome so the
// streaming client's resume-surfacing path can be asserted.
func interruptStreamHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, err := agui.DecodeRunAgentInput(mustReadAll(r))
		if err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		agui.WriteHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		sse := agui.NewSSEWriter(w)
		_ = sse.Write(agui.NewRunStarted(in.ThreadID, in.RunID))
		_ = sse.Write(agui.NewTextMessageStart("m1", "assistant"))
		_ = sse.Write(agui.NewTextMessageContent("m1", "need approval"))
		_ = sse.Write(agui.NewTextMessageEnd("m1"))
		fin := agui.NewRunFinishedInterrupt(in.ThreadID, in.RunID, agui.Interrupt{
			InterruptID: "int-7",
			Prompt:      "Approve deploy?",
			Mode:        agui.InterruptModeChoices,
			Choices: []agui.InterruptChoice{
				{ID: "yes", Label: "Yes"},
				{ID: "no", Label: "No"},
			},
		})
		_ = sse.Write(fin)
	}
}

// drain ranges over a stream to completion, returning the event types seen in
// order. It fully drives the reader goroutine so Result()/Err() are stable.
func drain(s *Stream) []agui.EventType {
	var types []agui.EventType
	for ev := range s.Events() {
		types = append(types, ev.EventType())
	}
	return types
}

func TestStream_DecodesFullEventSet(t *testing.T) {
	srv := httptest.NewServer(fullStreamHandler(""))
	defer srv.Close()

	s, err := New(srv.URL).Stream(t.Context(), UserMessage("t1", "r1", "hello"))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer s.Close()

	got := drain(s)
	if s.Err() != nil {
		t.Fatalf("stream err: %v", s.Err())
	}
	want := []agui.EventType{
		agui.EventRunStarted,
		agui.EventReasoningStart,
		agui.EventReasoningMessageContent,
		agui.EventReasoningEnd,
		agui.EventToolCallStart,
		agui.EventToolCallArgs,
		agui.EventToolCallEnd,
		agui.EventToolCallResult,
		agui.EventStateSnapshot,
		agui.EventTextMessageStart,
		agui.EventTextMessageContent,
		agui.EventTextMessageEnd,
		agui.EventRunFinished,
	}
	if len(got) != len(want) {
		t.Fatalf("streamed %d events, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	// Result() must carry the same accumulated sequence for terminal inspection.
	res := s.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("result status = %d, want 200", res.StatusCode)
	}
	if len(res.Events) != len(want) {
		t.Fatalf("result carries %d events, want %d", len(res.Events), len(want))
	}
	if res.Outcome() != "" {
		t.Fatalf("outcome = %q, want empty on a normal finish", res.Outcome())
	}
	if _, ok := res.Interrupt(); ok {
		t.Fatal("Interrupt() ok = true on a normal finish, want false")
	}
}

func TestStream_BearerRejected(t *testing.T) {
	srv := httptest.NewServer(fullStreamHandler("secret"))
	defer srv.Close()

	// No token -> 401, an already-closed empty channel, no terminal error.
	s, err := New(srv.URL).Stream(t.Context(), UserMessage("t1", "r1", "hello"))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer s.Close()
	if got := drain(s); len(got) != 0 {
		t.Fatalf("streamed %v on rejection, want none", got)
	}
	if s.Err() != nil {
		t.Fatalf("err = %v, want nil on a clean rejection", s.Err())
	}
	if s.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", s.Result().StatusCode)
	}

	// Correct token -> a full 200 stream.
	s, err = New(srv.URL, WithBearer("secret")).Stream(t.Context(), UserMessage("t1", "r1", "hello"))
	if err != nil {
		t.Fatalf("stream with token: %v", err)
	}
	defer s.Close()
	drain(s)
	if s.Err() != nil {
		t.Fatalf("authorized stream err: %v", s.Err())
	}
	if s.Result().First(agui.EventRunFinished) == nil {
		t.Fatal("no RunFinished in authorized stream")
	}
}

func TestStream_SurfacesInterruptOutcome(t *testing.T) {
	srv := httptest.NewServer(interruptStreamHandler())
	defer srv.Close()

	s, err := New(srv.URL).Stream(t.Context(), UserMessage("t1", "r1", "deploy please"))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer s.Close()

	drain(s)
	if s.Err() != nil {
		t.Fatalf("stream err: %v", s.Err())
	}

	res := s.Result()
	if res.Outcome() != agui.OutcomeInterrupt {
		t.Fatalf("outcome = %q, want interrupt", res.Outcome())
	}
	in, ok := res.Interrupt()
	if !ok {
		t.Fatal("Interrupt() ok = false, want true on an interrupt finish")
	}
	if in.InterruptID != "int-7" || in.Prompt != "Approve deploy?" {
		t.Fatalf("interrupt = %+v, want int-7/Approve deploy?", in)
	}
	if len(in.Choices) != 2 {
		t.Fatalf("interrupt choices = %d, want 2", len(in.Choices))
	}

	// The surfaced interrupt is the anchor for a resume: build a continuation
	// input from it and confirm it correlates back to the pending interrupt.
	resume := ResumeInput("t1", "r2", ResolveChoice(in.InterruptID, "yes", ""))
	if len(resume.Resume) != 1 || resume.Resume[0].InterruptID != "int-7" {
		t.Fatalf("resume = %+v, want one item for int-7", resume.Resume)
	}
}
