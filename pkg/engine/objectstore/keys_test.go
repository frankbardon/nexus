package objectstore

import (
	"errors"
	"testing"
)

func TestValidateKeyAcceptsLegalKeys(t *testing.T) {
	for _, key := range []string{
		"single",
		"a/b",
		"sessions/sess-1/metadata/session.json",
		"files/with spaces/and a name.txt",
		"files/...triple",
		"files/.hidden",
		"files/日本語/notes.md",
		"a/b.c/d.e.f",
		"~/looks-like-home", // a prefix is a key, not a path: no expansion, no rejection
	} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
}

func TestValidateKeyRejectsMalformedKeys(t *testing.T) {
	for _, key := range []string{
		"",
		"/leading",
		"trailing/",
		"double//slash",
		"has/./dot",
		"has/../dotdot",
		".",
		"..",
		"../escape",
		"escape/..",
		`windows\path`,
		"nul\x00byte",
	} {
		err := ValidateKey(key)
		if err == nil {
			t.Errorf("ValidateKey(%q) = nil, want an error", key)
			continue
		}
		// The sentinel is what lets a caller tell a malformed key (a bug, never
		// worth retrying) from a remote failure (retryable) without string
		// matching, and the contract suite requires backends to preserve it.
		if !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want it to wrap ErrInvalidKey", key, err)
		}
	}
}

func TestValidateKeyPrefixAllowsOnlyTheEmptyExtra(t *testing.T) {
	// The empty prefix legally means "everything the configured prefix covers".
	if err := ValidateKeyPrefix(""); err != nil {
		t.Errorf("ValidateKeyPrefix(\"\") = %v, want nil", err)
	}
	if err := ValidateKey(""); err == nil {
		t.Error("ValidateKey(\"\") = nil; an empty key names no object")
	}
	// Everything else is judged the same as a key.
	for _, prefix := range []string{"/leading", "trailing/", "a//b", "a/../b"} {
		if err := ValidateKeyPrefix(prefix); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKeyPrefix(%q) = %v, want ErrInvalidKey", prefix, err)
		}
	}
	if err := ValidateKeyPrefix("sessions/sess-1"); err != nil {
		t.Errorf("ValidateKeyPrefix(%q) = %v, want nil", "sessions/sess-1", err)
	}
}

func TestTrimKeyPrefix(t *testing.T) {
	cases := []struct {
		key     string
		prefix  string
		wantRel string
		wantOK  bool
	}{
		// The empty prefix covers everything, and the key is its own remainder.
		{"a/b/c", "", "a/b/c", true},
		{"", "", "", false},

		{"sessions/sess-1/files/a.txt", "sessions/sess-1", "files/a.txt", true},
		{"sessions/sess-1/a.txt", "sessions", "sess-1/a.txt", true},

		// The collision the engine's key scheme actually produces. Raw string
		// matching would hydrate sess-10's objects into sess-1's tree.
		{"sessions/sess-10/files/b.txt", "sessions/sess-1", "", false},
		{"sessionsX/c.txt", "sessions", "", false},

		// An object at exactly the prefix has no path beneath a hydration
		// destination, so it is not under it.
		{"sessions/sess-1", "sessions/sess-1", "", false},

		{"other/d.txt", "sessions", "", false},
		{"a", "a/b", "", false},
	}
	for _, tc := range cases {
		rel, ok := TrimKeyPrefix(tc.key, tc.prefix)
		if rel != tc.wantRel || ok != tc.wantOK {
			t.Errorf("TrimKeyPrefix(%q, %q) = (%q, %v), want (%q, %v)",
				tc.key, tc.prefix, rel, ok, tc.wantRel, tc.wantOK)
		}
	}
}
