package dbcleaner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sync/semaphore"
)

// turns is whose turn it is to commit, per database, in this process: every Cleaner on one
// database shares its turn, so that a test holding a committing session is known to all of
// them. Across processes the dialect's Lock does the same job; the turn keeps a waiting
// session in this process from holding a connection while it waits, and names the holder.
type turns struct {
	sem    *semaphore.Weighted
	mu     sync.Mutex
	holder string // the test whose session holds the turn, if a test's does
}

var (
	turnsMu    sync.Mutex
	turnsByKey = map[string]*turns{}
)

// turnsOf is the turn of the database the driver opens url as.
func turnsOf(driver, url string) *turns {
	turnsMu.Lock()
	defer turnsMu.Unlock()
	key := driver + "\x00" + url
	t, ok := turnsByKey[key]
	if !ok {
		t = &turns{sem: semaphore.NewWeighted(1)}
		turnsByKey[key] = t
	}
	return t
}

// refuseNested fails a request from owner (a test's name, or "") that would wait for a turn
// owner's own test, or one it runs under, holds until it ends: that would be forever.
func (t *turns) refuseNested(owner string) error {
	t.mu.Lock()
	holder := t.holder
	t.mu.Unlock()
	if owner == "" || holder == "" || (owner != holder && !strings.HasPrefix(owner, holder+"/")) {
		return nil
	}
	return fmt.Errorf("dbcleaner: %s waits for the database while %s commits to it until it ends: it would wait forever", owner, holder)
}

// take waits for the turn and then for the database's lock (see [Dialect.Lock]) on db, for
// owner. give gives both back.
func (t *turns) take(ctx context.Context, d Dialect, db *sql.DB, owner string) (give func() error, err error) {
	if err := t.refuseNested(owner); err != nil {
		return nil, err
	}
	if err := t.sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("dbcleaner: wait for what commits to the database to end: %w", err)
	}
	unlock, err := d.Lock(ctx, db)
	if err != nil {
		t.sem.Release(1)
		return nil, fmt.Errorf("dbcleaner: lock the database: %w", err)
	}
	t.mu.Lock()
	t.holder = owner
	t.mu.Unlock()
	return func() error {
		t.mu.Lock()
		t.holder = ""
		t.mu.Unlock()
		err := unlock()
		t.sem.Release(1)
		if err != nil {
			return fmt.Errorf("dbcleaner: unlock the database: %w", err)
		}
		return nil
	}, nil
}

// lockSession takes the database's turn for a session of a committing strategy that owner
// asks for. unlock may be called more than once, and [Cleaner.Close] calls it for a session
// that was never cleaned.
func (c *Cleaner) lockSession(ctx context.Context, owner string) (unlock func() error, err error) {
	db, err := c.lockPool()
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: open: %w", err)
	}
	give, err := c.turns.take(ctx, c.dialect, db, owner)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	var unlockErr error
	unlock = func() error {
		once.Do(func() {
			c.heldMu.Lock()
			delete(c.held, &once)
			c.heldMu.Unlock()
			unlockErr = give()
		})
		return unlockErr
	}
	c.heldMu.Lock()
	c.held[&once] = unlock
	c.heldMu.Unlock()
	return unlock, nil
}

// lockPool is the pool the database's lock is taken on. It is not the committing pool:
// that one is the tests' own, and a test that limits its connections must not find one
// of them taken by the lock.
func (c *Cleaner) lockPool() (*sql.DB, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lockDB != nil {
		return c.lockDB, nil
	}
	db, err := sql.Open(c.driver, c.url)
	if err != nil {
		return nil, err
	}
	c.lockDB = db
	return db, nil
}

// unlockAll gives back the locks of the sessions that were never cleaned.
func (c *Cleaner) unlockAll() error {
	c.heldMu.Lock()
	unlocks := make([]func() error, 0, len(c.held))
	for _, unlock := range c.held {
		unlocks = append(unlocks, unlock)
	}
	c.heldMu.Unlock()
	var errs []error
	for _, unlock := range unlocks {
		errs = append(errs, unlock())
	}
	return errors.Join(errs...)
}
