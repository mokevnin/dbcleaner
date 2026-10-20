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
	tables, err := c.dialect.Tables(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: list tables: %w", err)
	}
	references, err := c.dialect.References(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dbcleaner: list foreign keys: %w", err)
	}
	s := &schema{tables: tables, references: references}
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
