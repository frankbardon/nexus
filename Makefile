.PHONY: build build-broker run clean test test-race test-broker-integration fmt vet lint docs docs-serve docs-clean build-yaegi-wasm verify-yaegi-wasm check-events

BINARY_NAME=nexus
BROKER_BINARY_NAME=nexus-broker
BUILD_DIR=bin
GO=go
YAEGI_WASM=pkg/engine/sandbox/wasm/yaegi.wasm.gz

# cmd/nexus has no CGO deps (modernc.org/sqlite + chromem-go are pure Go).
# Disabling CGO + stripping symbols cuts binary ~25%. Desktop builds (Wails)
# require CGO and use a separate build path, not this Makefile.
CGO_ENABLED ?= 0
export CGO_ENABLED
BUILD_LDFLAGS=-s -w
BUILD_FLAGS=-ldflags="$(BUILD_LDFLAGS)" -trimpath

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

build: build-broker
	$(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/nexus

# nexus-broker: standalone service fronting OS-isolated Nexus instances.
# Pure Go (stdlib net/http + coder/websocket), so the same CGO_ENABLED=0
# build path applies.
build-broker:
	$(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BROKER_BINARY_NAME) ./cmd/nexus-broker

run: build
	$(BUILD_DIR)/$(BINARY_NAME) -config configs/default.yaml

clean:
	rm -rf $(BUILD_DIR)

test:
	$(GO) test ./...

# Repo-wide race-detector sweep. Run by the `race` job in
# .github/workflows/ci.yml via this exact target, so local and CI cannot drift —
# change the command here and CI changes with it.
#
# CGO_ENABLED=1 is a deliberate, target-local override of the file-level
# CGO_ENABLED=0 above: on linux/amd64 (the CI runner) `go build -race` fails
# outright with "-race requires cgo". Nothing in the build graph needs more than
# libc headers under cgo, so no extra runner packages are required.
#
# RACE_TIMEOUT is the per-package `go test` budget, and it is deliberately far
# larger than the default 600s. It is not padding for slow tests in general: it
# exists for pkg/engine/sandbox/wasm, whose first test builds a wasm sandbox and
# so pays the cold wazero AOT compile of yaegi.wasm.gz. Measured under -race,
# that one package costs ~227s on the ubuntu CI runner and 245s on an idle
# darwin/arm64 -- but 1477-1554s on that same darwin host under ~4x CPU
# oversubscription. A 6x spread on CPU contention alone is the point: the
# default 600s does not leave the required `race` job 2.6x of headroom, it
# leaves it 2.6x of headroom ON AN UNCONTENDED RUNNER, and GitHub's shared
# runners are not reliably that. Worse, when the budget is exceeded the failure
# reads `test timed out after 10m0s` -- a hang, not a budget -- so a slow runner
# looks like a deadlock somebody has to bisect.
#
# 40m is ~10x the uncontended figure and still ~1.5x the slowest contended
# measurement, while keeping a genuine deadlock bounded: the tradeoff accepted
# here is that a real hang takes up to 40 minutes to report instead of 10. That
# is the right way round for a REQUIRED check -- a spurious timeout blocks every
# PR and reads as a mystery, whereas a genuine deadlock blocks the PR either way
# and only costs one job's wall clock. It stays well inside GitHub's 6h job cap,
# so the job still terminates on its own.
#
# Do not lower this to "tidy up" a long-looking number without re-measuring the
# wasm package under -race, under contention, on both platforms.
RACE_TIMEOUT ?= 40m

test-race:
	CGO_ENABLED=1 $(GO) test -race -timeout $(RACE_TIMEOUT) ./...

# Broker integration suite. cmd/nexus-broker/claim_integration_test.go is
# //go:build integration, so `make test` (plain `go test ./...`, no tags) never
# runs it — the tag is deliberate, it keeps ~18s of stub-instance `go build`
# out of the default loop.
#
# Scoped to ./cmd/nexus-broker/ on purpose. `-tags integration ./...` would also
# sweep in tests/integration/, which carries the same tag but makes real LLM
# calls in live mode and needs ANTHROPIC_API_KEY. This suite is loopback-only:
# it spawns stub instance binaries it compiles itself, so it needs no key and no
# secrets. Run the engine suite separately:
#   go test -tags integration ./tests/integration/ -v
#
# -count=1 because the stub binaries are produced by a `go build` subprocess at
# test time; a cached PASS would not be a real gate.
test-broker-integration:
	$(GO) test -tags integration -count=1 ./cmd/nexus-broker/

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

# STATICCHECK_VERSION is pinned, never @latest. v0.8.0 declares go 1.26.0, so an
# unpinned `go run ...@latest` breaks the moment a release outpaces the Go version
# CI pins (GOTOOLCHAIN=local, Go 1.25) -- deterministically, on every commit,
# including ones that passed hours earlier. It also passes on a developer machine
# running a newer Go, which is how it reached main unnoticed.
# Bump this together with the go-version matrix in .github/workflows/ci.yml.
STATICCHECK_VERSION ?= v0.7.0

lint: vet check-events
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

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
