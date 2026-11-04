package agentsql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
)

func openStore(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func openEngine(t *testing.T, s *db.Store) *Engine {
	t.Helper()
	e := Open(s.Path)
	t.Cleanup(func() { e.Close() })
	return e
}

// snapshot is everything a write could change: the schema, the seeded rows, the header.
func snapshot(t *testing.T, s *db.Store) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT type || ' ' || name || ' ' || coalesce(sql, '') FROM sqlite_master ORDER BY name`,
		`SELECT key || '=' || value FROM meta ORDER BY key`,
		`SELECT code || ' ' || name FROM accounts ORDER BY code`,
		`SELECT count(*) FROM keys`,
		`PRAGMA user_version`,
		`PRAGMA journal_mode`,
	} {
		rows, err := s.RW.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			rows.Scan(&v)
			b.WriteString(v + "\n")
		}
		rows.Close()
	}
	return b.String()
}

func TestWritesAndEscapesAreRefused(t *testing.T) {
	s := openStore(t)
	e := openEngine(t, s)
	ctx := context.Background()
	if _, err := s.RW.Exec(`INSERT INTO meta(key, value) VALUES ('probe', 'synthetic')`); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s)
	dir := t.TempDir()
	attached, vacuumed := filepath.Join(dir, "attached.db"), filepath.Join(dir, "copy.db")
	cases := []struct {
		sql  string
		kind Kind
		says string // part of the message
	}{
		{`INSERT INTO meta(key, value) VALUES ('x', 'y')`, Denied, "INSERT meta"},
		{`UPDATE meta SET value = 'changed'`, Denied, "UPDATE meta"},
		{`DELETE FROM meta`, Denied, "DELETE meta"},
		{`REPLACE INTO meta(key, value) VALUES ('probe', 'changed')`, Denied, "INSERT"},
		{`INSERT INTO meta SELECT 'a', 'b' RETURNING *`, Denied, "INSERT"},
		{`WITH x AS (SELECT 1) DELETE FROM meta`, Denied, "DELETE"},
		{`CREATE TABLE t(x)`, Denied, "CREATE, изменение схемы"},
		{`CREATE TEMP TABLE t(x)`, Denied, "CREATE, изменение схемы"},
		{`CREATE INDEX meta_value ON meta(value)`, Denied, "CREATE"},
		{`CREATE VIEW v AS SELECT 1`, Denied, "CREATE"},
		{`CREATE VIRTUAL TABLE f USING fts5(x)`, Denied, "CREATE"},
		{`DROP TABLE meta`, Denied, "DROP, изменение схемы"},
		{`DROP VIEW v_balances`, Denied, "DROP"},
		{`ALTER TABLE meta ADD COLUMN x TEXT`, Denied, "ALTER TABLE"},
		{fmt.Sprintf(`ATTACH DATABASE '%s' AS x`, attached), Denied, "ATTACH"},
		{`DETACH DATABASE temp`, Denied, "DETACH"},
		{`PRAGMA query_only = 0`, Denied, "PRAGMA query_only 0"},
		{`PRAGMA query_only = OFF`, Denied, "PRAGMA query_only"},
		{`PRAGMA user_version = 5`, Denied, "PRAGMA user_version 5"},
		{`PRAGMA journal_mode = DELETE`, Denied, "PRAGMA journal_mode"},
		{`PRAGMA writable_schema = ON`, Denied, "PRAGMA writable_schema"},
		{`PRAGMA table_info(meta)`, Denied, "PRAGMA table_info"},
		{`SELECT * FROM pragma_table_info('meta')`, Denied, "PRAGMA"},
		{`BEGIN IMMEDIATE`, Denied, "BEGIN"},
		{`COMMIT`, Denied, "COMMIT"},
		{`ROLLBACK`, Denied, "ROLLBACK"},
		{`SAVEPOINT s`, Denied, "SAVEPOINT"},
		{`VACUUM`, Denied, "ATTACH"},
		{fmt.Sprintf(`VACUUM INTO '%s'`, vacuumed), Denied, "ATTACH"},
		{`REINDEX`, Denied, "REINDEX"},
		{`ANALYZE`, Denied, "ANALYZE"},
		{`EXPLAIN DELETE FROM meta`, Denied, "DELETE meta"},
		{`EXPLAIN PRAGMA query_only = 0`, Denied, "PRAGMA query_only"},
		{`SELECT 1; DELETE FROM meta`, Invalid, "одной"},
		{`SELECT 1; SELECT 2`, Invalid, "одной"},
		{`CREATE TRIGGER tr AFTER INSERT ON meta BEGIN DELETE FROM meta; END`, Invalid, ""},
	}
	for _, c := range cases {
		res, err := e.Run(ctx, c.sql)
		var qe *Error
		if !errors.As(err, &qe) || qe.Kind != c.kind {
			t.Errorf("%s: want kind %d, got %v (result %v)", c.sql, c.kind, err, res)
			continue
		}
		if !strings.Contains(qe.Msg, c.says) {
			t.Errorf("%s: message %q lacks %q", c.sql, qe.Msg, c.says)
		}
	}
	if after := snapshot(t, s); after != before {
		t.Fatalf("data changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, f := range []string{attached, vacuumed} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after a refused statement: %v", f, err)
		}
	}
	// A refusal leaves the pool usable.
	res, err := e.Run(ctx, `SELECT value FROM meta WHERE key = 'probe'`)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "synthetic" {
		t.Fatalf("read after refusals: %v %v", res, err)
	}
}

func TestReadsWork(t *testing.T) {
	s := openStore(t)
	e := openEngine(t, s)
	ctx := context.Background()
	for _, c := range []struct {
		sql  string
		rows int
		cols string
	}{
		{`SELECT key, value FROM meta`, 1, "key,value"},
		{`SELECT account, balance FROM v_balances WHERE account = 'bybit'`, 1, "account,balance"},
		{`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 5) SELECT sum(x) AS s FROM c`, 1, "s"},
		{`SELECT json_extract('{"a": 2}', '$.a') AS a; -- trailing note`, 1, "a"},
		{`SELECT 'a;b' AS s, x'00ff' AS b, 1e999 AS inf, NULL AS n`, 1, "s,b,inf,n"},
		{`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'meta'`, 1, "name"},
		{`EXPLAIN QUERY PLAN SELECT * FROM events WHERE source = 'bybit'`, -1, "id,parent,notused,detail"},
		{`VALUES (1, 'a'), (2, 'b')`, 2, "column1,column2"},
		{`SELECT code FROM accounts WHERE 0`, 0, "code"},
	} {
		res, err := e.Run(ctx, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if c.rows >= 0 && len(res.Rows) != c.rows || strings.Join(res.Columns, ",") != c.cols || res.Truncated {
			t.Errorf("%s: columns %v, %d rows, truncated %v", c.sql, res.Columns, len(res.Rows), res.Truncated)
		}
	}
	res, err := e.Run(ctx, `SELECT 7 * 6 AS n, 'a;b' AS s, 1e999 AS inf`)
	if err != nil || res.Rows[0][0] != int64(42) || res.Rows[0][1] != "a;b" || res.Rows[0][2] != "+Inf" {
		t.Fatalf("values: %#v %v", res, err)
	}
	if _, err := e.Run(ctx, `SELECT nonsense FROM nowhere`); err == nil {
		t.Fatal("a bad query succeeded")
	} else if qe := (*Error)(nil); !errors.As(err, &qe) || qe.Kind != Failed {
		t.Fatalf("bad query: %v", err)
	}
}

func TestTimeoutStopsEveryStep(t *testing.T) {
	s := openStore(t)
	e := openEngine(t, s)
	e.Timeout = 300 * time.Millisecond
	ctx := context.Background()
	const forever = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) `
	for _, q := range []string{
		forever + `SELECT count(*) FROM c`,               // never yields a first row
		forever + `SELECT x FROM c WHERE x = 1 OR x < 0`, // first row at once, the second never
	} {
		start := time.Now()
		res, err := e.Run(ctx, q)
		elapsed := time.Since(start)
		var qe *Error
		if !errors.As(err, &qe) || qe.Kind != Timeout {
			t.Fatalf("%s: want a timeout, got %v %v", q, res, err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("%s: stopped after %s", q, elapsed)
		}
	}
	res, err := e.Run(ctx, `SELECT 1`)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("query after timeouts: %v %v", res, err)
	}
}

func TestRowsAreCapped(t *testing.T) {
	s := openStore(t)
	e := openEngine(t, s)
	e.MaxRows = 5
	ctx := context.Background()
	for q, want := range map[string]bool{
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c LIMIT 100) SELECT x FROM c`: true,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c`:           true,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c LIMIT 5) SELECT x FROM c`:   false,
	} {
		res, err := e.Run(ctx, q)
		if err != nil || len(res.Rows) != 5 || res.Truncated != want || res.Rows[4][0] != int64(5) {
			t.Fatalf("%s: %v rows, truncated %v, %v", q, len(res.Rows), res.Truncated, err)
		}
	}
	e.MaxRows, e.MaxBytes = 100, 1000
	res, err := e.Run(ctx, `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c LIMIT 50) SELECT hex(zeroblob(400)) FROM c`)
	if err != nil || len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("byte cap: %d rows, truncated %v, %v", len(res.Rows), res.Truncated, err)
	}
	if _, err := e.Run(ctx, `SELECT zeroblob(100000000)`); err == nil {
		t.Fatal("a 100 MB blob was built")
	}
}
