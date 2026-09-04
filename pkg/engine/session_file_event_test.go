package engine

// The guard on the session.file.* payload shape.
//
// E2-S4 recorded that nothing asserted an emitter publishes a session-relative
// path plus session_id / size / offset / bytes_added, and nominated E3-S5 as
// the home for it. The gap was real and it had a live instance: until this
// file existed, cmd/desktop/internal/matcher emitted session.file.created with
// an *absolute* path, an "action" key nothing reads, and none of the other four
// keys. A subscriber keyed on `path` would have written it under a key no other
// emitter can produce, and the object-store seam could not have used it at all.
//
// `make check-events` could not catch this on its own. The payload is a
// map[string]any, and events.SessionFile used to be a struct nobody emitted —
// so the lint saw a type no subscriber could receive and a map no type
// declared. sessionFileEvent now builds the map from events.SessionFile.Map, so
// renaming or retyping a field does trip check-events; what the lint still
// cannot see is whether an emitter goes through that helper at all, or what it
// passes. That is the whole reason this guard has to be a test.
//
// Two halves, both required:
//
//  1. Shape. Every engine emit path is driven and its payload checked against
//     the contract in docs/src/events/reference.md. A guard that only forbade
//     new emitters would not have noticed the existing ones drifting.
//  2. Provenance. A source scan requiring every session.file.* emit outside the
//     workspace to go through AnnounceWrite / AnnounceAppend. Centralising the
//     payload in sessionFileEvent makes a *partial* shape impossible to write
//     by accident; this is what makes it impossible to bypass on purpose.
//
// BLIND SPOTS, recorded because a guard whose limits are undocumented gets
// trusted past them, and matching the ones session_writers_enforce_test.go
// records for its own scan:
//   - The scan matches the literal event type in an Emit call. An emit through
//     a variable event type, a helper in another package, or a reflected
//     dispatch is invisible.
//   - Only plugins/ and cmd/ are scanned. pkg/ is not: the workspace itself is
//     the sanctioned emitter and would be its own violation.

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sessionFileEventKeys is the payload contract, in one place. Adding a key here
// without adding it to sessionFileEvent, or the other way round, fails the
// shape test below.
//
// _schema_version joined the four original keys when the payload started being
// built from events.SessionFile.Map. It is additive — every subscriber reads
// keys by name — and it brings session.file.* into line with every other event
// in pkg/events, which have carried a version since they were structs.
var sessionFileEventKeys = []string{"_schema_version", "session_id", "path", "size", "offset", "bytes_added"}

// checkSessionFileEvent asserts one payload against the contract.
func checkSessionFileEvent(t *testing.T, what string, sessionID string, payload any, wantPath string, wantSize, wantOffset, wantAdded int) {
	t.Helper()
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("%s: payload is %T, want map[string]any — the wire shape every subscriber type-asserts", what, payload)
	}
	for _, key := range sessionFileEventKeys {
		if _, present := m[key]; !present {
			t.Errorf("%s: payload is missing %q; a subscriber cannot tell a missing key from a whole-object rewrite. got %v",
				what, key, sortedMapKeys(m))
		}
	}
	if len(m) != len(sessionFileEventKeys) {
		t.Errorf("%s: payload carries %v, want exactly %v", what, sortedMapKeys(m), sessionFileEventKeys)
	}
	if got, _ := m["session_id"].(string); got != sessionID {
		t.Errorf("%s: session_id = %v, want %q", what, m["session_id"], sessionID)
	}
	got, isString := m["path"].(string)
	if !isString {
		t.Fatalf("%s: path is %T, want string", what, m["path"])
	}
	if got != wantPath {
		t.Errorf("%s: path = %q, want %q", what, got, wantPath)
	}
	if filepath.IsAbs(got) {
		t.Errorf("%s: path %q is absolute; it must be session-relative so it can be used directly as an object key", what, got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("%s: path %q contains a backslash; it must be slash-separated on every OS", what, got)
	}
	for label, pair := range map[string][2]int{
		"size":        {intOf(t, what, "size", m["size"]), wantSize},
		"offset":      {intOf(t, what, "offset", m["offset"]), wantOffset},
		"bytes_added": {intOf(t, what, "bytes_added", m["bytes_added"]), wantAdded},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: %s = %d, want %d", what, label, pair[0], pair[1])
		}
	}
	// The delta contract: [offset, offset+bytes_added) is the region that
	// changed, so it must end at the file's new length.
	if o, a, sz := intOf(t, what, "offset", m["offset"]), intOf(t, what, "bytes_added", m["bytes_added"]), intOf(t, what, "size", m["size"]); o+a != sz {
		t.Errorf("%s: offset(%d) + bytes_added(%d) = %d, want size(%d) — the delta must reach the end of the object",
			what, o, a, o+a, sz)
	}
}

func intOf(t *testing.T, what, key string, v any) int {
	t.Helper()
	n, ok := v.(int)
	if !ok {
		t.Fatalf("%s: %s is %T, want int", what, key, v)
	}
	return n
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every engine emit path, driven for real, checked against the contract.
func TestSessionFileEventsCarryTheDeclaredShape(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	session := eng.Session

	type seen struct {
		typ     string
		payload any
	}
	var events []seen
	eng.Bus.SubscribeAll(func(ev Event[any]) {
		if ev.Type == "session.file.created" || ev.Type == "session.file.updated" {
			events = append(events, seen{typ: ev.Type, payload: ev.Payload})
		}
	})

	take := func(t *testing.T) seen {
		t.Helper()
		if len(events) == 0 {
			t.Fatal("no session.file.* event was emitted")
		}
		ev := events[len(events)-1]
		events = nil
		return ev
	}

	t.Run("WriteFile", func(t *testing.T) {
		body := []byte("hello world")
		if err := session.WriteFile("files/report.md", body); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ev := take(t)
		if ev.typ != "session.file.created" {
			t.Errorf("event type = %q, want session.file.created for a new path", ev.typ)
		}
		checkSessionFileEvent(t, "WriteFile", session.ID, ev.payload, "files/report.md", len(body), 0, len(body))
	})

	t.Run("WriteFile overwrite", func(t *testing.T) {
		body := []byte("hello world, again")
		if err := session.WriteFile("files/report.md", body); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ev := take(t)
		if ev.typ != "session.file.updated" {
			t.Errorf("event type = %q, want session.file.updated for an existing path", ev.typ)
		}
		checkSessionFileEvent(t, "WriteFile overwrite", session.ID, ev.payload, "files/report.md", len(body), 0, len(body))
	})

	t.Run("AppendFile", func(t *testing.T) {
		first := []byte("line one\n")
		if err := session.AppendFile("context/log.jsonl", first); err != nil {
			t.Fatalf("AppendFile: %v", err)
		}
		ev := take(t)
		if ev.typ != "session.file.created" {
			t.Errorf("event type = %q, want session.file.created for the append that created the file", ev.typ)
		}
		checkSessionFileEvent(t, "AppendFile create", session.ID, ev.payload, "context/log.jsonl", len(first), 0, len(first))

		second := []byte("line two\n")
		if err := session.AppendFile("context/log.jsonl", second); err != nil {
			t.Fatalf("AppendFile: %v", err)
		}
		ev = take(t)
		if ev.typ != "session.file.updated" {
			t.Errorf("event type = %q, want session.file.updated for a true append", ev.typ)
		}
		checkSessionFileEvent(t, "AppendFile append", session.ID, ev.payload,
			"context/log.jsonl", len(first)+len(second), len(first), len(second))
	})

	t.Run("SaveMeta", func(t *testing.T) {
		meta, err := session.SessionMetadata()
		if err != nil {
			t.Fatalf("SessionMetadata: %v", err)
		}
		meta.TurnCount++
		if err := session.SaveMeta(meta); err != nil {
			t.Fatalf("SaveMeta: %v", err)
		}
		ev := take(t)
		size := statSize(filepath.Join(session.RootDir, "metadata", "session.json"))
		checkSessionFileEvent(t, "SaveMeta", session.ID, ev.payload, "metadata/session.json", size, 0, size)
	})

	t.Run("AnnounceWrite", func(t *testing.T) {
		// The second door: a writer that holds its own os.* call. Written with
		// filepath.Join on purpose, so the helper has to normalise the
		// separator rather than the test handing it a slash path.
		full := filepath.Join(session.RootDir, "plugins", "nexus.test.guard", "state.json")
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"state":1}`)
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
		session.AnnounceWrite(full, false)
		ev := take(t)
		checkSessionFileEvent(t, "AnnounceWrite", session.ID, ev.payload,
			"plugins/nexus.test.guard/state.json", len(body), 0, len(body))
	})

	t.Run("AnnounceAppend", func(t *testing.T) {
		full := filepath.Join(session.RootDir, "plugins", "nexus.test.guard", "journal.jsonl")
		first := []byte("one\n")
		if err := os.WriteFile(full, first, 0o644); err != nil {
			t.Fatal(err)
		}
		session.AnnounceAppend(full, len(first))
		ev := take(t)
		checkSessionFileEvent(t, "AnnounceAppend create", session.ID, ev.payload,
			"plugins/nexus.test.guard/journal.jsonl", len(first), 0, len(first))

		second := []byte("two\n")
		f, err := os.OpenFile(full, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(second); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		session.AnnounceAppend(full, len(second))
		ev = take(t)
		checkSessionFileEvent(t, "AnnounceAppend append", session.ID, ev.payload,
			"plugins/nexus.test.guard/journal.jsonl", len(first)+len(second), len(first), len(second))
	})

	// The doors that must stay shut: nothing outside the session tree, and
	// nothing the seam excludes, may be announced however a caller wires it up.
	t.Run("refuses paths off the seam", func(t *testing.T) {
		for _, full := range []string{
			filepath.Join(root, "outside.md"),
			filepath.Join(session.RootDir, sessionLockFilename),
			filepath.Join(session.RootDir, "plugins", "nexus.test.guard", "store.db-wal"),
		} {
			session.AnnounceWrite(full, false)
			session.AnnounceAppend(full, 1)
			if len(events) != 0 {
				t.Errorf("announcing %q emitted %d events; it is outside the seam", full, len(events))
				events = nil
			}
		}
	})
}

// The provenance half: session.file.* is the workspace's to emit.
//
// A hand-built emit is how the shape drifts — the payload is an untyped map, so
// nothing at compile time or in `make check-events` objects to one carrying
// three keys and an absolute path. Routing every announcement through
// AnnounceWrite / AnnounceAppend is what makes sessionFileEvent the only
// definition of the shape rather than merely the most common one.
func TestSessionFileEventsAreEmittedOnlyByTheWorkspaceHelpers(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"plugins", "cmd"} {
		found, err := scanSessionFileEmits(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("scanning %s/: %v", dir, err)
		}
		for _, site := range found {
			t.Errorf(`%s/%s emits session.file.* by hand:
    %s

The session.file.* payload is an untyped map, so nothing at compile time and
nothing in make check-events objects to one carrying the wrong keys or an
absolute path. That is how a hand-built emit drifts: cmd/desktop's matcher
carried an absolute path and none of session_id, size, offset or bytes_added
until E3-S5, and a subscriber keyed on path would have stored it under a key no
other emitter produces.

Announce the write instead — the workspace builds the payload:

    session.AnnounceWrite(absPath, existedBefore)   // whole-file write
    session.AnnounceAppend(absPath, bytesAdded)     // append to the tail

Both are no-ops when the path is outside the session tree or is a file the seam
excludes, so a plugin with a configurable output directory can call them
unconditionally. Or write through session.WriteFile / session.AppendFile, which
announce for you.

See docs/src/events/reference.md, "session.file.created / session.file.updated
payload".`, dir, site.path, strings.Join(site.sites, "\n    "))
		}
	}

}

// The positive control. A scan that finds nothing proves nothing unless it can
// be shown to find something, and the tree is (now) clean — so the sample is
// synthetic rather than a file in the repo. It is byte-for-byte the emit
// cmd/desktop/internal/matcher carried before E3-S5, which is the regression
// this scan exists to catch.
//
// pkg/engine cannot serve as the control: the workspace spells the event type
// as a variable (created vs updated is decided at runtime), which is precisely
// the blind spot recorded at the top of this file.
func TestSessionFileEmitScannerFindsAHandBuiltEmit(t *testing.T) {
	const src = `package p

func f(bus B, path string) {
	_ = bus.Emit("session.file.created", map[string]any{
		"path":   path,
		"action": "created",
	})
	_ = bus.Emit("llm.request", nil)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sites := sessionFileEmitsIn(fset, f)
	if len(sites) != 1 {
		t.Fatalf("scanner found %v in the sample, want exactly the session.file.created emit", sites)
	}
	if !strings.Contains(sites[0], "session.file.created") {
		t.Errorf("site = %q, want it to name the emitted type", sites[0])
	}
}

type sessionFileEmitSite struct {
	// path is slash-separated and relative to the scanned root.
	path  string
	sites []string
}

// scanSessionFileEmits finds Emit calls whose first argument is a
// "session.file.*" string literal.
func scanSessionFileEmits(root string) ([]sessionFileEmitSite, error) {
	var out []sessionFileEmitSite
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// Cheap pre-filter: a file that never spells the prefix cannot contain
		// one of these calls, and skipping the parse keeps the scan well under
		// a tenth of a second across the whole tree.
		if !bytes.Contains(src, []byte(`"session.file.`)) {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		sites := sessionFileEmitsIn(fset, f)
		if len(sites) == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, sessionFileEmitSite{path: filepath.ToSlash(rel), sites: sites})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// sessionFileEmitsIn reports every Emit call in f whose first argument is a
// "session.file.*" string literal. Split out of the walk so the scanner itself
// is testable against a sample.
func sessionFileEmitsIn(fset *token.FileSet, f *ast.File) []string {
	var sites []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Emit" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, `"session.file.`) {
			return true
		}
		sites = append(sites, fmt.Sprintf("%d: Emit(%s, ...)", fset.Position(call.Pos()).Line, lit.Value))
		return true
	})
	return sites
}
