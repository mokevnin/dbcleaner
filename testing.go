package dbcleaner

import (
	"context"
	"database/sql"
	"testing"
)

// DB starts a session for the test and cleans it when the test ends. A failure to
// start or to clean fails the test, and so does waiting for the database while the test, or
// one it runs under, holds a committing session on it, from this Cleaner or another: that
// session is cleaned when the test ends, so the wait would never end.
func (c *Cleaner) DB(t testing.TB) *sql.DB {
	t.Helper()
	s, err := c.start(t.Context(), c.strategy, t.Name())
	if err != nil {
		t.Fatalf("dbcleaner: %v", err)
	}
	t.Cleanup(func() {
		// The test's context is already cancelled when cleanups run.
		if err := s.Clean(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("dbcleaner: clean: %v", err)
		}
	})
	return s.DB()
}
