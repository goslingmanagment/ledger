package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/server"
)

func TestSyncAndMirrorThroughTheAPI(t *testing.T) {
	s, ctx := openDB(t)
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('accrual_from', '2026-09-01', 'x'), ('models', 'Alfa', 'x')`); err != nil {
		t.Fatal(err)
	}
	srv := server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ts := httptest.NewServer(srv.Mux)
	defer ts.Close()
	key := func(name string, scopes ...string) string {
		tok, _, err := auth.Create(context.Background(), s.RW, name, scopes, false, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	sync, reader := key("sync", "import"), key("agent", "read")
	call := func(method, path, tok string, body any) (int, []byte) {
		t.Helper()
		var r io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			r = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, ts.URL+path, r)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	text := sample()
	bundle := FromFile(text, "c1", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	bundle.Versions[0].Ref, bundle.FinalRef = "c1", "c1"
	if code, _ := call("POST", "/v1/import/ledger-md", reader, map[string]any{"bundle": bundle}); code != 403 {
		t.Fatalf("a read key imported: %d", code)
	}
	code, out := call("POST", "/v1/import/ledger-md", sync, map[string]any{"bundle": bundle})
	if code != 200 {
		t.Fatalf("import %d %s", code, out)
	}
	code, out = call("POST", "/v1/import/ledger-md", sync, map[string]any{"bundle": bundle})
	var again struct {
		Report Report `json:"report"`
	}
	if err := json.Unmarshal(out, &again); err != nil || code != 200 || again.Report.Skipped != 1 || again.Report.Entries != 0 {
		t.Fatalf("repeat %d %s", code, out)
	}
	if code, body := call("GET", "/v1/export/ledger-md", reader, nil); code != 200 || string(body) != text {
		t.Fatalf("mirror %d differs", code)
	}
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('mode', 'live', 'x')`); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("POST", "/v1/import/ledger-md", sync, map[string]any{"bundle": bundle}); code != 409 {
		t.Fatalf("import into the canon: %d", code)
	}
	_ = ctx
}
