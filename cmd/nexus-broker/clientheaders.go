package main

import (
	"net/http"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusheaders"
)

// This file carries a client connection's X-Nexus-* request headers across the
// broker hop, on every surface — not just the A2A one this binary parses.
//
// # The problem the A2A path does not have
//
// On /agents/..., the broker DECODES the client's request and BUILDS the
// instance's `input` payload itself, so it simply sets Headers on the message
// (see a2aturn.go). Everywhere else the broker is an opaque pipe: a client
// speaks the instance IO envelope directly and the gateway forwards its frames
// verbatim, never decoding and never rewriting them. That rule is what lets an
// instance be newer than the broker in front of it, and it is not worth
// breaking to attach a header.
//
// So the headers travel as a frame of their OWN. The broker sends an
// instance-bound `client.headers` IO payload reporting the current client
// connection's headers, and nexus.io.broker applies it to the input that
// follows. Injecting a broker-authored frame is not the same as modifying a
// client's: every byte a client sent still reaches the instance unchanged, and
// a frame the instance does not recognise is ignored rather than fatal, so an
// older instance behind a newer broker is unaffected.
//
// # Why the announcement wins over a client-supplied Headers field
//
// Because the pipe is opaque, a client CAN put `headers` on its own `input`
// envelope and the broker cannot strip it. The announcement therefore takes
// precedence on the instance side: it is derived from the HTTP request, which
// is the hop an operator's reverse proxy controls and injects into (an
// oauth2-proxy stamping X-Nexus-Subject, say), while the envelope field is
// whatever the client typed with no proxy in the path. If the client's field
// won, anything a trusted proxy asserted could be overridden by the caller it
// was asserting about.
//
// # Why it is re-sent before every client IO frame
//
// The alternative — announce once when the client attaches — has two holes,
// both of which produce a turn running under the wrong context rather than a
// visible failure: an announcement sent before the instance has dialled back
// is DROPPED (forwardToInstance has no peer to queue it on), and an instance
// re-spawned mid-connection after a crash comes up knowing nothing. Sending it
// immediately ahead of each IO frame makes the pairing structural: whatever
// input the instance is about to read, it has just been told the headers that
// input arrived with. The cost is one small frame per user action — client to
// instance traffic is typed input and approval answers, not streaming — which
// is a price worth paying to have no state to get stale.
//
// An empty set is announced too, and is what clears the instance's copy. That
// is what stops a second client connection that sends no X-Nexus-* header from
// inheriting the first one's values.

// ioTypeClientHeaders is the instance-bound IO payload type carrying one client
// connection's X-Nexus-* request headers. It is broker-originated: no client
// and no instance ever sends one.
const ioTypeClientHeaders = "client.headers"

// announceClientHeaders wraps an instance-bound forward function so that every
// SignalIO frame is preceded by a `client.headers` payload reporting headers.
//
// Non-IO frames (pings, liveness, control) are forwarded untouched: they do not
// carry user input, so there is nothing for a header to describe.
func (g *Gateway) announceClientHeaders(headers map[string]string, forward func(string, brokerframe.Frame, []byte)) func(string, brokerframe.Frame, []byte) {
	return func(leaseID string, frame brokerframe.Frame, data []byte) {
		if frame.Signal == brokerframe.SignalIO {
			g.sendClientHeaders(leaseID, headers)
		}
		forward(leaseID, frame, data)
	}
}

// sendClientHeaders forwards one `client.headers` payload to the lease's
// instance.
//
// A failure here is logged and swallowed rather than dropping the input frame
// that follows. Losing the headers costs a turn its out-of-band context, which
// is a degradation; refusing the turn over it would be an outage, and the
// headers are supplementary by construction.
func (g *Gateway) sendClientHeaders(leaseID string, headers map[string]string) {
	data, err := encodeIOFrame(leaseID, brokerIOMessage{
		Type:    ioTypeClientHeaders,
		Headers: headers,
	})
	if err != nil {
		g.logger.Warn("could not encode the client's request headers for the instance",
			"lease_id", leaseID, "error", err)
		return
	}
	peer := g.registry.InstanceConn(leaseID)
	if peer == nil {
		// The input frame this would have preceded is about to be dropped for
		// the same reason, so there is nothing extra to report here.
		return
	}
	if !peer.queue(data) {
		g.logger.Warn("peer send buffer full, dropping the client's request headers",
			"lease_id", leaseID)
		g.registry.Metrics().frameDropped(frameDropInstanceBufferFull)
	}
}

// clientRequestHeaders reads the X-Nexus-* headers off a client handshake.
//
// It is a thin wrapper so every broker surface reads them through one call and
// the bounds in pkg/nexusheaders apply identically to the gateway hop and to
// the in-process transports.
func clientRequestHeaders(r *http.Request) map[string]string {
	return nexusheaders.Extract(r.Header)
}
