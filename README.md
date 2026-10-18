# pgcleaner

Put a PostgreSQL database back to its baseline after each Go test: a small analogue of
Ruby's [DatabaseCleaner](https://github.com/DatabaseCleaner/database_cleaner), built on
[go-txdb](https://github.com/DATA-DOG/go-txdb). It knows nothing about your ORM: it hands
out a `*sql.DB` and runs one setup callback (migrations, fixtures) per process.

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

The suite needs a PostgreSQL server whose role can create databases:

```sh
PGCLEANER_TEST_URL=postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable mise run test
```
