// Package host implements the wasm host module that backs the nexus_sdk/*
// snippet API. A single dispatch function (`invoke`) takes an opcode + JSON
// request blob from the snippet, runs the gated operation, writes a JSON
// response back into the snippet's scratch buffer, and returns a packed
// (status<<32 | length) result. Capability gates fire here, before any real
// I/O hits the host kernel.
package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// HostModuleName is the wazero host module name imported by the runner via
// //go:wasmimport directives. Bumping the suffix cuts old runners off; v1
// runners must speak v1 host functions.
const HostModuleName = "nexus_bridge_v1"

// statusCode values mirror those returned to the snippet via the high bits
// of the bridge return value.
const (
	StatusOK            uint16 = 0
	StatusInvalidOp     uint16 = 1
	StatusBadRequest    uint16 = 2
	StatusCapDenied     uint16 = 3
	StatusInternal      uint16 = 4
	StatusBufferTooSmall uint16 = 5
)

// Bridge holds the gated dependencies the host function needs to satisfy
// snippet calls. One per WasmBackend.
type Bridge struct {
	policy Policy
	caps   Capabilities
}

// Policy carries the configured-at-init permissions for a sandbox session.
type Policy struct {
	NetAllowHosts []string
	FSMounts      []FSMount
	ExecAllowed   []string
	EnvVars       map[string]string
}

// FSMount maps a host directory tree onto a guest path with rw or ro
// access. Symlink-following is disabled host-side for safety; see the fs op.
type FSMount struct {
	Host  string
	Guest string
	Mode  string // "ro" or "rw"
}

// Capabilities is the runtime adapter the host function uses to perform
// gated I/O. Injected so tests can fake HTTP / fs / exec without standing
// up real kernels.
type Capabilities interface {
	HTTPGet(ctx context.Context, url string) (status int, body []byte, headers map[string][]string, err error)
	FSReadFile(ctx context.Context, guestPath string) ([]byte, error)
	FSWriteFile(ctx context.Context, guestPath string, data []byte) error
	ExecRun(ctx context.Context, name string, args []string) (stdout, stderr []byte, exit int, err error)
}

// NewBridge constructs a Bridge with the given policy and capabilities
// adapter. Capabilities may be nil — operations that require it will return
// StatusInvalidOp.
func NewBridge(policy Policy, caps Capabilities) *Bridge {
	return &Bridge{policy: policy, caps: caps}
}

// Register installs the bridge host functions on the supplied wazero
// runtime. Call once per Runtime; reuse across many module instantiations.
func (b *Bridge) Register(ctx context.Context, rt wazero.Runtime) error {
	mod := rt.NewHostModuleBuilder(HostModuleName)
	mod.NewFunctionBuilder().
		WithFunc(b.invoke).
		Export("invoke")
	if _, err := mod.Instantiate(ctx); err != nil {
		return fmt.Errorf("host bridge: register: %w", err)
	}
	return nil
}

// invoke is the single bridge function exported to wasm. Signature:
//
//	invoke(opPtr i32, opLen i32, reqPtr i32, reqLen i32,
//	       scratchPtr i32, scratchLen i32) -> i64
//
// where the i64 return packs (status uint16 << 48) | (length uint32). A
// non-zero status maps to a snippet-side error; status 0 means the response
// JSON is in scratch[0:length].
func (b *Bridge) invoke(
	ctx context.Context,
	mod api.Module,
	opPtr, opLen, reqPtr, reqLen, scratchPtr, scratchLen uint32,
) uint64 {
	mem := mod.Memory()
	op, ok := mem.Read(opPtr, opLen)
	if !ok {
		return pack(StatusBadRequest, 0)
	}
	req, ok := mem.Read(reqPtr, reqLen)
	if !ok {
		return pack(StatusBadRequest, 0)
	}

	respBytes, status := b.dispatch(ctx, string(op), req)
	if status != StatusOK {
		// Treat respBytes as a UTF-8 error message when status != OK.
		return writeOrPack(mem, scratchPtr, scratchLen, respBytes, status)
	}
	return writeOrPack(mem, scratchPtr, scratchLen, respBytes, StatusOK)
}

func writeOrPack(mem api.Memory, scratchPtr, scratchLen uint32, body []byte, status uint16) uint64 {
	if uint32(len(body)) > scratchLen {
		// Tell the snippet how much space it would have needed. Snippet
		// retries with a bigger buffer.
		return pack(StatusBufferTooSmall, uint32(len(body)))
	}
	if !mem.Write(scratchPtr, body) {
		return pack(StatusInternal, 0)
	}
	return pack(status, uint32(len(body)))
}

func pack(status uint16, length uint32) uint64 {
	return (uint64(status) << 48) | uint64(length)
}

// dispatch routes the opcode to its handler. Returns response bytes (JSON
// for OK, plain UTF-8 for non-OK) and the status code.
func (b *Bridge) dispatch(ctx context.Context, op string, req []byte) ([]byte, uint16) {
	switch op {
	case "http.get":
		return b.handleHTTPGet(ctx, req)
	case "fs.read":
		return b.handleFSRead(ctx, req)
	case "fs.write":
		return b.handleFSWrite(ctx, req)
	case "exec.run":
		return b.handleExecRun(ctx, req)
	case "env.get":
		return b.handleEnvGet(req)
	default:
		return []byte("unknown op: " + op), StatusInvalidOp
	}
}

// -- handlers ---------------------------------------------------------------

func (b *Bridge) handleHTTPGet(ctx context.Context, req []byte) ([]byte, uint16) {
	var r struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return []byte("parse: " + err.Error()), StatusBadRequest
	}
	if !b.netHostAllowed(r.URL) {
		return []byte("net.allow_hosts denied: " + r.URL), StatusCapDenied
	}
	if b.caps == nil {
		return []byte("http capability not configured"), StatusInternal
	}
	status, body, headers, err := b.caps.HTTPGet(ctx, r.URL)
	if err != nil {
		return []byte("http: " + err.Error()), StatusInternal
	}
	out, _ := json.Marshal(map[string]any{
		"status":  status,
		"body":    body,
		"headers": headers,
	})
	return out, StatusOK
}

func (b *Bridge) handleFSRead(ctx context.Context, req []byte) ([]byte, uint16) {
	var r struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return []byte("parse: " + err.Error()), StatusBadRequest
	}
	if !b.fsReadAllowed(r.Path) {
		return []byte("fs.mounts denied: " + r.Path), StatusCapDenied
	}
	if b.caps == nil {
		return []byte("fs capability not configured"), StatusInternal
	}
	data, err := b.caps.FSReadFile(ctx, r.Path)
	if err != nil {
		return []byte("fs: " + err.Error()), StatusInternal
	}
	out, _ := json.Marshal(map[string]any{"data": data})
	return out, StatusOK
}

func (b *Bridge) handleFSWrite(ctx context.Context, req []byte) ([]byte, uint16) {
	var r struct {
		Path string `json:"path"`
		Data []byte `json:"data"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return []byte("parse: " + err.Error()), StatusBadRequest
	}
	if !b.fsWriteAllowed(r.Path) {
		return []byte("fs.mounts denied (read-only or unmounted): " + r.Path), StatusCapDenied
	}
	if b.caps == nil {
		return []byte("fs capability not configured"), StatusInternal
	}
	if err := b.caps.FSWriteFile(ctx, r.Path, r.Data); err != nil {
		return []byte("fs: " + err.Error()), StatusInternal
	}
	return []byte("{}"), StatusOK
}

func (b *Bridge) handleExecRun(ctx context.Context, req []byte) ([]byte, uint16) {
	var r struct {
		Name string   `json:"name"`
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return []byte("parse: " + err.Error()), StatusBadRequest
	}
	if !b.execAllowed(r.Name) {
		return []byte("exec.allowed denied: " + r.Name), StatusCapDenied
	}
	if b.caps == nil {
		return []byte("exec capability not configured"), StatusInternal
	}
	stdout, stderr, exit, err := b.caps.ExecRun(ctx, r.Name, r.Args)
	if err != nil {
		return []byte("exec: " + err.Error()), StatusInternal
	}
	out, _ := json.Marshal(map[string]any{
		"stdout": stdout,
		"stderr": stderr,
		"exit":   exit,
	})
	return out, StatusOK
}

func (b *Bridge) handleEnvGet(req []byte) ([]byte, uint16) {
	var r struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return []byte("parse: " + err.Error()), StatusBadRequest
	}
	val := b.policy.EnvVars[r.Key]
	out, _ := json.Marshal(map[string]any{"value": val})
	return out, StatusOK
}
