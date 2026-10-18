package pgcleaner

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// DB starts a session for the test and cleans it when the test ends. A failure to
// start or to clean fails the test.
func (c *Cleaner) DB(t testing.TB) *sql.DB {
	t.Helper()
	s, err := c.Start(t.Context())
	if err != nil {
		t.Fatalf("pgcleaner: %v", err)
	}
	t.Cleanup(func() {
		// The test's context is already cancelled when cleanups run.
		if err := s.Clean(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("pgcleaner: clean: %v", err)
		}
	})
	return s.DB()
}

var (
	scratchCounter atomic.Uint64
	unsafeName     = regexp.MustCompile(`[^a-z0-9]+`)
)

// ScratchDatabase creates an empty, uniquely named database on the server behind
// adminURL and returns its URL; the database is dropped when the test ends. It is
// for code that needs a database with no schema at all (migrations on a fresh
// instance), which a pre-migrated one cannot give. The role in adminURL must be able
// to create databases.
func ScratchDatabase(t testing.TB, adminURL string) string {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())

	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("pgcleaner: open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// Readable prefix from the test's name, then a hash that also covers the process
	// and a counter: packages run in parallel, and a test may ask for several.
	readable := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(t.Name()), "_"), "_")
	if len(readable) > 24 {
		readable = readable[:24]
	}
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%s/%d/%d", t.Name(), os.Getpid(), scratchCounter.Add(1))
	name := fmt.Sprintf("scratch_%s_%08x", readable, h.Sum32())

	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quote(name)); err != nil {
		t.Fatalf("pgcleaner: create scratch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quote(name)+` WITH (FORCE)`); err != nil {
			t.Errorf("pgcleaner: drop scratch database %s: %v", name, err)
		}
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("pgcleaner: parse admin url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
