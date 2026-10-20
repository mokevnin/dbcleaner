// Package conformance is the test suite every dialect has to pass: the same behaviour of the
// strategies and the guard, on whatever engine the dialect is for.
package conformance

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mokevnin/dbcleaner"
	"github.com/stretchr/testify/require"
)

// Env is one engine as the suite needs it.
type Env struct {
	// Driver is the database/sql driver name, to open a second connection with.
	Driver string
	// Options select the dialect (WithDialect) when the URL cannot.
	Options []dbcleaner.Option
	// Fresh returns the URL of an empty database that is dropped when the test ends.
	Fresh func(t *testing.T) string
	// Schema creates, one statement each: schema_migrations holding one row; authors
	// (id counting up by itself, name); books (id counting up by itself, author_id referencing
	// authors, title); tags (id counting up by itself, name, parent_id referencing tags).
	Schema []string
	// Leases is whether the dialect supports WithTemplate; if not, the suite checks it is refused.
	Leases bool
}

func (e Env) migrate(ctx context.Context, db *sql.DB) error {
	for _, statement := range e.Schema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// fresh returns a cleaner over an empty database at the baseline schema, and a second
// connection to it: what another process sees.
func (e Env) fresh(t *testing.T, opts ...dbcleaner.Option) (*dbcleaner.Cleaner, *sql.DB) {
	t.Helper()
	url := e.Fresh(t)
	all := append(append([]dbcleaner.Option{dbcleaner.WithSetup(e.migrate)}, e.Options...), opts...)
	c := dbcleaner.New(url, all...)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	other, err := sql.Open(e.Driver, url)
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

func exec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), query)
	require.NoError(t, err)
}

// fill writes a row into every table, with a foreign key between the tables, and a tag that
// references another tag.
func fill(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO authors (name) VALUES ('Le Guin')`)
	exec(t, db, `INSERT INTO books (author_id, title) VALUES (1, 'Earthsea')`)
	exec(t, db, `INSERT INTO tags (name) VALUES ('fiction')`)
	exec(t, db, `INSERT INTO tags (name, parent_id) VALUES ('fantasy', 1)`)
}

// Run runs the suite.
func Run(t *testing.T, env Env) {
	t.Run("Transaction rolls the test back", func(t *testing.T) {
		c, other := env.fresh(t)
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		fill(t, s.DB())
		require.Equal(t, 1, count(t, s.DB(), "books"))
		require.Zero(t, count(t, other, "books"), "another connection does not see it")
		require.NoError(t, s.Clean(t.Context()))
		for _, table := range []string{"authors", "books", "tags"} {
			require.Zero(t, count(t, other, table), table)
		}
	})

	t.Run("Truncation empties linked tables, resets counters and keeps migrations", func(t *testing.T) {
		c, other := env.fresh(t, dbcleaner.WithStrategy(dbcleaner.Truncation()))
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		fill(t, s.DB())
		require.Equal(t, 1, count(t, other, "books"), "rows are committed")

		require.NoError(t, s.Clean(t.Context()))
		for _, table := range []string{"authors", "books", "tags"} {
			require.Zero(t, count(t, other, table), table)
		}
		require.Equal(t, 1, count(t, other, "schema_migrations"))

		exec(t, other, `INSERT INTO authors (name) VALUES ('Butler')`)
		var id int
		require.NoError(t, other.QueryRowContext(t.Context(), `SELECT id FROM authors`).Scan(&id))
		require.Equal(t, 1, id, "the counter starts over")
	})

	t.Run("Truncation leaves foreign keys enforced", func(t *testing.T) {
		c, _ := env.fresh(t)
		require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation()))
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		_, err = s.DB().ExecContext(t.Context(), `INSERT INTO books (author_id, title) VALUES (42, 'Orphan')`)
		require.Error(t, err)
		require.NoError(t, s.Clean(t.Context()))
	})

	t.Run("Truncation honours Only and Except", func(t *testing.T) {
		c, other := env.fresh(t)
		require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Null())) // runs the setup
		fill(t, other)
		require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation(dbcleaner.Except("tags"))))
		require.Zero(t, count(t, other, "books"))
		require.Zero(t, count(t, other, "authors"))
		require.Equal(t, 2, count(t, other, "tags"))
	})

	t.Run("Truncation honours Only", func(t *testing.T) {
		c, other := env.fresh(t)
		require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Null())) // runs the setup
		fill(t, other)
		require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation(dbcleaner.Only("tags"))))
		require.Zero(t, count(t, other, "tags"))
		require.Equal(t, 1, count(t, other, "books"))
	})

	t.Run("Deletion goes children first, self-references included", func(t *testing.T) {
		c, other := env.fresh(t, dbcleaner.WithStrategy(dbcleaner.Deletion()))
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		fill(t, s.DB())
		require.NoError(t, s.Clean(t.Context()))
		for _, table := range []string{"authors", "books", "tags"} {
			require.Zero(t, count(t, other, table), table)
		}
		require.Equal(t, 1, count(t, other, "schema_migrations"))
	})

	t.Run("the guard sees the database name", func(t *testing.T) {
		var seen string
		c, _ := env.fresh(t, dbcleaner.WithStrategy(dbcleaner.Truncation()),
			dbcleaner.WithGuard(func(name string) bool { seen = name; return false }))
		_, err := c.Start(t.Context())
		require.ErrorContains(t, err, "refusing to wipe database")
		require.NotEmpty(t, seen)
	})

	t.Run("Null cleans nothing", func(t *testing.T) {
		c, other := env.fresh(t, dbcleaner.WithStrategy(dbcleaner.Null()))
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		fill(t, s.DB())
		require.NoError(t, s.Clean(t.Context()))
		require.Equal(t, 1, count(t, other, "books"))
	})

	// A dialect that leases is tested for it by its own tests, which know what it leaves behind.
	if !env.Leases {
		t.Run("WithTemplate is refused", func(t *testing.T) {
			build := func(ctx context.Context, db *sql.DB, _ string) error { return env.migrate(ctx, db) }
			c := dbcleaner.New(env.Fresh(t), append(env.Options, dbcleaner.WithTemplate("v1", build))...)
			t.Cleanup(func() { _ = c.Close() })
			_, err := c.Start(t.Context())
			require.ErrorContains(t, err, "WithTemplate is not supported")
		})
	}
}
