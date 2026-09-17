package googleadc

import (
	"context"
	"fmt"
	"os"
	"sync"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexuscreds"
)

// Name is the registry name this source is selected by in configuration.
const Name = "google-adc"

// CloudPlatformScope is the only scope this source ever requests. Vertex AI
// accepts nothing narrower, so it is a constant rather than a config key —
// see the package doc.
const CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

func init() {
	nexuscreds.Register(Name, factory)
}

// factory adapts New to nexuscreds.Factory.
//
// New returns *Source rather than nexuscreds.Source so a caller that wants
// Resolve can construct one directly without a type assertion; the adapter
// converts explicitly, which also avoids handing back a non-nil interface
// wrapping a nil pointer on the error path.
func factory(cfg map[string]any) (nexuscreds.Source, error) {
	s, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Source mints Google access tokens from Application Default Credentials.
//
// It is safe for concurrent use. The mutex below guards CREDENTIAL
// RESOLUTION — deciding which ADC entry applies and building its TokenSource
// once — and nothing else. Token caching, expiry and refresh belong entirely
// to the oauth2.TokenSource; this package deliberately owns none of that.
type Source struct {
	// keyJSON is the contents of an explicitly configured key file, or nil to
	// run the full ADC chain. It is read at construction so a bad path fails
	// at plugin Init rather than at the first LLM call.
	keyJSON []byte
	// origin names where the credential came from, for error messages and for
	// Resolved.Origin. It is the config key for a key file, or originADC.
	origin string

	mu    sync.Mutex
	creds *google.Credentials
}

// originADC is Source.origin when no key file was configured.
const originADC = "application default credentials"

// New builds a Source from a credentials config block.
//
// cfg may be nil, which means "the full ADC chain" — the case that matters on
// GKE, where there is nothing to configure. Two keys are recognised, both
// forwarded by the plugin that owns them:
//
//	service_account_json:     path to a credentials JSON file
//	service_account_json_env: name of an env var holding that path
//
// New performs no network I/O, as nexuscreds.Factory requires: in the
// motivating case it runs during nexus.llm.gemini's Init, before that plugin's
// HTTP client exists. It reads a configured key file (local I/O, and worth
// failing fast on), but defers building the credential itself to the first
// Token call, where there is a request context to honour.
func New(cfg map[string]any) (*Source, error) {
	path, origin, err := keyFileFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return &Source{origin: originADC}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("googleadc: reading the key file named by %s: %w", origin, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("googleadc: the key file named by %s is empty: %s", origin, path)
	}
	return &Source{keyJSON: data, origin: origin}, nil
}

// keyFileFromConfig resolves the configured key-file path, if any, and returns
// it alongside a human-readable description of where it came from. An empty
// path means no key file was configured and the full ADC chain applies.
//
// GOOGLE_APPLICATION_CREDENTIALS is deliberately NOT consulted here.
// FindDefaultCredentials reads it as step one of the chain, and duplicating
// that would make the env var behave differently depending on whether some
// unrelated config key happened to be set.
func keyFileFromConfig(cfg map[string]any) (path, origin string, err error) {
	if raw, ok := cfg["service_account_json"]; ok {
		p, ok := raw.(string)
		if !ok {
			return "", "", fmt.Errorf("googleadc: service_account_json must be a string, got %T", raw)
		}
		if p != "" {
			// engine.ExpandPath at the read site: every config-supplied path in
			// Nexus may be written "~/..." and there is exactly one helper.
			return engine.ExpandPath(p), "service_account_json", nil
		}
	}
	if raw, ok := cfg["service_account_json_env"]; ok {
		name, ok := raw.(string)
		if !ok {
			return "", "", fmt.Errorf("googleadc: service_account_json_env must be a string, got %T", raw)
		}
		if name != "" {
			p := os.Getenv(name)
			if p == "" {
				// Falling through to the ADC chain here would turn a typo in
				// the variable name into a silently different identity, which
				// is the worst way for a credential to be wrong.
				return "", "", fmt.Errorf("googleadc: service_account_json_env names %s, but that environment variable is unset or empty", name)
			}
			return engine.ExpandPath(p), "service_account_json_env=" + name, nil
		}
	}
	return "", "", nil
}

// Token returns a Google access token for the cloud-platform scope.
//
// It implements nexuscreds.Source. The caller does not cache, and neither does
// this method: the oauth2.TokenSource underneath returns the same token until
// shortly before it expires and refreshes it thereafter.
func (s *Source) Token(ctx context.Context) (string, error) {
	creds, err := s.credentials(ctx)
	if err != nil {
		return "", err
	}
	tok, err := tokenWithContext(ctx, creds.TokenSource)
	if err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("googleadc: %s produced a token with no access token", s.origin)
	}
	return tok.AccessToken, nil
}

// credentials resolves the credential once and reuses it.
//
// The resolution is lazy because nexuscreds.Factory forbids network I/O at
// construction and FindDefaultCredentials probes the metadata server. A failed
// resolution is NOT memoised: a pod that starts before its metadata server is
// reachable would otherwise never recover without a restart.
func (s *Source) credentials(ctx context.Context) (*google.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds != nil {
		return s.creds, nil
	}

	// oauth2 stores this context inside the TokenSource and reuses it for every
	// later refresh, so the TokenSource would otherwise inherit the deadline of
	// whichever request happened to be first — and the second turn's refresh
	// would fail because the first turn ended. WithoutCancel keeps the values
	// (an injected *http.Client among them) and drops only the lifetime.
	// Per-call cancellation is honoured separately, in tokenWithContext.
	rctx := context.WithoutCancel(ctx)

	var (
		creds *google.Credentials
		err   error
	)
	if s.keyJSON != nil {
		creds, err = credentialsFromKeyFile(rctx, s.keyJSON)
		if err != nil {
			return nil, fmt.Errorf("googleadc: loading the credential from %s: %w", s.origin, err)
		}
	} else {
		creds, err = google.FindDefaultCredentials(rctx, CloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("googleadc: resolving application default credentials: %w", err)
		}
	}
	s.creds = creds
	return creds, nil
}

// credentialsFromKeyFile parses an explicitly configured credentials file.
//
// It reads the credential type first and passes it to
// CredentialsFromJSONWithTypeAndParams rather than calling the shorter
// CredentialsFromJSON. Both of the shorter forms are deprecated in
// x/oauth2 — they load whatever type the file claims to be, which is a real
// hazard for a file an operator points at rather than authors — and
// staticcheck rejects a call to either. Reading the type up front also gives
// us the allowlist below, so a file naming a type this build does not expect
// fails here with its type named, instead of somewhere inside the library.
func credentialsFromKeyFile(ctx context.Context, jsonData []byte) (*google.Credentials, error) {
	credType, _, err := describe(jsonData)
	if err != nil {
		return nil, err
	}
	libType, ok := supportedTypes[credType]
	if !ok {
		return nil, fmt.Errorf("unsupported credential type %q (expected one of %s)", credType, supportedTypeList())
	}
	params := google.CredentialsParams{Scopes: []string{CloudPlatformScope}}
	return google.CredentialsFromJSONWithTypeAndParams(ctx, jsonData, libType, params)
}

// Resolved describes the credential ADC actually produced. Nexus logs it once,
// at plugin Init, because "which identity is this process acting as" is the
// first question asked when a pod gets a 403 and the answer is otherwise
// invisible — ADC resolves silently through five candidate locations.
type Resolved struct {
	// Type is the ADC credential kind, e.g. TypeMetadata on a keyless pod.
	Type CredentialType
	// Principal is the acting identity, best effort: the service-account email
	// where one is knowable, the federated audience where it is not. It may be
	// empty, which is not an error — a metadata server that declines to name
	// the attached account still mints perfectly good tokens.
	Principal string
	// ProjectID is the project the credential itself reports. It may be empty;
	// it is not the same thing as the project a request is addressed to.
	ProjectID string
	// Origin says where the credential came from — the config key that named a
	// key file, or "application default credentials".
	Origin string
}

// Resolve reports which credential ADC produced, resolving it if that has not
// happened yet. It shares the resolution with Token, so calling it costs at
// most one extra metadata lookup for the principal.
//
// It takes a context because resolution reaches the network: on a keyless pod
// both the credential and the acting identity come from the metadata server.
func (s *Source) Resolve(ctx context.Context) (Resolved, error) {
	creds, err := s.credentials(ctx)
	if err != nil {
		return Resolved{}, err
	}
	r := Resolved{ProjectID: creds.ProjectID, Origin: s.origin}

	// A nil JSON means no credentials file was involved at all, which in the
	// ADC chain means exactly one thing: the GCE metadata server. That is the
	// keyless-pod case this package exists for.
	if creds.JSON == nil {
		r.Type = TypeMetadata
		// Best effort, and deliberately not fatal: the token already works.
		if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
			r.Principal = email
		}
		return r, nil
	}

	credType, principal, err := describe(creds.JSON)
	if err != nil {
		return Resolved{}, fmt.Errorf("googleadc: describing the credential from %s: %w", s.origin, err)
	}
	r.Type, r.Principal = credType, principal
	return r, nil
}

// tokenWithContext calls ts.Token honouring ctx.
//
// oauth2.TokenSource.Token takes no context — the one it uses was fixed when
// the TokenSource was built — but nexuscreds.Source.Token promises to honour
// the caller's cancellation and deadline. This bounds the wait and nothing
// else: the caching, expiry and refresh behind that call stay the library's.
// A cancelled call leaves the in-flight fetch running to completion, which is
// correct — its result populates the cache for the next request.
func tokenWithContext(ctx context.Context, ts oauth2.TokenSource) (*oauth2.Token, error) {
	type result struct {
		tok *oauth2.Token
		err error
	}
	// Buffered so the goroutine never blocks on a receiver that gave up.
	ch := make(chan result, 1)
	go func() {
		tok, err := ts.Token()
		ch <- result{tok, err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("googleadc: minting an access token: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("googleadc: minting an access token: %w", r.err)
		}
		return r.tok, nil
	}
}
