package sampler

import (
	"bytes"
	"testing"
)

func TestIdentityRedactor_PassesThrough(t *testing.T) {
	r := IdentityRedactor{}
	in := []byte(`{"role":"user","content":"hello"}`)
	out, err := r.Redact("io.input", in)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("out=%q want=%q", out, in)
	}
}

func TestIdentityRedactor_HandlesNil(t *testing.T) {
	out, err := IdentityRedactor{}.Redact("any", nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out != nil {
		t.Errorf("out=%v, want nil", out)
	}
}

// dropRedactor is a stub illustrating how a non-identity redactor is allowed
// to drop a payload entirely. The plugin treats a nil return as "wipe the
// payload", which the snapshot post-pass test exercises.
type dropRedactor struct{}

func (dropRedactor) Redact(_ string, _ []byte) ([]byte, error) { return nil, nil }

func TestDropRedactor_ReturnsNilNoError(t *testing.T) {
	out, err := dropRedactor{}.Redact("llm.response", []byte(`{"foo":"bar"}`))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out != nil {
		t.Errorf("expected nil payload, got %q", out)
	}
}
