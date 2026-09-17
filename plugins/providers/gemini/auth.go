package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/frankbardon/nexus/pkg/nexuscreds"

	// gcemeta is imported for its GCE metadata helpers only — the project and
	// region this pod runs in. Those are facts about the environment, not about
	// credentials, and gcemeta registers nothing: this plugin is in
	// pkg/engine/allplugins, so an import that registered a credential source
	// from init would put that source in the registry of every binary carrying
	// the plugin, and gemini's credentials key defaults to google-adc. An
	// embedder shipping their own Vault or SPIFFE source would then get Google
	// ADC silently selected by a config that merely omitted the key, instead of
	// an error naming the sources their binary actually has.
	//
	// The token this plugin signs with is still opened by NAME through
	// nexuscreds, so no credential implementation is compiled in by the signing
	// path either.
	"github.com/frankbardon/nexus/pkg/nexuscreds/gcemeta"
)

// authMode determines how requests are authenticated and routed.
type authMode string

const (
	authModeAPIKey authMode = "api_key" // public Gemini Generative Language API
	authModeVertex authMode = "vertex"  // Vertex AI on Google Cloud
)

// defaultCredentialSource is the nexuscreds source used when the config names
// none. It is google-adc so a GKE deployment running under Workload Identity
// needs no credential config at all: no key file exists on such a pod, and the
// whole point of the default is that the operator does not have to say so.
const defaultCredentialSource = "google-adc"

// defaultVertexLocation is the location used when neither the config nor the
// GCE metadata server can say where this process runs.
const defaultVertexLocation = "us-central1"

// metadataTimeout bounds each GCE metadata lookup made while resolving auth.
// These run during plugin Init, so an unreachable metadata server must not be
// able to hang boot indefinitely — on a host that is not on GCE the helpers
// return without any request at all, so this only bites where the metadata
// server exists but is wedged.
const metadataTimeout = 5 * time.Second

// The GCE metadata lookups are reached through variables rather than called
// directly so tests can pin them.
//
// The reason is memoisation, not style: metadata.OnGCE — which gcemeta
// consults before every lookup — fixes its answer in a package-level sync.Once
// on the first call in a process, and with GCE_METADATA_HOST unset that call
// probes the real 169.254.169.254. A test that needs "not on GCE" therefore
// cannot get there by clearing an environment variable. gcemeta pins its own
// onGCE for exactly this reason; this package pins one level up.
var (
	metadataProjectID = gcemeta.ProjectID
	metadataRegion    = gcemeta.Region
)

// authState holds resolved auth configuration and, in Vertex mode, the
// credential source requests are signed with.
//
// It deliberately holds no token, no expiry and no mutex. Minting, caching and
// refreshing an access token all belong to the nexuscreds.Source — which for
// google-adc is an oauth2.TokenSource that already does each of them, for every
// Google credential type rather than only for a service-account key file.
type authState struct {
	mode authMode

	// API-key path.
	apiKey string

	// Vertex path.
	projectID string
	location  string // e.g. "us-central1"
	creds     nexuscreds.Source
}

// resolveAuth reads auth config and returns a configured authState.
//
// Vertex credentials are resolved here, at plugin Init, so a misconfigured
// deployment fails at boot with a clear error rather than on its first LLM
// call. Resolution is construction only: nexuscreds.Factory forbids network
// I/O, which matters because this runs before the plugin's HTTP client exists.
func resolveAuth(cfg map[string]any) (*authState, error) {
	mode := authModeAPIKey
	if v, ok := cfg["auth"].(string); ok {
		switch authMode(v) {
		case authModeAPIKey, authModeVertex:
			mode = authMode(v)
		default:
			return nil, fmt.Errorf("gemini: unknown auth mode %q (expected api_key or vertex)", v)
		}
	}

	a := &authState{mode: mode}

	switch mode {
	case authModeAPIKey:
		if key, ok := cfg["api_key"].(string); ok && key != "" {
			a.apiKey = key
		} else {
			envVar, _ := cfg["api_key_env"].(string)
			if envVar != "" {
				a.apiKey = os.Getenv(envVar)
			} else {
				if v := os.Getenv("GEMINI_API_KEY"); v != "" {
					a.apiKey = v
				} else if v := os.Getenv("GOOGLE_API_KEY"); v != "" {
					a.apiKey = v
				}
			}
		}
		if a.apiKey == "" {
			return nil, fmt.Errorf("gemini: no API key configured (set api_key in config or GEMINI_API_KEY / GOOGLE_API_KEY env var)")
		}

	case authModeVertex:
		project, err := resolveProjectID(cfg)
		if err != nil {
			return nil, err
		}
		a.projectID = project
		a.location = resolveLocation(cfg)

		source, err := resolveCredentials(cfg)
		if err != nil {
			return nil, err
		}
		a.creds = source
	}

	return a, nil
}

// resolveProjectID resolves the Vertex project through three steps, in order:
// the project_id config key, the GOOGLE_CLOUD_PROJECT environment variable,
// and the GCE metadata server.
//
// The last step is what lets a GKE deployment configure nothing GCP-specific
// beyond auth: vertex. A pod already knows which project it belongs to, and
// making an operator repeat that in YAML is a chance to get it wrong. It comes
// last so an explicit setting always wins over the environment it happens to
// run in, and gcemeta.ProjectID answers ErrNotOnGCE without a request when
// this process is not on GCE, so a non-GCP host pays no network probe.
//
// Failure is still fatal at Init, because a Vertex URL cannot be built without
// a project — but now only when every step has failed, rather than whenever
// the config key was absent.
func resolveProjectID(cfg map[string]any) (string, error) {
	if v, _ := cfg["project_id"].(string); v != "" {
		return v, nil
	}
	if v := os.Getenv("GOOGLE_CLOUD_PROJECT"); v != "" {
		return v, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()

	project, err := metadataProjectID(ctx)
	switch {
	case err == nil:
		return project, nil
	case errors.Is(err, gcemeta.ErrNotOnGCE):
		return "", fmt.Errorf("gemini: vertex auth requires project_id config or GOOGLE_CLOUD_PROJECT env var (this process is not on GCE, so the metadata server could not supply one)")
	default:
		return "", fmt.Errorf("gemini: vertex auth has no project_id config and no GOOGLE_CLOUD_PROJECT env var, and the GCE metadata server could not supply one: %w", err)
	}
}

// resolveLocation resolves the Vertex location through the location config
// key, then the region this process runs in as derived from the GCE metadata
// server's zone, then defaultVertexLocation.
//
// Unlike the project, an unresolvable location is not fatal: there is a usable
// default, and the deployment is better served by booting than by refusing to.
// Note the accepted risk that follows from deriving it: Vertex model
// availability is per-region, so a pod in a quiet region may resolve its own
// region and then 404 on a model served only elsewhere — at first inference,
// not at boot. The mitigation is documentation, deliberately not a boot-time
// availability probe.
func resolveLocation(cfg map[string]any) string {
	if v, _ := cfg["location"].(string); v != "" {
		return v
	}

	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()

	if region, err := metadataRegion(ctx); err == nil {
		return region
	}
	return defaultVertexLocation
}

// resolveCredentials opens the nexuscreds source named by the credentials key.
//
// The error from an unknown name is passed through rather than reworded:
// nexuscreds.Open already says that the binary must blank-import the package
// registering the source, and lists what this build does have, which is the
// actionable half of the message.
func resolveCredentials(cfg map[string]any) (nexuscreds.Source, error) {
	name := defaultCredentialSource
	if raw, ok := cfg["credentials"]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("gemini: credentials must be a string naming a credential source, got %T", raw)
		}
		if s != "" {
			name = s
		}
	}

	source, err := nexuscreds.Open(name, credentialSourceConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("gemini: vertex auth: %w", err)
	}
	return source, nil
}

// credentialSourceConfig builds the config block handed to the credential
// source.
//
// The two key-file keys predate the nexuscreds seam and are kept verbatim so no
// existing deployment has to edit its YAML: they are simply forwarded to the
// source, which owns what they mean. They are forwarded individually rather
// than by passing the plugin's whole config map, so a source can never observe
// — or collide with — an unrelated gemini key such as api_key.
//
// A key absent from the config is left absent rather than forwarded empty:
// google-adc distinguishes "not configured" (run the full ADC chain, the
// keyless-pod case) from "configured to something empty".
func credentialSourceConfig(cfg map[string]any) map[string]any {
	out := make(map[string]any, 2)
	for _, key := range []string{"service_account_json", "service_account_json_env"} {
		if v, ok := cfg[key]; ok {
			out[key] = v
		}
	}
	return out
}

// apiURL builds the request URL for a model + operation.
// op is "generateContent" or "streamGenerateContent". For streaming, alt=sse
// is appended.
func (a *authState) apiURL(model, op string) string {
	switch a.mode {
	case authModeVertex:
		base := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
			a.location, a.projectID, a.location, model, op)
		if op == "streamGenerateContent" {
			base += "?alt=sse"
		}
		return base
	default:
		base := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:%s", model, op)
		if op == "streamGenerateContent" {
			base += "?alt=sse"
		}
		return base
	}
}

// applyAuth attaches credentials to an outgoing request. In Vertex mode it asks
// the credential source for a token on every request and does not cache the
// answer: caching is the source's contract, not the caller's.
func (a *authState) applyAuth(ctx context.Context, req *http.Request) error {
	switch a.mode {
	case authModeAPIKey:
		req.Header.Set("x-goog-api-key", a.apiKey)
		return nil
	case authModeVertex:
		if a.creds == nil {
			return fmt.Errorf("gemini: vertex auth has no credential source")
		}
		token, err := a.creds.Token(ctx)
		if err != nil {
			return fmt.Errorf("gemini: vertex credentials: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	return fmt.Errorf("gemini: unknown auth mode")
}

// filesAPIBaseURL returns the base URL for the Files API. Files API is only
// available on the public endpoint; in Vertex mode an empty string is returned
// to signal callers to use inline data.
func (a *authState) filesAPIBaseURL() string {
	if a.mode == authModeVertex {
		return ""
	}
	return "https://generativelanguage.googleapis.com"
}

// cachedContentsURL returns the base URL for the cachedContents resource.
func (a *authState) cachedContentsURL() string {
	switch a.mode {
	case authModeVertex:
		return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/cachedContents",
			a.location, a.projectID, a.location)
	default:
		return "https://generativelanguage.googleapis.com/v1beta/cachedContents"
	}
}
