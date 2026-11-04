package agentsql

import (
	"strings"
	"testing"
)

func TestStatementKeepsOneStatement(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                               "SELECT 1",
		"  SELECT 1 ;  ":                         "SELECT 1",
		"SELECT 1;;\n; ":                         "SELECT 1",
		"SELECT 1; -- note; DELETE FROM meta":    "SELECT 1",
		"SELECT 1; /* DELETE FROM meta; */":      "SELECT 1",
		"SELECT 1 -- note; DELETE FROM meta":     "SELECT 1 -- note; DELETE FROM meta",
		"SELECT 'a;b', \"c;d\", `e;f`, [g;h]":    "SELECT 'a;b', \"c;d\", `e;f`, [g;h]",
		"SELECT 'it''s; fine'; ":                 "SELECT 'it''s; fine'",
		"SELECT \"x\"\"; y\" FROM t":             "SELECT \"x\"\"; y\" FROM t",
		"/* lead; */ SELECT 2":                   "/* lead; */ SELECT 2",
		"; SELECT 3":                             "SELECT 3",
		"SELECT x'00ff';":                        "SELECT x'00ff'",
		"SELECT 1 /* unclosed; DELETE FROM meta": "SELECT 1 /* unclosed; DELETE FROM meta",
		"SELECT 'unclosed; DELETE FROM meta":     "SELECT 'unclosed; DELETE FROM meta",
	} {
		got, err := Statement(in)
		if err != nil || got != want {
			t.Errorf("Statement(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestStatementRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"  ;; ",
		"-- only a comment",
		"/* only */ ; -- comments",
		"SELECT 1; DELETE FROM meta",
		"SELECT 1;DELETE FROM meta;",
		"SELECT ';'; SELECT 2",
		"SELECT 1 -- c\n; DROP TABLE meta",
		"SELECT [a;b]; ATTACH 'x' AS y",
		"CREATE TRIGGER tr AFTER INSERT ON meta BEGIN DELETE FROM meta; END",
		"SELECT 1\x00; DELETE FROM meta",
		"SELECT '" + strings.Repeat("x", MaxSQL) + "'",
	} {
		if got, err := Statement(in); err == nil {
			t.Errorf("Statement(%.60q) = %.60q, want an error", in, got)
		}
	}
}
