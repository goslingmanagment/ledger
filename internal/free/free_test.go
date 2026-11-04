package free

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
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

func post(t *testing.T, s *db.Store, ctx context.Context, date string, lines ...ledger.Line) {
	t.Helper()
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: date, Kind: "test", Author: "test", Lines: lines}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func line(acct, v string) ledger.Line { return ledger.Line{Account: acct, Amount: money.MustParse(v)} }

func set(t *testing.T, s *db.Store, k, v string) {
	t.Helper()
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES (?, ?, 'x') ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
		t.Fatal(err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.006 }

// Wednesday 2026-11-04: the next withdrawal is Monday 09.11, the money arrives on 10.11.
func TestFreeBreakdown(t *testing.T) {
	s, ctx := openDB(t)
	now, _ := ts.Parse("2026-11-04T12:00:00+03:00")
	post(t, s, ctx, "2026-10-01", line("bybit", "10000.00"), line("opening", "-10000.00"))
	post(t, s, ctx, "2026-10-01", line("cincin_wallet", "100.00"), line("opening", "-100.00"))
	post(t, s, ctx, "2026-10-01", line("card:1111", "50.00"), line("opening", "-50.00"))
	post(t, s, ctx, "2026-10-01", line("card:2222", "70.00"), line("opening", "-70.00")) // frozen
	post(t, s, ctx, "2026-10-02", line("expense:chatter_salary", "1200.00"), line("payable:petr", "-1200.00"))
	post(t, s, ctx, "2026-10-02", line("expense:chatter_salary", "300.00"), line("payable:vasya", "-300.00"))
	post(t, s, ctx, "2026-10-02", line("prepaid:svc", "999.00"), line("opening", "-999.00")) // not liquid
	set(t, s, "frozen_accounts", `["card:2222"]`)
	set(t, s, "owner_salary_rate", "3500")
	tx, _ := s.RW.BeginTx(ctx, nil)
	if _, err := ImportCommitments(ctx, tx, []CommitmentSeed{
		{Kind: "subscription", Name: "Сервис А", Amount: "200.00", Period: "month", NextDue: "2026-11-07"},
		{Kind: "subscription", Name: "Сервис Б", Amount: "60.00", Period: "month", NextDue: "2026-11-20"},
		{Kind: "contractor", Name: "Подрядчик", Amount: "40.00", Period: "week", NextDue: "2026-11-06"},
		{Kind: "reinvest", Name: "Статья роста", Amount: "2500.00", Period: "once", NextDue: "2026-11-25", Until: "2026-11-30"},
		{Kind: "subscription", Name: "Закончилась", Amount: "999.00", Period: "month", NextDue: "2026-01-01", Until: "2026-10-01"},
	}, "t", now); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	// A salary row of the month with a part moved in advance from October counts for November.
	for _, d := range []ledger.Document{
		{Num: 1, Date: "2026-11-02", Type: "expense", Category: "Owner зарплата", Counterparty: "В", Model: "—", Amount: "500.00", Currency: "USDT", Method: "P2P (Bybit)", Status: "paid", Note: "транш", Author: "t", Origin: "t"},
		{Num: 2, Date: "2026-10-31", Type: "expense", Category: "Owner зарплата", Counterparty: "В", Model: "—", Amount: "300.00", Currency: "USDT", Method: "P2P (Bybit)", Status: "paid", Note: "аванс {за 2026-11: 300}", Author: "t", Origin: "t"},
		{Num: 3, Date: "2026-10-20", Type: "expense", Category: "Чаттер зарплата", Counterparty: "Пётр", Model: "Alfa", Amount: "800.00", Currency: "USDT", Method: "Internal Transfer (Bybit)", Status: "paid", Note: "выплата", Author: "t", Origin: "t"},
		{Num: 4, Date: "2026-09-01", Type: "expense", Category: "Чаттер зарплата", Counterparty: "Пётр", Model: "Alfa", Amount: "9999.00", Currency: "USDT", Method: "Internal Transfer (Bybit)", Status: "paid", Note: "старая", Author: "t", Origin: "t"},
	} {
		if _, _, err := ledger.PutDocument(ctx, s.RW, d, now); err != nil {
			t.Fatal(err)
		}
	}
	// No coverage yet: money in transit is unknown and stays out.
	a, err := Compute(ctx, s.RO, now)
	if err != nil {
		t.Fatal(err)
	}
	if a.NextIncome != "2026-11-10" {
		t.Fatalf("next income %s", a.NextIncome)
	}
	if !near(a.Liquid, 10150) || !near(a.Debts, 1500) {
		t.Fatalf("liquid %v debts %v", a.Liquid, a.Debts)
	}
	// Before 10.11: Сервис А 200 + two weeks of the contractor 80 (06.11 and... 13.11 is after).
	if !near(a.CommitmentsSoon, 240) {
		t.Fatalf("soon %v %+v", a.CommitmentsSoon, a.SoonItems)
	}
	// Month of fixed costs: 200 + 60 + 40 × 52/12 = 433.33; a week of payouts: 800 / 4 = 200.
	if !near(a.Cushion, 200+60+40*52.0/12+200) {
		t.Fatalf("cushion %v (%s)", a.Cushion, a.CushionNote)
	}
	if !near(a.FreeNow, 10150-1500-240-a.Cushion) {
		t.Fatalf("free now %v", a.FreeNow)
	}
	if a.TransitStatus != "unknown" || a.Expected != 0 {
		t.Fatalf("transit %s expected %v", a.TransitStatus, a.Expected)
	}
	// Later this month: Сервис Б 60, the contractor on 13, 20, 27 = 120, reinvest 2500, owner salary rest 3500 − 800 = 2700.
	if !near(a.CommitmentsLate, 60+120+2500+2700) {
		t.Fatalf("later %v %+v", a.CommitmentsLate, a.LaterItems)
	}
	if !near(a.FreeAfter, a.FreeNow-a.CommitmentsLate) {
		t.Fatalf("free after %v", a.FreeAfter)
	}
	// With a coverage: riding + available + hold reaching the withdrawal, at the average k.
	if _, err := s.RW.Exec(`INSERT INTO coverage_runs(as_of, started_at, finished_at, status) VALUES ('x', 'x', 'x', 'ok')`); err != nil {
		t.Fatal(err)
	}
	ins := func(state, avail string, est float64) {
		if _, err := s.RW.Exec(`INSERT INTO coverage_parts(page_label, payment_key, paid_at, avail_at, net, part, state, rate, est) VALUES ('p', 'k', 'x', ?, 1, 1, ?, 1, ?)`, avail, state, est); err != nil {
			t.Fatal(err)
		}
	}
	ins("riding", "2026-10-01T00:00:00.000+03:00", 1000)
	ins("page", "2026-11-01T00:00:00.000+03:00", 200)
	ins("hold", "2026-11-08T00:00:00.000+03:00", 300)
	ins("hold", "2026-11-12T00:00:00.000+03:00", 777) // after the withdrawal
	ins("stuck", "2026-10-01T00:00:00.000+03:00", 55)
	for _, k := range []float64{0.99, 1.01} {
		if _, err := s.RW.Exec(`INSERT INTO coverage_transfers(num, model, date, cut, amount, est, k) VALUES (?, 'M', '2026-10-01', 'x', 1, 1, ?)`, int(k*100), k); err != nil {
			t.Fatal(err)
		}
	}
	b, err := Compute(ctx, s.RO, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.TransitStatus != "ok" || !near(b.Expected, 1500) {
		t.Fatalf("expected %v %s", b.Expected, b.ExpectedNote)
	}
	if !near(b.FreeAfter, b.FreeNow+1500-b.CommitmentsLate) {
		t.Fatalf("free after %v", b.FreeAfter)
	}
	set(t, s, "cushion", "1000")
	c, err := Compute(ctx, s.RO, now)
	if err != nil || c.Cushion != 1000 {
		t.Fatalf("owner's cushion %v %v", c.Cushion, err)
	}
	if err := Record(ctx, s.RW, c, now); err != nil {
		t.Fatal(err)
	}
	var freeNow sql.NullFloat64
	if err := s.RO.QueryRow(`SELECT free_now FROM v_free`).Scan(&freeNow); err != nil || !near(freeNow.Float64, c.FreeNow) {
		t.Fatalf("v_free %v %v", freeNow, err)
	}
}
