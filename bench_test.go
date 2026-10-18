package pgcleaner_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/mokevnin/pgcleaner"
	"github.com/stretchr/testify/require"
)

// tableCount is the size of the schema the benchmarks run on: a mid-sized application.
const tableCount = 60

func wideSchema(ctx context.Context, db *sql.DB) error {
	for i := range tableCount {
		query := fmt.Sprintf(`CREATE TABLE t%d (id serial PRIMARY KEY, name text NOT NULL, owner_id int)`, i)
		if i > 0 {
			query = fmt.Sprintf(`CREATE TABLE t%d (id serial PRIMARY KEY, name text NOT NULL, owner_id int REFERENCES t%d (id))`, i, i-1)
		}
		if _, err := db.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

// One test's whole lifecycle against a baseline: get a database, write two rows into
// one table, undo it.
func benchmarkStrategy(b *testing.B, strategy pgcleaner.Strategy) {
	url := pgcleaner.ScratchDatabase(b, serverURL(b))
	c := pgcleaner.New(url, pgcleaner.WithStrategy(strategy), pgcleaner.WithSetup(wideSchema))
	b.Cleanup(func() { require.NoError(b, c.Close()) })
	require.NoError(b, c.CleanWith(b.Context(), pgcleaner.Null()), "run the setup before timing")

	b.ResetTimer()
	for range b.N {
		s, err := c.Start(b.Context())
		require.NoError(b, err)
		_, err = s.DB().ExecContext(b.Context(), `INSERT INTO t0 (name) VALUES ('a'), ('b')`)
		require.NoError(b, err)
		require.NoError(b, s.Clean(b.Context()))
	}
}

func BenchmarkTransaction(b *testing.B) { benchmarkStrategy(b, pgcleaner.Transaction()) }
func BenchmarkTruncation(b *testing.B)  { benchmarkStrategy(b, pgcleaner.Truncation()) }
func BenchmarkDeletion(b *testing.B)    { benchmarkStrategy(b, pgcleaner.Deletion()) }

// The same lifecycle with a database per test, cloned from a migrated template.
func BenchmarkTemplateDatabase(b *testing.B) {
	server := serverURL(b)
	templateURL := pgcleaner.ScratchDatabase(b, server)
	template := pgcleaner.New(templateURL, pgcleaner.WithSetup(wideSchema))
	require.NoError(b, template.CleanWith(b.Context(), pgcleaner.Null()))
	require.NoError(b, template.Close())

	admin, err := sql.Open("pgx", server)
	require.NoError(b, err)
	b.Cleanup(func() { _ = admin.Close() })
	templateName := databaseName(b, templateURL)

	b.ResetTimer()
	for i := range b.N {
		name := fmt.Sprintf("%s_clone_%d", templateName, i)
		_, err := admin.ExecContext(b.Context(), fmt.Sprintf(`CREATE DATABASE %q TEMPLATE %q`, name, templateName))
		require.NoError(b, err)

		db, err := sql.Open("pgx", replaceDatabase(b, server, name))
		require.NoError(b, err)
		_, err = db.ExecContext(b.Context(), `INSERT INTO t0 (name) VALUES ('a'), ('b')`)
		require.NoError(b, err)
		require.NoError(b, db.Close())
		_, err = admin.ExecContext(b.Context(), fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, name))
		require.NoError(b, err)
	}
}
