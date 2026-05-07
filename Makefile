.PHONY: build run clean test fmt vet lint docs docs-serve docs-clean build-yaegi-wasm verify-yaegi-wasm check-events

BINARY_NAME=nexus
BUILD_DIR=bin
GO=go
YAEGI_WASM=pkg/engine/sandbox/wasm/yaegi.wasm.gz

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/nexus

run: build
	$(BUILD_DIR)/$(BINARY_NAME) -config configs/default.yaml

clean:
	rm -rf $(BUILD_DIR)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint: vet check-events
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

# Static check: every pkg/events/ struct mutation must bump its
# <Name>Version constant. Compares the working tree against
# $$CHECK_EVENTS_BASE (default HEAD~1). See scripts/check-event-versions.sh
# for usage and docs/src/architecture/events.md for the rule itself.
check-events:
	@scripts/check-event-versions.sh

docs:
	mdbook build docs

docs-serve:
	mdbook serve docs --open

docs-clean:
	rm -rf docs/book

# Build the embedded Yaegi-on-Wasm runner. Output: pkg/engine/sandbox/wasm/yaegi.wasm.gz
# Pinned to the host's Go toolchain. CI verifies reproducibility — see
# verify-yaegi-wasm — so a drift between checked-in artefact and a fresh
# build fails the build.
build-yaegi-wasm:
	@echo "Building cmd/yaegi-runner for GOOS=wasip1 GOARCH=wasm..."
	GOOS=wasip1 GOARCH=wasm $(GO) build -ldflags="-s -w" -trimpath -o $(YAEGI_WASM:.gz=) ./cmd/yaegi-runner
	@echo "Compressing with gzip -9..."
	gzip -9 -n -f $(YAEGI_WASM:.gz=)
	@echo "Result: $(YAEGI_WASM)"
	@ls -la $(YAEGI_WASM)

# Rebuild yaegi.wasm.gz into a tmp file and diff bytes against the
# checked-in artefact. Bumping the Go toolchain or the runner source must be
# an explicit commit that updates both .go-version and the embedded bytes.
verify-yaegi-wasm:
	@echo "Verifying $(YAEGI_WASM) is up to date..."
	@tmp_dir=$$(mktemp -d); \
	GOOS=wasip1 GOARCH=wasm $(GO) build -ldflags="-s -w" -trimpath -o $$tmp_dir/yaegi.wasm ./cmd/yaegi-runner && \
	gzip -9 -n -f $$tmp_dir/yaegi.wasm && \
	if cmp -s $(YAEGI_WASM) $$tmp_dir/yaegi.wasm.gz; then \
	  echo "OK — embedded artefact matches a fresh build."; \
	  rm -rf $$tmp_dir; \
	else \
	  echo "DRIFT — $(YAEGI_WASM) differs from a fresh build. Run 'make build-yaegi-wasm' and commit the result."; \
	  rm -rf $$tmp_dir; \
	  exit 1; \
	fi

.DEFAULT_GOAL := build
