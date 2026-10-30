package client

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resourceSubscriber is the slice of *mcp.ClientSession the update
// subscription path needs. It exists so the capability gate and the
// failure-reporting policy can be unit-tested against a fake session.
type resourceSubscriber interface {
	InitializeResult() *mcp.InitializeResult
	Subscribe(ctx context.Context, params *mcp.SubscribeParams) error
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

// subscribeResources subscribes to update notifications for each
// auto-registered static resource when resources.subscribe_updates is on.
func (s *server) subscribeResources(ctx context.Context, resources []*mcp.Resource) {
	if !s.cfg.Resources.SubscribeUpdates || len(resources) == 0 {
		return
	}
	sess := s.getSession()
	if sess == nil {
		return
	}
	s.subscribeWith(ctx, sess, resources)
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
