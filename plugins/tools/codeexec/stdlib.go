package codeexec

import (
	"reflect"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// defaultAllowedStdlib lists the Go stdlib packages scripts may import by
// default. The policy is "pure compute only" — packages that reach out to
// the filesystem, network, OS processes, reflection, or unsafe memory are
// excluded. Anything that needs those side-effects goes through the tools.*
// shim so gates still fire.
//
// io and bufio are included because their concrete types (Reader, Writer,
// Scanner, LimitReader, ...) are interface-driven and safe as long as
// scripts cannot obtain an *os.File — which they can't, since os is
// blocked.
var defaultAllowedStdlib = []string{
	// Formatting & parsing.
	"fmt",
	"strings",
	"strconv",
	"bytes",
	"unicode",
	"unicode/utf8",
	"unicode/utf16",
	"regexp",

	// Encoding (data interchange — no network, no disk).
	"encoding/json",
	"encoding/base64",
	"encoding/hex",
	"encoding/csv",
	"encoding/xml",
	"encoding/pem",
	"encoding/binary",

	// Crypto & hashing (pure compute — no network).
	"crypto/sha256",
	"crypto/sha512",
	"crypto/sha1",
	"crypto/md5",
	"crypto/hmac",
	"crypto/rand",
	"crypto/subtle",

	// Math & random.
	"math",
	"math/big",
	"math/rand",
	"math/rand/v2",
	"math/bits",

	// Collections & algorithms.
	"sort",
	"container/heap",
	"container/list",
	"container/ring",
	// slices and maps are intentionally excluded — Yaegi does not fully
	// support Go generics, so importing either produces "undefined type for
	// E/K" errors at compile time. Revisit when Yaegi adds generics.

	// Error handling.
	"errors",

	// Time.
	"time",

	// Control-flow / cancellation.
	"context",

	// Stream plumbing (interface-only; safe without *os.File).
	"io",
	"bufio",

	// Hash interfaces (SHA/MD5 consumers refer to hash.Hash).
	"hash",
	"hash/crc32",
	"hash/crc64",
	"hash/fnv",
	"hash/adler32",
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
