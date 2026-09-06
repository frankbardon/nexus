package fileio

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// write_file used to publish its path relative to the session's files/
// directory rather than to the session root, so it announced "report.txt"
// where every other emitter announces "files/report.txt" — two object keys for
// one file, as far as anything syncing the tree is concerned. It also predated
// the offset / bytes_added delta. Both are now the workspace's job.
func TestWriteFile_AnnouncesSessionRelativePathWithDelta(t *testing.T) {
	bus := engine.NewEventBus()
	ws, err := engine.NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	var (
		mu   sync.Mutex
		seen []string
	)
	record := func(ev engine.Event[any]) {
		data, _ := ev.Payload.(map[string]any)
		path, _ := data["path"].(string)
		size, _ := data["size"].(int)
		offset, _ := data["offset"].(int)
		added, _ := data["bytes_added"].(int)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf("%s %s size=%d offset=%d added=%d",
			ev.Type, path, size, offset, added))
	}
	bus.Subscribe("session.file.created", record)
	bus.Subscribe("session.file.updated", record)

	p := New()
	if err := p.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: ws,
		DataDir: ws.PluginDir("nexus.tool.fileio"),
		Replay:  engine.NewReplayState(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}

	for i, content := range []string{"hello", "hello again"} {
		_ = bus.Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            fmt.Sprintf("w%d", i),
			Name:          "write_file",
			Arguments:     map[string]any{"path": "report.txt", "content": content},
		})
	}

	want := []string{
		"session.file.created files/report.txt size=5 offset=0 added=5",
		"session.file.updated files/report.txt size=11 offset=0 added=11",
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("emissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("emission %d = %q, want %q", i, got[i], want[i])
		}
	}
}
