//go:build wasip1

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"
)

// hostModuleName must match host.HostModuleName exactly. Bumped together
// when the bridge ABI changes.
const hostModuleName = "nexus_bridge_v1"

// scratchBuf is the per-call response buffer. Single-threaded snippet
// execution lets us reuse a fixed buffer; large responses can grow it via
// retryWithBigger.
var scratchBuf = make([]byte, 64*1024)

// status codes match host.Bridge constants.
const (
	statusOK             uint16 = 0
	statusInvalidOp      uint16 = 1
	statusBadRequest     uint16 = 2
	statusCapDenied      uint16 = 3
	statusInternal       uint16 = 4
	statusBufferTooSmall uint16 = 5
)

// invoke is the single wasmimport into the host bridge. Returns a packed
// (status<<48 | length) result. When status == statusBufferTooSmall, length
// is the size the response would have taken — the caller grows scratch and
// retries.
//
//go:wasmimport nexus_bridge_v1 invoke
//go:noescape
func invoke(opPtr, opLen, reqPtr, reqLen, scratchPtr, scratchLen uint32) uint64

// bridgeCall sends opcode + JSON request to the host, returns response bytes
// (JSON when the call succeeded, plain UTF-8 error message on the negative
// path). errCapDenied is surfaced as a typed error so SDK callers can
// distinguish "you didn't have permission" from "the operation itself
// failed" or "the bridge is misconfigured".
func bridgeCall(op string, req any) ([]byte, error) {
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("nexus_sdk: marshal request: %w", err)
	}
	opBytes := []byte(op)

	for {
		ret := invoke(
			ptr(opBytes), uint32(len(opBytes)),
			ptr(reqBytes), uint32(len(reqBytes)),
			ptr(scratchBuf), uint32(len(scratchBuf)),
		)
		status := uint16(ret >> 48)
		length := uint32(ret)

		switch status {
		case statusOK:
			out := make([]byte, length)
			copy(out, scratchBuf[:length])
			return out, nil
		case statusBufferTooSmall:
			scratchBuf = make([]byte, int(length)+1024)
			continue
		case statusCapDenied:
			return nil, fmt.Errorf("%w: %s", errCapDenied, string(scratchBuf[:length]))
		case statusInvalidOp:
			return nil, fmt.Errorf("nexus_sdk: invalid op %q: %s", op, string(scratchBuf[:length]))
		case statusBadRequest:
			return nil, fmt.Errorf("nexus_sdk: bad request: %s", string(scratchBuf[:length]))
		case statusInternal:
			return nil, fmt.Errorf("nexus_sdk: bridge: %s", string(scratchBuf[:length]))
		default:
			return nil, fmt.Errorf("nexus_sdk: unknown status %d", status)
		}
	}
}

// errCapDenied is the sentinel returned to snippets when a capability gate
// rejects the call. Use errors.Is(err, ErrCapDenied) in snippet code.
var errCapDenied = errors.New("capability denied")

// ptr returns the start address of b as a uint32 suitable for a wasmimport
// argument. b must remain alive for the duration of the call (Go's GC may
// not move heap objects but the pointer-as-int cast loses the tracker — use
// this only for arguments that are clearly live across the call).
func ptr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}
