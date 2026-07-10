package agui

import "fmt"

// ValidateSequence checks that a slice of events forms a well-formed AG-UI run:
// it must open with RunStarted and close with a terminal event (RunFinished or
// RunError), with no events outside that envelope. It also enforces basic
// nesting for streamed messages, tool calls, and steps.
//
// This is an optional helper for tests and conformance checking; the codec does
// not enforce ordering during encode/decode.
func ValidateSequence(events []Event) error {
	if len(events) == 0 {
		return fmt.Errorf("agui: empty event sequence")
	}
	if events[0].EventType() != EventRunStarted {
		return fmt.Errorf("agui: sequence must begin with RunStarted, got %s", events[0].EventType())
	}

	openMessages := map[string]bool{}
	openTools := map[string]bool{}
	stepDepth := 0
	terminated := false

	for i, e := range events {
		if terminated {
			return fmt.Errorf("agui: event %d (%s) after run termination", i, e.EventType())
		}
		switch e.EventType() {
		case EventRunStarted:
			if i != 0 {
				return fmt.Errorf("agui: unexpected RunStarted at position %d", i)
			}
		case EventRunFinished, EventRunError:
			terminated = true

		case EventStepStarted:
			stepDepth++
		case EventStepFinished:
			if stepDepth == 0 {
				return fmt.Errorf("agui: StepFinished at position %d without matching StepStarted", i)
			}
			stepDepth--

		case EventTextMessageStart:
			ev := e.(*TextMessageStartEvent)
			openMessages[ev.MessageID] = true
		case EventTextMessageContent:
			ev := e.(*TextMessageContentEvent)
			if !openMessages[ev.MessageID] {
				return fmt.Errorf("agui: TextMessageContent for unopened message %q at position %d", ev.MessageID, i)
			}
		case EventTextMessageEnd:
			ev := e.(*TextMessageEndEvent)
			if !openMessages[ev.MessageID] {
				return fmt.Errorf("agui: TextMessageEnd for unopened message %q at position %d", ev.MessageID, i)
			}
			delete(openMessages, ev.MessageID)

		case EventToolCallStart:
			ev := e.(*ToolCallStartEvent)
			openTools[ev.ToolCallID] = true
		case EventToolCallArgs:
			ev := e.(*ToolCallArgsEvent)
			if !openTools[ev.ToolCallID] {
				return fmt.Errorf("agui: ToolCallArgs for unopened tool call %q at position %d", ev.ToolCallID, i)
			}
		case EventToolCallEnd:
			ev := e.(*ToolCallEndEvent)
			if !openTools[ev.ToolCallID] {
				return fmt.Errorf("agui: ToolCallEnd for unopened tool call %q at position %d", ev.ToolCallID, i)
			}
			delete(openTools, ev.ToolCallID)
		}
	}

	if !terminated {
		return fmt.Errorf("agui: sequence must end with RunFinished or RunError")
	}
	if stepDepth != 0 {
		return fmt.Errorf("agui: %d step(s) left unfinished", stepDepth)
	}
	if len(openMessages) != 0 {
		return fmt.Errorf("agui: %d text message(s) left unclosed", len(openMessages))
	}
	if len(openTools) != 0 {
		return fmt.Errorf("agui: %d tool call(s) left unclosed", len(openTools))
	}
	return nil
}
