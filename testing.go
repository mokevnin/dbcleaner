package dbcleaner

import (
	"context"
	"database/sql"
	"testing"
)

// DB starts a session for the test and cleans it when the test ends. A failure to
// start or to clean fails the test.
func (c *Cleaner) DB(t testing.TB) *sql.DB {
	t.Helper()
	s, err := c.Start(t.Context())
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
