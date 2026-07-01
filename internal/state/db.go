// Package state owns the SQLite database: schema migrations, row models, and
// the repository layer. It uses the pure-Go modernc.org/sqlite driver (no CGO)
// with FTS5 for text search.
package state

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with ai-reviewer repositories.
type DB struct {
	*sql.DB
}

// Open opens (creating if needed) the SQLite database at path with WAL mode,
// foreign keys, and a busy timeout. The connection pool is capped at one
// connection: a single-user local tool has no need for write concurrency and
// this sidesteps SQLITE_BUSY entirely.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_txlock=immediate",
		path,
	)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &DB{sqldb}, nil
}

// FTS5Available reports whether the driver supports FTS5 by attempting to
// create a temporary virtual table. Used by the doctor command.
func (db *DB) FTS5Available() error {
	if _, err := db.Exec(`CREATE VIRTUAL TABLE temp._fts5_probe USING fts5(x)`); err != nil {
		return fmt.Errorf("fts5 not available: %w", err)
	}
	_, _ = db.Exec(`DROP TABLE temp._fts5_probe`)
	return nil
}
