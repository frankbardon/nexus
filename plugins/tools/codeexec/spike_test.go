package codeexec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// TestSpike_YaegiReflectBinding proves we can:
//  1. Build a struct type at runtime via reflect.StructOf from a JSON-ish schema.
//  2. Build a function value at runtime via reflect.MakeFunc.
//  3. Inject it into a Yaegi interpreter via Use() at a synthetic import path.
//  4. Have a script import that path, call the function with a struct literal, and
//     observe the result.
//
// If this passes, the core binding mechanism for run_code is sound and we can
// proceed with the full plugin implementation.
func TestSpike_YaegiReflectBinding(t *testing.T) {
	// 1. Build ShellExecArgs and ShellExecResult as runtime-defined structs.
	argsType := reflect.StructOf([]reflect.StructField{
		{Name: "Command", Type: reflect.TypeOf(""), Tag: `json:"command"`},
	})
	resultType := reflect.StructOf([]reflect.StructField{
		{Name: "Output", Type: reflect.TypeOf(""), Tag: `json:"output"`},
		{Name: "Error", Type: reflect.TypeOf(""), Tag: `json:"error"`},
	})
	errorType := reflect.TypeOf((*error)(nil)).Elem()

	// 2. Build a func(ShellExecArgs) (ShellExecResult, error) at runtime.
	// The handler captures the command and fabricates a result — stand-in for
	// the real bus round-trip.
	var seenCommand string
	funcType := reflect.FuncOf([]reflect.Type{argsType}, []reflect.Type{resultType, errorType}, false)
	funcVal := reflect.MakeFunc(funcType, func(in []reflect.Value) []reflect.Value {
		args := in[0]
		seenCommand = args.FieldByName("Command").String()
		result := reflect.New(resultType).Elem()
		result.FieldByName("Output").SetString("fake: " + seenCommand)
		return []reflect.Value{result, reflect.Zero(errorType)}
	})

	// 3. Spin up Yaegi and register the synthetic "tools" package.
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatalf("stdlib.Symbols: %v", err)
	}
	// Yaegi Exports key format is "importpath/pkgname".
	// Yaegi's convention: types are exported as typed-nil pointers; funcs as
	// reflect.Value of the func; vars as pointer-to-var.
	exports := interp.Exports{
		"tools/tools": {
			"ShellExec":       funcVal,
			"ShellExecArgs":   reflect.Zero(reflect.PointerTo(argsType)),
			"ShellExecResult": reflect.Zero(reflect.PointerTo(resultType)),
		},
	}
	if err := i.Use(exports); err != nil {
		t.Fatalf("Use tools: %v", err)
	}

	// 4. Run a script that imports it and calls the function.
	script := `
package main

import (
	"fmt"
	"tools"
)

func Run() (string, error) {
	r, err := tools.ShellExec(tools.ShellExecArgs{Command: "echo hi"})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("got=%s", r.Output), nil
}
`
	if _, err := i.Eval(script); err != nil {
		t.Fatalf("Eval script: %v", err)
	}

	// Invoke main.Run and capture its typed return.
	runVal, err := i.Eval("main.Run")
	if err != nil {
		t.Fatalf("resolve main.Run: %v", err)
	}
	out := runVal.Call(nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 return values, got %d", len(out))
	}
	if !out[1].IsNil() {
		t.Fatalf("Run returned error: %v", out[1].Interface())
	}
	got := out[0].String()
	if !strings.Contains(got, "fake: echo hi") {
		t.Fatalf("unexpected script output: %q", got)
	}
	if seenCommand != "echo hi" {
		t.Fatalf("handler saw command=%q, want %q", seenCommand, "echo hi")
	}
}
