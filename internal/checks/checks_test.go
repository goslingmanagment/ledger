package checks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/free"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/journal"
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
	cfg := month.Config{Models: []string{"Alfa", "Gamma"}, Pages: []string{"alfa-1", "gamma-1"}, Share: 0.65, KLow: 0.985, KHigh: 1.015,
		Routes: map[string]month.Route{"alfa-1": route("alfa-1", "Alfa", "model", 1), "gamma-1": route("gamma-1", "Gamma", "agency", 1)}}
	t := &month.Transfer{N: 10, Model: "Alfa", Date: "2026-10-20", Amount: 650, Deps: []float64{1000}, HasK: true, K: 1.0, ByMonth: map[string]float64{}}
	q := &month.Request{Page: "alfa-1", Key: "q1", T: at("2026-10-19T12:00:00+03:00"), Amount: 1000, Avail: 1000, Transfer: t}
	return &month.Cover{Cfg: cfg, AsOf: now, Reqs: []*month.Request{q}, Transfers: map[string][]*month.Transfer{"Alfa": {t}},
		Src: src{incomes: map[string][]float64{"2026-10-20": {650}}}}
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
	agency := cover()
	agency.Reqs[0].Page = "gamma-1"
	tr := agency.Transfers["Alfa"][0]
	tr.Model, tr.Deps, tr.Amount, tr.Fee, tr.N = "Gamma", []float64{1100}, 1100, 100, 11
	agency.Transfers = map[string][]*month.Transfer{"Gamma": {tr}}
	agency.Src = src{incomes: map[string][]float64{"2026-10-20": {1000}}}
	if r := Deposits(agency); !r.OK {
		t.Errorf("the route fee is inside the deposits of an agency route: %+v", r)
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
	// The bot has said nothing for two days, but the collector still sends the last day as repeats.
	exec(t, s, `INSERT INTO events(source, source_id, kind, received_at, payload) VALUES ('cincin_bot', '1', 'message', ?, '{}')`,
		ts.Format(now.Add(-48*time.Hour)))
	if r := Sources(ctx, e); r.OK || !strings.Contains(r.Summary, "cincin_bot молчит") {
		t.Fatalf("collector silent for two days: %+v", r)
	}
	exec(t, s, `INSERT INTO cursors(name, value, updated_at) VALUES ('contact:cincin_bot', ?, 'x')`, ts.Format(now.Add(-time.Hour)))
	if r := Sources(ctx, e); !r.OK {
		t.Fatalf("quiet bot, live collector: %+v", r)
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
	if r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "ещё не собраны") {
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

func TestStaleRunIsNotGreen(t *testing.T) {
	green := []Result{{Name: "а", OK: true}}
	if got := WithStale(green, ts.Format(now.Add(-time.Hour)), now); len(got) != 1 {
		t.Fatalf("a recent run: %+v", got)
	}
	got := WithStale(green, ts.Format(now.Add(-3*time.Hour)), now)
	if len(got) != 2 || got[1].OK {
		t.Fatalf("an old run: %+v", got)
	}
	if got := WithStale(nil, "", now); len(got) != 1 || got[0].OK {
		t.Fatalf("no run: %+v", got)
	}
}

// One run with a cash difference wakes nobody: the balance and the movements are read at different moments.
func TestCashDifferenceMustHold(t *testing.T) {
	s, ctx := openDB(t)
	bad := []Result{{Name: cashCheck, Severity: "urgent", Summary: "Δ"}}
	if err := Save(ctx, s.RW, []Result{{Name: cashCheck, OK: true, Severity: "urgent", Summary: "ok"}}, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, s.RW, bad, now, now); err != nil {
		t.Fatal(err)
	}
	if out, err := Urgent(ctx, s.RW, bad, now, 6*time.Hour); err != nil || len(out) != 0 {
		t.Fatalf("first failure: %+v %v", out, err)
	}
	if err := Save(ctx, s.RW, bad, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if out, err := Urgent(ctx, s.RW, bad, now.Add(time.Hour), 6*time.Hour); err != nil || len(out) != 1 {
		t.Fatalf("second failure: %+v %v", out, err)
	}
}

func TestWalletAgainstTheBot(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	post := func(day, v string) {
		t.Helper()
		if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: day, Kind: "test", Author: "t", Lines: []ledger.Line{
			{Account: "cincin_wallet", Amount: money.MustParse(v)}, {Account: "opening", Amount: -money.MustParse(v)}}}, now); err != nil {
			t.Fatal(err)
		}
	}
	anchor := func(kind, subject, v, at string) {
		t.Helper()
		if _, err := s.RW.Exec(`INSERT INTO observations(kind, subject, value, observed_at, source) VALUES (?, ?, ?, ?, 'test')`, kind, subject, int64(money.MustParse(v)), at); err != nil {
			t.Fatal(err)
		}
	}
	post("2026-10-01", "100.00")
	anchor("wallet_anchor", "cincin_wallet", "100.00", "2026-10-01T12:00:00+03:00")
	anchor("card_anchor", "card:1111", "0.00", "2026-10-01T12:00:00+03:00")
	if r := CardAnchors(ctx, e); !r.OK {
		t.Fatalf("equal: %+v", r)
	}
	post("2026-10-10", "50.00") // after the anchor: the bot has not been asked since
	if r := CardAnchors(ctx, e); !r.OK {
		t.Fatalf("a later entry is not a difference: %+v", r)
	}
	anchor("wallet_anchor", "cincin_wallet", "20.00", "2026-10-11T12:00:00+03:00")
	if r := CardAnchors(ctx, e); r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "кошелёк") {
		t.Fatalf("a drifting wallet: %+v", r)
	}
}

// The bot printed the wallet at a moment; a journal row of that day whose money came later is not in it.
func TestWalletAtTheAnchorMoment(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	event := func(id int64, at string) {
		t.Helper()
		exec(t, s, `INSERT INTO events(id, source, source_id, kind, occurred_at, received_at, payload) VALUES (?, 'test', ?, 'x', ?, ?, '{}')`, id, id, at, at)
	}
	post := func(v string, doc int64, events ...int64) {
		t.Helper()
		if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-10-20", Kind: "test", Author: "t", DocumentID: doc, EventIDs: events,
			Lines: []ledger.Line{{Account: "cincin_wallet", Amount: money.MustParse(v)}, {Account: "opening", Amount: -money.MustParse(v)}}}, now); err != nil {
			t.Fatal(err)
		}
	}
	fee := func(fee, at string) {
		t.Helper()
		exec(t, s, `INSERT INTO proposals(key, kind, summary, content, created_at, author) VALUES (?, 'card_load', 's', ?, 'x', 't')`,
			"card:"+at, `{"loaded_card":"1111","fee":"`+fee+`","at":"`+at+`"}`)
	}
	event(1, "2026-10-19T23:50:00+03:00") // the day before: an entry of 20.10 is not moved to it
	event(2, "2026-10-20T22:00:00+03:00")
	event(3, "2026-10-20T23:40:00+03:00")
	event(4, "2026-10-20T23:57:00+03:00")
	post("10.00", 0)    // no event: counts on its day
	post("5.00", 0, 1)  // its only event is another day's: counts on its day
	post("20.00", 0, 2) // in before the anchor
	post("300.00", 0, 3)
	exec(t, s, `INSERT INTO documents(id, num, date, type, category, counterparty, model, amount, amount_micros, currency, method, status, note, recorded_at, author, origin)
		VALUES (9, 9, '2026-10-20', 'transfer', 'c', 'c', '—', '400.00', 400000000, 'USD', 'm', 'paid', '', 'x', 't', 't')`)
	exec(t, s, `INSERT INTO event_links(event_id, document_num, how, decision, created_at) VALUES (2, 9, 'rule', 'recorded', 'x'), (4, 9, 'rule', 'recorded', 'x')`)
	post("400.00", 9) // a top-up: the withdrawal before the anchor, the deposit after it
	fee("1.00", "2026-10-20T22:00:00+03:00")
	fee("13.00", "2026-10-20T23:40:00+03:00")
	exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source) VALUES ('wallet_anchor', 'cincin_wallet', ?, '2026-10-20T23:20:00+03:00', 'test'),
		('card_anchor', 'card:1111', 0, '2026-10-20T23:20:00+03:00', 'test')`, int64(money.MustParse("34.00")))
	if r := CardAnchors(ctx, e); !r.OK {
		t.Fatalf("the books at the anchor's moment: %+v", r)
	}
	post("7.00", 0, 2)
	if r := CardAnchors(ctx, e); r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "на момент анкора 42.00") {
		t.Fatalf("a difference before the anchor: %+v", r)
	}
}

// A coin on the exchange that no account holds is money the books do not see: a warning, never silence.
func TestCoinsWithoutAccount(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	if r := Coins(ctx, e); r.OK {
		t.Fatalf("no reading yet: %+v", r)
	}
	reading := func(at, payload string) {
		t.Helper()
		exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source, payload) VALUES ('bybit_balance', 'bybit', 0, ?, 't', ?)`, at, payload)
	}
	exec(t, s, `INSERT INTO accounts(code, name, kind, grp, opened_at) VALUES ('bybit:xyz', 'XYZ', 'asset', 'money', '2026-01-01')`)
	reading("2026-11-04T10:00:00+03:00", `{"wallets":{"FUND":{"USDT":"10","USDC":"1","XYZ":"0.5"}}}`)
	if r := Coins(ctx, e); !r.OK {
		t.Fatalf("stable coins and a coin with its account: %+v", r)
	}
	reading("2026-11-04T11:00:00+03:00", `{"wallets":{"FUND":{"USDT":"10","XYZ":"0.5","ABC":"0.0123"},"UNIFIED":{"DEF":"7"}}}`)
	r := Coins(ctx, e)
	if r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "монета без счёта") ||
		!strings.Contains(r.Summary, "ABC 0.0123 (FUND); DEF 7 (UNIFIED)") || strings.Contains(r.Summary, "XYZ") {
		t.Fatalf("coins without an account: %+v", r)
	}
}

// The coin account against the coin left at its purchase price: a payment written at a lower market price
// leaves the account above it, and a quantity the exchange does not hold is no cost at all.
func TestCoinsAtCost(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	add := func(ev ledger.Event) int64 {
		t.Helper()
		id, _, err := ledger.AddEvent(ctx, s.RW, ev, now)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add(bybit.FundingRow{ID: "u", Coin: "USDT", Signed: "-100", After: "900", Type: "Convert", At: "2026-11-01T10:00:00+03:00"}.Event())
	add(bybit.FundingRow{ID: "b", Coin: "BTC", Signed: "0.002", After: "0.002", Type: "Convert", At: "2026-11-01T10:00:00+03:00"}.Event())
	if _, err := bybit.PostConverts(ctx, s.RW, now); err != nil {
		t.Fatal(err)
	}
	if r := CoinsAtCost(ctx, e); !r.OK {
		t.Fatalf("bought, nothing spent: %+v", r)
	}
	add(bybit.FundingRow{ID: "w", Coin: "BTC", Signed: "-0.001", After: "0.001", Type: "Withdraw", At: "2026-11-02T12:00:00+03:00"}.Event())
	w := add(bybit.Movement{Key: "withdrawal:1", Kind: "withdrawal", TimeMs: at("2026-11-02T12:00:00+03:00").UnixMilli(), Coin: "BTC", Amount: "0.001"}.Event())
	d, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 1, Date: "2026-11-02", Type: "expense", Category: "c", Counterparty: "x", Model: "—",
		Amount: "45.00", Currency: "USD", Method: "BTC (Bybit)", Status: "paid", Note: "withdrawal:1", Author: "t", Origin: "t"}, now)
	if err != nil {
		t.Fatal(err)
	}
	v := money.MustParse("45.00")
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-11-02", Kind: "document", Author: "t", DocumentID: d.ID,
		Lines: []ledger.Line{{Account: "bybit:btc", Amount: -v}, {Account: "expense:other", Amount: v}}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := bybit.AddLink(ctx, s.RW, bybit.Link{EventID: w, DocumentNum: 1, How: "note_key", Decision: "recorded"}, now); err != nil {
		t.Fatal(err)
	}
	reading := func(at, btc string) {
		t.Helper()
		exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source, payload) VALUES ('bybit_balance', 'bybit', 0, ?, 't', ?)`,
			at, `{"wallets":{"FUND":{"USDT":"900","BTC":"`+btc+`"}},"all_coins":true}`)
	}
	reading("2026-11-03T10:00:00+03:00", "0.001")
	if r := CoinsAtCost(ctx, e); r.OK || !strings.Contains(r.Summary, "по книгам 55.00, по средней цене покупки 50.00") || strings.Contains(r.Summary, "остаток") {
		t.Fatalf("a payment below the purchase price: %+v", r)
	}
	reading("2026-11-03T11:00:00+03:00", "0.0015")
	if r := CoinsAtCost(ctx, e); r.OK || !strings.Contains(r.Summary, "остаток по Funding 0.001 BTC, на бирже 0.0015") {
		t.Fatalf("the exchange holds another quantity: %+v", r)
	}
}

// A coin that never moved through Funding stands at its starting balance: it is checked by its quantity
// against the reading the balance was posted after, in FUND or UNIFIED, ±$1 at the starting price.
func TestCoinAtItsStartingBalance(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	reading := func(at, xyz, abc string) {
		t.Helper()
		exec(t, s, `INSERT INTO observations(kind, subject, value, observed_at, source, payload) VALUES ('bybit_balance', 'bybit', 0, ?, 't', ?)`,
			at, `{"wallets":{"FUND":{"USDT":"900","XYZ":"`+xyz+`"},"UNIFIED":{"ABC":"`+abc+`"}},"all_coins":true}`)
	}
	for i, r := range []journal.Row{
		{Num: 1, Date: "2026-11-04", Type: "transfer", Category: "Остаток на старте", Counterparty: "XYZ (Bybit)", Model: "—",
			Amount: "5.00", Currency: "USDT", Method: "XYZ (Bybit)", Status: "paid"},
		{Num: 2, Date: "2026-11-04", Type: "transfer", Category: "Остаток на старте", Counterparty: "ABC (Bybit)", Model: "—",
			Amount: "0.02", Currency: "USDT", Method: "ABC (Bybit)", Status: "paid"},
	} {
		es, err := journal.Entries(r, journal.Tags{}, &journal.Context{})
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range es {
			en.Author = "t"
			if _, err := ledger.Post(ctx, s.RW, en, now.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	reading("2026-11-04T13:00:00+03:00", "0.05", "0.00001")
	if r := Coins(ctx, e); !r.OK {
		t.Fatalf("both coins have their accounts: %+v", r)
	}
	if r := CoinsAtCost(ctx, e); r.OK || !strings.Contains(r.Summary, "bybit:xyz: остаток на старте проведён") || !strings.Contains(r.Summary, "показания биржи до него нет") {
		t.Fatalf("no reading before the starting balance: %+v", r)
	}
	reading("2026-11-04T11:00:00+03:00", "0.05", "0.00001") // bybit:btc of the chart is the third
	if r := CoinsAtCost(ctx, e); !r.OK || r.Summary != "3 счетов монет по цене покупки ±$1" {
		t.Fatalf("the quantity read before the starting balance is still there: %+v", r)
	}
	reading("2026-11-04T14:00:00+03:00", "0.041", "0")
	if r := CoinsAtCost(ctx, e); !r.OK {
		t.Fatalf("0.009 XYZ at 100 and the whole 0.02 of ABC are under a dollar: %+v", r)
	}
	reading("2026-11-04T15:00:00+03:00", "0.03", "0")
	r := CoinsAtCost(ctx, e)
	if r.OK || !strings.Contains(r.Summary, "bybit:xyz: на старте 0.05 XYZ (FUND, показание 2026-11-04T11:00:00+03:00), на бирже 0.03") || strings.Contains(r.Summary, "bybit:abc") {
		t.Fatalf("0.02 XYZ at 100 left without a Funding row: %+v", r)
	}
}

func TestRatesAgainstCards(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	if _, err := s.RW.Exec(`INSERT INTO people(slug, name, rate, source, imported_at) VALUES
		('petr', 'Пётр', '25% от выручки', 't', 'x'), ('vasya', 'Вася', 'фикс за смену', 't', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.Exec(`INSERT INTO events(id, source, source_id, kind, occurred_at, received_at, payload) VALUES (1, 'sheets', 't1', 'tab', 'x', 'x', '{}')`); err != nil {
		t.Fatal(err)
	}
	add := func(model, person string, rate float64) {
		t.Helper()
		if _, err := s.RW.Exec(`INSERT INTO payroll_tabs(model, name, person, status, fetched_at, rate, event_id, listed) VALUES (?, ?, ?, 'ok', 'x', ?, 1, 1)`,
			model, person, person, rate); err != nil {
			t.Fatal(err)
		}
	}
	add("Alfa", "petr", 0.25)
	add("Alfa", "vasya", 0.3)
	if r := Rates(ctx, e); !r.OK {
		t.Fatalf("equal: %+v", r)
	}
	add("Beta", "petr", 0.2)
	if r := Rates(ctx, e); r.OK || !strings.Contains(r.Summary, "petr (Beta)") {
		t.Fatalf("different: %+v", r)
	}
}

// now is 2026-11-04: the check looks at 05.11–07.11.
func TestCardCover(t *testing.T) {
	s, ctx := openDB(t)
	e := Env{Q: s.RO, Now: now}
	if r := CardCover(ctx, e); !r.OK {
		t.Fatalf("no commitments: %+v", r)
	}
	post(t, s, ctx, "card:1111", "50.00")
	post(t, s, ctx, "card:2222", "500.00")
	post(t, s, ctx, "cincin_wallet", "80.00")
	tx, _ := s.RW.BeginTx(ctx, nil)
	if _, err := free.ImportCommitments(ctx, tx, []free.CommitmentSeed{
		{Kind: "subscription", Name: "Сервис А", Amount: "40.00", Period: "month", NextDue: "2026-11-05", Account: "card:1111"},
		{Kind: "subscription", Name: "Сервис Б", Amount: "30.00", Period: "month", NextDue: "2026-11-07", Account: "card:1111"},
		{Kind: "subscription", Name: "Сегодня", Amount: "900.00", Period: "month", NextDue: "2026-11-04", Account: "card:1111"},
		{Kind: "subscription", Name: "Позже", Amount: "900.00", Period: "month", NextDue: "2026-11-08", Account: "card:1111"},
		{Kind: "subscription", Name: "Хватает", Amount: "100.00", Period: "month", NextDue: "2026-11-06", Account: "card:2222"},
		{Kind: "subscription", Name: "Без счёта", Amount: "900.00", Period: "month", NextDue: "2026-11-06"},
	}, "t", now); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	r := CardCover(ctx, e)
	if r.OK || r.Severity != "warn" {
		t.Fatalf("a short card passed: %+v", r)
	}
	for _, want := range []string{"карта •1111: на счёте 50.00, к списанию 70.00 до 07.11 (Сервис А, Сервис Б), не хватает 20.00", "в кошельке CinCin 80.00"} {
		if !strings.Contains(r.Summary, want) {
			t.Fatalf("summary %q lacks %q", r.Summary, want)
		}
	}
	if strings.Contains(r.Summary, "2222") {
		t.Fatalf("a covered card is named: %q", r.Summary)
	}
	post(t, s, ctx, "card:1111", "20.00")
	if r := CardCover(ctx, e); !r.OK {
		t.Fatalf("a loaded card fails: %+v", r)
	}
}
