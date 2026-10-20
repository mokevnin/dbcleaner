// Package mysql is the MySQL (and MariaDB) dialect of dbcleaner, on the go-sql-driver/mysql
// driver. The driver's DSN is not a URL (user:pass@tcp(host:3306)/app_test), so there is no
// scheme to register it for: pass [Dialect] to [dbcleaner.WithDialect].
//
//	dbcleaner.New("root@tcp(127.0.0.1:3306)/app_test", dbcleaner.WithDialect(mysql.Dialect()))
//
// MySQL commits implicitly before and after DDL, so a test running under
// [dbcleaner.Transaction] must not create or alter tables. It does not support
// [dbcleaner.WithTemplate]: MySQL cannot clone a database.
package mysql

import (
	"context"
	"database/sql"
	"strings"

	_ "github.com/go-sql-driver/mysql" // the driver
	"github.com/mokevnin/dbcleaner"
)

// Dialect is the MySQL dialect.
func Dialect() dbcleaner.Dialect { return dialect{} }

type dialect struct{}

func (dialect) Driver() string { return "mysql" }

func (dialect) Quote(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}

func (dialect) CurrentDatabase(ctx context.Context, db *sql.DB) (string, error) {
	var name sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&name); err != nil {
		return "", err
	}
	return name.String, nil
}

func (dialect) Tables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (dialect) References(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT table_name, referenced_table_name
		FROM information_schema.referential_constraints
		WHERE constraint_schema = DATABASE() AND table_name <> referenced_table_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	references := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		references[child] = append(references[child], parent)
	}
	return references, rows.Err()
}

// Truncate runs one TRUNCATE per table (MySQL has no list form) on one connection with
// foreign_key_checks off, which TRUNCATE refuses to run without when a foreign key points at
// the table. TRUNCATE resets AUTO_INCREMENT. The setting is put back as it was.
func (d dialect) Truncate(ctx context.Context, db *sql.DB, tables []string) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	var checks int
	if err := conn.QueryRowContext(ctx, `SELECT @@foreign_key_checks`).Scan(&checks); err != nil {
		return err
	}
	if checks != 0 {
		if _, err := conn.ExecContext(ctx, `SET foreign_key_checks = 0`); err != nil {
			return err
		}
		defer func() {
			if _, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `SET foreign_key_checks = 1`); err == nil {
				err = restoreErr
			}
		}()
	}
	for _, table := range tables {
		if _, err := conn.ExecContext(ctx, `TRUNCATE TABLE `+d.Quote(table)); err != nil {
			return err
		}
	}
	return nil
}
