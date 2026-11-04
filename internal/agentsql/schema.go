package agentsql

import (
	"context"
	"database/sql"
	"strings"
)

// Table is a table or a view with its columns and descriptions from schema_docs.
type Table struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // table | view
	Description string   `json:"description,omitempty"`
	Columns     []Column `json:"columns"`
}

// Column of a table; References is "table.column" of a foreign key.
type Column struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	NotNull     bool   `json:"notNull,omitempty"`
	PrimaryKey  bool   `json:"primaryKey,omitempty"`
	References  string `json:"references,omitempty"`
	Description string `json:"description,omitempty"`
}

// ReadSchema lists the tables, then the views, by name, without SQLite's and Litestream's own.
func ReadSchema(ctx context.Context, db *sql.DB) ([]Table, error) {
	docs := map[[2]string]string{}
	rows, err := db.QueryContext(ctx, `SELECT tbl, col, description FROM schema_docs`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t, c, d string
		if err := rows.Scan(&t, &c, &d); err != nil {
			rows.Close()
			return nil, err
		}
		docs[[2]string{t, c}] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `SELECT name, type FROM sqlite_master WHERE type IN ('table', 'view') ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	var tables []Table
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name, &t.Kind); err != nil {
			rows.Close()
			return nil, err
		}
		if !strings.HasPrefix(t.Name, "sqlite_") && !strings.HasPrefix(t.Name, "_litestream_") {
			t.Description = docs[[2]string{t.Name, ""}]
			tables = append(tables, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range tables {
		t := &tables[i]
		refs := map[string]string{}
		rows, err := db.QueryContext(ctx, `SELECT "from", "table", coalesce("to", '') FROM pragma_foreign_key_list(?)`, t.Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var from, table, to string
			if err := rows.Scan(&from, &table, &to); err != nil {
				rows.Close()
				return nil, err
			}
			refs[from] = strings.TrimSuffix(table+"."+to, ".")
		}
		rows.Close()
		rows, err = db.QueryContext(ctx, `SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, t.Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c Column
			var pk int
			if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &pk); err != nil {
				rows.Close()
				return nil, err
			}
			c.PrimaryKey = pk > 0
			c.References = refs[c.Name]
			c.Description = docs[[2]string{t.Name, c.Name}]
			t.Columns = append(t.Columns, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return tables, nil
}
