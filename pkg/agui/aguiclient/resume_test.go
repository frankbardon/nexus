package aguiclient

import (
	"encoding/json"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
)

// TestResumeInput_CarriesItems asserts ResumeInput builds a continuation input
// with the resume[] items and no messages, and that it round-trips through the
// wire encoder the server decodes.
func TestResumeInput_CarriesItems(t *testing.T) {
	in := ResumeInput("thread-1", "run-2",
		ResolveChoice("int-1", "staging", ""),
	)
	if in.ThreadID != "thread-1" || in.RunID != "run-2" {
		t.Fatalf("thread/run = %q/%q, want thread-1/run-2", in.ThreadID, in.RunID)
	}
	if len(in.Messages) != 0 {
		t.Fatalf("messages = %d, want 0 on a resume", len(in.Messages))
	}
	if len(in.Resume) != 1 || in.Resume[0].InterruptID != "int-1" {
		t.Fatalf("resume = %+v, want one item for int-1", in.Resume)
	}

	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := agui.DecodeRunAgentInput(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back.Resume) != 1 || back.Resume[0].Status != agui.ResumeResolved {
		t.Fatalf("decoded resume = %+v, want one resolved item", back.Resume)
	}
}

func TestResolveChoice_Payload(t *testing.T) {
	item := ResolveChoice("int-1", "prod", "with caution")
	if item.Status != agui.ResumeResolved {
		t.Fatalf("status = %q, want resolved", item.Status)
	}
	var got map[string]string
	if err := json.Unmarshal(item.Payload, &got); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if got["choiceId"] != "prod" || got["freeText"] != "with caution" {
		t.Fatalf("payload = %v, want choiceId=prod freeText=with caution", got)
	}
}

func TestResolveText_Payload(t *testing.T) {
	item := ResolveText("int-1", "acme-project")
	var got map[string]string
	if err := json.Unmarshal(item.Payload, &got); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if got["freeText"] != "acme-project" {
		t.Fatalf("payload = %v, want freeText=acme-project", got)
	}
	if _, has := got["choiceId"]; has {
		t.Fatalf("payload = %v, want no choiceId for a free-text resolve", got)
	}
}

func TestResolveToolResult_Payload(t *testing.T) {
	item := ResolveToolResult("int-1", "sunny", "")
	var got map[string]string
	if err := json.Unmarshal(item.Payload, &got); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if got["output"] != "sunny" {
		t.Fatalf("payload = %v, want output=sunny", got)
	}
	if _, has := got["error"]; has {
		t.Fatalf("payload = %v, want no error on a successful tool result", got)
	}
}

func TestCancel_NoPayload(t *testing.T) {
	item := Cancel("int-1")
	if item.Status != agui.ResumeCancelled {
		t.Fatalf("status = %q, want cancelled", item.Status)
	}
	if len(item.Payload) != 0 {
		t.Fatalf("payload = %s, want empty for a cancel", item.Payload)
	}
}

// TestResult_Interrupt asserts the decoded Result surfaces the interrupt payload
// of a RunFinished(interrupt) and reports its outcome, and that a normal finish
// yields no interrupt.
func TestResult_Interrupt(t *testing.T) {
	interrupted := Result{Events: []agui.Event{
		&agui.RunStartedEvent{},
		func() *agui.RunFinishedEvent {
			ev := agui.NewRunFinishedInterrupt("t1", "r1", agui.Interrupt{
				InterruptID: "int-9",
				Prompt:      "Approve?",
				Mode:        agui.InterruptModeChoices,
			})
			return &ev
		}(),
	}}
	in, ok := interrupted.Interrupt()
	if !ok {
		t.Fatal("Interrupt() ok = false, want true for an interrupt finish")
	}
	if in.InterruptID != "int-9" || in.Prompt != "Approve?" {
		t.Fatalf("interrupt = %+v, want int-9/Approve?", in)
	}
	if interrupted.Outcome() != agui.OutcomeInterrupt {
		t.Fatalf("outcome = %q, want interrupt", interrupted.Outcome())
	}

	normal := Result{Events: []agui.Event{
		func() *agui.RunFinishedEvent { ev := agui.NewRunFinished("t1", "r2"); return &ev }(),
	}}
	if _, ok := normal.Interrupt(); ok {
		t.Fatal("Interrupt() ok = true on a normal finish, want false")
	}
	if normal.Outcome() != "" {
		t.Fatalf("outcome = %q, want empty on a normal finish", normal.Outcome())
	}
}
