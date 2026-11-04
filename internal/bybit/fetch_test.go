package bybit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWindowsCoverTheRangeWithoutGaps(t *testing.T) {
	from, to := at("2026-08-01"), at("2026-11-04T12:00:00+03:00")
	ws := windows(from, to, window)
	if len(ws) != 4 {
		t.Fatalf("%d windows", len(ws))
	}
	if !ws[0].From.Equal(from) || !ws[len(ws)-1].To.Equal(to) {
		t.Fatalf("ends %v", ws)
	}
	for i, w := range ws {
		if w.To.Sub(w.From) > window {
			t.Fatalf("window %d is %v", i, w.To.Sub(w.From))
		}
		if i > 0 && !w.From.Equal(ws[i-1].To) {
			t.Fatalf("gap before window %d", i)
		}
	}
	if windows(to, to, window) != nil {
		t.Fatal("empty range gives windows")
	}
}

func TestFetchMovementsWindowsPagesAndStatuses(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	// Five deposits over three months: pages of two, and a window boundary crossed.
	f.deposits = []map[string]any{
		deposit("1", "2026-08-02T10:00:00+03:00", "100", "TfromA"),
		deposit("2", "2026-08-03T10:00:00+03:00", "20.5", "TfromB"),
		deposit("3", "2026-08-04T10:00:00+03:00", "30", "TfromC"),
		deposit("4", "2026-09-20T10:00:00+03:00", "40", ""),
		deposit("5", "2026-10-25T10:00:00+03:00", "50", "TfromD"),
	}
	rolledBack := deposit("6", "2026-08-05T10:00:00+03:00", "60", "TfromE")
	rolledBack["status"] = 7
	f.deposits = append(f.deposits, rolledBack)
	f.internals = []map[string]any{
		internalDeposit("7", "2026-08-06T10:00:00+03:00", "70", "1001"),
		{"id": "8", "coin": "USDT", "amount": "80", "status": 1, "createdTime": secs("2026-08-07T10:00:00+03:00"), "fromMemberId": "1002"},
	}
	onChain := withdrawal("9", "2026-08-08T10:00:00+03:00", "90", "1", "TtoA", "success")
	internal := withdrawal("10", "2026-08-09T10:00:00+03:00", "100", "0", "2002", "success")
	internal["withdrawType"] = 1
	eth := withdrawal("11", "2026-08-10T10:00:00+03:00", "110", "2.5", "0xto", "success")
	eth["chain"] = "ETH"
	hanging := withdrawal("12", "2026-08-11T10:00:00+03:00", "120", "1", "TtoB", "SecurityCheck")
	cancelled := withdrawal("13", "2026-08-12T10:00:00+03:00", "130", "1", "TtoC", "CancelByUser")
	f.withdrawals = []map[string]any{onChain, internal, eth, hanging, cancelled}

	got, err := FetchMovements(ctx, c, at("2026-08-01"), at("2026-11-04"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Movement{}
	for i, m := range got.Movements {
		byKey[m.Key] = m
		if i > 0 && got.Movements[i-1].TimeMs > m.TimeMs {
			t.Fatalf("not in time order: %s", m.Key)
		}
	}
	if len(got.Movements) != 9 || got.Skipped != 4 || len(got.Pending) != 1 {
		t.Fatalf("movements %d skipped %d pending %d", len(got.Movements), got.Skipped, len(got.Pending))
	}
	if p := got.Pending[0]; p.Movement.Key != "withdrawal:12" || p.Status != "SecurityCheck" {
		t.Fatalf("pending %+v", p)
	}
	d := byKey["deposit:1"]
	if d.Direction != "income" || d.Method != "USDT TRC-20" || d.Counterparty != "TfromA" || d.CounterpartyKind != "address" ||
		d.Date != "2026-08-02" || d.At != "2026-08-02T10:00:00.000+03:00" || !strings.Contains(string(d.Raw), `"confirmations"`) {
		t.Fatalf("deposit %+v", d)
	}
	if m := byKey["internal_deposit:7"]; m.Direction != "income" || m.CounterpartyKind != "uid" || m.Counterparty != "1001" ||
		m.Method != "Internal Transfer (Bybit)" || m.TimeMs != at("2026-08-06T10:00:00+03:00").UnixMilli() {
		t.Fatalf("internal deposit (time comes in seconds) %+v", m)
	}
	if m := byKey["withdrawal:9"]; m.Direction != "expense" || m.Fee != "1" || m.Method != "USDT TRC-20" || m.CounterpartyKind != "address" {
		t.Fatalf("withdrawal %+v", m)
	}
	if m := byKey["withdrawal:10"]; m.CounterpartyKind != "uid" || m.Method != "Internal Transfer (Bybit)" {
		t.Fatalf("internal withdrawal %+v", m)
	}
	if m := byKey["withdrawal:11"]; m.Method != "USDT ERC-20" {
		t.Fatalf("ERC-20 withdrawal %+v", m)
	}
	// Pagination really ran: deposit pages of the first window hold more than one call.
	if n := len(f.callsTo(depositPath)); n < 5 {
		t.Fatalf("deposit requests %d: windows or pages skipped", n)
	}
}

func TestPaginationGuards(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	f.failCode[depositPath] = 131001
	if _, err := FetchMovements(ctx, c, at("2026-09-01"), at("2026-09-02")); err == nil {
		t.Fatal("a refused window must fail the pass")
	}
	answer := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(answer)) }))
	defer srv.Close()
	c.BaseURL = srv.URL
	answer = `{"retCode":0,"result":{"nextPageCursor":""}}`
	if _, err := paged(ctx, c, depositPath, nil); err == nil || !strings.Contains(err.Error(), "нет rows") {
		t.Fatalf("no rows: %v", err)
	}
	answer = `{"retCode":0,"result":{"rows":[],"nextPageCursor":"same"}}`
	if _, err := paged(ctx, c, depositPath, nil); err == nil || !strings.Contains(err.Error(), "курсор повторился") {
		t.Fatalf("repeating cursor: %v", err)
	}
	answer = `{"retCode":0,"result":{"nextPageCursor":""}}`
	if _, err := FetchFunding(ctx, c, at("2026-09-01"), at("2026-09-02")); err == nil || !strings.Contains(err.Error(), "нет list") {
		t.Fatalf("funding without list: %v", err)
	}
	if _, err := FetchP2P(ctx, c, at("2026-09-01"), at("2026-09-02")); err == nil || !strings.Contains(err.Error(), "нет items") {
		t.Fatalf("p2p without items: %v", err)
	}
}

func TestP2PMapping(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	order := func(id, status, side, when string, extra map[string]any) map[string]any {
		o := map[string]any{"id": id, "status": status, "side": side, "createDate": ms(when), "tokenId": "USDT", "amount": "12300",
			"currencyId": "RUB", "price": "100", "targetNickName": "buyer" + id, "targetUserId": "77" + id}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	f.p2p = []map[string]any{
		order("1", "50", "1", "2026-09-05T10:00:00+03:00", map[string]any{"notifyTokenQuantity": "123.4561", "quantity": "590", "fee": "0"}),
		order("2", "50", "0", "2026-09-06T10:00:00+03:00", map[string]any{"quantity": "590"}),
		order("3", "50", "1", "2026-09-07T10:00:00+03:00", map[string]any{"amount": "1000", "price": "3"}), // fiat / price
		order("4", "20", "1", "2026-09-08T10:00:00+03:00", nil),                                            // not finished
		order("5", "50", "1", "2026-09-09T10:00:00+03:00", map[string]any{"quantity": "1", "targetNickName": ""}),
	}
	got, err := FetchP2P(ctx, c, at("2026-09-01"), at("2026-09-20"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("%d orders: %+v", len(got), got)
	}
	if m := got[0]; m.Key != "p2p:1" || m.Direction != "expense" || m.Amount != "123.4561" || m.Fiat != "12300 RUB" || m.Price != "100" ||
		m.Method != "P2P (Bybit)" || m.CounterpartyKind != "p2p" || m.Counterparty != "buyer1" || m.Coin != "USDT" {
		t.Fatalf("sell %+v", m)
	}
	if m := got[1]; m.Direction != "income" || m.Amount != "590" {
		t.Fatalf("buy %+v", m)
	}
	if m := got[2]; m.Amount != "333.3333" {
		t.Fatalf("fiat / price gives %s", m.Amount)
	}
	if m := got[3]; m.Counterparty != "775" {
		t.Fatalf("no nickname: %+v", m)
	}
	for _, call := range f.callsTo(p2pPath) {
		if call.Params["size"] != "30" {
			t.Fatalf("size %s", call.Params["size"])
		}
	}
}

func TestP2PPaging(t *testing.T) {
	f, c := newFake(t)
	for i := 0; i < 35; i++ {
		f.p2p = append(f.p2p, map[string]any{"id": "o" + strings.Repeat("x", i), "status": 50, "side": 1,
			"createDate": ms("2026-09-05T10:00:00+03:00"), "tokenId": "USDT", "quantity": "1", "amount": "100", "price": "100", "currencyId": "RUB"})
	}
	got, err := FetchP2P(context.Background(), c, at("2026-09-01"), at("2026-09-20"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 35 || len(f.callsTo(p2pPath)) != 2 {
		t.Fatalf("%d orders in %d pages", len(got), len(f.callsTo(p2pPath)))
	}
}

func TestFetchWithdrawalByID(t *testing.T) {
	f, c := newFake(t)
	f.withdrawals = []map[string]any{withdrawal("55", "2026-08-08T10:00:00+03:00", "90", "1", "TtoA", "success")}
	st, m, err := FetchWithdrawal(context.Background(), c, "55")
	if err != nil || st != "success" || m == nil || m.Key != "withdrawal:55" {
		t.Fatal(st, m, err)
	}
	st, m, err = FetchWithdrawal(context.Background(), c, "56")
	if err != nil || st != "" || m != nil {
		t.Fatal(st, m, err)
	}
}

func TestToMsAndAmountHelpers(t *testing.T) {
	if v, _ := toMs("1700000000"); v != 1_700_000_000_000 {
		t.Fatal(v)
	}
	if v, _ := toMs("1700000000123"); v != 1_700_000_000_123 {
		t.Fatal(v)
	}
	if _, err := toMs("soon"); err == nil {
		t.Fatal("bad time accepted")
	}
	if v, err := toMicros("123.45610049"); err != nil || v.String() != "123.4561" {
		t.Fatal(v, err)
	}
	if v, _ := toMicros("0.0000005"); v != 0 { // a tie rounds to even
		t.Fatal(v)
	}
	if v, _ := toMicros("0.0000015"); v != 2 {
		t.Fatal(v)
	}
	if short("100") != "100" || short("123.45610") != "123.4561" || short("72.50") != "72.5" {
		t.Fatal(short("100"), short("123.45610"), short("72.50"))
	}
	if !sameAmount("100", "100.00") || sameAmount("100", "100.01") {
		t.Fatal("sameAmount")
	}
}
