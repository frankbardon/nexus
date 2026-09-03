package engine

// The enforcement half of the session-writer invariant.
//
// session_writers.go records a disposition for every raw writer that existed
// when the survey was taken. That closes today's gap. This file is what stops
// it reopening: it walks every non-test Go file under plugins/, finds the raw
// os.* calls that put bytes on disk, and requires each one to either announce
// itself on the bus or carry a row in SessionTreeWriters() explaining why it
// does not.
//
// The allowlist is SessionTreeWriters() rather than a list local to this file,
// deliberately. A second list would be free to drift from the first, and the
// drift would be invisible — the enforcement test would keep passing while the
// documented dispositions rotted. Sharing one table means a contributor
// silencing this test has to write down a reason in the same place a reader
// looks for one.
//
// Scope is plugins/ and not the whole tree. That is where contributors add
// code and where the survey found the writers that looked instrumented but
// were not; pkg/engine's raw writers are a small closed set of named
// subsystems (journal, blobs, toolcache, storage, the workspace itself, the
// object-store seam) that already have rows and are not somewhere a new file
// writer arrives unnoticed. Widening the scan to pkg/ would mean rows for
// objectstore.go and session.go — the implementations of the seam this
// invariant is about — which makes the table describe the mechanism rather
// than its users. See BLIND SPOTS below for what that costs.
//
// BLIND SPOTS, recorded because a guard whose limits are undocumented gets
// trusted past them:
//   - Only direct os.WriteFile / os.Create / os.CreateTemp / os.OpenFile calls
//     are seen. A write through a *os.File handed to another package, through
//     bufio/io.Copy onto a descriptor opened elsewhere, through
//     text/template.Execute, or through a third-party library is invisible.
//   - Announcement is detected per file, not per call. A file with one
//     announced write and one unannounced write reads as covered.
//   - Only plugins/ is scanned; cmd/ and pkg/ are not.

import (
	"bytes"
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

// rawWriteFuncs are the os package functions that create a file or put bytes
// in one. os.Open is absent because it is read-only; os.OpenFile is present
// even though it can be opened O_RDONLY, because deciding that statically
// means reading a flag expression that is frequently a variable, and a false
// positive here costs one allowlist row while a false negative costs the
// invariant.
var rawWriteFuncs = map[string]bool{
	"WriteFile":  true,
	"Create":     true,
	"CreateTemp": true,
	"OpenFile":   true,
}

// announceFuncs are the SessionWorkspace helpers that put a write on the bus.
// Matched by selector name alone: the receiver is a *SessionWorkspace reached
// through a plugin field under half a dozen different names, and resolving it
// properly would mean type-checking the whole repo on every `make test`.
var announceFuncs = map[string]bool{
	"AnnounceWrite":  true,
	"AnnounceAppend": true,
}

// rawWriteFile is one source file that contains at least one raw write.
type rawWriteFile struct {
	// Path is slash-separated and relative to the scanned root.
	Path string
	// Sites are human-readable "line: os.Func" strings, in file order.
	Sites []string
	// Announces reports whether the file also calls AnnounceWrite or
	// AnnounceAppend anywhere.
	Announces bool
}

// scanRawWrites walks root and returns every non-test Go file containing a raw
// os write call, sorted by path.
func scanRawWrites(root string) ([]rawWriteFile, error) {
	var out []rawWriteFile
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds fixtures that are not compiled into any
			// package, so a raw write there is not a writer.
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
		// Cheap pre-filter: a file that never spells the os import cannot
		// contain an os call, and skipping the parse for it keeps this test
		// well under a tenth of a second across ~330 files.
		if !bytes.Contains(src, []byte(`"os"`)) {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		osName, ok := osLocalName(f)
		if !ok {
			return nil
		}

		var sites []string
		announces := false
		ast.Inspect(f, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if announceFuncs[fun.Sel.Name] {
					announces = true
					return true
				}
				ident, isIdent := fun.X.(*ast.Ident)
				if !isIdent || ident.Name != osName || osName == "." {
					return true
				}
				if rawWriteFuncs[fun.Sel.Name] {
					sites = append(sites, fmt.Sprintf("%d: os.%s",
						fset.Position(call.Pos()).Line, fun.Sel.Name))
				}
			case *ast.Ident:
				// Dot-imported os. Nobody does this today, but leaving it
				// unhandled would make `import . "os"` a silent bypass of
				// the whole invariant.
				if osName == "." && rawWriteFuncs[fun.Name] {
					sites = append(sites, fmt.Sprintf("%d: %s (dot-imported os)",
						fset.Position(call.Pos()).Line, fun.Name))
				}
			}
			return true
		})

		if len(sites) == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rawWriteFile{
			Path:      filepath.ToSlash(rel),
			Sites:     sites,
			Announces: announces,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// osLocalName returns the identifier the file binds the os package to, and
// whether os is imported at all. A blank import yields false: it binds no name
// and so cannot be called through.
func osLocalName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		if imp.Path == nil || imp.Path.Value != `"os"` {
			continue
		}
		if imp.Name == nil {
			return "os", true
		}
		if imp.Name.Name == "_" {
			return "", false
		}
		return imp.Name.Name, true
	}
	return "", false
}

// undecidedWrites filters a scan down to the files that are neither announcing
// nor allowlisted. prefix is prepended to each scanned path to produce the
// repo-relative form SessionTreeWriters().Source uses.
func undecidedWrites(found []rawWriteFile, prefix string, allowed map[string]bool) []rawWriteFile {
	var out []rawWriteFile
	for _, f := range found {
		if f.Announces || allowed[prefix+f.Path] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// allowlistedSources indexes SessionTreeWriters() by Source.
func allowlistedSources() map[string]bool {
	allowed := make(map[string]bool)
	for _, w := range SessionTreeWriters() {
		allowed[w.Source] = true
	}
	return allowed
}

// The invariant: a write under a session tree must announce itself on the bus,
// because real-time sync is exactly as complete as the events are, and a write
// nothing announced is history that quietly never arrives. This test is the
// thing that keeps a *future* plugin from reopening the gap E2-S1..E2-S3
// closed — the failure mode is silent and surfaces months later as missing
// files, so it has to be caught at the commit that introduces it.
func TestPluginRawWritesAreAnnouncedOrAllowlisted(t *testing.T) {
	root := repoRoot(t)
	found, err := scanRawWrites(filepath.Join(root, "plugins"))
	if err != nil {
		t.Fatalf("scanning plugins/: %v", err)
	}
	// A scan that finds nothing is a scan that has stopped working — the
	// tree has raw writers today and they are meant to be found and judged.
	if len(found) == 0 {
		t.Fatal("scanRawWrites found no raw writes under plugins/ at all; the " +
			"scanner is broken, not the tree")
	}

	for _, f := range undecidedWrites(found, "plugins/", allowlistedSources()) {
		t.Errorf(`plugins/%s writes files without announcing them:
    %s

Writes under a session tree must announce themselves on the bus. A sync
backend learns about session files from session.file.created / .updated and
nothing else, so a raw write that emits nothing is a file that never syncs —
and the symptom appears months later as missing history, not as a failure here.

Pick one:

  1. Announce it. After the write, call
         session.AnnounceWrite(absPath, existedBefore)   // whole-file write
         session.AnnounceAppend(absPath, bytesAdded)     // append to the tail
     Both no-op when the path is outside the session tree or is an excluded
     file, so a plugin with a configurable output directory can call them
     unconditionally instead of repeating an escape check.

  2. Or write through the workspace: session.WriteFile / session.AppendFile
     announce for you.

  3. Or, if this writer must stay silent, record why. Add a row to
     SessionTreeWriters() in pkg/engine/session_writers.go:

         {
             Source:      %q,
             Writes:      "what lands where",
             Disposition: DispositionTurnBoundary, // or DispositionExcluded
             Why:         "the reasoning, and the alternative you rejected",
         },

     then bump sessionTreeWriterCount in session_writers_test.go. The row is
     the point: an allowlist without reasons accumulates silently, which is
     the state this table exists to end.

See docs/src/architecture/sessions.md, "Writers that bypass the helpers".`,
			f.Path, strings.Join(f.Sites, "\n    "), "plugins/"+f.Path)
	}
}

// The allowlist has to shrink as well as grow. A row whose file stopped
// writing raw bytes — refactored onto the workspace helpers, or the writer
// deleted — is a decision about nothing, and leaving it behind is how a
// documented table turns into a list nobody trusts.
func TestSessionTreeWriters_PluginRowsStillWriteRawBytes(t *testing.T) {
	root := repoRoot(t)
	found, err := scanRawWrites(filepath.Join(root, "plugins"))
	if err != nil {
		t.Fatalf("scanning plugins/: %v", err)
	}
	writes := make(map[string]bool, len(found))
	for _, f := range found {
		writes["plugins/"+f.Path] = true
	}

	for _, w := range SessionTreeWriters() {
		if !strings.HasPrefix(w.Source, "plugins/") {
			continue
		}
		if !writes[w.Source] {
			t.Errorf("%s is allowlisted in SessionTreeWriters() but no longer "+
				"contains a raw os write — delete the row rather than leaving a "+
				"disposition attached to nothing", w.Source)
		}
	}
}

// Everything above is only worth having if it actually fires. These fixtures
// exercise the scanner against a synthetic tree so the positive and negative
// cases are both proved here rather than asserted about the real tree, where
// "it passes" is indistinguishable from "it looks at nothing".
func TestScanRawWrites_Fixtures(t *testing.T) {
	dir := t.TempDir()
	writeFixture := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFixture("unannounced/plugin.go", `package unannounced

import "os"

func save(p string, b []byte) error { return os.WriteFile(p, b, 0o644) }
`)
	writeFixture("announced/plugin.go", `package announced

import "os"

type ws interface{ AnnounceWrite(string, bool) }

func save(s ws, p string, b []byte) error {
	err := os.WriteFile(p, b, 0o644)
	s.AnnounceWrite(p, false)
	return err
}
`)
	writeFixture("appending/plugin.go", `package appending

import "os"

type ws interface{ AnnounceAppend(string, int) }

func add(s ws, p string, b []byte) {
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	n, _ := f.Write(b)
	s.AnnounceAppend(p, n)
}
`)
	writeFixture("readonly/plugin.go", `package readonly

import "os"

func load(p string) ([]byte, error) { return os.ReadFile(p) }
`)
	writeFixture("aliased/plugin.go", `package aliased

import goos "os"

func save(p string, b []byte) error { return goos.WriteFile(p, b, 0o644) }
`)
	writeFixture("dotimport/plugin.go", `package dotimport

import . "os"

func save(p string, b []byte) error { return WriteFile(p, b, 0o644) }
`)
	// A raw write in a test file is not a writer under a session tree, and
	// counting one would make every plugin's own tests trip the guard.
	writeFixture("intests/plugin_test.go", `package intests

import "os"

func save(p string, b []byte) error { return os.WriteFile(p, b, 0o644) }
`)
	// testdata is fixture material, not compiled code.
	writeFixture("withdata/testdata/plugin.go", `package withdata

import "os"

func save(p string, b []byte) error { return os.WriteFile(p, b, 0o644) }
`)
	// A local identifier called os that is not the package.
	writeFixture("shadowed/plugin.go", `package shadowed

import "os"

type fake struct{}

func (fake) WriteFile(string, []byte, os.FileMode) error { return nil }

func save(p string, b []byte) error {
	os := fake{}
	return os.WriteFile(p, b, 0o644)
}
`)

	found, err := scanRawWrites(dir)
	if err != nil {
		t.Fatalf("scanRawWrites: %v", err)
	}
	gotAnnounces := map[string]bool{}
	for _, f := range found {
		gotAnnounces[f.Path] = f.Announces
		if len(f.Sites) == 0 {
			t.Errorf("%s: reported with no sites", f.Path)
		}
	}

	// shadowed/ is a known false positive: distinguishing a local variable
	// named os from the package needs type information this scanner
	// deliberately does not build. Pinned so the cost of the choice stays
	// visible rather than being rediscovered by whoever trips it.
	want := map[string]bool{
		"unannounced/plugin.go": false,
		"announced/plugin.go":   true,
		"appending/plugin.go":   true,
		"aliased/plugin.go":     false,
		"dotimport/plugin.go":   false,
		"shadowed/plugin.go":    false,
	}
	for path, wantAnnounce := range want {
		gotAnnounce, ok := gotAnnounces[path]
		if !ok {
			t.Errorf("%s: not reported as a raw writer", path)
			continue
		}
		if gotAnnounce != wantAnnounce {
			t.Errorf("%s: Announces = %v, want %v", path, gotAnnounce, wantAnnounce)
		}
	}
	for path := range gotAnnounces {
		if _, ok := want[path]; !ok {
			t.Errorf("%s: reported as a raw writer, want ignored", path)
		}
	}

	// The negative control the whole story turns on: an unannounced write is
	// undecided, and stays undecided until it is announced or allowlisted.
	undecided := undecidedWrites(found, "plugins/", nil)
	var names []string
	for _, f := range undecided {
		names = append(names, f.Path)
	}
	sort.Strings(names)
	wantUndecided := []string{
		"aliased/plugin.go",
		"dotimport/plugin.go",
		"shadowed/plugin.go",
		"unannounced/plugin.go",
	}
	if strings.Join(names, ",") != strings.Join(wantUndecided, ",") {
		t.Errorf("undecided = %v, want %v", names, wantUndecided)
	}

	// ...and allowlisting silences exactly the file that was allowlisted,
	// which is the other half of "the test is not vacuous": it has to be
	// possible to make it pass on purpose, and only on purpose.
	allowed := map[string]bool{
		"plugins/aliased/plugin.go":     true,
		"plugins/dotimport/plugin.go":   true,
		"plugins/shadowed/plugin.go":    true,
		"plugins/unannounced/plugin.go": true,
	}
	if rest := undecidedWrites(found, "plugins/", allowed); len(rest) != 0 {
		t.Errorf("allowlisting every writer left %d undecided: %+v", len(rest), rest)
	}
	delete(allowed, "plugins/unannounced/plugin.go")
	rest := undecidedWrites(found, "plugins/", allowed)
	if len(rest) != 1 || rest[0].Path != "unannounced/plugin.go" {
		t.Errorf("dropping one allowlist row should re-expose exactly that file, got %+v", rest)
	}
}
