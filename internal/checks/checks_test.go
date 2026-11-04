package checks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/month"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func openDB(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func at(s string) time.Time {
	v, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

var now = at("2026-11-04T12:00:00+03:00")

func post(t *testing.T, s *db.Store, ctx context.Context, acct, v string) {
	t.Helper()
	m := money.MustParse(v)
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-10-01", Kind: "test", Author: "test",
		Lines: []ledger.Line{{Account: acct, Amount: m}, {Account: "opening", Amount: -m}}}, now); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, s *db.Store, q string, args ...any) {
	t.Helper()
	if _, err := s.RW.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestBalanced(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	if r := Balanced(ctx, e); !r.OK {
		t.Fatal(r)
	}
	exec(t, s, `INSERT INTO entries(id, effective_at, recorded_at, kind, author) VALUES (77, '2026-10-01', 'x', 'bad', 'test')`)
	exec(t, s, `INSERT INTO lines(entry_id, account, amount) VALUES (77, 'bybit', 5)`)
	if r := Balanced(ctx, e); r.OK || r.Severity != "urgent" {
		t.Fatalf("broken entry passed: %+v", r)
	}
}

func TestBybitBalance(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	if r := BybitBalance(ctx, e); r.OK {
		t.Fatal("no observation must fail")
	}
	post(t, s, ctx, "bybit", "1000.00")
	exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source) VALUES ('bybit_balance', 'bybit', ?, ?, 't')`,
		int64(money.MustParse("1000.50")), ts.Format(now.Add(-time.Hour)))
	if r := BybitBalance(ctx, e); !r.OK {
		t.Fatalf("within a dollar: %+v", r)
	}
	exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source) VALUES ('bybit_balance', 'bybit', ?, ?, 't')`,
		int64(money.MustParse("1002.00")), ts.Format(now.Add(-time.Minute)))
	if r := BybitBalance(ctx, e); r.OK || r.Severity != "urgent" {
		t.Fatalf("two dollars off passed: %+v", r)
	}
	if r := BybitBalance(ctx, Env{Q: s.RO, Now: now.Add(4 * time.Hour)}); r.OK || r.Severity != "warn" {
		t.Fatalf("stale reading: %+v", r)
	}
}

func TestCardAnchors(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	post(t, s, ctx, "card:1111", "100.00")
	post(t, s, ctx, "card:2222", "5.00")
	exec(t, s, `INSERT INTO settings(key, value, updated_at) VALUES ('frozen_accounts', '["card:2222"]', 'x')`)
	exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source) VALUES ('card_anchor', 'card:2222', 999000000, 'x', 't')`)
	exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source) VALUES ('card_anchor', 'card:1111', ?, '2026-11-04T10:00:00+03:00', 't')`,
		int64(money.MustParse("80.00")))
	if r := CardAnchors(ctx, e); r.OK || !strings.Contains(r.Summary, "card:1111") || strings.Contains(r.Summary, "card:2222") {
		t.Fatalf("a $20 hole on the live card: %+v", r)
	}
	exec(t, s, `INSERT INTO proposals(key, kind, summary, content, created_at, author) VALUES ('card:1', 'purchase', 'покупка', ?, 'x', 't')`,
		`{"card":"1111","amount":"20.00"}`)
	if r := CardAnchors(ctx, e); !r.OK {
		t.Fatalf("a purchase waiting for a decision explains it: %+v", r)
	}
}

func TestUnlinked(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	exec(t, s, `INSERT INTO settings(key, value, updated_at) VALUES ('full_ledger_from', '2026-08-01', 'x')`)
	id, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "bybit", SourceID: "withdrawal:1", Kind: "withdrawal", OccurredAt: "2026-10-20T10:00:00+03:00",
		Payload: map[string]any{"amount": "10", "coin": "USDT", "direction": "expense"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if r := Unlinked(ctx, e); r.OK {
		t.Fatal("old movement without a row passed")
	}
	exec(t, s, `INSERT INTO event_links(event_id, how, decision, created_at) VALUES (?, 'manual', 'personal', 'x')`, id)
	if r := Unlinked(ctx, e); !r.OK {
		t.Fatalf("decided personal: %+v", r)
	}
	if _, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "bybit", SourceID: "withdrawal:2", Kind: "withdrawal", OccurredAt: ts.Format(now.Add(-time.Hour)),
		Payload: map[string]any{"amount": "10", "coin": "USDT", "direction": "expense"}}, now); err != nil {
		t.Fatal(err)
	}
	if r := Unlinked(ctx, e); !r.OK {
		t.Fatalf("a fresh movement has 3 days: %+v", r)
	}
}

type src struct{ incomes map[string][]float64 }

func (s src) Rows(string, time.Time, time.Time) (hub.Checked, error)       { return hub.Checked{}, nil }
func (s src) Payouts(string, time.Time, time.Time) ([]hub.Payout, error)   { return nil, nil }
func (s src) OFPayouts(string, time.Time, time.Time) ([]hub.Payout, error) { return nil, nil }
func (s src) Pages() ([]hub.Page, error)                                   { return nil, nil }
func (s src) SummaryNet(string, time.Time, time.Time) (float64, error)     { return 0, nil }
func (s src) BybitIncomes(d string) ([]float64, bool)                      { return s.incomes[d], true }

func cover() *month.Cover {
	old := at("0001-01-01T00:00:00+03:00")
	route := func(page, model, payee string, rate float64) month.Route {
		return month.Route{Page: page, Model: model, Channel: "Fansly", Rate: rate, Payee: payee, Rates: []month.MethodRate{{Method: "*", Rate: rate, From: old}}}
	}
	cfg := month.Config{Models: []string{"Alfa", "Gamma"}, Pages: []string{"alfa-1", "gamma-1"}, Share: 0.7, KLow: 0.985, KHigh: 1.015,
		Routes: map[string]month.Route{"alfa-1": route("alfa-1", "Alfa", "model", 1), "gamma-1": route("gamma-1", "Gamma", "agency", 1)}}
	t := &month.Transfer{N: 10, Model: "Alfa", Date: "2026-10-20", Amount: 700, Deps: []float64{1000}, HasK: true, K: 1.0, ByMonth: map[string]float64{}}
	q := &month.Request{Page: "alfa-1", Key: "q1", T: at("2026-10-19T12:00:00+03:00"), Amount: 1000, Avail: 1000, Transfer: t}
	return &month.Cover{Cfg: cfg, AsOf: now, Reqs: []*month.Request{q}, Transfers: map[string][]*month.Transfer{"Alfa": {t}},
		Src: src{incomes: map[string][]float64{"2026-10-20": {700}}}}
}

func TestCoverageChecks(t *testing.T) {
	cv := cover()
	for name, r := range map[string]Result{"requests": Requests(cv), "k": KNorm(cv), "overdue": Overdue(cv, now), "deposits": Deposits(cv)} {
		if !r.OK {
			t.Errorf("clean %s failed: %+v", name, r)
		}
	}
	bad := cover()
	bad.Reqs[0].Excess = 5
	if r := Requests(bad); r.OK {
		t.Error("request above the net passed")
	}
	bad = cover()
	bad.Transfers["Alfa"][0].K = 0.9
	if r := KNorm(bad); r.OK || r.Severity != "urgent" {
		t.Errorf("k out of norm: %+v", r)
	}
	bad = cover()
	bad.Reqs[0].Transfer = nil
	if r := Overdue(bad, at("2026-10-25T12:00:00+03:00")); !r.OK {
		t.Errorf("6 days is fine: %+v", r)
	}
	if r := Overdue(bad, at("2026-11-01T12:00:00+03:00")); r.OK || r.Severity != "urgent" {
		t.Errorf("13 days without a transfer: %+v", r)
	}
	bad.Reqs[0].Page = "gamma-1"
	if r := Overdue(bad, at("2026-11-01T12:00:00+03:00")); !r.OK {
		t.Errorf("an exchanger pays on its own schedule: %+v", r)
	}
	bad = cover()
	bad.Transfers["Alfa"][0].Deps = []float64{900}
	if r := Deposits(bad); r.OK || !strings.Contains(r.Summary, "по заявкам") {
		t.Errorf("deposits below the requests: %+v", r)
	}
	bad = cover()
	bad.Src = src{incomes: map[string][]float64{}}
	if r := Deposits(bad); r.OK || !strings.Contains(r.Summary, "зачисления") {
		t.Errorf("nothing credited on Bybit: %+v", r)
	}
}

func TestCopies(t *testing.T) {
	s, ctx := openDB(t)
	dir := t.TempDir()
	e := Env{Q: s.RO, Now: now, DataDir: dir}
	if r := Copies(ctx, e); r.OK {
		t.Fatal("no replica and no Mac copy passed")
	}
	if err := os.MkdirAll(filepath.Join(dir, "replica"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "replica", "ltx")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, now, now); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `INSERT INTO cursors(name, value, updated_at) VALUES ('backup:mac', ?, 'x')`, ts.Format(now.Add(-2*time.Hour)))
	if r := Copies(ctx, e); !r.OK {
		t.Fatalf("fresh copies: %+v", r)
	}
	if r := Copies(ctx, Env{Q: s.RO, Now: now.Add(48 * time.Hour), DataDir: dir}); r.OK {
		t.Fatal("two-day-old copies passed")
	}
}

func TestSources(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	if r := Sources(ctx, e); r.OK {
		t.Fatal("nothing collected passed")
	}
	raw, _ := json.Marshal(map[string]any{"at": ts.Format(now.Add(-10 * time.Minute)), "ok": true})
	exec(t, s, `INSERT INTO cursors(name, value, updated_at) VALUES ('bybit:status', ?, 'x')`, string(raw))
	exec(t, s, `INSERT INTO hub_sync(page_label, dataset, status, reasons, blockers, window_from, window_to, rows, net_mills, checked_at)
		VALUES ('p', 'transactions', 'unknown', '["blocker"]', '[]', 'x', 'x', 0, 0, ?)`, ts.Format(now))
	if r := Sources(ctx, e); r.OK || !strings.Contains(r.Summary, "неизвестно") {
		t.Fatalf("hub unknown: %+v", r)
	}
	exec(t, s, `DELETE FROM hub_sync`)
	exec(t, s, `INSERT INTO hub_sync(page_label, dataset, status, reasons, blockers, window_from, window_to, rows, net_mills, checked_at)
		VALUES ('p', 'transactions', 'ok', '[]', '[]', 'x', 'x', 0, 0, ?)`, ts.Format(now))
	if r := Sources(ctx, e); !r.OK {
		t.Fatalf("all alive: %+v", r)
	}
	raw, _ = json.Marshal(map[string]any{"at": ts.Format(now), "ok": false, "errors": map[string]string{"deposit": "retCode 10003"}})
	exec(t, s, `UPDATE cursors SET value = ? WHERE name = 'bybit:status'`, string(raw))
	if r := Sources(ctx, e); r.OK || !strings.Contains(r.Summary, "retCode") {
		t.Fatalf("Bybit error: %+v", r)
	}
}

func TestPayablesWithoutSheets(t *testing.T) {
	s, ctx := openDB(t)
	r := Payables(ctx, Env{Q: s.RO, Now: now})
	if r.OK || r.Severity != "warn" {
		t.Fatalf("no sheets yet: %+v", r)
	}
}

func TestUrgentOnceThenRepeatThenRecover(t *testing.T) {
	s, ctx := openDB(t)
	rs := []Result{{Name: "касса", Severity: "urgent", Summary: "не сходится"}, {Name: "мелочь", Severity: "warn", Summary: "x"}, {Name: "ок", OK: true, Severity: "info"}}
	first, err := Urgent(ctx, s.RW, rs, now, 6*time.Hour)
	if err != nil || len(first) != 1 || first[0].Name != "касса" {
		t.Fatalf("first %v %v", first, err)
	}
	second, _ := Urgent(ctx, s.RW, rs, now.Add(time.Hour), 6*time.Hour)
	if len(second) != 0 {
		t.Fatalf("repeated within the hour: %v", second)
	}
	third, _ := Urgent(ctx, s.RW, rs, now.Add(7*time.Hour), 6*time.Hour)
	if len(third) != 1 {
		t.Fatalf("not repeated after 7 hours: %v", third)
	}
	if _, err := Urgent(ctx, s.RW, nil, now.Add(8*time.Hour), 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	s.RO.QueryRow(`SELECT count(*) FROM alerts`).Scan(&n)
	if n != 0 {
		t.Fatalf("recovered failure stays in alerts: %d", n)
	}
	again, _ := Urgent(ctx, s.RW, rs, now.Add(9*time.Hour), 6*time.Hour)
	if len(again) != 1 {
		t.Fatalf("a new failure after recovery is new: %v", again)
	}
}

func TestSaveAndLatest(t *testing.T) {
	s, ctx := openDB(t)
	rs := []Result{{Name: "а", OK: true, Severity: "info", Summary: "старое"}}
	if err := Save(ctx, s.RW, rs, now, now); err != nil {
		t.Fatal(err)
	}
	rs = []Result{{Name: "а", Severity: "urgent", Summary: "новое", Details: map[string]int{"x": 1}}}
	if err := Save(ctx, s.RW, rs, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, at, err := Latest(ctx, s.RO)
	if err != nil || len(got) != 1 || got[0].OK || got[0].Summary != "новое" || at == "" {
		t.Fatalf("%+v %s %v", got, at, err)
	}
}
