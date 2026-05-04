// Package storage provides per-plugin SQLite-backed key/value and SQL storage
// scoped at session, agent, or application levels.
//
// Every plugin can request a Storage handle via PluginContext.Storage(scope).
// Each (scope, plugin) pair maps to a single .db file under a deterministic
// path so plugin state is isolated and survives restart. KV methods cover the
// trivial put/get/list cases; plugins that need joins, transactions, or
// virtual tables (FTS5) call DB() and own their schema.
//
// The backend is modernc.org/sqlite — pure Go, no CGO, FTS5 included.
// SQLite is opened in WAL mode with a 5s busy timeout; foreign keys are on by
// default. Each handle wraps a *sql.DB with a small idle pool.
package storage

import (
	"database/sql"
	"fmt"
)

// Scope determines where a plugin's storage file lands.
//
// Session scope is per-session and disappears when the session is archived.
// Agent scope persists across sessions for a single agent (used by the
// desktop shell where each agent has its own engine instance). App scope
// persists across every session and every agent on the machine.
//
// In a single-agent embedder (CLI, oneshot), Agent collapses to App since
// there is no meaningful per-agent partition.
type Scope int

const (
	// ScopeSession is per-session. Path: <session.RootDir>/plugins/<pluginID>/store.db
	ScopeSession Scope = iota
	// ScopeAgent is per-agent (desktop shell multi-agent). Path:
	// ~/.nexus/agents/<agentID>/plugins/<pluginID>/store.db. Falls back to
	// ScopeApp when the engine has no AgentID configured.
	ScopeAgent
	// ScopeApp is machine-wide. Path: ~/.nexus/plugins/<pluginID>/store.db
	ScopeApp
)

// String returns a short identifier for the scope, used in logs.
func (s Scope) String() string {
	switch s {
	case ScopeSession:
		return "session"
	case ScopeAgent:
		return "agent"
	case ScopeApp:
		return "app"
	default:
		return fmt.Sprintf("scope(%d)", int(s))
	}
}

// Storage is a per-plugin scoped storage handle.
//
// Both the KV API and DB() share the same underlying *sql.DB and connection
// pool. Plugins can mix the two freely. The kv table is created lazily on
// the first KV method call; SQL-only consumers never see it.
type Storage interface {
	// DB returns the underlying *sql.DB. Plugins that need raw SQL — joins,
	// transactions, virtual tables (FTS5) — call this and own their schema.
	// The handle is owned by the storage manager; do not Close it.
	DB() *sql.DB

	// Get returns the value stored under key. The bool reports presence;
	// missing keys are not an error.
	Get(key string) ([]byte, bool, error)

	// Put writes value under key. Existing values are replaced.
	Put(key string, value []byte) error

	// Delete removes the key. Missing keys are not an error.
	Delete(key string) error

	// List returns keys with the given prefix in lexicographic order.
	List(prefix string) ([]string, error)

	// Tx runs fn inside a serialized transaction. The transaction is
	// committed when fn returns nil and rolled back on any non-nil error
	// or panic. The kv table is visible inside the transaction.
	Tx(fn func(*sql.Tx) error) error
}

// Provider opens scoped Storage handles for plugins. The engine constructs
// a single Provider at boot and exposes per-handle access through the
// PluginContext.
type Provider interface {
	// Open returns the storage handle for (scope, pluginID). Handles are
	// pooled, so repeated calls with the same arguments return the same
	// underlying *sql.DB. Safe for concurrent use.
	Open(scope Scope, pluginID string) (Storage, error)

	// Close drains every open handle. Called by Engine.Stop.
	Close() error
}
