// Package postgres is the PostgreSQL dialect of dbcleaner. Importing it registers the
// dialect for postgres:// and postgresql:// URLs, and the pgx driver it opens them with:
//
//	import _ "github.com/mokevnin/dbcleaner/postgres"
package postgres

import (
	"context"
	"database/sql"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // the driver
	"github.com/mokevnin/dbcleaner"
)

func init() {
	dbcleaner.Register("postgres", dialect{})
	dbcleaner.Register("postgresql", dialect{})
}

// Dialect is the PostgreSQL dialect, for [dbcleaner.WithDialect] when the URL is a key=value
// DSN. It is also a [dbcleaner.Leaser].
func Dialect() dbcleaner.Leaser { return dialect{} }

type dialect struct{}

func (dialect) Driver() string { return "pgx" }

func (dialect) Quote(identifier string) string { return quoteIdent(identifier) }

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
