// Package googleadc resolves Google Application Default Credentials and
// registers them with pkg/nexuscreds under the name "google-adc".
//
//	import _ "github.com/frankbardon/nexus/pkg/nexuscreds/googleadc"
//
// It is the one credential source Nexus ships, and it exists so a keyless pod
// can reach Vertex AI. On GKE with Workload Identity there is no key file
// anywhere: the pod's Kubernetes service account is bound to a Google service
// account, and the GCE metadata server hands out access tokens for it. ADC is
// the resolution chain that finds that, and everything else, without the
// operator saying which.
//
// # Everything is delegated
//
// This package holds no token cache, no expiry arithmetic and no refresh
// timer. It picks one of two library calls — google.FindDefaultCredentials
// when nothing is configured, google.CredentialsFromJSONWithTypeAndParams when
// a key file is — and hands back the AccessToken from the oauth2.TokenSource
// they return. That TokenSource already caches, already refreshes early, and
// already knows each credential type's refresh protocol.
//
// That delegation is the point of the package rather than an implementation
// detail. The Gemini provider it replaces hand-rolled RS256 JWT signing, a
// token cache, a mutex and an expiry comparison, and understood exactly one
// credential type — a service_account key file. Deleting all of it in favour
// of the library is what buys the other five types: authorized_user (a
// developer's `gcloud auth application-default login`), external_account
// (Workload Identity Federation from AWS, Azure or any OIDC provider),
// external_account_authorized_user, impersonated_service_account and
// gdch_service_account, plus the metadata server itself.
//
// # Scope
//
// The scope is fixed at https://www.googleapis.com/auth/cloud-platform and is
// not configurable. Vertex AI accepts no narrower scope, and a configurable
// one would be a knob whose only correct setting is the default — every other
// value produces a token Vertex rejects, at the first LLM call rather than at
// config load.
//
// # Dependency placement
//
// The Google dependency is confined to this subpackage on purpose. The parent
// nexuscreds package is stdlib-only, so a build that does not blank-import
// this one pays nothing for golang.org/x/oauth2/google or
// cloud.google.com/go/compute/metadata. See pkg/nexuscreds/doc.go.
//
// Unlike its parent, this package imports pkg/engine — for ExpandPath, the
// single canonical tilde-expansion helper every config-supplied path in Nexus
// runs through. That is deliberate: the alternative is a local expandHome
// copy, which the repository forbids outright.
package googleadc
