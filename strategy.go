package dbcleaner

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/DATA-DOG/go-txdb"
)

// Strategy is how a session undoes a test's changes.
type Strategy interface {
	start(ctx context.Context, c *Cleaner) (*Session, error)
}

// Transaction rolls the test back. Every session owns one connection with one open
// transaction; the code under test sees it as an ordinary *sql.DB, and its own
// Begin/Commit/Rollback become savepoints, so nested transactions work and still
// vanish with the session. The connection is limited to one: a second would start a
// second transaction that does not see the first one's writes.
func Transaction() Strategy { return transaction{} }

type transaction struct{}

func (transaction) start(ctx context.Context, c *Cleaner) (*Session, error) {
	db := sql.OpenDB(txdb.New(c.driver, c.url))
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dbcleaner: connect: %w", err)
	}
	// Closing the pool rolls the transaction back.
	return &Session{db: db, clean: func(context.Context) error { return db.Close() }}, nil
}

// Truncation empties the tables with TRUNCATE after the test (the dialect decides how:
// on PostgreSQL, RESTART IDENTITY CASCADE). Rows are committed, so any connection sees them (a server under test in
// another process, a second pool), at the price of a slower clean and sessions that
// must not run in parallel against one database.
//
// Migration bookkeeping tables are never truncated (see [Except]).
func Truncation(opts ...TableOption) Strategy {
	return tableStrategy{kind: truncate, filter: newFilter(opts)}
}

// Deletion empties the tables with DELETE after the test, children before parents.
// It is the cheaper choice when a test touches few of many tables, and the one that
// works without the TRUNCATE privilege. Sequences are not reset. A foreign-key cycle
// among the cleaned tables is an error: use [Truncation] there.
//
// Migration bookkeeping tables are never cleaned (see [Except]).
func Deletion(opts ...TableOption) Strategy {
	return tableStrategy{kind: deletion, filter: newFilter(opts)}
}

// Null cleans nothing: the session is the committing pool, untouched afterwards.
func Null() Strategy { return null{} }

type null struct{}

func (null) start(ctx context.Context, c *Cleaner) (*Session, error) {
	db, err := c.committing(ctx)
	if err != nil {
		return nil, err
	}
	return &Session{db: db, clean: func(context.Context) error { return nil }}, nil
}

type tableKind int

const (
	truncate tableKind = iota
	deletion
)

type tableStrategy struct {
	kind   tableKind
	filter filter
}

func (s tableStrategy) start(ctx context.Context, c *Cleaner) (*Session, error) {
	db, err := c.wipeable(ctx)
	if err != nil {
		return nil, err
	}
	return &Session{db: db, clean: func(ctx context.Context) error { return s.clean(ctx, c, db) }}, nil
}

func (s tableStrategy) clean(ctx context.Context, c *Cleaner, db *sql.DB) error {
	sch, err := c.loadSchema(ctx, db)
	if err != nil {
		return err
	}
	tables := s.filter.apply(sch.tables)
	if len(tables) == 0 {
		return nil
	}
	if s.kind == truncate {
		if err := c.dialect.Truncate(ctx, db, tables); err != nil {
			return fmt.Errorf("dbcleaner: truncate: %w", err)
		}
		return nil
	}
	ordered, err := sch.childrenFirst(tables)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dbcleaner: delete: %w", err)
	}
	for _, table := range ordered {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+c.dialect.Quote(table)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("dbcleaner: delete from %s: %w", table, err)
		}
	}
	return tx.Commit()
}

// migrationTables are the bookkeeping tables of the common migration tools: wiping
// them makes the next run migrate again over an existing schema.
var migrationTables = []string{
	"schema_migrations",          // golang-migrate, dbmate, Rails
	"ar_internal_metadata",       // Rails
	"goose_db_version",           // goose
	"atlas_schema_revisions",     // Atlas
	"schema_version",             // tern
	"river_migration",            // river
	"gorp_migrations",            // sql-migrate
	"flyway_schema_history",      // Flyway
	"__diesel_schema_migrations", // Diesel
}

// TableOption narrows the tables [Truncation] and [Deletion] clean.
type TableOption func(*filter)

// Only cleans just these tables.
func Only(tables ...string) TableOption {
	return func(f *filter) { f.only = append(f.only, tables...) }
}

// Except leaves these tables alone, on top of the migration tables.
func Except(tables ...string) TableOption {
	return func(f *filter) { f.except = append(f.except, tables...) }
}

type filter struct{ only, except []string }

func newFilter(opts []TableOption) filter {
	var f filter
	for _, opt := range opts {
		opt(&f)
	}
	return f
}

func (f filter) apply(tables []string) []string {
	return slices.DeleteFunc(slices.Clone(tables), func(table string) bool {
		if slices.Contains(migrationTables, table) || slices.Contains(f.except, table) {
			return true
		}
		return len(f.only) > 0 && !slices.Contains(f.only, table)
	})
}
