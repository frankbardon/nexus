//go:build wasip1

package main

import (
	"reflect"

	"github.com/traefik/yaegi/interp"
)

// sdkBindings returns the Yaegi symbol map for the nexus_sdk/* packages.
// Snippet code does `import "nexus_sdk/http"` and the imports resolve
// against this map, dispatching through the wasmimport bridge to the host.
//
// Path key format is yaegi's "<importpath>/<pkgname>" convention.
func sdkBindings() interp.Exports {
	return interp.Exports{
		"nexus_sdk/http/http": {
			"Get":          reflect.ValueOf(HTTPGet),
			"HTTPResponse": reflect.ValueOf((*HTTPResponse)(nil)).Elem(),
			"ErrCapDenied": reflect.ValueOf(&ErrCapDenied).Elem(),
			"IsCapDenied":  reflect.ValueOf(IsCapDenied),
		},
		"nexus_sdk/fs/fs": {
			"ReadFile":  reflect.ValueOf(FSReadFile),
			"WriteFile": reflect.ValueOf(FSWriteFile),
		},
		"nexus_sdk/exec/exec": {
			"Run":        reflect.ValueOf(ExecRun),
			"ExecResult": reflect.ValueOf((*ExecResult)(nil)).Elem(),
		},
		"nexus_sdk/env/env": {
			"Get": reflect.ValueOf(EnvGet),
		},
	}
}
