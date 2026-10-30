package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// declaredVersionConsts parses this package's non-test sources and returns
// every package-level `<Name>Version` constant with its literal value. A
// version constant whose value is not an integer literal fails the test:
// the manifest guard can only vouch for what it can read.
func declaredVersionConsts(t *testing.T) map[string]int {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	out := make(map[string]int)
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					n := name.Name
					if !strings.HasSuffix(n, "Version") || n == "Version" || !ast.IsExported(n) {
						continue
					}
					if i >= len(vs.Values) {
						t.Fatalf("%s: constant %s has no explicit value", p, n)
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT {
						t.Fatalf("%s: constant %s is not an integer literal", p, n)
					}
					v, err := strconv.Atoi(lit.Value)
					if err != nil {
						t.Fatalf("%s: constant %s: %v", p, n, err)
					}
					out[strings.TrimSuffix(n, "Version")] = v
				}
			}
		}
	}
	return out
}

// TestVersionsManifestCoversEveryConstant is the drift guard for Versions:
// adding a `<Name>Version` constant without listing it in the manifest (or
// leaving a manifest entry behind after removing one) fails here.
func TestVersionsManifestCoversEveryConstant(t *testing.T) {
	declared := declaredVersionConsts(t)
	if len(declared) == 0 {
		t.Fatal("found no <Name>Version constants; parser walk is broken")
	}
	manifest := Versions()

	var problems []string
	for name, v := range declared {
		got, ok := manifest[name]
		switch {
		case !ok:
			problems = append(problems, name+"Version is declared but missing from the Versions manifest")
		case got != v:
			problems = append(problems, name+": manifest says "+strconv.Itoa(got)+", constant is "+strconv.Itoa(v))
		}
	}
	for name := range manifest {
		if _, ok := declared[name]; !ok {
			problems = append(problems, name+" is in the Versions manifest but no "+name+"Version constant exists")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// TestVersionsReturnsCopy pins that a caller mutating the returned map
// cannot change what the next caller sees.
func TestVersionsReturnsCopy(t *testing.T) {
	m := Versions()
	m["ToolCall"] = 999
	delete(m, "LLMRequest")
	again := Versions()
	if again["ToolCall"] != ToolCallVersion {
		t.Fatalf("ToolCall = %d after caller mutation, want %d", again["ToolCall"], ToolCallVersion)
	}
	if again["LLMRequest"] != LLMRequestVersion {
		t.Fatalf("LLMRequest = %d after caller delete, want %d", again["LLMRequest"], LLMRequestVersion)
	}
}

// TestVersionsConsumerPinExample is the pattern docs/src/architecture/events.md
// recommends for an embedder: one table naming the payloads it reads, one
// failure listing every one that moved. Here it pins the shipped values.
func TestVersionsConsumerPinExample(t *testing.T) {
	pinned := map[string]int{
		"ToolCall":   1,
		"LLMRequest": 2,
		"UserInput":  3,
	}
	current := Versions()
	for name, want := range pinned {
		got, ok := current[name]
		if !ok {
			t.Errorf("%s: no longer a versioned payload", name)
			continue
		}
		if got != want {
			t.Errorf("%s: pinned v%d, running v%d — read the release notes for this payload", name, want, got)
		}
	}
}
