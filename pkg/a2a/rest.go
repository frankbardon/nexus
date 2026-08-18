package a2a

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Content types for the HTTP+JSON/REST binding (specification section 11.1).
const (
	// ContentTypeJSON is the media type A2A prefers for REST requests and
	// responses.
	ContentTypeJSON = "application/a2a+json"
	// ContentTypeSSE is the media type of a streaming response body.
	ContentTypeSSE = "text/event-stream"
)

// REST path templates (specification section 11.3). The {id} placeholder is the
// task id; note that the cancel and subscribe operations are custom verbs
// appended to the resource path with a colon, so a task id itself may not
// contain a colon.
const (
	PathSendMessage   = "/message:send"
	PathStreamMessage = "/message:stream"
	PathGetTask       = "/tasks/{id}"
	PathListTasks     = "/tasks"
	PathCancelTask    = "/tasks/{id}:cancel"
	PathSubscribeTask = "/tasks/{id}:subscribe"

	PathCreatePushNotificationConfig = "/tasks/{id}/pushNotificationConfigs"
	PathGetPushNotificationConfig    = "/tasks/{id}/pushNotificationConfigs/{configId}"
	PathListPushNotificationConfigs  = "/tasks/{id}/pushNotificationConfigs"
	PathDeletePushNotificationConfig = "/tasks/{id}/pushNotificationConfigs/{configId}"

	PathExtendedAgentCard = "/extendedAgentCard"
)

// AgentCardPath is the well-known location of an agent's public Agent Card
// (specification section 8.2). The card types themselves are defined elsewhere.
const AgentCardPath = "/.well-known/agent-card.json"

// Route binds one A2A operation to its REST shape.
type Route struct {
	// Operation is the A2A operation name, identical to the JSON-RPC method.
	Operation string
	// HTTPMethod is the HTTP verb.
	HTTPMethod string
	// Path is the path template, with {id} and {configId} placeholders.
	Path string
	// Streaming reports whether the response is an SSE stream rather than a
	// single JSON body.
	Streaming bool
	// Supported reports whether this codec decodes the operation. Unsupported
	// routes are declared so that a server can answer them with the correct
	// protocol error instead of a bare 404.
	Supported bool
}

// routes is the full REST route table from specification section 11.3, in the
// order of the section 5.3 mapping table.
var routes = []Route{
	{Operation: MethodSendMessage, HTTPMethod: http.MethodPost, Path: PathSendMessage, Supported: true},
	{Operation: MethodSendStreamingMessage, HTTPMethod: http.MethodPost, Path: PathStreamMessage, Streaming: true, Supported: true},
	{Operation: MethodGetTask, HTTPMethod: http.MethodGet, Path: PathGetTask, Supported: true},
	{Operation: MethodListTasks, HTTPMethod: http.MethodGet, Path: PathListTasks, Supported: true},
	{Operation: MethodCancelTask, HTTPMethod: http.MethodPost, Path: PathCancelTask, Supported: true},
	{Operation: MethodSubscribeToTask, HTTPMethod: http.MethodPost, Path: PathSubscribeTask, Streaming: true, Supported: true},

	{Operation: MethodCreateTaskPushNotificationConfig, HTTPMethod: http.MethodPost, Path: PathCreatePushNotificationConfig},
	{Operation: MethodGetTaskPushNotificationConfig, HTTPMethod: http.MethodGet, Path: PathGetPushNotificationConfig},
	{Operation: MethodListTaskPushNotificationConfigs, HTTPMethod: http.MethodGet, Path: PathListPushNotificationConfigs},
	{Operation: MethodDeleteTaskPushNotificationConfig, HTTPMethod: http.MethodDelete, Path: PathDeletePushNotificationConfig},

	{Operation: MethodGetExtendedAgentCard, HTTPMethod: http.MethodGet, Path: PathExtendedAgentCard},
}

// Routes returns the full REST route table. The returned slice is a fresh copy.
func Routes() []Route {
	out := make([]Route, len(routes))
	copy(out, routes)
	return out
}

// SupportedRoutes returns only the routes this codec decodes.
func SupportedRoutes() []Route {
	var out []Route
	for _, r := range routes {
		if r.Supported {
			out = append(out, r)
		}
	}
	return out
}

// RouteFor returns the REST route for an A2A operation.
func RouteFor(operation string) (Route, bool) {
	for _, r := range routes {
		if r.Operation == operation {
			return r, true
		}
	}
	return Route{}, false
}

// MatchRoute resolves an HTTP method and request path to an A2A route,
// extracting path placeholders into vars.
//
// Matching is hand-rolled rather than delegated to http.ServeMux because A2A's
// custom verbs live in the last path segment after a colon: "/tasks/abc:cancel"
// and "/tasks/abc" differ only in that suffix, and a ServeMux "{id}" wildcard
// would swallow the verb into the id.
//
// It reports found=false when nothing matches the path at all, and
// methodMismatch=true when the path matches a known route but the HTTP verb does
// not, so a caller can answer 405 rather than 404.
func MatchRoute(httpMethod, path string) (route Route, vars map[string]string, found bool, methodMismatch bool) {
	path = normalizePath(path)
	pathMatched := false
	for _, r := range routes {
		v, ok := matchPath(r.Path, path)
		if !ok {
			continue
		}
		pathMatched = true
		if r.HTTPMethod != httpMethod {
			continue
		}
		return r, v, true, false
	}
	return Route{}, nil, false, pathMatched
}

// normalizePath trims a trailing slash from anything longer than the root so
// that "/tasks/" and "/tasks" resolve identically.
func normalizePath(path string) string {
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" {
		return "/"
	}
	return path
}

// matchPath matches a request path against a route template, returning the
// bound placeholder values.
func matchPath(template, path string) (map[string]string, bool) {
	tSegs := strings.Split(strings.TrimPrefix(template, "/"), "/")
	pSegs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(tSegs) != len(pSegs) {
		return nil, false
	}

	vars := map[string]string{}
	for i, tSeg := range tSegs {
		pSeg := pSegs[i]

		// A segment may be a bare placeholder ("{id}") or a placeholder with a
		// custom-verb suffix ("{id}:cancel"). Split the verb off both sides
		// before comparing.
		tName, tVerb, tHasVerb := strings.Cut(tSeg, ":")
		if !strings.HasPrefix(tName, "{") || !strings.HasSuffix(tName, "}") {
			// Literal segment: must match exactly, verb included.
			if tSeg != pSeg {
				return nil, false
			}
			continue
		}

		value := pSeg
		if tHasVerb {
			base, verb, hasVerb := strings.Cut(pSeg, ":")
			if !hasVerb || verb != tVerb {
				return nil, false
			}
			value = base
		} else if strings.Contains(pSeg, ":") {
			// A bare placeholder must not swallow a custom verb.
			return nil, false
		}
		if value == "" {
			return nil, false
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return nil, false
		}
		vars[strings.Trim(tName, "{}")] = decoded
	}
	return vars, true
}

// BuildPath renders a route template with the supplied placeholder values,
// URL-escaping each one.
func BuildPath(template string, vars map[string]string) (string, error) {
	out := template
	for name, value := range vars {
		placeholder := "{" + name + "}"
		if !strings.Contains(out, placeholder) {
			return "", fmt.Errorf("a2a: path template %q has no placeholder %s", template, placeholder)
		}
		out = strings.ReplaceAll(out, placeholder, url.PathEscape(value))
	}
	if i := strings.Index(out, "{"); i >= 0 {
		return "", fmt.Errorf("a2a: path template %q has unbound placeholder at offset %d", template, i)
	}
	return out, nil
}

// TaskPath renders a task-scoped route template for a specific task id.
func TaskPath(template, taskID string) (string, error) {
	return BuildPath(template, map[string]string{"id": taskID})
}

// ---- Query parameter mapping (specification section 11.5) ----
//
// GET and DELETE carry their request parameters as path and query parameters,
// named in camelCase to match the JSON body serialization.

// Query renders a GetTaskRequest's non-path parameters as query parameters.
func (r *GetTaskRequest) Query() url.Values {
	q := url.Values{}
	if r.Tenant != "" {
		q.Set("tenant", r.Tenant)
	}
	if r.HistoryLength != nil {
		q.Set("historyLength", strconv.Itoa(*r.HistoryLength))
	}
	return q
}

// ParseGetTaskQuery builds a GetTaskRequest from a path task id and the request
// query parameters.
func ParseGetTaskQuery(taskID string, q url.Values) (*GetTaskRequest, *Error) {
	if taskID == "" {
		return nil, ErrInvalidParams(FieldViolation{Field: "id", Description: "task id is required"})
	}
	r := &GetTaskRequest{ID: taskID, Tenant: q.Get("tenant")}
	n, err := parseIntParam(q, "historyLength")
	if err != nil {
		return nil, err
	}
	r.HistoryLength = n
	if err := validateHistoryLength(r.HistoryLength, "historyLength"); err != nil {
		return nil, err
	}
	return r, nil
}

// Query renders a ListTasksRequest as query parameters.
func (r *ListTasksRequest) Query() url.Values {
	q := url.Values{}
	if r.Tenant != "" {
		q.Set("tenant", r.Tenant)
	}
	if r.ContextID != "" {
		q.Set("contextId", r.ContextID)
	}
	if r.Status != "" {
		q.Set("status", string(r.Status))
	}
	if r.PageSize != nil {
		q.Set("pageSize", strconv.Itoa(*r.PageSize))
	}
	if r.PageToken != "" {
		q.Set("pageToken", r.PageToken)
	}
	if r.HistoryLength != nil {
		q.Set("historyLength", strconv.Itoa(*r.HistoryLength))
	}
	if r.StatusTimestampAfter != nil {
		q.Set("statusTimestampAfter", r.StatusTimestampAfter.UTC().Format(timestampLayout))
	}
	if r.IncludeArtifacts != nil {
		q.Set("includeArtifacts", strconv.FormatBool(*r.IncludeArtifacts))
	}
	return q
}

// ParseListTasksQuery builds a ListTasksRequest from request query parameters.
func ParseListTasksQuery(q url.Values) (*ListTasksRequest, *Error) {
	r := &ListTasksRequest{
		Tenant:    q.Get("tenant"),
		ContextID: q.Get("contextId"),
		Status:    TaskState(q.Get("status")),
		PageToken: q.Get("pageToken"),
	}

	pageSize, err := parseIntParam(q, "pageSize")
	if err != nil {
		return nil, err
	}
	r.PageSize = pageSize

	historyLength, err := parseIntParam(q, "historyLength")
	if err != nil {
		return nil, err
	}
	r.HistoryLength = historyLength

	if raw := q.Get("includeArtifacts"); raw != "" {
		b, convErr := strconv.ParseBool(raw)
		if convErr != nil {
			return nil, ErrInvalidParams(FieldViolation{
				Field:       "includeArtifacts",
				Description: fmt.Sprintf("must be true or false, got %q", raw),
			})
		}
		r.IncludeArtifacts = &b
	}

	if raw := q.Get("statusTimestampAfter"); raw != "" {
		var ts Timestamp
		if convErr := ts.UnmarshalJSON([]byte(strconv.Quote(raw))); convErr != nil {
			return nil, ErrInvalidParams(FieldViolation{
				Field:       "statusTimestampAfter",
				Description: fmt.Sprintf("must be an ISO 8601 timestamp, got %q", raw),
			})
		}
		r.StatusTimestampAfter = &ts
	}

	if err := ValidateListTasksRequest(r); err != nil {
		return nil, err
	}
	return r, nil
}

// parseIntParam reads an optional integer query parameter.
func parseIntParam(q url.Values, name string) (*int, *Error) {
	raw := q.Get(name)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, ErrInvalidParams(FieldViolation{
			Field:       name,
			Description: fmt.Sprintf("must be an integer, got %q", raw),
		})
	}
	return &n, nil
}

// ---- REST error rendering (specification section 11.6) ----

// RESTError is the HTTP error body: the google.rpc.Status JSON representation
// wrapped in an "error" envelope.
type RESTError struct {
	Error RESTErrorStatus `json:"error"`
}

// RESTErrorStatus is the google.rpc.Status body of a REST error response.
type RESTErrorStatus struct {
	// Code is the HTTP status code.
	Code int `json:"code"`
	// Status is the canonical gRPC status name, e.g. "NOT_FOUND".
	Status string `json:"status"`
	// Message is the human-readable description.
	Message string `json:"message"`
	// Details carries the structured detail objects, including the ErrorInfo
	// that disambiguates A2A error types sharing one HTTP status.
	Details []ErrorDetail `json:"details,omitempty"`
}

// RESTError renders the A2A error as a REST error body plus its HTTP status.
func (e *Error) RESTError() (int, RESTError) {
	status := e.HTTPStatus()
	return status, RESTError{Error: RESTErrorStatus{
		Code:    status,
		Status:  e.GRPCStatus(),
		Message: e.Message,
		Details: e.details(),
	}}
}
