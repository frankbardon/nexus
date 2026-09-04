// Package objectstoretest holds the shared conformance suite for
// objectstore.Backend, plus Memory - an in-memory backend that passes it.
//
// # Why an exported suite
//
// The point of objectstore.Backend is that a backend can live in a different
// module, written by someone who will never send a PR here. That freedom is
// only safe if "conformant" means something more precise than the prose on the
// interface. Prose does not catch a List that silently returns one page, a
// Hydrate that forgets to strip the prefix, or a prefix match that treats
// "sessions/sess-1" as covering "sessions/sess-10", and each of those produces
// a corrupted session rather than an error message.
//
// So the definition of conformant is executable and lives here. Three
// implementations are held to it: Memory below, the S3 module and the GCS
// module. An out-of-tree backend runs the same suite with a single call:
//
//	func TestContract(t *testing.T) {
//	    objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
//	        return newMyBackend(t) // empty, and cleaned up via t.Cleanup
//	    })
//	}
//
// # Shape
//
// One exported entry point taking a factory, not a pile of helpers a caller
// has to assemble. The factory is called once per case so no case can be
// affected by another's leftovers, and returning a Backend rather than
// accepting one is what makes that isolation the implementer's to arrange
// (a temp bucket, a cleared prefix, a fresh emulator namespace) instead of
// something the suite has to guess at.
//
// Taking *testing.T rather than returning an error - the testing/fstest.TestFS
// approach - is deliberate: subtests give a third-party implementer the name of
// the exact semantic they broke, and it matches pkg/testharness/contract, the
// house precedent for an exported suite.
package objectstoretest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// NewBackend builds a fresh, empty backend for one contract case. Any teardown
// belongs on t.Cleanup.
type NewBackend func(t *testing.T) objectstore.Backend

// defaultListProbeCount is the number of objects the pagination case stores.
//
// It is above 1000 on purpose: 1000 is the default page size of both
// ListObjectsV2 and the GCS list API, so a backend that forgets to follow its
// paginator passes any smaller probe. A backend for which 1200 sequential Puts
// is unaffordable lowers it with WithListProbeCount, accepting that the case
// then proves less.
const defaultListProbeCount = 1200

type options struct {
	listProbeCount     int
	skipConcurrency    bool
	skipObjectAtPrefix bool
}

// Option tunes the suite for backends that cannot afford a default.
type Option func(*options)

// WithListProbeCount sets how many objects the pagination case stores. Set it
// below the backend's natural page size and the case stops proving anything;
// the default is chosen to exceed the common ones.
func WithListProbeCount(n int) Option {
	return func(o *options) { o.listProbeCount = n }
}

// WithoutConcurrency skips the concurrent-use case. Provided for backends whose
// test double or emulator cannot take parallel traffic, not as a way to opt out
// of the requirement, which the interface states unconditionally.
func WithoutConcurrency() Option {
	return func(o *options) { o.skipConcurrency = true }
}

// WithoutObjectAtPrefix drops the tail of the prefix-matching case that stores
// an object at exactly the prefix other objects live under - key
// "sessions/sess-1" alongside "sessions/sess-1/files/a.txt".
//
// S3 and GCS both hold that state: their key spaces are flat, "/" has no
// meaning beyond being a byte, and a key that happens to be another key's
// prefix is unremarkable. The suite asserts the state because the *engine's*
// key scheme can produce it and a backend that lets the object at the prefix
// leak into a hydration writes a file where a directory belongs.
//
// Some stores cannot represent it at all. MinIO is the measured example: a PUT
// to "sessions/sess-1" against a store already holding
// "sessions/sess-1/files/a.txt" returns 200 and makes the child object
// disappear from every subsequent list, in both single-drive and erasure modes.
// That is a divergence from S3 in the emulator, not in the backend under test,
// and the resulting bucket cannot even be emptied by listing it.
//
// So this exists for emulators, and only for emulators. It is not a way for a
// backend to opt out of the semantic: a backend that needs it is claiming its
// *store* has a flat-key-space limitation, which is a property worth stating
// out loud at the call site. Run the rest of the suite unaffected -- every
// other prefix-matching assertion in the case still runs, including the
// "sess-1" versus "sess-10" collision that motivates the case.
func WithoutObjectAtPrefix() Option {
	return func(o *options) { o.skipObjectAtPrefix = true }
}

// RunSuite runs every conformance case against backends built by newBackend.
//
// Each case gets its own backend, so cases are independent and a failure in one
// does not cascade. Nothing here uses t.Parallel: a third-party backend may be
// pointed at a shared emulator, and a suite that is only correct when run
// serially is better than one that is intermittently wrong.
func RunSuite(t *testing.T, newBackend NewBackend, opts ...Option) {
	t.Helper()
	if newBackend == nil {
		t.Fatal("objectstoretest: RunSuite called with a nil NewBackend")
	}
	cfg := options{listProbeCount: defaultListProbeCount}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.listProbeCount < 1 {
		t.Fatalf("objectstoretest: list probe count %d must be at least 1", cfg.listProbeCount)
	}

	cases := []struct {
		name string
		run  func(*harness, options)
	}{
		{"KeyRoundTrip", caseKeyRoundTrip},
		{"Overwrite", caseOverwrite},
		{"DeleteThenList", caseDeleteThenList},
		{"DeleteMissingKeyIsNotAnError", caseDeleteMissingKey},
		{"ListPrefixMatchesWholeSegments", caseListPrefixSegments},
		{"ListReturnsFullKeysAndSizes", caseListFullKeysAndSizes},
		{"ListIsCompleteBeyondOnePage", caseListPagination},
		{"ListOfUnknownPrefixIsEmpty", caseListUnknownPrefix},
		{"HydrateStripsThePrefix", caseHydrateStripsPrefix},
		{"HydrateOverwritesAndPreservesUnrelatedFiles", caseHydrateOverwrites},
		{"HydrateOfUnknownPrefixLeavesDestinationAlone", caseHydrateUnknownPrefix},
		{"EmptyObjectRoundTrips", caseEmptyObject},
		{"PutOfAMissingLocalFileFails", casePutMissingLocalFile},
		{"PutLeavesTheLocalFileUntouched", casePutLeavesLocalFile},
		{"InvalidKeysAreRejected", caseInvalidKeys},
		{"InvalidPrefixesAreRejected", caseInvalidPrefixes},
		{"FlushIsIdempotentAndPreservesState", caseFlush},
		{"ConcurrentUseIsSafe", caseConcurrency},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBackend(t)
			if b == nil {
				t.Fatal("objectstoretest: NewBackend returned a nil Backend")
			}
			c.run(&harness{t: t, backend: b, ctx: context.Background(), src: t.TempDir()}, cfg)
		})
	}
}

// harness carries the per-case backend plus the small helpers every case wants.
// Unexported: the entry point is RunSuite, and an implementer who reaches for
// individual helpers is writing their own suite, not running this one.
type harness struct {
	t       *testing.T
	backend objectstore.Backend
	ctx     context.Context
	src     string // scratch dir for the local files Put reads
	seq     int
}

// localFile writes content to a fresh local path and returns it.
func (h *harness) localFile(content string) string {
	h.t.Helper()
	h.seq++
	path := filepath.Join(h.src, fmt.Sprintf("src-%04d.bin", h.seq))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatalf("writing local source file: %v", err)
	}
	return path
}

// put stores content under key and fails the test if the backend refuses.
func (h *harness) put(key, content string) {
	h.t.Helper()
	if err := h.backend.Put(h.ctx, key, h.localFile(content)); err != nil {
		h.t.Fatalf("Put(%q) = %v, want nil", key, err)
	}
}

func (h *harness) delete(key string) {
	h.t.Helper()
	if err := h.backend.Delete(h.ctx, key); err != nil {
		h.t.Fatalf("Delete(%q) = %v, want nil", key, err)
	}
}

func (h *harness) list(prefix string) []objectstore.Object {
	h.t.Helper()
	objs, err := h.backend.List(h.ctx, prefix)
	if err != nil {
		h.t.Fatalf("List(%q) = %v, want nil", prefix, err)
	}
	return objs
}

// listKeys returns List's keys sorted. Sorting here rather than asserting on
// the backend's order is the contract: List's order is unspecified.
func (h *harness) listKeys(prefix string) []string {
	h.t.Helper()
	objs := h.list(prefix)
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	slices.Sort(keys)
	return keys
}

// hydrate pulls prefix into a fresh directory and returns it.
func (h *harness) hydrate(prefix string) string {
	h.t.Helper()
	dir := h.t.TempDir()
	h.hydrateInto(prefix, dir)
	return dir
}

func (h *harness) hydrateInto(prefix, dir string) {
	h.t.Helper()
	if err := h.backend.Hydrate(h.ctx, prefix, dir); err != nil {
		h.t.Fatalf("Hydrate(%q) = %v, want nil", prefix, err)
	}
}

// tree returns every regular file under dir as a sorted slash-separated path
// relative to dir. Directories are excluded: an object store has no
// directories, so whether a backend materialises an empty one is not part of
// the contract.
func (h *harness) tree(dir string) []string {
	h.t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		h.t.Fatalf("walking hydrated tree: %v", err)
	}
	slices.Sort(out)
	return out
}

// readFile reads a slash-separated path under a hydrated directory.
func (h *harness) readFile(dir, rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("reading hydrated %q: %v", rel, err)
	}
	return string(data)
}

func (h *harness) wantTree(dir string, want ...string) {
	h.t.Helper()
	slices.Sort(want)
	if got := h.tree(dir); !slices.Equal(got, want) {
		h.t.Errorf("hydrated tree = %v, want %v", got, want)
	}
}

func (h *harness) wantKeys(prefix string, want ...string) {
	h.t.Helper()
	slices.Sort(want)
	if got := h.listKeys(prefix); !slices.Equal(got, want) {
		h.t.Errorf("List(%q) keys = %v, want %v", prefix, got, want)
	}
}

// ---------------------------------------------------------------------------
// Cases
// ---------------------------------------------------------------------------

// roundTripKeys exercises the shapes a real session tree produces plus the
// legal-but-awkward ones a naive implementation mangles: a segment of dots that
// is neither "." nor "..", a space, a non-ASCII segment, and a leading dot (the
// session tree is full of dotfiles).
//
// The non-ASCII segment is CJK rather than an accented Latin letter on purpose:
// accented Latin has an NFD decomposition, and a filesystem that normalises
// (HFS+ does) would fail the round trip for reasons that have nothing to do
// with the backend under test.
var roundTripKeys = map[string]string{
	"metadata/session.json":                `{"id":"s1"}`,
	"context/conversation.jsonl":           "{\"role\":\"user\"}\n{\"role\":\"assistant\"}\n",
	"files/report.md":                      "# Report\n\nbody\n",
	"files/nested/deeply/inside/notes.txt": "deep",
	"files/with spaces/and a name.txt":     "spaced",
	"files/日本語/notes.md":                   "a non-ASCII segment survives the round trip",
	"files/...triple":                      "dots are only special as whole . and .. segments",
	"plugins/nexus.scene/scene.jsonl":      "{}\n",
	"files/.hidden":                        "leading dot",
	"single":                               "a key with exactly one segment",
	"files/binary.bin":                     "\x00\x01\x02\xff\xfe",
}

func caseKeyRoundTrip(h *harness, _ options) {
	for key, content := range roundTripKeys {
		h.put(key, content)
	}

	want := make([]string, 0, len(roundTripKeys))
	for key := range roundTripKeys {
		want = append(want, key)
	}
	h.wantKeys("", want...)

	// The whole store hydrated under the empty prefix: relative paths equal the
	// keys, byte for byte.
	dir := h.hydrate("")
	h.wantTree(dir, want...)
	for key, content := range roundTripKeys {
		if got := h.readFile(dir, key); got != content {
			h.t.Errorf("hydrated %q = %q, want %q", key, got, content)
		}
	}
}

func caseOverwrite(h *harness, _ options) {
	const key = "files/report.md"
	const second = "second, materially longer version"
	h.put(key, "first version")
	h.put(key, second)

	// One object, not two, and the size must have followed the content. A
	// backend that writes the new bytes but leaves stale metadata behind breaks
	// every hydration diff built on List.
	objs := h.list("")
	if len(objs) != 1 {
		h.t.Fatalf("List after overwrite returned %d objects, want 1: %+v", len(objs), objs)
	}
	if want := int64(len(second)); objs[0].Size != want {
		h.t.Errorf("Size after overwrite = %d, want %d", objs[0].Size, want)
	}

	dir := h.hydrate("")
	if got := h.readFile(dir, key); got != second {
		h.t.Errorf("hydrated content = %q, want the second version", got)
	}
}

func caseDeleteThenList(h *harness, _ options) {
	h.put("a/one.txt", "1")
	h.put("a/two.txt", "2")
	h.put("b/three.txt", "3")

	h.delete("a/one.txt")

	h.wantKeys("", "a/two.txt", "b/three.txt")
	h.wantKeys("a", "a/two.txt")

	// The deleted object must be gone from the hydrated tree too, not merely
	// hidden from List.
	h.wantTree(h.hydrate(""), "a/two.txt", "b/three.txt")
}

func caseDeleteMissingKey(h *harness, _ options) {
	// Documented explicitly on the interface: every object store worth
	// targeting treats this as a success, and the push path relies on it so a
	// retried delete is not an error.
	if err := h.backend.Delete(h.ctx, "never/existed.txt"); err != nil {
		h.t.Errorf("Delete of a missing key = %v, want nil", err)
	}
	h.put("a.txt", "a")
	h.delete("a.txt")
	if err := h.backend.Delete(h.ctx, "a.txt"); err != nil {
		h.t.Errorf("second Delete of the same key = %v, want nil", err)
	}
}

func caseListPrefixSegments(h *harness, opts options) {
	// "sessions/sess-1" against "sessions/sess-10" is the exact collision the
	// engine's key scheme produces, and raw string prefix matching - what every
	// cloud list API does natively - gets it wrong.
	h.put("sessions/sess-1/files/a.txt", "a")
	h.put("sessions/sess-1/metadata/session.json", "{}")
	h.put("sessions/sess-10/files/b.txt", "b")
	h.put("sessionsX/c.txt", "c")
	h.put("other/d.txt", "d")

	h.wantKeys("sessions/sess-1",
		"sessions/sess-1/files/a.txt",
		"sessions/sess-1/metadata/session.json")
	h.wantKeys("sessions/sess-10", "sessions/sess-10/files/b.txt")
	h.wantKeys("sessions",
		"sessions/sess-1/files/a.txt",
		"sessions/sess-1/metadata/session.json",
		"sessions/sess-10/files/b.txt")
	h.wantKeys("", // the empty prefix means everything
		"sessions/sess-1/files/a.txt",
		"sessions/sess-1/metadata/session.json",
		"sessions/sess-10/files/b.txt",
		"sessionsX/c.txt",
		"other/d.txt")

	// The same rule applies to Hydrate, where getting it wrong mixes two
	// sessions' files into one tree.
	h.wantTree(h.hydrate("sessions/sess-1"), "files/a.txt", "metadata/session.json")

	// An object AT the prefix is not under it: it has no path beneath a
	// hydration destination, so there is nowhere for it to go.
	//
	// Left until last so that a store which cannot represent this state at all
	// - see WithoutObjectAtPrefix - skips only this block and still runs every
	// assertion above it.
	if opts.skipObjectAtPrefix {
		return
	}
	h.put("sessions/sess-1", "an object at the prefix itself")
	h.wantKeys("sessions/sess-1",
		"sessions/sess-1/files/a.txt",
		"sessions/sess-1/metadata/session.json")
}

func caseListFullKeysAndSizes(h *harness, _ options) {
	contents := map[string]string{
		"p/short":  "hi",
		"p/longer": strings.Repeat("x", 4096),
		"p/sub/z":  "",
	}
	for k, v := range contents {
		h.put(k, v)
	}

	started := time.Now().Add(-time.Minute)
	seen := 0
	for _, o := range h.list("p") {
		want, ok := contents[o.Key]
		if !ok {
			// Object.Key is the full store-relative key, never the remainder
			// after the prefix. Reporting the remainder makes every key the
			// caller feeds back into Put or Delete wrong.
			h.t.Errorf("List returned key %q; keys must be full store-relative keys", o.Key)
			continue
		}
		seen++
		if o.Size != int64(len(want)) {
			h.t.Errorf("Size of %q = %d, want %d", o.Key, o.Size, len(want))
		}
		// ModTime may be zero - the interface allows a backend that cannot
		// report one - but a non-zero value outside the window this test ran in
		// means a unit mix-up (seconds read as milliseconds, or the reverse).
		if !o.ModTime.IsZero() {
			if o.ModTime.Before(started) || o.ModTime.After(time.Now().Add(time.Minute)) {
				h.t.Errorf("ModTime of %q = %v, outside the window this test ran in", o.Key, o.ModTime)
			}
		}
	}
	if seen != len(contents) {
		h.t.Errorf("List returned %d of %d objects", seen, len(contents))
	}
}

func caseListPagination(h *harness, opts options) {
	// Reuse a handful of local files rather than one per object: the point is
	// the number of *objects*, and creating N local files would make the case
	// slower for everyone in exchange for nothing.
	sources := []string{
		h.localFile(""),
		h.localFile("x"),
		h.localFile(strings.Repeat("y", 64)),
	}
	want := make([]string, 0, opts.listProbeCount)
	for i := range opts.listProbeCount {
		key := fmt.Sprintf("page/%06d.bin", i)
		if err := h.backend.Put(h.ctx, key, sources[i%len(sources)]); err != nil {
			h.t.Fatalf("Put(%q) = %v", key, err)
		}
		want = append(want, key)
	}

	got := h.listKeys("page")
	if len(got) != len(want) {
		h.t.Fatalf("List returned %d of %d objects; a paging backend must follow every page",
			len(got), len(want))
	}
	if !slices.Equal(got, want) {
		h.t.Error("List returned the wrong set of keys across pages")
	}
}

func caseListUnknownPrefix(h *harness, _ options) {
	h.put("a/one.txt", "1")

	objs, err := h.backend.List(h.ctx, "nothing/here")
	if err != nil {
		h.t.Fatalf("List of an unknown prefix = %v, want nil", err)
	}
	if len(objs) != 0 {
		h.t.Errorf("List of an unknown prefix returned %d objects, want none: %+v", len(objs), objs)
	}
}

func caseHydrateStripsPrefix(h *harness, _ options) {
	h.put("sessions/s1/metadata/session.json", `{"id":"s1"}`)
	h.put("sessions/s1/files/report.md", "report")

	dir := h.hydrate("sessions/s1")
	h.wantTree(dir, "metadata/session.json", "files/report.md")
	if got := h.readFile(dir, "files/report.md"); got != "report" {
		h.t.Errorf("hydrated report = %q, want %q", got, "report")
	}
	// The prefix itself must not survive as directories under the destination.
	// A backend that writes the full key is two levels off, and the engine then
	// finds a session with no metadata at all.
	if _, err := os.Stat(filepath.Join(dir, "sessions")); !errors.Is(err, fs.ErrNotExist) {
		h.t.Errorf("hydration left a \"sessions\" directory under the destination (stat err = %v); the prefix must be stripped", err)
	}
}

func caseHydrateOverwrites(h *harness, _ options) {
	h.put("files/report.md", "from the store")

	dir := h.t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "report.md"), []byte("stale local copy"), 0o644); err != nil {
		h.t.Fatalf("seeding stale file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("keep me"), 0o644); err != nil {
		h.t.Fatalf("seeding unrelated file: %v", err)
	}

	h.hydrateInto("", dir)

	if got := h.readFile(dir, "files/report.md"); got != "from the store" {
		h.t.Errorf("hydrated over a stale file = %q, want %q", got, "from the store")
	}
	// Hydrate adds and overwrites; it does not mirror. Deleting what the store
	// has no object for would make Hydrate destructive on a partially populated
	// destination, and the engine hydrates into a staging directory it has
	// already created.
	if got := h.readFile(dir, "unrelated.txt"); got != "keep me" {
		h.t.Errorf("unrelated file = %q, want it untouched", got)
	}
}

func caseHydrateUnknownPrefix(h *harness, _ options) {
	h.put("sessions/other/a.txt", "a")

	dir := h.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pre-existing.txt"), []byte("keep"), 0o644); err != nil {
		h.t.Fatalf("seeding: %v", err)
	}

	// A prefix with no objects means a brand-new session, not a failure. The
	// engine turns an error here into a failed boot, so a backend that reports
	// "not found" makes every first run fail.
	if err := h.backend.Hydrate(h.ctx, "sessions/brand-new", dir); err != nil {
		h.t.Fatalf("Hydrate of a prefix with no objects = %v, want nil", err)
	}
	h.wantTree(dir, "pre-existing.txt")
}

func caseEmptyObject(h *harness, _ options) {
	// A zero-byte object is the classic thing to lose: several stores use an
	// empty object as a directory marker, and an implementation that filters
	// those out drops the session's real empty files along with them.
	h.put("files/empty.txt", "")
	h.put("files/notempty.txt", "x")

	objs := h.list("files")
	found := false
	for _, o := range objs {
		if o.Key != "files/empty.txt" {
			continue
		}
		found = true
		if o.Size != 0 {
			h.t.Errorf("Size of the empty object = %d, want 0", o.Size)
		}
	}
	if !found {
		h.t.Errorf("List omitted the zero-byte object: %+v", objs)
	}

	dir := h.hydrate("")
	h.wantTree(dir, "files/empty.txt", "files/notempty.txt")
	info, err := os.Stat(filepath.Join(dir, "files", "empty.txt"))
	if err != nil {
		h.t.Fatalf("stat hydrated empty object: %v", err)
	}
	if info.Size() != 0 {
		h.t.Errorf("hydrated empty object is %d bytes, want 0", info.Size())
	}
}

func casePutMissingLocalFile(h *harness, _ options) {
	missing := filepath.Join(h.src, "does-not-exist.bin")
	if err := h.backend.Put(h.ctx, "files/ghost.txt", missing); err == nil {
		h.t.Fatal("Put of a missing local file = nil, want an error")
	}
	// And nothing may be left behind. A zero-length object where a real file was
	// expected is worse than a failed push, because the next hydration serves it
	// as if it were the truth.
	h.wantKeys("")
}

func casePutLeavesLocalFile(h *harness, _ options) {
	const content = "the live working copy"
	path := h.localFile(content)
	if err := h.backend.Put(h.ctx, "files/live.txt", path); err != nil {
		h.t.Fatalf("Put = %v", err)
	}
	// The engine pushes the session's live files. A backend that moves or
	// truncates the source destroys the working copy the run is still using.
	got, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("local source file after Put: %v", err)
	}
	if string(got) != content {
		h.t.Errorf("local source file = %q after Put, want %q", got, content)
	}
}

// invalidKeys must be rejected by every method that takes a key.
var invalidKeys = []string{
	"",
	"/leading",
	"trailing/",
	"double//slash",
	"has/./dot",
	"has/../dotdot",
	".",
	"..",
	"../escape",
	"escape/..",
	`windows\path`,
	"nul\x00byte",
}

func caseInvalidKeys(h *harness, _ options) {
	local := h.localFile("payload")
	for _, key := range invalidKeys {
		// Rejection must be an error wrapping ErrInvalidKey, not a panic and not
		// a silent success: the engine distinguishes "this key is malformed" (a
		// bug, never worth retrying) from "the remote said no" (retryable)
		// without matching strings.
		if err := h.backend.Put(h.ctx, key, local); !errors.Is(err, objectstore.ErrInvalidKey) {
			h.t.Errorf("Put(%q) = %v, want an error wrapping objectstore.ErrInvalidKey", key, err)
		}
		if err := h.backend.Delete(h.ctx, key); !errors.Is(err, objectstore.ErrInvalidKey) {
			h.t.Errorf("Delete(%q) = %v, want an error wrapping objectstore.ErrInvalidKey", key, err)
		}
	}
	// A rejected Put must not have stored anything under any spelling.
	h.wantKeys("")
}

func caseInvalidPrefixes(h *harness, _ options) {
	dir := h.t.TempDir()
	for _, prefix := range invalidKeys {
		if prefix == "" {
			// The one difference between a key and a prefix: the empty prefix
			// legally means "everything".
			continue
		}
		if _, err := h.backend.List(h.ctx, prefix); !errors.Is(err, objectstore.ErrInvalidKey) {
			h.t.Errorf("List(%q) = %v, want an error wrapping objectstore.ErrInvalidKey", prefix, err)
		}
		if err := h.backend.Hydrate(h.ctx, prefix, dir); !errors.Is(err, objectstore.ErrInvalidKey) {
			h.t.Errorf("Hydrate(%q) = %v, want an error wrapping objectstore.ErrInvalidKey", prefix, err)
		}
	}
	// A rejected Hydrate must not have written into the destination. The
	// traversal spellings ("../escape") are the reason the check exists at all.
	h.wantTree(dir)
}

func caseFlush(h *harness, _ options) {
	// Flush on an untouched backend is the shutdown path of a run that wrote
	// nothing. It must not invent work and must not fail.
	if err := h.backend.Flush(h.ctx); err != nil {
		h.t.Fatalf("Flush on an empty backend = %v, want nil", err)
	}

	h.put("files/a.txt", "a")
	h.delete("files/a.txt")
	h.put("files/b.txt", "b")

	if err := h.backend.Flush(h.ctx); err != nil {
		h.t.Fatalf("Flush = %v, want nil", err)
	}
	// The engine flushes at every turn boundary as well as on shutdown, so a
	// second Flush with nothing new in between is the common case, not an edge.
	if err := h.backend.Flush(h.ctx); err != nil {
		h.t.Fatalf("second Flush = %v, want nil", err)
	}

	// Flush promises durability, so everything issued before it must be
	// observable after it - including the delete.
	h.wantKeys("", "files/b.txt")
	h.wantTree(h.hydrate(""), "files/b.txt")
}

func caseConcurrency(h *harness, opts options) {
	if opts.skipConcurrency {
		h.t.Skip("concurrency case disabled with WithoutConcurrency")
	}
	// The interface requires concurrent safety because the engine pushes
	// artifacts from bus handlers while a turn-boundary snapshot is running.
	// Under `make test-race` this case is where that requirement is really
	// checked; without -race it still catches a map written from two goroutines,
	// which the runtime detects unconditionally.
	const workers = 8
	const perWorker = 16

	local := h.localFile("concurrent")
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWorker {
				key := fmt.Sprintf("concurrent/w%02d/o%03d.bin", w, i)
				if err := h.backend.Put(h.ctx, key, local); err != nil {
					h.t.Errorf("concurrent Put(%q) = %v", key, err)
					return
				}
				if _, err := h.backend.List(h.ctx, "concurrent"); err != nil {
					h.t.Errorf("concurrent List = %v", err)
					return
				}
				// Deleting a key no worker ever wrote keeps a delete in the mix
				// without making the final state depend on scheduling.
				if err := h.backend.Delete(h.ctx, fmt.Sprintf("concurrent/w%02d/gone-%03d", w, i)); err != nil {
					h.t.Errorf("concurrent Delete = %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if err := h.backend.Flush(h.ctx); err != nil {
		h.t.Fatalf("Flush after concurrent writes = %v", err)
	}
	if got := len(h.listKeys("concurrent")); got != workers*perWorker {
		h.t.Errorf("List after concurrent writes returned %d objects, want %d", got, workers*perWorker)
	}
}
