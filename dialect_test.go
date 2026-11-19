package dbcleaner_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mokevnin/dbcleaner/v2"
	"github.com/mokevnin/dbcleaner/v2/postgres"
	"github.com/stretchr/testify/require"
)

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
	c, _ := freshWith(t, d, dbcleaner.Truncation())
	require.NoError(t, c.CleanWith(t.Context(), dbcleaner.Truncation()))
	require.Equal(t, [][]string{{"authors", "books", "tags"}}, d.truncated)
}
