package bybit

import (
	"context"
	"strings"
	"testing"
)

func fundingRecord(id, when, currency, amt, dir, after, kind string) map[string]any {
	return map[string]any{"currcCursor": id, "createTime": secs(when), "currency": currency, "txnAmt": amt, "ioDirection": dir,
		"afterAmt": after, "showBusiTypeEn": kind, "descriptionEn": "synthetic " + kind}
}

func TestFetchFundingWindowsAndSigns(t *testing.T) {
	f, c := newFake(t)
	f.funding = []map[string]any{
		fundingRecord("a", "2026-08-02T10:00:00+03:00", "USDT", "100", "I", "100", "Deposit"),
		fundingRecord("b", "2026-08-20T10:00:00+03:00", "USDT", "30.5", "O", "69.5", "Withdraw"),
		fundingRecord("c", "2026-09-10T10:00:00+03:00", "USDT", "9.5", "O", "60", "Fiat"),
	}
	rows, err := FetchFunding(context.Background(), c, at("2026-08-01"), at("2026-09-15"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	if rows[1].Signed != "-30.5" || rows[1].After != "69.5" || rows[1].Type != "Withdraw" || rows[1].At != "2026-08-20T10:00:00+03:00" {
		t.Fatalf("row %+v", rows[1])
	}
	if rows[0].Signed != "100" {
		t.Fatalf("income %+v", rows[0])
	}
	if n := len(f.callsTo(fundingPath)); n < 6 {
		t.Fatalf("%d requests for 45 days in 7-day windows", n)
	}
}

func funding(id string, sec int64, signed, after, kind string) FundingRow {
	return FundingRow{ID: id, TimeMs: sec * 1000, Coin: "USDT", Signed: signed, After: after, Type: kind}
}

func TestChainGaps(t *testing.T) {
	ok := []FundingRow{funding("a", 100, "100", "100", "Deposit"), funding("b", 200, "-30", "70", "Withdraw"), funding("c", 300, "5", "75", "Deposit")}
	if g := ChainGaps(ok); len(g) != 0 {
		t.Fatalf("clean chain has gaps: %+v", g)
	}
	// Same second, ids in the wrong order: the chain only holds with c before b.
	same := []FundingRow{funding("a", 100, "100", "100", "Deposit"), funding("b", 200, "-30", "65", "Withdraw"), funding("c", 200, "-5", "95", "Withdraw")}
	if g := ChainGaps(same); len(g) != 0 {
		t.Fatalf("same-second rows must be ordered by the chain: %+v", g)
	}
	broken := []FundingRow{funding("a", 100, "100", "100", "Deposit"), funding("b", 200, "-30", "60", "Withdraw")}
	g := ChainGaps(broken)
	if len(g) != 1 || g[0].Expected != "70" || g[0].Actual != "60" || g[0].Delta != "-10" || g[0].AfterID != "a" || g[0].BeforeID != "b" {
		t.Fatalf("gap %+v", g)
	}
	// Crypto amounts with eight places hold exactly.
	btc := []FundingRow{{ID: "x", TimeMs: 1000, Coin: "BTC", Signed: "0.12345678", After: "0.12345678"},
		{ID: "y", TimeMs: 2000, Coin: "BTC", Signed: "-0.00000001", After: "0.12345677"}}
	if g := ChainGaps(btc); len(g) != 0 {
		t.Fatalf("eight decimals: %+v", g)
	}
}

func TestMatchAndReconcile(t *testing.T) {
	base := at("2026-09-10T12:00:00+03:00").Unix()
	rows := []FundingRow{
		funding("w", base, "-101", "899", "Withdraw"),              // withdrawal 100 + fee 1
		funding("p", base+60, "-123.456", "775.544", "Fiat"),       // P2P 123.4561 within 0.001
		funding("d", base+120, "50", "825.544", "Deposit"),         // deposit
		funding("late", base+3*3600, "-10", "815.544", "Withdraw"), // the only operation is too far in time
		funding("tr", base+4*3600, "-2", "813.544", "Transfer"),    // between own accounts: not an operation
		funding("old", base+5*3600, "-12.5", "801.044", "Withdraw"),
	}
	ops := []Op{
		{Key: "withdrawal:1", Direction: "expense", Coin: "USDT", Amount: "100", Fee: "1", TimeMs: base * 1000},
		{Key: "p2p:2", Direction: "expense", Coin: "USDT", Amount: "123.4561", TimeMs: (base + 30) * 1000},
		{Key: "deposit:3", Direction: "income", Coin: "USDT", Amount: "50", TimeMs: (base + 100) * 1000},
		{Key: "withdrawal:4", Direction: "expense", Coin: "USDT", Amount: "10", Fee: "0", TimeMs: base * 1000},
		{Key: "withdrawal:5", Direction: "expense", Coin: "USDT", Amount: "10", TimeMs: (base + 6*3600) * 1000}, // no fee known: ±3
		{Key: "deposit:6", Direction: "income", Coin: "USDT", Amount: "1", TimeMs: (base + 200) * 1000},         // no Funding row
		{Key: "deposit:7", Direction: "income", Coin: "USDT", Amount: "1", TimeMs: (base - 1000) * 1000},        // before the window
	}
	m := Match(rows, ops)
	want := map[string]string{"w": "withdrawal:1", "p": "p2p:2", "d": "deposit:3", "old": "withdrawal:5"}
	if len(m) != len(want) {
		t.Fatalf("matched %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("matched %v", m)
		}
	}
	rep := Reconcile(rows, ops, (base-100)*1000)
	if rep.Rows != 6 || rep.Matched != 4 || rep.Transfers != 1 || len(rep.ChainGaps) != 0 || rep.EndBalance["USDT"] != "801.044" {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.UnmatchedFunding) != 1 || rep.UnmatchedFunding[0].ID != "late" {
		t.Fatalf("unmatched funding %+v", rep.UnmatchedFunding)
	}
	if got := strings.Join(rep.UnmatchedOps, ","); got != "withdrawal:4,deposit:6" {
		t.Fatalf("unmatched ops %s", got)
	}
}

func TestMatchIsOneToOne(t *testing.T) {
	base := at("2026-09-10T12:00:00+03:00").Unix()
	rows := []FundingRow{funding("a", base, "-10", "90", "Withdraw"), funding("b", base+10, "-10", "80", "Withdraw")}
	ops := []Op{{Key: "withdrawal:1", Direction: "expense", Coin: "USDT", Amount: "9", Fee: "1", TimeMs: base * 1000}}
	if m := Match(rows, ops); len(m) != 1 || m["a"] != "withdrawal:1" {
		t.Fatalf("one operation matched twice: %v", m)
	}
}
