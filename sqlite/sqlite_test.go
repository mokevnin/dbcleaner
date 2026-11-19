package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mokevnin/dbcleaner/v2"
	"github.com/mokevnin/dbcleaner/v2/internal/conformance"
	"github.com/mokevnin/dbcleaner/v2/sqlite"
	"github.com/stretchr/testify/require"
)

var schema = []string{
	`CREATE TABLE schema_migrations (version text PRIMARY KEY)`,
	`INSERT INTO schema_migrations VALUES ('1')`,
	`CREATE TABLE authors (id integer PRIMARY KEY AUTOINCREMENT, name text NOT NULL)`,
	`CREATE TABLE books (id integer PRIMARY KEY AUTOINCREMENT, author_id integer NOT NULL REFERENCES authors (id), title text NOT NULL)`,
	`CREATE TABLE tags (id integer PRIMARY KEY AUTOINCREMENT, name text NOT NULL, parent_id integer REFERENCES tags (id))`,
}

// file is the URL of a database file called name, foreign keys enforced.
func file(t *testing.T, name string) string {
	t.Helper()
	return "file:" + filepath.Join(t.TempDir(), name) + "?_pragma=foreign_keys(1)"
}

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Env{
		Dialect: sqlite.Dialect(),
		Fresh:   func(t *testing.T) string { return file(t, "app_test.db") },
		Schema:  schema,
	})
}

func TestTheGuardGoesByTheFileName(t *testing.T) {
	for name, allowed := range map[string]bool{"app.db": false, "app_test.db": true} {
		c := dbcleaner.New(sqlite.Dialect(), file(t, name))
		t.Cleanup(func() { _ = c.Close() })
		err := c.CleanWith(t.Context(), dbcleaner.Truncation())
		if allowed {
			require.NoError(t, err, name)
		} else {
			require.ErrorContains(t, err, `refusing to wipe database "`+name+`"`)
		}
	}
}

func TestADatabaseInMemoryPassesTheGuard(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	name, err := sqlite.Dialect().CurrentDatabase(t.Context(), db)
	require.NoError(t, err)
	require.True(t, dbcleaner.DefaultGuard(name), name)
}

func TestAPlainPathWorksToo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app_test.db")
	c := dbcleaner.New(sqlite.Dialect(), path)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Deletion()))
}

func TestTablesLeaveInternalTablesAndViewsOut(t *testing.T) {
	db, err := sql.Open("sqlite", file(t, "app_test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE plain (id integer PRIMARY KEY AUTOINCREMENT)`, // makes sqlite_sequence
		`CREATE VIEW plain_view AS SELECT id FROM plain`,
	} {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	tables, err := sqlite.Dialect().Tables(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, []string{"plain"}, tables)
}
