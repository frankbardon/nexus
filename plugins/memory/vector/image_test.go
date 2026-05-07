package vector

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// imageStubBus wires a Plugin with a stub multimodal embeddings.provider
// that returns a 4-dim vector for every input, recording the inputs it
// received so tests can assert image bytes flowed through.
func imageStubBus(t *testing.T, storeImages bool) (*Plugin, engine.EventBus, *[]events.EmbeddingsRequest, *[]events.VectorUpsert) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.namespace = "test-text-ns"
	p.storeImages = storeImages

	var embedReqs []events.EmbeddingsRequest
	bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(*events.EmbeddingsRequest)
		if !ok {
			return
		}
		// Snapshot the request before we mutate it for assertions.
		snapshot := *req
		embedReqs = append(embedReqs, snapshot)
		req.Vectors = [][]float32{{0.1, 0.2, 0.3, 0.4}}
		req.Provider = "test.embedder"
	}, engine.WithPriority(50))

	var upserts []events.VectorUpsert
	bus.Subscribe("vector.upsert", func(ev engine.Event[any]) {
		if up, ok := ev.Payload.(*events.VectorUpsert); ok {
			upserts = append(upserts, *up)
		}
	}, engine.WithPriority(100))

	return p, bus, &embedReqs, &upserts
}

func TestStoreImages_OffByDefault(t *testing.T) {
	p, _, embedReqs, upserts := imageStubBus(t, false)

	in := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "look at this picture",
		Files: []events.FileAttachment{
			{Name: "cat.png", MimeType: "image/png", Data: []byte{0x01, 0x02}},
		},
	}
	p.handleInput(engine.Event[any]{Payload: in})

	// Only the text path should run when store_images is false: a single
	// embeddings.request for the user content (recall / autoStore are off
	// by default, so no upsert happens).
	if len(*embedReqs) != 1 {
		t.Fatalf("expected 1 embed request (text recall), got %d", len(*embedReqs))
	}
	if len((*embedReqs)[0].Texts) != 1 || (*embedReqs)[0].Texts[0] != "look at this picture" {
		t.Errorf("expected text recall request; got %+v", (*embedReqs)[0])
	}
	for _, r := range *embedReqs {
		if len(r.Inputs) > 0 {
			t.Errorf("store_images=false: should not produce image Inputs; got %+v", r.Inputs)
		}
	}
	if len(*upserts) != 0 {
		t.Errorf("store_images=false: should not upsert; got %d upserts", len(*upserts))
	}
}

func TestStoreImages_OnEmbedsAndUpserts(t *testing.T) {
	p, _, embedReqs, upserts := imageStubBus(t, true)

	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	in := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "check the image",
		SessionID:     "sess-1",
		Files: []events.FileAttachment{
			{Name: "diagram.png", MimeType: "image/png", Data: pngBytes},
			{Name: "notes.txt", MimeType: "text/plain", Data: []byte("ignore me")},
		},
	}
	p.handleInput(engine.Event[any]{Payload: in})

	// Two embed requests: one for the image (Inputs path), one for the
	// text content (Texts path).
	if len(*embedReqs) != 2 {
		t.Fatalf("expected 2 embed requests (image + text), got %d", len(*embedReqs))
	}
	imgIdx := -1
	for i, r := range *embedReqs {
		if len(r.Inputs) == 1 && r.Inputs[0].Image != nil {
			imgIdx = i
			break
		}
	}
	if imgIdx < 0 {
		t.Fatalf("no image embed request found; got %+v", *embedReqs)
	}
	imgReq := (*embedReqs)[imgIdx]
	if string(imgReq.Inputs[0].Image) != string(pngBytes) {
		t.Errorf("image bytes mismatch")
	}
	if imgReq.Inputs[0].MimeType != "image/png" {
		t.Errorf("image mime mismatch: %q", imgReq.Inputs[0].MimeType)
	}

	// One vector.upsert into the image namespace.
	if len(*upserts) != 1 {
		t.Fatalf("expected 1 image upsert, got %d", len(*upserts))
	}
	up := (*upserts)[0]
	if up.Namespace != "test-text-ns-images" {
		t.Errorf("namespace mismatch: %q", up.Namespace)
	}
	if len(up.Docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(up.Docs))
	}
	doc := up.Docs[0]
	if len(doc.Vector) != 4 {
		t.Errorf("vector dim mismatch: %d", len(doc.Vector))
	}
	if doc.Metadata["source"] != "user_image" {
		t.Errorf("source metadata: %q", doc.Metadata["source"])
	}
	if doc.Metadata["file_name"] != "diagram.png" {
		t.Errorf("file_name metadata: %q", doc.Metadata["file_name"])
	}
	if doc.Metadata["mime_type"] != "image/png" {
		t.Errorf("mime_type metadata: %q", doc.Metadata["mime_type"])
	}
	if doc.Metadata["session"] != "sess-1" {
		t.Errorf("session metadata: %q", doc.Metadata["session"])
	}
	if !strings.Contains(doc.Content, "diagram.png") {
		t.Errorf("descriptor should reference filename; got %q", doc.Content)
	}
}

func TestStoreImages_CustomNamespace(t *testing.T) {
	p, _, _, upserts := imageStubBus(t, true)
	p.imageNamespace = "shared/team-a-images"

	in := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "image please",
		Files: []events.FileAttachment{
			{Name: "x.jpg", MimeType: "image/jpeg", Data: []byte{0x01}},
		},
	}
	p.handleInput(engine.Event[any]{Payload: in})

	if len(*upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(*upserts))
	}
	if (*upserts)[0].Namespace != "shared/team-a-images" {
		t.Errorf("expected custom namespace, got %q", (*upserts)[0].Namespace)
	}
}

func TestStoreImages_RejectionLogsAndContinues(t *testing.T) {
	// Stub embeddings.provider that rejects images (mimicking text-only
	// adapters such as nexus.embeddings.openai). The text recall path
	// should still complete.
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.namespace = "test-text-ns"
	p.storeImages = true

	bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(*events.EmbeddingsRequest)
		if !ok {
			return
		}
		if len(req.Inputs) > 0 {
			req.Error = "openai text-embedding-* models accept text only"
			req.Provider = "stub"
			return
		}
		req.Vectors = [][]float32{{0.5, 0.5, 0.5, 0.5}}
		req.Provider = "stub"
	}, engine.WithPriority(50))

	var upserts []events.VectorUpsert
	bus.Subscribe("vector.upsert", func(ev engine.Event[any]) {
		if up, ok := ev.Payload.(*events.VectorUpsert); ok {
			upserts = append(upserts, *up)
		}
	}, engine.WithPriority(100))

	in := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "what about this?",
		Files: []events.FileAttachment{
			{Name: "x.png", MimeType: "image/png", Data: []byte{0x01, 0x02}},
		},
	}
	p.handleInput(engine.Event[any]{Payload: in})

	// Image rejection must NOT produce an upsert.
	for _, u := range upserts {
		for _, d := range u.Docs {
			if d.Metadata["source"] == "user_image" {
				t.Errorf("rejected image should not upsert: %+v", d)
			}
		}
	}
}
