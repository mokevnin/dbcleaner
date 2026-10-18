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

## Development

Tools are pinned by [mise](https://mise.jdx.dev). The suite needs a PostgreSQL server whose
role can create databases (`PGCLEANER_TEST_URL`, default `127.0.0.1:15432`):

```sh
mise run test          # go test -race -shuffle=on
mise run check         # golangci-lint, typos, gitleaks, actionlint, zizmor
mise run check:vulns   # govulncheck (nightly in CI, not part of check)
```
