package client

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resourceSubscriber is the slice of *mcp.ClientSession the update
// subscription path needs. It exists so the capability gate, the
// failure-reporting policy and the drop-on-refresh unsubscribe can be
// unit-tested against a fake session.
type resourceSubscriber interface {
	InitializeResult() *mcp.InitializeResult
	Subscribe(ctx context.Context, params *mcp.SubscribeParams) error
	Unsubscribe(ctx context.Context, params *mcp.UnsubscribeParams) error
}

// supportsResourceSubscribe reports whether the server advertised
// capabilities.resources.subscribe during the handshake (initialize on the
// legacy protocol, server/discover at >= 2026-07-28).
//
// Subscribing without that capability is a protocol error either way: a
// legacy server answers resources/subscribe with MethodNotFound, and at
// 2026-07-28 — where resources/subscribe no longer exists and the SDK carries
// the subscription on a subscriptions/listen stream instead — the server
// silently drops the URI from the acknowledged set, so no update would ever
// arrive and no error would ever surface.
func supportsResourceSubscribe(res *mcp.InitializeResult) bool {
	return res != nil &&
		res.Capabilities != nil &&
		res.Capabilities.Resources != nil &&
		res.Capabilities.Resources.Subscribe
}

// syncSubscriptions reconciles update subscriptions with the set of
// auto-registered static resources after a resources refresh: URIs that
// were subscribed but are no longer registered are unsubscribed, then every
// current one is subscribed when resources.subscribe_updates is on. Passing
// nil (the auto_register_max overflow path) drops every subscription.
func (s *server) syncSubscriptions(ctx context.Context, resources []*mcp.Resource) {
	sess := s.getSession()
	if sess == nil {
		return
	}
	s.syncSubscriptionsWith(ctx, sess, resources)
}

func (s *server) syncSubscriptionsWith(ctx context.Context, sub resourceSubscriber, resources []*mcp.Resource) {
	s.unsubscribeDropped(ctx, sub, resources)
	if !s.cfg.Resources.SubscribeUpdates || len(resources) == 0 {
		return
	}
	s.subscribeWith(ctx, sub, resources)
}

// unsubscribeDropped unsubscribes every tracked URI absent from keep and
// removes it from local tracking. Unsubscribe is cheap — on the legacy
// protocol it is one resources/unsubscribe request, at >= 2026-07-28 the SDK
// just cancels that URI's subscriptions/listen stream — and skipping it
// leaves a stream open for a resource that no longer exists. A failure is
// logged and otherwise ignored: the URI is gone from the server's list, so
// it is dropped from tracking either way.
func (s *server) unsubscribeDropped(ctx context.Context, sub resourceSubscriber, keep []*mcp.Resource) {
	want := make(map[string]struct{}, len(keep))
	for _, r := range keep {
		want[r.URI] = struct{}{}
	}
	var dropped []string
	s.mu.Lock()
	for uri := range s.subscribedURIs {
		if _, ok := want[uri]; !ok {
			dropped = append(dropped, uri)
			delete(s.subscribedURIs, uri)
		}
	}
	s.mu.Unlock()

	for _, uri := range dropped {
		unsubCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		err := sub.Unsubscribe(unsubCtx, &mcp.UnsubscribeParams{URI: uri})
		cancel()
		if err != nil {
			s.logger.Warn("mcp: resources/unsubscribe failed for a resource dropped from the list",
				"server", s.cfg.Name, "uri", uri, "error", err)
			continue
		}
		s.logger.Debug("mcp: unsubscribed resource dropped from the list",
			"server", s.cfg.Name, "uri", uri)
	}
}

// subscribeWith runs the capability gate and issues one Subscribe per URI.
// A server that does not advertise resources.subscribe is skipped cleanly at
// Debug. A failure on a server that DID advertise it is a real fault and is
// logged at Warn — once per server, so a server that rejects every URI does
// not flood the log on each resources/list_changed refresh; later failures
// drop to Debug.
func (s *server) subscribeWith(ctx context.Context, sub resourceSubscriber, resources []*mcp.Resource) {
	res := sub.InitializeResult()
	if !supportsResourceSubscribe(res) {
		protocol := ""
		if res != nil {
			protocol = res.ProtocolVersion
		}
		s.logger.Debug("mcp: server does not advertise resources.subscribe; skipping resource update subscriptions",
			"server", s.cfg.Name, "protocol_version", protocol, "resources", len(resources))
		return
	}

	for _, r := range resources {
		subCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		err := sub.Subscribe(subCtx, &mcp.SubscribeParams{URI: r.URI})
		cancel()
		if err == nil {
			s.mu.Lock()
			s.subscribedURIs[r.URI] = struct{}{}
			s.mu.Unlock()
			continue
		}
		if s.subscribeWarned.CompareAndSwap(false, true) {
			s.logger.Warn("mcp: resources/subscribe failed on a server that advertised resources.subscribe; mcp.resource.updated will not fire for this resource",
				"server", s.cfg.Name, "uri", r.URI, "protocol_version", res.ProtocolVersion, "error", err)
			continue
		}
		s.logger.Debug("mcp: resources/subscribe failed",
			"server", s.cfg.Name, "uri", r.URI, "error", err)
	}
}
