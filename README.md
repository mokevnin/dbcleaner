# pgcleaner

Put a PostgreSQL database back to its baseline after each Go test: a small analogue of
Ruby's [DatabaseCleaner](https://github.com/DatabaseCleaner/database_cleaner), built on
[go-txdb](https://github.com/DATA-DOG/go-txdb). It knows nothing about your ORM: it hands
out a `*sql.DB` and runs one setup callback (migrations, fixtures) per process.

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
under test just receives a `*sql.DB`. pgcleaner adds what is left around it, the same way
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

## Usage

```go
var cleaner = pgcleaner.New(os.Getenv("DATABASE_URL"),
	pgcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
		// once per process, committed: migrate the schema, load the fixtures
		return migrate(ctx, db)
	}),
)

func TestCreateUser(t *testing.T) {
	db := cleaner.DB(t) // rolled back when the test ends
	// hand db to the code under test
}
```

One strategy before the suite, another per test, as in DatabaseCleaner's `clean_with`:

```go
func TestMain(m *testing.M) {
	_ = cleaner.CleanWith(context.Background(), pgcleaner.Truncation())
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

### Scratch databases

`ScratchDatabase(t, adminURL)` creates an empty, uniquely named database and drops it when
the test ends: for migrations on a fresh instance, and the way this package tests itself.

## Example

A hypothetical `users` package whose `Service` takes a `*sql.DB`. The test package builds one
`Cleaner`; every test asks it for a database and uses it like any other.

```go
package users_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/mokevnin/pgcleaner"
	"github.com/stretchr/testify/require"

	"example.com/app/users"
)

// One Cleaner per test package.
var cleaner = pgcleaner.New(os.Getenv("TEST_DATABASE_URL"),
	pgcleaner.WithSetup(func(ctx context.Context, db *sql.DB) error {
		// Once per process, committed: migrate the schema, load the fixtures.
		return migrate(ctx, db)
	}),
)

func TestRegisterStoresTheUser(t *testing.T) {
	t.Parallel()
	db := cleaner.DB(t) // the baseline, plus nothing: rolled back when the test ends
	svc := users.NewService(db)

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
machine), once per suite. pgcleaner is what runs inside it, per test. Start the container in
`TestMain`, point the `Cleaner` at it, and keep the isolation here.

## Development

Tools are pinned by [mise](https://mise.jdx.dev). The suite needs a PostgreSQL server whose
role can create databases (`PGCLEANER_TEST_URL`, default `127.0.0.1:15432`):

```sh
mise run test          # go test -race -shuffle=on
mise run check         # golangci-lint, typos, gitleaks, actionlint, zizmor
mise run check:vulns   # govulncheck (nightly in CI, not part of check)
```
