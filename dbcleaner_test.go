package dbcleaner_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/mokevnin/dbcleaner"
	"github.com/stretchr/testify/require"
)

// The suite needs a PostgreSQL server whose role can create databases; every test
// gets its own scratch database on it.
func serverURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("DBCLEANER_TEST_URL")
	require.NotEmpty(t, url, "set DBCLEANER_TEST_URL to a PostgreSQL URL, e.g. postgres://postgres@127.0.0.1:5432/postgres")
	return url
}

const baselineSchema = `
	CREATE TABLE schema_migrations (version text PRIMARY KEY);
	INSERT INTO schema_migrations VALUES ('1');
	CREATE TABLE authors (id serial PRIMARY KEY, name text NOT NULL);
	CREATE TABLE books (id serial PRIMARY KEY, author_id int NOT NULL REFERENCES authors (id), title text NOT NULL);
	CREATE TABLE tags (id serial PRIMARY KEY, name text NOT NULL);
`

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, baselineSchema)
	return err
}

// fresh returns a cleaner over a scratch database already at the baseline schema,
// and a second, plain connection to the same database: what another process sees.
func fresh(t *testing.T, strategy dbcleaner.Strategy, opts ...dbcleaner.Option) (*dbcleaner.Cleaner, *sql.DB) {
	t.Helper()
	url := dbcleaner.ScratchDatabase(t, serverURL(t))
	opts = append([]dbcleaner.Option{dbcleaner.WithStrategy(strategy), dbcleaner.WithSetup(migrate)}, opts...)
	c := dbcleaner.New(url, opts...)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	other, err := sql.Open("pgx", url)
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

func start(t *testing.T, c *dbcleaner.Cleaner) *dbcleaner.Session {
	t.Helper()
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	return s
}

func TestTransactionRollsBackEverythingTheSessionDid(t *testing.T) {
	c, other := fresh(t, dbcleaner.Transaction())
	s := start(t, c)
	db := s.DB()

	exec(t, db, `INSERT INTO authors (name) VALUES ('Ann')`)
	require.Equal(t, 1, count(t, db, "authors"))
	require.Zero(t, count(t, other, "authors"), "the session's rows are not committed")

	// The code under test opens its own transactions: they ride savepoints.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO authors (name) VALUES ('Bob')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, 2, count(t, db, "authors"))

	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO authors (name) VALUES ('Cid')`)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.Equal(t, 2, count(t, db, "authors"))

	require.NoError(t, s.Clean(t.Context()))
	require.Zero(t, count(t, other, "authors"))
}

func TestTransactionSessionsDoNotSeeEachOther(t *testing.T) {
	c, _ := fresh(t, dbcleaner.Transaction())
	a, b := start(t, c), start(t, c)

	exec(t, a.DB(), `INSERT INTO authors (name) VALUES ('Ann')`)

	require.Equal(t, 1, count(t, a.DB(), "authors"))
	require.Zero(t, count(t, b.DB(), "authors"))
	require.NoError(t, a.Clean(t.Context()))
	require.NoError(t, b.Clean(t.Context()))
}

func TestTruncationCommitsThenEmptiesTablesAndRestartsIdentity(t *testing.T) {
	c, other := fresh(t, dbcleaner.Truncation())
	s := start(t, c)

	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Ann')`)
	require.Equal(t, 1, count(t, other, "authors"), "rows are committed, visible to another connection")

	require.NoError(t, s.Clean(t.Context()))
	require.Zero(t, count(t, other, "authors"))

	var id int
	require.NoError(t, other.QueryRowContext(t.Context(), `INSERT INTO authors (name) VALUES ('Bob') RETURNING id`).Scan(&id))
	require.Equal(t, 1, id)
}

func TestTruncationNeverTouchesMigrationTables(t *testing.T) {
	c, other := fresh(t, dbcleaner.Truncation())
	s := start(t, c)

	require.NoError(t, s.Clean(t.Context()))

	require.Equal(t, 1, count(t, other, "schema_migrations"))
}

func TestTruncationHonorsOnlyAndExcept(t *testing.T) {
	for name, tc := range map[string]struct {
		strategy      dbcleaner.Strategy
		authors, tags int
	}{
		"only":   {dbcleaner.Truncation(dbcleaner.Only("tags")), 1, 0},
		"except": {dbcleaner.Truncation(dbcleaner.Except("tags")), 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			c, other := fresh(t, tc.strategy)
			s := start(t, c)
			exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Ann')`)
			exec(t, s.DB(), `INSERT INTO tags (name) VALUES ('go')`)

			require.NoError(t, s.Clean(t.Context()))

			require.Equal(t, tc.authors, count(t, other, "authors"))
			require.Equal(t, tc.tags, count(t, other, "tags"))
		})
	}
}

func TestDeletionRemovesChildrenBeforeParentsAndKeepsSequences(t *testing.T) {
	c, other := fresh(t, dbcleaner.Deletion())
	s := start(t, c)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Ann')`)
	exec(t, s.DB(), `INSERT INTO books (author_id, title) SELECT id, 'Go' FROM authors`)

	require.NoError(t, s.Clean(t.Context()))

	require.Zero(t, count(t, other, "books"))
	require.Zero(t, count(t, other, "authors"))
	require.Equal(t, 1, count(t, other, "schema_migrations"))
	var id int
	require.NoError(t, other.QueryRowContext(t.Context(), `INSERT INTO authors (name) VALUES ('Bob') RETURNING id`).Scan(&id))
	require.Equal(t, 2, id, "DELETE leaves the sequence where it was")
}

func TestDeletionRefusesAForeignKeyCycle(t *testing.T) {
	c, _ := fresh(t, dbcleaner.Deletion(), dbcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			CREATE TABLE a (id int PRIMARY KEY, b_id int);
			CREATE TABLE b (id int PRIMARY KEY, a_id int REFERENCES a (id));
			ALTER TABLE a ADD FOREIGN KEY (b_id) REFERENCES b (id);`)
		return err
	}))
	s := start(t, c)

	err := s.Clean(t.Context())

	require.ErrorContains(t, err, "cycle")
}

func TestNullCleansNothing(t *testing.T) {
	c, other := fresh(t, dbcleaner.Null())
	s := start(t, c)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Ann')`)

	require.NoError(t, s.Clean(t.Context()))

	require.Equal(t, 1, count(t, other, "authors"))
}

func TestCleanWithRunsAnotherStrategyOnce(t *testing.T) {
	c, other := fresh(t, dbcleaner.Transaction())
	// Run the setup first: the schema does not exist before it.
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Null()))
	exec(t, other, `INSERT INTO authors (name) VALUES ('Left behind by a crashed run')`)

	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation()))

	require.Zero(t, count(t, other, "authors"))
}

func TestCleaningCleansWhenTheBodyFails(t *testing.T) {
	c, other := fresh(t, dbcleaner.Truncation())
	boom := errors.New("boom")

	err := c.Cleaning(t.Context(), func(db *sql.DB) error {
		exec(t, db, `INSERT INTO authors (name) VALUES ('Ann')`)
		return boom
	})

	require.ErrorIs(t, err, boom)
	require.Zero(t, count(t, other, "authors"))
}

func TestSetupRunsOncePerCleanerAndItsErrorFailsEverySession(t *testing.T) {
	t.Run("once", func(t *testing.T) {
		// The setup inserts a primary-keyed row: a second run would violate it.
		c, other := fresh(t, dbcleaner.Transaction(), dbcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
			if err := migrate(ctx, db); err != nil {
				return err
			}
			_, err := db.ExecContext(ctx, `INSERT INTO tags (id, name) VALUES (7, 'baseline')`)
			return err
		}))
		for range 3 {
			require.NoError(t, start(t, c).Clean(t.Context()))
		}
		require.Equal(t, 1, count(t, other, "tags"))
	})

	t.Run("failure", func(t *testing.T) {
		boom := errors.New("migration failed")
		c, _ := fresh(t, dbcleaner.Transaction(), dbcleaner.WithSetup(func(context.Context, *sql.DB) error { return boom }))
		for range 2 {
			_, err := c.Start(t.Context())
			require.ErrorIs(t, err, boom)
		}
	})
}

func TestTheGuardRefusesToWipeADatabaseThatDoesNotLookLikeATestOne(t *testing.T) {
	admin, err := sql.Open("pgx", serverURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	name := fmt.Sprintf("dbcleaner_keepme_%d", os.Getpid())
	exec(t, admin, `CREATE DATABASE `+name)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.WithoutCancel(t.Context()), `DROP DATABASE `+name+` WITH (FORCE)`)
	})
	url := replaceDatabase(t, serverURL(t), name)

	for _, strategy := range []dbcleaner.Strategy{dbcleaner.Truncation(), dbcleaner.Deletion()} {
		_, err := dbcleaner.New(url).Start(t.Context())
		require.NoError(t, err, "the transaction strategy wipes nothing")

		_, err = dbcleaner.New(url, dbcleaner.WithStrategy(strategy)).Start(t.Context())
		require.ErrorContains(t, err, "refusing to wipe")

		c := dbcleaner.New(url, dbcleaner.WithStrategy(strategy), dbcleaner.WithGuard(nil))
		_, err = c.Start(t.Context())
		require.NoError(t, err, "a nil guard allows it")
		require.NoError(t, c.Close())
	}
}

func TestDBCleansWhenTheTestEnds(t *testing.T) {
	c, other := fresh(t, dbcleaner.Transaction())

	t.Run("dirties the database", func(t *testing.T) {
		exec(t, c.DB(t), `INSERT INTO authors (name) VALUES ('Ann')`)
		require.Zero(t, count(t, other, "authors"))
	})

	require.Zero(t, count(t, other, "authors"))
	// And the next test starts from the baseline again.
	require.Zero(t, count(t, c.DB(t), "authors"))
}

func TestScratchDatabaseIsEmptyUniqueAndDroppedAfterTheTest(t *testing.T) {
	admin, err := sql.Open("pgx", serverURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	var first, second string
	t.Run("creates", func(t *testing.T) {
		first = dbcleaner.ScratchDatabase(t, serverURL(t))
		second = dbcleaner.ScratchDatabase(t, serverURL(t))
		require.NotEqual(t, first, second)

		db, err := sql.Open("pgx", first)
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		var tables int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables))
		require.Zero(t, tables)
	})

	for _, url := range []string{first, second} {
		db, err := sql.Open("pgx", url)
		require.NoError(t, err)
		require.Error(t, db.PingContext(t.Context()), "the database is gone after its test")
		_ = db.Close()
	}
}
