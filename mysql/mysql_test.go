package mysql_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/mokevnin/dbcleaner"
	"github.com/mokevnin/dbcleaner/mysql"
	"github.com/stretchr/testify/require"
)

// The suite needs a MySQL server whose root can create databases, e.g.
// DBCLEANER_MYSQL_TEST_URL='root:root@tcp(127.0.0.1:3306)/'. Without it the tests are skipped.
func serverDSN(t testing.TB) *gomysql.Config {
	t.Helper()
	raw := os.Getenv("DBCLEANER_MYSQL_TEST_URL")
	if raw == "" {
		t.Skip("set DBCLEANER_MYSQL_TEST_URL to a MySQL DSN to run the MySQL tests")
	}
	cfg, err := gomysql.ParseDSN(raw)
	require.NoError(t, err)
	return cfg
}

var scratchCounter atomic.Uint64

// scratch creates a database called name_<n> and drops it with the test, and returns its DSN.
func scratch(t *testing.T, name string) string {
	t.Helper()
	server := serverDSN(t)
	admin, err := sql.Open("mysql", server.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	database := fmt.Sprintf("%s_%d_%d", name, os.Getpid(), scratchCounter.Add(1))
	_, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+database+"`")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP DATABASE IF EXISTS `"+database+"`")
		require.NoError(t, err)
	})
	cfg := server.Clone()
	cfg.DBName = database
	return cfg.FormatDSN()
}

func migrate(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version varchar(20) PRIMARY KEY)`,
		`INSERT INTO schema_migrations VALUES ('1')`,
		`CREATE TABLE authors (id int AUTO_INCREMENT PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE books (id int AUTO_INCREMENT PRIMARY KEY, author_id int NOT NULL, title text NOT NULL,
			FOREIGN KEY (author_id) REFERENCES authors (id))`,
		`CREATE TABLE tags (id int AUTO_INCREMENT PRIMARY KEY, name text NOT NULL, parent_id int NULL,
			FOREIGN KEY (parent_id) REFERENCES tags (id))`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// fresh returns a cleaner over a scratch database at the baseline schema, and a second
// connection to it: what another process sees.
func fresh(t *testing.T, name string, opts ...dbcleaner.Option) (*dbcleaner.Cleaner, *sql.DB) {
	t.Helper()
	dsn := scratch(t, name)
	opts = append([]dbcleaner.Option{dbcleaner.WithDialect(mysql.Dialect()), dbcleaner.WithSetup(migrate)}, opts...)
	c := dbcleaner.New(dsn, opts...)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	other, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	return c, other
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM `"+table+"`").Scan(&n))
	return n
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

func TestTransactionRollsTheTestBack(t *testing.T) {
	c, other := fresh(t, "app_test")
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Le Guin')`)
	require.Equal(t, 1, count(t, s.DB(), "authors"))
	require.NoError(t, s.Clean(t.Context()))
	require.Equal(t, 0, count(t, other, "authors"))
}

func TestTruncationEmptiesLinkedTablesResetsCountersAndKeepsMigrations(t *testing.T) {
	c, other := fresh(t, "app_test", dbcleaner.WithStrategy(dbcleaner.Truncation()))
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
	require.Equal(t, 1, id, "AUTO_INCREMENT starts over")
}

func TestTruncationRestoresForeignKeyChecks(t *testing.T) {
	c, _ := fresh(t, "app_test", dbcleaner.WithStrategy(dbcleaner.Truncation()))
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation()))
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	_, err = s.DB().ExecContext(t.Context(), `INSERT INTO books (author_id, title) VALUES (42, 'Orphan')`)
	require.Error(t, err, "the pool's connections check foreign keys again")
	require.NoError(t, s.Clean(t.Context()))
}

func TestDeletionGoesChildrenFirstAndSkipsSelfReferences(t *testing.T) {
	c, other := fresh(t, "app_test", dbcleaner.WithStrategy(dbcleaner.Deletion()))
	s, err := c.Start(t.Context())
	require.NoError(t, err)
	exec(t, s.DB(), `INSERT INTO authors (name) VALUES ('Le Guin')`)
	exec(t, s.DB(), `INSERT INTO books (author_id, title) VALUES (1, 'Earthsea')`)
	exec(t, s.DB(), `INSERT INTO tags (name, parent_id) VALUES ('root', NULL)`)
	require.NoError(t, s.Clean(t.Context()))
	require.Zero(t, count(t, other, "authors"))
	require.Zero(t, count(t, other, "books"))
}

func TestTheGuardRefusesADatabaseThatIsNotForTests(t *testing.T) {
	c, _ := fresh(t, "app", dbcleaner.WithStrategy(dbcleaner.Truncation()))
	_, err := c.Start(t.Context())
	require.ErrorContains(t, err, "refusing to wipe database")
}

func TestWithTemplateIsNotSupported(t *testing.T) {
	c := dbcleaner.New("root@tcp(127.0.0.1:3306)/app_test", dbcleaner.WithDialect(mysql.Dialect()),
		dbcleaner.WithTemplate("v1", func(context.Context, *sql.DB, string) error { return nil }))
	_, err := c.Start(t.Context())
	require.ErrorContains(t, err, "WithTemplate is not supported")
}
