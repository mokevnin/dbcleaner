package postgres

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
		t.Fatalf("dbcleaner: open admin connection: %v", err)
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

	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		t.Fatalf("dbcleaner: create scratch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			t.Errorf("dbcleaner: drop scratch database %s: %v", name, err)
		}
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("dbcleaner: parse admin url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
