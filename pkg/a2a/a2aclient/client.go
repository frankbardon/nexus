package a2aclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// Defaults. Each is overridable; the reasoning for the value is with its option.
const (
	// DefaultRequestTimeout bounds a control-plane call — GetTask, CancelTask.
	// Those answer from the remote's task store and have no reason to be slow.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultStreamOpenTimeout bounds the wait for a streaming call's response
	// HEADERS, not its body. A stream that has not been accepted in this long
	// is not going to be.
	DefaultStreamOpenTimeout = 30 * time.Second
	// DefaultStreamIdleTimeout bounds total silence on an open stream. See
	// WithStreamIdleTimeout for why the default is non-zero and why "silence"
	// is measured in bytes rather than frames.
	DefaultStreamIdleTimeout = 5 * time.Minute
	// DefaultUserAgent identifies this client to a remote agent.
	DefaultUserAgent = "nexus-a2aclient/1.0"
)

// Client talks A2A to one remote agent. It is safe for concurrent use; the
// resolved Agent Card is fetched once and shared.
//
// Construct one with New. The zero value is not usable.
type Client struct {
	base    *url.URL
	binding a2a.ProtocolBinding

	jsonrpcEndpoint string
	restEndpoint    string

	httpc      *http.Client
	creds      CredentialSource
	retry      RetryPolicy
	extensions []string
	userAgent  string

	requestTimeout    time.Duration
	messageTimeout    time.Duration
	streamOpenTimeout time.Duration
	streamIdleTimeout time.Duration

	validateCard bool

	mu          sync.Mutex
	card        *a2a.AgentCard
	credsCarded bool

	rpcID atomic.Uint64
}

// Option configures a Client.
type Option func(*Client)

// WithBinding selects the protocol binding. a2a.BindingJSONRPC is the default;
// a2a.BindingHTTPJSON selects the REST binding. Any other value is rejected by
// New, since this package implements only the two HTTP bindings.
func WithBinding(b a2a.ProtocolBinding) Option {
	return func(c *Client) { c.binding = b }
}

// WithJSONRPCEndpoint pins the JSON-RPC endpoint URL, skipping Agent Card
// discovery for it. Use it when the endpoint is known out of band.
func WithJSONRPCEndpoint(endpoint string) Option {
	return func(c *Client) { c.jsonrpcEndpoint = strings.TrimRight(endpoint, "/") }
}

// WithRESTEndpoint pins the HTTP+JSON base URL — the prefix the operation paths
// hang off, not a single operation URL — skipping Agent Card discovery for it.
func WithRESTEndpoint(endpoint string) Option {
	return func(c *Client) { c.restEndpoint = strings.TrimRight(endpoint, "/") }
}

// WithCard supplies a pre-obtained Agent Card, skipping the well-known fetch.
// Specification section 8.2 sanctions distributing a card out of band ("Direct
// Configuration") for an agent that does not publish one.
func WithCard(card *a2a.AgentCard) Option {
	return func(c *Client) { c.card = card }
}

// WithCardValidation controls whether a fetched card is checked against the
// specification's required fields. It is on by default: a card that omits them
// is a remote this client cannot reason about, and failing at discovery is a
// better diagnosis than a confusing failure three calls later. Turn it off to
// interoperate with a remote whose card is usable but not strictly conformant.
func WithCardValidation(enabled bool) Option {
	return func(c *Client) { c.validateCard = enabled }
}

// WithHTTPClient overrides the underlying *http.Client. This is also how mutual
// TLS is configured: supply a client whose Transport carries the client
// certificate.
//
// A Timeout set on the supplied client applies to streaming calls too and will
// truncate a long-running stream. Leave it zero and use WithRequestTimeout,
// which this package applies per call and only where it is safe.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpc = hc }
}

// WithCredentials sets the credential source applied to every request. A nil
// source means no credentials, which is correct for an open endpoint.
func WithCredentials(src CredentialSource) Option {
	return func(c *Client) { c.creds = src }
}

// WithRetryPolicy overrides the retry policy. See RetryPolicy for what is
// retried and why it differs between reads and message sends.
func WithRetryPolicy(p RetryPolicy) Option {
	return func(c *Client) { c.retry = p }
}

// WithExtensions declares the A2A protocol extension URIs this client
// understands, sent as the A2A-Extensions service parameter. A server activates
// only the extensions a client asked for.
func WithExtensions(uris ...string) Option {
	return func(c *Client) { c.extensions = append(c.extensions[:0:0], uris...) }
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithRequestTimeout bounds a control-plane call: GetTask and CancelTask. It
// does NOT bound SendMessage, which blocks for the duration of the remote's
// work by design (specification section 3.2.2) and is bounded by
// WithMessageTimeout instead. Zero disables it, leaving the caller's context as
// the only deadline.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.requestTimeout = d }
}

// WithMessageTimeout bounds a non-streaming SendMessage. It defaults to zero —
// no client-imposed deadline — because a blocking SendMessage legitimately
// takes as long as the remote's work does, and a package-level guess at that
// duration would abort correct calls. Set it when the caller knows the bound.
func WithMessageTimeout(d time.Duration) Option {
	return func(c *Client) { c.messageTimeout = d }
}

// WithStreamOpenTimeout bounds the wait for a streaming call's response
// headers. Zero disables it.
func WithStreamOpenTimeout(d time.Duration) Option {
	return func(c *Client) { c.streamOpenTimeout = d }
}

// WithStreamIdleTimeout bounds total silence on an open stream. Zero disables
// it.
//
// Silence is measured in BYTES READ, not frames decoded. That distinction is
// the whole reason a non-zero default is safe: A2A's keep-alive mechanism is an
// SSE comment record, which carries no frame, and a client that reset its idle
// clock only on frames would kill exactly the parked INPUT_REQUIRED stream the
// comments exist to hold open. Counting bytes means any sign of life counts,
// and only a genuinely dead connection trips the timeout.
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(c *Client) { c.streamIdleTimeout = d }
}

// New builds a Client for the remote agent published at baseURL. The base URL
// is the ORIGIN (and optional path prefix) the agent is served under, not an
// operation endpoint: the Agent Card is fetched from AgentCardPath beneath it
// and names the per-binding endpoints.
//
// The base URL may be empty only when an endpoint is pinned with
// WithJSONRPCEndpoint or WithRESTEndpoint, since discovery is then unnecessary.
func New(baseURL string, opts ...Option) (*Client, error) {
	c := &Client{
		binding:           a2a.BindingJSONRPC,
		retry:             DefaultRetryPolicy(),
		userAgent:         DefaultUserAgent,
		requestTimeout:    DefaultRequestTimeout,
		streamOpenTimeout: DefaultStreamOpenTimeout,
		streamIdleTimeout: DefaultStreamIdleTimeout,
		validateCard:      true,
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}

	switch c.binding {
	case a2a.BindingJSONRPC, a2a.BindingHTTPJSON:
	default:
		return nil, &BindingError{
			Binding: string(c.binding),
			Detail: fmt.Sprintf("unsupported binding; this client speaks %s and %s",
				a2a.BindingJSONRPC, a2a.BindingHTTPJSON),
			Err: ErrNoEndpoint,
		}
	}

	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("a2aclient: parse base url %q: %w", baseURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("a2aclient: base url %q must be http or https", baseURL)
		}
		u.Path = strings.TrimRight(u.Path, "/")
		c.base = u
	} else if c.pinnedEndpoint() == "" {
		return nil, fmt.Errorf("a2aclient: a base url is required unless an endpoint is pinned")
	}

	if c.httpc == nil {
		// No Timeout: it would apply to streaming responses too. Per-call
		// deadlines are applied through the context instead.
		c.httpc = &http.Client{}
	}
	if c.creds == nil {
		c.creds = NoCredentials{}
	}
	return c, nil
}

// BaseURL returns the configured base URL, empty when only an endpoint was
// pinned.
func (c *Client) BaseURL() string {
	if c.base == nil {
		return ""
	}
	return c.base.String()
}

// Binding returns the protocol binding this client speaks.
func (c *Client) Binding() a2a.ProtocolBinding { return c.binding }

// pinnedEndpoint returns the explicitly configured endpoint for the active
// binding, empty when discovery is required.
func (c *Client) pinnedEndpoint() string {
	if c.binding == a2a.BindingHTTPJSON {
		return c.restEndpoint
	}
	return c.jsonrpcEndpoint
}

// ---- Request plumbing ----

// httpCall is one outgoing HTTP request, described completely enough that the
// retry loop can rebuild it.
type httpCall struct {
	operation string
	method    string
	url       string
	body      []byte
	accept    string
	// idempotent widens the retry set. See RetryPolicy.
	idempotent bool
}

// newRequest builds one attempt of a call, including service parameters and
// credentials. Credentials are applied per attempt so a source that refreshes
// an expired token does so on the retry rather than replaying a stale one.
func (c *Client) newRequest(ctx context.Context, call httpCall) (*http.Request, error) {
	var body io.Reader
	if call.body != nil {
		body = bytes.NewReader(call.body)
	}
	req, err := http.NewRequestWithContext(ctx, call.method, call.url, body)
	if err != nil {
		return nil, fmt.Errorf("a2aclient: build %s request: %w", call.operation, err)
	}
	if call.body != nil {
		payload := call.body
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", a2a.ContentTypeJSON)
	}
	if call.accept != "" {
		req.Header.Set("Accept", call.accept)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	// Section 3.6.1: a 1.0 client always states its version. Omitting it means
	// 0.3 to a conforming server, which would reject this client's payloads.
	a2a.ServiceParams{Version: a2a.ProtocolVersion, Extensions: c.extensions}.Apply(req.Header)

	if c.creds != nil {
		if err := c.creds.Apply(ctx, req); err != nil {
			return nil, fmt.Errorf("a2aclient: apply credentials for %s: %w", call.operation, err)
		}
	}
	return req, nil
}

// send performs a call with retries, returning the live response. The caller
// owns closing the body. A response that will be retried is drained and closed
// here so the connection is reusable.
func (c *Client) send(ctx context.Context, call httpCall) (*http.Response, error) {
	attempts := c.retry.attempts()
	var (
		delay   time.Duration
		lastErr error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, delay); err != nil {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, fmt.Errorf("a2aclient: %s: %w", call.operation, err)
			}
		}

		req, err := c.newRequest(ctx, call)
		if err != nil {
			return nil, err
		}

		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("a2aclient: %s: %w", call.operation, err)
			// A cancelled context is the caller's decision, not a fault to
			// retry around.
			if ctx.Err() != nil || !call.idempotent || attempt == attempts {
				return nil, lastErr
			}
			delay = c.retry.backoff(attempt, 0)
			continue
		}

		if attempt < attempts && retryableStatus(resp.StatusCode, call.idempotent) {
			retryAfter := parseRetryAfter(resp.Header, time.Now())
			lastErr = &HTTPError{
				StatusCode:  resp.StatusCode,
				Status:      resp.Status,
				URL:         call.url,
				ContentType: resp.Header.Get("Content-Type"),
			}
			drain(resp)
			delay = c.retry.backoff(attempt, retryAfter)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("a2aclient: %s: exhausted %d attempts", call.operation, attempts)
	}
	return nil, lastErr
}

// doJSON performs a call whose response is a single JSON body, returning the
// body bytes and the response headers. A non-2xx response is converted to the
// most specific error available.
func (c *Client) doJSON(ctx context.Context, call httpCall) ([]byte, http.Header, error) {
	if call.accept == "" {
		call.accept = a2a.ContentTypeJSON + ", application/json"
	}
	resp, err := c.send(ctx, call)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header, fmt.Errorf("a2aclient: %s: read response: %w", call.operation, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.Header, interpretHTTPError(call.url, resp, body)
	}
	return body, resp.Header, nil
}

// rpc performs one JSON-RPC operation and decodes its result into out.
func (c *Client) rpc(ctx context.Context, endpoint, method string, params, out any, idempotent bool) (http.Header, error) {
	id := strconv.FormatUint(c.rpcID.Add(1), 10)
	req, err := a2a.NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	payload, err := req.Encode()
	if err != nil {
		return nil, err
	}

	body, header, err := c.doJSON(ctx, httpCall{
		operation:  method,
		method:     http.MethodPost,
		url:        endpoint,
		body:       payload,
		idempotent: idempotent,
	})
	if err != nil {
		return header, err
	}

	resp, err := a2a.DecodeResponse(body)
	if err != nil {
		return header, a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
			"%s: %v", method, err)
	}
	// A response answering a different request is a correlation failure, and
	// silently accepting it would let a confused server hand one call's result
	// to another.
	if want, got := string(req.ID), strings.TrimSpace(string(resp.ID)); want != got {
		return header, a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
			"%s: response id %s does not match request id %s", method, got, want)
	}
	if err := resp.DecodeResult(out); err != nil {
		return header, wrapDecodeError(method, err)
	}
	return header, nil
}

// restJSON performs one REST operation and decodes its JSON body into out.
func (c *Client) restJSON(ctx context.Context, call httpCall, out any) (http.Header, error) {
	body, header, err := c.doJSON(ctx, call)
	if err != nil {
		return header, err
	}
	if out == nil {
		return header, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return header, a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse,
			"%s: decode response: %v", call.operation, err)
	}
	return header, nil
}

// wrapDecodeError keeps a protocol error the server sent as itself, and turns a
// local decode failure into an InvalidAgentResponseError. DecodeResult conflates
// the two by returning error; a caller branching on *a2a.Error must not see a
// server's TaskNotFoundError rewritten as something else.
func wrapDecodeError(operation string, err error) error {
	var protoErr *a2a.Error
	if errors.As(err, &protoErr) {
		return protoErr
	}
	return a2a.Errorf(a2a.ErrorTypeInvalidAgentResponse, "%s: %v", operation, err)
}

// interpretHTTPError converts a non-2xx response into the most specific error
// available: the A2A error the body encodes in either binding's framing, or an
// *HTTPError when the body is not an A2A error at all — which is what an
// intermediary's 502 page looks like.
func interpretHTTPError(callURL string, resp *http.Response, body []byte) error {
	if protoErr := decodeProtocolError(body); protoErr != nil {
		return protoErr
	}
	return &HTTPError{
		StatusCode:  resp.StatusCode,
		Status:      resp.Status,
		URL:         callURL,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        snippet(body),
	}
}

// decodeProtocolError recovers the *a2a.Error a response body encodes in either
// binding's framing, or nil when the body is not an A2A error at all.
//
// It is deliberately independent of the HTTP status: the JSON-RPC binding
// answers 200 even for an error, so a refusal is just as likely to arrive on a
// success status as on a failure one.
func decodeProtocolError(body []byte) error {
	if len(body) == 0 || !json.Valid(body) {
		return nil
	}
	// JSON-RPC framing: {"jsonrpc":"2.0","error":{...}}.
	var rpc struct {
		JSONRPC string        `json:"jsonrpc"`
		Error   *a2a.RPCError `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err == nil && rpc.JSONRPC != "" && rpc.Error != nil {
		return rpc.Error.AsError()
	}
	// REST framing: the google.rpc.Status body of section 11.6.
	var rest a2a.RESTError
	if err := json.Unmarshal(body, &rest); err == nil {
		if rest.Error.Code != 0 || rest.Error.Status != "" || rest.Error.Message != "" {
			return rest.AsError()
		}
	}
	return nil
}

// drain reads and closes a response body so its connection can be reused.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// withTimeout derives a context carrying d as its deadline, or returns ctx and
// a no-op cancel when d is zero.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}
