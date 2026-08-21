package a2aremote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
	"github.com/frankbardon/nexus/pkg/engine"
)

// The credential seam.
//
// a2aclient supplies credentials through a CredentialSource — an interface that
// mutates each outgoing request and may do work (a token refresh) first. This
// plugin binds one source per configured remote, and this file is where that
// binding happens.
//
// # Per remote, never inherited
//
// Unlike every transport knob, a `credentials:` block exists ONLY inside an
// `agents[]` entry. There is no plugin-level default and there must not be one:
// a default credential silently applied to a remote an operator added later is
// how a token reaches a host it was never issued for. Naming the credential on
// the remote it belongs to makes that impossible to do by accident.
//
// # Built once, at Init
//
// A source is built once per remote at Init rather than per call, because a
// CredentialSource is documented as safe for concurrent use and a token cache
// that is rebuilt per call is not a cache. Building at Init is also what makes
// a misconfiguration LOUD: an unset environment variable, an unreadable client
// certificate or a key that does not match its certificate stops the engine at
// boot with a message naming the agent and the key at fault, rather than
// surfacing as a 401 the first time a model happens to delegate.
//
// What is deliberately NOT done at Init is anything that touches the remote.
// The Agent Card is fetched lazily on first use (see remote.resolveCard), so a
// remote that is down cannot fail boot; everything that depends on the card —
// discovering an OAuth2 token endpoint, checking the configured credential
// against the card's declared securitySchemes — happens through
// a2aclient.CardAwareCredentialSource, on first use, off the boot path.
//
// # Secrets
//
// No credential value is logged, ever, on any path. Failure messages name the
// agent, the key and the kind of failure and stop there; the token, client
// secret and private key never appear in a log line, an error string or a tool
// result. credentialConfig implements slog.LogValuer so even logging the whole
// config by accident yields a redacted record rather than a secret.

// credentialKind names the credential a remote presents. It is the `type` key
// of the `credentials:` block, and the kinds are mutually exclusive: a remote
// presents one credential, and stacking (mTLS plus a bearer token, say) is a
// combination no configured remote has needed yet.
type credentialKind string

const (
	// credNone is the absence of a credentials block: an open endpoint, which
	// is exactly right for a loopback or development remote.
	credNone credentialKind = "none"
	// credBearer is a static token in a header, from config or an env var.
	credBearer credentialKind = "bearer"
	// credOAuth2 is the OAuth 2.0 client credentials grant (RFC 6749 §4.4).
	credOAuth2 credentialKind = "oauth2_client_credentials"
	// credMTLS is client-certificate authentication, configured on the
	// transport rather than on the request.
	credMTLS credentialKind = "mtls"
)

// credentialKinds lists the accepted `type` values, sorted, for error text.
func credentialKinds() []string {
	out := []string{string(credNone), string(credBearer), string(credOAuth2), string(credMTLS)}
	sort.Strings(out)
	return out
}

// Defaults for the bearer scheme. The header and scheme are configurable
// because a remote is free to want an `X-Api-Key: <token>` rather than an
// RFC 6750 `Authorization: Bearer <token>`, and both are one static string in
// one header.
const (
	defaultBearerHeader = "Authorization"
	defaultBearerScheme = "Bearer"
)

// credential is what a factory hands back: the request-level source, plus the
// HTTP client when the credential is a TRANSPORT-level one.
//
// The two are separate because mutual TLS is not expressible as a request
// mutation — the certificate is presented during the handshake, before any
// header exists — so it is configured by handing a2aclient a pre-built
// *http.Client through WithHTTPClient. A source is still returned in that case:
// it sends nothing, and exists to receive the card and check it against the
// configured credential.
type credential struct {
	source     a2aclient.CredentialSource
	httpClient *http.Client
}

// credentialFactory builds the credential for one configured remote. It is a
// field on Plugin so a test can substitute a source without a config block, and
// so the whole credential surface is reachable from one seam.
type credentialFactory func(agentConfig, *slog.Logger) (credential, error)

// buildCredential is the production factory.
//
// Everything it can fail on, it fails on here: an unreadable certificate, a key
// that does not match it, an OAuth2 remote with neither a configured token
// endpoint nor a card to discover one from. None of it contacts the remote.
func buildCredential(ac agentConfig, logger *slog.Logger) (credential, error) {
	cc := ac.credentials
	warn := &mismatchWarner{logger: logger, agent: ac.name, kind: cc.kind}

	switch cc.kind {
	case credNone, "":
		// The empty kind is the zero credentialConfig, which is what an agent
		// with no credentials block resolves to. It is the same thing as an
		// explicit `type: none`.
		return credential{source: &noneSource{warn: warn}}, nil

	case credBearer:
		return credential{source: &bearerSource{
			header: cc.header,
			scheme: cc.scheme,
			token:  cc.token,
			warn:   warn,
		}}, nil

	case credOAuth2:
		// The token endpoint comes from config or from the card's oauth2
		// scheme. A remote with neither is unsatisfiable, and saying so at boot
		// is better than discovering it on the first delegation.
		if cc.tokenURL == "" && ac.baseURL == "" {
			return credential{}, fmt.Errorf(
				"%s.%s is required: this agent pins an endpoint instead of a base_url, so there is no agent card to discover the token endpoint from",
				cfgKeyCredentials, cfgKeyTokenURL)
		}
		return credential{source: newOAuth2Source(cc, ac.name, logger, warn)}, nil

	case credMTLS:
		httpc, err := mtlsClient(cc)
		if err != nil {
			return credential{}, err
		}
		return credential{source: &mtlsSource{warn: warn}, httpClient: httpc}, nil

	default:
		return credential{}, fmt.Errorf("%s.%s: unknown credential type %q; want one of %s",
			cfgKeyCredentials, cfgKeyCredType, string(cc.kind), strings.Join(credentialKinds(), ", "))
	}
}

// ---- none ----

// noneSource sends no credentials. It is not a2aclient.NoCredentials because it
// still wants the card: a remote that declares security schemes while this
// instance is configured to send nothing is a misconfiguration worth one
// warning, and the 401 it otherwise produces says far less.
type noneSource struct{ warn *mismatchWarner }

func (noneSource) Apply(context.Context, *http.Request) error { return nil }

func (s *noneSource) UseCard(_ context.Context, card *a2a.AgentCard) error {
	s.warn.check(card)
	return nil
}

// ---- bearer ----

// bearerSource presents a static token, resolved at Init from either the
// inline `token` key or the environment variable named by `token_env` — the
// same choice, and the same precedence, that every LLM provider in this repo
// offers through api_key / api_key_env.
//
// The token is not re-read from the environment per request. A credential that
// changed under a running process would be a rotation mechanism, and a
// half-implemented one; rotation is a restart.
type bearerSource struct {
	header string
	scheme string
	token  string
	warn   *mismatchWarner
}

func (s *bearerSource) Apply(_ context.Context, req *http.Request) error {
	value := s.token
	if s.scheme != "" {
		value = s.scheme + " " + s.token
	}
	req.Header.Set(s.header, value)
	return nil
}

func (s *bearerSource) UseCard(_ context.Context, card *a2a.AgentCard) error {
	s.warn.check(card)
	return nil
}

// ---- mTLS ----

// mtlsSource carries no request-level credential: the certificate was presented
// during the TLS handshake by the transport mtlsClient built. It exists so the
// card still reaches the mismatch check.
type mtlsSource struct{ warn *mismatchWarner }

func (mtlsSource) Apply(context.Context, *http.Request) error { return nil }

func (s *mtlsSource) UseCard(_ context.Context, card *a2a.AgentCard) error {
	s.warn.check(card)
	return nil
}

// mtlsClient builds the HTTP client that presents the client certificate.
//
// It reads the files at Init on purpose. A certificate path that is wrong, a
// key that does not match it, or a CA bundle that contains no certificate are
// all operator errors that a handshake failure reports far less clearly, and
// all three are known before the engine finishes booting.
//
// The client carries NO Timeout: a2aclient documents that one would truncate a
// long-running stream, and per-call deadlines ride on the context instead.
func mtlsClient(cc credentialConfig) (*http.Client, error) {
	certPath := engine.ExpandPath(cc.certFile)
	keyPath := engine.ExpandPath(cc.keyFile)

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		// tls's own errors name the parse failure, never the key material.
		return nil, fmt.Errorf("%s: load client certificate %s / key %s: %w",
			cfgKeyCredentials, certPath, keyPath, err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cc.serverName != "" {
		tlsCfg.ServerName = cc.serverName
	}
	if cc.caFile != "" {
		caPath := engine.ExpandPath(cc.caFile)
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: read %s: %w", cfgKeyCredentials, cfgKeyCAFile, caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s.%s: %s contains no PEM certificate", cfgKeyCredentials, cfgKeyCAFile, caPath)
		}
		tlsCfg.RootCAs = pool
	}

	transport, _ := http.DefaultTransport.(*http.Transport)
	if transport == nil {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}, nil
	}
	clone := transport.Clone()
	clone.TLSClientConfig = tlsCfg
	return &http.Client{Transport: clone}, nil
}

// ---- Card mismatch ----

// mismatchWarner compares the configured credential against the security
// schemes the remote's card declares, and warns — once — when they obviously
// disagree.
//
// It WARNS and does not refuse, deliberately. A card's securitySchemes block is
// optional and routinely incomplete: a remote behind a gateway that terminates
// mTLS may declare nothing at all, and a remote that accepts a bearer token
// while declaring only oauth2 is conformant. Refusing on that evidence would
// break working deployments over a documentation defect. What the warning buys
// is that the far more common case — a credential configured against the wrong
// remote — is diagnosed in a log line instead of an opaque 401.
//
// The check runs on FIRST USE, not at boot, because that is when the card
// arrives. Nothing about it can fail a call.
type mismatchWarner struct {
	logger *slog.Logger
	agent  string
	kind   credentialKind
	once   sync.Once
}

// compatibleKinds maps a configured credential onto the card scheme kinds that
// plausibly accept it.
//
// The mapping is deliberately generous. A bearer token satisfies an httpAuth
// scheme by definition, an apiKey scheme when the operator has pointed it at
// the right header, and both oauth2 and openIdConnect, whose access tokens are
// presented as bearer tokens — an operator holding a pre-issued token for an
// OAuth2-protected remote is a normal arrangement, not a mistake. Only the
// genuinely incompatible pairing warns.
var compatibleKinds = map[credentialKind]map[a2a.SecuritySchemeKind]bool{
	credBearer: {
		a2a.SecuritySchemeHTTPAuth:      true,
		a2a.SecuritySchemeAPIKey:        true,
		a2a.SecuritySchemeOAuth2:        true,
		a2a.SecuritySchemeOpenIDConnect: true,
	},
	credOAuth2: {
		a2a.SecuritySchemeOAuth2:        true,
		a2a.SecuritySchemeOpenIDConnect: true,
		a2a.SecuritySchemeHTTPAuth:      true,
	},
	credMTLS: {
		a2a.SecuritySchemeMutualTLS: true,
	},
}

func (w *mismatchWarner) check(card *a2a.AgentCard) {
	if w == nil || w.logger == nil || card == nil {
		return
	}
	w.once.Do(func() { w.warn(card) })
}

func (w *mismatchWarner) warn(card *a2a.AgentCard) {
	declared := declaredKinds(card)
	if len(declared) == 0 {
		// An incomplete card says nothing about what the remote accepts, so
		// there is nothing to disagree with.
		return
	}

	if w.kind == credNone {
		w.logger.Warn("a2a_remote sends no credentials to a remote that declares security schemes",
			"agent", w.agent,
			"card_schemes", strings.Join(declared, ","),
			"hint", "add a credentials block to this agent, or ignore this if a gateway supplies the credential")
		return
	}

	accepted := compatibleKinds[w.kind]
	for _, kind := range declared {
		if accepted[a2a.SecuritySchemeKind(kind)] {
			return
		}
	}
	w.logger.Warn("a2a_remote credential does not match any security scheme the remote declares",
		"agent", w.agent,
		"configured", string(w.kind),
		"card_schemes", strings.Join(declared, ","),
		"hint", "the card may be incomplete; the call is attempted regardless")
}

// declaredKinds lists the distinct scheme kinds a card declares, sorted.
func declaredKinds(card *a2a.AgentCard) []string {
	seen := map[string]bool{}
	for _, scheme := range card.SecuritySchemes {
		if kind := scheme.Kind(); kind != a2a.SecuritySchemeUnset {
			seen[string(kind)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}
