package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestPotsHubFromDryRunThroughCLI(t *testing.T) {
	t.Setenv("LEDGER_DB", filepath.Join(t.TempDir(), "demo.db"))
	t.Setenv("LEDGER_HUB_AGENT_KEY", "demo")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/pot-distributions/feed" || r.URL.Query().Has("since") {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"changes":[],"held":[],"next":"0","more":false,"serverNow":"2026-11-17T10:00:00Z"}`)
	}))
	defer srv.Close()
	t.Setenv("LEDGER_HUB_BASE_URL", srv.URL)
	ctx := context.Background()
	call := func(name string, args ...string) error { return commands[name].Run(ctx, args) }
	if err := call("payroll pots-hub-from", "2026-12-01", "--dry-run"); err == nil {
		t.Fatal("enabled without K2")
	}
	if err := call("hub pots-sync"); err != nil {
		t.Fatal(err)
	}
	if err := call("payroll pots-hub-from", "2026-12-01", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var from string
	if err := store.RO.QueryRow("SELECT value FROM settings WHERE key='pot_shares_hub_from'").Scan(&from); err != nil || from != "" {
		t.Fatal(from, err)
	}
}

func TestPotsHubFromOffRunsLiveSyncBeforeDisabling(t *testing.T) {
	t.Setenv("LEDGER_DB", filepath.Join(t.TempDir(), "demo.db"))
	t.Setenv("LEDGER_HUB_AGENT_KEY", "demo")
	mode := "empty"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch mode {
		case "empty":
			fmt.Fprint(w, `{"changes":[],"held":[],"next":"0","more":false,"serverNow":"2026-11-17T10:00:00Z"}`)
		case "fail":
			w.WriteHeader(http.StatusForbidden)
		case "posted":
			fmt.Fprint(w, `{"changes":[{"seq":1,"change":"posted","at":"2026-12-01T10:00:00Z","distribution":{"id":9,"pot":{"code":"demo-pool","name":"Тестовый","modelSlug":"alpha"},"day":"2026-12-01","formula":{"kind":"equal","params":{}},"baseMills":10000,"inflows":[{"transactionId":1,"pageLabel":"page-a","baseMills":10000}],"lines":[{"personCode":"alfa","baseMills":10000}]}}],"held":[],"next":"1","more":false,"serverNow":"2026-12-01T10:00:00Z"}`)
		}
	}))
	defer srv.Close()
	t.Setenv("LEDGER_HUB_BASE_URL", srv.URL)
	ctx := context.Background()
	call := func(name string, args ...string) error { return commands[name].Run(ctx, args) }
	if err := call("hub pots-sync"); err != nil {
		t.Fatal(err)
	}
	if err := call("payroll pots-hub-from", "2026-12-01"); err != nil {
		t.Fatal(err)
	}
	// Workboard outage blocks --off even though the local mirror was just synced.
	mode = "fail"
	if err := call("payroll pots-hub-from", "--off"); err == nil {
		t.Fatal("--off succeeded during Workboard outage")
	}
	// A newly posted distribution on Workboard blocks --off via the live sync.
	mode = "posted"
	if err := call("payroll pots-hub-from", "--off"); err == nil {
		t.Fatal("--off succeeded after Workboard posted a distribution")
	}
	if hits < 3 {
		t.Fatalf("expected live Workboard checks, got %d requests", hits)
	}
}

func TestPotsHubFromOffDryRunWritesNothingToDatabase(t *testing.T) {
	t.Setenv("LEDGER_DB", filepath.Join(t.TempDir(), "demo.db"))
	t.Setenv("LEDGER_HUB_AGENT_KEY", "demo")
	mode := "empty"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "empty":
			fmt.Fprint(w, `{"changes":[],"held":[],"next":"0","more":false,"serverNow":"2026-11-17T10:00:00Z"}`)
		case "fail":
			w.WriteHeader(http.StatusBadGateway)
		case "posted":
			fmt.Fprint(w, `{"changes":[{"seq":1,"change":"posted","at":"2026-12-01T10:00:00Z","distribution":{"id":9,"pot":{"code":"demo-pool","name":"Тестовый","modelSlug":"alpha"},"day":"2026-12-01","formula":{"kind":"equal","params":{}},"baseMills":10000,"inflows":[{"transactionId":1,"pageLabel":"page-a","baseMills":10000}],"lines":[{"personCode":"alfa","baseMills":10000}]}}],"held":[],"next":"1","more":false,"serverNow":"2026-12-01T10:00:00Z"}`)
		}
	}))
	defer srv.Close()
	t.Setenv("LEDGER_HUB_BASE_URL", srv.URL)
	ctx := context.Background()
	call := func(name string, args ...string) error { return commands[name].Run(ctx, args) }
	if err := call("hub pots-sync"); err != nil {
		t.Fatal(err)
	}
	if err := call("payroll pots-hub-from", "2026-12-01"); err != nil {
		t.Fatal(err)
	}
	// Clear dirty flag so dry-run reaches live probe + SetPotSharesFrom cleanly.
	store, err := OpenStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.RW.ExecContext(ctx, `UPDATE hub_pot_sync SET dirty=0`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (state, errMsg, cursor string, gen, dists int) {
		if err := store.RO.QueryRow(`SELECT state, error, generation,
			coalesce((SELECT value FROM cursors WHERE name='hub:pot_distributions'),''),
			(SELECT count(*) FROM hub_pot_distributions)
			FROM hub_pot_sync WHERE singleton=1`).Scan(&state, &errMsg, &gen, &cursor, &dists); err != nil {
			t.Fatal(err)
		}
		return
	}
	st0, err0, cur0, gen0, d0 := snapshot()

	// 1. Outage during --off --dry-run must fail WITHOUT writing error state or incrementing generation.
	mode = "fail"
	if err := call("payroll pots-hub-from", "--off", "--dry-run"); err == nil {
		t.Fatal("--off --dry-run succeeded during outage")
	}
	if st1, err1, cur1, gen1, d1 := snapshot(); st1 != st0 || err1 != err0 || cur1 != cur0 || gen1 != gen0 || d1 != d0 {
		t.Fatalf("outage dry-run mutated DB: got (%q,%q,%q,%d,%d), want (%q,%q,%q,%d,%d)", st1, err1, cur1, gen1, d1, st0, err0, cur0, gen0, d0)
	}

	// 2. Live posted distribution during --off --dry-run must fail WITHOUT persisting cursor or distributions.
	mode = "posted"
	if err := call("payroll pots-hub-from", "--off", "--dry-run"); err == nil {
		t.Fatal("--off --dry-run succeeded with live posted distribution")
	}
	if st2, err2, cur2, gen2, d2 := snapshot(); st2 != st0 || err2 != err0 || cur2 != cur0 || gen2 != gen0 || d2 != d0 {
		t.Fatalf("posted dry-run mutated DB: got (%q,%q,%q,%d,%d), want (%q,%q,%q,%d,%d)", st2, err2, cur2, gen2, d2, st0, err0, cur0, gen0, d0)
	}
}
