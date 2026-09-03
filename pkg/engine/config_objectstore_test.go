package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// fakeObjectStore is the smallest thing that satisfies the seam. It exists so
// these tests can prove config resolution without any cloud dependency —
// which is the whole point of the registry being name-based.
type fakeObjectStore struct{}

func (fakeObjectStore) Hydrate(context.Context, string, string) error { return nil }
func (fakeObjectStore) Put(context.Context, string, string) error     { return nil }
func (fakeObjectStore) Delete(context.Context, string) error          { return nil }
func (fakeObjectStore) List(context.Context, string) ([]objectstore.Object, error) {
	return nil, nil
}
func (fakeObjectStore) Flush(context.Context) error { return nil }

// registerFakeBackend stands in for the blank import an embedder writes.
// There is deliberately no exported unregister — production code has no
// business removing a driver — so every test uses its own unique name and the
// registration simply lives for the rest of the test binary.
func registerFakeBackend(t *testing.T, name string) {
	t.Helper()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return fakeObjectStore{}, nil
	})
}

func TestDefaultConfig_ObjectStoreDisabled(t *testing.T) {
	c := DefaultConfig()
	if c.Core.ObjectStore.Enabled() {
		t.Fatal("DefaultConfig enables the object store; the default path must stay local-only")
	}
	if c.Core.ObjectStore != (objectstore.Config{}) {
		t.Errorf("DefaultConfig object store = %+v, want the zero value", c.Core.ObjectStore)
	}
}

func TestLoadConfig_NoObjectStoreBlockIsZeroValued(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte("core:\n  log_level: info\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.Core.ObjectStore != (objectstore.Config{}) {
		t.Errorf("object store = %+v, want zero value for a config that never mentions it", cfg.Core.ObjectStore)
	}
}

func TestLoadConfig_ObjectStoreResolvesRegisteredBackend(t *testing.T) {
	registerFakeBackend(t, "fake-resolve")

	cfg, err := LoadConfigFromBytes([]byte(`
core:
  object_store:
    backend: fake-resolve
    bucket: nexus-sessions
    prefix: prod/nexus
    region: us-east-1
    endpoint: http://127.0.0.1:9000
    failure_policy: strict
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	got := cfg.Core.ObjectStore
	want := objectstore.Config{
		BackendName:   "fake-resolve",
		Bucket:        "nexus-sessions",
		Prefix:        "prod/nexus",
		Region:        "us-east-1",
		Endpoint:      "http://127.0.0.1:9000",
		FailurePolicy: objectstore.FailurePolicyStrict,
	}
	if got != want {
		t.Errorf("object store = %+v, want %+v", got, want)
	}

	// Resolution is by name only — no core change is needed to add a backend.
	b, err := objectstore.Open(context.Background(), got)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b == nil {
		t.Fatal("Open returned a nil Backend for a config that loaded cleanly")
	}
}

func TestLoadConfig_ObjectStoreDefaultsFailurePolicyToDegrade(t *testing.T) {
	registerFakeBackend(t, "fake-policy")
	cfg, err := LoadConfigFromBytes([]byte(`
core:
  object_store:
    backend: fake-policy
    bucket: b
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if got := cfg.Core.ObjectStore.FailurePolicy; got != objectstore.FailurePolicyDegrade {
		t.Errorf("failure_policy = %q, want %q", got, objectstore.FailurePolicyDegrade)
	}
}

func TestLoadConfig_UnknownObjectStoreBackendFailsAtLoad(t *testing.T) {
	// The headline guarantee: an unimported backend module is a boot failure
	// with the key in the message, not a surprise at the first write.
	_, err := LoadConfigFromBytes([]byte(`
core:
  object_store:
    backend: gcs
    bucket: b
`))
	if err == nil {
		t.Fatal("LoadConfigFromBytes accepted an unregistered backend")
	}
	for _, want := range []string{"core.object_store.backend", "gcs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoadConfig_ObjectStoreKeyErrorsNameTheKey(t *testing.T) {
	registerFakeBackend(t, "fake-errs")
	cases := []struct {
		name    string
		yaml    string
		wantKey string
	}{
		{
			name:    "missing bucket",
			yaml:    "core:\n  object_store:\n    backend: fake-errs\n",
			wantKey: "core.object_store.bucket",
		},
		{
			name:    "invalid failure policy",
			yaml:    "core:\n  object_store:\n    backend: fake-errs\n    bucket: b\n    failure_policy: explode\n",
			wantKey: "core.object_store.failure_policy",
		},
		{
			name:    "prefix with a leading slash",
			yaml:    "core:\n  object_store:\n    backend: fake-errs\n    bucket: b\n    prefix: /leading\n",
			wantKey: "core.object_store.prefix",
		},
		{
			name:    "bucket set with no backend named",
			yaml:    "core:\n  object_store:\n    bucket: b\n",
			wantKey: "core.object_store.bucket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("LoadConfigFromBytes accepted an invalid object-store block")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name %q", err, tc.wantKey)
			}
		})
	}
}

func TestLoadConfig_ObjectStoreCredentialsFileExpanded(t *testing.T) {
	registerFakeBackend(t, "fake-creds")
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	cfg, err := LoadConfigFromBytes([]byte(`
core:
  object_store:
    backend: fake-creds
    bucket: b
    credentials_file: ~/.nexus/creds.json
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	want := filepath.Join(home, ".nexus", "creds.json")
	if got := cfg.Core.ObjectStore.CredentialsFile; got != want {
		t.Errorf("credentials_file = %q, want %q (engine.ExpandPath must run at the read site)", got, want)
	}
}

func TestLoadConfig_ObjectStorePrefixIsNotPathExpanded(t *testing.T) {
	// prefix is an object key, not a filesystem path. Expanding a leading "~"
	// here would silently rewrite the bucket layout to a local home directory.
	registerFakeBackend(t, "fake-prefix")
	cfg, err := LoadConfigFromBytes([]byte(`
core:
  object_store:
    backend: fake-prefix
    bucket: b
    prefix: "~tenant/nexus"
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if got := cfg.Core.ObjectStore.Prefix; got != "~tenant/nexus" {
		t.Errorf("prefix = %q, want it left verbatim", got)
	}
}
