package codeexec

import (
	"reflect"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// defaultAllowedStdlib lists the Go stdlib packages scripts may import in
// phase 1. Anything touching the network, filesystem, OS processes, reflection
// or unsafe memory is excluded by design — routes for that work live behind
// the tools.* bus shim so they still hit every gate.
var defaultAllowedStdlib = []string{
	"fmt",
	"strings",
	"strconv",
	"encoding/json",
	"math",
	"sort",
	"errors",
	"time",
	"context",
}

// filteredStdlibSymbols returns the subset of stdlib.Symbols whose package
// path is in allowed. The symbol-map key format yaegi uses is
// "importpath/pkgname", so we match on the importpath prefix before the
// final slash-separator.
func filteredStdlibSymbols(allowed []string) interp.Exports {
	allowedSet := make(map[string]bool, len(allowed))
	for _, p := range allowed {
		allowedSet[p] = true
	}

	out := interp.Exports{}
	for key, syms := range stdlib.Symbols {
		importPath := splitYaegiKey(key)
		if allowedSet[importPath] {
			// Shallow-copy to avoid accidentally mutating the global map.
			dup := make(map[string]reflect.Value, len(syms))
			for k, v := range syms {
				dup[k] = v
			}
			out[key] = dup
		}
	}
	return out
}

// splitYaegiKey extracts the import path from a yaegi symbol-map key. The
// key format is "importpath/pkgname" — we strip the trailing "/pkgname".
//
// e.g. "fmt/fmt" → "fmt", "encoding/json/json" → "encoding/json".
func splitYaegiKey(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return key
}
