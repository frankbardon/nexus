package codeexec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
)

// scriptAnalysis is the output of staticAnalyze for a candidate Run script.
type scriptAnalysis struct {
	Imports []string // import paths referenced by the script
}

// staticAnalyze parses script source and enforces phase-1 sandbox rules before
// Yaegi sees it:
//
//  1. Must be package main.
//  2. Must declare func Run(ctx context.Context) (any, error) as-is.
//  3. No go statements (goroutines forbidden in phase 1).
//  4. No unsafe/syscall/os-family imports unless on allowedImports.
//
// Structural violations return an error. Script is rejected before any
// execution.
func staticAnalyze(script string, allowedImports map[string]bool) (*scriptAnalysis, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "script.go", script, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if file.Name == nil || file.Name.Name != "main" {
		return nil, fmt.Errorf("script must declare package main")
	}

	imports, err := validateImports(file, allowedImports)
	if err != nil {
		return nil, err
	}

	if err := validateRunSignature(file); err != nil {
		return nil, err
	}

	if err := rejectForbiddenStatements(file); err != nil {
		return nil, err
	}

	return &scriptAnalysis{Imports: imports}, nil
}

func validateImports(file *ast.File, allowed map[string]bool) ([]string, error) {
	var imports []string
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("import path %s: %w", imp.Path.Value, err)
		}
		if !allowed[path] {
			return nil, fmt.Errorf("import %q is not allowed", path)
		}
		imports = append(imports, path)
	}
	return imports, nil
}

func validateRunSignature(file *ast.File) error {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv != nil || fn.Name == nil || fn.Name.Name != "Run" {
			continue
		}
		// Arg list: (ctx context.Context).
		if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
			return fmt.Errorf("Run must accept exactly one parameter of type context.Context")
		}
		if !isContextContext(fn.Type.Params.List[0].Type) {
			return fmt.Errorf("Run's parameter must be context.Context")
		}
		// Return list: (any, error).
		if fn.Type.Results == nil || len(fn.Type.Results.List) != 2 {
			return fmt.Errorf("Run must return (any, error)")
		}
		if !isAny(fn.Type.Results.List[0].Type) {
			return fmt.Errorf("Run's first return value must be any/interface{}")
		}
		if !isIdent(fn.Type.Results.List[1].Type, "error") {
			return fmt.Errorf("Run's second return value must be error")
		}
		return nil
	}
	return fmt.Errorf("script must declare func Run(ctx context.Context) (any, error)")
}

// rejectForbiddenStatements walks the AST and fails on any `go` statement,
// which would spawn a goroutine inside the interpreter. Phase 1 disallows
// goroutines; revisit if/when we ship structured concurrency.
func rejectForbiddenStatements(file *ast.File) error {
	var found error
	ast.Inspect(file, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if _, ok := n.(*ast.GoStmt); ok {
			found = fmt.Errorf("go statements are not allowed in scripts (phase 1)")
			return false
		}
		return true
	})
	return found
}

// isContextContext returns true for the expression `context.Context`.
func isContextContext(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "context" && sel.Sel.Name == "Context"
}

func isAny(expr ast.Expr) bool {
	// `any` is an identifier alias for interface{} — accept either form.
	if isIdent(expr, "any") {
		return true
	}
	iface, ok := expr.(*ast.InterfaceType)
	if !ok {
		return false
	}
	return iface.Methods == nil || len(iface.Methods.List) == 0
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}
