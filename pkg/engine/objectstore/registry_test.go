package objectstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubBackend is the minimum a registration test needs. The real in-memory
// backend and the shared contract suite land in a later story; this only has
// to prove the seam is satisfiable, which is also a compile-time check that
// Backend has no unimplementable method.
type stubBackend struct{}

func (stubBackend) Hydrate(context.Context, string, string) error  { return nil }
func (stubBackend) Put(context.Context, string, string) error      { return nil }
func (stubBackend) Delete(context.Context, string) error           { return nil }
func (stubBackend) List(context.Context, string) ([]Object, error) { return nil, nil }
func (stubBackend) Flush(context.Context) error                    { return nil }
func stubFactory(context.Context, Config) (Backend, error)         { return stubBackend{}, nil }
func failingFactory(context.Context, Config) (Backend, error)      { return nil, errors.New("dial failed") }
func nilFactory(context.Context, Config) (Backend, error)          { return nil, nil }

// Compile-time proof that the interface is satisfiable by a plain struct with
// no cloud dependency — the property that lets a third party implement it.
var _ Backend = stubBackend{}

// registerTemp registers a backend for the duration of one test. The registry
// is process-global, so leaking a fake would make later tests order-dependent.
func registerTemp(t *testing.T, name string, f Factory) {
	t.Helper()
	Register(name, f)
	t.Cleanup(func() { unregister(name) })
}

func TestRegisterMakesBackendSelectable(t *testing.T) {
	if Registered("stub") {
		t.Fatal("stub registered before the test ran; registry leaked from another test")
	}
	registerTemp(t, "stub", stubFactory)

	if !Registered("stub") {
		t.Error("Registered(stub) = false after Register")
	}
	found := false
	for _, n := range Backends() {
		if n == "stub" {
			found = true
		}
	}
	if !found {
		t.Errorf("Backends() = %v, want it to contain stub", Backends())
	}
}

func TestRegisterPanics(t *testing.T) {
	cases := []struct {
		name string
		run  func()
	}{
		{"empty name", func() { Register("", stubFactory) }},
		{"nil factory", func() { Register("nilf", nil) }},
		{"duplicate", func() { Register("dup", stubFactory); Register("dup", stubFactory) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { unregister("dup"); unregister("nilf") })
			defer func() {
				if recover() == nil {
					t.Error("expected panic, got none")
				}
			}()
			tc.run()
		})
	}
}

func TestOpenDisabledConfigReturnsNilBackend(t *testing.T) {
	// The zero-impact default: a disabled config must not reach any backend
	// code at all, and the caller's check is a plain nil test.
	b, err := Open(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Open(zero config) error = %v, want nil", err)
	}
	if b != nil {
		t.Errorf("Open(zero config) = %v, want nil Backend", b)
	}
}

func TestOpenResolvesRegisteredBackend(t *testing.T) {
	registerTemp(t, "stub-open", stubFactory)
	b, err := Open(context.Background(), Config{BackendName: "stub-open", Bucket: "b"})
	if err != nil {
		t.Fatalf("Open error = %v", err)
	}
	if b == nil {
		t.Fatal("Open returned a nil Backend for a registered name")
	}
}

func TestOpenUnregisteredBackendFails(t *testing.T) {
	_, err := Open(context.Background(), Config{BackendName: "nope", Bucket: "b"})
	if err == nil {
		t.Fatal("Open(unregistered) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q does not name the backend", err)
	}
}

func TestOpenPropagatesFactoryFailure(t *testing.T) {
	registerTemp(t, "stub-fail", failingFactory)
	_, err := Open(context.Background(), Config{BackendName: "stub-fail", Bucket: "b"})
	if err == nil || !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("Open error = %v, want it to wrap the factory error", err)
	}
}

func TestOpenRejectsNilBackendWithoutError(t *testing.T) {
	registerTemp(t, "stub-nil", nilFactory)
	_, err := Open(context.Background(), Config{BackendName: "stub-nil", Bucket: "b"})
	if err == nil {
		t.Fatal("Open error = nil for a factory returning (nil, nil); want an error")
	}
}

func TestObjectCarriesOnlyPortableFields(t *testing.T) {
	// Unkeyed on purpose: it stops compiling the moment someone widens Object
	// with an ETag, a generation number or any other single-vendor concept,
	// which is the "no cloud-specific concepts" rule turned into a build gate.
	o := Object{"a/b", 3, time.Unix(0, 0).UTC()}
	if o.Key != "a/b" || o.Size != 3 {
		t.Fatalf("Object round-trip: %+v", o)
	}
}
