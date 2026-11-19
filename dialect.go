package dbcleaner

import (
	"context"
	"database/sql"
)

// Dialect is what the cleaner has to ask one database engine: which database to work on,
// which tables there are, which of them reference which, how to empty them and how to lock
// the database. Everything else is database/sql. The engines this module knows live in
// subpackages (postgres.Dialect()), and a Cleaner is built with one: [New].
//
// Every method is required: an engine that cannot do one says so in its code, where the
// compiler sees it, and what only one engine can do (a template database on PostgreSQL) is
// an option of that engine's package, not one a Cleaner of another engine could be given.
type Dialect interface {
	// Driver is the database/sql driver name the URL is opened with by default. The
	// package of the dialect imports the driver.
	Driver() string
	// Lease returns the URL of the database the Cleaner works on, given the one it was built
	// with: that one, or a database of this process's own. release gives back what Lease
	// took; [Cleaner.Close] calls it. It runs once, before the setup.
	Lease(ctx context.Context, url, driver string) (leased string, release func() error, err error)
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
	// Delete deletes every row of one table. [Deletion] calls it inside its transaction, for
	// every table, children before parents; a table that references itself is the dialect's
	// to handle.
	Delete(ctx context.Context, tx *sql.Tx, table string) error
	// Lock blocks until it holds the lock of the database db is connected to, or ctx ends,
	// and unlock gives it back. It is what makes everything that commits (the setup, the
	// sessions of the committing strategies) take turns, across connections and processes.
	// db is a pool of its own, not the one handed to tests. The lock must be let go when its
	// holder dies however it dies (a session lock, a file lock), so that a crashed test
	// process never leaves it taken.
	Lock(ctx context.Context, db *sql.DB) (unlock func() error, err error)
}
