package bybit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
)

func TestConvertPostsOnceAndRespectsTheJournal(t *testing.T) {
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
	add := func(id, at, coin, signed string) {
		t.Helper()
		if _, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "bybit", SourceID: "funding:" + id, Kind: "funding", OccurredAt: at,
			Payload: FundingRow{ID: id, Coin: coin, Signed: signed, Type: "Convert", At: at}}, now); err != nil {
			t.Fatal(err)
		}
	}
	// The coin's deposit stands on bybit:btc at 100 dollars.
	v := money.MustParse("100.00")
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-10-01", Kind: "test", Author: "t",
		Lines: []ledger.Line{{Account: "bybit:btc", Amount: v}, {Account: "income:custom", Amount: -v}}}, now); err != nil {
		t.Fatal(err)
	}
	add("1", "2026-10-14T11:55:00+03:00", "BTC", "-0.001")
	add("2", "2026-10-14T11:55:00+03:00", "USDT", "103.50")
	// A conversion the journal already counted through «… → Convert USDT».
	add("3", "2026-10-30T22:54:00+03:00", "BTC", "-0.002")
	add("4", "2026-10-30T22:54:00+03:00", "USDT", "378.09")
	if _, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 1, Date: "2026-10-30", Type: "income", Category: "Custom", Counterparty: "Фан", Model: "Alfa",
		Amount: "378.0900", Currency: "USDT", Method: "BTC (Bybit) → Convert USDT", Status: "paid", Note: "x {доля 0}", Author: "t", Origin: "t"}, now); err != nil {
		t.Fatal(err)
	}
	n, err := PostConverts(ctx, s.RW, now)
	if err != nil || n != 1 {
		t.Fatalf("posted %d %v", n, err)
	}
	if again, err := PostConverts(ctx, s.RW, now); err != nil || again != 0 {
		t.Fatalf("repeat posted %d %v", again, err)
	}
	for acct, want := range map[string]string{"bybit": "103.50", "bybit:btc": "0", "income:other": "-3.50"} {
		got, err := ledger.Balance(ctx, s.RO, acct, "", "")
		if err != nil || got != money.MustParse(want) {
			t.Errorf("%s = %s (%v), want %s", acct, got, err, want)
		}
	}
}
