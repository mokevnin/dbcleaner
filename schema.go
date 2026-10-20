package dbcleaner

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// schema is what the table strategies need to know about the database: its ordinary
// tables and which of them reference which.
type schema struct {
	tables []string
	// references[child] lists the tables child holds a foreign key to.
	references map[string][]string
}

// loadSchema reads the tables of the current schema once per Cleaner; the schema
// does not change under a running suite.
func (c *Cleaner) loadSchema(ctx context.Context, db *sql.DB) (*schema, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.schema != nil {
		return c.schema, nil
	}
	s := &schema{references: map[string][]string{}}

	// Partitions are left out: cleaning the parent cleans them.
	tables, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: list tables: %w", err)
	}
	defer func() { _ = tables.Close() }()
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			return nil, fmt.Errorf("dbcleaner: list tables: %w", err)
		}
		s.tables = append(s.tables, name)
	}
	if err := tables.Err(); err != nil {
		return nil, fmt.Errorf("dbcleaner: list tables: %w", err)
	}

	edges, err := db.QueryContext(ctx, `
		SELECT child.relname, parent.relname
		FROM pg_constraint co
		JOIN pg_class child ON child.oid = co.conrelid
		JOIN pg_class parent ON parent.oid = co.confrelid
		JOIN pg_namespace n ON n.oid = child.relnamespace
		WHERE co.contype = 'f' AND n.nspname = current_schema() AND child.oid <> parent.oid`)
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: list foreign keys: %w", err)
	}
	defer func() { _ = edges.Close() }()
	for edges.Next() {
		var child, parent string
		if err := edges.Scan(&child, &parent); err != nil {
			return nil, fmt.Errorf("dbcleaner: list foreign keys: %w", err)
		}
		s.references[child] = append(s.references[child], parent)
	}
	if err := edges.Err(); err != nil {
		return nil, fmt.Errorf("dbcleaner: list foreign keys: %w", err)
	}

	c.schema = s
	return s, nil
}

// childrenFirst orders tables so that a table comes before every table it
// references, which is the order DELETE can run in.
func (s *schema) childrenFirst(tables []string) ([]string, error) {
	in := map[string]bool{}
	for _, table := range tables {
		in[table] = true
	}
	// blockers[t] counts the tables in the set that still hold a foreign key to t.
	blockers := map[string]int{}
	for _, child := range tables {
		for _, parent := range s.references[child] {
			if in[parent] {
				blockers[parent]++
			}
		}
	}

	var ordered []string
	ready := slices.DeleteFunc(slices.Clone(tables), func(t string) bool { return blockers[t] > 0 })
	for len(ready) > 0 {
		table := ready[0]
		ready = ready[1:]
		ordered = append(ordered, table)
		for _, parent := range s.references[table] {
			if !in[parent] {
				continue
			}
			blockers[parent]--
			if blockers[parent] == 0 {
				ready = append(ready, parent)
			}
		}
	}
	if len(ordered) != len(tables) {
		return nil, fmt.Errorf("dbcleaner: foreign keys form a cycle among the tables to delete from; use Truncation")
	}
	return ordered, nil
}
