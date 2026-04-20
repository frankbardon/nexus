package codeexec

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBroadStdlib_PureCompute exercises the broadened whitelist with a
// single multi-package script — regexp + encoding/base64 + crypto/sha256 +
// slices + bytes + bufio — to prove everything we advertise is actually
// importable and functional. If any of these packages are missing from the
// filtered stdlib symbols, the script fails at compile time.
func TestBroadStdlib_PureCompute(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

func Run(ctx context.Context) (any, error) {
	// regexp
	numberRE := regexp.MustCompile("[0-9]+")
	nums := numberRE.FindAllString("a1 b22 c333", -1)

	// bufio + bytes
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	for _, n := range nums {
		w.WriteString(n + "\n")
	}
	w.Flush()

	// base64 + sha256 + hex
	digest := sha256.Sum256([]byte("hello"))
	encoded := base64.StdEncoding.EncodeToString(digest[:])
	hexed := hex.EncodeToString(digest[:4])

	// sort
	reversed := make([]string, len(nums))
	copy(reversed, nums)
	sort.Sort(sort.Reverse(sort.StringSlice(reversed)))

	return map[string]any{
		"lines":      strings.TrimSpace(buf.String()),
		"digest_b64": encoded,
		"hex_prefix": hexed,
		"reversed":   reversed,
	}, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("script errored: %s", res.Error)
	}

	var env map[string]any
	if err := json.Unmarshal([]byte(res.Output), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	result, _ := env["result"].(map[string]any)

	if result["lines"] != "1\n22\n333" {
		t.Errorf("lines: %v", result["lines"])
	}
	// SHA-256 of "hello" = 2cf24dba... so hex prefix is 2cf24dba.
	if result["hex_prefix"] != "2cf24dba" {
		t.Errorf("hex_prefix: %v", result["hex_prefix"])
	}
	// Base64 of that digest should start with LPJNul (standard SHA-256 quick check).
	b64, _ := result["digest_b64"].(string)
	if !strings.HasPrefix(b64, "LPJNul") {
		t.Errorf("digest_b64: %q", b64)
	}

	reversed, _ := result["reversed"].([]any)
	if len(reversed) != 3 || reversed[0] != "333" {
		t.Errorf("reversed: %v", reversed)
	}
}

// The import allowlist must still reject blocked packages (os, net, etc.).
func TestBroadStdlib_StillBlocksDangerous(t *testing.T) {
	cases := []string{"os", "net/http", "os/exec", "syscall", "unsafe", "reflect", "runtime"}
	for _, pkg := range cases {
		t.Run(pkg, func(t *testing.T) {
			h := newHarness(t, nil)
			script := `package main
import (
	"context"
	"` + pkg + `"
)
func Run(ctx context.Context) (any, error) {
	_ = ` + shortIdent(pkg) + `
	return nil, nil
}
`
			res := h.runCode(script)
			if res.Error == "" || !strings.Contains(res.Error, pkg) {
				t.Fatalf("expected rejection of %q, got %q", pkg, res.Error)
			}
		})
	}
}

// shortIdent returns the package identifier portion of an import path so we
// can produce a compilable-looking reference in the test scripts (e.g.
// "net/http" → "http"). This is just so the generated script uses the
// imported package and doesn't trip an "imported and not used" diagnostic
// before it reaches the import rejection we're actually testing.
func shortIdent(pkg string) string {
	if idx := strings.LastIndex(pkg, "/"); idx != -1 {
		return pkg[idx+1:]
	}
	return pkg
}
