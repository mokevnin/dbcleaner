package postgres

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
	"github.com/peterldowns/pgtestdb"
)

// maxWorkers bounds how many processes lease a worker database of one server at once.
const maxWorkers = 64

// templateOptions is what WithTemplate was given.
type templateOptions struct {
	migrator pgtestdb.Migrator
}

// WithTemplate makes the Cleaner work on a database of its own instead of the one in its
// URL, so that test processes (one per package under `go test ./...`) run in parallel without
// sharing rows or locks. The schema comes from a [pgtestdb.Migrator]: pgtestdb has one for
// goose, golang-migrate, atlas, dbmate, tern, bun and plain SQL files
// (github.com/peterldowns/pgtestdb/migrators/...), and one of your own is two methods. Its
// Hash names the schema: a changed hash builds a new template. Two kinds of database are kept next to the one in the URL,
// called base:
//
//   - base_tpl_<hash>, the template, where hash is the migrator's Hash (a base too long for a
//     63-byte name is shortened, with a hash of it). It is built once per hash, by whichever
//     process comes first and under an advisory lock, by running Migrate on an empty
//     database; others wait for it. A template that exists is always complete. Templates of
//     older hashes are dropped.
//   - base_w<i>, the workers. A process leases the lowest free index with a session advisory
//     lock, which the server releases when the process ends, however it ends: there is no
//     cleanup hook to forget, and the number of databases stays bounded by the number of
//     concurrent processes. A worker is cloned from the template when it is missing or was
//     cloned from another one, and is otherwise reused as it is.
//
// What differs per process belongs in [dbcleaner.WithSetup], which runs on the worker every
// time (fixtures with relative dates, for one): it also wipes whatever an earlier process
// left. The database to hand to code that opens its own connection is [dbcleaner.Cleaner.URL].
//
// The URL must be a postgres:// URL, and its role needs the right to create databases. A
// migrator that connects by itself uses [pgtestdb.Config.URL], which does not escape the user
// and password: keep URL-special characters out of them. A
// Cleaner whose first session starts inside a testing/synctest bubble must be closed inside
// it, or never: a test process just ends.
func WithTemplate(m pgtestdb.Migrator) Option {
	return func(d *dialect) { d.template = &templateOptions{migrator: m} }
}

// migratorConfig is the template database at rawURL as a pgtestdb migrator expects it.
func migratorConfig(u *url.URL, driver string) pgtestdb.Config {
	password, _ := u.User.Password()
	host, port := u.Hostname(), u.Port()
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5432"
	}
	return pgtestdb.Config{
		DriverName: driver,
		Host:       host,
		Port:       port,
		User:       u.User.Username(),
		Password:   password,
		Database:   strings.TrimPrefix(u.Path, "/"),
		Options:    u.RawQuery,
	}
}

// Lease implements [dbcleaner.Dialect]: the URL itself, or with [WithTemplate], this
// process's worker database.
func (d dialect) Lease(ctx context.Context, rawURL, driver string) (string, func() error, error) {
	if d.template == nil {
		return rawURL, func() error { return nil }, nil
	}
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme == "" {
		return "", nil, fmt.Errorf("WithTemplate needs a postgres:// URL to name its databases after, not %q", rawURL)
	}
	m := d.template.migrator
	version, err := m.Hash()
	if err != nil {
		return "", nil, fmt.Errorf("hash the migrations: %w", err)
	}
	build := func(ctx context.Context, db *sql.DB, buildURL string) error {
		u, err := url.Parse(buildURL)
		if err != nil {
			return err
		}
		return m.Migrate(ctx, db, migratorConfig(u, driver))
	}
	baseName := strings.TrimPrefix(base.Path, "/")
	sum := sha256.Sum256([]byte(version))
	versionTag := "_tpl_" + hex.EncodeToString(sum[:])[:10]
	template := derive(baseName, versionTag)

	if err := ensureTemplate(ctx, rawURL, driver, build, base, baseName, versionTag, template); err != nil {
		return "", nil, err
	}

	// The locks live in the database of the URL: advisory locks are per database.
	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("connect: %w", err)
	}
	worker, err := claimWorker(ctx, conn, baseName)
	if err == nil {
		err = cloneWorker(ctx, conn, worker, template)
	}
	if err != nil {
		_ = conn.Close(ctx)
		return "", nil, err
	}
	// The connection is a bare one, not a pool: it has no goroutine, so a first session
	// started inside a testing/synctest bubble leaves nothing behind in it.
	return withDatabase(base, worker), func() error { return conn.Close(context.Background()) }, nil
}

func ensureTemplate(ctx context.Context, rawURL, driver string, build func(context.Context, *sql.DB, string) error, base *url.URL, baseName, versionTag, template string) error {
	conn, err := pgx.Connect(ctx, rawURL)
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
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(building)+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quoteIdent(building)); err != nil {
		return err
	}
	buildURL := withDatabase(base, building)
	buildDB, err := sql.Open(driver, buildURL)
	if err != nil {
		return err
	}
	buildErr := build(ctx, buildDB, buildURL)
	_ = buildDB.Close() // the rename below needs no connection left
	if buildErr != nil {
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(building)+` WITH (FORCE)`)
		return fmt.Errorf("build template: %w", buildErr)
	}
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+quoteIdent(building)+` RENAME TO `+quoteIdent(template)); err != nil {
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
		_, _ = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name))
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
	return "", fmt.Errorf("dbcleaner: all %d worker databases are taken", maxWorkers)
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
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(worker)+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quoteIdent(worker)+` TEMPLATE `+quoteIdent(template)); err != nil {
		return fmt.Errorf("clone %s: %w", worker, err)
	}
	_, err = conn.Exec(ctx, `COMMENT ON DATABASE `+quoteIdent(worker)+` IS `+literal(template))
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
