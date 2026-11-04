package agentsql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newKey(t *testing.T, s *db.Store, name string, scopes ...string) string {
	t.Helper()
	token, _, err := auth.Create(context.Background(), s.RW, name, scopes, false, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func call(t *testing.T, url, method, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: answer is not JSON: %v", method, url, err)
	}
	return resp.StatusCode, out
}

type logRow struct {
	key, sql string
	rows     int
	err      sql.NullString
}

func queryLog(t *testing.T, s *db.Store) []logRow {
	t.Helper()
	rows, err := s.RW.Query(`SELECT key_name, sql, rows, error FROM query_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.key, &r.sql, &r.rows, &r.err); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestSQLEndpointAnswersAndLogsEveryCall(t *testing.T) {
	st := openStore(t)
	eng := openEngine(t, st)
	eng.Timeout = 300 * time.Millisecond
	s := &server.Server{Store: st, Log: quiet, Mux: http.NewServeMux()}
	Mount(s, eng)
	srv := httptest.NewServer(s.Mux)
	defer srv.Close()
	reader := newKey(t, st, "reader", auth.ScopeRead)
	proposer := newKey(t, st, "proposer", auth.ScopePropose)
	url := srv.URL + "/v1/sql"

	if code, _ := call(t, url, "POST", "", `{"sql": "SELECT 1"}`); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := call(t, url, "POST", proposer, `{"sql": "SELECT 1"}`); code != http.StatusForbidden {
		t.Fatalf("key without read: %d", code)
	}

	code, out := call(t, url, "POST", reader, `{"sql": "SELECT key, value FROM meta"}`)
	if code != http.StatusOK || out["ok"] != true || out["truncated"] != false || len(out["rows"].([]any)) != 1 {
		t.Fatalf("select: %d %v", code, out)
	}
	if _, ok := out["durationMs"].(float64); !ok || len(out["columns"].([]any)) != 2 {
		t.Fatalf("select answer: %v", out)
	}
	code, out = call(t, url, "POST", reader, `{"sql": "DELETE FROM meta"}`)
	if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "DELETE") {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, _ = call(t, url, "POST", reader, `{"sql": "SELECT 1; DELETE FROM meta"}`); code != http.StatusBadRequest {
		t.Fatalf("two statements: %d", code)
	}
	if code, _ = call(t, url, "POST", reader, `not json`); code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", code)
	}
	code, out = call(t, url, "POST", reader,
		`{"sql": "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c WHERE x = 1 OR x < 0"}`)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "прерван") {
		t.Fatalf("timeout: %d %v", code, out)
	}

	got := queryLog(t, st)
	if len(got) != 5 {
		t.Fatalf("query_log has %d rows, want 5: %+v", len(got), got)
	}
	for i, r := range got {
		if r.key != "reader" {
			t.Errorf("row %d: key %q", i, r.key)
		}
		if (i == 0) == r.err.Valid {
			t.Errorf("row %d: error %v", i, r.err)
		}
	}
	if got[0].rows != 1 || got[0].sql != "SELECT key, value FROM meta" || got[1].sql != "DELETE FROM meta" {
		t.Errorf("logged: %+v", got[:2])
	}
	if n := 0; st.RW.QueryRow(`SELECT count(*) FROM meta`).Scan(&n) != nil || n != 1 {
		t.Fatalf("meta has %d rows", n)
	}
}

func TestSchemaEndpoint(t *testing.T) {
	st := openStore(t)
	s := &server.Server{Store: st, Log: quiet, Mux: http.NewServeMux()}
	Mount(s, openEngine(t, st))
	srv := httptest.NewServer(s.Mux)
	defer srv.Close()
	reader := newKey(t, st, "reader", auth.ScopeRead)
	if code, _ := call(t, srv.URL+"/v1/schema", "GET", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	var resp SchemaResponse
	c := client.New(config.Client{URL: srv.URL, Key: reader})
	if err := c.Do(context.Background(), "GET", "/v1/schema", nil, &resp); err != nil {
		t.Fatal(err)
	}
	tables := map[string]Table{}
	for _, tb := range resp.Tables {
		if strings.HasPrefix(tb.Name, "sqlite_") {
			t.Errorf("internal table %s listed", tb.Name)
		}
		tables[tb.Name] = tb
	}
	col := func(table, name string) Column {
		for _, c := range tables[table].Columns {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no column %s.%s", table, name)
		return Column{}
	}
	if tables["events"].Description == "" || col("events", "source").Description == "" {
		t.Errorf("events lacks descriptions: %+v", tables["events"])
	}
	if c := col("lines", "entry_id"); c.References != "entries.id" || !c.NotNull {
		t.Errorf("lines.entry_id: %+v", c)
	}
	if c := col("keys", "id"); !c.PrimaryKey || c.Type != "INTEGER" {
		t.Errorf("keys.id: %+v", c)
	}
	if col("keys", "hash").Description == "" || col("query_log", "error").Description == "" {
		t.Errorf("keys and query_log lack the descriptions of 0500")
	}
	if tables["v_balances"].Kind != "view" || col("v_balances", "balance").Name == "" {
		t.Errorf("v_balances: %+v", tables["v_balances"])
	}
}

// The service built by server.New serves /v1/sql to the CLI's client.
func TestServiceAnswersTheClient(t *testing.T) {
	st := openStore(t)
	srv := httptest.NewServer(server.New(st, quiet, "test").Mux)
	defer srv.Close()
	ctx := context.Background()
	key := newKey(t, st, "claude-test", auth.ScopeRead)

	var resp SQLResponse
	c := client.New(config.Client{URL: srv.URL + "/", Key: key})
	if err := c.Do(ctx, "POST", "/v1/sql", SQLRequest{SQL: "SELECT 9007199254740993 AS big"}, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.Rows) != 1 || resp.Rows[0][0] != json.Number("9007199254740993") {
		t.Fatalf("answer: %+v", resp)
	}
	err := c.Do(ctx, "POST", "/v1/sql", SQLRequest{SQL: "DROP TABLE meta"}, &resp)
	var ce *client.Error
	if !errors.As(err, &ce) || ce.Status != http.StatusForbidden || !strings.Contains(ce.Message, "DROP") {
		t.Fatalf("drop: %v", err)
	}
	bad := client.New(config.Client{URL: srv.URL, Key: "ldg_claude-test_" + strings.Repeat("a", 52)})
	if err := bad.Do(ctx, "GET", "/v1/schema", nil, nil); !errors.As(err, &ce) || ce.Status != http.StatusUnauthorized {
		t.Fatalf("wrong key: %v", err)
	}
	// A proxy's HTML error still reads as an error with its status.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.Copy(w, bytes.NewBufferString("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer proxy.Close()
	err = client.New(config.Client{URL: proxy.URL, Key: key}).Do(ctx, "GET", "/v1/schema", nil, nil)
	if !errors.As(err, &ce) || ce.Status != http.StatusBadGateway || !strings.Contains(ce.Message, "502") {
		t.Fatalf("proxy error: %v", err)
	}
}
