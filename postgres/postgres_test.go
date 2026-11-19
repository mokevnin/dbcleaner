package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/mokevnin/dbcleaner/v2"
	"github.com/mokevnin/dbcleaner/v2/internal/conformance"
	"github.com/mokevnin/dbcleaner/v2/postgres"
	"github.com/peterldowns/pgtestdb"
	"github.com/stretchr/testify/require"
)

// The suite needs a PostgreSQL server whose role can create databases; every test gets its
// own scratch database on it.
func serverURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("DBCLEANER_TEST_URL")
	require.NotEmpty(t, url, "set DBCLEANER_TEST_URL to a PostgreSQL URL, e.g. postgres://postgres@127.0.0.1:5432/postgres")
	return url
}

var schema = []string{
	`CREATE TABLE schema_migrations (version text PRIMARY KEY)`,
	`INSERT INTO schema_migrations VALUES ('1')`,
	`CREATE TABLE authors (id serial PRIMARY KEY, name text NOT NULL)`,
	`CREATE TABLE books (id serial PRIMARY KEY, author_id int NOT NULL REFERENCES authors (id), title text NOT NULL)`,
	`CREATE TABLE tags (id serial PRIMARY KEY, name text NOT NULL, parent_id int REFERENCES tags (id))`,
}

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Env{
		Dialect: postgres.Dialect(),
		Fresh:   func(t *testing.T) string { return postgres.ScratchDatabase(t, serverURL(t)) },
		Schema:  schema,
	})
}

// sqlmigrator migrates nothing.
type sqlmigrator struct{}

func (sqlmigrator) Hash() (string, error)                                   { return "none", nil }
func (sqlmigrator) Migrate(context.Context, *sql.DB, pgtestdb.Config) error { return nil }

func TestWithTemplateNeedsAURLToNameItsDatabasesAfter(t *testing.T) {
	c := dbcleaner.New(postgres.Dialect(postgres.WithTemplate(sqlmigrator{})), "host=127.0.0.1 dbname=app_test")
	_, err := c.Start(t.Context())
	require.ErrorContains(t, err, "WithTemplate needs a postgres:// URL")
}

func TestTablesLeavePartitionsAndViewsOut(t *testing.T) {
	url := postgres.ScratchDatabase(t, serverURL(t))
	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE events (id int, day date) PARTITION BY RANGE (day)`,
		`CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE TABLE plain (id int)`,
		`CREATE VIEW plain_view AS SELECT id FROM plain`,
	} {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	tables, err := postgres.Dialect().Tables(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, []string{"events", "plain"}, tables)
}

func TestReferencesLeaveSelfReferencesOut(t *testing.T) {
	url := postgres.ScratchDatabase(t, serverURL(t))
	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range schema {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	references, err := postgres.Dialect().References(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"books": {"authors"}}, references)
}
