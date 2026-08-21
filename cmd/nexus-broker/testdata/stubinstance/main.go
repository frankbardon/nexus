// Command stubinstance is a tiny stand-in for the real nexus binary used by the
// broker's integration test. It imports pkg/brokerframe (via stubcore), reads
// the broker dial-back env the broker injects at spawn, connects to the
// broker's instance endpoint, registers its lease, signals ready, reports a
// session id, and then echoes any inbound IO frame straight back. This proves
// the broker's claim/spawn/proxy mechanics end to end without booting a real
// engine or requiring an LLM API key.
//
// It mirrors the real engine's resume contract just enough for the test: when
// spawned with -recall <id> it reports that id back as its session id (proving
// the broker passed the recall arg); otherwise it synthesizes a deterministic
// new-session id the broker returns to the caller.
//
// This is the BASE variant — the binary the broker's reserved `nexus` registry
// entry points at in every test that does not care which variant it got. All of
// the behaviour lives in stubcore so that testdata/stubvariant can be a second,
// separately linked executable with an identical protocol implementation and a
// different compile-time identity.
//
// It lives under testdata/ so the normal `go build ./...` ignores it; the
// integration test builds it on demand and registers it as a spawn target.
package main

import "github.com/frankbardon/nexus/cmd/nexus-broker/testdata/stubcore"

func main() { stubcore.Run(stubcore.VariantBase) }
