package dbcleaner

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
)

// Dialect is what the cleaner has to ask one database engine: how to quote a name, which
// tables there are, which of them reference which, and how to empty them. Everything else
// is database/sql. The engines this module knows live in subpackages (dbcleaner/postgres),
// which register themselves for their URL schemes when imported.
type Dialect interface {
	// Driver is the database/sql driver name the URL is opened with by default. The
	// package of the dialect imports the driver.
	Driver() string
	// Quote quotes one identifier.
	Quote(identifier string) string
	// CurrentDatabase is the name of the database db is connected to, for the guard.
	CurrentDatabase(ctx context.Context, db *sql.DB) (string, error)
	// Tables lists the ordinary tables of the current schema, sorted, leaving out those
	// that cleaning another table cleans too (partitions).
	Tables(ctx context.Context, db *sql.DB) ([]string, error)
	// References maps a table to the tables it holds a foreign key to. A table that
	// references itself is not listed as referencing itself.
	References(ctx context.Context, db *sql.DB) (map[string][]string, error)
	// Truncate empties the tables, resets what counts rows (identity columns, sequences)
	// and does not stop at a foreign key between them or to them.
	Truncate(ctx context.Context, db *sql.DB, tables []string) error
}

// Leaser is a Dialect that can give a process a database of its own, cloned from a template:
// what [WithTemplate] needs. An engine with no cheap way to clone a database leaves it out.
type Leaser interface {
	Dialect
	// Lease prepares the databases next to the one in rawURL as [WithTemplate] describes and
	// returns the URL of this process's worker. release gives the lease back; it is called
	// by [Cleaner.Close].
	Lease(ctx context.Context, rawURL, driver, version string, build func(ctx context.Context, db *sql.DB, url string) error) (leased string, release func() error, err error)
}

var (
	dialectsMu sync.RWMutex
	dialects   = map[string]Dialect{}
)

// Register makes a dialect the default for the URLs of a scheme, the way database/sql
// registers a driver: from an init function of the package that has it. It panics on a
// scheme registered twice.
func Register(scheme string, d Dialect) {
	dialectsMu.Lock()
	defer dialectsMu.Unlock()
	if d == nil {
		panic("dbcleaner: Register dialect is nil")
	}
	if _, dup := dialects[scheme]; dup {
		panic("dbcleaner: Register called twice for scheme " + scheme)
	}
	dialects[scheme] = d
}

// WithDialect sets the dialect, for a URL whose scheme names none (a key=value DSN) or
// to use another than the registered one.
func WithDialect(d Dialect) Option { return func(c *Cleaner) { c.dialect = d } }

// resolveDialect is the dialect the option set, or the one registered for the URL's scheme.
func (c *Cleaner) resolveDialect() (Dialect, error) {
	if c.dialect != nil {
		return c.dialect, nil
	}
	u, err := url.Parse(c.url)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("dbcleaner: the database URL names no scheme, so there is no way to tell the engine: use WithDialect")
	}
	dialectsMu.RLock()
	d, ok := dialects[u.Scheme]
	dialectsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("dbcleaner: no dialect for the scheme %q: import the package of the engine (dbcleaner/postgres) or use WithDialect", u.Scheme)
	}
	return d, nil
}
