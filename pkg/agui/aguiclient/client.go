// Package aguiclient is a reusable, pure-Go AG-UI conformance client. It POSTs a
// RunAgentInput to an AG-UI serve endpoint and decodes the resulting
// text/event-stream SSE response into a slice of typed AG-UI events using the
// pkg/agui codec.
//
// It has no dependency on the Nexus engine or event bus and speaks only the
// AG-UI wire, so it is shared by the serve-phase conformance tests and the
// Phase-4 consume client. No JS toolchain and no third-party AG-UI SDK are
// involved — this is standard-library Go on top of pkg/agui.
package aguiclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
)

// Client is an AG-UI conformance client bound to a single serve endpoint URL.
// The zero value is not usable; construct one with New.
type Client struct {
	url    string
	http   *http.Client
	bearer string
	origin string
}

// Option configures a Client.
type Option func(*Client)

// WithBearer sets the bearer token sent in the Authorization header. When empty
// (the default) no Authorization header is sent.
func WithBearer(token string) Option {
	return func(c *Client) { c.bearer = token }
}

// WithOrigin sets the Origin request header, used to exercise CORS behavior.
func WithOrigin(origin string) Option {
	return func(c *Client) { c.origin = origin }
}

// WithHTTPClient overrides the underlying *http.Client (e.g. to tune timeouts).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// New builds a Client for the AG-UI endpoint at url (the full POST path, e.g.
// "http://127.0.0.1:8090/agui").
func New(url string, opts ...Option) *Client {
	c := &Client{
		url:  url,
		http: &http.Client{Timeout: 60 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Result is the outcome of a single AG-UI run: the raw HTTP response metadata
// plus the fully decoded, ordered event stream.
type Result struct {
	// StatusCode is the HTTP status of the POST response.
	StatusCode int
	// Header is the response header set (Content-Type, CORS, ...).
	Header http.Header
	// Events is the ordered sequence of decoded AG-UI events. It is nil when the
	// server rejected the request before opening an SSE stream.
	Events []agui.Event
}

// Types returns the ordered event-type discriminators of the decoded stream, a
// convenience for asserting the canonical AG-UI sequence.
func (r Result) Types() []agui.EventType {
	out := make([]agui.EventType, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.EventType())
	}
	return out
}

// First returns the first event of the given type in the stream, or nil.
func (r Result) First(t agui.EventType) agui.Event {
	for _, e := range r.Events {
		if e.EventType() == t {
			return e
		}
	}
	return nil
}

// Count returns how many events of the given type appear in the stream.
func (r Result) Count(t agui.EventType) int {
	n := 0
	for _, e := range r.Events {
		if e.EventType() == t {
			n++
		}
	}
	return n
}

// Run POSTs the input and drains the SSE stream to completion, returning the
// decoded events. It blocks until the server closes the stream (RunFinished /
// RunError) or ctx is cancelled.
//
// A non-2xx status is not an error: the Result carries the status and header so
// callers can assert auth/CORS rejections. A transport failure or a malformed
// SSE record does return an error.
func (c *Client) Run(ctx context.Context, input agui.RunAgentInput) (Result, error) {
	resp, err := c.post(ctx, input)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	res := Result{StatusCode: resp.StatusCode, Header: resp.Header}

	// A rejection (non-2xx) or a non-SSE body carries no event stream; drain and
	// return the metadata so the caller can assert on it.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return res, nil
	}

	events, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		return res, fmt.Errorf("aguiclient: read sse stream: %w", err)
	}
	res.Events = events
	return res, nil
}

// post encodes the input, builds the POST request with auth/CORS/accept
// headers, and performs it. It returns the live *http.Response; the caller owns
// closing the body. Encoding, request-construction, and transport failures are
// returned as wrapped errors.
func (c *Client) post(ctx context.Context, input agui.RunAgentInput) (*http.Response, error) {
	body, err := input.Encode()
	if err != nil {
		return nil, fmt.Errorf("aguiclient: encode input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("aguiclient: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", agui.ContentType)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aguiclient: do request: %w", err)
	}
	return resp, nil
}

// UserMessage is a convenience for building a single-user-message RunAgentInput.
func UserMessage(threadID, runID, content string) agui.RunAgentInput {
	return agui.RunAgentInput{
		ThreadID: threadID,
		RunID:    runID,
		Messages: []agui.Message{{ID: "m1", Role: "user", Content: content}},
	}
}
