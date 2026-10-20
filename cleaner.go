// Package dbcleaner puts a PostgreSQL database back to its baseline after a test,
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
package dbcleaner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // the default driver
)

// Cleaner starts cleaning sessions against one database. Build it once per test
// process (a package variable next to TestMain) and share it between tests.
type Cleaner struct {
	url      string
	driver   string
	strategy Strategy
	setup    func(context.Context, *sql.DB) error
	guard    func(database string) bool

	templateVersion string
	templateBuild   func(ctx context.Context, db *sql.DB, url string) error
	// leaseConn holds the worker database lease; see WithTemplate. A bare connection, not
	// a pool: it has no goroutine, so a first session started inside a testing/synctest
	// bubble leaves nothing behind in it.
	leaseConn *pgx.Conn

	setupOnce sync.Once
	setupErr  error

	mu     sync.Mutex
	real   *sql.DB // the shared, committing pool of the non-transactional strategies
	schema *schema // table and foreign-key listing, read once
}

// Option tunes a Cleaner.
type Option func(*Cleaner)

// WithStrategy sets the strategy [Cleaner.Start] and [Cleaner.DB] use. The default
// is [Transaction].
func WithStrategy(s Strategy) Option { return func(c *Cleaner) { c.strategy = s } }

// WithDriver names the database/sql driver the URL is opened with. The default is
// "pgx", registered by this package; any other driver must be imported by the caller.
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

// New returns a Cleaner for the database at url. Nothing connects until the first
// session starts.
func New(url string, opts ...Option) *Cleaner {
	c := &Cleaner{url: url, driver: "pgx", strategy: Transaction(), guard: DefaultGuard}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Close releases the shared pool the committing strategies use and the worker database
// lease. A Cleaner that only ran [Transaction] on its own database holds nothing to close.
func (c *Cleaner) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	if c.real != nil {
		errs = append(errs, c.real.Close())
		c.real = nil
	}
	if c.leaseConn != nil {
		errs = append(errs, c.leaseConn.Close(context.Background()))
		c.leaseConn = nil
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
func (c *Cleaner) Start(ctx context.Context) (*Session, error) {
	return c.start(ctx, c.strategy)
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
	session, err := c.start(ctx, s)
	if err != nil {
		return err
	}
	return session.Clean(ctx)
}

func (c *Cleaner) start(ctx context.Context, s Strategy) (*Session, error) {
	if err := c.ensureSetup(ctx); err != nil {
		return nil, err
	}
	return s.start(ctx, c)
}

// ensureSetup leases the worker database (WithTemplate) and runs the setup callback,
// once, and remembers the error. The setup runs on a pool of its own that is closed
// afterwards, so it leaves no goroutine behind.
func (c *Cleaner) ensureSetup(ctx context.Context) error {
	c.setupOnce.Do(func() {
		ctx := context.WithoutCancel(ctx)
		if c.templateBuild != nil {
			if err := c.lease(ctx); err != nil {
				c.setupErr = fmt.Errorf("dbcleaner: lease: %w", err)
				return
			}
		}
		if c.setup == nil {
			return
		}
		db, err := sql.Open(c.driver, c.url)
		if err != nil {
			c.setupErr = fmt.Errorf("dbcleaner: open: %w", err)
			return
		}
		defer func() { _ = db.Close() }()
		if err := c.setup(ctx, db); err != nil {
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
	var name string
	if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		return nil, fmt.Errorf("dbcleaner: read database name: %w", err)
	}
	if !c.guard(name) {
		return nil, fmt.Errorf("dbcleaner: refusing to wipe database %q: the guard does not allow it (see WithGuard)", name)
	}
	return db, nil
}
