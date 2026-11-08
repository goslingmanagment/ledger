package bybit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A coin bought for USDT and spent in payments written at a lower market price: the account keeps the
// cost of what is left at the average price, and the corrections bring it there once switched on.
func TestCoinCostAtAveragePrice(t *testing.T) {
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	add := func(r FundingRow) {
		t.Helper()
		if _, _, err := ledger.AddEvent(ctx, s.RW, r.Event(), now); err != nil {
			t.Fatal(err)
		}
	}
	// A payment: the withdrawal (amount and network fee), its journal row at the market price of the day.
	pay := func(n int, at, amount, fee, signed, after, value string) {
		t.Helper()
		add(FundingRow{ID: "w" + amount, Coin: "BTC", Signed: signed, After: after, Type: "Withdraw", At: at})
		tm, err := ts.Parse(at)
		if err != nil {
			t.Fatal(err)
		}
		m := Movement{Key: "withdrawal:" + amount, Kind: "withdrawal", Direction: "expense", TimeMs: tm.UnixMilli(), At: at, Date: at[:10], Coin: "BTC", Amount: amount, Fee: fee}
		id, _, err := ledger.AddEvent(ctx, s.RW, m.Event(), now)
		if err != nil {
			t.Fatal(err)
		}
		if value == "" {
			return // the row is not written yet
		}
		d, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: n, Date: at[:10], Type: "expense", Category: "Трафик", Counterparty: "X", Model: "—",
			Amount: value, Currency: "USD", Method: "BTC (Bybit)", Status: "paid", Note: m.Key, Author: "t", Origin: "t"}, now)
		if err != nil {
			t.Fatal(err)
		}
		v := money.MustParse(value)
		if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: at[:10], Kind: "document", Author: "t", DocumentID: d.ID,
			Lines: []ledger.Line{{Account: "bybit:btc", Amount: -v}, {Account: "expense:other", Amount: v}}}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := AddLink(ctx, s.RW, Link{EventID: id, DocumentNum: n, How: "note_key", Decision: "recorded"}, now); err != nil {
			t.Fatal(err)
		}
	}
	add(FundingRow{ID: "u1", Coin: "USDT", Signed: "-500", After: "1000", Type: "Convert", At: "2026-11-01T10:00:00+03:00"})
	add(FundingRow{ID: "b1", Coin: "BTC", Signed: "0.01", After: "0.01", Type: "Convert", At: "2026-11-01T10:00:01+03:00"})
	if _, err := PostConverts(ctx, s.RW, now); err != nil {
		t.Fatal(err)
	}
	// 0.004 cost 200 and was written at 180; 0.005 of the 0.006 left cost 250 and was written at 225.
	pay(1, "2026-11-02T12:00:00+03:00", "0.0039", "0.0001", "-0.004", "0.006", "180.00")
	pay(2, "2026-11-03T12:00:00+03:00", "0.0049", "0.0001", "-0.005", "0.001", "225.00")
	costs, err := CoinCosts(ctx, s.RO)
	if err != nil || len(costs) != 1 {
		t.Fatalf("costs %+v %v", costs, err)
	}
	c := costs[0]
	if c.Unknown != "" || c.Book != money.MustParse("95.00") || c.Cost != money.MustParse("50.00") || c.Qty != 0.001 || len(c.Outflows) != 2 {
		t.Fatalf("walk %+v", c)
	}
	// Off until the setting: the books of written payments do not change by themselves.
	if n, err := PostCoinCost(ctx, s.RW, now); err != nil || n != 0 {
		t.Fatalf("posted %d %v with the setting off", n, err)
	}
	plan, n, err := StartCoinCost(ctx, s.RW, "2026-11-01", now)
	if err != nil || n != 2 || len(plan.Corrections) != 2 || plan.Corrections[0].Amount != "-20.00" || plan.Corrections[1].Amount != "-25.00" {
		t.Fatalf("start: %d %+v %v", n, plan, err)
	}
	for acct, want := range map[string]string{"bybit:btc": "50.00", "income:other": "45.00"} {
		if got, err := ledger.Balance(ctx, s.RO, acct, "", ""); err != nil || got != money.MustParse(want) {
			t.Errorf("%s = %s (%v), want %s", acct, got, err, want)
		}
	}
	if n, err := PostCoinCost(ctx, s.RW, now); err != nil || n != 0 {
		t.Fatalf("repeat posted %d %v", n, err)
	}
	// A payment whose row is not written yet stops the corrections of its coin: its value is no price difference.
	pay(3, "2026-11-04T12:00:00+03:00", "0.0004", "0.0001", "-0.0005", "0.0005", "")
	plan, err = PlanCoinCost(ctx, s.RO, "2026-11-01")
	if err != nil || len(plan.Corrections) != 2 || len(plan.Stopped) != 1 || !strings.Contains(plan.Stopped[0], "без строки журнала") {
		t.Fatalf("a payment without its row: %+v %v", plan, err)
	}
}
