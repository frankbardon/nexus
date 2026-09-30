#!/usr/bin/env bash
# with-minio.sh — run a command with a MinIO server available, and take it down
# again afterwards. Wraps the server lifecycle so that `make
# test-objectstore-minio` is one command, identical on a laptop and in CI.
#
# Usage:
#   scripts/with-minio.sh go test -C modules/objectstore-s3 -tags minio ./...
#   NEXUS_TEST_MINIO_ENDPOINT=http://minio.internal:9000 scripts/with-minio.sh ...
#
# Environment (all optional):
#   NEXUS_TEST_MINIO_ENDPOINT    already-running MinIO; nothing is built or started
#   NEXUS_TEST_MINIO_ACCESS_KEY  credentials for it (defaults below)
#   NEXUS_TEST_MINIO_SECRET_KEY
#   NEXUS_TEST_MINIO_PORT        API port to bind (default: a free one is picked)
#   NEXUS_TEST_MINIO_VERSION     pinned MinIO release tag (see below)
#   GO                           go toolchain to build MinIO with
#
# ---------------------------------------------------------------------------
# Why this builds MinIO from source
# ---------------------------------------------------------------------------
#
# Because source is the only official MinIO artifact an anonymous machine can
# still obtain. This script used to `docker run` MinIO's own image, and it moved
# registry once already: MinIO withdrew this release from docker.io/minio/minio
# (a tag query answered "object not found", a pull answered "pull access denied
# ... may require 'docker login'" — wording that reads like a credentials
# problem and was not one), so the pin moved to quay.io/minio/minio, which still
# served the same multi-arch manifest to anonymous pullers. On 2026-09-25 that
# went too: the scheduled objectstore-minio job started failing one second in,
# with `unauthorized: access to the requested resource is not authorized`, on a
# commit that had been green the day before. Every official channel was probed
# anonymously on 2026-09-30:
#
#   quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z   no such manifest; repo API 401
#   quay.io/minio/minio:latest                         no such manifest
#   docker.io/minio/minio (the pin, and :latest)       denied
#   dl.min.io/server/minio/release/... (binaries)      410 Gone
#   github.com/minio/minio tag RELEASE.2025-09-07T16-13-09Z   present
#
# Chasing another registry is chasing the thing that has now failed twice.
# MinIO is a Go program with a go.mod, so `go install
# github.com/minio/minio@<tag>` builds the exact pinned release — the tag
# resolves to v0.0.0-20250907161309-07c3a429bfed, verified against the checksum
# database, so the bytes built are the bytes that were tagged — with the
# toolchain this repository already requires. That is the same mechanism
# scripts/with-fake-gcs.sh uses for fake-gcs-server, and the two scripts are now
# the same shape end to end: one command, a pinned version built into a scratch
# GOBIN, a port nothing else can be holding, a readiness wait, an EXIT trap, and
# NEXUS_TEST_*_REQUIRED so a provisioned run cannot pass by skipping. A side
# effect worth having: neither emulator suite needs a container runtime any more.
#
# The alternatives, and why each was rejected:
#
#   - A third-party repackaged image (bitnami and friends). A supply-chain
#     decision — somebody else's build of somebody else's code, on a tag they
#     can move — made only because the upstream image went away. It also has no
#     reason to outlive the upstream channels it was built from.
#   - Our own image on GHCR, built from source. Solves reachability by adding a
#     publishing pipeline, a registry this repository must keep alive, and a
#     token scope CI does not currently need, to end up running the same binary
#     `go install` produces directly.
#   - A different S3-compatible emulator. This suite exists to run the S3
#     backend against MinIO specifically — MinIO is the S3-compatible store
#     docs/src/guides/object-storage.md names, and
#     TestMinIOCannotRepresentAnObjectAtAPrefix pins a MinIO behaviour. Swapping
#     the emulator changes what is being tested to make a distribution problem
#     go away.
#
# What it costs: the first run builds MinIO, which is a large program — about
# 50s cold on a laptop and a ~140 MB binary; the module and build caches make
# later runs a few seconds. Expect a cold CI runner to be slower than the laptop.
#
# Two rejections from the container era still stand. A GitHub Actions
# `services:` block would make CI provision MinIO differently from a developer's
# terminal, which is the drift docs/src/guides/go-modules.md argues against: one
# command per concern, shared verbatim between CI and a terminal. testcontainers-go
# would put a large dependency tree in modules/objectstore-s3/go.mod — test
# scaffolding in the one module that exists to keep a dependency out of
# everybody else's build.

set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "with-minio.sh: usage: with-minio.sh <command> [args...]" >&2
	exit 2
fi

# Pinned, never @latest, for the same reason STATICCHECK_VERSION is pinned in
# the Makefile: an unpinned version turns "CI went red" into "CI went red and
# nothing in this repository changed". Bumping it is a commit, and the commit is
# where a behaviour change in the emulator gets noticed —
# TestMinIOCannotRepresentAnObjectAtAPrefix in modules/objectstore-s3 records
# the one MinIO behaviour this suite has had to accommodate, and it is version-
# sensitive by design.
#
# The release is unchanged on purpose, across both the Docker Hub -> quay.io
# move and the quay.io -> source move: each was a change of where MinIO comes
# from, not which MinIO, and neither may smuggle in a different MinIO alongside
# the fix. MinIO's go.mod at this tag declares go 1.24.0, below this
# repository's floor, so the CI toolchain (GOTOOLCHAIN=local) builds it as is.
EMULATOR="github.com/minio/minio"
VERSION="${NEXUS_TEST_MINIO_VERSION:-RELEASE.2025-09-07T16-13-09Z}"
GO="${GO:-go}"

ACCESS_KEY="${NEXUS_TEST_MINIO_ACCESS_KEY:-nexusminio}"
# MinIO requires a root password of at least eight characters.
SECRET_KEY="${NEXUS_TEST_MINIO_SECRET_KEY:-nexusminio-secret}"

# NEXUS_TEST_MINIO_REQUIRED is what stops this suite from being green-by-skip.
#
# The tests skip when nothing is listening, because they have to: a contributor
# whose network or toolchain cannot produce MinIO must be able to run `go test
# -tags minio ./...` and get an honest "not run" rather than a red they cannot
# act on. But a skip reads as a pass in every CI summary ever built, so the
# caller that *provisioned* MinIO sets this and converts the skip into a
# failure. By the time the command below runs, this script has already waited
# for MinIO to report healthy; a skip after that is a bug in the test file, not
# a missing dependency, and it should be loud.
export NEXUS_TEST_MINIO_REQUIRED=1
export NEXUS_TEST_MINIO_ACCESS_KEY="$ACCESS_KEY"
export NEXUS_TEST_MINIO_SECRET_KEY="$SECRET_KEY"

# An endpoint supplied from outside means somebody else owns the lifecycle:
# a shared MinIO, a docker-compose stack, a colleague's cluster. Build nothing,
# start nothing, tear nothing down, and do not touch their credentials beyond
# the defaults above.
if [ -n "${NEXUS_TEST_MINIO_ENDPOINT:-}" ]; then
	echo "==> using the MinIO already at $NEXUS_TEST_MINIO_ENDPOINT"
	exec "$@"
fi

if ! command -v "$GO" >/dev/null 2>&1; then
	echo "with-minio.sh: no '$GO' on PATH." >&2
	echo "  Install Go, set GO=/path/to/go, or point" >&2
	echo "  NEXUS_TEST_MINIO_ENDPOINT at a MinIO you are running yourself." >&2
	exit 1
fi

# Free ports, discovered rather than assumed — picked exactly the way
# scripts/with-fake-gcs.sh picks its one.
#
# 9000 is MinIO's own default and was the obvious choice, and it is wrong here:
# it is one of the most contended ports on a developer machine — this was found
# by a maintainer laptop that already had an unrelated project's MinIO bound to
# it, so the very first run of `make test-objectstore-minio` failed with "port
# is already allocated" and nothing to do with the code under test. A test
# harness that only works when a well-known port happens to be free is a test
# harness that intermittently blames the wrong thing. The container era let
# Docker choose and asked it afterwards; a bare binary is told a port and binds
# it, so the port is chosen here and handed to both the server and the tests.
# Pin the API port with NEXUS_TEST_MINIO_PORT only if you want to poke at the
# store by hand while it runs.
#
# MinIO also opens a console listener, on a random port of its own unless told
# otherwise and on every interface — so it is given an explicit loopback port
# too, picked the same way and never equal to the API port.
port_is_free() {
	# bash's /dev/tcp connects if something is listening, so a *failure* to
	# connect is what "free" looks like.
	! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

pick_port() {
	# $1: a port the pick must not collide with (may be empty).
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		candidate=$((20000 + RANDOM % 20000))
		if [ "$candidate" != "${1:-}" ] && port_is_free "$candidate"; then
			echo "$candidate"
			return 0
		fi
	done
	return 1
}

PORT="${NEXUS_TEST_MINIO_PORT:-}"
if [ -z "$PORT" ]; then
	PORT="$(pick_port "")" || PORT=""
fi
CONSOLE_PORT=""
if [ -n "$PORT" ]; then
	CONSOLE_PORT="$(pick_port "$PORT")" || CONSOLE_PORT=""
fi
if [ -z "$PORT" ] || [ -z "$CONSOLE_PORT" ]; then
	echo "with-minio.sh: could not find free ports to bind." >&2
	exit 1
fi

# Built into a scratch GOBIN rather than run with `go run`, so the EXIT trap has
# a PID it can actually kill: `go run` starts the program as a child of itself,
# and killing the wrapper does not reliably stop the server underneath it. The
# data directory and the server log are scratch too, and go with it.
BIN_DIR="$(mktemp -d)"
DATA_DIR="$(mktemp -d)"
LOG_FILE="$(mktemp)"
SERVER=""

cleanup() {
	if [ -n "$SERVER" ]; then
		kill "$SERVER" 2>/dev/null || true
		wait "$SERVER" 2>/dev/null || true
	fi
	rm -rf "$BIN_DIR" "$DATA_DIR"
	rm -f "$LOG_FILE"
}
trap cleanup EXIT

dump_log() {
	echo "---- MinIO server log ----" >&2
	cat "$LOG_FILE" >&2 || true
	echo "---- end of MinIO server log ----" >&2
}

echo "==> building MinIO ($EMULATOR@$VERSION; slow on a cold cache)"
GOBIN="$BIN_DIR" "$GO" install "$EMULATOR@$VERSION"

echo "==> starting MinIO on 127.0.0.1:$PORT (console 127.0.0.1:$CONSOLE_PORT)"
# Both listeners on 127.0.0.1 rather than MinIO's default of every interface,
# so this is loopback-only, matching what `make test-broker-integration` and
# `make test-objectstore-fake-gcs` promise: no secrets, no cloud account, and
# nothing reachable from outside the machine.
#
# A single-drive `server <dir>` rather than a multi-drive erasure set: the wire
# protocol is what is under test and the drive layout does not change it, while
# erasure mode needs four drives and a slower readiness handshake. The one
# behaviour this suite has had to accommodate was measured as identical in both
# modes.
#
# The log goes to a file rather than the terminal: MinIO's startup banner is
# noise in a passing run, and the whole of it is what matters in a failing one.
MINIO_ROOT_USER="$ACCESS_KEY" MINIO_ROOT_PASSWORD="$SECRET_KEY" \
	"$BIN_DIR/minio" server "$DATA_DIR" \
	--address "127.0.0.1:$PORT" \
	--console-address "127.0.0.1:$CONSOLE_PORT" \
	>"$LOG_FILE" 2>&1 &
SERVER=$!

export NEXUS_TEST_MINIO_ENDPOINT="http://127.0.0.1:$PORT"

# /minio/health/cluster, not /minio/health/live. `live` answers 200 as soon as
# the HTTP listener is up, while the object layer is still initialising, and a
# CreateBucket issued in that window fails with XMinioServerNotInitialized —
# measured, not theorised. `cluster` is 503 until the store can actually serve.
# Polled rather than slept on, and a server that has already died fails at once
# rather than after the whole deadline.
deadline=$((SECONDS + 90))
until curl -fsS -o /dev/null "$NEXUS_TEST_MINIO_ENDPOINT/minio/health/cluster" 2>/dev/null; do
	if ! kill -0 "$SERVER" 2>/dev/null; then
		echo "with-minio.sh: MinIO exited before becoming healthy." >&2
		dump_log
		exit 1
	fi
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "with-minio.sh: MinIO did not become healthy within 90s." >&2
		dump_log
		exit 1
	fi
	sleep 0.5
done
echo "==> MinIO is healthy at $NEXUS_TEST_MINIO_ENDPOINT"

# No `exec`: the EXIT trap has to run. Preserve the command's status so the
# make target and the CI step fail for the reason the tests failed.
status=0
"$@" || status=$?
exit "$status"
