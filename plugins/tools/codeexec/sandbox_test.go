package codeexec

import (
	"strings"
	"testing"
)

func TestStaticAnalyze_Valid(t *testing.T) {
	script := `package main

import (
	"context"
	"fmt"
)

func Run(ctx context.Context) (any, error) {
	return fmt.Sprintf("ok"), nil
}
`
	allowed := map[string]bool{"context": true, "fmt": true}
	got, err := staticAnalyze(script, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Imports) != 2 {
		t.Fatalf("want 2 imports, got %v", got.Imports)
	}
}

func TestStaticAnalyze_ParseError(t *testing.T) {
	script := `package main
func Run(ctx {`
	_, err := staticAnalyze(script, nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestStaticAnalyze_WrongPackage(t *testing.T) {
	script := `package foo
import "context"
func Run(ctx context.Context) (any, error) { return nil, nil }
`
	_, err := staticAnalyze(script, map[string]bool{"context": true})
	if err == nil || !strings.Contains(err.Error(), "package main") {
		t.Fatalf("want package main error, got %v", err)
	}
}

func TestStaticAnalyze_DisallowedImport(t *testing.T) {
	script := `package main
import (
	"context"
	"os"
)
func Run(ctx context.Context) (any, error) { return nil, nil }
`
	_, err := staticAnalyze(script, map[string]bool{"context": true})
	if err == nil || !strings.Contains(err.Error(), `"os"`) {
		t.Fatalf("want os import rejection, got %v", err)
	}
}

func TestStaticAnalyze_MissingRun(t *testing.T) {
	script := `package main
import "context"
func other(ctx context.Context) (any, error) { return nil, nil }
`
	_, err := staticAnalyze(script, map[string]bool{"context": true})
	if err == nil || !strings.Contains(err.Error(), "func Run") {
		t.Fatalf("want missing Run error, got %v", err)
	}
}

func TestStaticAnalyze_WrongRunSignature(t *testing.T) {
	cases := map[string]string{
		"missing ctx": `package main
func Run() (any, error) { return nil, nil }`,
		"wrong ctx type": `package main
func Run(x int) (any, error) { return nil, nil }`,
		"wrong first return": `package main
import "context"
func Run(ctx context.Context) (string, error) { return "", nil }`,
		"wrong second return": `package main
import "context"
func Run(ctx context.Context) (any, string) { return nil, "" }`,
		"no returns": `package main
import "context"
func Run(ctx context.Context) {}`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := staticAnalyze(script, map[string]bool{"context": true})
			if err == nil {
				t.Fatal("want signature rejection")
			}
		})
	}
}

func TestStaticAnalyze_RejectsGoStmt(t *testing.T) {
	script := `package main
import "context"
func Run(ctx context.Context) (any, error) {
	go func() {}()
	return nil, nil
}
`
	_, err := staticAnalyze(script, map[string]bool{"context": true})
	if err == nil || !strings.Contains(err.Error(), "go statements") {
		t.Fatalf("want go rejection, got %v", err)
	}
}

func TestStaticAnalyze_AcceptsInterfaceBrace(t *testing.T) {
	script := `package main
import "context"
func Run(ctx context.Context) (interface{}, error) { return nil, nil }
`
	if _, err := staticAnalyze(script, map[string]bool{"context": true}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
