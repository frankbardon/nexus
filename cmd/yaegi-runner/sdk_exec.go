//go:build wasip1

package main

import (
	"encoding/json"
)

// ExecResult mirrors os/exec output: separate stdout/stderr buffers plus
// exit code. Errors here represent host-level failures (missing binary,
// timeout, capability denied); a non-zero Exit is not an error.
type ExecResult struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Exit   int    `json:"exit"`
}

// ExecRun invokes a sandbox-allowlisted command with args and waits for
// completion. Returns ErrCapDenied if name is not on the configured allow
// list.
func ExecRun(name string, args []string) (*ExecResult, error) {
	resp, err := bridgeCall("exec.run", map[string]any{
		"name": name,
		"args": args,
	})
	if err != nil {
		return nil, err
	}
	var out ExecResult
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
