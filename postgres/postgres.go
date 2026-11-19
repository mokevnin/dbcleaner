// Package postgres is the PostgreSQL dialect of dbcleaner, on the pgx driver:
//
//	dbcleaner.New(postgres.Dialect(), "postgres://postgres@127.0.0.1/app_test")
//
// With [WithTemplate], every test process works on a database of its own, cloned from a
// template.
package postgres

import (
	"context"
	"database/sql"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // the driver
	"github.com/mokevnin/dbcleaner/v2"
)

// Dialect is the PostgreSQL dialect. The URL may be a postgres:// URL or a key=value DSN,
// except with [WithTemplate], which derives the names of its databases from a URL.
func Dialect(opts ...Option) dbcleaner.Dialect {
	var d dialect
	for _, opt := range opts {
		opt(&d)
	}
	return d
}

// Option tunes the PostgreSQL dialect.
type Option func(*dialect)

type dialect struct {
	// template is set by WithTemplate.
	template *templateOptions
}

var _ dbcleaner.Dialect = dialect{}

func (dialect) Driver() string { return "pgx" }

func quoteIdent(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func (dialect) CurrentDatabase(ctx context.Context, db *sql.DB) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name)
	return name, err
}

func (dialect) Tables(ctx context.Context, db *sql.DB) ([]string, error) {
	// Partitions are left out: cleaning the parent cleans them.
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		ORDER BY c.relname`)
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
		SELECT child.relname, parent.relname
		FROM pg_constraint co
		JOIN pg_class child ON child.oid = co.conrelid
		JOIN pg_class parent ON parent.oid = co.confrelid
		JOIN pg_namespace n ON n.oid = child.relnamespace
		WHERE co.contype = 'f' AND n.nspname = current_schema() AND child.oid <> parent.oid`)
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

func (dialect) Truncate(ctx context.Context, db *sql.DB, tables []string) error {
	quoted := make([]string, len(tables))
	for i, table := range tables {
		quoted[i] = quoteIdent(table)
	}
	_, err := db.ExecContext(ctx, `TRUNCATE TABLE `+strings.Join(quoted, ", ")+` RESTART IDENTITY CASCADE`)
	return err
}

// Delete deletes the rows of the table. PostgreSQL checks a foreign key at the end of the
// statement, so a table that references itself needs nothing more.
func (dialect) Delete(ctx context.Context, tx *sql.Tx, table string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(table))
	return err
}
