package gcsstore

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/storage"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"google.golang.org/api/option"
)

// BackendName is the name this backend registers under, and the value an
// operator writes for core.object_store.backend.
const BackendName = "gcs"

// jsonAPIPath is the base path the Cloud Storage JSON API is served under.
//
// The SDK appends it to its own default host, and its emulator support appends
// it to STORAGE_EMULATOR_HOST, but option.WithEndpoint replaces the whole base
// URL and appends nothing. So an operator who wrote a bare host would get every
// request sent one path component too high, and the failure would be a 404 that
// says nothing about the config key. normalizeEndpoint appends it instead of
// asking the operator to remember it -- which also keeps core.object_store.endpoint
// spelled the same way for both backends: a plain scheme://host[:port].
const jsonAPIPath = "storage/v1/"

func init() {
	// Driver-style registration, per objectstore.Register. This is the entire
	// coupling between this module and Nexus core: a blank import turns
	// `backend: gcs` from a boot failure into a working configuration, and core
	// never learns this module exists.
	objectstore.Register(BackendName, New)
}

// New builds a GCS backend from a validated object-store config. It is the
// objectstore.Factory registered above, exported so an embedder who wants to
// construct one directly -- for a test, or to run two backends side by side --
// does not have to go through the registry.
//
// # What is and is not checked here
//
// Everything cheap and local is checked: the endpoint parses, a credentials
// file that was named exists, and some credential source can be resolved at
// all. Those are configuration mistakes, and house style is that a malformed
// configuration fails the boot rather than surfacing an hour into a session.
//
// Nothing remote is checked. A bucket-attributes probe was considered and
// rejected for the reason core.object_store.failure_policy exists: `degrade`
// promises that an object-store outage degrades a run instead of ending it, and
// a boot-time round trip would make it structurally unable to keep that promise
// -- a brief GCS blip would stop every agent from starting. Credential
// *retrieval* is left to the SDK for the same reason: under Workload Identity
// it is itself a network call to the metadata server, and the SDK refreshes on
// expiry anyway, so minting a token at boot would prove less than it appears to.
func New(ctx context.Context, cfg objectstore.Config) (objectstore.Backend, error) {
	logger := cfg.Logger
	if logger == nil {
		// Config documents Logger as engine-injected and possibly nil for a
		// hand-built Config, so nil-check rather than assume.
		logger = slog.Default()
	}

	if cfg.Bucket == "" {
		// Config.Validate already covers this for the config path; repeat it
		// for a direct caller, who has not necessarily been through Validate.
		return nil, fmt.Errorf("gcs object store: bucket is required")
	}
	if err := objectstore.ValidateKeyPrefix(cfg.Prefix); err != nil {
		return nil, fmt.Errorf("gcs object store: prefix: %w", err)
	}
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.Region != "" {
		// Accepted and ignored rather than rejected. A GCS bucket's location is
		// chosen when the bucket is created and is never named by a client, so
		// there is nothing to apply the value to -- but core.object_store is one
		// shared block, and a config that is copied between an S3 and a GCS
		// deployment will carry a region. Failing the boot over a key that
		// cannot possibly change behaviour would be hostile; saying so once, at
		// boot, is enough to stop an operator believing it did something.
		logger.Warn("gcs object store ignores core.object_store.region",
			"region", cfg.Region,
			"reason", "a GCS bucket's location is a property of the bucket, not of the client")
	}

	opts, credentialSource, err := resolveCredentials(cfg, endpoint, logger)
	if err != nil {
		return nil, err
	}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}

	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcs object store: creating the Cloud Storage client: %w", err)
	}

	logger.Debug("gcs object store opened",
		"bucket", cfg.Bucket,
		"prefix", cfg.Prefix,
		"endpoint", endpoint,
		"credentials", credentialSource)

	return &Backend{
		client: client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
		log:    logger,
	}, nil
}

// resolveCredentials picks the credential source and returns the client
// options that select it, plus a label for the boot log.
//
// Three outcomes, in this order:
//
//  1. credentials_file set -- a static service-account JSON key file, the
//     format `gcloud iam service-accounts keys create` produces and the one
//     every Kubernetes secret and CI variable already holds. It is pinned
//     explicitly rather than left to be discovered, so the file that is meant
//     to be used is the file that is used, and its *type* is pinned too -- see
//     the comment on the call below for why only a service-account key is
//     accepted here.
//
//  2. Application Default Credentials resolve -- the production path, and the
//     one with no key material on disk. GOOGLE_APPLICATION_CREDENTIALS, the
//     gcloud well-known file, GKE Workload Identity and the GCE service account
//     via the metadata server, service-account impersonation and Workload
//     Identity Federation all come from here, with expiry-aware refresh. Nexus
//     neither reorders nor narrows that chain.
//
//  3. Neither, but an endpoint is set -- unauthenticated, which is the emulator
//     path. Every Cloud Storage emulator (fake-gcs-server, the Google testbench)
//     is unauthenticated, and the SDK's own STORAGE_EMULATOR_HOST support
//     hard-codes exactly this pairing. Doing it from config instead of an
//     environment variable is what lets an emulator deployment be described
//     entirely in the YAML. It is logged at warn level because an
//     unauthenticated client talking to something that is not an emulator is a
//     mistake worth seeing.
//
// Anything else is a boot failure. That is a deliberate improvement on the SDK,
// which does not error when it cannot find credentials -- it builds a client
// and fails at the first request instead, which under failure_policy: degrade
// would show up as a session that silently persists nothing.
//
// No context is taken, which is worth a word because every other boot-time
// helper in this file does. credentials.DetectDefault has no context parameter:
// it inspects the environment and, on a Google host, hands back a lazy
// credential that only talks to the metadata server when a token is first
// needed -- by which point the caller's own context governs the call.
//
// The detection in step 2 is a probe, not the resolution: the credentials it
// finds are thrown away and the SDK resolves the chain itself when the client
// is built. Passing the probed credentials in would pin the chain to whatever
// this module understood about it on the day it was written, and the property
// worth keeping is that an operator who has made `gcloud` work on a host has
// made this work.
func resolveCredentials(cfg objectstore.Config, endpoint string, logger *slog.Logger) ([]option.ClientOption, string, error) {
	if cfg.CredentialsFile != "" {
		// The path arrives already expanded: objectstore.Config documents that
		// the engine runs it through engine.ExpandPath at config load, and this
		// module must not re-expand it (nor could it -- ExpandPath lives in
		// package engine, which the seam is deliberately importable without).
		//
		// Stat it rather than letting the SDK discover the problem: a missing
		// key file surfaces from deep inside the auth library with a message
		// that names neither the config key nor, in some versions, the path.
		if _, err := os.Stat(cfg.CredentialsFile); err != nil {
			return nil, "", fmt.Errorf("gcs object store: credentials_file %q: %w", cfg.CredentialsFile, err)
		}
		// The credential type is pinned to a service-account key rather than
		// left open. option.WithCredentialsFile, which accepts any of them, is
		// deprecated for a reason worth honouring here: an external-account
		// (Workload Identity Federation) or impersonation configuration names a
		// URL the auth library will fetch a token from, so accepting one from a
		// file path that may itself have come from a shared config repository
		// hands an attacker a credential-exfiltration primitive. A Workload
		// Identity Federation configuration belongs on the ambient path via
		// GOOGLE_APPLICATION_CREDENTIALS, where an operator has opted into it
		// at the environment level, not in a YAML key documented as "a static
		// service-account key file".
		return []option.ClientOption{
			option.WithAuthCredentialsFile(option.ServiceAccount, cfg.CredentialsFile),
		}, "file:" + cfg.CredentialsFile, nil
	}

	// ScopeFullControl is what the storage client asks for itself, so probing
	// with anything narrower could succeed where the real client will not.
	_, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: []string{storage.ScopeFullControl}})
	if err == nil {
		return nil, "adc", nil
	}
	if endpoint == "" {
		return nil, "", fmt.Errorf("gcs object store: no Google credentials found; "+
			"set core.object_store.credentials_file, configure Application Default Credentials, "+
			"or set core.object_store.endpoint to talk to an emulator unauthenticated: %w", err)
	}
	logger.Warn("gcs object store is unauthenticated",
		"endpoint", endpoint,
		"reason", "no credentials_file and no Application Default Credentials; assuming an emulator")
	return []option.ClientOption{option.WithoutAuthentication()}, "anonymous", nil
}

// normalizeEndpoint validates core.object_store.endpoint and turns it into the
// base URL the SDK wants.
//
// The rule an operator sees is the same one the S3 backend states: an absolute
// http(s) URL naming a host. Catching a bare "localhost:4443" here is the
// difference between a boot failure naming core.object_store.endpoint and an
// unresolvable-host error much later that mentions no config key at all.
//
// The JSON API path is appended when the URL has none, so
// "http://127.0.0.1:4443" becomes "http://127.0.0.1:4443/storage/v1/". A URL
// that already carries a path is left alone -- an emulator behind a reverse
// proxy on a sub-path is a real deployment, and rewriting it would break the
// one operator who needed the escape hatch.
func normalizeEndpoint(endpoint string) (string, error) {
	if endpoint == "" {
		return "", nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("gcs object store: endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("gcs object store: endpoint %q must be an absolute http:// or https:// URL", endpoint)
	}
	if u.Host == "" {
		return "", fmt.Errorf("gcs object store: endpoint %q has no host", endpoint)
	}
	if strings.Trim(u.Path, "/") == "" {
		u.Path = "/" + jsonAPIPath
	}
	return u.String(), nil
}
