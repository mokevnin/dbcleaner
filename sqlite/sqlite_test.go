package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mokevnin/dbcleaner"
	"github.com/mokevnin/dbcleaner/sqlite"
	"github.com/stretchr/testify/require"
)

const baselineSchema = `
	CREATE TABLE schema_migrations (version text PRIMARY KEY);
	INSERT INTO schema_migrations VALUES ('1');
	CREATE TABLE authors (id integer PRIMARY KEY AUTOINCREMENT, name text NOT NULL);
	CREATE TABLE books (id integer PRIMARY KEY AUTOINCREMENT, author_id integer NOT NULL REFERENCES authors (id), title text NOT NULL);
	CREATE TABLE tags (id integer PRIMARY KEY, name text NOT NULL);
`

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, baselineSchema)
	return err
}

// fresh returns a cleaner over a database file named name, at the baseline schema, and a
// second connection to it: what another process sees.
func fresh(t *testing.T, name string, opts ...dbcleaner.Option) (*dbcleaner.Cleaner, *sql.DB) {
	t.Helper()
	url := "file:" + filepath.Join(t.TempDir(), name) + "?_pragma=foreign_keys(1)"
	c := dbcleaner.New(url, append([]dbcleaner.Option{dbcleaner.WithSetup(migrate)}, opts...)...)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	other, err := sql.Open("sqlite", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	return c, other
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&n))
	return n
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

func TestTransactionRollsTheTestBack(t *testing.T) {
	c, other := fresh(t, "app_test.db")
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Le Guin')`)
	require.Equal(t, 1, count(t, s.DB(), "authors"))
	require.NoError(t, s.Clean(t.Context()))
	require.Equal(t, 0, count(t, other, "authors"))
}

func TestTruncationEmptiesTablesResetsCountersAndKeepsMigrations(t *testing.T) {
	c, other := fresh(t, "app_test.db", dbcleaner.WithStrategy(dbcleaner.Truncation()))
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Le Guin')`)
	exec(t, s.DB(), `INSERT INTO books (author_id, title) VALUES (1, 'Earthsea')`)
	exec(t, s.DB(), `INSERT INTO tags (name) VALUES ('fantasy')`)
	require.Equal(t, 1, count(t, other, "books"), "rows are committed")

	require.NoError(t, s.Clean(t.Context()))
	for _, table := range []string{"authors", "books", "tags"} {
		require.Zero(t, count(t, other, table), table)
	}
	require.Equal(t, 1, count(t, other, "schema_migrations"))

	exec(t, other, `INSERT INTO authors (name) VALUES ('Butler')`)
	var id int
	require.NoError(t, other.QueryRowContext(t.Context(), `SELECT id FROM authors`).Scan(&id))
	require.Equal(t, 1, id, "AUTOINCREMENT starts over")
}

func TestTruncationRestoresForeignKeyEnforcement(t *testing.T) {
	c, _ := fresh(t, "app_test.db", dbcleaner.WithStrategy(dbcleaner.Truncation()))
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation()))
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	_, err = s.DB().ExecContext(t.Context(), `INSERT INTO books (author_id, title) VALUES (42, 'Orphan')`)
	require.Error(t, err, "the pool's connections enforce foreign keys again")
	require.NoError(t, s.Clean(t.Context()))
}

func TestDeletionGoesChildrenFirst(t *testing.T) {
	c, other := fresh(t, "app_test.db", dbcleaner.WithStrategy(dbcleaner.Deletion()))
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Le Guin')`)
	exec(t, s.DB(), `INSERT INTO books (author_id, title) VALUES (1, 'Earthsea')`)
	require.NoError(t, s.Clean(t.Context()))
	require.Zero(t, count(t, other, "authors"))
	require.Zero(t, count(t, other, "books"))
}

func TestTheGuardRefusesADatabaseThatIsNotForTests(t *testing.T) {
	c, _ := fresh(t, "app.db", dbcleaner.WithStrategy(dbcleaner.Truncation()))
	_, err := c.Start(t.Context())
	require.ErrorContains(t, err, `refusing to wipe database "app.db"`)
}

func TestAPlainPathNeedsWithDialect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app_test.db")
	_, err := dbcleaner.New(path).Start(t.Context())
	require.ErrorContains(t, err, "WithDialect")

	c := dbcleaner.New(path, dbcleaner.WithDialect(sqlite.Dialect()), dbcleaner.WithSetup(migrate))
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Deletion()))
}

func TestWithTemplateIsNotSupported(t *testing.T) {
	c, _ := fresh(t, "app_test.db", dbcleaner.WithTemplate("v1", func(context.Context, *sql.DB, string) error { return nil }))
	_, err := c.Start(t.Context())
	require.ErrorContains(t, err, "WithTemplate is not supported")
}
