package dbcleaner_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/mokevnin/dbcleaner"
	"github.com/stretchr/testify/require"
)

// itemsSchema is what the template builds: a schema with nothing in it.
func itemsSchema(ctx context.Context, db *sql.DB, _ string) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE items (id serial PRIMARY KEY, name text NOT NULL)`)
	return err
}

func leasing(url, version string, build func(context.Context, *sql.DB, string) error, opts ...dbcleaner.Option) *dbcleaner.Cleaner {
	return dbcleaner.New(url, append([]dbcleaner.Option{dbcleaner.WithTemplate(version, build)}, opts...)...)
}

// scratchBase is a scratch database whose leased siblings (template and workers) are
// dropped with it, and the name of that base.
func scratchBase(t *testing.T) (url, name string) {
	t.Helper()
	url = dbcleaner.ScratchDatabase(t, serverURL(t))
	name = databaseName(t, url)
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", serverURL(t))
		require.NoError(t, err)
		defer func() { _ = admin.Close() }()
		for _, sibling := range databasesLike(t, name+"\\_%") {
			_, err := admin.ExecContext(context.WithoutCancel(t.Context()), `DROP DATABASE IF EXISTS "`+sibling+`" WITH (FORCE)`)
			require.NoError(t, err)
		}
	})
	return url, name
}

func databasesLike(t *testing.T, pattern string) []string {
	t.Helper()
	admin, err := sql.Open("pgx", serverURL(t))
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	rows, err := admin.QueryContext(context.WithoutCancel(t.Context()), `SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY 1`, pattern)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

func TestProcessesStartingTogetherBuildOneTemplateAndTakeOneWorkerEach(t *testing.T) {
	base, name := scratchBase(t)
	const processes = 6

	urls := make([]string, processes)
	cleaners := make([]*dbcleaner.Cleaner, processes)
	var wg sync.WaitGroup
	for i := range processes {
		cleaners[i] = leasing(base, "v1", itemsSchema)
		wg.Go(func() {
			url, err := cleaners[i].URL(t.Context())
			require.NoError(t, err)
			urls[i] = url
		})
	}
	wg.Wait()
	for _, c := range cleaners {
		t.Cleanup(func() { require.NoError(t, c.Close()) })
	}

	require.Len(t, uniq(urls), processes, "every process has a database of its own")
	for _, c := range cleaners {
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		require.Zero(t, count(t, s.DB(), "items"), "a worker has the template's schema")
		require.NoError(t, s.Clean(t.Context()))
	}
	require.Len(t, databasesLike(t, name+"\\_tpl\\_%"), 1, "the template is built once")
}

func TestAWorkerIsFreedWhenItsProcessEndsAndTheLowestFreeOneIsReused(t *testing.T) {
	base, _ := scratchBase(t)
	first, second := leasing(base, "v1", itemsSchema), leasing(base, "v1", itemsSchema)
	firstURL, err := first.URL(t.Context())
	require.NoError(t, err)
	secondURL, err := second.URL(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, firstURL, secondURL)

	require.NoError(t, first.Close())
	third := leasing(base, "v1", itemsSchema)
	t.Cleanup(func() { require.NoError(t, third.Close()); require.NoError(t, second.Close()) })
	thirdURL, err := third.URL(t.Context())
	require.NoError(t, err)

	require.Equal(t, firstURL, thirdURL)
}

func TestANewVersionReplacesTheTemplateAndRebuildsTheWorker(t *testing.T) {
	base, name := scratchBase(t)
	old := leasing(base, "v1", itemsSchema)
	oldURL, err := old.URL(t.Context())
	require.NoError(t, err)
	require.NoError(t, old.Close())

	renamed := func(ctx context.Context, db *sql.DB, _ string) error {
		_, err := db.ExecContext(ctx, `CREATE TABLE things (id serial PRIMARY KEY)`)
		return err
	}
	next := leasing(base, "v2", renamed)
	t.Cleanup(func() { require.NoError(t, next.Close()) })
	nextURL, err := next.URL(t.Context())
	require.NoError(t, err)

	require.Equal(t, oldURL, nextURL, "the freed worker is reused, now rebuilt")
	s, err := next.Start(t.Context())
	require.NoError(t, err)
	require.Zero(t, count(t, s.DB(), "things"))
	require.NoError(t, s.Clean(t.Context()))
	require.Len(t, databasesLike(t, name+"\\_tpl\\_%"), 1, "the old template is gone")
}

func TestAFailedBuildLeavesNoTemplateAndTheNextProcessBuildsItAgain(t *testing.T) {
	base, name := scratchBase(t)
	boom := errors.New("migration failed")
	failing := leasing(base, "v1", func(context.Context, *sql.DB, string) error { return boom })

	_, err := failing.URL(t.Context())

	require.ErrorIs(t, err, boom)
	require.Empty(t, databasesLike(t, name+"\\_tpl\\_%"), "neither a template nor a half-built one")

	good := leasing(base, "v1", itemsSchema)
	t.Cleanup(func() { require.NoError(t, good.Close()) })
	_, err = good.URL(t.Context())
	require.NoError(t, err)
}

func TestSetupRunsOnTheWorkerEveryTimeNotOnTheTemplate(t *testing.T) {
	base, _ := scratchBase(t)
	seed := dbcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `TRUNCATE items; INSERT INTO items (name) VALUES ('fixture')`)
		return err
	})

	for range 2 {
		c := leasing(base, "v1", itemsSchema, seed)
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, count(t, s.DB(), "items"), "the fixtures are loaded once, not piled up")
		require.NoError(t, s.Clean(t.Context()))
		require.NoError(t, c.Close())
	}
}

// The first test of a package may start its Cleaner inside a testing/synctest bubble,
// which must end with no goroutine of its own left behind.
func TestAFirstSessionStartedInsideASynctestBubbleLeavesNothingBehind(t *testing.T) {
	base, _ := scratchBase(t)
	c := leasing(base, "v1", itemsSchema, dbcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `INSERT INTO items (name) VALUES ('fixture') ON CONFLICT DO NOTHING`)
		return err
	}))

	synctest.Test(t, func(t *testing.T) {
		db := c.DB(t)
		require.Equal(t, 1, count(t, db, "items"))
	})
	// The lease outlives the bubble, as in a test process, and is not closed from outside
	// it: a connection made inside a bubble cannot be used outside it. The scratch
	// database is dropped with FORCE, which ends the connection.
}

func uniq(values []string) map[string]bool {
	set := map[string]bool{}
	for _, v := range values {
		set[v] = true
	}
	return set
}
