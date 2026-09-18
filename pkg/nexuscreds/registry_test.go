package nexuscreds

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// registerForTest registers f under name and removes it when the test ends. The
// registry is process-global, so every test that touches it has to clean up or
// the next one sees a registry it did not build.
func registerForTest(t *testing.T, name string, f Factory) {
	t.Helper()
	Register(name, f)
	t.Cleanup(func() { Unregister(name) })
}

// mustPanic runs fn and fails unless it panicked with a message containing want.
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", want)
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panicked with %T (%v), want a string message", r, r)
		}
		if !strings.Contains(msg, want) {
			t.Fatalf("panic message %q does not contain %q", msg, want)
		}
	}()
	fn()
}

func TestRegisterOpen_RoundTrip(t *testing.T) {
	registerForTest(t, "roundtrip", func(map[string]any) (Source, error) {
		return staticSource{token: "abc123"}, nil
	})

	src, err := Open("roundtrip", nil)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	if tok != "abc123" {
		t.Fatalf("Token = %q, want %q", tok, "abc123")
	}
}

func TestOpen_PassesConfigToFactory(t *testing.T) {
	var got map[string]any
	registerForTest(t, "cfgcapture", func(cfg map[string]any) (Source, error) {
		got = cfg
		return staticSource{}, nil
	})

	want := map[string]any{"audience": "vertex", "scopes": []any{"cloud-platform"}}
	if _, err := Open("cfgcapture", want); err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("factory received a nil config, want the map Open was given")
	}
	if got["audience"] != "vertex" {
		t.Fatalf("factory config[audience] = %v, want %q", got["audience"], "vertex")
	}
}

func TestOpen_PassesNilConfigThrough(t *testing.T) {
	called := false
	registerForTest(t, "nilcfg", func(cfg map[string]any) (Source, error) {
		called = true
		if cfg != nil {
			t.Errorf("factory config = %#v, want nil passed through untouched", cfg)
		}
		return staticSource{}, nil
	})

	if _, err := Open("nilcfg", nil); err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if !called {
		t.Fatal("factory was never called")
	}
}

func TestRegister_PanicsOnEmptyName(t *testing.T) {
	mustPanic(t, "empty source name", func() {
		Register("", func(map[string]any) (Source, error) { return staticSource{}, nil })
	})
}

func TestRegister_PanicsOnNilFactory(t *testing.T) {
	mustPanic(t, "nil Factory for source nilfactory", func() {
		Register("nilfactory", nil)
	})
	if Registered("nilfactory") {
		t.Fatal("a panicking Register left the source in the registry")
	}
}

func TestRegister_PanicsOnDuplicateName(t *testing.T) {
	registerForTest(t, "dupe", func(map[string]any) (Source, error) {
		return staticSource{}, nil
	})

	mustPanic(t, "Register called twice for source dupe", func() {
		Register("dupe", func(map[string]any) (Source, error) { return staticSource{}, nil })
	})
}

func TestOpen_UnknownNameNamesTheMissingBlankImport(t *testing.T) {
	registerForTest(t, "present", func(map[string]any) (Source, error) {
		return staticSource{}, nil
	})

	src, err := Open("absent", nil)
	if err == nil {
		t.Fatal("Open with an unregistered name returned no error")
	}
	if src != nil {
		t.Fatalf("Open returned a Source alongside an error: %#v", src)
	}
	msg := err.Error()
	for _, want := range []string{`"absent"`, "blank-import", "present"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestOpen_UnknownNameSaysNothingIsImportedWhenRegistryIsEmpty(t *testing.T) {
	if got := Sources(); len(got) != 0 {
		t.Fatalf("precondition: registry is not empty, holds %v — a test leaked a registration", got)
	}

	_, err := Open("google-adc", nil)
	if err == nil {
		t.Fatal("Open against an empty registry returned no error")
	}
	if !strings.Contains(err.Error(), "no credential-source package is imported into this build") {
		t.Fatalf("error %q does not name the empty-registry cause", err)
	}
}

func TestOpen_EmptyNameIsAnErrorNotAPanic(t *testing.T) {
	src, err := Open("", nil)
	if err == nil {
		t.Fatal("Open with an empty name returned no error")
	}
	if src != nil {
		t.Fatalf("Open returned a Source alongside an error: %#v", src)
	}
	if !strings.Contains(err.Error(), "no credential source name given") {
		t.Fatalf("error %q does not name the empty-name cause", err)
	}
}

func TestOpen_WrapsFactoryError(t *testing.T) {
	want := errors.New("metadata server unreachable")
	registerForTest(t, "failing", func(map[string]any) (Source, error) { return nil, want })

	_, err := Open("failing", nil)
	if !errors.Is(err, want) {
		t.Fatalf("Open err = %v, want it to wrap %v", err, want)
	}
	if !strings.Contains(err.Error(), `"failing"`) {
		t.Fatalf("error %q does not name the source it came from", err)
	}
}

func TestOpen_RejectsNilSourceWithNoError(t *testing.T) {
	registerForTest(t, "liar", func(map[string]any) (Source, error) { return nil, nil })

	src, err := Open("liar", nil)
	if err == nil {
		t.Fatal("Open accepted a factory that returned (nil, nil)")
	}
	if src != nil {
		t.Fatalf("Open returned a Source alongside an error: %#v", src)
	}
	if !strings.Contains(err.Error(), "nil Source with no error") {
		t.Fatalf("error %q does not name the nil-Source cause", err)
	}
}

func TestRegistered_TrueForARegisteredName(t *testing.T) {
	registerForTest(t, "known", func(map[string]any) (Source, error) {
		return staticSource{}, nil
	})

	if !Registered("known") {
		t.Fatal("Registered(known) = false, want true")
	}
}

func TestRegistered_FalseForAnUnknownName(t *testing.T) {
	if Registered("never-registered") {
		t.Fatal("Registered(never-registered) = true, want false")
	}
}

func TestUnregister_AllowsAFakeToBeSwapped(t *testing.T) {
	// The mutex-guarded (not write-once) map exists so a test can substitute a
	// fake and put things back. This is that path.
	Register("swappable", func(map[string]any) (Source, error) {
		return staticSource{token: "first"}, nil
	})
	t.Cleanup(func() { Unregister("swappable") })

	Unregister("swappable")
	Register("swappable", func(map[string]any) (Source, error) {
		return staticSource{token: "second"}, nil
	})

	src, err := Open("swappable", nil)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	if tok != "second" {
		t.Fatalf("Token = %q, want the swapped-in %q", tok, "second")
	}
}

func TestUnregister_IsANoOpForAnUnknownName(t *testing.T) {
	Unregister("never-registered") // must not panic
	if Registered("never-registered") {
		t.Fatal("Unregister created an entry")
	}
}

func TestSources_ReturnsRegisteredNamesSorted(t *testing.T) {
	registerForTest(t, "zulu", func(map[string]any) (Source, error) { return staticSource{}, nil })
	registerForTest(t, "alpha", func(map[string]any) (Source, error) { return staticSource{}, nil })
	registerForTest(t, "mike", func(map[string]any) (Source, error) { return staticSource{}, nil })

	got := Sources()
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("Sources() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sources() = %v, want %v", got, want)
		}
	}
}
