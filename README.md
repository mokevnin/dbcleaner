# dbcleaner

Put a database back to its baseline after each Go test: a small analogue of
Ruby's [DatabaseCleaner](https://github.com/DatabaseCleaner/database_cleaner), built on
[go-txdb](https://github.com/DATA-DOG/go-txdb). It knows nothing about your ORM: it hands
out a `*sql.DB` and runs one setup callback (migrations, fixtures) per process.

What differs between engines is a small `Dialect` (quoting, listing tables and foreign keys,
truncating). Import the package of your engine:

| Engine | Package | Registered for | `WithTemplate` |
|---|---|---|---|
| PostgreSQL | `dbcleaner/postgres` | `postgres://`, `postgresql://` | yes |
| SQLite | `dbcleaner/sqlite` | `file:` | no |
| MySQL, MariaDB | `dbcleaner/mysql` | none: `WithDialect(mysql.Dialect())` | no |

A DSN that is not a URL (`root@tcp(127.0.0.1:3306)/app_test`, a plain SQLite path) names no
scheme, so it takes `dbcleaner.WithDialect`. Another engine is one more package with the
same six methods. The examples below use PostgreSQL.

## Why?

Tests that touch a database must not leak into each other: a row one test inserts cannot
change what the next one sees, or the suite passes in one order and fails in another. There
are three ways to get there, and most ecosystems ship an answer to this out of the box:

1. **Run each test in a transaction and roll it back.** Nothing is ever committed, so there
   is nothing to clean, and it costs almost nothing: a rollback is far cheaper than
   recreating data. This is the default almost everywhere:
   Rails (`use_transactional_tests`), Django (`TestCase`), Laravel (`RefreshDatabase`,
   `DatabaseTransactions`), Spring (`@Transactional` tests roll back by default) and Elixir's
   Ecto (`SQL.Sandbox`).
2. **Truncate or delete the tables after the test.** Slower, but the data is really
   committed, so a second connection can see it. This is what those same frameworks fall back
   to when a test cannot share one connection: Django's `TransactionTestCase`, Laravel's
   `DatabaseTruncation`, and the `:truncation` and `:deletion` strategies of Ruby's
   [DatabaseCleaner](https://github.com/DatabaseCleaner/database_cleaner), which this package
   is modeled on.
3. **Create a fresh database per test.** Fully isolated, but creating and migrating one takes
   far longer than a rollback.

Go's standard `testing` package has no such thing, so every project writes its own: open a
`*sql.Tx` in each test, hand it around, remember to roll back. That works until the code under
test opens a transaction of its own (a `*sql.Tx` cannot be nested), or the code takes a
`*sql.DB` and not a `*sql.Tx`, and then it is rewritten per project.

[go-txdb](https://github.com/DATA-DOG/go-txdb) solves the hard part of the first approach: it
is a `database/sql` driver that puts the whole connection inside one transaction, turns the
code's own `Begin`/`Commit`/`Rollback` into savepoints, and rolls back on `Close`. The code
under test just receives a `*sql.DB`. dbcleaner adds what is left around it, the same way
DatabaseCleaner wraps Ruby's primitives: the choice between the strategies above, a setup
step that runs once per process (migrations, fixtures), the table lists and foreign-key order
for truncation and deletion, a guard against wiping the wrong database, and scratch databases.

**Pick `Transaction` unless the test cannot use the handle it is given.** The usual reason is
code that talks to the database over its own connection: an HTTP server in another process,
a worker with its own pool, a `LISTEN`/`NOTIFY` consumer. Their connections cannot see an
uncommitted transaction, so use `Truncation` or `Deletion` for those tests.

| Strategy        | How a test is undone                          | Use when                                                            |
| --------------- | --------------------------------------------- | ------------------------------------------------------------------- |
| `Transaction()` | the whole test runs in one rolled-back tx     | the default; the test uses the `*sql.DB` it is given                |
| `Truncation()`  | `TRUNCATE … RESTART IDENTITY CASCADE` after   | another connection must see the rows (a server in another process) |
| `Deletion()`    | `DELETE` after, children before parents       | few tables touched, or no `TRUNCATE` privilege                      |
| `Null()`        | nothing                                       | you clean by hand                                                   |

`Truncation` and `Deletion` accept `Only(...)` and `Except(...)`, and never touch the
migration bookkeeping tables of golang-migrate, goose, Atlas, river, Rails and the like.

## Set up with a coding agent

Copy this prompt into Claude Code, Codex, Cursor or another coding agent opened in your
project. The agent finds how the tests isolate the database today, moves them to dbcleaner
with the strategy each package needs and removes the helpers it replaces.

```text
Adopt dbcleaner (https://github.com/mokevnin/dbcleaner) for the database tests of this Go project. Read its README first; it explains the strategies and when each one fits.

1. Find how the tests isolate the database today (hand-written transactions, truncation helpers, a database per test, `go test -p 1`, testcontainers) and which migrations and fixtures they run.
2. Run `go get github.com/mokevnin/dbcleaner` and import the dialect package of the engine: `postgres`, `sqlite` or `mysql`.
3. In each test package that touches the database, build one Cleaner whose `WithSetup` runs the existing migrations and fixtures, and replace the hand-written isolation with `cleaner.DB(t)`. Keep the default `Transaction` strategy. Use `Truncation` or `Deletion` only for a package whose code under test reaches the database over its own connection (a server in another process, a worker with its own pool, LISTEN/NOTIFY), and say why for each such package.
4. On PostgreSQL, if test packages are serialized with `-p 1` or share one database, use `WithTemplate` so each process gets a database of its own, then drop `-p 1`. The database role needs the right to create databases.
5. The committing strategies refuse a database whose name lacks `test`, `e2e` or `scratch`. Rename the test database if needed; never loosen `WithGuard` to get around it.
6. Remove the helpers dbcleaner replaces. If the project starts its database with testcontainers, keep that and point the Cleaner at it.
7. Add to the project's agent instructions (AGENTS.md, CLAUDE.md or whatever file the project already uses; create AGENTS.md if there is none): a test that touches the database takes its `*sql.DB` from the package's Cleaner with `cleaner.DB(t)` and never opens a transaction by hand to isolate itself.
8. Run the whole suite twice with `-race -shuffle=on` and report the strategy of each package, what you removed, and how long the suite took before and after.
```

## Usage

A hypothetical `users` package whose `Service` takes a `*sql.DB`. The test package builds one
`Cleaner`; every test asks it for a database and uses it like any other.

```go
package users_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/mokevnin/dbcleaner"
	_ "github.com/mokevnin/dbcleaner/postgres" // the dialect for postgres:// URLs
	"github.com/stretchr/testify/require"

	"example.com/app/users"
)

// One Cleaner per test package.
var cleaner = dbcleaner.New(os.Getenv("TEST_DATABASE_URL"),
	dbcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
		// Once per process, committed: migrate the schema, load the fixtures.
		return migrate(ctx, db)
	}),
)

func TestRegisterStoresTheUser(t *testing.T) {
	t.Parallel()
	svc := users.NewService(cleaner.DB(t)) // the baseline; rolled back when the test ends

	_, err := svc.Register(t.Context(), "bob@example.com")
	require.NoError(t, err)

	got, err := svc.ByEmail(t.Context(), "bob@example.com")
	require.NoError(t, err)
	require.Equal(t, "bob@example.com", got.Email)
}

func TestRegisterRejectsADuplicate(t *testing.T) {
	t.Parallel()
	svc := users.NewService(cleaner.DB(t))
	_, err := svc.Register(t.Context(), "ann@example.com")
	require.NoError(t, err)

	_, err = svc.Register(t.Context(), "ann@example.com")

	// Bob from the test above is not here, and Ann will not be in the next one.
	require.ErrorIs(t, err, users.ErrEmailTaken)
}
```

`Register` may open its own `db.BeginTx` inside: it becomes a savepoint of the test's
transaction and is still undone with it.

When a test cannot use the handle it is given (the code under test runs in another process or
on its own pool), pick another strategy for that package:

```go
var cleaner = dbcleaner.New(url,
	dbcleaner.WithStrategy(dbcleaner.Truncation(dbcleaner.Except("countries"))),
	dbcleaner.WithSetup(migrateAndSeed),
)
```

One strategy before the suite, another per test, as in DatabaseCleaner's `clean_with`:

```go
func TestMain(m *testing.M) {
	_ = cleaner.CleanWith(context.Background(), dbcleaner.Truncation())
	os.Exit(m.Run())
}
```

Without `testing`, use `Start` / `Session.Clean`, or `Cleaning(ctx, func(db *sql.DB) error)`.

`Transaction` limits a session to one connection, because a second one would open a second
transaction that does not see the first one's writes. The code under test may still call
`Begin`/`Commit`/`Rollback`: they become savepoints. Do not run sessions of the committing
strategies in parallel against one database.

### Safety

`Truncation` and `Deletion` refuse to run unless the database name contains `test`, `e2e` or
`scratch`, so a URL left pointing at a development database is not emptied. Replace the check
with `WithGuard(func(name string) bool)`, or pass `nil` to allow everything.

### Parallel test packages

`go test ./...` runs one process per package. Against one database they fight over the same
rows and locks, so the usual fix is `-p 1`, which also serializes building and linking the test
binaries (the slow half of many suites). `WithTemplate` gives every process a database of its
own instead:

```go
var cleaner = dbcleaner.New(os.Getenv("DATABASE_URL"),
	// The schema, built once per version into a template database.
	dbcleaner.WithTemplate(schemaHash, func(ctx context.Context, db *sql.DB, url string) error {
		return migrate(ctx, db)
	}),
	// What differs per process: runs on the process's own database every time.
	dbcleaner.WithSetup(loadFixtures),
)
```

The databases are siblings of the one in the URL: `<name>_tpl_<hash>`, the template, built by
whichever process comes first and under an advisory lock; and `<name>_w<i>`, the workers. A
process leases the lowest free worker with a session advisory lock, which the server releases
when the process ends however it ends, so there is nothing to clean up and the number of
databases stays at the number of concurrent processes. A worker is cloned from the template when
it is missing or was cloned from an older version. Bump the version (a hash of the migration
files, say) when the schema changes; older templates are dropped.

Keep what has relative dates in `WithSetup`, not in `build`: a template is built once and
lives on. `cleaner.URL(ctx)` is the database to hand to code that opens its own connection.
The role needs the right to create databases, and the dialect must support leasing (PostgreSQL's does). In the suite this was extracted from, 131 s with
`-p 1` became about 30 s.

### Scratch databases

`postgres.ScratchDatabase(t, adminURL)` creates an empty, uniquely named database and drops it when
the test ends: for migrations on a fresh instance, and the way this package tests itself.

## How fast is it?

One test's whole lifecycle against a 60-table schema: get a database at the baseline, insert
two rows, undo it. Measured with `go test -bench .` (see `bench_test.go`), Apple M4, PostgreSQL 18.6
on localhost:

| Approach                            | Per test | Grows with                        |
| ----------------------------------- | -------- | --------------------------------- |
| `Transaction()`                     | ~3.4 ms  | nothing (mostly one connection)   |
| `Deletion()`                        | ~1.5 ms  | tables and rows left behind       |
| `Truncation()`                      | ~36 ms   | the number of tables              |
| `CREATE DATABASE … TEMPLATE` + drop | ~78 ms   | the size of the template database |

Read it as orders of magnitude, not a ranking to copy: with two rows and a warm pool `Deletion`
wins by a couple of milliseconds, and it loses as tests write to more tables. What does not
change is that `Transaction` costs the same however big the schema or the test's data is, and
it is the only one whose tests can run with `t.Parallel()` against one database (as long as
they do not contend for the same rows), because every session has its own transaction.
A database per test from a template is exact and needs no cleanup logic, but at tens of
milliseconds it is the slowest and adds up over a few thousand tests.

[testcontainers](https://golang.testcontainers.org) is not an alternative to any of these: it
provisions the server (about 1.3 s for a PostgreSQL 17 container with a warm image on the same
machine), once per suite. dbcleaner is what runs inside it, per test. Start the container in
`TestMain`, point the `Cleaner` at it, and keep the isolation here.

## Development

Tools are pinned by [mise](https://mise.jdx.dev). The suite needs a PostgreSQL server whose
role can create databases (`DBCLEANER_TEST_URL`, default `127.0.0.1:15432`), and a MySQL one
(`DBCLEANER_MYSQL_TEST_URL`, a DSN, default `root:root@tcp(127.0.0.1:13306)/`); the SQLite tests
need nothing. Every dialect passes the same suite, `internal/conformance`:

```sh
mise run test          # go test -race -shuffle=on
mise run check         # golangci-lint, typos, gitleaks, actionlint, zizmor
mise run check:vulns   # govulncheck (nightly in CI, not part of check)
```
