//go:build wasip1

package main

import (
	"encoding/json"
)

// FSReadFile reads a file from a guest path that maps to a configured
// mount. Returns ErrCapDenied when the path falls outside any mount.
func FSReadFile(path string) ([]byte, error) {
	resp, err := bridgeCall("fs.read", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []byte `json:"data"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// FSWriteFile writes data to a guest path. The mount must be configured
// with mode rw or the call returns ErrCapDenied.
func FSWriteFile(path string, data []byte) error {
	_, err := bridgeCall("fs.write", map[string]any{
		"path": path,
		"data": data,
	})
	return err
}
