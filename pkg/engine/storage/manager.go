package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Manager owns the open Storage handles for an engine instance. It resolves
// scope+pluginID to a deterministic on-disk path and pools handles so two
// requests for the same (scope, pluginID) share a single *sql.DB.
//
// Manager is constructed once per engine in Engine.newFromConfig and torn
// down by Engine.Stop. PluginContext.Storage is a thin wrapper around it.
type Manager struct {
	root      string
	agentID   string
	sessionFn func() string
	opts      SQLiteOptions

	mu   sync.Mutex
	pool map[handleKey]*sqliteStore
}

type handleKey struct {
	scope    Scope
	pluginID string
}

// NewManager constructs a storage manager.
//
//   - root is the user data root, typically "~/.nexus" expanded. App and
//     Agent scope paths land beneath it.
//   - agentID partitions Agent-scope storage. Empty string collapses Agent
//     to App, which is the common case for CLI / single-agent embedders.
//   - sessionFn returns the active session's RootDir. It is called lazily so
//     the manager can be constructed before the session exists. Returning
//     "" causes Open(ScopeSession) to fail with a clear error — Manager
//     does not assume a session is always available.
//   - opts are the SQLite tuning parameters; nil falls back to
//     DefaultSQLiteOptions.
func NewManager(root, agentID string, sessionFn func() string, opts *SQLiteOptions) *Manager {
	o := DefaultSQLiteOptions()
	if opts != nil {
		if opts.BusyTimeoutMs > 0 {
			o.BusyTimeoutMs = opts.BusyTimeoutMs
		}
		if opts.CacheSizeKB > 0 {
			o.CacheSizeKB = opts.CacheSizeKB
		}
		if opts.PoolMaxIdle > 0 {
			o.PoolMaxIdle = opts.PoolMaxIdle
		}
		if opts.PoolMaxOpen > 0 {
			o.PoolMaxOpen = opts.PoolMaxOpen
		}
	}
	return &Manager{
		root:      root,
		agentID:   agentID,
		sessionFn: sessionFn,
		opts:      o,
		pool:      make(map[handleKey]*sqliteStore),
	}
}

// AttachSessionResolver replaces the session resolver. The engine wires this
// after construction so plugins that call Open(ScopeSession) during Init see
// the live session that was created in Boot.
func (m *Manager) AttachSessionResolver(fn func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionFn = fn
}

// Open returns the storage handle for (scope, pluginID), opening it on first
// use. Repeated calls with the same arguments share the same *sql.DB.
//
// When scope is ScopeAgent and the manager was constructed without an
// agent ID, the request collapses to ScopeApp so plugins do not end up with
// two separate connection pools pointing at the same file.
func (m *Manager) Open(scope Scope, pluginID string) (Storage, error) {
	if pluginID == "" {
		return nil, fmt.Errorf("storage: pluginID required")
	}
	if scope == ScopeAgent && m.agentID == "" {
		scope = ScopeApp
	}
	key := handleKey{scope: scope, pluginID: pluginID}

	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.pool[key]; ok {
		return h, nil
	}

	dir, err := m.dirFor(scope, pluginID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir %q: %w", dir, err)
	}
	path := filepath.Join(dir, "store.db")

	st, err := openSQLite(path, m.opts)
	if err != nil {
		return nil, err
	}
	m.pool[key] = st
	return st, nil
}

// Close drains every open handle. Safe to call multiple times; subsequent
// calls return nil. Errors from individual handles are joined into a single
// error for visibility but never blockclose of remaining handles.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for k, st := range m.pool {
		if err := st.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("storage: close %s/%s: %w", k.scope, k.pluginID, err)
		}
		delete(m.pool, k)
	}
	return firstErr
}

// dirFor resolves the directory that holds the .db file for (scope, pluginID).
// Path conventions:
//
//	ScopeApp     → <root>/plugins/<pluginID>/
//	ScopeAgent   → <root>/agents/<agentID>/plugins/<pluginID>/
//	             → falls back to ScopeApp when agentID is empty
//	ScopeSession → <session.RootDir>/plugins/<pluginID>/
func (m *Manager) dirFor(scope Scope, pluginID string) (string, error) {
	switch scope {
	case ScopeApp:
		return filepath.Join(m.root, "plugins", pluginID), nil
	case ScopeAgent:
		if m.agentID == "" {
			return filepath.Join(m.root, "plugins", pluginID), nil
		}
		return filepath.Join(m.root, "agents", m.agentID, "plugins", pluginID), nil
	case ScopeSession:
		if m.sessionFn == nil {
			return "", fmt.Errorf("storage: session scope unavailable (no session yet)")
		}
		sessionDir := m.sessionFn()
		if sessionDir == "" {
			return "", fmt.Errorf("storage: session scope unavailable (no active session)")
		}
		return filepath.Join(sessionDir, "plugins", pluginID), nil
	default:
		return "", fmt.Errorf("storage: unknown scope %s", scope)
	}
}
