package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func setup(t *testing.T) (*db.Store, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := &server.Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Mux: http.NewServeMux()}
	s.Mux.Handle("GET /whoami", Require(s, ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, KeyName(r.Context()))
	})))
	srv := httptest.NewServer(s.Mux)
	t.Cleanup(srv.Close)
	return st, srv
}

func get(t *testing.T, srv *httptest.Server, header string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/whoami", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRequireChecksKeyAndScope(t *testing.T) {
	st, srv := setup(t)
	ctx := context.Background()
	reader, _, err := Create(ctx, st.RW, "reader", []string{ScopeRead, ScopePropose}, false, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	proposer, _, err := Create(ctx, st.RW, "proposer", []string{ScopePropose}, false, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		header string
		code   int
		body   string
	}{
		{"", http.StatusUnauthorized, "нужен ключ"},
		{"Basic " + reader, http.StatusUnauthorized, "нужен ключ"},
		{"Bearer", http.StatusUnauthorized, "нужен ключ"},
		{"Bearer ldg_reader_" + strings.Repeat("a", 52), http.StatusUnauthorized, "не подходит"},
		{"Bearer " + reader + "x", http.StatusUnauthorized, "не подходит"},
		{"Bearer " + proposer, http.StatusForbidden, "нет права read"},
		{"Bearer " + reader, http.StatusOK, "reader"},
		{"bearer  " + reader + " ", http.StatusOK, "reader"},
	} {
		code, body := get(t, srv, c.header)
		if code != c.code || !strings.Contains(body, c.body) {
			t.Errorf("%.30q: %d %s, want %d %s", c.header, code, body, c.code, c.body)
		}
	}
	if _, err := Revoke(ctx, st.RW, "reader", ts.Now()); err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, srv, "Bearer "+reader); code != http.StatusUnauthorized || !strings.Contains(body, "отозван") {
		t.Fatalf("revoked key: %d %s", code, body)
	}
	if _, err := Revoke(ctx, st.RW, "nobody", ts.Now()); !errors.Is(err, ErrNoKey) {
		t.Fatalf("revoke of a missing key: %v", err)
	}
}

func TestLastUsedAtAtMostOnceAMinute(t *testing.T) {
	st, srv := setup(t)
	t0, _ := ts.Parse("2026-11-04T12:00:00+03:00")
	now := t0
	ts.Now = func() time.Time { return now }
	t.Cleanup(func() { ts.Now = time.Now })
	token, _, err := Create(context.Background(), st.RW, "reader", []string{ScopeRead}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	lastUsed := func() string {
		var v string
		st.RW.QueryRow(`SELECT coalesce(last_used_at, '') FROM keys WHERE name = 'reader'`).Scan(&v)
		return v
	}
	for _, step := range []struct {
		after time.Duration
		want  time.Duration
	}{{0, 0}, {30 * time.Second, 0}, {59 * time.Second, 0}, {61 * time.Second, 61 * time.Second}} {
		now = t0.Add(step.after)
		if code, _ := get(t, srv, "Bearer "+token); code != http.StatusOK {
			t.Fatalf("request: %d", code)
		}
		if got, want := lastUsed(), ts.Format(t0.Add(step.want)); got != want {
			t.Fatalf("after %s: last_used_at %s, want %s", step.after, got, want)
		}
	}
}

func TestCreateValidatesAndReplaces(t *testing.T) {
	st, srv := setup(t)
	ctx := context.Background()
	for _, name := range []string{"", "Bobr", "bobr_2", "-bobr", "a b", strings.Repeat("a", 33)} {
		if _, _, err := Create(ctx, st.RW, name, []string{ScopeRead}, false, ts.Now()); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if _, _, err := Create(ctx, st.RW, "bobr", []string{"root"}, false, ts.Now()); err == nil {
		t.Error("unknown scope accepted")
	}
	if got, err := ParseScopes(" decide,read ,read"); err != nil || strings.Join(got, ",") != "read,decide" {
		t.Errorf("ParseScopes: %v %v", got, err)
	}
	for _, bad := range []string{"", " , ", "read,root"} {
		if _, err := ParseScopes(bad); err == nil {
			t.Errorf("ParseScopes(%q) accepted", bad)
		}
	}

	first, k, err := Create(ctx, st.RW, "openclaw-main", []string{ScopeRead, ScopeDecide}, false, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "ldg_openclaw-main_") || len(first) != len("ldg_openclaw-main_")+52 ||
		!strings.HasPrefix(first, k.Prefix) || !k.Has(ScopeDecide) {
		t.Fatalf("key %q, record %+v", first[:len(k.Prefix)], k)
	}
	var stored string
	st.RW.QueryRow(`SELECT hash FROM keys WHERE name = 'openclaw-main'`).Scan(&stored)
	if stored != Hash(first) || strings.Contains(stored, first[len(k.Prefix):]) {
		t.Fatal("the database must keep the hash, not the key")
	}
	if _, _, err := Create(ctx, st.RW, "openclaw-main", []string{ScopeRead}, false, ts.Now()); !errors.Is(err, ErrExists) {
		t.Fatalf("second key with one name: %v", err)
	}
	second, _, err := Create(ctx, st.RW, "openclaw-main", []string{ScopeRead}, true, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, srv, "Bearer "+first); code != http.StatusUnauthorized {
		t.Fatalf("replaced key still works: %d", code)
	}
	if code, body := get(t, srv, "Bearer "+second); code != http.StatusOK || body != "openclaw-main" {
		t.Fatalf("new key: %d %s", code, body)
	}
	keys, err := List(ctx, st.RO)
	if err != nil || len(keys) != 2 || keys[0].Name != "openclaw-main" || keys[0].RevokedAt != "" ||
		!strings.HasPrefix(keys[1].Name, "openclaw-main~") || keys[1].RevokedAt == "" {
		t.Fatalf("list: %+v %v", keys, err)
	}
}
