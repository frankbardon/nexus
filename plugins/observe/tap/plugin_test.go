package tap

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_ObserveOnly(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{"bind": "127.0.0.1:0"}))
	if got := h.Plugin().Subscriptions(); len(got) != 0 {
		t.Errorf("Subscriptions() = %v, want empty (wildcard only)", got)
	}
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty", got)
	}
	h.Inject("io.output", map[string]any{"text": "hi"})
	h.InjectVetoable("before:tool.invoke", map[string]any{"name": "shell"})
	// InjectVetoable cannot tag its event as an inject, so the harness
	// reports it as plugin-origin; anything else would be the tap's own.
	for _, e := range h.PluginEmissions() {
		if e.Type != "before:tool.invoke" {
			t.Errorf("tap emitted %q, want nothing", e.Type)
		}
	}
	h.AssertNoUndeclaredEmissions()
}

// sseFrame is one parsed SSE frame.
type sseFrame struct {
	id    string
	event string
	data  string
}

type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
}

func dial(t *testing.T, url string, header http.Header) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	s := &sseStream{resp: resp, frames: make(chan sseFrame, 256)}
	if resp.StatusCode != http.StatusOK {
		return s
	}
	go func() {
		defer close(s.frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.data != "" || cur.event != "" {
					s.frames <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s
}

func (s *sseStream) next(t *testing.T) sseFrame {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			t.Fatal("stream closed")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for frame")
	}
	return sseFrame{}
}

// nextEnvelope skips control frames until an envelope arrives.
func (s *sseStream) nextEnvelope(t *testing.T) (journal.Envelope, sseFrame) {
	t.Helper()
	for {
		f := s.next(t)
		if f.event != "" {
			continue
		}
		var env journal.Envelope
		if err := json.Unmarshal([]byte(f.data), &env); err != nil {
			t.Fatalf("bad envelope %q: %v", f.data, err)
		}
		return env, f
	}
}

func boot(t *testing.T, cfg map[string]any, opts ...contract.ContractOption) (*contract.ContractHarness, string) {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["bind"] = "127.0.0.1:0"
	cfg["heartbeat_interval"] = "0"
	opts = append(opts, contract.WithPluginConfig(cfg))
	h := contract.NewContract(t, New, opts...)
	return h, "http://" + h.Plugin().(*Plugin).Addr()
}

func waitClients(t *testing.T, h *contract.ContractHarness, n int) {
	t.Helper()
	p := h.Plugin().(*Plugin)
	deadline := time.Now().Add(3 * time.Second)
	for p.hub.clientCount() != n {
		if time.Now().After(deadline) {
			t.Fatalf("clients = %d, want %d", p.hub.clientCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStream_LiveEnvelope(t *testing.T) {
	h, base := boot(t, nil)
	s := dial(t, base+eventsPath, nil)
	waitClients(t, h, 1)

	h.Inject("io.output", map[string]any{"text": "hello"})
	env, f := s.nextEnvelope(t)
	if env.Type != "io.output" || env.Seq == 0 || env.EventID == "" {
		t.Fatalf("envelope = %+v", env)
	}
	if p, _ := env.Payload.(map[string]any); p["text"] != "hello" {
		t.Errorf("payload = %v", env.Payload)
	}
	if !strings.HasPrefix(f.id, h.Plugin().(*Plugin).hub.epoch+":") {
		t.Errorf("id = %q, want epoch-prefixed cursor", f.id)
	}
}

func TestStream_VetoedEventReported(t *testing.T) {
	h, base := boot(t, nil)
	s := dial(t, base+eventsPath+"?types=before:*", nil)
	waitClients(t, h, 1)

	unsub := h.Bus().Subscribe("before:tool.invoke", func(e engine.Event[any]) {
		e.Payload.(*engine.VetoablePayload).Veto = engine.VetoResult{Vetoed: true, Reason: "nope"}
	})
	defer unsub()
	h.InjectVetoable("before:tool.invoke", map[string]any{"name": "shell"})

	env, _ := s.nextEnvelope(t)
	if env.Type != "before:tool.invoke" || !env.Vetoed || env.VetoReason != "nope" {
		t.Fatalf("envelope = %+v, want vetoed before:tool.invoke", env)
	}
	if p, _ := env.Payload.(map[string]any); p["name"] != "shell" {
		t.Errorf("payload not unwrapped from VetoablePayload: %v", env.Payload)
	}
}

func TestStream_ClientAndCeilingFilters(t *testing.T) {
	h, base := boot(t, map[string]any{"exclude_events": []any{"secret.*"}})
	s := dial(t, base+eventsPath+"?types=tool.*,secret.*&exclude=tool.result", nil)
	waitClients(t, h, 1)

	h.Inject("secret.thing", nil) // ceiling excludes
	h.Inject("tool.result", nil)  // client excludes
	h.Inject("io.output", nil)    // not in client include
	h.Inject("tool.invoke", nil)
	env, _ := s.nextEnvelope(t)
	if env.Type != "tool.invoke" {
		t.Fatalf("first streamed type = %q, want tool.invoke", env.Type)
	}
}

func TestStream_RingBackfillAndExactResume(t *testing.T) {
	h, base := boot(t, nil)
	h.Inject("a.one", nil)
	h.Inject("a.two", nil)

	s := dial(t, base+eventsPath+"?backfill=ring&types=a.*", nil)
	e1, f1 := s.nextEnvelope(t)
	e2, _ := s.nextEnvelope(t)
	if e1.Type != "a.one" || e2.Type != "a.two" {
		t.Fatalf("ring backfill = %s,%s", e1.Type, e2.Type)
	}

	// Resume from after a.one: must get a.two then live, nothing else.
	h.Inject("a.three", nil)
	r := dial(t, base+eventsPath+"?types=a.*", http.Header{"Last-Event-ID": {f1.id}})
	got := []string{}
	for range 2 {
		e, _ := r.nextEnvelope(t)
		got = append(got, e.Type)
	}
	if strings.Join(got, ",") != "a.two,a.three" {
		t.Fatalf("resume = %v, want a.two,a.three", got)
	}
}

func TestStream_ForeignEpochReportsGapThenBackfill(t *testing.T) {
	h, base := boot(t, nil)
	h.Inject("a.one", nil)
	s := dial(t, base+eventsPath+"?backfill=ring&types=a.*", http.Header{"Last-Event-ID": {"otherepoch:5"}})
	f := s.next(t)
	if f.event != ctlGap || !strings.Contains(f.data, "epoch_changed") {
		t.Fatalf("first frame = %+v, want gap/epoch_changed", f)
	}
	e, _ := s.nextEnvelope(t)
	if e.Type != "a.one" {
		t.Fatalf("after gap got %q, want ring backfill a.one", e.Type)
	}
}

func TestStream_JournalBackfillMergesRingWithoutDuplicates(t *testing.T) {
	h, base := boot(t, nil, contract.WithSession())

	// Journal on disk holds seqs 1..100 — the earlier part of the session.
	// Seq 100 is also what the ring will NOT hold, seq 101 will be in both.
	w, err := journal.NewWriter(filepath.Join(h.SessionDir(), "journal"), journal.WriterOptions{
		FsyncMode: journal.FsyncNone, SchemaVersion: journal.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 100; i++ {
		w.Append(&journal.Envelope{Seq: i, Type: "old.event", Ts: time.Now()})
	}

	h.Bus().(engine.SeqController).SetSeqFloor(100)
	h.Inject("new.event", nil) // seq 101, in the ring
	w.Append(&journal.Envelope{Seq: 101, Type: "new.event", Ts: time.Now()})
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	s := dial(t, base+eventsPath+"?backfill=journal&since_seq=95&types=old.*,new.*", nil)
	var seqs []uint64
	for len(seqs) < 6 {
		e, _ := s.nextEnvelope(t)
		seqs = append(seqs, e.Seq)
	}
	want := []uint64{96, 97, 98, 99, 100, 101}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("seqs = %v, want %v", seqs, want)
		}
	}
	// Live continues and seq 101 is not repeated.
	h.Inject("new.event", nil)
	e, _ := s.nextEnvelope(t)
	if e.Seq != 102 {
		t.Fatalf("next live seq = %d, want 102 (101 must not repeat)", e.Seq)
	}
}

func TestStream_JournalBackfillReportsUnjournaledGap(t *testing.T) {
	h, base := boot(t, nil, contract.WithSession())
	// Hub's first seq is 51, journal is empty: 1..50 are lost to both.
	w, err := journal.NewWriter(filepath.Join(h.SessionDir(), "journal"), journal.WriterOptions{
		FsyncMode: journal.FsyncNone, SchemaVersion: journal.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Bus().(engine.SeqController).SetSeqFloor(50)
	h.Inject("x.event", nil)

	s := dial(t, base+eventsPath+"?backfill=journal&types=x.*", nil)
	var gap sseFrame
	for {
		f := s.next(t)
		if f.event == ctlGap {
			gap = f
			break
		}
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(gap.data), &body)
	if body["reason"] != "not_yet_journaled" || body["after_seq"] != float64(0) || body["through_seq"] != float64(50) {
		t.Fatalf("gap = %v, want not_yet_journaled after 0 through 50", body)
	}
	e, _ := s.nextEnvelope(t)
	if e.Seq != 51 {
		t.Fatalf("ring frame seq = %d, want 51", e.Seq)
	}
}

func TestStream_OverflowFrameNamesLastID(t *testing.T) {
	h, base := boot(t, nil)
	s := dial(t, base+eventsPath+"?types=a.*", nil)
	waitClients(t, h, 1)
	h.Inject("a.one", nil)
	_, f := s.nextEnvelope(t)

	p := h.Plugin().(*Plugin)
	p.hub.mu.Lock()
	for c := range p.hub.clients {
		p.hub.dropLocked(c, dropOverflow)
	}
	p.hub.mu.Unlock()

	ctl := s.next(t)
	if ctl.event != ctlOverflow || !strings.Contains(ctl.data, f.id) {
		t.Fatalf("frame = %+v, want overflow naming %s", ctl, f.id)
	}
}

func TestStream_Auth(t *testing.T) {
	_, base := boot(t, map[string]any{"auth": map[string]any{
		"validators": []any{map[string]any{
			"type":   "static",
			"tokens": []any{map[string]any{"token": "t0k", "principal": "consumer"}},
		}},
	}})
	if s := dial(t, base+eventsPath, nil); s.resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", s.resp.StatusCode)
	}
	if s := dial(t, base+eventsPath, http.Header{"Authorization": {"Bearer t0k"}}); s.resp.StatusCode != http.StatusOK {
		t.Errorf("good token: status %d, want 200", s.resp.StatusCode)
	}
	// healthz stays open.
	resp, err := http.Get(base + healthzPath)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %v %v", resp, err)
	}
	if resp != nil {
		resp.Body.Close()
	}
}

func TestStream_BadBackfill(t *testing.T) {
	_, base := boot(t, nil)
	if s := dial(t, base+eventsPath+"?backfill=all", nil); s.resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", s.resp.StatusCode)
	}
}
