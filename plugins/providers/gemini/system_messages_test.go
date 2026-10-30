package gemini

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// Gemini's systemInstruction is one text, so every system message — leading
// or mid-conversation — is folded into it in order, blank-line separated, and
// none is dropped or left in contents (GitHub #205).
func TestConvertMessages_AllSystemMessagesInOrder(t *testing.T) {
	msgs := []events.Message{
		{Role: "system", Content: "agent prompt"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "system", Content: "summary so far"},
		{Role: "user", Content: "next"},
	}
	sys, contents, err := (&Plugin{}).convertMessages(msgs)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if want := "agent prompt\n\nsummary so far"; sys != want {
		t.Errorf("system = %q, want %q", sys, want)
	}
	if len(contents) != 3 {
		t.Fatalf("contents: want 3 non-system turns, got %d", len(contents))
	}
}
