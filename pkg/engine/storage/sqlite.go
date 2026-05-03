package storage

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// sqliteStore is the SQLite-backed Storage implementation. Each instance owns
// one *sql.DB pointing at a single .db file. The kv table is created lazily
// on first KV method call so plugins that only use DB() never see it.
type sqliteStore struct {
	db      *sql.DB
	path    string
	kvOnce  sync.Once
	kvErr   error
	closeFn func() error
}

func openSQLite(path string, opts SQLiteOptions) (*sqliteStore, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-%d)",
		path, opts.BusyTimeoutMs, opts.CacheSizeKB,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", path, err)
	}
	db.SetMaxIdleConns(opts.PoolMaxIdle)
	db.SetMaxOpenConns(opts.PoolMaxOpen)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping %q: %w", path, err)
	}

	return &sqliteStore{
		db:      db,
		path:    path,
		closeFn: db.Close,
	}, nil
}

// SQLiteOptions tunes the per-handle SQLite connection. Defaults are picked
// for an interactive single-user agent: 5s busy timeout, 2MB cache, small
// connection pool.
type SQLiteOptions struct {
	BusyTimeoutMs int
	CacheSizeKB   int
	PoolMaxIdle   int
	PoolMaxOpen   int
}

// DefaultSQLiteOptions returns the baseline tuning. Embedders override via
// the storage config block.
func DefaultSQLiteOptions() SQLiteOptions {
	return SQLiteOptions{
		BusyTimeoutMs: 5000,
		CacheSizeKB:   2048,
		PoolMaxIdle:   2,
		PoolMaxOpen:   4,
	}
}

func (s *sqliteStore) DB() *sql.DB { return s.db }

func (s *sqliteStore) ensureKV() error {
	s.kvOnce.Do(func() {
		_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS kv (
			k TEXT PRIMARY KEY,
			v BLOB NOT NULL
		) WITHOUT ROWID`)
		if err != nil {
			s.kvErr = fmt.Errorf("storage: ensure kv: %w", err)
		}
	})
	return s.kvErr
}

func (s *sqliteStore) Get(key string) ([]byte, bool, error) {
	if err := s.ensureKV(); err != nil {
		return nil, false, err
	}
	var v []byte
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: get %q: %w", key, err)
	}
	return v, true, nil
}

func (s *sqliteStore) Put(key string, value []byte) error {
	if err := s.ensureKV(); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO kv(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, value)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	return nil
}

func (s *sqliteStore) Delete(key string) error {
	if err := s.ensureKV(); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM kv WHERE k = ?`, key)
	if err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

func (s *sqliteStore) List(prefix string) ([]string, error) {
	if err := s.ensureKV(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT k FROM kv WHERE k LIKE ? ESCAPE '\' ORDER BY k`, escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("storage: list %q: %w", prefix, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("storage: list scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Tx(fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit tx: %w", err)
	}
	return nil
}

func (s *sqliteStore) Close() error {
	if s.closeFn == nil {
		return nil
	}
	return s.closeFn()
}

// escapeLike escapes the SQL LIKE wildcards (% and _) and the backslash escape
// character so prefix is matched literally. The query above includes
// ESCAPE-equivalent semantics by using a raw concat — modernc/sqlite supports
// the standard backslash escape behavior when paired with `\` characters
// inserted here. We do not rely on user-controlled input crossing this
// boundary in the engine itself, but plugins can hand it arbitrary keys.
func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' || c == '_' || c == '\\' {
			out = append(out, '\\', c)
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
