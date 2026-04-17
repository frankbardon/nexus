package fanout

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestBuildJudgePrompt(t *testing.T) {
	responses := []events.LLMResponse{
		{
			Content: "Response from provider A",
			Model:   "model-a",
			Metadata: map[string]any{
				"_fanout_provider": "provider-a",
			},
		},
		{
			Content: "Response from provider B",
			Model:   "model-b",
			Metadata: map[string]any{
				"_fanout_provider": "provider-b",
			},
		},
		{
			Content: "Response from provider C",
			Model:   "model-c",
			Metadata: map[string]any{
				"_fanout_provider": "provider-c",
			},
		},
	}

	prompt := buildJudgePrompt(responses)

	// Verify all responses are listed with indices.
	for i, r := range responses {
		provider := r.Metadata["_fanout_provider"].(string)
		indexMarker := strings.Contains(prompt, "Response "+string(rune('0'+i)))
		if !indexMarker {
			t.Errorf("prompt missing index marker for response %d", i)
		}
		if !strings.Contains(prompt, provider) {
			t.Errorf("prompt missing provider %q", provider)
		}
		if !strings.Contains(prompt, r.Model) {
			t.Errorf("prompt missing model %q", r.Model)
		}
		if !strings.Contains(prompt, r.Content) {
			t.Errorf("prompt missing content for response %d", i)
		}
	}

	// Verify the prompt asks for JSON output.
	if !strings.Contains(prompt, "chosen_index") {
		t.Error("prompt missing chosen_index instruction")
	}
}

func TestBuildJudgePromptSingleResponse(t *testing.T) {
	responses := []events.LLMResponse{
		{
			Content:  "Only response",
			Model:    "model-x",
			Metadata: map[string]any{"_fanout_provider": "prov-x"},
		},
	}

	prompt := buildJudgePrompt(responses)
	if !strings.Contains(prompt, "Response 0") {
		t.Error("prompt missing index 0 for single response")
	}
	if !strings.Contains(prompt, "Only response") {
		t.Error("prompt missing response content")
	}
}

func TestParseJudgeResponse_Valid(t *testing.T) {
	tests := []struct {
		name    string
		content string
		n       int
		want    int
	}{
		{
			name:    "index 0",
			content: `{"chosen_index": 0, "reason": "more complete"}`,
			n:       3,
			want:    0,
		},
		{
			name:    "index 2",
			content: `{"chosen_index": 2, "reason": "better formatting"}`,
			n:       3,
			want:    2,
		},
		{
			name:    "with whitespace",
			content: `  {"chosen_index": 1, "reason": "clearer"}  `,
			n:       2,
			want:    1,
		},
		{
			name:    "markdown fenced",
			content: "```json\n{\"chosen_index\": 0, \"reason\": \"best\"}\n```",
			n:       2,
			want:    0,
		},
		{
			name:    "extra fields ignored",
			content: `{"chosen_index": 1, "reason": "good", "confidence": 0.9}`,
			n:       3,
			want:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jr, err := parseJudgeResponse(tt.content, tt.n)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if jr.ChosenIndex != tt.want {
				t.Errorf("got chosen_index=%d, want %d", jr.ChosenIndex, tt.want)
			}
			if jr.Reason == "" {
				t.Error("expected non-empty reason")
			}
		})
	}
}

func TestParseJudgeResponse_InvalidIndex(t *testing.T) {
	tests := []struct {
		name    string
		content string
		n       int
	}{
		{
			name:    "index too high",
			content: `{"chosen_index": 5, "reason": "oops"}`,
			n:       3,
		},
		{
			name:    "negative index",
			content: `{"chosen_index": -1, "reason": "oops"}`,
			n:       3,
		},
		{
			name:    "index equals count",
			content: `{"chosen_index": 2, "reason": "boundary"}`,
			n:       2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseJudgeResponse(tt.content, tt.n)
			if err == nil {
				t.Fatal("expected error for out-of-range index")
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Errorf("expected 'out of range' error, got: %v", err)
			}
		})
	}
}

func TestParseJudgeResponse_MalformedJSON(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "empty string",
			content: "",
		},
		{
			name:    "not json",
			content: "I choose response 0 because it is better.",
		},
		{
			name:    "incomplete json",
			content: `{"chosen_index": 1`,
		},
		{
			name:    "array instead of object",
			content: `[0, "reason"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseJudgeResponse(tt.content, 3)
			if err == nil {
				t.Fatal("expected error for malformed JSON")
			}
			if !strings.Contains(err.Error(), "parse judge JSON") {
				t.Errorf("expected 'parse judge JSON' error, got: %v", err)
			}
		})
	}
}
