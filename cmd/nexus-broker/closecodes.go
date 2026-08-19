package main

import "github.com/coder/websocket"

// Application-defined WebSocket close codes.
//
// RFC 6455 reserves 4000-4999 for the application, and the broker uses that
// range for exactly one purpose: telling a client WHY its socket went away when
// the difference changes what the client should do next. Everything the client
// can treat identically — a manual release, an idle reap, a shutdown — closes
// with the ordinary websocket.StatusGoingAway and needs no code of its own.
//
// They are declared together, in one place, so a future teardown reason cannot
// quietly reuse a value. Both are part of the broker's client-facing contract
// and are documented in docs/src/guides/session-broker.md.
//
//	4500  the instance crashed          — the session is gone; do not reconnect
//	                                      to this lease, claim a new one.
//	4501  a newer connection superseded — the lease is alive and now belongs to
//	      this one                        another socket; do NOT reconnect in a
//	                                      loop, or two clients will evict each
//	                                      other forever.
//
// The contrast is the point. A crash means the far end no longer exists; an
// eviction means it does exist and is talking to somebody else — which, in the
// common case, is this same client's newer tab or its own successful reconnect.
const (
	// crashCloseStatus is the close code the gateway uses when an instance
	// exits unexpectedly, so a connected client can tell a crash apart from a
	// normal release (which uses websocket.StatusGoingAway).
	crashCloseStatus = websocket.StatusCode(4500)

	// crashCloseReason is the human-readable close reason paired with
	// crashCloseStatus.
	crashCloseReason = "instance crashed"

	// evictedCloseStatus is the close code the gateway uses on a client
	// connection that a newer, successfully authenticated connection to the
	// same lease has displaced. It is deliberately distinguishable from BOTH a
	// crash and a going-away close: the lease is neither dead nor released, so
	// a client that reconnected on top of itself must not be told the session
	// ended, and a client that has genuinely been displaced by another socket
	// must not retry into a reconnect war.
	evictedCloseStatus = websocket.StatusCode(4501)

	// evictedCloseReason is the human-readable close reason paired with
	// evictedCloseStatus.
	evictedCloseReason = "superseded by a newer client connection"
)
