package a2a

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// authedConfig returns a config guarded by a single shared bearer token.
func authedConfig(t *testing.T, overrides map[string]any) map[string]any {
	t.Helper()
	base := map[string]any{"bearer_token": "s3cret"}
	for k, v := range overrides {
		base[k] = v
	}
	return testConfig(t, base)
}

func withBearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withVersion(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(a2a.HeaderVersion, v) }
}

func jsonrpcBody(t *testing.T, method string, params any) func(*http.Request) {
	t.Helper()
	req, err := a2a.NewRequest(1, method, params)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	data, err := req.Encode()
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return func(r *http.Request) {
		r.Body = readCloser(data)
		r.ContentLength = int64(len(data))
		r.Header.Set("Content-Type", a2a.ContentTypeJSON)
	}
}

// readCloser wraps a body payload for httptest.NewRequest, which does not take
// one directly when the request is built by target alone.
func readCloser(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}

// ---- Auth guard ----

// TestOperationsAreAuthGuarded asserts every operation surface refuses an
// unauthenticated caller, in the error shape its binding uses.
func TestOperationsAreAuthGuarded(t *testing.T) {
	s := newTestServer(t, authedConfig(t, nil))

	t.Run("jsonrpc without credentials", func(t *testing.T) {
		rec := do(t, s, http.MethodPost, "/a2a", withVersion("1.0"),
			jsonrpcBody(t, a2a.MethodGetTask, map[string]any{"id": "task-1"}))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ch := rec.Header().Get("WWW-Authenticate"); !strings.Contains(ch, `realm="nexus-a2a"`) {
			t.Errorf("WWW-Authenticate = %q, want an RFC 6750 challenge", ch)
		}
		var resp a2a.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("refusal is not a JSON-RPC envelope: %v (%s)", err, rec.Body)
		}
		if resp.JSONRPC != a2a.JSONRPCVersion || resp.Error == nil {
			t.Fatalf("refusal envelope = %+v", resp)
		}
		if resp.Error.Code != codeAuthDenied {
			t.Errorf("error code = %d, want %d (JSON-RPC implementation-defined range, outside A2A's -32001..-32099)",
				resp.Error.Code, codeAuthDenied)
		}
		if len(resp.Error.Data) == 0 || resp.Error.Data[0]["reason"] != reasonAuthRequired {
			t.Errorf("error data = %v, want an ErrorInfo naming the reason", resp.Error.Data)
		}
		if resp.Error.Data[0]["domain"] == a2a.ErrorDomain {
			t.Error("a non-protocol refusal was stamped with the A2A error domain")
		}
	})

	t.Run("jsonrpc with a bad token", func(t *testing.T) {
		rec := do(t, s, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("wrong"),
			jsonrpcBody(t, a2a.MethodGetTask, map[string]any{"id": "task-1"}))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ch := rec.Header().Get("WWW-Authenticate"); !strings.Contains(ch, "invalid_token") {
			t.Errorf("WWW-Authenticate = %q, want error=\"invalid_token\"", ch)
		}
	})

	t.Run("rest without credentials", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/a2a/v1/tasks/task-1", withVersion("1.0"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		var body a2a.RESTError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("refusal is not a google.rpc.Status body: %v (%s)", err, rec.Body)
		}
		if body.Error.Code != http.StatusUnauthorized || body.Error.Status != "UNAUTHENTICATED" {
			t.Errorf("refusal body = %+v", body.Error)
		}
		if len(body.Error.Details) == 0 || body.Error.Details[0]["reason"] != reasonAuthRequired {
			t.Errorf("details = %v, want an ErrorInfo naming the reason", body.Error.Details)
		}
	})

	t.Run("authenticated callers get past the guard", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/a2a/v1/tasks/task-1", withVersion("1.0"), withBearer("s3cret"))
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("a valid token was refused: %s", rec.Body)
		}
	})
}

// TestPreflightIsNotAuthGuarded pins the CORS carve-out: a browser never sends
// Authorization on a preflight.
func TestPreflightIsNotAuthGuarded(t *testing.T) {
	s := newTestServer(t, authedConfig(t, map[string]any{"cors_origins": "https://ui.example.test"}))
	for _, path := range []string{"/a2a", "/a2a/v1/tasks", a2a.AgentCardPath} {
		rec := do(t, s, http.MethodOptions, path, func(r *http.Request) {
			r.Header.Set("Origin", "https://ui.example.test")
		})
		if rec.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s status = %d, want 204", path, rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "https://ui.example.test" {
			t.Errorf("OPTIONS %s did not echo the allowed origin: %v", path, rec.Header())
		}
	}
}

// ---- Agent Card auth posture ----

// TestAgentCardAuthPosture pins the documented decision: public by default,
// gateable on request.
func TestAgentCardAuthPosture(t *testing.T) {
	t.Run("public by default even when operations are guarded", func(t *testing.T) {
		s := newTestServer(t, authedConfig(t, nil))
		rec := do(t, s, http.MethodGet, a2a.AgentCardPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; the discovery document is the pre-authentication bootstrap", rec.Code)
		}
	})

	t.Run("gated when card_requires_auth is set", func(t *testing.T) {
		s := newTestServer(t, authedConfig(t, map[string]any{"card_requires_auth": true}))
		rec := do(t, s, http.MethodGet, a2a.AgentCardPath)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("no challenge on a gated card")
		}
		rec = do(t, s, http.MethodGet, a2a.AgentCardPath, withBearer("s3cret"))
		if rec.Code != http.StatusOK {
			t.Fatalf("authenticated card fetch = %d: %s", rec.Code, rec.Body)
		}
	})
}

// ---- Version negotiation ----

// TestAbsentVersionHeaderPolicy pins both readings of specification 3.6.2 and
// the fact that the default is the lenient one.
func TestAbsentVersionHeaderPolicy(t *testing.T) {
	t.Run("lenient by default", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, nil))
		rec := do(t, s, http.MethodGet, "/a2a/v1/tasks/task-1")
		if rec.Code == http.StatusBadRequest {
			var body a2a.RESTError
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if len(body.Error.Details) > 0 && body.Error.Details[0]["reason"] == "VERSION_NOT_SUPPORTED" {
				t.Fatal("a header-less request was refused as 0.3; the default policy is to assume 1.0")
			}
		}
		if got := rec.Header().Get(a2a.HeaderVersion); got != a2a.ProtocolVersion {
			t.Errorf("response %s = %q, want %q so the client can see what it was processed as",
				a2a.HeaderVersion, got, a2a.ProtocolVersion)
		}
	})

	t.Run("strict when configured", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, map[string]any{"strict_version_header": true}))
		rec := do(t, s, http.MethodGet, "/a2a/v1/tasks/task-1")
		var body a2a.RESTError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
		}
		if len(body.Error.Details) == 0 || body.Error.Details[0]["reason"] != "VERSION_NOT_SUPPORTED" {
			t.Fatalf("strict mode did not reject an absent header as 0.3: %+v", body.Error)
		}
	})

	t.Run("an unsupported explicit version is always refused", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, nil))
		rec := do(t, s, http.MethodGet, "/a2a/v1/tasks/task-1", withVersion("0.3"))
		var body a2a.RESTError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
		}
		if len(body.Error.Details) == 0 || body.Error.Details[0]["reason"] != "VERSION_NOT_SUPPORTED" {
			t.Fatalf("an explicit 0.3 was accepted: %+v", body.Error)
		}
	})

	t.Run("the version may ride a query parameter", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, map[string]any{"strict_version_header": true}))
		rec := do(t, s, http.MethodGet, "/a2a/v1/tasks/task-1?A2A-Version=1.0")
		var body a2a.RESTError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
		}
		if len(body.Error.Details) > 0 && body.Error.Details[0]["reason"] == "VERSION_NOT_SUPPORTED" {
			t.Fatal("the A2A-Version query parameter was not honoured (specification section 3.6.1)")
		}
	})
}

// ---- Operation wiring ----

// TestOperationsReturnNotImplemented pins the current maturity line: the
// operations that are NOT wired are routed and answered with a well-formed
// UnsupportedOperationError, never a bare 404 or a silent success.
//
// The loop skips whatever implementedOperations claims, so wiring an operation
// moves it out of this test automatically instead of leaving a stale expectation
// that has to be remembered.
func TestOperationsReturnNotImplemented(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil))

	t.Run("jsonrpc", func(t *testing.T) {
		for _, method := range a2a.Methods() {
			if operationImplemented(method) {
				continue
			}
			t.Run(method, func(t *testing.T) {
				rec := do(t, s, http.MethodPost, "/a2a", withVersion("1.0"),
					jsonrpcBody(t, method, paramsFor(method)))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d; JSON-RPC carries its outcome in the body", rec.Code)
				}
				var resp a2a.Response
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("not a JSON-RPC envelope: %v (%s)", err, rec.Body)
				}
				if resp.Error == nil {
					t.Fatalf("expected an error response, got %+v", resp)
				}
				if resp.Error.Code != a2a.CodeUnsupportedOperation {
					t.Errorf("code = %d, want %d (UnsupportedOperationError)", resp.Error.Code, a2a.CodeUnsupportedOperation)
				}
				// The id from the request is echoed, as JSON-RPC requires.
				if string(resp.ID) != "1" {
					t.Errorf("id = %s, want the request id echoed", resp.ID)
				}
			})
		}
	})

	t.Run("rest", func(t *testing.T) {
		// Driven off the codec's own route table and filtered by
		// implementedOperations, for the same reason the JSON-RPC loop is: an
		// operation that gets wired leaves this test by itself.
		cases := []struct{ method, path string }{
			{http.MethodGet, "/a2a/v1/tasks/task-1"},
			{http.MethodGet, "/a2a/v1/tasks"},
			{http.MethodPost, "/a2a/v1/tasks/task-1:cancel"},
			{http.MethodPost, "/a2a/v1/tasks/task-1:subscribe"},
		}
		for _, c := range cases {
			suffix := strings.TrimPrefix(c.path, "/a2a/v1")
			route, _, found, _ := a2a.MatchRoute(c.method, suffix)
			if !found {
				t.Fatalf("%s %s does not resolve to an A2A route", c.method, c.path)
			}
			if operationImplemented(route.Operation) {
				continue
			}
			t.Run(c.method+" "+c.path, func(t *testing.T) {
				rec := do(t, s, c.method, c.path, withVersion("1.0"))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (UnsupportedOperationError maps to FAILED_PRECONDITION): %s", rec.Code, rec.Body)
				}
				var body a2a.RESTError
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("not a google.rpc.Status body: %v (%s)", err, rec.Body)
				}
				if body.Error.Status != "FAILED_PRECONDITION" {
					t.Errorf("status = %q", body.Error.Status)
				}
				if len(body.Error.Details) == 0 || body.Error.Details[0]["reason"] != "UNSUPPORTED_OPERATION" {
					t.Errorf("details = %v, want an ErrorInfo with reason UNSUPPORTED_OPERATION", body.Error.Details)
				}
			})
		}
	})
}

// paramsFor supplies the minimum valid parameter object for an operation, so
// the request reaches the dispatch check rather than failing validation.
func paramsFor(method string) any {
	switch method {
	case a2a.MethodSendMessage, a2a.MethodSendStreamingMessage:
		return map[string]any{"message": map[string]any{
			"messageId": "m1",
			"role":      "ROLE_USER",
			"parts":     []any{map[string]any{"text": "hello"}},
		}}
	case a2a.MethodListTasks:
		return map[string]any{}
	default:
		return map[string]any{"id": "task-1"}
	}
}

// TestUnknownAndMismatchedRoutes pins the two ways a REST path can miss.
func TestUnknownAndMismatchedRoutes(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil))

	t.Run("unknown path is MethodNotFound", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/a2a/v1/nope", withVersion("1.0"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
		}
	})

	t.Run("wrong verb on a real route is 405 with Allow", func(t *testing.T) {
		rec := do(t, s, http.MethodDelete, "/a2a/v1/tasks/task-1", withVersion("1.0"))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Errorf("Allow = %q, want the verb that would have worked", allow)
		}
	})
}

// TestUnimplementedSpecOperationsAreClassified pins that the push-notification
// and extended-card operations get their specified refusal, not a generic one.
func TestUnimplementedSpecOperationsAreClassified(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil))
	cases := map[string]int{
		a2a.MethodCreateTaskPushNotificationConfig: a2a.CodePushNotificationNotSupported,
		a2a.MethodGetExtendedAgentCard:             a2a.CodeUnsupportedOperation,
	}
	for method, wantCode := range cases {
		t.Run(method, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/a2a", withVersion("1.0"), jsonrpcBody(t, method, map[string]any{}))
			var resp a2a.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
			}
			if resp.Error == nil || resp.Error.Code != wantCode {
				t.Errorf("error = %+v, want code %d", resp.Error, wantCode)
			}
		})
	}
}

// TestMalformedJSONRPCIsClassified pins the transport-level error mapping.
func TestMalformedJSONRPCIsClassified(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil))

	cases := map[string]struct {
		body     string
		wantCode int
	}{
		"not json":       {`{nope`, a2a.CodeJSONParse},
		"bad envelope":   {`{"jsonrpc":"1.0","id":1,"method":"GetTask"}`, a2a.CodeInvalidRequest},
		"unknown method": {`{"jsonrpc":"2.0","id":1,"method":"Nope","params":{}}`, a2a.CodeMethodNotFound},
		"bad params":     {`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{}}`, a2a.CodeInvalidParams},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/a2a", withVersion("1.0"), func(r *http.Request) {
				r.Body = readCloser([]byte(c.body))
				r.ContentLength = int64(len(c.body))
			})
			var resp a2a.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
			}
			if resp.Error == nil || resp.Error.Code != c.wantCode {
				t.Errorf("error = %+v, want code %d", resp.Error, c.wantCode)
			}
		})
	}
}

// TestMessageOperationsAreWired is the counterpart to the test above: the
// operations this plugin implements must NOT answer with the not-implemented
// refusal, and the card must agree.
func TestMessageOperationsAreWired(t *testing.T) {
	for _, method := range []string{
		a2a.MethodSendMessage,
		a2a.MethodSendStreamingMessage,
		a2a.MethodGetTask,
		a2a.MethodListTasks,
		a2a.MethodSubscribeToTask,
	} {
		if !operationImplemented(method) {
			t.Errorf("%s is not marked implemented; the turn mapping and the card derive from this map", method)
		}
	}
}

// TestCardStreamingCapabilityCoversBothStreamingOperations pins the derivation
// the card makes: streaming is claimed because both operations that open a
// stream are wired, not because one of them once was.
func TestCardStreamingCapabilityCoversBothStreamingOperations(t *testing.T) {
	for _, method := range a2a.Methods() {
		if !a2a.IsStreamingMethod(method) {
			continue
		}
		if !operationImplemented(method) {
			t.Fatalf("the card claims streaming while %s is unimplemented", method)
		}
	}
	s := newTestServer(t, testConfig(t, nil))
	if !s.cfg.card.card.Capabilities.Streaming {
		t.Error("capabilities.streaming is false while both streaming operations are wired")
	}
	if s.cfg.card.card.Capabilities.PushNotifications || s.cfg.card.card.Capabilities.ExtendedAgentCard {
		t.Error("the card claims a capability this plugin does not implement")
	}
}

// TestTransportWithoutABridgeFailsClosed pins the defensive path: a Server built
// without a bus bridge (a transport-only test, or an embedder wiring it by hand)
// answers with an internal error rather than panicking on a nil bridge.
func TestTransportWithoutABridgeFailsClosed(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil))
	rec := do(t, s, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, paramsFor(a2a.MethodSendMessage)))

	var resp a2a.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not a JSON-RPC envelope: %v (%s)", err, rec.Body)
	}
	if resp.Error == nil || resp.Error.Code != a2a.CodeInternal {
		t.Fatalf("error = %+v, want an internal error", resp.Error)
	}
}
