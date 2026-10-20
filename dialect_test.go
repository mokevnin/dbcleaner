package dbcleaner_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mokevnin/dbcleaner"
	"github.com/mokevnin/dbcleaner/postgres"
	"github.com/stretchr/testify/require"
)

func TestAURLOfAnUnknownSchemeNamesTheMissingDialect(t *testing.T) {
	_, err := dbcleaner.New("mysql://root@127.0.0.1/app_test").Start(t.Context())
	require.ErrorContains(t, err, `no dialect for the scheme "mysql"`)
}

func TestADSNWithoutASchemeNeedsWithDialect(t *testing.T) {
	_, err := dbcleaner.New("host=127.0.0.1 dbname=app_test").Start(t.Context())
	require.ErrorContains(t, err, "WithDialect")
}

// plain hides the Leaser side of the PostgreSQL dialect.
type plain struct{ dbcleaner.Dialect }

func TestWithTemplateNeedsADialectThatCanLease(t *testing.T) {
	c := dbcleaner.New("postgres://postgres@127.0.0.1/app_test",
		dbcleaner.WithDialect(plain{postgres.Dialect()}),
		dbcleaner.WithTemplate("v1", func(context.Context, *sql.DB, string) error { return nil }))
	_, err := c.Start(t.Context())
	require.ErrorContains(t, err, "WithTemplate is not supported")
}

// recording counts what the strategies ask of the dialect.
type recording struct {
	dbcleaner.Dialect
	truncated [][]string
}

func (r *recording) Truncate(ctx context.Context, db *sql.DB, tables []string) error {
	r.truncated = append(r.truncated, tables)
	return r.Dialect.Truncate(ctx, db, tables)
}

func TestTruncationAsksTheDialectToTruncate(t *testing.T) {
	d := &recording{Dialect: postgres.Dialect()}
	c, _ := fresh(t, dbcleaner.Truncation(), dbcleaner.WithDialect(d))
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation()))
	require.Equal(t, [][]string{{"authors", "books", "tags"}}, d.truncated)
}
