//go:build wasip1

package main

import (
	"encoding/json"
	"errors"
)

// HTTPResponse mirrors a subset of net/http.Response shape that survives the
// JSON envelope. Body is bytes, not an io.Reader, so the SDK ABI stays flat.
type HTTPResponse struct {
	Status  int                 `json:"status"`
	Body    []byte              `json:"body"`
	Headers map[string][]string `json:"headers"`
}

// HTTPGet performs a gated HTTP GET. Capability denied errors are reported
// via ErrCapDenied; other errors are surfaced as plain errors.
func HTTPGet(url string) (*HTTPResponse, error) {
	resp, err := bridgeCall("http.get", map[string]any{"url": url})
	if err != nil {
		return nil, err
	}
	var out HTTPResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrCapDenied is the sentinel for "the sandbox refused to perform the
// operation because the configured capability gate denied it". Snippet code
// uses errors.Is(err, ErrCapDenied) to distinguish denial from runtime
// failure.
var ErrCapDenied = errCapDenied

// IsCapDenied is a helper for snippet code that wants the same answer
// without importing errors directly.
func IsCapDenied(err error) bool {
	return errors.Is(err, ErrCapDenied)
}
