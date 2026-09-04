#!/usr/bin/env bash
# with-minio.sh — run a command with a MinIO server available, and take it down
# again afterwards. Wraps the container lifecycle so that `make
# test-objectstore-minio` is one command, identical on a laptop and in CI.
#
# Usage:
#   scripts/with-minio.sh go test -C modules/objectstore-s3 -tags minio ./...
#   NEXUS_TEST_MINIO_ENDPOINT=http://minio.internal:9000 scripts/with-minio.sh ...
#
# Environment (all optional):
#   NEXUS_TEST_MINIO_ENDPOINT    already-running MinIO; no container is started
#   NEXUS_TEST_MINIO_ACCESS_KEY  credentials for it (defaults below)
#   NEXUS_TEST_MINIO_SECRET_KEY
#   NEXUS_TEST_MINIO_PORT        host port to publish (default: let Docker pick)
#   NEXUS_TEST_MINIO_IMAGE       pinned image (see below)
#   NEXUS_TEST_MINIO_NAME        container name
#   DOCKER                       container runtime (default docker; podman works)
#
# ---------------------------------------------------------------------------
# Why a script driving `docker run`, rather than the two obvious alternatives
# ---------------------------------------------------------------------------
#
# A GitHub Actions `services:` container was the first choice and was rejected.
# `services:` cannot override an image's command, and minio/minio needs the
# `server /data` argument to start at all — the usual way round that is to swap
# in a third-party repackaging of MinIO, which is a supply-chain decision made
# to satisfy a YAML limitation. It would also mean CI provisions MinIO one way
# and a developer provisions it another, which is precisely the drift
# docs/src/guides/go-modules.md argues against: one command per concern, shared
# verbatim between CI and a terminal, so the two cannot diverge into a state
# where CI is testing something else.
#
# testcontainers-go was the second and was rejected too. It would put the
# lifecycle inside the test binary, which is tidy, at the cost of a large
# dependency tree in modules/objectstore-s3/go.mod — added for test scaffolding,
# in the one module that exists specifically to keep a dependency out of
# everybody else's build. It also still requires a Docker daemon, so it buys no
# portability; it only moves where the daemon is spoken to.
#
# What this shape gives up: it does not work without a container runtime. That
# is deliberate and bounded — the suite is build-tagged, so `make test` on a
# machine with no Docker is untouched, and a developer who has MinIO running
# some other way sets NEXUS_TEST_MINIO_ENDPOINT and this script gets out of the
# way entirely.

set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "with-minio.sh: usage: with-minio.sh <command> [args...]" >&2
	exit 2
fi

# Pinned, never :latest, for the same reason STATICCHECK_VERSION is pinned in
# the Makefile: an unpinned tag turns "CI went red" into "CI went red and
# nothing in this repository changed". Bumping it is a commit, and the commit is
# where a behaviour change in the emulator gets noticed —
# TestMinIOCannotRepresentAnObjectAtAPrefix in modules/objectstore-s3 records
# the one MinIO behaviour this suite has had to accommodate, and it is version-
# sensitive by design.
IMAGE="${NEXUS_TEST_MINIO_IMAGE:-minio/minio:RELEASE.2025-09-07T16-13-09Z}"
NAME="${NEXUS_TEST_MINIO_NAME:-nexus-test-minio}"

# Empty means "let Docker choose a free host port", which is the default.
#
# 9000 is MinIO's own default and was the obvious choice, and it is wrong here:
# it is one of the most contended ports on a developer machine — this was found
# by a maintainer laptop that already had an unrelated project's MinIO bound to
# it, so the very first run of `make test-objectstore-minio` failed with "port
# is already allocated" and nothing to do with the code under test. A test
# harness that only works when a well-known port happens to be free is a test
# harness that intermittently blames the wrong thing. The port is discovered
# from the running container below and handed to the tests as an endpoint, so
# nothing needs to know it in advance; pin one here only if you want to poke at
# the store by hand while it runs.
PORT="${NEXUS_TEST_MINIO_PORT:-}"
ACCESS_KEY="${NEXUS_TEST_MINIO_ACCESS_KEY:-nexusminio}"
# MinIO requires a root password of at least eight characters.
SECRET_KEY="${NEXUS_TEST_MINIO_SECRET_KEY:-nexusminio-secret}"
DOCKER="${DOCKER:-docker}"

# NEXUS_TEST_MINIO_REQUIRED is what stops this suite from being green-by-skip.
#
# The tests skip when nothing is listening, because they have to: a contributor
# without a container runtime must be able to run `go test -tags minio ./...`
# and get an honest "not run" rather than a red they cannot act on. But a skip
# reads as a pass in every CI summary ever built, so the caller that
# *provisioned* MinIO sets this and converts the skip into a failure. By the
# time the command below runs, this script has already waited for MinIO to
# report healthy; a skip after that is a bug in the test file, not a missing
# dependency, and it should be loud.
export NEXUS_TEST_MINIO_REQUIRED=1
export NEXUS_TEST_MINIO_ACCESS_KEY="$ACCESS_KEY"
export NEXUS_TEST_MINIO_SECRET_KEY="$SECRET_KEY"

# An endpoint supplied from outside means somebody else owns the lifecycle:
# a shared MinIO, a docker-compose stack, a colleague's cluster. Start nothing,
# tear nothing down, and do not touch their credentials beyond the defaults
# above.
if [ -n "${NEXUS_TEST_MINIO_ENDPOINT:-}" ]; then
	echo "==> using the MinIO already at $NEXUS_TEST_MINIO_ENDPOINT"
	exec "$@"
fi

if ! command -v "$DOCKER" >/dev/null 2>&1; then
	echo "with-minio.sh: no '$DOCKER' on PATH." >&2
	echo "  Install a container runtime, set DOCKER=podman, or point" >&2
	echo "  NEXUS_TEST_MINIO_ENDPOINT at a MinIO you are running yourself." >&2
	exit 1
fi

# A previous run that was killed rather than exited leaves the container behind
# holding its name (and, if one was pinned, its port). Remove it up front; a
# fixed name is what makes that possible, and is also what lets someone remove
# it by hand.
"$DOCKER" rm -f "$NAME" >/dev/null 2>&1 || true

cleanup() {
	"$DOCKER" rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> starting MinIO ($IMAGE)"
# Published on 127.0.0.1 rather than 0.0.0.0 so this is loopback-only, matching
# what `make test-broker-integration` promises: no secrets, no cloud account,
# and nothing reachable from outside the machine.
#
# Single-drive `server /data` rather than a multi-drive erasure set: the wire
# protocol is what is under test and the drive layout does not change it, while
# erasure mode needs four mounts and a slower readiness handshake. The one
# behaviour this suite has had to accommodate was measured as identical in both
# modes.
"$DOCKER" run -d \
	--name "$NAME" \
	-p "127.0.0.1:$PORT:9000" \
	-e "MINIO_ROOT_USER=$ACCESS_KEY" \
	-e "MINIO_ROOT_PASSWORD=$SECRET_KEY" \
	"$IMAGE" server /data >/dev/null

# Ask the runtime which host port it actually bound. With PORT empty the -p
# above reads "127.0.0.1::9000", so this is the only place the answer exists.
mapped="$("$DOCKER" port "$NAME" 9000/tcp | head -n 1)"
if [ -z "$mapped" ]; then
	echo "with-minio.sh: could not determine the published port for container $NAME" >&2
	"$DOCKER" logs "$NAME" >&2 2>&1 || true
	exit 1
fi
export NEXUS_TEST_MINIO_ENDPOINT="http://127.0.0.1:${mapped##*:}"
echo "==> MinIO listening on $NEXUS_TEST_MINIO_ENDPOINT"

# /minio/health/cluster, not /minio/health/live. `live` answers 200 as soon as
# the HTTP listener is up, while the object layer is still initialising, and a
# CreateBucket issued in that window fails with XMinioServerNotInitialized —
# measured, not theorised. `cluster` is 503 until the store can actually serve.
deadline=$((SECONDS + 90))
until curl -fsS -o /dev/null "$NEXUS_TEST_MINIO_ENDPOINT/minio/health/cluster" 2>/dev/null; do
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "with-minio.sh: MinIO did not become healthy within 90s. Container log:" >&2
		"$DOCKER" logs "$NAME" >&2 2>&1 || true
		exit 1
	fi
	sleep 0.5
done
echo "==> MinIO is healthy"

# No `exec`: the EXIT trap has to run. Preserve the command's status so the
# make target and the CI step fail for the reason the tests failed.
status=0
"$@" || status=$?
exit "$status"
