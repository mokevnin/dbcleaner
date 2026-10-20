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
	"github.com/mokevnin/dbcleaner/internal/conformance"
	"github.com/mokevnin/dbcleaner/mysql"
	"github.com/stretchr/testify/require"
)

// The suite needs a MySQL server whose root can create databases, e.g.
// DBCLEANER_MYSQL_TEST_URL='root:root@tcp(127.0.0.1:3306)/'.
func serverDSN(t testing.TB) *gomysql.Config {
	t.Helper()
	raw := os.Getenv("DBCLEANER_MYSQL_TEST_URL")
	require.NotEmpty(t, raw, "set DBCLEANER_MYSQL_TEST_URL to a MySQL DSN, e.g. root:root@tcp(127.0.0.1:3306)/")
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

var schema = []string{
	`CREATE TABLE schema_migrations (version varchar(20) PRIMARY KEY)`,
	`INSERT INTO schema_migrations VALUES ('1')`,
	`CREATE TABLE authors (id int AUTO_INCREMENT PRIMARY KEY, name text NOT NULL)`,
	`CREATE TABLE books (id int AUTO_INCREMENT PRIMARY KEY, author_id int NOT NULL, title text NOT NULL,
		FOREIGN KEY (author_id) REFERENCES authors (id))`,
	`CREATE TABLE tags (id int AUTO_INCREMENT PRIMARY KEY, name text NOT NULL, parent_id int NULL,
		FOREIGN KEY (parent_id) REFERENCES tags (id))`,
}

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Env{
		Driver:  "mysql",
		Options: []dbcleaner.Option{dbcleaner.WithDialect(mysql.Dialect())},
		Fresh:   func(t *testing.T) string { return scratch(t, "app_test") },
		Schema:  schema,
	})
}

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", scratch(t, "app_test"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestTablesLeaveViewsOut(t *testing.T) {
	db := open(t)
	for _, statement := range []string{
		`CREATE TABLE plain (id int)`,
		`CREATE VIEW plain_view AS SELECT id FROM plain`,
	} {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	tables, err := mysql.Dialect().Tables(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, []string{"plain"}, tables)
}

func TestReferencesLeaveSelfReferencesOut(t *testing.T) {
	db := open(t)
	for _, statement := range schema {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	references, err := mysql.Dialect().References(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"books": {"authors"}}, references)
}

func TestTheGuardGoesByTheDatabaseName(t *testing.T) {
	c := dbcleaner.New(scratch(t, "app"), dbcleaner.WithDialect(mysql.Dialect()))
	t.Cleanup(func() { _ = c.Close() })
	require.ErrorContains(t, c.CleanWith(t.Context(), dbcleaner.Truncation()), "refusing to wipe database")
}

func TestQuote(t *testing.T) {
	require.Equal(t, "`order`", mysql.Dialect().Quote("order"))
	require.Equal(t, "`we``ird`", mysql.Dialect().Quote("we`ird"))
}
