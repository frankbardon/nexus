package gemini

import (
	"encoding/base64"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestBuildParts_TextOnly(t *testing.T) {
	got, err := buildParts(events.Message{Role: "user", Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["text"] != "hello" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestBuildParts_InlineImage(t *testing.T) {
	data := []byte("PNGDATA")
	got, err := buildParts(events.Message{
		Role: "user",
		Parts: []events.MessagePart{{
			Type:     "image",
			MimeType: "image/png",
			Data:     data,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	inline, ok := got[0]["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("expected inlineData, got %+v", got[0])
	}
	if inline["mimeType"] != "image/png" {
		t.Fatalf("mime: %v", inline["mimeType"])
	}
	if inline["data"] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("data not base64-encoded")
	}
}

func TestBuildParts_FileURI(t *testing.T) {
	got, err := buildParts(events.Message{
		Role: "user",
		Parts: []events.MessagePart{{
			Type:     "file",
			MimeType: "application/pdf",
			URI:      "https://generativelanguage.googleapis.com/v1beta/files/abc",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	fd, ok := got[0]["fileData"].(map[string]any)
	if !ok {
		t.Fatalf("expected fileData, got %+v", got[0])
	}
	if fd["fileUri"] != "https://generativelanguage.googleapis.com/v1beta/files/abc" {
		t.Fatalf("uri mismatch: %v", fd)
	}
}

func TestBuildParts_TextPlusImage(t *testing.T) {
	got, err := buildParts(events.Message{
		Role:    "user",
		Content: "describe this:",
		Parts: []events.MessagePart{{
			Type:     "image",
			MimeType: "image/jpeg",
			Data:     []byte("JPEG"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d", len(got))
	}
	if got[0]["text"] != "describe this:" {
		t.Fatalf("first part should be content text")
	}
	if _, ok := got[1]["inlineData"]; !ok {
		t.Fatalf("second part should be inlineData")
	}
}

func TestBuildParts_MissingMime(t *testing.T) {
	_, err := buildParts(events.Message{
		Role: "user",
		Parts: []events.MessagePart{{
			Type: "image",
			Data: []byte("X"),
		}},
	})
	if err == nil {
		t.Fatal("expected error for missing mime_type")
	}
}

func TestBuildParts_OversizeInline(t *testing.T) {
	big := make([]byte, inlineDataLimit+1)
	_, err := buildParts(events.Message{
		Role: "user",
		Parts: []events.MessagePart{{
			Type:     "image",
			MimeType: "image/png",
			Data:     big,
		}},
	})
	if err == nil {
		t.Fatal("expected error for oversize inline data")
	}
}

func TestBuildParts_UnsupportedType(t *testing.T) {
	_, err := buildParts(events.Message{
		Role:  "user",
		Parts: []events.MessagePart{{Type: "weird"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown part type")
	}
}
