package a2a

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestRESTRouteShapes pins every route from specification sections 5.3 and 11.3,
// including the HTTP verb and whether the response streams.
func TestRESTRouteShapes(t *testing.T) {
	tests := []struct {
		operation  string
		httpMethod string
		path       string
		streaming  bool
		supported  bool
	}{
		{MethodSendMessage, http.MethodPost, "/message:send", false, true},
		{MethodSendStreamingMessage, http.MethodPost, "/message:stream", true, true},
		{MethodGetTask, http.MethodGet, "/tasks/{id}", false, true},
		{MethodListTasks, http.MethodGet, "/tasks", false, true},
		{MethodCancelTask, http.MethodPost, "/tasks/{id}:cancel", false, true},
		{MethodSubscribeToTask, http.MethodPost, "/tasks/{id}:subscribe", true, true},
		{MethodCreateTaskPushNotificationConfig, http.MethodPost, "/tasks/{id}/pushNotificationConfigs", false, false},
		{MethodGetTaskPushNotificationConfig, http.MethodGet, "/tasks/{id}/pushNotificationConfigs/{configId}", false, false},
		{MethodListTaskPushNotificationConfigs, http.MethodGet, "/tasks/{id}/pushNotificationConfigs", false, false},
		{MethodDeleteTaskPushNotificationConfig, http.MethodDelete, "/tasks/{id}/pushNotificationConfigs/{configId}", false, false},
		{MethodGetExtendedAgentCard, http.MethodGet, "/extendedAgentCard", false, false},
	}
	if len(Routes()) != len(tests) {
		t.Fatalf("route table has %d entries, want %d", len(Routes()), len(tests))
	}
	for _, tc := range tests {
		t.Run(tc.operation, func(t *testing.T) {
			route, ok := RouteFor(tc.operation)
			if !ok {
				t.Fatalf("no route for %q", tc.operation)
			}
			if route.HTTPMethod != tc.httpMethod {
				t.Errorf("HTTP method = %q, want %q", route.HTTPMethod, tc.httpMethod)
			}
			if route.Path != tc.path {
				t.Errorf("path = %q, want %q", route.Path, tc.path)
			}
			if route.Streaming != tc.streaming {
				t.Errorf("streaming = %v, want %v", route.Streaming, tc.streaming)
			}
			if route.Supported != tc.supported {
				t.Errorf("supported = %v, want %v", route.Supported, tc.supported)
			}
		})
	}

	if got := len(SupportedRoutes()); got != 6 {
		t.Fatalf("SupportedRoutes() = %d entries, want the 6 operations this codec decodes", got)
	}
}

// TestMatchRoute exercises the hand-rolled matcher, in particular the custom
// verb suffixes that a stdlib ServeMux wildcard would swallow into the task id.
func TestMatchRoute(t *testing.T) {
	tests := []struct {
		name           string
		httpMethod     string
		path           string
		wantOperation  string
		wantVars       map[string]string
		wantFound      bool
		wantMethodFail bool
	}{
		{
			name: "send message", httpMethod: http.MethodPost, path: "/message:send",
			wantOperation: MethodSendMessage, wantVars: map[string]string{}, wantFound: true,
		},
		{
			name: "stream message", httpMethod: http.MethodPost, path: "/message:stream",
			wantOperation: MethodSendStreamingMessage, wantVars: map[string]string{}, wantFound: true,
		},
		{
			name: "get task", httpMethod: http.MethodGet, path: "/tasks/task-uuid",
			wantOperation: MethodGetTask, wantVars: map[string]string{"id": "task-uuid"}, wantFound: true,
		},
		{
			name: "list tasks", httpMethod: http.MethodGet, path: "/tasks",
			wantOperation: MethodListTasks, wantVars: map[string]string{}, wantFound: true,
		},
		{
			name: "list tasks tolerates a trailing slash", httpMethod: http.MethodGet, path: "/tasks/",
			wantOperation: MethodListTasks, wantVars: map[string]string{}, wantFound: true,
		},
		{
			name: "cancel task", httpMethod: http.MethodPost, path: "/tasks/task-uuid:cancel",
			wantOperation: MethodCancelTask, wantVars: map[string]string{"id": "task-uuid"}, wantFound: true,
		},
		{
			name: "subscribe to task", httpMethod: http.MethodPost, path: "/tasks/task-uuid:subscribe",
			wantOperation: MethodSubscribeToTask, wantVars: map[string]string{"id": "task-uuid"}, wantFound: true,
		},
		{
			name: "percent-encoded task id is decoded", httpMethod: http.MethodGet, path: "/tasks/a%2Fb",
			wantOperation: MethodGetTask, wantVars: map[string]string{"id": "a/b"}, wantFound: true,
		},
		{
			name: "push config list", httpMethod: http.MethodGet, path: "/tasks/t-1/pushNotificationConfigs",
			wantOperation: MethodListTaskPushNotificationConfigs, wantVars: map[string]string{"id": "t-1"}, wantFound: true,
		},
		{
			name: "push config get", httpMethod: http.MethodGet, path: "/tasks/t-1/pushNotificationConfigs/c-1",
			wantOperation: MethodGetTaskPushNotificationConfig,
			wantVars:      map[string]string{"id": "t-1", "configId": "c-1"}, wantFound: true,
		},
		{
			name: "extended agent card", httpMethod: http.MethodGet, path: "/extendedAgentCard",
			wantOperation: MethodGetExtendedAgentCard, wantVars: map[string]string{}, wantFound: true,
		},

		// Negative cases.
		{name: "unknown path", httpMethod: http.MethodGet, path: "/nope", wantFound: false},
		{name: "unknown custom verb", httpMethod: http.MethodPost, path: "/tasks/t-1:explode", wantFound: false},
		{name: "empty task id", httpMethod: http.MethodGet, path: "/tasks//", wantFound: false},
		{
			name: "known path with the wrong verb", httpMethod: http.MethodDelete, path: "/tasks/t-1",
			wantFound: false, wantMethodFail: true,
		},
		{
			name: "cancel over GET", httpMethod: http.MethodGet, path: "/tasks/t-1:cancel",
			wantFound: false, wantMethodFail: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route, vars, found, methodMismatch := MatchRoute(tc.httpMethod, tc.path)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (route %+v)", found, tc.wantFound, route)
			}
			if !tc.wantFound {
				if methodMismatch != tc.wantMethodFail {
					t.Fatalf("methodMismatch = %v, want %v", methodMismatch, tc.wantMethodFail)
				}
				return
			}
			if route.Operation != tc.wantOperation {
				t.Fatalf("operation = %q, want %q", route.Operation, tc.wantOperation)
			}
			if !reflect.DeepEqual(vars, tc.wantVars) {
				t.Fatalf("vars = %v, want %v", vars, tc.wantVars)
			}
		})
	}
}

// TestMatchRouteDoesNotSwallowVerbs is the regression guard for the reason the
// matcher is hand-rolled: "/tasks/{id}" must not match "/tasks/t-1:cancel".
func TestMatchRouteDoesNotSwallowVerbs(t *testing.T) {
	route, vars, found, _ := MatchRoute(http.MethodPost, "/tasks/t-1:cancel")
	if !found {
		t.Fatal("cancel route did not match")
	}
	if route.Operation != MethodCancelTask {
		t.Fatalf("operation = %q, want %q", route.Operation, MethodCancelTask)
	}
	if vars["id"] != "t-1" {
		t.Fatalf("id = %q, want %q; the custom verb must not be folded into the id", vars["id"], "t-1")
	}
}

// TestBuildPath covers rendering a route template back into a concrete path.
func TestBuildPath(t *testing.T) {
	tests := []struct {
		name     string
		template string
		vars     map[string]string
		want     string
		wantErr  bool
	}{
		{name: "cancel", template: PathCancelTask, vars: map[string]string{"id": "t-1"}, want: "/tasks/t-1:cancel"},
		{name: "get", template: PathGetTask, vars: map[string]string{"id": "t-1"}, want: "/tasks/t-1"},
		{name: "escaping", template: PathGetTask, vars: map[string]string{"id": "a/b"}, want: "/tasks/a%2Fb"},
		{name: "no placeholders", template: PathListTasks, vars: nil, want: "/tasks"},
		{name: "unbound placeholder", template: PathGetTask, vars: nil, wantErr: true},
		{name: "unknown placeholder", template: PathListTasks, vars: map[string]string{"id": "t-1"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildPath(tc.template, tc.vars)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildPath: %v", err)
			}
			if got != tc.want {
				t.Fatalf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildPathMatchRoundTrip proves the builder and matcher agree.
func TestBuildPathMatchRoundTrip(t *testing.T) {
	for _, route := range Routes() {
		vars := map[string]string{}
		if strings.Contains(route.Path, "{id}") {
			vars["id"] = "task-42"
		}
		if strings.Contains(route.Path, "{configId}") {
			vars["configId"] = "config-7"
		}
		path, err := BuildPath(route.Path, vars)
		if err != nil {
			t.Fatalf("%s: BuildPath: %v", route.Operation, err)
		}
		matched, gotVars, found, _ := MatchRoute(route.HTTPMethod, path)
		if !found {
			t.Fatalf("%s: built path %q does not match its own route", route.Operation, path)
		}
		if matched.Operation != route.Operation {
			t.Fatalf("%s: built path %q matched %q instead", route.Operation, path, matched.Operation)
		}
		if !reflect.DeepEqual(gotVars, vars) {
			t.Fatalf("%s: vars = %v, want %v", route.Operation, gotVars, vars)
		}
	}
}

// TestListTasksQueryRoundTrip covers the camelCase query parameter mapping from
// specification section 11.5.
func TestListTasksQueryRoundTrip(t *testing.T) {
	after := NewTimestamp(time.Date(2025, 11, 9, 10, 30, 0, 0, time.UTC))
	includeArtifacts := true
	original := &ListTasksRequest{
		Tenant:               "tenant-a",
		ContextID:            "context-uuid",
		Status:               TaskStateWorking,
		PageSize:             PageSize(50),
		PageToken:            "cursor-token",
		HistoryLength:        HistoryLength(10),
		StatusTimestampAfter: after,
		IncludeArtifacts:     &includeArtifacts,
	}

	q := original.Query()
	wantParams := map[string]string{
		"tenant":               "tenant-a",
		"contextId":            "context-uuid",
		"status":               "TASK_STATE_WORKING",
		"pageSize":             "50",
		"pageToken":            "cursor-token",
		"historyLength":        "10",
		"statusTimestampAfter": "2025-11-09T10:30:00.000Z",
		"includeArtifacts":     "true",
	}
	for name, want := range wantParams {
		if got := q.Get(name); got != want {
			t.Errorf("query %q = %q, want %q", name, got, want)
		}
	}
	if len(q) != len(wantParams) {
		t.Errorf("query has %d parameters, want %d: %v", len(q), len(wantParams), q)
	}

	parsed, err := ParseListTasksQuery(q)
	if err != nil {
		t.Fatalf("ParseListTasksQuery: %v", err)
	}
	if !reflect.DeepEqual(parsed, original) {
		t.Fatalf("round trip drifted:\nwant %+v\n got %+v", original, parsed)
	}
}

// TestParseListTasksQueryErrors covers malformed query parameters.
func TestParseListTasksQueryErrors(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "non-numeric page size", query: "pageSize=lots"},
		{name: "page size above the maximum", query: "pageSize=101"},
		{name: "page size below the minimum", query: "pageSize=0"},
		{name: "negative history length", query: "historyLength=-5"},
		{name: "non-numeric history length", query: "historyLength=all"},
		{name: "unknown status", query: "status=TASK_STATE_NOPE"},
		{name: "non-boolean includeArtifacts", query: "includeArtifacts=maybe"},
		{name: "malformed timestamp", query: "statusTimestampAfter=yesterday"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			req, protoErr := ParseListTasksQuery(values)
			if protoErr == nil {
				t.Fatalf("expected an error, got %+v", req)
			}
			if protoErr.Type != ErrorTypeInvalidParams {
				t.Fatalf("error type = %q, want %q", protoErr.Type, ErrorTypeInvalidParams)
			}
		})
	}
}

// TestParseListTasksQueryDefaults checks that an empty query yields an
// unfiltered request rather than an error.
func TestParseListTasksQueryDefaults(t *testing.T) {
	req, err := ParseListTasksQuery(url.Values{})
	if err != nil {
		t.Fatalf("ParseListTasksQuery: %v", err)
	}
	if req.PageSize != nil || req.HistoryLength != nil || req.Status != "" || req.IncludeArtifacts != nil {
		t.Fatalf("expected an entirely unset request, got %+v", req)
	}
}

// TestGetTaskQueryRoundTrip covers GET /tasks/{id}?historyLength=N.
func TestGetTaskQueryRoundTrip(t *testing.T) {
	original := &GetTaskRequest{ID: "task-uuid", Tenant: "tenant-a", HistoryLength: HistoryLength(10)}
	q := original.Query()
	if q.Get("historyLength") != "10" || q.Get("tenant") != "tenant-a" {
		t.Fatalf("query = %v", q)
	}

	parsed, err := ParseGetTaskQuery("task-uuid", q)
	if err != nil {
		t.Fatalf("ParseGetTaskQuery: %v", err)
	}
	if !reflect.DeepEqual(parsed, original) {
		t.Fatalf("round trip drifted:\nwant %+v\n got %+v", original, parsed)
	}

	if _, err := ParseGetTaskQuery("", nil); err == nil {
		t.Fatal("expected an error for a missing task id")
	}
	bad := url.Values{"historyLength": []string{"-1"}}
	if _, err := ParseGetTaskQuery("t-1", bad); err == nil {
		t.Fatal("expected an error for a negative history length")
	}
}

// TestRESTErrorBody pins the google.rpc.Status shape from specification section
// 11.6, including the ErrorInfo that disambiguates A2A errors sharing an HTTP
// status.
func TestRESTErrorBody(t *testing.T) {
	err := ErrTaskNotFound("task-123")
	status, body := err.RESTError()

	if status != http.StatusNotFound {
		t.Fatalf("HTTP status = %d, want 404", status)
	}
	if body.Error.Code != http.StatusNotFound {
		t.Errorf("body code = %d, want 404", body.Error.Code)
	}
	if body.Error.Status != "NOT_FOUND" {
		t.Errorf("body status = %q, want NOT_FOUND", body.Error.Status)
	}
	if len(body.Error.Details) != 1 {
		t.Fatalf("details = %v, want one ErrorInfo", body.Error.Details)
	}
	if body.Error.Details[0]["reason"] != "TASK_NOT_FOUND" {
		t.Errorf("reason = %v", body.Error.Details[0]["reason"])
	}

	encoded, encodeErr := Encode(body)
	if encodeErr != nil {
		t.Fatalf("encode: %v", encodeErr)
	}
	var check struct {
		Error struct {
			Code    int              `json:"code"`
			Status  string           `json:"status"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(encoded, &check); unmarshalErr != nil {
		t.Fatalf("decode: %v", unmarshalErr)
	}
	if check.Error.Details[0]["@type"] != TypeErrorInfo {
		t.Errorf("@type = %v", check.Error.Details[0]["@type"])
	}
	if check.Error.Details[0]["domain"] != ErrorDomain {
		t.Errorf("domain = %v", check.Error.Details[0]["domain"])
	}
}

// TestTwoErrorsSharingAnHTTPStatusAreDistinguishable is the reason the ErrorInfo
// detail is mandatory: both of these are 400 Bad Request.
func TestTwoErrorsSharingAnHTTPStatusAreDistinguishable(t *testing.T) {
	notCancelable := ErrTaskNotCancelable("t-1", TaskStateCompleted)
	noPush := NewError(ErrorTypePushNotificationNotSupported, "")

	statusA, bodyA := notCancelable.RESTError()
	statusB, bodyB := noPush.RESTError()
	if statusA != statusB || statusA != http.StatusBadRequest {
		t.Fatalf("statuses = %d/%d, both should be 400", statusA, statusB)
	}
	if bodyA.Error.Details[0]["reason"] == bodyB.Error.Details[0]["reason"] {
		t.Fatalf("both errors report reason %v; the ErrorInfo must disambiguate them",
			bodyA.Error.Details[0]["reason"])
	}
}

// TestContentTypes pins the media types from specification sections 11.1 and
// 11.7.
func TestContentTypes(t *testing.T) {
	if ContentTypeJSON != "application/a2a+json" {
		t.Errorf("ContentTypeJSON = %q", ContentTypeJSON)
	}
	if ContentTypeSSE != "text/event-stream" {
		t.Errorf("ContentTypeSSE = %q", ContentTypeSSE)
	}
	if AgentCardPath != "/.well-known/agent-card.json" {
		t.Errorf("AgentCardPath = %q", AgentCardPath)
	}
}

// TestRESTBodyDecoders covers the REST request bodies for the operations whose
// task id lives in the path.
func TestRESTBodyDecoders(t *testing.T) {
	t.Run("send message body", func(t *testing.T) {
		body := `{"message":{"messageId":"msg-uuid","role":"ROLE_USER","parts":[{"text":"Hello"}]},` +
			`"configuration":{"acceptedOutputModes":["text/plain"]}}`
		req, err := DecodeSendMessageRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeSendMessageRequest: %v", err)
		}
		if req.Message.MessageID != "msg-uuid" {
			t.Fatalf("message = %+v", req.Message)
		}
	})
	t.Run("send message body is validated", func(t *testing.T) {
		if _, err := DecodeSendMessageRequest([]byte(`{"message":{"role":"ROLE_USER","parts":[]}}`)); err == nil {
			t.Fatal("expected a validation error")
		}
	})
	t.Run("send message body must be JSON", func(t *testing.T) {
		_, err := DecodeSendMessageRequest([]byte(`{"message":`))
		if err == nil || err.Type != ErrorTypeJSONParse {
			t.Fatalf("err = %v, want a JSONParseError", err)
		}
	})
	t.Run("cancel with an empty body", func(t *testing.T) {
		req, err := DecodeCancelTaskRequest("task-uuid", nil)
		if err != nil {
			t.Fatalf("DecodeCancelTaskRequest: %v", err)
		}
		if req.ID != "task-uuid" {
			t.Fatalf("id = %q", req.ID)
		}
	})
	t.Run("path id wins over the body id", func(t *testing.T) {
		req, err := DecodeCancelTaskRequest("path-id", []byte(`{"id":"body-id","metadata":{"why":"user asked"}}`))
		if err != nil {
			t.Fatalf("DecodeCancelTaskRequest: %v", err)
		}
		if req.ID != "path-id" {
			t.Fatalf("id = %q, want the path id to win", req.ID)
		}
		if req.Metadata["why"] != "user asked" {
			t.Fatalf("metadata = %v", req.Metadata)
		}
	})
	t.Run("cancel requires a task id", func(t *testing.T) {
		if _, err := DecodeCancelTaskRequest("", nil); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("subscribe with an empty body", func(t *testing.T) {
		req, err := DecodeSubscribeToTaskRequest("task-uuid", nil)
		if err != nil {
			t.Fatalf("DecodeSubscribeToTaskRequest: %v", err)
		}
		if req.ID != "task-uuid" {
			t.Fatalf("id = %q", req.ID)
		}
	})
	t.Run("subscribe requires a task id", func(t *testing.T) {
		if _, err := DecodeSubscribeToTaskRequest("", nil); err == nil {
			t.Fatal("expected an error")
		}
	})
}
