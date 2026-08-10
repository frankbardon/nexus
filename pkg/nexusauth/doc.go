// Package nexusauth verifies a credential minted elsewhere and turns an inbound
// HTTP request into a Principal.
//
// It deliberately knows nothing about the session broker or about any plugin.
// cmd/nexus-broker is package main and therefore importable by nothing, while
// nexus.io.agui is a plugin — the only place code both can share is a neutral
// pkg/ package, which is why this one exists.
//
// The moving parts:
//
//   - Principal — the verified identity. ID is the only field authorization
//     compares; Tenant, Scopes and Claims are carried for policy and audit.
//   - Validator — the single extension point. Three implementations ship here:
//     static (a token → Principal table), jwks (an OIDC JWS verified against the
//     issuer's published key set) and introspect (an opaque token verified by
//     asking the issuer, per RFC 7662). Others might trust a fronting proxy's
//     headers.
//   - Chain — an ordered set of named validators. The first success wins. When
//     every validator denies, the aggregate *DeniedError names each validator
//     and its reason so an operator can diagnose the rejection from a single
//     slog record.
//   - Kind and Error — four denial kinds a caller maps onto transport status
//     codes: KindNoCredential and KindInvalidCredential are 401,
//     KindInsufficientScope is 403, and KindUnavailable is 503 (we could not
//     find out — retry, do not re-authenticate). Use KindOf, or errors.Is
//     against the package sentinels; never match on error strings.
//
// A chain built from an empty validator list is *disabled*, not permissive:
// Validate always returns ErrAuthDisabled and never a Principal. Callers decide
// what an unconfigured chain means — the broker treats it as auth-off plus a
// loud warning — but the package itself will never silently allow a request.
//
// Config arrives as a map[string]any so the same code path serves broker.yaml
// (decoded by gopkg.in/yaml.v3) and a plugin's ctx.Config block. See
// ParseConfig and BuildChain.
//
// The package is stdlib-only apart from github.com/golang-jwt/jwt/v5, which the
// jwks validator uses for signature and standard-claim verification (the
// introspect validator is plain net/http and encoding/json). That is a
// deliberate, single exception: hand-rolling JWS verification is exactly the
// kind of code that fails silently and open. Everything around it — the JWKS
// fetch, the JWK-to-public-key reconstruction, the key cache and its rotation
// behaviour — is written here against stdlib crypto rather than pulling in a
// JOSE library. This is the shared trust boundary for two very different hosts
// and must not drag dependencies into either.
package nexusauth
