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
	// The Funding check leaves both conversions to PostConverts; a USDC swap stays for the owner.
	add("5", "2026-10-31T10:00:00+03:00", "USDC", "-50")
	add("6", "2026-10-31T10:00:00+03:00", "USDT", "49.98")
	conv, err := ConvertedFunding(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"1": true, "2": true, "3": true, "4": true, "5": false, "6": false} {
		if conv[id] != want {
			t.Errorf("converted[%s] = %v, want %v", id, conv[id], want)
		}
	}
	for acct, want := range map[string]string{"bybit": "103.50", "bybit:btc": "0", "income:other": "-3.50"} {
		got, err := ledger.Balance(ctx, s.RO, acct, "", "")
		if err != nil || got != money.MustParse(want) {
			t.Errorf("%s = %s (%v), want %s", acct, got, err, want)
		}
	}
}

// Rows are written later than the movement: the conversion follows them by a reversal and a new entry.
func TestConvertFollowsTheJournal(t *testing.T) {
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
	funding := func(id, at, coin, signed, kind string) {
		t.Helper()
		if _, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "bybit", SourceID: "funding:" + id, Kind: "funding", OccurredAt: at,
			Payload: FundingRow{ID: id, Coin: coin, Signed: signed, Type: kind, At: at}}, now); err != nil {
			t.Fatal(err)
		}
	}
	bal := func(acct string) money.Micros {
		t.Helper()
		v, err := ledger.Balance(ctx, s.RW, acct, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	run := func() {
		t.Helper()
		if _, err := PostConverts(ctx, s.RW, now); err != nil {
			t.Fatal(err)
		}
	}
	// The sale is collected before any row: the coin's account has to exist for it to count.
	if err := ledger.EnsureAccount(ctx, s.RW, "bybit:btc", "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	funding("1", "2026-10-14T11:55:00+03:00", "BTC", "-0.001", "Convert")
	funding("2", "2026-10-14T11:55:00+03:00", "USDT", "100.00", "Convert")
	run()
	if bal("bybit") != money.MustParse("100.00") || bal("income:other") != -money.MustParse("100.00") {
		t.Fatalf("first run: bybit %s, other %s", bal("bybit"), bal("income:other"))
	}
	// The deposit's row arrives: the sale now takes its book value, the leftover clears.
	v := money.MustParse("90.00")
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-10-01", Kind: "test", Author: "t",
		Lines: []ledger.Line{{Account: "bybit:btc", Amount: v}, {Account: "income:custom", Amount: -v}}}, now); err != nil {
		t.Fatal(err)
	}
	run()
	if bal("bybit:btc") != 0 || bal("income:other") != -money.MustParse("10.00") || bal("bybit") != money.MustParse("100.00") {
		t.Fatalf("after the row: btc %s, other %s, bybit %s", bal("bybit:btc"), bal("income:other"), bal("bybit"))
	}
	run()
	if n := bal("bybit"); n != money.MustParse("100.00") {
		t.Fatalf("a repeat moved bybit to %s", n)
	}
	// A row that counts the USDT itself arrives: the entry goes away, nothing is counted twice.
	if _, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 1, Date: "2026-10-14", Type: "income", Category: "Custom", Counterparty: "Фан", Model: "—",
		Amount: "100.0000", Currency: "USDT", Method: "BTC (Bybit) → Convert USDT", Status: "paid", Note: "n"}, now); err != nil {
		t.Fatal(err)
	}
	run()
	if bal("bybit") != 0 || bal("income:other") != 0 {
		t.Fatalf("with a journal row: bybit %s, other %s", bal("bybit"), bal("income:other"))
	}
}

// A partial sale takes only the sold part of the book value.
func TestConvertPartialSale(t *testing.T) {
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
	v := money.MustParse("1000.00")
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-10-01", Kind: "test", Author: "t",
		Lines: []ledger.Line{{Account: "bybit:btc", Amount: v}, {Account: "income:custom", Amount: -v}}}, now); err != nil {
		t.Fatal(err)
	}
	for _, r := range []FundingRow{
		{ID: "1", Coin: "BTC", Signed: "0.01", Type: "Deposit", At: "2026-10-01T10:00:00+03:00"},
		{ID: "2", Coin: "BTC", Signed: "-0.001", Type: "Convert", At: "2026-10-14T11:55:00+03:00"},
		{ID: "3", Coin: "USDT", Signed: "120.00", Type: "Convert", At: "2026-10-14T11:55:00+03:00"},
	} {
		if _, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "bybit", SourceID: "funding:" + r.ID, Kind: "funding", OccurredAt: r.At, Payload: r}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PostConverts(ctx, s.RW, now); err != nil {
		t.Fatal(err)
	}
	for acct, want := range map[string]string{"bybit": "120.00", "bybit:btc": "900.00", "income:other": "-20.00"} {
		v, err := ledger.Balance(ctx, s.RW, acct, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if v != money.MustParse(want) {
			t.Errorf("%s: %s, want %s", acct, v, want)
		}
	}
}

// Bybit may write the two legs of a conversion in neighbouring seconds, and a purchase of a coin for USDT
// is the mirror of a sale: the USDT paid moves from bybit to the coin's account. A repeat posts nothing.
func TestConvertLegsInNeighbouringSecondsAndPurchase(t *testing.T) {
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
	v := money.MustParse("80.00")
	if _, err := ledger.Post(ctx, s.RW, ledger.Entry{EffectiveAt: "2026-10-01", Kind: "test", Author: "t",
		Lines: []ledger.Line{{Account: "bybit:btc", Amount: v}, {Account: "income:custom", Amount: -v}}}, now); err != nil {
		t.Fatal(err)
	}
	rows := []FundingRow{
		{ID: "1", Coin: "BTC", Signed: "0.002", Type: "Deposit", At: "2026-10-01T10:00:00+03:00"},
		// A sale, its legs one second apart.
		{ID: "2", Coin: "BTC", Signed: "-0.002", Type: "Convert", At: "2026-10-20T12:00:00+03:00"},
		{ID: "3", Coin: "USDT", Signed: "85.00", Type: "Convert", At: "2026-10-20T12:00:01+03:00"},
		// A lone leg one second before a sale of one second: the sale keeps its own legs.
		{ID: "4", Coin: "XRP", Signed: "-5", Type: "Convert", At: "2026-10-21T09:59:59+03:00"},
		{ID: "5", Coin: "BTC", Signed: "0.001", Type: "Deposit", At: "2026-10-21T09:00:00+03:00"},
		{ID: "6", Coin: "USDT", Signed: "41.00", Type: "Convert", At: "2026-10-21T10:00:00+03:00"},
		{ID: "7", Coin: "BTC", Signed: "-0.001", Type: "Convert", At: "2026-10-21T10:00:00+03:00"},
		// A purchase, its legs one second apart.
		{ID: "8", Coin: "USDT", Signed: "-250", Type: "Convert", At: "2026-11-02T18:30:30+03:00"},
		{ID: "9", Coin: "BTC", Signed: "0.0031", Type: "Convert", At: "2026-11-02T18:30:31+03:00"},
		// Legs three seconds apart are two separate movements for the owner.
		{ID: "10", Coin: "USDT", Signed: "-10", Type: "Convert", At: "2026-11-03T08:00:00+03:00"},
		{ID: "11", Coin: "BTC", Signed: "0.0001", Type: "Convert", At: "2026-11-03T08:00:03+03:00"},
	}
	addAll := func() {
		t.Helper()
		for _, r := range rows {
			if _, _, err := ledger.AddEvent(ctx, s.RW, r.Event(), now); err != nil {
				t.Fatal(err)
			}
		}
	}
	addAll()
	if n, err := PostConverts(ctx, s.RW, now); err != nil || n != 3 {
		t.Fatalf("posted %d %v, want 3", n, err)
	}
	// The collector reads the same rows again and runs again: nothing is posted twice.
	addAll()
	if n, err := PostConverts(ctx, s.RW, now); err != nil || n != 0 {
		t.Fatalf("repeat posted %d %v", n, err)
	}
	// bybit: +85 − 250 + 41; bybit:btc: 80 − 80 (the whole coin sold) + 250 (bought); the second sale's book value is 0.
	for acct, want := range map[string]string{"bybit": "-124.00", "bybit:btc": "250.00", "income:other": "-46.00"} {
		got, err := ledger.Balance(ctx, s.RO, acct, "", "")
		if err != nil || got != money.MustParse(want) {
			t.Errorf("%s = %s (%v), want %s", acct, got, err, want)
		}
	}
	var memo, at string
	if err := s.RO.QueryRowContext(ctx, `SELECT memo, effective_at FROM entries WHERE source_key = 'bybit:convert:funding:8'`).Scan(&memo, &at); err != nil {
		t.Fatal(err)
	}
	if memo != "покупка BTC за USDT на Bybit" || at != "2026-11-02T18:30:31+03:00" {
		t.Errorf("purchase entry: %q at %s", memo, at)
	}
	conv, err := ConvertedFunding(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"2": true, "3": true, "4": false, "6": true, "7": true, "8": true, "9": true, "10": false, "11": false} {
		if conv[id] != want {
			t.Errorf("converted[%s] = %v, want %v", id, conv[id], want)
		}
	}
}
