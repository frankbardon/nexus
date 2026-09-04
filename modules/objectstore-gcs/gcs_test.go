package gcsstore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"

	gcsstore "github.com/frankbardon/nexus/modules/objectstore-gcs"
)

// googleEnvVars is every environment variable that can influence Application
// Default Credentials, the endpoint, or the universe the SDK thinks it is in.
//
// The list is cleared before each test rather than trusted to be absent. These
// tests run on developer laptops with a gcloud ADC file and possibly an
// exported GOOGLE_APPLICATION_CREDENTIALS, and on CI runners with none of it; a
// test whose result depends on which of those it is on is not a test. Clearing
// also stops a real credential from ever reaching the loopback fake.
//
// STORAGE_EMULATOR_HOST is on the list because the SDK honours it directly,
// overriding both the endpoint and the credential choice this backend made. A
// developer with it exported for another tool would otherwise silently redirect
// every one of these tests.
var googleEnvVars = []string{
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_QUOTA_PROJECT",
	"GOOGLE_CLOUD_UNIVERSE_DOMAIN",
	"GOOGLE_API_USE_CLIENT_CERTIFICATE",
	"CLOUDSDK_CONFIG",
	// GCE_METADATA_HOST is on the list, and is deliberately *unset* rather
	// than pointed somewhere harmless. compute/metadata treats the variable
	// merely being set as proof the process is on GCE -- it short-circuits the
	// probe and returns true -- so the obvious isolation trick of aiming it at
	// a dead port does the exact opposite of what it looks like, and turns
	// every "no credentials" test into a multi-second metadata timeout.
	"GCE_METADATA_HOST",
	"STORAGE_EMULATOR_HOST",
	"STORAGE_EMULATOR_HOST_GRPC",
}

// isolateGoogleEnv unsets everything in googleEnvVars for the duration of the
// test and points HOME at an empty directory, so the well-known gcloud ADC file
// under $HOME/.config/gcloud cannot be found either.
//
// What it cannot isolate is a genuine GCE or GKE host, where the metadata
// server really does answer and Application Default Credentials really do
// resolve. compute/metadata memoises that answer once per process and offers no
// override, so the tests that assert "no credentials" would find some. CI runs
// on GitHub-hosted runners, which are not GCE; a developer running these on a
// Compute Engine VM should expect TestNoCredentialsAndNoEndpointFailsTheBoot
// and TestUnauthenticatedEmulatorPath to fail for that reason and no other.
func isolateGoogleEnv(t *testing.T) {
	t.Helper()
	for _, name := range googleEnvVars {
		if old, ok := os.LookupEnv(name); ok {
			t.Cleanup(func() { _ = os.Setenv(name, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(name) })
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
	// The well-known Application Default Credentials file lives at
	// $HOME/.config/gcloud, so an empty HOME is what hides a developer's own
	// gcloud login from the test.
	t.Setenv("HOME", t.TempDir())
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newBackend builds a backend pointed at f with no credentials, which is the
// unauthenticated emulator path resolveCredentials selects when an endpoint is
// set and nothing else is available.
func newBackend(t *testing.T, f *fakeGCS, mutate ...func(*objectstore.Config)) objectstore.Backend {
	t.Helper()
	isolateGoogleEnv(t)

	cfg := objectstore.Config{
		BackendName: gcsstore.BackendName,
		Bucket:      f.bucket,
		Endpoint:    f.endpoint(),
		Logger:      quietLogger(),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	b, err := gcsstore.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("gcsstore.New: %v", err)
	}
	if c, ok := b.(io.Closer); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	return b
}

// TestContractSuite is the acceptance criterion, and the reason this module
// exists beyond GCS support: a second independent backend held to the shared
// conformance suite, unmodified, is the evidence that objectstore.Backend is a
// general interface rather than the S3 API under different names.
//
// It runs against the loopback fake so it is part of the untagged `make test`
// sweep -- no container, no credential, no network. The same suite is run
// against fake-gcs-server behind a build tag by E5-S2, which is what closes the
// fidelity gap the fake cannot close on its own.
//
// No Option is passed other than the page-count reduction: in particular
// WithoutObjectAtPrefix is not needed, because GCS's key space is genuinely
// flat and an object at "sessions/sess-1" coexists with
// "sessions/sess-1/files/a.txt" exactly as the suite requires.
func TestContractSuite(t *testing.T) {
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		// A fresh fake per case, which is what makes the suite's per-case
		// isolation real: nothing is shared between cases, not even a bucket.
		return newBackend(t, newFakeGCS(t, "nexus-contract"))
	},
		// The fake's page size is 50 (see defaultFakePageSize), so 200 objects
		// span four pages and catch an iterator that is not drained. The
		// suite's 1200 default exists to clear the API's real 1000-object page
		// and is paid once, against fake-gcs-server, rather than 1200 times
		// inside `make test`.
		objectstoretest.WithListProbeCount(200),
	)
}

// TestContractSuiteUnderAConfiguredPrefix runs the whole suite again with a
// bucket prefix set.
//
// The prefix is the one piece of state that turns every key the suite uses into
// a different key on the wire, and a backend can pass the suite cleanly with a
// prefix bug -- applying it on Put but not on List, or stripping it with a raw
// string trim -- because the suite only ever sees store-relative keys. Running
// the suite twice is the cheapest way to cover both, and it is why
// TestKeyLayoutMirrorsTheLocalTree below can restrict itself to asserting the
// physical layout.
func TestContractSuiteUnderAConfiguredPrefix(t *testing.T) {
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return newBackend(t, newFakeGCS(t, "nexus-contract"), func(c *objectstore.Config) {
			c.Prefix = "prod/nexus"
		})
	}, objectstoretest.WithListProbeCount(200))
}

func TestRegistersItselfAsGCS(t *testing.T) {
	// The blank-import contract: importing this module must be enough for
	// `core.object_store.backend: gcs` to validate.
	if !objectstore.Registered(gcsstore.BackendName) {
		t.Fatalf("backend %q is not registered; the init-time Register did not run", gcsstore.BackendName)
	}
	cfg := objectstore.Config{BackendName: gcsstore.BackendName, Bucket: "b"}
	if err := cfg.Validate("core.object_store"); err != nil {
		t.Fatalf("Validate with a registered backend = %v, want nil", err)
	}
}

// awkwardKeys are the shapes the story calls out as having to round-trip, plus
// the ones a naive key mapping mangles. Deliberately the same set the S3
// module pins, because the two backends are specified to produce the same
// layout in the bucket and a divergence between these two lists would be the
// first sign that they had drifted.
var awkwardKeys = map[string]string{
	"metadata/session.json":                    `{"id":"s1"}`,
	"plugins/nexus.scene/scene.jsonl":          "{}\n",
	"plugins/nexus.vectorstore.chromem/x/y/z":  "nested plugin dirs are ordinary segments",
	"files/.hidden":                            "a dotfile",
	"plugins/nexus.memory.longterm/.gitignore": "a dotfile inside a nested plugin dir",
	"files/empty.txt":                          "",
	"files/with spaces/and a name.txt":         "spaces survive URL escaping",
	"files/日本語/notes.md":                       "so does a non-ASCII segment",
	"files/...triple":                          "dots are only special as whole segments",
	"ui-state.json":                            "a single-segment key at the root",
}

// TestKeyLayoutMirrorsTheLocalTree asserts the *physical* keys in the bucket,
// which is the half of the mapping the contract suite cannot see: the suite
// speaks store-relative keys on both sides, so it would pass against a backend
// that encoded or flattened keys internally.
//
// The property being pinned is that an operator can open the bucket in the
// Cloud console and read their session tree -- and, because these are the same
// keys the S3 backend produces, that a bucket can be copied between the two
// clouds with the vendors' own tools and no translation step.
func TestKeyLayoutMirrorsTheLocalTree(t *testing.T) {
	const prefix = "prod/nexus"
	f := newFakeGCS(t, "nexus-layout")
	b := newBackend(t, f, func(c *objectstore.Config) { c.Prefix = prefix })
	ctx := context.Background()

	src := t.TempDir()
	var want []string
	for key, content := range awkwardKeys {
		local := filepath.Join(src, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := b.Put(ctx, key, local); err != nil {
			t.Fatalf("Put(%q) = %v", key, err)
		}
		want = append(want, prefix+"/"+key)
	}
	slices.Sort(want)

	if got := f.keys(); !slices.Equal(got, want) {
		t.Errorf("bucket keys =\n%v\nwant\n%v", got, want)
	}

	// And the inverse: hydrating puts every one of them back at the local path
	// it came from, byte for byte.
	dest := t.TempDir()
	if err := b.Hydrate(ctx, "", dest); err != nil {
		t.Fatalf("Hydrate = %v", err)
	}
	for key, content := range awkwardKeys {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(key)))
		if err != nil {
			t.Errorf("hydrated %q: %v", key, err)
			continue
		}
		if string(got) != content {
			t.Errorf("hydrated %q = %q, want %q", key, got, content)
		}
	}
}

// TestConfiguredPrefixIsolatesNeighbours pins the segment-aware prefix rule at
// the *bucket* prefix level, where the contract suite does not reach.
//
// Sharing one bucket between deployments is the documented purpose of
// core.object_store.prefix, and a raw string trim would let "prod/nexus" claim
// every object of "prod/nexus-staging" and hydrate them into the wrong tree.
func TestConfiguredPrefixIsolatesNeighbours(t *testing.T) {
	f := newFakeGCS(t, "nexus-shared")
	b := newBackend(t, f, func(c *objectstore.Config) { c.Prefix = "prod/nexus" })
	ctx := context.Background()

	// Written straight into the fake: these belong to other writers, and this
	// backend has no way to create them.
	f.put("prod/nexus-staging/files/theirs.txt", []byte("staging"))
	f.put("prod/nexusfiles.txt", []byte("no separator at all"))
	f.put("elsewhere/files/theirs.txt", []byte("another deployment"))

	local := filepath.Join(t.TempDir(), "ours.txt")
	if err := os.WriteFile(local, []byte("ours"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(ctx, "files/ours.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}

	objs, err := b.List(ctx, "")
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(objs) != 1 || objs[0].Key != "files/ours.txt" {
		t.Fatalf("List = %+v, want exactly [files/ours.txt]", objs)
	}

	dest := t.TempDir()
	if err := b.Hydrate(ctx, "", dest); err != nil {
		t.Fatalf("Hydrate = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "files", "theirs.txt")); err == nil {
		t.Error("hydration pulled in a neighbouring deployment's object")
	}
}

// serviceAccountJSON writes a syntactically real service-account key file with
// a freshly generated RSA key, pointing its token_uri at the fake.
//
// Redirecting token_uri is what makes the static-credential path testable
// offline: without it the SDK signs a JWT assertion and posts it to
// oauth2.googleapis.com, so the test would need network access and a real
// account. With it, the entire exchange -- assertion, access token, bearer
// header -- happens over the loopback interface, and the test can assert that
// the token the fake minted is the one that signed the storage request.
func serviceAccountJSON(t *testing.T, tokenURI, email string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a test key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	body, err := json.Marshal(map[string]string{
		"type":                        "service_account",
		"project_id":                  "nexus-test",
		"private_key_id":              "test-key-id",
		"private_key":                 string(pemKey),
		"client_email":                email,
		"client_id":                   "1",
		"token_uri":                   tokenURI,
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	})
	if err != nil {
		t.Fatalf("marshalling the test key file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the test key file: %v", err)
	}
	return path
}

// TestCredentialSourceStaticFile exercises the credentials_file path: a static
// service-account JSON key, which is the format every Kubernetes secret and CI
// variable already holds.
//
// The assertions are that a token was actually minted and that the storage
// request carried it. Without them the test would pass against a backend that
// ignored credentials_file entirely and talked to the fake unauthenticated,
// which is precisely the silent misconfiguration worth catching.
func TestCredentialSourceStaticFile(t *testing.T) {
	f := newFakeGCS(t, "nexus-creds")
	isolateGoogleEnv(t) // no ADC, so only the file can win
	keyFile := serviceAccountJSON(t, f.tokenURI(), "static@nexus-test.iam.gserviceaccount.com")

	b, err := gcsstore.New(context.Background(), objectstore.Config{
		BackendName:     gcsstore.BackendName,
		Bucket:          f.bucket,
		Endpoint:        f.endpoint(),
		CredentialsFile: keyFile,
		Logger:          quietLogger(),
	})
	if err != nil {
		t.Fatalf("gcsstore.New: %v", err)
	}
	t.Cleanup(func() { _ = b.(io.Closer).Close() })

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}

	if got := f.tokenRequests(); got == 0 {
		t.Error("no token was exchanged; credentials_file did not reach the client")
	}
	if auth := f.authHeader(); auth != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q, want the token minted from credentials_file", auth)
	}
}

// TestCredentialSourceApplicationDefault exercises the no-credentials_file
// path.
//
// GOOGLE_APPLICATION_CREDENTIALS is the leg of Application Default Credentials
// a test can actually drive. The legs that matter most in production -- GKE
// Workload Identity and the GCE service account via the metadata server,
// impersonation, and Workload Identity Federation -- resolve through the same
// chain and cannot be exercised here, because no emulator reproduces Google's
// IAM (the PRD records this as the emulator-fidelity risk). What this test pins
// is that Nexus hands the chain over unmodified rather than narrowing it, which
// is the part Nexus is responsible for.
func TestCredentialSourceApplicationDefault(t *testing.T) {
	f := newFakeGCS(t, "nexus-creds")
	isolateGoogleEnv(t)
	keyFile := serviceAccountJSON(t, f.tokenURI(), "adc@nexus-test.iam.gserviceaccount.com")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyFile)

	b, err := gcsstore.New(context.Background(), objectstore.Config{
		BackendName: gcsstore.BackendName,
		Bucket:      f.bucket,
		Endpoint:    f.endpoint(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("gcsstore.New: %v", err)
	}
	t.Cleanup(func() { _ = b.(io.Closer).Close() })

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}

	if got := f.tokenRequests(); got == 0 {
		t.Error("no token was exchanged; ambient credentials were not used")
	}
	if auth := f.authHeader(); auth != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q, want the token minted from ADC", auth)
	}
}

// TestCredentialsFileMustExist covers the typo case. The auth library reports a
// missing key file from several layers down with a message that names neither
// the config key nor, reliably, the path -- and under failure_policy: degrade
// the operator would see a session that persists nothing rather than a boot
// failure.
func TestCredentialsFileMustExist(t *testing.T) {
	f := newFakeGCS(t, "nexus-creds")
	isolateGoogleEnv(t)
	_, err := gcsstore.New(context.Background(), objectstore.Config{
		BackendName:     gcsstore.BackendName,
		Bucket:          f.bucket,
		Endpoint:        f.endpoint(),
		CredentialsFile: filepath.Join(t.TempDir(), "not-there"),
		Logger:          quietLogger(),
	})
	if err == nil {
		t.Fatal("New with a missing credentials_file = nil, want an error")
	}
	if !strings.Contains(err.Error(), "credentials_file") {
		t.Errorf("error = %v, want it to name credentials_file", err)
	}
}

// TestNoCredentialsAndNoEndpointFailsTheBoot is the deliberate improvement on
// the SDK, which builds a client happily when it cannot find credentials and
// fails at the first request instead. Under failure_policy: degrade that would
// be a run that starts, looks healthy and persists nothing.
func TestNoCredentialsAndNoEndpointFailsTheBoot(t *testing.T) {
	isolateGoogleEnv(t)
	_, err := gcsstore.New(context.Background(), objectstore.Config{
		BackendName: gcsstore.BackendName,
		Bucket:      "nexus-sessions",
		Logger:      quietLogger(),
	})
	if err == nil {
		t.Fatal("New with no credentials and no endpoint = nil, want an error")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error = %v, want it to name the credential problem", err)
	}
}

// TestUnauthenticatedEmulatorPath is the other half: with an endpoint set and
// no credentials anywhere, the client is built unauthenticated rather than
// failing, which is what lets an emulator deployment be described entirely in
// YAML with no environment variable.
//
// Every other test in this file depends on this behaviour implicitly; asserting
// it once explicitly is what stops it from being changed by accident.
func TestUnauthenticatedEmulatorPath(t *testing.T) {
	f := newFakeGCS(t, "nexus-emulator")
	b := newBackend(t, f)

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put against an unauthenticated endpoint = %v", err)
	}
	if got := f.tokenRequests(); got != 0 {
		t.Errorf("%d token exchanges, want none on the unauthenticated path", got)
	}
	if auth := f.authHeader(); auth != "" {
		t.Errorf("Authorization = %q, want no credential sent", auth)
	}
}

// TestEndpointGetsTheJSONAPIPath pins normalizeEndpoint's one piece of
// rewriting. option.WithEndpoint replaces the whole base URL and appends
// nothing, so a bare host would send every JSON API request one path component
// too high -- and the failure would be a 404 that says nothing about the config
// key. The fake serves the API only under /storage/v1/, so a regression here is
// a failed Put.
func TestEndpointGetsTheJSONAPIPath(t *testing.T) {
	f := newFakeGCS(t, "nexus-endpoint")
	b := newBackend(t, f, func(c *objectstore.Config) {
		// A trailing slash is what a copy-paste from a browser produces.
		c.Endpoint = f.endpoint() + "/"
	})

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put with a bare-host endpoint = %v", err)
	}
	if got := f.keys(); !slices.Equal(got, []string{"files/a.txt"}) {
		t.Errorf("bucket keys = %v, want [files/a.txt]", got)
	}
}

func TestEndpointMustBeAnAbsoluteURL(t *testing.T) {
	for _, endpoint := range []string{"localhost:4443", "ftp://store.example", "https://"} {
		isolateGoogleEnv(t)
		_, err := gcsstore.New(context.Background(), objectstore.Config{
			BackendName: gcsstore.BackendName,
			Bucket:      "nexus-sessions",
			Endpoint:    endpoint,
			Logger:      quietLogger(),
		})
		if err == nil {
			t.Errorf("New with endpoint %q = nil, want an error", endpoint)
			continue
		}
		if !strings.Contains(err.Error(), "endpoint") {
			t.Errorf("New with endpoint %q error = %v, want it to name the endpoint", endpoint, err)
		}
	}
}

// TestRegionIsAcceptedAndIgnored records the decision in New: a GCS bucket's
// location is fixed when the bucket is created and no client ever names one, so
// there is nothing to apply core.object_store.region to. Failing the boot over
// a key that cannot change behaviour would break a config shared with an S3
// deployment, so it is accepted, warned about once, and otherwise inert.
func TestRegionIsAcceptedAndIgnored(t *testing.T) {
	f := newFakeGCS(t, "nexus-region")
	b := newBackend(t, f, func(c *objectstore.Config) { c.Region = "europe-west2" })

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put with a region set = %v; region must be inert, not fatal", err)
	}
}

// TestFlushIsProvableThroughAFreshClient is the in-process rehearsal of what
// E5-S2 runs against fake-gcs-server.
//
// Flush's durability promise cannot be demonstrated by a backend reading back
// through its own handle -- a cache would satisfy that. Building a second,
// independent backend and reading through it is the shape that can fail, and it
// is why Put is synchronous: everything is already durable by the time Flush is
// called, so a fresh handle sees it.
func TestFlushIsProvableThroughAFreshClient(t *testing.T) {
	f := newFakeGCS(t, "nexus-flush")
	ctx := context.Background()

	first := newBackend(t, f)
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("durable"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Put(ctx, "files/a.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}
	if err := first.Flush(ctx); err != nil {
		t.Fatalf("Flush = %v", err)
	}

	second := newBackend(t, f)
	dest := t.TempDir()
	if err := second.Hydrate(ctx, "", dest); err != nil {
		t.Fatalf("Hydrate through a fresh client = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "files", "a.txt"))
	if err != nil {
		t.Fatalf("reading hydrated file: %v", err)
	}
	if string(got) != "durable" {
		t.Errorf("hydrated content = %q, want %q", got, "durable")
	}
}

// TestCancelledContextStopsWork records this backend's cancellation semantics,
// which are identical to the S3 backend's and for the same reason: both are
// synchronous, so cancellation is the SDK's.
//
// A cancelled context aborts the in-flight request and the method returns an
// error. It does NOT promise the object was not stored -- a request the service
// already received completes server-side regardless -- so a caller that cancels
// must treat the key as being in an unknown state and re-Put or re-Delete it.
// Flush reports the context's state rather than claiming success over a window
// in which nothing was attempted.
func TestCancelledContextStopsWork(t *testing.T) {
	f := newFakeGCS(t, "nexus-cancel")
	b := newBackend(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(ctx, "files/a.txt", local); err == nil {
		t.Error("Put with a cancelled context = nil, want an error")
	}
	if err := b.Delete(ctx, "files/a.txt"); err == nil {
		t.Error("Delete with a cancelled context = nil, want an error")
	}
	if _, err := b.List(ctx, ""); err == nil {
		t.Error("List with a cancelled context = nil, want an error")
	}
	if err := b.Hydrate(ctx, "", t.TempDir()); err == nil {
		t.Error("Hydrate with a cancelled context = nil, want an error")
	}
	if err := b.Flush(ctx); err == nil {
		t.Error("Flush with a cancelled context = nil, want an error")
	}
}

// TestDeleteOfAMissingObjectIsNotAnError is the divergence from S3 that this
// backend has to absorb, pinned on its own rather than only inside the contract
// suite. GCS returns 404 where S3 returns 204; the engine's push path retries a
// delete and would turn every retry into a permanent failure if this leaked.
func TestDeleteOfAMissingObjectIsNotAnError(t *testing.T) {
	f := newFakeGCS(t, "nexus-delete")
	b := newBackend(t, f)

	if err := b.Delete(context.Background(), "files/never-existed.txt"); err != nil {
		t.Errorf("Delete of a missing object = %v, want nil", err)
	}
}

// TestPutRejectsADirectory keeps the error message useful. Without the explicit
// check the copy fails with a platform-dependent errno that says nothing about
// what was handed in.
func TestPutRejectsADirectory(t *testing.T) {
	f := newFakeGCS(t, "nexus-dir")
	b := newBackend(t, f)

	if err := b.Put(context.Background(), "files/a.txt", t.TempDir()); err == nil {
		t.Fatal("Put of a directory = nil, want an error")
	}
	if got := f.keys(); len(got) != 0 {
		t.Errorf("bucket keys = %v, want nothing stored", got)
	}
}

// TestCloseReleasesTheClient pins the io.Closer half of the arrangement
// described on Backend.Close: objectstore.Backend has no Close, the engine
// type-asserts for one, and this backend has something to release. The
// assertion is that the type still satisfies io.Closer -- if it stops, the
// engine silently stops releasing the client.
func TestCloseReleasesTheClient(t *testing.T) {
	f := newFakeGCS(t, "nexus-close")
	isolateGoogleEnv(t)
	b, err := gcsstore.New(context.Background(), objectstore.Config{
		BackendName: gcsstore.BackendName,
		Bucket:      f.bucket,
		Endpoint:    f.endpoint(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("gcsstore.New: %v", err)
	}
	c, ok := b.(io.Closer)
	if !ok {
		t.Fatal("the backend no longer implements io.Closer; the engine will not release the client")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
