package client

import (
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// injectedServers is the process-wide registry of host-constructed, live
// official-SDK MCP servers keyed by an opaque string. A host registers a
// *mcp.Server under a key BEFORE engine.Boot(); a ServerConfig with
// transport "inprocess" and a matching `server: <key>` then connects to it
// over an in-memory transport (no stdio subprocess, no HTTP dial).
//
// This mirrors the engine's PluginRegistry host-injection precedent
// (eng.Registry.Register(id, factory) before Boot) used by orbit's
// propose-tool. It lives in this plugin package rather than on pkg/engine
// because the engine core stays free of the MCP SDK — only this plugin
// imports it — so the injected-object type cannot cross the engine boundary.
var injectedServers = struct {
	mu      sync.RWMutex
	servers map[string]*mcp.Server
}{servers: map[string]*mcp.Server{}}

// RegisterInProcessServer registers a live MCP server under key so a
// ServerConfig with transport "inprocess" can connect to it in-process.
// Call before engine.Boot(). A second call with the same key replaces the
// previous registration.
func RegisterInProcessServer(key string, server *mcp.Server) {
	injectedServers.mu.Lock()
	defer injectedServers.mu.Unlock()
	injectedServers.servers[key] = server
}

// UnregisterInProcessServer removes a host-injected server. Intended for test
// cleanup so a process-wide registration doesn't leak across tests; hosts that
// live for the process lifetime need not call it.
func UnregisterInProcessServer(key string) {
	injectedServers.mu.Lock()
	defer injectedServers.mu.Unlock()
	delete(injectedServers.servers, key)
}

// lookupInProcessServer returns the host-injected server registered under key.
func lookupInProcessServer(key string) (*mcp.Server, bool) {
	injectedServers.mu.RLock()
	defer injectedServers.mu.RUnlock()
	s, ok := injectedServers.servers[key]
	return s, ok
}
