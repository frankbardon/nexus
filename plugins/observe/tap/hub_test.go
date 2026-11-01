package tap

import (
	"testing"
	"time"
)

func TestHub_CursorIsArrivalOrderNotSeq(t *testing.T) {
	h := newHub("e", 8)
	// Child (seq 11) arrives before parent (seq 10), as nested emits do.
	h.publish(&frame{seq: 11, typ: "child"})
	h.publish(&frame{seq: 10, typ: "parent"})
	snap := h.snapshotLocked()
	if snap[0].cursor != 1 || snap[0].seq != 11 || snap[1].cursor != 2 || snap[1].seq != 10 {
		t.Fatalf("ring = %+v, want cursors 1,2 in arrival order", snap)
	}
	if h.minSeq != 10 {
		t.Errorf("minSeq = %d, want 10", h.minSeq)
	}
}

func TestHub_RingEvictionTracksEvictedSeq(t *testing.T) {
	h := newHub("e", 3)
	for i := uint64(1); i <= 5; i++ {
		h.publish(&frame{seq: i, typ: "x"})
	}
	snap := h.snapshotLocked()
	if len(snap) != 3 || snap[0].seq != 3 || snap[2].seq != 5 {
		t.Fatalf("snapshot seqs = %v, want 3,4,5", seqsOf(snap))
	}
	if h.evictedMaxSeq != 2 {
		t.Errorf("evictedMaxSeq = %d, want 2", h.evictedMaxSeq)
	}
}

func TestRegistration_CanResume(t *testing.T) {
	h := newHub("e", 3)
	for i := uint64(1); i <= 5; i++ {
		h.publish(&frame{seq: i, typ: "x"})
	}
	reg, _ := h.register(typeFilter{}, 4)
	// Ring holds cursors 3,4,5.
	cases := map[uint64]bool{1: false, 2: true, 3: true, 5: true, 6: false}
	for cursor, want := range cases {
		if got := reg.canResume(cursor); got != want {
			t.Errorf("canResume(%d) = %v, want %v", cursor, got, want)
		}
	}
}

func TestHub_SlowClientDroppedNotBlocking(t *testing.T) {
	h := newHub("e", 16)
	reg, _ := h.register(typeFilter{}, 2)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			h.publish(&frame{seq: uint64(i + 1), typ: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a full client queue")
	}
	select {
	case <-reg.client.gone:
	default:
		t.Fatal("client not dropped on overflow")
	}
	if reg.client.dropReason != dropOverflow {
		t.Errorf("dropReason = %q, want %q", reg.client.dropReason, dropOverflow)
	}
	if h.clientCount() != 0 {
		t.Errorf("clientCount = %d, want 0", h.clientCount())
	}
}

func TestHub_ClientFilterAppliedBeforeQueue(t *testing.T) {
	h := newHub("e", 16)
	reg, _ := h.register(typeFilter{include: []string{"llm.*"}}, 1)
	// Two non-matching events must not consume the single queue slot.
	h.publish(&frame{seq: 1, typ: "tool.invoke"})
	h.publish(&frame{seq: 2, typ: "tool.result"})
	h.publish(&frame{seq: 3, typ: "llm.request"})
	f := <-reg.client.ch
	if f.typ != "llm.request" {
		t.Fatalf("got %q, want llm.request", f.typ)
	}
}

func TestHub_CloseDropsWithShutdown(t *testing.T) {
	h := newHub("e", 4)
	reg, _ := h.register(typeFilter{}, 4)
	h.close()
	<-reg.client.gone
	if reg.client.dropReason != dropShutdown {
		t.Errorf("dropReason = %q, want %q", reg.client.dropReason, dropShutdown)
	}
	if _, ok := h.register(typeFilter{}, 4); ok {
		t.Error("register after close succeeded")
	}
}

func TestTypeFilter(t *testing.T) {
	f := typeFilter{include: []string{"llm.*", "tool.invoke"}, exclude: []string{"llm.stream.*"}}
	cases := map[string]bool{
		"llm.request":      true,
		"llm.stream.chunk": false,
		"tool.invoke":      true,
		"tool.result":      false,
	}
	for typ, want := range cases {
		if got := f.match(typ); got != want {
			t.Errorf("match(%q) = %v, want %v", typ, got, want)
		}
	}
	if !(typeFilter{}).match("anything") {
		t.Error("empty filter must admit everything")
	}
	if (typeFilter{exclude: []string{"*"}}).match("x") {
		t.Error(`exclude "*" must reject everything`)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.bind != defaultBind || cfg.ringSize != defaultRingSize || cfg.clientBuffer != defaultClientBuffer ||
		cfg.heartbeat != defaultHeartbeatInterval || cfg.chain.Enabled() {
		t.Errorf("defaults = %+v", cfg)
	}

	bad := []map[string]any{
		{"ring_size": 0},
		{"client_buffer": "x"},
		{"heartbeat_interval": "soon"},
		{"include_events": "llm.*"},
		{"auth": "token"},
		{"auth": map[string]any{"bogus": true}},
	}
	for _, raw := range bad {
		if _, err := parseConfig(raw); err == nil {
			t.Errorf("parseConfig(%v) succeeded, want error", raw)
		}
	}
}

func seqsOf(fs []*frame) []uint64 {
	out := make([]uint64, len(fs))
	for i, f := range fs {
		out[i] = f.seq
	}
	return out
}
