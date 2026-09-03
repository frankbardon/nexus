.PHONY: build build-broker run clean test test-race test-broker-integration test-objectstore-minio fmt vet lint docs docs-serve docs-clean build-yaegi-wasm verify-yaegi-wasm check-events check-modules submodules

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

# ---------------------------------------------------------------------------
# Go submodules
#
# Directories under modules/ that carry their own go.mod are separate Go
# modules, and a separate module is INVISIBLE to every `./...` pattern run from
# the repo root: `go build ./...`, `go test ./...`, `go vet ./...` and
# staticcheck all stop dead at a nested go.mod without a word. Left alone, a
# submodule that does not compile and a submodule whose tests fail both report
# as success, forever -- which is the failure mode this list exists to close.
# Every target below that sweeps the tree sweeps this list as well.
#
# Discovered by glob rather than listed by hand, so adding a module is one
# directory rather than a directory plus five Makefile edits somebody forgets
# one of. modules/ is the only place they may live; check-modules enforces that.
GO_SUBMODULES := $(patsubst %/go.mod,%,$(wildcard modules/*/go.mod))

# Run one go command inside every submodule.
#
# The `==>` line names the module before the command runs, so a failure buried
# in a long CI step is attributable at a glance instead of looking like it came
# from the root module. `set -e` plus a subshell per module means the first
# failure aborts the target: collecting failures and reporting them at the end
# was the alternative, and it hides the first error under the noise of the ones
# it caused.
define in_submodules
@set -e; for mod in $(GO_SUBMODULES); do \
	echo "==> $$mod: $(1)"; \
	( cd $$mod && $(1) ); \
done
endef

# Print the discovered submodules. Diagnostic only -- useful when a submodule's
# tests mysteriously stop running, since an empty list here is the answer.
submodules:
	@for mod in $(GO_SUBMODULES); do echo $$mod; done

# The submodule sweep here is a compile check, not a build: submodules ship no
# binary (the object-store backends are libraries an embedder blank-imports into
# its own main), so it takes no BUILD_FLAGS and writes no output. The point is
# only that a submodule which does not compile fails `make build` exactly as
# cmd/nexus would.
build: build-broker
	$(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/nexus
	$(call in_submodules,$(GO) build ./...)

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
	$(call in_submodules,$(GO) test ./...)

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

# The submodules are swept too. They are small and the race job's wall clock is
# dominated by pkg/engine/sandbox/wasm, so the cost is noise -- and the seam is
# explicitly documented as safe for concurrent use, which is a claim only the
# race detector actually checks.
test-race:
	CGO_ENABLED=1 $(GO) test -race -timeout $(RACE_TIMEOUT) ./...
	$(call in_submodules,CGO_ENABLED=1 $(GO) test -race -timeout $(RACE_TIMEOUT) ./...)

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

# Object-store suite against a real S3 implementation.
#
# modules/objectstore-s3/minio_test.go is //go:build minio, so `make test`
# (plain `go test ./...`, no tags — including the submodule sweep) never runs
# it. That is not tidiness: the submodule sweep IS untagged, so an emulator
# suite left untagged would put a container start and ~10s of round trips into
# the default loop and break the "fast, offline, secret-free" promise `make
# test` makes.
#
# Follows test-broker-integration rather than tests/integration/. The engine
# suite is excluded from CI because live mode needs ANTHROPIC_API_KEY; that
# reasoning does not reach here. MinIO is an S3-compatible store running on
# loopback in a container this target starts and stops itself, so this needs no
# cloud account, no API key and no repository secret — exactly the property that
# lets the broker suite run in CI, and this one runs there too.
#
# scripts/with-minio.sh owns the container lifecycle and records why it is a
# script rather than a GitHub Actions `services:` block or testcontainers. It
# also exports NEXUS_TEST_MINIO_REQUIRED, which turns the suite's
# no-MinIO-so-skip path into a failure: the skip exists for a laptop with no
# container runtime, and a skip in a run that provisioned MinIO would be green
# while testing nothing.
#
# -count=1 for the broker suite's reason: the result depends on a server this
# target just started, so a cached PASS from a previous run — possibly against a
# different MinIO — would not be a gate.
#
# Not driven by GO_SUBMODULES. MinIO emulates S3 and nothing else, so this is
# specific to modules/objectstore-s3 by nature; the GCS module gets its own
# target and its own emulator.
test-objectstore-minio:
	scripts/with-minio.sh $(GO) test -C modules/objectstore-s3 -tags minio -count=1 ./...

fmt:
	$(GO) fmt ./...
	$(call in_submodules,$(GO) fmt ./...)

vet:
	$(GO) vet ./...
	$(call in_submodules,$(GO) vet ./...)

# STATICCHECK_VERSION is pinned, never @latest. v0.8.0 declares go 1.26.0, so an
# unpinned `go run ...@latest` breaks the moment a release outpaces the Go version
# CI pins (GOTOOLCHAIN=local, Go 1.25) -- deterministically, on every commit,
# including ones that passed hours earlier. It also passes on a developer machine
# running a newer Go, which is how it reached main unnoticed.
# Bump this together with the go-version matrix in .github/workflows/ci.yml.
STATICCHECK_VERSION ?= v0.7.0

# check-events is deliberately NOT run per submodule: scripts/check-event-versions.sh
# cd's to the repo top level and inspects pkg/events/ only, so running it from
# inside modules/<name> would either repeat the identical root-module check or,
# worse, look like it were checking something about the submodule. Event structs
# live in the root module and nowhere else.
#
# `go run <pkg>@<version>` resolves independently of the current module, so the
# same pinned staticcheck runs inside a submodule without that module needing to
# require it.
lint: vet check-events check-modules
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	$(call in_submodules,$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...)

# Static check: every pkg/events/ struct mutation must bump its
# <Name>Version constant. Compares the working tree against
# $$CHECK_EVENTS_BASE (default HEAD~1). See scripts/check-event-versions.sh
# for usage and docs/src/architecture/events.md for the rule itself.
check-events:
	@scripts/check-event-versions.sh

# Guard the assumption GO_SUBMODULES is built on: every go.mod other than the
# root one lives at modules/<name>/go.mod. A go.mod dropped anywhere else --
# pkg/engine/objectstore/s3/ is the tempting spot, right next to the interface it
# implements -- silently removes that subtree from every ./... sweep above, and
# nothing else in the build would ever mention it again. Cheaper to fail here
# than to discover months later that a package has not been compiled since.
#
# testdata/ is excluded because the go tool ignores those directories anyway.
check-modules:
	@stray=$$(find . -name go.mod \
	    -not -path './go.mod' \
	    -not -path './modules/*/go.mod' \
	    -not -path './.git/*' \
	    -not -path '*/testdata/*'); \
	if [ -n "$$stray" ]; then \
	  echo "check-modules: go.mod outside modules/<name>/ -- these packages are invisible to every ./... sweep:"; \
	  echo "$$stray"; \
	  echo "Move the module under modules/<name>/ (see docs/src/guides/go-modules.md) or fold it into the root module."; \
	  exit 1; \
	fi

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
