package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the multimodal half of the Responses request serializer:
// the Item content shapes, the vision switch, and the Files preflight's one
// surface-dependent rule. responses_test.go owns the text-only body.

// visionOn is what parseMultimodalConfig produces for a deployment that
// declares no multimodal: block.
func visionOn() multimodalConfig {
	return multimodalConfig{Vision: true, DefaultImageDetail: "auto"}
}

func responsesPlugin(cfg multimodalConfig) *Plugin {
	p := quietPlugin()
	p.multimodal = cfg
	return p
}

// firstItem returns the single input Item, failing when there isn't exactly one.
func firstItem(t *testing.T, p *Plugin, msgs ...events.Message) map[string]any {
	t.Helper()
	got := p.buildResponsesInput(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 Item, got %d: %v", len(got), got)
	}
	return got[0]
}

// contentParts pulls an Item's content out as a parts array.
func contentParts(t *testing.T, item map[string]any, key string) []map[string]any {
	t.Helper()
	parts, ok := item[key].([]map[string]any)
	if !ok {
		t.Fatalf("%s is %T (%v), want a content-parts array", key, item[key], item[key])
	}
	return parts
}

// --- the text-only shape stays a plain string -------------------------------

// The deliberate choice E4-S4 made: an Item with no Parts keeps the plain
// string content the API also accepts, rather than being restructured into a
// one-element array for uniformity's sake.
func TestResponsesInput_NoPartsStaysAPlainString(t *testing.T) {
	p := responsesPlugin(visionOn())
	for _, role := range []string{"user", "assistant", "system", "developer"} {
		item := firstItem(t, p, events.Message{Role: role, Content: "hi"})
		if _, ok := item["content"].(string); !ok {
			t.Errorf("role %s: content is %T, want a plain string", role, item["content"])
		}
	}
	item := firstItem(t, p, events.Message{Role: "tool", ToolCallID: "call_1", Content: "42"})
	if _, ok := item["output"].(string); !ok {
		t.Errorf("tool output is %T, want a plain string", item["output"])
	}
}

// --- image parts ------------------------------------------------------------

// Inline image bytes become a flat `input_image` with a data URL — flat, not
// the chat surface's nested image_url object.
func TestResponsesInput_ImageInlineBecomesInputImage(t *testing.T) {
	p := responsesPlugin(multimodalConfig{Vision: true, DefaultImageDetail: "high"})
	item := firstItem(t, p, events.Message{Role: "user", Content: "what is this", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: []byte{0x89, 0x50}},
	}})

	parts := contentParts(t, item, "content")
	if len(parts) != 2 {
		t.Fatalf("expected text + image, got %d parts: %v", len(parts), parts)
	}
	if parts[0]["type"] != "input_text" || parts[0]["text"] != "what is this" {
		t.Errorf("first part = %v, want an input_text carrying the message content", parts[0])
	}
	img := parts[1]
	if img["type"] != "input_image" {
		t.Fatalf("image part type = %v, want input_image", img["type"])
	}
	if img["detail"] != "high" {
		t.Errorf("detail = %v, want the configured high", img["detail"])
	}
	url, ok := img["image_url"].(string)
	if !ok {
		t.Fatalf("image_url is %T, want a flat string on this surface", img["image_url"])
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image_url = %q, want a data URL", url)
	}
	if _, nested := img["image_url"].(map[string]any); nested {
		t.Error("image_url must not be the chat surface's nested object")
	}
}

// The capability the chat surface does not have: an image referenced by the
// file id the Files preflight left behind.
func TestResponsesInput_ImageFileIDIsAccepted(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", FileID: "file-abc"},
	}})
	img := contentParts(t, item, "content")[0]
	if img["type"] != "input_image" || img["file_id"] != "file-abc" {
		t.Fatalf("image part = %v, want an input_image carrying file_id", img)
	}
	if _, ok := img["image_url"]; ok {
		t.Error("a file_id image must not also carry an image_url")
	}

	// The same part on the chat surface is still an error — the two surfaces
	// genuinely differ here, and that is why the preflight is surface-aware.
	if _, err := buildImagePart(events.MessagePart{Type: "image", FileID: "file-abc"}, "auto"); err == nil {
		t.Error("chat buildImagePart must still reject a bare file_id")
	}
}

// A plain URI passes through as the image_url, same as the chat surface.
func TestResponsesInput_ImageURIPassesThrough(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", URI: "https://example.test/cat.png"},
	}})
	img := contentParts(t, item, "content")[0]
	if img["image_url"] != "https://example.test/cat.png" {
		t.Fatalf("image_url = %v, want the URI passed through", img["image_url"])
	}
}

// --- file parts -------------------------------------------------------------

// The preflight's output shape: a bare file id on a flat input_file.
func TestResponsesInput_FileIDBecomesInputFile(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "user", Parts: []events.MessagePart{
		{Type: "file", MimeType: "application/pdf", FileID: "file-pdf"},
	}})
	f := contentParts(t, item, "content")[0]
	if f["type"] != "input_file" || f["file_id"] != "file-pdf" {
		t.Fatalf("file part = %v, want a flat input_file with file_id", f)
	}
	if _, nested := f["file"]; nested {
		t.Error("input_file must not carry the chat surface's nested file object")
	}
}

// Files disabled means the bytes still have to travel, so they ride inline.
func TestResponsesInput_FileInlineCarriesFileData(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "user", Parts: []events.MessagePart{
		{Type: "file", MimeType: "application/pdf", Data: []byte("%PDF-1.7")},
	}})
	f := contentParts(t, item, "content")[0]
	if f["type"] != "input_file" {
		t.Fatalf("type = %v, want input_file", f["type"])
	}
	if f["filename"] == "" {
		t.Error("inline file_data requires a filename alongside it")
	}
	data, _ := f["file_data"].(string)
	if !strings.HasPrefix(data, "data:application/pdf;base64,") {
		t.Errorf("file_data = %q, want a data URL", data)
	}
}

// A URI-only file is refused on both surfaces: nothing fetches it, so
// accepting it would send a file the model never sees.
func TestResponsesInput_FileURIIsRefusedOnBothSurfaces(t *testing.T) {
	part := events.MessagePart{Type: "file", MimeType: "application/pdf", URI: "https://example.test/x.pdf"}
	if _, err := buildResponsesFilePart(part); err == nil {
		t.Error("expected the Responses file part to refuse a URI")
	}
	if _, err := buildFilePart(part); err == nil {
		t.Error("expected the chat file part to refuse a URI")
	}
}

// --- role-dependent text type ----------------------------------------------

// Assistant Items carry produced text, typed output_text; everything else
// carries supplied text, typed input_text. Sending the wrong one is rejected.
func TestResponsesInput_AssistantTextIsOutputText(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "assistant", Content: "here", Parts: []events.MessagePart{
		{Type: "text", Text: "you go"},
	}})
	parts := contentParts(t, item, "content")
	for _, part := range parts {
		if part["type"] != "output_text" {
			t.Errorf("assistant part type = %v, want output_text", part["type"])
		}
	}
}

// An assistant Item accepts only text content, so an image on one degrades to
// the plain string rather than failing the turn.
func TestResponsesInput_AssistantImageDegradesToString(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "assistant", Content: "see attached", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: []byte{1}},
	}})
	if item["content"] != "see attached" {
		t.Fatalf("content = %v, want the plain string fallback", item["content"])
	}
}

// A malformed part must cost the part, not the turn.
func TestResponsesInput_BadPartFallsBackToString(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{Role: "user", Content: "hello", Parts: []events.MessagePart{
		{Type: "image"}, // no URI, Data or FileID
	}})
	if item["content"] != "hello" {
		t.Fatalf("content = %v, want the plain string fallback", item["content"])
	}
}

// --- system and tool Items --------------------------------------------------

// The system Item still goes through the prompt registry when it carries parts,
// and the registry's output is what lands in the first text part.
func TestResponsesInput_SystemPartsKeepThePromptRegistry(t *testing.T) {
	p := responsesPlugin(visionOn())
	p.prompts = engine.NewPromptRegistry()
	p.prompts.Register("extra", 10, func() string { return "appended" })

	item := firstItem(t, p, events.Message{Role: "system", Content: "base", Parts: []events.MessagePart{
		{Type: "text", Text: "extra"},
	}})
	parts := contentParts(t, item, "content")
	text, _ := parts[0]["text"].(string)
	if text == "base" || !strings.Contains(text, "base") {
		t.Fatalf("first system part = %v, want the registry-decorated content", parts[0])
	}
	if parts[0]["type"] != "input_text" {
		t.Errorf("system part type = %v, want input_text", parts[0]["type"])
	}
}

// A tool that returned an image puts a content-parts array on `output`, which
// is the Responses counterpart of the chat path's multimodal role:tool message.
func TestResponsesInput_ToolOutputCarriesParts(t *testing.T) {
	p := responsesPlugin(visionOn())
	item := firstItem(t, p, events.Message{
		Role: "tool", ToolCallID: "call_7", Content: "screenshot",
		Parts: []events.MessagePart{{Type: "image", MimeType: "image/png", Data: []byte{1}}},
	})
	if item["type"] != "function_call_output" || item["call_id"] != "call_7" {
		t.Fatalf("item = %v, want a function_call_output for call_7", item)
	}
	parts := contentParts(t, item, "output")
	if len(parts) != 2 || parts[1]["type"] != "input_image" {
		t.Fatalf("output parts = %v, want text + input_image", parts)
	}
}

// --- vision: false ----------------------------------------------------------

// The switch suppresses image parts on the Responses surface exactly as it does
// on the chat one, and leaves everything else on the turn.
func TestResponsesInput_VisionOffDropsImages(t *testing.T) {
	p := responsesPlugin(multimodalConfig{Vision: false, DefaultImageDetail: "auto"})
	item := firstItem(t, p, events.Message{Role: "user", Content: "look", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: []byte{1}},
		{Type: "text", Text: "and read"},
	}})
	parts := contentParts(t, item, "content")
	if len(parts) != 2 {
		t.Fatalf("expected the two texts, got %v", parts)
	}
	for _, part := range parts {
		if part["type"] != "input_text" {
			t.Errorf("part type = %v, want only input_text with vision off", part["type"])
		}
	}
}

// Nothing left to serialize means the plain string content, never an empty
// content array — which the API rejects outright.
func TestResponsesInput_VisionOffImageOnlyFallsBack(t *testing.T) {
	p := responsesPlugin(multimodalConfig{Vision: false, DefaultImageDetail: "auto"})
	item := firstItem(t, p, events.Message{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: []byte{1}},
	}})
	if _, ok := item["content"].(string); !ok {
		t.Fatalf("content is %T (%v), want the plain string fallback", item["content"], item["content"])
	}
}

// --- the Files preflight, which is the one surface-dependent rule -----------

// An inline image at or over the threshold is uploaded on the Responses
// surface and referenced by the id that comes back — the criterion the chat
// surface structurally cannot meet.
func TestPreuploadParts_ImagePart_UploadedOnResponses(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file-img","object":"file"}`))
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", UploadThreshold: 4, CacheUploads: true}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: make([]byte, 10)},
	}}}
	out, err := p.preuploadParts(context.Background(), msgs, apiResponses)
	if err != nil {
		t.Fatalf("preuploadParts: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upload, got %d", calls)
	}
	if out[0].Parts[0].FileID != "file-img" {
		t.Fatalf("FileID = %q, want file-img", out[0].Parts[0].FileID)
	}
	if len(out[0].Parts[0].Data) != 0 {
		t.Error("uploaded bytes should be released")
	}

	// And the id is what reaches the wire.
	p.multimodal = visionOn()
	item := firstItem(t, p, out[0])
	if contentParts(t, item, "content")[0]["file_id"] != "file-img" {
		t.Error("the uploaded id must be what the input_image references")
	}
}

// Under the threshold an image stays inline: a data URL costs one request, an
// upload costs two.
func TestPreuploadParts_ImagePart_UnderThresholdStaysInline(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", UploadThreshold: 4096, CacheUploads: true}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: make([]byte, 10)},
	}}}
	out, err := p.preuploadParts(context.Background(), msgs, apiResponses)
	if err != nil {
		t.Fatalf("preuploadParts: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no upload under the threshold, got %d", calls)
	}
	if len(out[0].Parts[0].Data) != 10 {
		t.Error("an under-threshold image must keep its bytes")
	}
}

// With vision off the image is going to be dropped anyway, so uploading it
// would burn a request and leave an orphan the operator has to reclaim.
func TestPreuploadParts_ImagePart_VisionOffSkipsUpload(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", UploadThreshold: 4, CacheUploads: true}
	p.multimodal = multimodalConfig{Vision: false, DefaultImageDetail: "auto"}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: make([]byte, 10)},
	}}}
	if _, err := p.preuploadParts(context.Background(), msgs, apiResponses); err != nil {
		t.Fatalf("preuploadParts: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no upload with vision off, got %d", calls)
	}
}

// File parts are threshold-independent and surface-independent: file_id is
// their canonical reference at any size, on both APIs. This is the
// "purpose + caching + tracking still work" half of the criterion.
func TestPreuploadParts_FilePart_SameOnBothSurfaces(t *testing.T) {
	for _, api := range []apiSurface{apiChatCompletions, apiResponses} {
		t.Run(string(api), func(t *testing.T) {
			var calls int32
			var gotPurpose string
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				_ = r.ParseMultipartForm(1 << 20)
				gotPurpose = r.FormValue("purpose")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"file-doc","object":"file"}`))
			})
			p, _ := newFilesTestPlugin(t, mux)
			p.files = filesConfig{Enabled: true, Purpose: "assistants", UploadThreshold: 1 << 30, CacheUploads: true}

			// Two identical parts: the second must be served from the cache.
			msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
				{Type: "file", MimeType: "application/pdf", Data: []byte("tiny")},
				{Type: "file", MimeType: "application/pdf", Data: []byte("tiny")},
			}}}
			out, err := p.preuploadParts(context.Background(), msgs, api)
			if err != nil {
				t.Fatalf("preuploadParts: %v", err)
			}
			if calls != 1 {
				t.Fatalf("expected 1 upload with caching on, got %d", calls)
			}
			if gotPurpose != "assistants" {
				t.Errorf("purpose = %q, want the configured assistants", gotPurpose)
			}
			for i, part := range out[0].Parts {
				if part.FileID != "file-doc" {
					t.Errorf("part %d FileID = %q, want file-doc", i, part.FileID)
				}
			}
			if ids := p.snapshotSessionFileIDs(); len(ids) != 1 || ids[0] != "file-doc" {
				t.Errorf("session-tracked ids = %v, want [file-doc] for delete_on_shutdown", ids)
			}
		})
	}
}

// --- parity -----------------------------------------------------------------

// The same multimodal request serializes on both `api:` values: each surface
// gets its own shape, both carry the image and the file, and neither drops
// content the other kept. Flipping `api:` must not cost capability — which is
// the whole point of E4-S4.
func TestMultimodalParity_BothSurfacesCarryImagesAndFiles(t *testing.T) {
	p := responsesPlugin(visionOn())
	p.auth = &authState{mode: authModeOpenAI}

	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages: []events.Message{{
			Role:    "user",
			Content: "summarise these",
			Parts: []events.MessagePart{
				{Type: "image", MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
				{Type: "file", MimeType: "application/pdf", FileID: "file-doc"},
			},
		}},
	}

	// Both bodies must be marshalable — the parity that actually reaches the
	// wire is JSON, not Go maps.
	chat := p.buildRequestBody("gpt-5.1", 512, req)
	resp := p.buildResponsesBody("gpt-5.1", 512, req)
	for name, body := range map[string]map[string]any{"chat": chat, "responses": resp} {
		if _, err := json.Marshal(body); err != nil {
			t.Fatalf("%s body does not marshal: %v", name, err)
		}
	}

	// Chat: nested image_url + nested file, under `messages`.
	msgs, ok := chat["messages"].([]map[string]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("chat messages = %v", chat["messages"])
	}
	chatParts, ok := msgs[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("chat content is %T, want a parts array", msgs[0]["content"])
	}
	chatTypes := partTypes(chatParts)
	if want := []string{"text", "image_url", "file"}; !equalStrings(chatTypes, want) {
		t.Fatalf("chat part types = %v, want %v", chatTypes, want)
	}

	// Responses: flat input_image + input_file, under `input`.
	respItems := items(t, resp)
	if len(respItems) != 1 {
		t.Fatalf("responses input = %v, want 1 Item", respItems)
	}
	respParts := contentParts(t, respItems[0], "content")
	respTypes := partTypes(respParts)
	if want := []string{"input_text", "input_image", "input_file"}; !equalStrings(respTypes, want) {
		t.Fatalf("responses part types = %v, want %v", respTypes, want)
	}

	// Same count, same order, same payloads — only the spelling differs.
	if len(chatParts) != len(respParts) {
		t.Fatalf("part counts differ: chat %d, responses %d", len(chatParts), len(respParts))
	}
	if chatParts[0]["text"] != respParts[0]["text"] {
		t.Errorf("text differs: %v vs %v", chatParts[0]["text"], respParts[0]["text"])
	}
	chatURL := chatParts[1]["image_url"].(map[string]any)["url"]
	if chatURL != respParts[1]["image_url"] {
		t.Errorf("image payload differs: %v vs %v", chatURL, respParts[1]["image_url"])
	}
	chatFileID := chatParts[2]["file"].(map[string]any)["file_id"]
	if chatFileID != respParts[2]["file_id"] {
		t.Errorf("file id differs: %v vs %v", chatFileID, respParts[2]["file_id"])
	}

	// And vision: false suppresses the image on both, identically.
	p.multimodal = multimodalConfig{Vision: false, DefaultImageDetail: "auto"}
	blindChat := p.buildRequestBody("gpt-5.1", 512, req)["messages"].([]map[string]any)[0]["content"].([]map[string]any)
	blindResp := contentParts(t, items(t, p.buildResponsesBody("gpt-5.1", 512, req))[0], "content")
	if want := []string{"text", "file"}; !equalStrings(partTypes(blindChat), want) {
		t.Errorf("chat with vision off = %v, want %v", partTypes(blindChat), want)
	}
	if want := []string{"input_text", "input_file"}; !equalStrings(partTypes(blindResp), want) {
		t.Errorf("responses with vision off = %v, want %v", partTypes(blindResp), want)
	}
}

func partTypes(parts []map[string]any) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s, _ := p["type"].(string)
		out = append(out, s)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
