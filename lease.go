package pgcleaner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// maxWorkers bounds how many processes lease a worker database of one server at once.
const maxWorkers = 64

// WithTemplate makes the Cleaner work on a database of its own instead of the one in
// its URL, so that test processes (one per package under `go test ./...`) run in
// parallel without sharing rows or locks.
//
// Two kinds of database are kept next to the one in the URL, called base:
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
	base, err := url.Parse(c.url)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	baseName := strings.TrimPrefix(base.Path, "/")
	sum := sha256.Sum256([]byte(c.templateVersion))
	versionTag := "_tpl_" + hex.EncodeToString(sum[:])[:10]
	template := derive(baseName, versionTag)

	if err := c.ensureTemplate(ctx, base, baseName, versionTag, template); err != nil {
		return err
	}

	// The locks live in the database of the URL: advisory locks are per database.
	conn, err := pgx.Connect(ctx, c.url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	worker, err := claimWorker(ctx, conn, baseName)
	if err == nil {
		err = cloneWorker(ctx, conn, worker, template)
	}
	if err != nil {
		_ = conn.Close(ctx)
		return err
	}
	c.leaseConn = conn // kept open on purpose: closing it releases the lease
	c.url = withDatabase(base, worker)
	return nil
}

func (c *Cleaner) ensureTemplate(ctx context.Context, base *url.URL, baseName, versionTag, template string) error {
	conn, err := pgx.Connect(ctx, c.url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	key := lockKey("template:" + template)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, key) }()

	if databaseExists(ctx, conn, template) {
		return nil
	}

	// Built under another name and renamed when complete, so a template that exists is
	// whole even after a build that was interrupted.
	building := derive(baseName, versionTag+"_build")
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quote(building)+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quote(building)); err != nil {
		return err
	}
	buildURL := withDatabase(base, building)
	buildDB, err := sql.Open(c.driver, buildURL)
	if err != nil {
		return err
	}
	buildErr := c.templateBuild(ctx, buildDB, buildURL)
	_ = buildDB.Close() // the rename below needs no connection left
	if buildErr != nil {
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quote(building)+` WITH (FORCE)`)
		return fmt.Errorf("build template: %w", buildErr)
	}
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+quote(building)+` RENAME TO `+quote(template)); err != nil {
		return err
	}

	// Templates of older versions are dead weight. One that cannot be dropped now (a
	// worker is being cloned from it) is left for the next build.
	prefix := strings.TrimSuffix(derive(baseName, versionTag), versionTag)
	like := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(prefix) + `\_tpl\_%`
	rows, err := conn.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1 AND datname <> $2`, like, template)
	if err != nil {
		return nil
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil
	}
	for _, name := range stale {
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quote(name))
	}
	return nil
}

// claimWorker takes the lowest free worker index and returns that worker's name.
func claimWorker(ctx context.Context, conn *pgx.Conn, baseName string) (string, error) {
	for i := range maxWorkers {
		name := derive(baseName, fmt.Sprintf("_w%d", i))
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey("worker:"+name)).Scan(&got); err != nil {
			return "", err
		}
		if got {
			return name, nil
		}
	}
	return "", fmt.Errorf("pgcleaner: all %d worker databases are taken", maxWorkers)
}

// cloneWorker makes the worker a copy of the template unless it already is one. The
// template a worker came from is kept in the database's comment.
func cloneWorker(ctx context.Context, conn *pgx.Conn, worker, template string) error {
	var from *string
	err := conn.QueryRow(ctx,
		`SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, worker).Scan(&from)
	if err == nil && from != nil && *from == template {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quote(worker)+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quote(worker)+` TEMPLATE `+quote(template)); err != nil {
		return fmt.Errorf("clone %s: %w", worker, err)
	}
	_, err = conn.Exec(ctx, `COMMENT ON DATABASE `+quote(worker)+` IS `+literal(template))
	return err
}

func databaseExists(ctx context.Context, conn *pgx.Conn, name string) bool {
	var one int
	return conn.QueryRow(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`, name).Scan(&one) == nil
}

// withDatabase is the server URL pointed at another database.
func withDatabase(server *url.URL, name string) string {
	u := *server
	u.Path = "/" + name
	return u.String()
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func lockKey(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64())
}

// derive returns base+suffix. PostgreSQL silently cuts identifiers at 63 bytes, which would
// make `x_tpl_1` and `x_tpl_1_build` the same database for a long base; past the limit
// the base is shortened and a hash of the whole of it keeps the names apart. Names are
// assumed to be ASCII.
func derive(base, suffix string) string {
	const maxLen = 63
	if len(base)+len(suffix) <= maxLen {
		return base + suffix
	}
	sum := sha256.Sum256([]byte(base))
	tag := "_" + hex.EncodeToString(sum[:4])
	return base[:maxLen-len(suffix)-len(tag)] + tag + suffix
}
