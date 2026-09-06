package s3store_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"

	s3store "github.com/frankbardon/nexus/modules/objectstore-s3"
)

// awsEnvVars is every environment variable that can influence the SDK's
// credential or endpoint resolution.
//
// The list is cleared before each test rather than trusted to be absent. These
// tests run on developer laptops with a populated ~/.aws and an AWS_PROFILE
// exported by a shell profile, and on CI runners with none of it; a test whose
// result depends on which of those it is on is not a test. Clearing also stops
// a real credential from ever reaching the loopback fake.
var awsEnvVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_DEFAULT_PROFILE",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"AWS_ROLE_ARN",
	"AWS_ROLE_SESSION_NAME",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_S3",
	"AWS_USE_PATH_STYLE_ENDPOINT",
}

// isolateAWSEnv unsets everything in awsEnvVars for the duration of the test
// and points the shared config and credentials files at paths that do not
// exist, so the SDK reads no file the developer happens to have.
//
// IMDS is disabled too. Without it, a test that expects "no credentials
// resolved" spends the SDK's instance-metadata timeout discovering there is no
// metadata service, which turns an assertion into a multi-second stall on every
// laptop.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	for _, name := range awsEnvVars {
		if old, ok := os.LookupEnv(name); ok {
			t.Cleanup(func() { _ = os.Setenv(name, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(name) })
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "no-such-aws-dir")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(missing, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(missing, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newBackend builds a backend pointed at f, with static credentials supplied
// through the environment leg of the SDK's default chain.
func newBackend(t *testing.T, f *fakeS3, mutate ...func(*objectstore.Config)) objectstore.Backend {
	t.Helper()
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAAMBIENTEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")

	cfg := objectstore.Config{
		BackendName: s3store.BackendName,
		Bucket:      f.bucket,
		Region:      "us-east-1",
		Endpoint:    f.endpoint(),
		Logger:      quietLogger(),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	b, err := s3store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("s3store.New: %v", err)
	}
	return b
}

// TestContractSuite is the acceptance criterion: this backend is held to the
// shared conformance suite, not to assertions written to match it.
//
// It runs against the loopback fake so it is part of the untagged `make test`
// sweep -- no container, no credential, no network. The same suite is run
// against MinIO behind a build tag by E4-S3, which is what closes the fidelity
// gap the fake cannot close on its own.
func TestContractSuite(t *testing.T) {
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		// A fresh fake per case, which is what makes the suite's per-case
		// isolation real: nothing is shared between cases, not even a bucket.
		return newBackend(t, newFakeS3(t, "nexus-contract"))
	},
		// The fake's page size is 50 (see defaultFakePageSize), so 200 objects
		// span four pages and catch a paginator that is not followed. The
		// suite's 1200 default exists to clear S3's real 1000-key page and is
		// paid once, against MinIO, rather than 1200 times inside `make test`.
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
// TestKeyLayout below can restrict itself to asserting the physical layout.
func TestContractSuiteUnderAConfiguredPrefix(t *testing.T) {
	objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
		return newBackend(t, newFakeS3(t, "nexus-contract"), func(c *objectstore.Config) {
			c.Prefix = "prod/nexus"
		})
	}, objectstoretest.WithListProbeCount(200))
}

func TestRegistersItselfAsS3(t *testing.T) {
	// The blank-import contract: importing this module must be enough for
	// `core.object_store.backend: s3` to validate.
	if !objectstore.Registered(s3store.BackendName) {
		t.Fatalf("backend %q is not registered; the init-time Register did not run", s3store.BackendName)
	}
	cfg := objectstore.Config{BackendName: s3store.BackendName, Bucket: "b"}
	if err := cfg.Validate("core.object_store"); err != nil {
		t.Fatalf("Validate with a registered backend = %v, want nil", err)
	}
}

// awkwardKeys are the shapes the story calls out as having to round-trip, plus
// the ones a naive key mapping mangles.
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
// The property being pinned is that an operator can open the bucket in a
// console and read their session tree.
func TestKeyLayoutMirrorsTheLocalTree(t *testing.T) {
	const prefix = "prod/nexus"
	f := newFakeS3(t, "nexus-layout")
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
	f := newFakeS3(t, "nexus-shared")
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

// TestCredentialSourceStaticFile exercises the credentials_file path: a static
// access key and secret in the ordinary AWS INI format.
//
// The assertion is on the Credential scope of the Authorization header, which
// names the access key id that actually signed the request. That is the only
// end-to-end way to tell which leg of the credential chain won without
// reimplementing SigV4 in the test.
func TestCredentialSourceStaticFile(t *testing.T) {
	f := newFakeS3(t, "nexus-creds")
	credsDir := t.TempDir()
	credsFile := filepath.Join(credsDir, "credentials")
	if err := os.WriteFile(credsFile, []byte(
		"[default]\naws_access_key_id = AKIASTATICFILEKEY\naws_secret_access_key = static-file-secret\n",
	), 0o600); err != nil {
		t.Fatalf("writing credentials file: %v", err)
	}

	isolateAWSEnv(t) // no ambient key, so only the file can win
	b, err := s3store.New(context.Background(), objectstore.Config{
		BackendName:     s3store.BackendName,
		Bucket:          f.bucket,
		Region:          "eu-west-2",
		Endpoint:        f.endpoint(),
		CredentialsFile: credsFile,
		Logger:          quietLogger(),
	})
	if err != nil {
		t.Fatalf("s3store.New: %v", err)
	}

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}

	auth := f.authHeader()
	if !strings.Contains(auth, "Credential=AKIASTATICFILEKEY/") {
		t.Errorf("Authorization = %q, want it signed by the key in credentials_file", auth)
	}
	// The credential scope carries the region, so this doubles as proof that
	// core.object_store.region reaches the signer.
	if !strings.Contains(auth, "/eu-west-2/s3/aws4_request") {
		t.Errorf("Authorization = %q, want the configured region in the credential scope", auth)
	}
}

// TestCredentialSourceAmbient exercises the no-credentials_file path.
//
// Environment variables are the leg of the SDK's default chain a test can
// actually drive. The legs that matter most in production -- IRSA / EKS Pod
// Identity via the projected web-identity token, ECS task roles, and the EC2
// instance role via IMDSv2 -- come from the same
// awsconfig.LoadDefaultConfig call and cannot be exercised here, because no
// emulator reproduces IAM (the PRD records this as the emulator-fidelity risk).
// What this test pins is that Nexus hands the chain over unmodified rather than
// narrowing it, which is the part Nexus is responsible for.
func TestCredentialSourceAmbient(t *testing.T) {
	f := newFakeS3(t, "nexus-creds")
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAAMBIENTKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("AWS_SESSION_TOKEN", "ambient-session-token")

	b, err := s3store.New(context.Background(), objectstore.Config{
		BackendName: s3store.BackendName,
		Bucket:      f.bucket,
		Region:      "us-east-1",
		Endpoint:    f.endpoint(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("s3store.New: %v", err)
	}

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put = %v", err)
	}

	if auth := f.authHeader(); !strings.Contains(auth, "Credential=AKIAAMBIENTKEY/") {
		t.Errorf("Authorization = %q, want it signed by the ambient key", auth)
	}
}

// TestCredentialsFileMustExist covers the silent-misconfiguration case: the SDK
// ignores a shared credentials file that is not there and falls through to
// ambient credentials, so a typo'd path would otherwise produce a running
// process authenticating as the wrong principal.
func TestCredentialsFileMustExist(t *testing.T) {
	f := newFakeS3(t, "nexus-creds")
	isolateAWSEnv(t)
	_, err := s3store.New(context.Background(), objectstore.Config{
		BackendName:     s3store.BackendName,
		Bucket:          f.bucket,
		Region:          "us-east-1",
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

// TestPathStyleAddressing pins the line that makes MinIO, Ceph and Backblaze
// work unmodified.
//
// Virtual-host addressing would put the bucket in the hostname, which needs
// wildcard DNS the fake -- like a self-hosted store or a laptop container --
// does not have. The fake fails any request whose path does not begin with the
// bucket, so a regression to virtual-host style shows up as a failed Put rather
// than as an obscure DNS error in someone's cluster.
func TestPathStyleAddressing(t *testing.T) {
	f := newFakeS3(t, "nexus-pathstyle")
	b := newBackend(t, f)

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put against a bucket-less endpoint = %v; path-style addressing is not in effect", err)
	}
	if got := f.keys(); !slices.Equal(got, []string{"files/a.txt"}) {
		t.Errorf("bucket keys = %v, want [files/a.txt]", got)
	}
}

// TestRegionFallbackForCustomEndpoints covers the MinIO-on-a-laptop
// configuration, where an operator has no region to give because their store
// has no concept of one. SigV4 still signs over a region, so a fallback has to
// come from somewhere; this pins where.
func TestRegionFallbackForCustomEndpoints(t *testing.T) {
	f := newFakeS3(t, "nexus-region")
	b := newBackend(t, f, func(c *objectstore.Config) { c.Region = "" })

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := b.Put(context.Background(), "files/a.txt", local); err != nil {
		t.Fatalf("Put with no configured region = %v", err)
	}
	if auth := f.authHeader(); !strings.Contains(auth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization = %q, want the us-east-1 fallback in the credential scope", auth)
	}
}

// TestRegionIsRequiredWithoutAnEndpoint is the other half: against real AWS the
// region selects where the data physically lives, so guessing it would hand an
// operator a bucket-not-found for a bucket that exists in the region they
// forgot to name.
func TestRegionIsRequiredWithoutAnEndpoint(t *testing.T) {
	isolateAWSEnv(t)
	_, err := s3store.New(context.Background(), objectstore.Config{
		BackendName: s3store.BackendName,
		Bucket:      "nexus-sessions",
		Logger:      quietLogger(),
	})
	if err == nil {
		t.Fatal("New with no region and no endpoint = nil, want an error")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error = %v, want it to name the region", err)
	}
}

func TestEndpointMustBeAnAbsoluteURL(t *testing.T) {
	// The SDK accepts a bare host:port and then fails much later with an
	// unresolvable-host error that never mentions the config key.
	for _, endpoint := range []string{"localhost:9000", "ftp://store.example", "https://"} {
		isolateAWSEnv(t)
		_, err := s3store.New(context.Background(), objectstore.Config{
			BackendName: s3store.BackendName,
			Bucket:      "nexus-sessions",
			Region:      "us-east-1",
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

// TestFlushIsProvableThroughAFreshClient is the in-process rehearsal of what
// E4-S3 runs against MinIO.
//
// Flush's durability promise cannot be demonstrated by a backend reading back
// through its own handle -- a cache would satisfy that. Building a second,
// independent backend and reading through it is the shape that can fail, and it
// is why Put is synchronous: everything is already durable by the time Flush is
// called, so a fresh handle sees it.
func TestFlushIsProvableThroughAFreshClient(t *testing.T) {
	f := newFakeS3(t, "nexus-flush")
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
// which E1-S3 left open because objectstore.Backend permits an asynchronous Put.
//
// This one is synchronous, so cancellation is the SDK's: a cancelled context
// aborts the in-flight request and the method returns an error. It does NOT
// promise the object was not stored -- a PUT the store already received
// completes server-side regardless -- so a caller that cancels must treat the
// key as being in an unknown state and re-Put or re-Delete it. Flush reports the
// context's state rather than claiming success over a window in which nothing
// was attempted.
func TestCancelledContextStopsWork(t *testing.T) {
	f := newFakeS3(t, "nexus-cancel")
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

// TestPutRejectsADirectory keeps the error message useful. Without the explicit
// check the read fails with a platform-dependent errno that says nothing about
// what was handed in.
func TestPutRejectsADirectory(t *testing.T) {
	f := newFakeS3(t, "nexus-dir")
	b := newBackend(t, f)

	if err := b.Put(context.Background(), "files/a.txt", t.TempDir()); err == nil {
		t.Fatal("Put of a directory = nil, want an error")
	}
	if got := f.keys(); len(got) != 0 {
		t.Errorf("bucket keys = %v, want nothing stored", got)
	}
}
