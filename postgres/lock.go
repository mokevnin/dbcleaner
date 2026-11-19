package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"

	"github.com/mokevnin/dbcleaner/v2"
)

// sessionLock is the advisory lock of the committing sessions. Advisory locks are per
// database, so one key serves every database; it differs from the keys of the lease, which
// may be taken in the same database.
var sessionLock = lockKey("session")

// Lock implements [dbcleaner.Dialect] with a session advisory lock, which the server lets go
// when the connection ends, however it ends.
func (dialect) Lock(ctx context.Context, db *sql.DB) (func() error, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, sessionLock); err != nil {
		// A wait cut short may still have got the lock: ending the connection lets it go.
		discard(conn)
		return nil, err
	}
	return func() error {
		if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, sessionLock); err != nil {
			discard(conn)
			return err
		}
		return conn.Close()
	}, nil
}

var _ dbcleaner.Dialect = dialect{}

// discard closes the connection instead of handing it back to the pool, so that a lock it
// may hold goes with it.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
