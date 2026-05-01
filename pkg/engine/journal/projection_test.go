package journal

import (
	"sync"
	"testing"
)

func TestSubscribeProjection_FiresInSeqOrder(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)

	var (
		mu   sync.Mutex
		seen []uint64
	)
	unsub := w.SubscribeProjection(nil, func(env Envelope) {
		mu.Lock()
		seen = append(seen, env.Seq)
		mu.Unlock()
	})
	defer unsub()

	for i := uint64(1); i <= 5; i++ {
		w.Append(&Envelope{Seq: i, Type: "any"})
	}
	mustClose(t, w)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 5 {
		t.Fatalf("projection saw %d envelopes, want 5", len(seen))
	}
	for i, s := range seen {
		if s != uint64(i+1) {
			t.Errorf("pos %d: seq=%d want %d", i, s, i+1)
		}
	}
}

func TestSubscribeProjection_FiltersByType(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)

	var mu sync.Mutex
	got := map[string]int{}
	unsub := w.SubscribeProjection([]string{"thinking.step"}, func(env Envelope) {
		mu.Lock()
		got[env.Type]++
		mu.Unlock()
	})
	defer unsub()

	w.Append(&Envelope{Seq: 1, Type: "thinking.step"})
	w.Append(&Envelope{Seq: 2, Type: "irrelevant"})
	w.Append(&Envelope{Seq: 3, Type: "thinking.step"})
	w.Append(&Envelope{Seq: 4, Type: "plan.progress"})
	mustClose(t, w)

	mu.Lock()
	defer mu.Unlock()
	if got["thinking.step"] != 2 {
		t.Errorf("thinking.step matches = %d, want 2", got["thinking.step"])
	}
	if got["irrelevant"] != 0 || got["plan.progress"] != 0 {
		t.Errorf("filter leaked: %v", got)
	}
}

func TestSubscribeProjection_Unsubscribe(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)

	var (
		mu  sync.Mutex
		cnt int
	)
	unsub := w.SubscribeProjection(nil, func(env Envelope) {
		mu.Lock()
		cnt++
		mu.Unlock()
	})

	w.Append(&Envelope{Seq: 1, Type: "x"})
	// Allow drain to fire the projection before we unsub.
	mustClose(t, w)
	unsub()

	mu.Lock()
	defer mu.Unlock()
	if cnt != 1 {
		t.Errorf("count = %d before unsub, want 1", cnt)
	}
}

func TestProjectFile_PostMortemWalk(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, FsyncNone)
	w.Append(&Envelope{Seq: 1, Type: "thinking.step", Payload: map[string]any{"content": "a"}})
	w.Append(&Envelope{Seq: 2, Type: "agent.turn.end"})
	w.Append(&Envelope{Seq: 3, Type: "thinking.step", Payload: map[string]any{"content": "b"}})
	w.Append(&Envelope{Seq: 4, Type: "plan.progress", Payload: map[string]any{"step_id": "s1"}})
	mustClose(t, w)

	var thinking []string
	err := ProjectFile(dir, []string{"thinking.step"}, func(env Envelope) {
		m, ok := env.Payload.(map[string]any)
		if !ok {
			return
		}
		if c, _ := m["content"].(string); c != "" {
			thinking = append(thinking, c)
		}
	})
	if err != nil {
		t.Fatalf("ProjectFile: %v", err)
	}
	if len(thinking) != 2 || thinking[0] != "a" || thinking[1] != "b" {
		t.Errorf("ProjectFile thinking content = %v", thinking)
	}
}
