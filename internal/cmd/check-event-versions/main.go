// Command check-event-versions enforces the schema-versioning rule on
// pkg/events/*.go: any commit that mutates an existing payload struct
// (removes a field, renames a field, or changes a field's type) MUST
// bump the matching `<Name>Version` constant. Additive changes (a new
// field with a sensible zero default) are allowed without a bump because
// they remain forward-compatible — older consumers ignore the unknown
// field, the round-trip preserves it.
//
// The check parses the working-tree pkg/events/*.go files and the same
// files at a base git revision (default: HEAD~1, or origin/main on CI).
// It compares struct shapes per declared name and the corresponding
// version constant. Disagreements without a bump exit non-zero with a
// human-readable diff.
//
// Heuristic — false positives (e.g., reordering fields) are tolerable;
// the operator just bumps the version trivially. False negatives (a
// rename that slips through) are the failure case the rule guards
// against. The position+type+name comparator catches that class.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	base := flag.String("base", "", "git revision to diff against; default HEAD~1, or $CHECK_EVENTS_BASE")
	pkgPath := flag.String("pkg", "pkg/events", "path to events package (relative to repo root)")
	verbose := flag.Bool("v", false, "verbose output")
	flag.Parse()

	if *base == "" {
		if env := os.Getenv("CHECK_EVENTS_BASE"); env != "" {
			*base = env
		} else {
			*base = "HEAD~1"
		}
	}

	repo, err := repoRoot()
	if err != nil {
		fail("locate repo root: %v", err)
	}
	absPkg := filepath.Join(repo, *pkgPath)

	files, err := goFilesIn(absPkg)
	if err != nil {
		fail("enumerate %s: %v", absPkg, err)
	}

	// Load current (working-tree) and base parses for every file.
	curStructs, curConsts, err := parseTree(files)
	if err != nil {
		fail("parse current: %v", err)
	}

	baseFiles := make(map[string]string, len(files))
	for _, f := range files {
		rel, _ := filepath.Rel(repo, f)
		content, err := gitShow(*base, rel)
		if err != nil {
			// File didn't exist at base — entirely new file, no rule applies.
			if *verbose {
				fmt.Fprintf(os.Stderr, "skip new file %s\n", rel)
			}
			continue
		}
		baseFiles[rel] = content
	}
	baseStructs, baseConsts, err := parseSources(baseFiles)
	if err != nil {
		fail("parse base: %v", err)
	}

	var failures []string
	for name, baseStruct := range baseStructs {
		curStruct, exists := curStructs[name]
		if !exists {
			// Deleted struct — this is a removal of an event type. Treat as
			// a breaking change unless the version constant was bumped (or
			// also removed alongside).
			baseV := baseConsts[name+"Version"]
			curV, hasCurV := curConsts[name+"Version"]
			if !hasCurV {
				// Constant also removed; coordinated deletion of the type
				// and its version. Allow.
				continue
			}
			if curV > baseV {
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: struct removed without bumping %sVersion (base=%d cur=%d)", name, name, baseV, curV))
			continue
		}
		breaking := diffStructs(baseStruct, curStruct)
		if len(breaking) == 0 {
			continue
		}
		baseV := baseConsts[name+"Version"]
		// Treat a missing constant at base as v1 baseline (the v0==v1
		// rule documented in pkg/events/doc.go). Bootstrap commits that
		// only ADD `SchemaVersion` and the version constant pass the
		// additive-only branch above; commits that also break shape
		// must move the constant past 1.
		if baseV == 0 {
			baseV = 1
		}
		curV := curConsts[name+"Version"]
		if curV > baseV {
			continue // bumped — OK.
		}
		for _, reason := range breaking {
			failures = append(failures, fmt.Sprintf("%s: %s without bumping %sVersion (currently %d at base, %d in working tree)", name, reason, name, baseV, curV))
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		fmt.Fprintln(os.Stderr, "check-event-versions: schema breaking changes detected without version bump:")
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  -", f)
		}
		fmt.Fprintln(os.Stderr, "\nFix: bump the matching <Name>Version constant in pkg/events/*.go and add a")
		fmt.Fprintln(os.Stderr, "compat migrator in pkg/events/compat/ if the change is semantic. See")
		fmt.Fprintln(os.Stderr, "docs/src/architecture/events.md for the full convention.")
		os.Exit(1)
	}
	if *verbose {
		fmt.Println("check-event-versions: OK")
	}
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitShow(rev, path string) (string, error) {
	out, err := exec.Command("git", "show", rev+":"+path).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func goFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// fieldShape is the comparable signature of one struct field.
type fieldShape struct {
	Name string
	Type string
}

type structShape struct {
	Fields []fieldShape
}

func parseTree(files []string) (map[string]structShape, map[string]int, error) {
	srcs := make(map[string]string, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		srcs[f] = string(b)
	}
	return parseSources(srcs)
}

func parseSources(srcs map[string]string) (map[string]structShape, map[string]int, error) {
	structs := make(map[string]structShape)
	consts := make(map[string]int)
	fset := token.NewFileSet()
	for path, src := range srcs {
		file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gen.Tok {
			case token.TYPE:
				for _, spec := range gen.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					structs[ts.Name.Name] = shapeOf(st)
				}
			case token.CONST:
				readConsts(gen, consts)
			}
		}
	}
	return structs, consts, nil
}

func shapeOf(st *ast.StructType) structShape {
	var fields []fieldShape
	for _, f := range st.Fields.List {
		typ := types.ExprString(f.Type)
		// Each *ast.Field can name multiple identifiers (e.g., `A, B int`).
		if len(f.Names) == 0 {
			fields = append(fields, fieldShape{Name: typ, Type: typ}) // embedded
			continue
		}
		for _, n := range f.Names {
			fields = append(fields, fieldShape{Name: n.Name, Type: typ})
		}
	}
	return structShape{Fields: fields}
}

func readConsts(gen *ast.GenDecl, into map[string]int) {
	// For `const ( Foo = 1; Bar = 2 )` the parser stitches values onto each
	// ValueSpec. Iota-driven blocks evaluate via a tiny manual walk.
	var iota int
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, ident := range vs.Names {
			val, ok := evalIntExpr(valueAt(vs, i), iota)
			if !ok {
				continue
			}
			into[ident.Name] = val
		}
		iota++
	}
}

func valueAt(vs *ast.ValueSpec, i int) ast.Expr {
	if i < len(vs.Values) {
		return vs.Values[i]
	}
	if len(vs.Values) == 1 {
		return vs.Values[0]
	}
	return nil
}

func evalIntExpr(e ast.Expr, iotaVal int) (int, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.INT {
			n, err := strconv.Atoi(v.Value)
			if err == nil {
				return n, true
			}
		}
	case *ast.Ident:
		if v.Name == "iota" {
			return iotaVal, true
		}
	}
	return 0, false
}

func diffStructs(base, cur structShape) []string {
	var problems []string
	baseByName := indexFields(base.Fields)
	curByName := indexFields(cur.Fields)

	// Removed or type-changed fields.
	for name, bf := range baseByName {
		cf, ok := curByName[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("removed field %q", name))
			continue
		}
		if bf.Type != cf.Type {
			problems = append(problems, fmt.Sprintf("field %q type changed (%s -> %s)", name, bf.Type, cf.Type))
		}
	}

	// Heuristic rename detection: if a field at position i in base no longer
	// exists in cur but a field with the SAME type appears at position i in
	// cur with a different name, flag as rename.
	for i := 0; i < len(base.Fields) && i < len(cur.Fields); i++ {
		bf := base.Fields[i]
		cf := cur.Fields[i]
		if bf.Name == cf.Name {
			continue
		}
		if _, stillExists := curByName[bf.Name]; stillExists {
			continue
		}
		if bf.Type == cf.Type && cf.Name != bf.Name {
			problems = append(problems, fmt.Sprintf("field renamed at position %d (%q -> %q, type %s)", i, bf.Name, cf.Name, bf.Type))
		}
	}

	return dedup(problems)
}

func indexFields(fields []fieldShape) map[string]fieldShape {
	out := make(map[string]fieldShape, len(fields))
	for _, f := range fields {
		out[f.Name] = f
	}
	return out
}

func dedup(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "check-event-versions: "+format+"\n", args...)
	os.Exit(1)
}
