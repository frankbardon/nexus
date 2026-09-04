# Repository Go Modules

Nexus is one Go module — `github.com/frankbardon/nexus` — plus a small number of
**submodules** under `modules/`, each with its own `go.mod`.

This page is the contract for that arrangement: where a submodule may live, how
it resolves the core module, how the build and CI cover it, and how it is
versioned. If you are writing an object-store backend, an exporter or anything
else that needs a dependency Nexus core must not carry, start here.

## Why submodules exist at all

The root module deliberately carries no vendor SDKs. Every LLM provider is raw
`net/http`, storage is pure-Go `modernc.org/sqlite`, and the direct dependency
count is a number the project defends.

A cloud object-store backend cannot honour that. The AWS and Google SDKs are
large, transitively deep, and pull in their own HTTP, auth and retry machinery.
Adding one to `go.mod` would impose it on every `nexus` build, including builds
that will never touch a bucket.

So the seam is designed to be implemented from outside:
`pkg/engine/objectstore` imports nothing beyond the standard library, and its
conformance suite `pkg/engine/objectstore/objectstoretest` is exported. A
backend lives in its own module with its own dependency graph, and an embedder
who wants it blank-imports it into their own `main`. The root module's
dependency list does not move.

## Layout

```
nexus/
  go.mod                      # the root module: github.com/frankbardon/nexus
  pkg/ plugins/ cmd/ ...      # all of it root-module code, no exceptions
  modules/
    objectstore-seamcheck/    # github.com/frankbardon/nexus/modules/objectstore-seamcheck
      go.mod
```

Rules:

- **Every non-root `go.mod` lives at `modules/<name>/go.mod`.** One level, no
  nesting. `make check-modules` (run as part of `make lint`) fails the build if a
  `go.mod` appears anywhere else.
- **Module path mirrors the directory**: `modules/<name>` becomes
  `github.com/frankbardon/nexus/modules/<name>`. This is not a style preference —
  Go derives a submodule's version tag from its directory, so path and directory
  cannot diverge.
- **Names are flat and hyphenated**, describing the seam then the implementation:
  `objectstore-s3`, `objectstore-gcs`. Grouping them as `modules/objectstore/s3`
  was rejected because it makes `modules/objectstore` look like a module it is
  not, and lengthens every version tag for no gain.

Placing a backend beside the interface it implements — `pkg/engine/objectstore/s3/`
— reads better and was rejected anyway. `pkg/` means "root-module code" with no
exceptions worth remembering, and a nested `go.mod` under a directory everything
else sweeps is exactly the invisible-package trap described below.

## Why there is no `go.work`

A workspace is the obvious way to make a multi-module repo build as one, and it
is deliberately not used here. `go.work` is in `.gitignore`.

A workspace merges every listed module into a **single build list**. The cloud
SDKs required by the backend modules would then take part in version selection
for the root module as well, and `bin/nexus` built by a contributor with a
workspace active could resolve different transitive versions than the binary CI
and the release build produce. Guaranteeing that the root module's dependency
graph is exactly what its `go.mod` says is the entire reason the backends were
split out; a committed `go.work` would quietly give that back.

A workspace also does not buy what people assume. `go test ./...` does **not**
span workspace modules — `./...` still stops at the current module — so a
`go.work` would not have removed the need for the Makefile to walk `modules/`.

Instead, **each submodule carries a `replace`**:

```go
require github.com/frankbardon/nexus v0.18.2

replace github.com/frankbardon/nexus => ../..
```

The `replace` is scoped to that one module and affects nothing else in the tree.
Local development and CI therefore always build the submodule against the
working tree, which is what makes a submodule capable of failing when a change
to the seam breaks it. Anyone who depends on the submodule from outside this
repository ignores the `replace` — Go honours `replace` only in the main module —
and gets the `require`d version, which is why that line must always name a real
published tag rather than a `v0.0.0` placeholder.

`go.work` and `go.work.sum` are gitignored rather than merely absent, so a local
workspace for editor or debugging convenience is fine. Just never commit one.

## Build, test and lint coverage

The failure mode this plumbing exists to prevent: **a separate module is
invisible to every `./...` pattern.** `go build ./...`, `go test ./...`,
`go vet ./...` and `staticcheck ./...` all stop at a nested `go.mod` without a
word. A submodule that does not compile, or whose tests fail, reports as success
forever.

The Makefile therefore discovers submodules by glob and sweeps each one:

```make
GO_SUBMODULES := $(patsubst %/go.mod,%,$(wildcard modules/*/go.mod))
```

| Target | Root module | Submodules |
|---|---|---|
| `make build` | builds `cmd/nexus` + `cmd/nexus-broker` | `go build ./...` (compile check; submodules ship no binary) |
| `make test` | `go test ./...` | `go test ./...` |
| `make test-objectstore-minio` | — | `modules/objectstore-s3` only, `-tags minio` |
| `make test-objectstore-fake-gcs` | — | `modules/objectstore-gcs` only, `-tags fakegcsserver` |
| `make test-race` | `go test -race ./...` | `go test -race ./...` |
| `make fmt` | `go fmt ./...` | `go fmt ./...` |
| `make vet` | `go vet ./...` | `go vet ./...` |
| `make lint` | `vet` + `check-events` + `check-modules` + staticcheck | staticcheck |
| `make check-events` | root only | — |
| `make check-modules` | fails on a `go.mod` outside `modules/<name>/` | — |
| `make submodules` | prints the discovered list | — |

Three deliberate exceptions:

- **The emulator targets are not sweeps.** Each runs one submodule's
  build-tagged suite against an emulator the target starts and stops itself:
  `test-objectstore-minio` against MinIO via `scripts/with-minio.sh`, and
  `test-objectstore-fake-gcs` against fake-gcs-server via
  `scripts/with-fake-gcs.sh`. Two targets rather than one shared "emulator"
  target, because MinIO emulates S3 and fake-gcs-server emulates GCS — folding
  them together would mean one red step for two unrelated stores, and neither
  suite could be run on its own while working on its own backend. Everything
  above stays untagged, which is what keeps `make test` offline and secret-free
  even though it sweeps `modules/`.

  The two scripts are the same shape on purpose — a pinned emulator version, a
  readiness wait, a port nothing else can be holding, an EXIT trap, and a
  `NEXUS_TEST_*_REQUIRED` variable that turns the suite's no-emulator skip into
  a failure so a provisioned run cannot pass by skipping — and differ in one
  place: `with-minio.sh` runs a pinned container, while `with-fake-gcs.sh`
  builds a pinned Go binary with `go install <module>@<version>`, because
  fake-gcs-server is a Go module and MinIO is not. That means the GCS emulator
  suite needs no container runtime at all. Each script records the reasoning and
  the alternatives that were rejected.

  Both suites are also where the kill-and-resume cycle is proven against a real
  store, which is why **both** `modules/objectstore-s3/go.mod` and
  `modules/objectstore-gcs/go.mod` carry indirect requirements —
  `modernc.org/sqlite`, `gopkg.in/yaml.v3`, `klauspost/compress` — that have
  nothing to do with either cloud. They come from the root module's `pkg/engine`,
  which those **tests** import so they can drive a real engine against a real
  bucket; no non-test file in either module imports anything above
  `pkg/engine/objectstore`. The direction that matters is unchanged: the root
  module still does not require either one.

  The cycle itself is written once, in `pkg/engine/objectstore/enginetest`, and
  each module supplies only the four store-specific hooks it needs — register a
  factory, make an empty bucket, list the bucket, read one object. It is exported
  from the root module for the reason `objectstoretest` is: a backend may live in
  a module this repository never sees, and passing the interface conformance
  suite does not prove a session resumes from it. It is a separate package from
  `objectstoretest` because it imports `pkg/engine`, and `pkg/engine`'s own tests
  are `package engine` and import `objectstoretest` — so the two halves have to
  sit in different packages or neither builds.

- **`check-events` stays root-only.** `scripts/check-event-versions.sh` `cd`s to
  the repository top level and inspects `pkg/events/` alone. Event structs live
  in the root module and nowhere else, so running it per submodule would repeat
  the identical check while looking like it were checking something else.
- **`go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)` resolves
  independently of the current module**, so the same pinned staticcheck runs
  inside a submodule without that module having to require it.

CI needs no submodule-specific job for the sweeps: `.github/workflows/ci.yml`
runs `make build`, `make test`, `make test-race`, `make vet` and `make lint`, and
those cover `modules/` already. A build-tagged emulator suite is the one thing
that does need a workflow edit, because no sweep runs it — the
`objectstore-minio` and `objectstore-fake-gcs` jobs exist for that, one per
emulator, and each runs the same make target a developer runs. That is the
design — one command per concern, shared verbatim between CI and a developer's
terminal, so the two cannot drift into a state where CI skips something. Adding
a module under `modules/` requires no workflow edit; adding an *emulator* suite
to it does.

Dependabot covers the submodules through a glob (`directories: [/, /modules/*]`)
for the same reason.

## Versioning and tagging

The core module is released as a bare tag, `vX.Y.Z`, with a matching GitHub
Release. Nothing about submodules changes that, because **a bare `vX.Y.Z` tag
never versions a submodule** — Go requires the tag to be prefixed with the
module's directory.

The rules:

1. **Submodule tags are not cut by default.** Cutting a core release does not cut
   `modules/*` tags, and the release process does not have to know how many
   submodules exist. Inside the repository a submodule's version is irrelevant
   anyway: `make` and CI always build the working tree through the `replace`.
2. **A submodule tag is cut on demand**, when someone needs to `go get` that
   module into a program built outside this repository. The tag format is fixed
   by Go: `modules/<name>/vX.Y.Z`, for example
   `modules/objectstore-s3/v0.1.0`.
3. **Submodule versions are independent of the core version.** They are not kept
   in step with `vX.Y.Z` and must not be assumed to match. A backend whose SDK
   needs a patch release should not have to wait for a core release, and a core
   release should not imply that every backend was re-tested.
4. **When a submodule tag is cut, bump its `require github.com/frankbardon/nexus`
   line to the newest core tag first**, in the same commit. That line is what
   external consumers actually resolve, and a stale one gives them a core module
   older than the seam the backend was written against. The `replace` stays where
   it is — consumers ignore it.
5. **Compatibility is expressed by that `require` line**, not by a naming
   convention. "Which Nexus does this backend work with" is answered by reading
   `modules/<name>/go.mod`, and by the fact that CI builds it against the current
   tree on every commit.

## Adding a submodule

1. `mkdir modules/<name>` and write `go.mod` with module path
   `github.com/frankbardon/nexus/modules/<name>`, a `require` on the newest core
   tag, and `replace github.com/frankbardon/nexus => ../..`.
2. Write the code. Depend on whatever you need — that is the point.
3. `make build && make test && make lint`. The glob picks the module up with no
   Makefile, CI or Dependabot edit.
4. Prove the coverage is real: break something in the new module on purpose and
   confirm `make build` and `make test` go red. If they stay green, the module is
   in the wrong place or the plumbing has regressed.

`modules/objectstore-seamcheck` is the worked example, and is also the permanent
canary for step 4: it is not a backend and stores nothing, it exists to hold true
the property that `objectstore.Backend` and `objectstoretest.RunSuite` are usable
from a module that is not `github.com/frankbardon/nexus`.
