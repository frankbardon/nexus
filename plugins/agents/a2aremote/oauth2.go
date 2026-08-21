package a2aremote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// OAuth 2.0 client credentials (RFC 6749 §4.4), hand-rolled over net/http.
//
// There is no golang.org/x/oauth2 dependency here, and that is a house rule
// rather than an oversight: every LLM provider in this repo speaks its wire
// protocol through net/http, and a grant that is one form POST and one JSON
// response does not justify being the exception. What the library would supply
// beyond that POST — the browser-interactive grants, token sources chained off
// cloud metadata servers — is not what a machine-to-machine agent call needs.

const (
	// oauth2TokenTimeout bounds one token request. A token endpoint answers
	// from its own datastore; a minute of it is already pathological, and the
	// delegated call has its own, much larger budget waiting behind this.
	oauth2TokenTimeout = 30 * time.Second

	// oauth2DefaultLifetime is how long a token is trusted when the server
	// omits expires_in. RFC 6749 only RECOMMENDS the field, so a conforming
	// server may leave it out; a short assumed lifetime re-fetches more often
	// than necessary but never presents a token the server has already expired.
	oauth2DefaultLifetime = 60 * time.Second

	// defaultRefreshLeeway is how far ahead of the stated expiry a token is
	// replaced, covering the flight time of the request it is about to
	// authenticate plus any clock skew between here and the remote.
	defaultRefreshLeeway = 30 * time.Second
)

// Accepted `auth_style` values: how the client authenticates ITSELF to the
// token endpoint. RFC 6749 §2.3.1 makes HTTP Basic the mechanism a server MUST
// support and form parameters the one it MAY support, so basic is the default
// and body is the opt-in for a server that only does the latter.
const (
	authStyleBasic = "basic"
	authStyleBody  = "body"
)

// oauth2Source obtains and caches an access token via the client credentials
// grant and presents it as a bearer token on every A2A request.
//
// # The cache, and why it is single-flight
//
// One Nexus instance can have many delegated calls to the same remote in
// flight: a model that fans out to a remote agent produces a burst of tool
// calls that all start within milliseconds of each other, and each one is a
// separate goroutine reaching Apply at the same moment. A cache guarded by
// nothing but "is the token expired?" would send every one of those goroutines
// to the token endpoint simultaneously — a stampede that most authorization
// servers answer with a 429, converting a working configuration into an
// intermittently failing one exactly when it is busiest.
//
// So a fetch is single-flight: the first goroutine to find the cache cold
// becomes the fetcher and publishes an `inflight` channel; every other
// goroutine waits on that channel rather than issuing its own request. When the
// fetch succeeds the waiters read the cached token. When it fails they take the
// fetcher's error rather than each retrying, which keeps a token endpoint that
// is down from being hit N times per burst; the NEXT call after the burst
// starts a fresh fetch, so a transient failure is not sticky.
//
// A waiter still respects its own context: a delegated call whose budget
// expires while waiting for a token gives up rather than blocking on a fetch it
// no longer needs.
type oauth2Source struct {
	agent  string
	logger *slog.Logger
	warn   *mismatchWarner

	clientID     string
	clientSecret string
	scopes       []string
	audience     string
	authStyle    string
	leeway       time.Duration

	httpc *http.Client

	mu sync.Mutex
	// tokenURL is the configured endpoint, or the one discovered from the
	// card's oauth2 scheme on first use.
	tokenURL string
	token    string
	expires  time.Time
	// inflight is non-nil while a fetch is running, and is closed when it ends.
	inflight chan struct{}
	// lastErr is the outcome of the most recent fetch, handed to the waiters
	// that were parked on it.
	lastErr error
}

// newOAuth2Source builds the source from validated configuration.
func newOAuth2Source(cc credentialConfig, agent string, logger *slog.Logger, warn *mismatchWarner) *oauth2Source {
	leeway := cc.refreshLeeway
	if leeway <= 0 {
		leeway = defaultRefreshLeeway
	}
	style := cc.authStyle
	if style == "" {
		style = authStyleBasic
	}
	return &oauth2Source{
		agent:        agent,
		logger:       logger,
		warn:         warn,
		clientID:     cc.clientID,
		clientSecret: cc.clientSecret,
		scopes:       cc.scopes,
		audience:     cc.audience,
		authStyle:    style,
		leeway:       leeway,
		tokenURL:     cc.tokenURL,
		// No Timeout on the client: the per-request deadline rides on the
		// context, which is what lets a caller's cancellation reach the fetch.
		httpc: &http.Client{},
	}
}

// UseCard discovers the token endpoint from the remote's own declaration, and
// runs the credential/scheme mismatch check.
//
// Discovery is the point of the card-aware seam: the token endpoint of an
// OAuth2-protected agent is published in its card's clientCredentials flow, so
// an operator who supplies a client id and secret should not also have to copy
// a URL out of the remote's documentation and keep it in step.
//
// Returning an error here aborts the call, and this is the one place that is
// right: a source with no token endpoint cannot produce a token, so every
// subsequent request would be a 401 with a less useful explanation.
func (s *oauth2Source) UseCard(_ context.Context, card *a2a.AgentCard) error {
	s.warn.check(card)

	s.mu.Lock()
	configured := s.tokenURL
	s.mu.Unlock()
	if configured != "" {
		return nil
	}

	discovered := clientCredentialsTokenURL(card)
	if discovered == "" {
		return fmt.Errorf(
			"remote agent %q declares no oauth2 client-credentials flow to take a token endpoint from; set %s.%s",
			s.agent, cfgKeyCredentials, cfgKeyTokenURL)
	}

	s.mu.Lock()
	if s.tokenURL == "" {
		s.tokenURL = discovered
	}
	s.mu.Unlock()

	s.logger.Info("a2a_remote discovered an oauth2 token endpoint from the agent card",
		"agent", s.agent, "token_url", discovered)
	return nil
}

// clientCredentialsTokenURL returns the token endpoint of the first oauth2
// scheme on the card that declares a client credentials flow.
func clientCredentialsTokenURL(card *a2a.AgentCard) string {
	if card == nil {
		return ""
	}
	// Map iteration is unordered, so pick deterministically by scheme name.
	names := make([]string, 0, len(card.SecuritySchemes))
	for name := range card.SecuritySchemes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		scheme := card.SecuritySchemes[name]
		if scheme.OAuth2 == nil || scheme.OAuth2.Flows.ClientCredentials == nil {
			continue
		}
		if u := strings.TrimSpace(scheme.OAuth2.Flows.ClientCredentials.TokenURL); u != "" {
			return u
		}
	}
	return ""
}

// Apply attaches the access token, obtaining or refreshing it first.
//
// # The one unauthenticated request
//
// When the token endpoint is being discovered from the card rather than
// configured, there is exactly one request that necessarily precedes it: the
// well-known Agent Card fetch. That request goes out WITHOUT a credential,
// because the alternative is a deadlock — the token needs the card and the card
// would need the token. It is sound because specification section 8.2 makes the
// well-known card a public document; a remote that protects its card is telling
// the operator to configure token_url explicitly, and the 401 it answers with
// says so.
//
// Every request after the card has resolved carries a token, and a card that
// declares no client-credentials flow aborts the call in UseCard before any
// operation request is made — so this is never a silent downgrade.
func (s *oauth2Source) Apply(ctx context.Context, req *http.Request) error {
	s.mu.Lock()
	tokenURL := s.tokenURL
	s.mu.Unlock()
	if tokenURL == "" {
		return nil
	}

	token, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set(defaultBearerHeader, defaultBearerScheme+" "+token)
	return nil
}

// accessToken returns a live token, fetching one at most once per burst.
func (s *oauth2Source) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()

	if token, ok := s.liveToken(); ok {
		s.mu.Unlock()
		return token, nil
	}

	// Somebody else is already fetching: wait for them rather than piling onto
	// the token endpoint.
	if wait := s.inflight; wait != nil {
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return "", fmt.Errorf("oauth2: waiting for an access token for remote agent %q: %w", s.agent, ctx.Err())
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if token, ok := s.liveToken(); ok {
			return token, nil
		}
		if s.lastErr != nil {
			return "", s.lastErr
		}
		return "", fmt.Errorf("oauth2: no access token for remote agent %q", s.agent)
	}

	// Become the fetcher.
	done := make(chan struct{})
	s.inflight = done
	tokenURL := s.tokenURL
	s.mu.Unlock()

	token, expires, err := s.fetch(ctx, tokenURL)

	s.mu.Lock()
	if err == nil {
		s.token, s.expires = token, expires
	}
	s.lastErr = err
	s.inflight = nil
	s.mu.Unlock()
	close(done)

	if err != nil {
		return "", err
	}
	return token, nil
}

// liveToken reports the cached token when it is still usable. The caller holds
// s.mu.
func (s *oauth2Source) liveToken() (string, bool) {
	if s.token == "" {
		return "", false
	}
	if !s.expires.IsZero() && !time.Now().Before(s.expires) {
		return "", false
	}
	return s.token, true
}

// fetch performs the client credentials POST.
//
// Nothing it returns carries a credential value. A token endpoint that refuses
// the client answers with an RFC 6749 error CODE — a fixed enum such as
// invalid_client — and that code plus the HTTP status is the whole of what an
// operator needs; the free-text error_description is deliberately dropped,
// because a server is free to echo the client id or secret into it and this
// package will not be the thing that writes it to a log.
func (s *oauth2Source) fetch(ctx context.Context, tokenURL string) (string, time.Time, error) {
	if tokenURL == "" {
		return "", time.Time{}, fmt.Errorf(
			"oauth2: no token endpoint for remote agent %q: set %s.%s or point the agent at a base_url whose card declares one",
			s.agent, cfgKeyCredentials, cfgKeyTokenURL)
	}

	ctx, cancel := context.WithTimeout(ctx, oauth2TokenTimeout)
	defer cancel()

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if len(s.scopes) > 0 {
		form.Set("scope", strings.Join(s.scopes, " "))
	}
	if s.audience != "" {
		form.Set("audience", s.audience)
	}
	if s.authStyle == authStyleBody {
		form.Set("client_id", s.clientID)
		form.Set("client_secret", s.clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("oauth2: build token request for remote agent %q: %w", s.agent, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if s.authStyle != authStyleBody {
		// RFC 6749 §2.3.1: the id and secret are form-urlencoded BEFORE being
		// base64'd into the Basic credential.
		req.SetBasicAuth(url.QueryEscape(s.clientID), url.QueryEscape(s.clientSecret))
	}

	resp, err := s.httpc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("oauth2: token request for remote agent %q failed: %w", s.agent, err)
	}
	defer resp.Body.Close()

	// A token response is small; a runaway body is not read into memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("oauth2: read token response for remote agent %q: %w", s.agent, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf(
			"oauth2: token endpoint refused the client credentials for remote agent %q (HTTP %d%s); check %s.%s and %s.%s",
			s.agent, resp.StatusCode, errorCodeSuffix(body),
			cfgKeyCredentials, cfgKeyClientID, cfgKeyCredentials, cfgKeyClientSecret)
	}

	// expires_in is read as a bare JSON value rather than a float, because a
	// non-trivial number of authorization servers quote it ("expires_in":
	// "3600"). Insisting on the specified type there would reject an otherwise
	// perfectly good token over a formatting wart.
	var parsed struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   any    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf(
			"oauth2: token endpoint for remote agent %q answered something that is not a token response", s.agent)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return "", time.Time{}, fmt.Errorf(
			"oauth2: token endpoint for remote agent %q returned no access_token", s.agent)
	}
	if tt := strings.TrimSpace(parsed.TokenType); tt != "" && !strings.EqualFold(tt, "bearer") {
		// Anything other than a bearer token would have to ride in a different
		// place on the request, which this source does not know how to do.
		return "", time.Time{}, fmt.Errorf(
			"oauth2: token endpoint for remote agent %q issued a %q token; only bearer tokens can be presented to an A2A remote",
			s.agent, tt)
	}

	return parsed.AccessToken, s.expiryFor(secondsOf(parsed.ExpiresIn)), nil
}

// expiryFor turns the server's expires_in into the instant this source stops
// trusting the token.
//
// The leeway is subtracted, and then clamped: a token with a lifetime shorter
// than the leeway would otherwise be born expired and re-fetched on every
// single request, turning a short-lived-token deployment into the stampede the
// cache exists to prevent. Half the lifetime is the floor in that case.
func (s *oauth2Source) expiryFor(expiresIn float64) time.Time {
	lifetime := time.Duration(expiresIn * float64(time.Second))
	if lifetime <= 0 {
		lifetime = oauth2DefaultLifetime
	}
	usable := lifetime - s.leeway
	if usable <= 0 {
		usable = lifetime / 2
	}
	if usable <= 0 {
		usable = time.Second
	}
	return time.Now().Add(usable)
}

// secondsOf reads an expires_in value, tolerating both the number the
// specification calls for and the quoted number some servers send. Anything
// else reads as absent, which expiryFor turns into the assumed lifetime.
func secondsOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// errorCodeSuffix extracts the RFC 6749 `error` code from a token error
// response, rendered for inclusion in a message. The free-text
// error_description is deliberately NOT read; see fetch.
func errorCodeSuffix(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	code := strings.TrimSpace(parsed.Error)
	if code == "" {
		return ""
	}
	// The code is a fixed enum in the specification, but a non-conforming
	// server could put anything there, so it is length-bounded and stripped of
	// anything that is not an error code shape.
	if len(code) > 64 {
		code = code[:64]
	}
	for _, r := range code {
		isCodeRune := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if !isCodeRune {
			return ""
		}
	}
	return ", error=" + code
}
