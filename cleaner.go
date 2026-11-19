// Package dbcleaner puts a database back to its baseline after a test,
// the way Ruby's DatabaseCleaner does: pick a strategy, start a session, clean it.
//
// Three strategies are built in:
//
//   - [Transaction] runs the whole test inside one transaction and rolls it back.
//     The fastest, but the test must use the *sql.DB it is given: another
//     connection (a server in another process, a second pool) cannot see its rows.
//   - [Truncation] commits normally and runs TRUNCATE on the tables afterwards.
//   - [Deletion] commits normally and runs DELETE on the tables afterwards, children
//     before parents.
//
// [Null] cleans nothing. The package knows nothing about an ORM: it hands out a
// *database/sql.DB and runs one setup callback (migrations, fixtures) per process.
//
// What differs between engines is a [Dialect], from the package of the engine:
//
//	dbcleaner.New(postgres.Dialect(), os.Getenv("TEST_DATABASE_URL"))
package dbcleaner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Cleaner starts cleaning sessions against one database. Build it once per test
// process (a package variable next to TestMain) and share it between tests.
type Cleaner struct {
	url      string
	driver   string
	dialect  Dialect
	strategy Strategy
	setup    func(context.Context, *sql.DB) error
	guard    func(database string) bool

	// release gives back what the dialect's Lease took.
	release   func() error
	leaseOnce sync.Once
	leaseErr  error
	turns     *turns // whose turn it is to commit to the leased database; see lock.go

	setupOnce sync.Once
	setupErr  error
	setupDone atomic.Bool

	mu     sync.Mutex
	real   *sql.DB // the shared, committing pool of the non-transactional strategies
	lockDB *sql.DB // the pool the database's lock is taken on; see Dialect.Lock
	schema *schema // table and foreign-key listing, read once

	heldMu sync.Mutex
	held   map[*sync.Once]func() error // the unlocks of the sessions not cleaned yet
}

// Option tunes a Cleaner.
type Option func(*Cleaner)

// WithStrategy sets the strategy [Cleaner.Start] and [Cleaner.DB] use. The default
// is [Transaction].
func WithStrategy(s Strategy) Option { return func(c *Cleaner) { c.strategy = s } }

// WithDriver names the database/sql driver the URL is opened with. The default is the
// dialect's own; any other driver must be imported by the caller.
func WithDriver(name string) Option { return func(c *Cleaner) { c.driver = name } }

// WithSetup registers the callback that brings the database to its baseline:
// migrate the schema, load the fixtures. It runs once per Cleaner, on a committing
// connection, before the first session starts. Its error fails every session.
func WithSetup(fn func(ctx context.Context, db *sql.DB) error) Option {
	return func(c *Cleaner) { c.setup = fn }
}

// WithGuard replaces the check that the database may be wiped, run before
// [Truncation] and [Deletion] touch it (see [DefaultGuard]). A nil guard allows every
// database.
func WithGuard(allow func(database string) bool) Option {
	return func(c *Cleaner) { c.guard = allow }
}

// DefaultGuard allows a database whose name contains "test", "e2e" or "scratch".
// Wiping a database is the one irreversible thing this package does, so a URL that
// still points at a development database is refused rather than emptied.
func DefaultGuard(database string) bool {
	name := strings.ToLower(database)
	for _, marker := range []string{"test", "e2e", "scratch"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// New returns a Cleaner for the database at url, of the engine of the dialect. Nothing
// connects until the first session starts.
func New(dialect Dialect, url string, opts ...Option) *Cleaner {
	if dialect == nil {
		panic("dbcleaner: New with a nil Dialect")
	}
	c := &Cleaner{
		dialect: dialect, url: url, strategy: Transaction(), guard: DefaultGuard,
		held: map[*sync.Once]func() error{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Close releases the shared pool the committing strategies use, the lock of a session that
// was never cleaned and the worker database lease. A Cleaner that only ran [Transaction] on
// its own database holds nothing to close.
func (c *Cleaner) Close() error {
	errs := []error{c.unlockAll()}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, db := range []**sql.DB{&c.real, &c.lockDB} {
		if *db != nil {
			errs = append(errs, (*db).Close())
			*db = nil
		}
	}
	if c.release != nil {
		errs = append(errs, c.release())
		c.release = nil
	}
	return errors.Join(errs...)
}

// Session is one test's view of the database, from [Cleaner.Start] to [Session.Clean].
type Session struct {
	db    *sql.DB
	clean func(context.Context) error
}

// DB is the handle the code under test must use.
func (s *Session) DB() *sql.DB { return s.db }

// Clean undoes what the session did. A session is cleaned once.
func (s *Session) Clean(ctx context.Context) error { return s.clean(ctx) }

// Start begins a session with the Cleaner's strategy.
//
// A session of [Truncation], [Deletion] or [Null] commits, so it holds the database's lock
// until it is cleaned: another one, from this Cleaner, another Cleaner or another process,
// waits for it until ctx ends. Code that starts a second one before cleaning the first waits
// for itself; [Cleaner.DB] knows the test and fails such a request at once. [Transaction]
// sessions take no lock and, once the setup is done, never wait.
func (c *Cleaner) Start(ctx context.Context) (*Session, error) {
	return c.start(ctx, c.strategy, "")
}

// Cleaning runs fn in a session and cleans it afterwards, even when fn fails.
func (c *Cleaner) Cleaning(ctx context.Context, fn func(db *sql.DB) error) error {
	s, err := c.Start(ctx)
	if err != nil {
		return err
	}
	runErr := fn(s.DB())
	return errors.Join(runErr, s.Clean(context.WithoutCancel(ctx)))
}

// CleanWith cleans once with the given strategy, whatever the Cleaner's own is.
// The usual use is a [Truncation] before the suite, then [Transaction] per test.
func (c *Cleaner) CleanWith(ctx context.Context, s Strategy) error {
	session, err := c.start(ctx, s, "")
	if err != nil {
		return err
	}
	return session.Clean(ctx)
}

// start starts a session; owner is the name of the test that asks for it, if one does.
func (c *Cleaner) start(ctx context.Context, s Strategy, owner string) (*Session, error) {
	if err := c.ready(ctx, owner); err != nil {
		return nil, err
	}
	return s.start(ctx, c, owner)
}

// ready takes the dialect's lease and runs the setup, each once. A setup still to run
// waits for the database's turn however long that takes, so a test whose own committing
// session holds the turn is refused before it waits for itself.
func (c *Cleaner) ready(ctx context.Context, owner string) error {
	if err := c.ensureLease(ctx); err != nil {
		return err
	}
	if c.setup != nil && !c.setupDone.Load() {
		if err := c.turns.refuseNested(owner); err != nil {
			return err
		}
	}
	return c.ensureSetup(ctx)
}

// ensureLease takes the dialect's lease, once, and remembers the error.
func (c *Cleaner) ensureLease(ctx context.Context) error {
	c.leaseOnce.Do(func() {
		if c.driver == "" {
			c.driver = c.dialect.Driver()
		}
		leased, release, err := c.dialect.Lease(context.WithoutCancel(ctx), c.url, c.driver)
		if err != nil {
			c.leaseErr = fmt.Errorf("dbcleaner: lease: %w", err)
			return
		}
		c.url, c.release = leased, release
		c.turns = turnsOf(c.driver, c.url)
	})
	return c.leaseErr
}

// ensureSetup runs the setup callback, once, and remembers the error. The setup commits, so
// it takes the database's turn like a committing session, and waits for it however long that
// takes, as the template build does: a cancelled wait would be remembered as the setup's
// error. The setup and its lock run on pools of their own that are closed afterwards, so
// they leave no goroutine behind.
func (c *Cleaner) ensureSetup(ctx context.Context) error {
	c.setupOnce.Do(func() {
		defer c.setupDone.Store(true)
		ctx := context.WithoutCancel(ctx)
		if c.setup == nil {
			return
		}
		db, err := sql.Open(c.driver, c.url)
		if err != nil {
			c.setupErr = fmt.Errorf("dbcleaner: open: %w", err)
			return
		}
		defer func() { _ = db.Close() }()
		lockDB, err := sql.Open(c.driver, c.url)
		if err != nil {
			c.setupErr = fmt.Errorf("dbcleaner: open: %w", err)
			return
		}
		defer func() { _ = lockDB.Close() }()
		give, err := c.turns.take(ctx, c.dialect, lockDB, "")
		if err != nil {
			c.setupErr = fmt.Errorf("dbcleaner: setup: %w", err)
			return
		}
		if err := c.setup(ctx, db); err != nil {
			c.setupErr = fmt.Errorf("dbcleaner: setup: %w", err)
		}
		if err := give(); err != nil && c.setupErr == nil {
			c.setupErr = fmt.Errorf("dbcleaner: setup: %w", err)
		}
	})
	return c.setupErr
}

// committing is the shared pool that commits what it runs.
func (c *Cleaner) committing(ctx context.Context) (*sql.DB, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real != nil {
		return c.real, nil
	}
	db, err := sql.Open(c.driver, c.url)
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dbcleaner: connect: %w", err)
	}
	c.real = db
	return db, nil
}

// wipeable returns the committing pool once the guard accepts its database.
func (c *Cleaner) wipeable(ctx context.Context) (*sql.DB, error) {
	db, err := c.committing(ctx)
	if err != nil {
		return nil, err
	}
	if c.guard == nil {
		return db, nil
	}
	name, err := c.dialect.CurrentDatabase(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: read database name: %w", err)
	}
	if !c.guard(name) {
		return nil, fmt.Errorf("dbcleaner: refusing to wipe database %q: the guard does not allow it (see WithGuard)", name)
	}
	return db, nil
}
