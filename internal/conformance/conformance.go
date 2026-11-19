// Package conformance is the test suite every dialect has to pass: the same behaviour of the
// strategies and the guard, on whatever engine the dialect is for.
package conformance

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mokevnin/dbcleaner/v2"
	"github.com/stretchr/testify/require"
)

// Env is one engine as the suite needs it.
type Env struct {
	// Dialect is the engine's.
	Dialect dbcleaner.Dialect
	// Fresh returns the URL of an empty database that is dropped when the test ends.
	Fresh func(t *testing.T) string
	// Schema creates, one statement each: schema_migrations holding one row; authors
	// (id counting up by itself, name); books (id counting up by itself, author_id referencing
	// authors, title); tags (id counting up by itself, name, parent_id referencing tags).
	Schema []string
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
	c := dbcleaner.New(e.Dialect, url, append([]dbcleaner.Option{dbcleaner.WithSetup(e.migrate)}, opts...)...)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	other, err := sql.Open(e.Dialect.Driver(), url)
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

	t.Run("committing sessions take turns", func(t *testing.T) { env.runTurns(t) })
}

// waitBriefly is how long a session that must wait is watched for not starting.
const waitBriefly = 300 * time.Millisecond

// started starts a session in the background; the channel gets it once it starts.
func started(ctx context.Context, c *dbcleaner.Cleaner) <-chan *dbcleaner.Session {
	done := make(chan *dbcleaner.Session, 1)
	go func() {
		s, err := c.Start(ctx)
		if err != nil {
			s = nil
		}
		done <- s
	}()
	return done
}

// requireWaits checks that the session has not started yet, then that it starts once
// release runs, and cleans it.
func requireWaits(t *testing.T, next <-chan *dbcleaner.Session, release func() error) {
	t.Helper()
	select {
	case <-next:
		t.Fatal("a second committing session started while the first was open")
	case <-time.After(waitBriefly):
	}
	require.NoError(t, release())
	select {
	case s := <-next:
		require.NotNil(t, s, "the second session failed to start")
		require.NoError(t, s.Clean(t.Context()))
	case <-time.After(10 * time.Second):
		t.Fatal("the second session did not start once the first was cleaned")
	}
}

func (e Env) runTurns(t *testing.T) {
	// another is a second Cleaner on the same database, as another test process has.
	another := func(t *testing.T, url string, opts ...dbcleaner.Option) *dbcleaner.Cleaner {
		c := dbcleaner.New(e.Dialect, url, opts...)
		t.Cleanup(func() { require.NoError(t, c.Close()) })
		return c
	}
	open := func(t *testing.T, strategy dbcleaner.Strategy) (*dbcleaner.Cleaner, string, *dbcleaner.Session) {
		url := e.Fresh(t)
		c := dbcleaner.New(e.Dialect, url, dbcleaner.WithSetup(e.migrate), dbcleaner.WithStrategy(strategy))
		t.Cleanup(func() { require.NoError(t, c.Close()) })
		s, err := c.Start(t.Context())
		require.NoError(t, err)
		return c, url, s
	}
	clean := func(t *testing.T, s *dbcleaner.Session) func() error {
		return func() error { return s.Clean(t.Context()) }
	}

	for name, strategy := range map[string]dbcleaner.Strategy{
		"Truncation": dbcleaner.Truncation(), "Deletion": dbcleaner.Deletion(), "Null": dbcleaner.Null(),
	} {
		t.Run(name+" waits for another Cleaner's session", func(t *testing.T) {
			_, url, first := open(t, dbcleaner.Truncation())
			second := another(t, url, dbcleaner.WithStrategy(strategy))
			requireWaits(t, started(t.Context(), second), clean(t, first))
		})
	}

	t.Run("one Cleaner's sessions wait for each other", func(t *testing.T) {
		c, _, first := open(t, dbcleaner.Deletion())
		requireWaits(t, started(t.Context(), c), clean(t, first))
	})

	t.Run("a wait ends with its context and leaves nothing held", func(t *testing.T) {
		c, url, first := open(t, dbcleaner.Truncation())
		ctx, cancel := context.WithTimeout(t.Context(), waitBriefly)
		defer cancel()
		_, err := another(t, url, dbcleaner.WithStrategy(dbcleaner.Truncation())).Start(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		ctx, cancel = context.WithTimeout(t.Context(), waitBriefly)
		defer cancel()
		_, err = c.Start(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)

		requireWaits(t, started(t.Context(), another(t, url, dbcleaner.WithStrategy(dbcleaner.Truncation()))), clean(t, first))
	})

	t.Run("a setup waits for an open committing session", func(t *testing.T) {
		_, url, first := open(t, dbcleaner.Truncation())
		var ran atomic.Bool
		second := another(t, url, dbcleaner.WithSetup(func(context.Context, *sql.DB) error { ran.Store(true); return nil }))
		requireWaits(t, started(t.Context(), second), func() error {
			require.False(t, ran.Load(), "the setup ran while a committing session was open")
			return first.Clean(t.Context())
		})
		require.True(t, ran.Load())
	})

	t.Run("Transaction does not wait once set up", func(t *testing.T) {
		_, url, first := open(t, dbcleaner.Truncation())
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		s, err := another(t, url).Start(ctx)
		require.NoError(t, err)
		require.NoError(t, s.Clean(t.Context()))
		require.NoError(t, first.Clean(t.Context()))
	})

	t.Run("a refused session holds nothing", func(t *testing.T) {
		url := e.Fresh(t)
		refused := another(t, url, dbcleaner.WithSetup(e.migrate), dbcleaner.WithStrategy(dbcleaner.Truncation()),
			dbcleaner.WithGuard(func(string) bool { return false }))
		_, err := refused.Start(t.Context())
		require.ErrorContains(t, err, "refusing to wipe database")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		s, err := another(t, url, dbcleaner.WithStrategy(dbcleaner.Truncation())).Start(ctx)
		require.NoError(t, err)
		require.NoError(t, s.Clean(t.Context()))
	})

	t.Run("a process that dies lets go of the database", func(t *testing.T) { e.runDyingHolder(t) })

	t.Run("Close lets go of a session never cleaned", func(t *testing.T) {
		c, url, _ := open(t, dbcleaner.Null())
		next := started(t.Context(), another(t, url, dbcleaner.WithStrategy(dbcleaner.Null())))
		requireWaits(t, next, c.Close)
	})
}

// holderEnv carries the database's URL to the child process of runDyingHolder.
const holderEnv = "DBCLEANER_CONFORMANCE_HOLDER_URL"

// heldMarker is what that child prints once its session holds the database.
const heldMarker = "dbcleaner-conformance: held"

// runDyingHolder starts this test binary again, running only this test, as another test
// process holding a committing session; kills it with no chance to clean up; and checks that
// a session here waits for it while it lives and starts once it is dead.
func (e Env) runDyingHolder(t *testing.T) {
	if url := os.Getenv(holderEnv); url != "" {
		// The child: hold the database until killed.
		c := dbcleaner.New(e.Dialect, url, dbcleaner.WithStrategy(dbcleaner.Truncation()))
		_, err := c.Start(t.Context())
		require.NoError(t, err)
		fmt.Println(heldMarker)
		_, _ = io.Copy(io.Discard, os.Stdin) // the parent never closes it
		return
	}

	url := e.Fresh(t)
	setUp := dbcleaner.New(e.Dialect, url, dbcleaner.WithSetup(e.migrate))
	require.NoError(t, setUp.CleanWith(t.Context(), dbcleaner.Null()))
	require.NoError(t, setUp.Close())

	cmd := osexec.CommandContext(t.Context(), os.Args[0], "-test.run="+runPattern(t.Name()), "-test.count=1")
	cmd.Env = append(os.Environ(), holderEnv+"="+url)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	dead := false
	kill := func() error {
		if dead {
			return nil
		}
		dead = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	t.Cleanup(func() { _ = kill() })

	held := make(chan error, 1)
	go func() {
		var out strings.Builder
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			if lines.Text() == heldMarker {
				held <- nil
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
			out.WriteString(lines.Text() + "\n")
		}
		held <- fmt.Errorf("the child ended before it held the database:\n%s", out.String())
	}()
	// stderr is read only once the child is gone: until then os/exec writes to it.
	select {
	case err := <-held:
		if err != nil {
			_ = kill()
			t.Fatalf("%v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		_ = kill()
		t.Fatalf("the child did not hold the database in time; stderr:\n%s", stderr.String())
	}

	here := dbcleaner.New(e.Dialect, url, dbcleaner.WithStrategy(dbcleaner.Truncation()))
	t.Cleanup(func() { require.NoError(t, here.Close()) })
	requireWaits(t, started(t.Context(), here), kill)
}

// runPattern is the -test.run pattern that runs only the test called name.
func runPattern(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = "^" + regexp.QuoteMeta(part) + "$"
	}
	return strings.Join(parts, "/")
}
