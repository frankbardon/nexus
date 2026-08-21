package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// This file is the answer to the risk this story created: the instance IO
// envelope now has a SECOND consumer.
//
// Until now `ioMessage` (plugins/io/broker/server.go) had exactly one reader —
// the client at the far end of the broker's WebSocket — and the broker itself
// forwarded the payload without ever decoding it. The A2A ingress decodes it,
// which means a field added, renamed or retyped over there can now silently
// stop being understood over here, and the symptom would be a turn that quietly
// loses part of itself rather than a build failure.
//
// The mirror cannot be replaced by an import: `ioMessage` is unexported, and
// pulling a plugin package into this binary would drag the whole engine in for
// one struct. So the two are kept in step by READING THE SOURCE: the tests
// below parse plugins/io/broker/server.go and compare the declaration there,
// field by field, against brokerIOMessage's reflected shape.
//
// A drift therefore fails `make test` naming the offending field, which is the
// only property worth having here — a mirror nobody checks is a mirror that has
// already drifted.

// brokerPluginSourcePath is the file the instance-side declarations live in.
// Relative from cmd/nexus-broker.
const brokerPluginSourcePath = "../../plugins/io/broker/server.go"

// TestIOMessageMirrorsThePluginDeclaration is the contract: brokerIOMessage and
// plugins/io/broker's ioMessage must agree on every field's name, Go type and
// JSON tag, in the same order.
//
// ORDER is compared as well as content, and deliberately: the two structs are
// meant to be diffable by eye, and a reordering that nobody noticed is exactly
// the state in which the next real change gets applied to the wrong place.
func TestIOMessageMirrorsThePluginDeclaration(t *testing.T) {
	for _, tc := range []struct {
		mirror   any
		declared string
	}{
		{mirror: brokerIOMessage{}, declared: "ioMessage"},
		{mirror: brokerIOChoice{}, declared: "ioChoice"},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			want, err := parseStructFields(brokerPluginSourcePath, tc.declared)
			if err != nil {
				t.Fatalf("reading the instance-side declaration: %v", err)
			}
			got := reflectStructFields(tc.mirror)

			if len(got) != len(want) {
				t.Errorf("%s: the mirror has %d field(s), the plugin declares %d\n mirror: %v\n plugin: %v\n"+
					"Add the missing field to %T (and teach a2aTask.deliver what it means, or say in a "+
					"comment why it is deliberately unmapped).",
					tc.declared, len(got), len(want), fieldNames(got), fieldNames(want), tc.mirror)
			}
			n := len(got)
			if len(want) < n {
				n = len(want)
			}
			for i := 0; i < n; i++ {
				if got[i] != want[i] {
					t.Errorf("%s field %d: the mirror declares %s, the plugin declares %s",
						tc.declared, i, got[i], want[i])
				}
			}
		})
	}
}

// TestIOMessageEncodesTheSameJSON is the wire half of the same contract: a
// fully-populated payload must serialize to exactly the key set the instance
// side produces, so a mirror that agreed structurally but disagreed about
// omitempty would still be caught.
func TestIOMessageEncodesTheSameJSON(t *testing.T) {
	full := brokerIOMessage{
		Type: "hitl.request", TurnID: "turn-1", Content: "c", Role: "assistant",
		FinishReason: "end_turn", State: "thinking", Detail: "d",
		PromptID: "p", Description: "desc", ToolCall: "tc", Risk: "low",
		Approved: true, Always: true,
		RequestID: "q-1", Prompt: "why?", Mode: "choices",
		Choices:  []brokerIOChoice{{ID: "yes", Label: "Yes"}},
		ChoiceID: "yes", FreeText: "ft",
		Resumable: boolPtr(true), Source: "broker",
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}

	want, err := parseStructFields(brokerPluginSourcePath, "ioMessage")
	if err != nil {
		t.Fatalf("reading the instance-side declaration: %v", err)
	}
	for _, f := range want {
		if _, present := decoded[f.jsonName]; !present {
			t.Errorf("a fully-populated payload does not encode %q, which the plugin declares", f.jsonName)
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("a fully-populated payload encodes %d key(s), the plugin declares %d fields", len(decoded), len(want))
	}
}

// TestUnknownIOFieldsAndTypesAreIgnored pins the leniency the story requires: a
// payload carrying something this broker does not understand must be ignored,
// never a task failure.
//
// Both halves matter. An unknown FIELD means the instance is newer than the
// broker in front of it, which is a supported deployment. An unknown TYPE means
// the instance is speaking to another consumer of the same envelope — the
// browser UI a broker client runs — and a turn must not die because a payload
// went past that was not addressed to A2A.
func TestUnknownIOFieldsAndTypesAreIgnored(t *testing.T) {
	msg, err := decodeIOPayload(json.RawMessage(
		`{"type":"output","content":"hello","turn_id":"t1","a_field_from_the_future":{"nested":true}}`))
	if err != nil {
		t.Fatalf("a payload with an unknown field must decode, got: %v", err)
	}
	if msg.Content != "hello" || msg.TurnID != "t1" {
		t.Errorf("known fields were not decoded: %+v", msg)
	}

	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "hello")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: "status", State: "thinking", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: "a.type.from.the.future", Content: "ignore me", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: "code.exec.stdout", Content: "not mine", TurnID: "t1"})

	if task.terminated() {
		t.Fatal("an unknown payload type failed the task; it must be ignored")
	}
	if state := task.snapshotTask().Status.State; state != a2a.TaskStateWorking {
		t.Errorf("task state = %s, want WORKING: an unknown payload must not move the task", state)
	}

	// And the turn still completes normally afterwards, so the unknown payloads
	// left nothing behind.
	instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "done", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	if !task.terminated() {
		t.Error("the turn did not complete after the unknown payloads")
	}
}

// ---- source parsing ----

// structField is one field of a struct declaration, reduced to what the two
// sides must agree on.
type structField struct {
	name     string
	goType   string
	jsonName string
	omitted  bool
}

func (f structField) String() string {
	tag := f.jsonName
	if f.omitted {
		tag += ",omitempty"
	}
	return fmt.Sprintf("%s %s `json:%q`", f.name, f.goType, tag)
}

func fieldNames(fields []structField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.name)
	}
	return out
}

// parseStructFields reads one struct declaration out of a Go source file.
//
// It parses rather than reflects because the declaration is unexported in a
// package this binary must not import. Type names are compared as SOURCE TEXT,
// with the mirror's package qualifiers stripped on the other side, so a field
// that changed from string to []string is caught even though both sides would
// reflect as different types anyway.
func parseStructFields(path, name string) ([]structField, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.FromSlash(path), nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var found *ast.StructType
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name == nil || spec.Name.Name != name {
			return true
		}
		if st, ok := spec.Type.(*ast.StructType); ok {
			found = st
		}
		return false
	})
	if found == nil {
		return nil, fmt.Errorf("%s declares no struct named %q; has it been renamed or moved?", path, name)
	}

	var fields []structField
	for _, f := range found.Fields.List {
		typeText, err := exprText(fset, path, f.Type)
		if err != nil {
			return nil, err
		}
		jsonName, omitted := jsonTag(f.Tag)
		for _, ident := range f.Names {
			fields = append(fields, structField{
				name:     ident.Name,
				goType:   typeText,
				jsonName: jsonName,
				omitted:  omitted,
			})
		}
	}
	return fields, nil
}

// exprText renders a type expression as the source text it was written as.
func exprText(fset *token.FileSet, path string, expr ast.Expr) (string, error) {
	src, err := readSource(path)
	if err != nil {
		return "", err
	}
	start := fset.Position(expr.Pos()).Offset
	end := fset.Position(expr.End()).Offset
	if start < 0 || end > len(src) || start > end {
		return "", fmt.Errorf("%s: cannot render a type expression", path)
	}
	return string(src[start:end]), nil
}

var sourceCache = map[string][]byte{}

func readSource(path string) ([]byte, error) {
	if src, ok := sourceCache[path]; ok {
		return src, nil
	}
	src, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		return nil, err
	}
	sourceCache[path] = src
	return src, nil
}

// jsonTag extracts the json name and the omitempty flag from a struct tag.
func jsonTag(tag *ast.BasicLit) (string, bool) {
	if tag == nil {
		return "", false
	}
	unquoted, err := strconv.Unquote(tag.Value)
	if err != nil {
		return "", false
	}
	value := reflect.StructTag(unquoted).Get("json")
	parts := strings.Split(value, ",")
	name := parts[0]
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			return name, true
		}
	}
	return name, false
}

// reflectStructFields renders the mirror's shape in the same vocabulary.
//
// Package qualifiers are stripped from type names ("[]broker.ioChoice" and
// "[]brokerIOChoice" both become "[]ioChoice"-shaped once the local prefixes
// are normalized) so the two naming conventions do not read as a difference.
func reflectStructFields(v any) []structField {
	rt := reflect.TypeOf(v)
	out := make([]structField, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		parts := strings.Split(f.Tag.Get("json"), ",")
		omitted := false
		for _, opt := range parts[1:] {
			if opt == "omitempty" {
				omitted = true
			}
		}
		out = append(out, structField{
			name:     f.Name,
			goType:   normalizeTypeName(f.Type.String()),
			jsonName: parts[0],
			omitted:  omitted,
		})
	}
	return out
}

// normalizeTypeName reduces a type name to the form both sides spell the same.
//
// The two declarations name the SAME wire shape with different Go identifiers
// (brokerIOChoice here, ioChoice there) and reflect qualifies with a package
// path. Both are normalized away, so the comparison is about the wire shape
// rather than about naming conventions — while a genuine change (a string
// becoming a slice, a value becoming a pointer) still shows.
func normalizeTypeName(name string) string {
	name = strings.ReplaceAll(name, "main.", "")
	name = strings.ReplaceAll(name, "broker.", "")
	name = strings.ReplaceAll(name, "brokerIOChoice", "ioChoice")
	return name
}

func boolPtr(b bool) *bool { return &b }
