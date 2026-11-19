package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	"github.com/mokevnin/dbcleaner/v2"
)

// lockName is the named lock of the committing sessions. Named locks are server-wide and at
// most 64 characters long, so the name carries a hash of the database's.
const lockName = `CONCAT('dbcleaner:', SHA1(DATABASE()))`

// Lock implements [dbcleaner.Dialect] with GET_LOCK, which the server lets go when the
// connection ends, however it ends. It waits a second at a time, so that a cancelled wait
// does not leave the server waiting on a connection nobody reads.
func (dialect) Lock(ctx context.Context, db *sql.DB) (func() error, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	for {
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(`+lockName+`, 1)`).Scan(&got); err != nil {
			// A wait cut short may still have got the lock: ending the connection lets it go.
			discard(conn)
			return nil, err
		}
		if !got.Valid {
			discard(conn)
			return nil, errors.New("GET_LOCK failed: is a database selected?")
		}
		if got.Int64 == 1 {
			break
		}
		if err := ctx.Err(); err != nil {
			discard(conn)
			return nil, err
		}
	}
	return func() error {
		if _, err := conn.ExecContext(context.Background(), `DO RELEASE_LOCK(`+lockName+`)`); err != nil {
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
