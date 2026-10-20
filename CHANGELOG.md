# Changelog

## [1.0.0](https://github.com/mokevnin/dbcleaner/compare/v0.2.0...v1.0.0) (2026-10-20)


### ⚠ BREAKING CHANGES

* import _ "github.com/mokevnin/dbcleaner/postgres"; ScratchDatabase is postgres.ScratchDatabase
* import github.com/mokevnin/dbcleaner instead of github.com/mokevnin/pgcleaner

### Features

* add SQLite and MySQL dialects ([1dc6e6c](https://github.com/mokevnin/dbcleaner/commit/1dc6e6c1e0afec7cd88a3b9dadf8f9d2192fe188))
* split the PostgreSQL parts into a Dialect ([d9bbf50](https://github.com/mokevnin/dbcleaner/commit/d9bbf5075dbf0a2383b6974a312d9e213aecac79))


### Miscellaneous Chores

* release the first stable version ([fa42a02](https://github.com/mokevnin/dbcleaner/commit/fa42a029df113cc9819c16fae504d35d0549f599))


### Code Refactoring

* rename pgcleaner to dbcleaner ([4d197d7](https://github.com/mokevnin/dbcleaner/commit/4d197d76567eaeb40ddfeb810f69b3d8a494fb97))

## [0.2.0](https://github.com/mokevnin/pgcleaner/compare/v0.1.0...v0.2.0) (2026-10-18)

### Features

- lease a worker database cloned from a template, for parallel test packages ([538d893](https://github.com/mokevnin/pgcleaner/commit/538d893eae1e8d22373736c2e4c5858c2502e254))

## 0.1.0 (2026-10-18)

### Features

- database cleaner with transaction, truncation and deletion strategies ([c962179](https://github.com/mokevnin/pgcleaner/commit/c962179f4780101f7e40b9fb35b6da4106207638))
