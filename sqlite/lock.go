package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofrs/flock"
	"github.com/mokevnin/dbcleaner/v2"
)

// lockSuffix names the lock file, next to the database's: database.db-dbcleaner.
const lockSuffix = "-dbcleaner"

// retryDelay is how often a waiting session tries the lock file again.
const retryDelay = 20 * time.Millisecond

// Lock implements [dbcleaner.Dialect] with an exclusive lock on a file next to the
// database's (flock on Unix, LockFileEx on Windows, through github.com/gofrs/flock), which the
// system lets go when the process ends, however it ends. The file is left in place: removing
// it would race with the next holder. A database in memory has no file and no other process
// to share it with: there is nothing to lock.
func (dialect) Lock(ctx context.Context, db *sql.DB) (func() error, error) {
	var file string
	err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file)
	if err != nil {
		return nil, err
	}
	if file == "" {
		return func() error { return nil }, nil
	}
	// A Flock of its own per call: one that holds the lock already would say it got it again.
	lock := flock.New(file + lockSuffix)
	got, err := lock.TryLockContext(ctx, retryDelay)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !got {
		_ = lock.Close()
		return nil, errors.New("the lock file was not locked")
	}
	return lock.Unlock, nil
}

var _ dbcleaner.Dialect = dialect{}
