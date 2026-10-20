// Package sqlite is the SQLite dialect of dbcleaner, on the pure-Go modernc.org/sqlite
// driver. Importing it registers the dialect for file: URLs:
//
//	import _ "github.com/mokevnin/dbcleaner/sqlite"
//
//	dbcleaner.New("file:" + path)
//
// A plain path, or ":memory:", names no scheme: pass [Dialect] to [dbcleaner.WithDialect].
// Give the database a file, not ":memory:": every connection to ":memory:" is a database of
// its own, so the setup and the sessions would not see each other.
//
// SQLite has no TRUNCATE, so [dbcleaner.Truncation] deletes the rows, with the foreign keys
// switched off, and resets AUTOINCREMENT counters. It does not support [dbcleaner.WithTemplate]:
// a database per process is a file per process, which t.TempDir already is.
package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"

	"github.com/mokevnin/dbcleaner"
	_ "modernc.org/sqlite" // the driver
)

func init() { dbcleaner.Register("file", dialect{}) }

// Dialect is the SQLite dialect, for [dbcleaner.WithDialect] when the URL is a plain path.
func Dialect() dbcleaner.Dialect { return dialect{} }

type dialect struct{}

func (dialect) Driver() string { return "sqlite" }

func (dialect) Quote(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// CurrentDatabase is the file name of the main database. An in-memory or temporary one has
// no file, and nobody's data: it is named so that [dbcleaner.DefaultGuard] lets it through.
func (dialect) CurrentDatabase(ctx context.Context, db *sql.DB) (string, error) {
	var seq int
	var name, file string
	err := db.QueryRowContext(ctx, `SELECT seq, name, file FROM pragma_database_list WHERE name = 'main'`).Scan(&seq, &name, &file)
	if err != nil {
		return "", err
	}
	if file == "" {
		return "scratch (in memory)", nil
	}
	return filepath.Base(file), nil
}

func (dialect) Tables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (dialect) References(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT m.name, f."table"
		FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND m.name <> f."table"`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	references := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		references[child] = append(references[child], parent)
	}
	return references, rows.Err()
}

// Truncate deletes the rows on one connection with foreign_keys off (a pragma that is
// per connection, and that SQLite ignores inside a transaction), then puts it back as it was.
func (d dialect) Truncate(ctx context.Context, db *sql.DB, tables []string) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	var foreignKeys bool
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if foreignKeys {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer func() {
			if _, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); err == nil {
				err = restoreErr
			}
		}()
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+d.Quote(table)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	// sqlite_sequence exists once a table has used AUTOINCREMENT.
	var sequences int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&sequences); err != nil {
		_ = tx.Rollback()
		return err
	}
	if sequences > 0 {
		for _, table := range tables {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = ?`, table); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}
