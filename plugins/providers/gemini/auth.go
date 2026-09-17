package gemini

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/frankbardon/nexus/pkg/nexuscreds"
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
		project, _ := cfg["project_id"].(string)
		if project == "" {
			project = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
		if project == "" {
			return nil, fmt.Errorf("gemini: vertex auth requires project_id config or GOOGLE_CLOUD_PROJECT env var")
		}
		a.projectID = project

		loc, _ := cfg["location"].(string)
		if loc == "" {
			loc = "us-central1"
		}
		a.location = loc

		source, err := resolveCredentials(cfg)
		if err != nil {
			return nil, err
		}
		a.creds = source
	}

	return a, nil
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
