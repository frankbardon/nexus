#!/usr/bin/env bash
# with-fake-gcs.sh — run a command with a Cloud Storage emulator available, and
# take it down again afterwards. Wraps the emulator lifecycle so that `make
# test-objectstore-fake-gcs` is one command, identical on a laptop and in CI.
#
# Usage:
#   scripts/with-fake-gcs.sh go test -C modules/objectstore-gcs -tags fakegcsserver ./...
#   NEXUS_TEST_FAKE_GCS_ENDPOINT=http://gcs.internal:4443 scripts/with-fake-gcs.sh ...
#
# Environment (all optional):
#   NEXUS_TEST_FAKE_GCS_ENDPOINT  already-running emulator; nothing is started
#   NEXUS_TEST_FAKE_GCS_PORT      host port to bind (default: a free one is picked)
#   NEXUS_TEST_FAKE_GCS_VERSION   pinned emulator version (see below)
#   GO                            go toolchain to build the emulator with
#
# ---------------------------------------------------------------------------
# Why this builds a Go binary where scripts/with-minio.sh runs a container
# ---------------------------------------------------------------------------
#
# The two scripts are deliberately the same shape — one command, a pinned
# version, a readiness wait, a port nothing else can be holding, an EXIT trap,
# and NEXUS_TEST_*_REQUIRED so a provisioned run cannot pass by skipping — and
# deliberately different in exactly one place: how the emulator is obtained.
#
# fake-gcs-server is a Go program distributed as a Go module, so `go install
# <module>@<version>` is available here and was not available for MinIO. Taking
# it means:
#
#   - No container runtime. The suite runs anywhere the repository's own
#     toolchain runs, which is a strictly larger set of machines than "has a
#     working Docker daemon" — a distinction that stops being academic the first
#     time a maintainer's daemon is broken and the emulator suite is the only
#     thing that cannot be run.
#   - A stronger pin than an image tag. `@v1.56.1` is verified against the
#     checksum database, so the bytes that are built are the bytes that were
#     published; a Docker tag is mutable by whoever owns the repository.
#   - An existing house mechanism rather than a new one. `make lint` already
#     fetches a pinned third-party Go tool this way
#     (`go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)`), so
#     nothing about this is a shape the tree has not already committed to.
#
# What it gives up: it needs a Go toolchain and a network fetch on the first
# run (about 12s cold, and the module and build caches make later runs a couple
# of seconds). Both are already true of every other target in the Makefile.
#
# A GitHub Actions `services:` block was rejected for the reason with-minio.sh
# records — it would make CI provision the emulator differently from a
# developer's terminal, which is the drift docs/src/guides/go-modules.md argues
# against. testcontainers-go was rejected for its reason too: a large dependency
# tree inside the one module that exists to keep dependencies out of everybody
# else's build.

set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "with-fake-gcs.sh: usage: with-fake-gcs.sh <command> [args...]" >&2
	exit 2
fi

# Pinned, never @latest, for the same reason STATICCHECK_VERSION is pinned in
# the Makefile: an unpinned version turns "CI went red" into "CI went red and
# nothing in this repository changed".
#
# It was held at v1.56.0 while this repository's floor was Go 1.25, because
# v1.56.1 raised its go directive to 1.26; the floor moved, so the pin tracks the
# newest release again. If a future release outruns the go directive, move this
# together with the go-version matrix in .github/workflows/ci.yml and the go
# directives in every go.mod, never separately.
#
# Bumping it is a commit, and the commit is where a behaviour change in the
# emulator gets noticed: TestFakeGCSServerDoesNotPaginateUnlessAsked and
# TestFakeGCSServerHasAFlatKeySpace in modules/objectstore-gcs both measure
# emulator behaviour the suite is built on, and both are version-sensitive by
# design.
EMULATOR="github.com/fsouza/fake-gcs-server"
VERSION="${NEXUS_TEST_FAKE_GCS_VERSION:-v1.56.1}"
GO="${GO:-go}"

# NEXUS_TEST_FAKE_GCS_REQUIRED is what stops this suite from being
# green-by-skip.
#
# The tests skip when nothing is listening, because they have to: a contributor
# whose network or toolchain cannot produce the emulator must be able to run
# `go test -tags fakegcsserver ./...` and get an honest "not run" rather than a
# red they cannot act on. But a skip reads as a pass in every CI summary ever
# built, so the caller that *provisioned* the emulator sets this and converts
# the skip into a failure. By the time the command below runs, this script has
# already waited for the health endpoint; a skip after that is a bug in the test
# file, not a missing dependency, and it should be loud.
export NEXUS_TEST_FAKE_GCS_REQUIRED=1

# An endpoint supplied from outside means somebody else owns the lifecycle: a
# shared emulator, a docker-compose stack, a `docker run fsouza/fake-gcs-server`
# somebody prefers. Start nothing, tear nothing down.
if [ -n "${NEXUS_TEST_FAKE_GCS_ENDPOINT:-}" ]; then
	echo "==> using the fake-gcs-server already at $NEXUS_TEST_FAKE_GCS_ENDPOINT"
	exec "$@"
fi

if ! command -v "$GO" >/dev/null 2>&1; then
	echo "with-fake-gcs.sh: no '$GO' on PATH." >&2
	echo "  Install Go, set GO=/path/to/go, or point" >&2
	echo "  NEXUS_TEST_FAKE_GCS_ENDPOINT at an emulator you are running yourself." >&2
	exit 1
fi

# A free port, discovered rather than assumed.
#
# 4443 is fake-gcs-server's own default and was the obvious choice, and it is
# wrong here for the reason 9000 was wrong for MinIO: a test harness that only
# works when a well-known port happens to be free intermittently blames the
# wrong thing. The emulator has no equivalent of `docker port` — it is told a
# port and binds it, and `-port 0` logs ":0" rather than what it got — so the
# port is chosen here and handed to both the server and the tests. Pin one with
# NEXUS_TEST_FAKE_GCS_PORT only if you want to poke at the store by hand while
# it runs.
port_is_free() {
	# bash's /dev/tcp connects if something is listening, so a *failure* to
	# connect is what "free" looks like.
	! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

PORT="${NEXUS_TEST_FAKE_GCS_PORT:-}"
if [ -z "$PORT" ]; then
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		candidate=$((20000 + RANDOM % 20000))
		if port_is_free "$candidate"; then
			PORT="$candidate"
			break
		fi
	done
fi
if [ -z "$PORT" ]; then
	echo "with-fake-gcs.sh: could not find a free port to bind." >&2
	exit 1
fi

# Built into a scratch GOBIN rather than run with `go run`, so the EXIT trap has
# a PID it can actually kill: `go run` starts the program as a child of itself,
# and killing the wrapper does not reliably stop the server underneath it.
BIN_DIR="$(mktemp -d)"
SERVER=""

cleanup() {
	if [ -n "$SERVER" ]; then
		kill "$SERVER" 2>/dev/null || true
		wait "$SERVER" 2>/dev/null || true
	fi
	rm -rf "$BIN_DIR"
}
trap cleanup EXIT

echo "==> building fake-gcs-server ($EMULATOR@$VERSION)"
GOBIN="$BIN_DIR" "$GO" install "$EMULATOR@$VERSION"

echo "==> starting fake-gcs-server on 127.0.0.1:$PORT"
# -host 127.0.0.1 rather than the emulator's default 0.0.0.0, so this is
# loopback-only, matching what `make test-broker-integration` and
# `make test-objectstore-minio` promise: no secrets, no cloud account, and
# nothing reachable from outside the machine.
#
# -backend memory because every case creates and destroys its own bucket and
# nothing is meant to outlive the run; the filesystem backend would need a root
# to clean up and would put the tests' 1200-object pagination case on a disk.
#
# -public-host 127.0.0.1 is load-bearing, not cosmetic. The SDK reads object
# bodies from the endpoint host's root (GET /<bucket>/<object>), and
# fake-gcs-server only routes that path when the request's Host matches its
# configured public host; left at the default, every download 404s while every
# upload succeeds.
"$BIN_DIR/fake-gcs-server" \
	-scheme http \
	-host 127.0.0.1 \
	-port "$PORT" \
	-backend memory \
	-public-host 127.0.0.1 \
	-log-level error &
SERVER=$!

export NEXUS_TEST_FAKE_GCS_ENDPOINT="http://127.0.0.1:$PORT"

# /_internal/healthcheck answers 200 once the server is serving. Polled rather
# than slept on: a fixed sleep is either too short on a loaded CI runner or
# wasted on a laptop.
deadline=$((SECONDS + 60))
until curl -fsS -o /dev/null "$NEXUS_TEST_FAKE_GCS_ENDPOINT/_internal/healthcheck" 2>/dev/null; do
	if ! kill -0 "$SERVER" 2>/dev/null; then
		echo "with-fake-gcs.sh: fake-gcs-server exited before becoming healthy." >&2
		exit 1
	fi
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "with-fake-gcs.sh: fake-gcs-server did not become healthy within 60s." >&2
		exit 1
	fi
	sleep 0.2
done
echo "==> fake-gcs-server is healthy at $NEXUS_TEST_FAKE_GCS_ENDPOINT"

# No `exec`: the EXIT trap has to run. Preserve the command's status so the make
# target and the CI step fail for the reason the tests failed.
status=0
"$@" || status=$?
exit "$status"
