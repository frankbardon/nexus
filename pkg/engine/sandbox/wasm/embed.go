package wasm

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
)

// runnerWasmGz is the gzip-compressed bytes of cmd/yaegi-runner built for
// GOOS=wasip1 GOARCH=wasm. Refresh via `make build-yaegi-wasm`. CI verifies
// reproducibility against a pinned Go toolchain.
//
//go:embed yaegi.wasm.gz
var runnerWasmGz []byte

// runnerBytes decompresses the embedded runner artefact. Cheap (~50 ms one
// shot); called once per WasmBackend.
func runnerBytes() ([]byte, error) {
	if len(runnerWasmGz) == 0 {
		return nil, fmt.Errorf("wasm: embedded yaegi runner is empty (run `make build-yaegi-wasm`)")
	}
	gr, err := gzip.NewReader(bytes.NewReader(runnerWasmGz))
	if err != nil {
		return nil, fmt.Errorf("wasm: gunzip runner: %w", err)
	}
	defer gr.Close()
	out, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("wasm: read runner: %w", err)
	}
	return out, nil
}
