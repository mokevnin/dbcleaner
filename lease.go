package dbcleaner

import (
	"context"
	"database/sql"
	"fmt"
)

// WithTemplate makes the Cleaner work on a database of its own instead of the one in
// its URL, so that test processes (one per package under `go test ./...`) run in
// parallel without sharing rows or locks.
//
// The dialect must be a [Leaser] (PostgreSQL's is). What follows is what it does, in
// PostgreSQL's terms. Two kinds of database are kept next to the one in the URL, called base:
//
//   - base_tpl_<hash of version>, the template (a base too long for a 63-byte name is shortened,
//     with a hash of it). It is built once per version, by whichever
//     process comes first and under an advisory lock, by running build on an empty
//     database; others wait for it. A template that exists is always complete. Older
//     versions are dropped.
//   - base_w<i>, the workers. A process leases the lowest free index with a session
//     advisory lock, which the server releases when the process ends, however it ends:
//     there is no cleanup hook to forget, and the number of databases stays bounded by
//     the number of concurrent processes. A worker is cloned from the template when it is
//     missing or was cloned from another version, and is otherwise reused as it is.
//
// version names everything that decides what build produces: bump it when the schema
// changes (a hash of the migration files, say). build gets the template database and its
// URL, for a tool that opens its own connection.
//
// What differs per process belongs in [WithSetup], which runs on the worker every time
// (fixtures with relative dates, for one): it also wipes whatever an earlier process left.
// The database to hand to code that opens its own connection is [Cleaner.URL].
//
// The role needs the right to create databases. A Cleaner whose first session starts inside
// a testing/synctest bubble must be closed inside it, or never: a test process just ends.
func WithTemplate(version string, build func(ctx context.Context, db *sql.DB, url string) error) Option {
	return func(c *Cleaner) { c.templateVersion, c.templateBuild = version, build }
}

// URL is the URL of the database the Cleaner works on: the one it was built with, or,
// with [WithTemplate], this process's worker database. It starts what is needed (the
// lease, the setup) if that has not happened yet.
func (c *Cleaner) URL(ctx context.Context) (string, error) {
	if err := c.ensureSetup(ctx); err != nil {
		return "", err
	}
	return c.url, nil
}

// lease sets c.url to this process's worker database.
func (c *Cleaner) lease(ctx context.Context) error {
	l, ok := c.dialect.(Leaser)
	if !ok {
		return fmt.Errorf("the dialect %T cannot lease a database: WithTemplate is not supported for it", c.dialect)
	}
	leased, release, err := l.Lease(ctx, c.url, c.driver, c.templateVersion, c.templateBuild)
	if err != nil {
		return err
	}
	c.release = release // kept on purpose: giving it back releases the lease
	c.url = leased
	return nil
}
