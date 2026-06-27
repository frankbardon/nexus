package client

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseSlashArgs_Positional(t *testing.T) {
	decl := []*mcp.PromptArgument{
		{Name: "pr", Required: true},
		{Name: "verbose"},
	}
	got, err := parseSlashArgs("123 true", decl)
	if err != nil {
		t.Fatal(err)
	}
	if got["pr"] != "123" || got["verbose"] != "true" {
		t.Fatalf("got %#v", got)
	}
}

func TestParseSlashArgs_KeyValue(t *testing.T) {
	decl := []*mcp.PromptArgument{
		{Name: "pr", Required: true},
		{Name: "verbose"},
	}
	got, err := parseSlashArgs("verbose=false pr=42", decl)
	if err != nil {
		t.Fatal(err)
	}
	if got["pr"] != "42" || got["verbose"] != "false" {
		t.Fatalf("got %#v", got)
	}
}

func TestParseSlashArgs_Mixed(t *testing.T) {
	decl := []*mcp.PromptArgument{
		{Name: "pr", Required: true},
		{Name: "verbose"},
		{Name: "comment"},
	}
	got, err := parseSlashArgs(`42 comment="needs review" verbose=true`, decl)
	if err != nil {
		t.Fatal(err)
	}
	if got["pr"] != "42" || got["verbose"] != "true" || got["comment"] != "needs review" {
		t.Fatalf("got %#v", got)
	}
}

func TestParseSlashArgs_MissingRequired(t *testing.T) {
	decl := []*mcp.PromptArgument{{Name: "pr", Required: true}}
	if _, err := parseSlashArgs("", decl); err == nil {
		t.Fatal("expected error for missing required argument")
	}
}

func TestParseSlashArgs_UnknownKey(t *testing.T) {
	decl := []*mcp.PromptArgument{{Name: "pr"}}
	if _, err := parseSlashArgs("pr=1 bogus=2", decl); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestParseSlashArgs_UnterminatedQuote(t *testing.T) {
	decl := []*mcp.PromptArgument{{Name: "comment"}}
	if _, err := parseSlashArgs(`comment="missing`, decl); err == nil {
		t.Fatal("expected unterminated quote error")
	}
}

func TestParseSlashArgs_PositionalSkipsExplicitlySet(t *testing.T) {
	decl := []*mcp.PromptArgument{
		{Name: "a", Required: true},
		{Name: "b", Required: true},
	}
	// "a=first second" -> a gets "first" via kv, b gets "second" via positional
	got, err := parseSlashArgs("a=first second", decl)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "first" || got["b"] != "second" {
		t.Fatalf("got %#v", got)
	}
}
