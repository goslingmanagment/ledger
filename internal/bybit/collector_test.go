package bybit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func collector(s *db.Store, c *Client, now string) *Collector {
	col := NewCollector(s, c)
	col.Now = func() time.Time { return at(now) }
	return col
}

func count(t *testing.T, s *db.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.RW.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(query, err)
	}
	return n
}

func TestPendingWithdrawalBecomesEventWhenItSucceeds(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	w := withdrawal("70", "2026-09-10T10:00:00+03:00", "100", "1", "TtoA", "SecurityCheck")
	f.withdrawals = []map[string]any{w}

	st, err := collector(s, c, "2026-09-10T12:00:00+03:00").Run(ctx, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM events WHERE source_id = 'withdrawal:70'`) != 0 {
		t.Fatal("a hanging withdrawal must not be an event")
	}
	if len(st.Pending) != 1 || st.Pending[0].Key != "withdrawal:70" || st.Pending[0].Status != "SecurityCheck" {
		t.Fatalf("status pending %+v", st.Pending)
	}

	// Still hanging, new status: the window of the next pass still holds it, the table follows.
	w["status"] = "Pending"
	if st, err = collector(s, c, "2026-09-11T12:00:00+03:00").Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM bybit_pending WHERE key = 'withdrawal:70' AND last_status = 'Pending' AND resolved_at IS NULL`) != 1 {
		t.Fatal("status not updated from the window")
	}

	// Two passes later the withdrawal is out of the window (watermark − 72 h): asked by id.
	for _, now := range []string{"2026-09-20T12:00:00+03:00", "2026-09-20T12:15:00+03:00"} {
		if st, err = collector(s, c, now).Run(ctx, RunOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	found := false
	for _, call := range f.callsTo(withdrawPath) {
		if call.Params["withdrawID"] == "70" {
			found = true
		}
	}
	if !found {
		t.Fatal("no request by withdrawID")
	}
	if len(st.Pending) != 1 {
		t.Fatalf("still hanging: %+v", st.Pending)
	}

	w["status"] = "success"
	st, err = collector(s, c, "2026-09-21T12:00:00+03:00").Run(ctx, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.ResolvedPending != 1 || st.NewEvents != 1 || len(st.Pending) != 0 {
		t.Fatalf("status %+v", st)
	}
	if count(t, s, `SELECT count(*) FROM events WHERE source = 'bybit' AND source_id = 'withdrawal:70' AND kind = 'withdrawal'`) != 1 {
		t.Fatal("success did not become an event")
	}
	if count(t, s, `SELECT count(*) FROM bybit_pending WHERE key = 'withdrawal:70' AND resolution = 'success'`) != 1 {
		t.Fatal("pending not closed")
	}
	if count(t, s, `SELECT count(*) FROM proposals WHERE key = 'withdrawal:70' AND status = 'open'`) != 1 {
		t.Fatal("a late success must reach the brief")
	}
}

func TestPendingWithdrawalSucceedsInsideTheWindowAndCancelled(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	a := withdrawal("71", "2026-09-10T10:00:00+03:00", "10", "1", "TtoA", "BlockchainConfirmed")
	b := withdrawal("72", "2026-09-10T11:00:00+03:00", "20", "1", "TtoB", "Pending")
	f.withdrawals = []map[string]any{a, b}
	if _, err := collector(s, c, "2026-09-10T12:00:00+03:00").Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	a["status"] = "success"
	b["status"] = "Reject"
	st, err := collector(s, c, "2026-09-10T13:00:00+03:00").Run(ctx, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEvents != 1 || st.ResolvedPending != 2 || len(st.Pending) != 0 {
		t.Fatalf("status %+v", st)
	}
	if count(t, s, `SELECT count(*) FROM events WHERE source_id = 'withdrawal:72'`) != 0 {
		t.Fatal("rejected withdrawal became an event")
	}
	if count(t, s, `SELECT count(*) FROM bybit_pending WHERE key = 'withdrawal:72' AND resolution = 'Reject'`) != 1 ||
		count(t, s, `SELECT count(*) FROM bybit_pending WHERE key = 'withdrawal:71' AND resolution = 'success'`) != 1 {
		t.Fatal("pending not closed")
	}
}

func TestRepeatedPassesDoNotDuplicate(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	d := deposit("1", "2026-09-05T10:00:00+03:00", "100", "TfromA")
	f.deposits = []map[string]any{d}
	f.internals = []map[string]any{internalDeposit("2", "2026-09-05T11:00:00+03:00", "5", "1001")}
	f.withdrawals = []map[string]any{withdrawal("3", "2026-09-05T12:00:00+03:00", "7", "1", "TtoA", "success")}
	f.p2p = []map[string]any{{"id": "4", "status": 50, "side": 1, "createDate": ms("2026-09-05T13:00:00+03:00"), "tokenId": "USDT",
		"quantity": "9", "amount": "900", "price": "100", "currencyId": "RUB", "targetNickName": "buyer"}}
	f.funding = []map[string]any{fundingRecord("f1", "2026-09-05T10:00:01+03:00", "USDT", "100", "I", "100", "Deposit")}

	col := collector(s, c, "2026-09-05T14:00:00+03:00")
	st, err := col.Run(ctx, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEvents != 4 || st.NewFunding != 1 || st.Proposals.Unknown != 4 {
		t.Fatalf("first pass %+v", st)
	}
	events := count(t, s, `SELECT count(*) FROM events`)
	proposals := count(t, s, `SELECT count(*) FROM proposals`)

	// A volatile raw field changes between reads: the first observation stays and nothing conflicts.
	d["confirmations"] = "99"
	col.Now = func() time.Time { return at("2026-09-05T14:15:00+03:00") }
	st, err = col.Run(ctx, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEvents != 0 || st.KnownEvents != 4 || st.NewFunding != 0 || st.Proposals != (ClassifyReport{}) || len(st.Mismatches) != 0 {
		t.Fatalf("second pass %+v", st)
	}
	if count(t, s, `SELECT count(*) FROM events`) != events || count(t, s, `SELECT count(*) FROM proposals`) != proposals {
		t.Fatal("a repeat pass added rows")
	}
	if !st.OK || st.LastOKAt != "2026-09-05T14:15:00+03:00" {
		t.Fatalf("status %+v", st)
	}

	// A changed amount is reported, the stored fact stays.
	d["amount"] = "101"
	col.Now = func() time.Time { return at("2026-09-05T14:30:00+03:00") }
	if st, err = col.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(st.Mismatches) != 1 || !strings.Contains(st.Mismatches[0], "deposit:1") {
		t.Fatalf("mismatches %v", st.Mismatches)
	}
}

func TestWatermarksAndFailedStreams(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)

	first := "2026-09-05T14:00:00+03:00"
	if _, err := collector(s, c, first).Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	calls := f.callsTo(depositPath)
	if calls[0].Params["startTime"] != ms("2026-08-01") {
		t.Fatalf("first pass starts at %s", calls[0].Params["startTime"])
	}
	f.resetCalls()
	f.failCode[p2pPath] = 10005

	second := "2026-09-05T14:15:00+03:00"
	st, err := collector(s, c, second).Run(ctx, RunOptions{})
	if err == nil || st.OK || st.Errors["p2p"] == "" {
		t.Fatalf("failed stream: err %v status %+v", err, st)
	}
	calls = f.callsTo(depositPath)
	if want := at(first).Add(-72 * time.Hour); calls[0].Params["startTime"] != msParam(want) {
		t.Fatalf("second pass starts at %s, want watermark − 72 h", calls[0].Params["startTime"])
	}
	if wm, _, _ := watermark(ctx, s.RW, cursorMovements); !wm.Equal(at(second)) {
		t.Fatalf("movements watermark %v", wm)
	}
	if wm, _, _ := watermark(ctx, s.RW, cursorP2P); !wm.Equal(at(first)) {
		t.Fatalf("a failed stream must keep its watermark: %v", wm)
	}
	raw, _ := getCursor(ctx, s.RW, cursorStatus)
	var saved Status
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.OK || saved.LastOKAt != ts.Format(at(first)) || saved.Errors["p2p"] == "" {
		t.Fatalf("saved status %+v", saved)
	}

	// The next clean pass reads P2P from the old watermark again.
	delete(f.failCode, p2pPath)
	f.resetCalls()
	if _, err := collector(s, c, "2026-09-05T14:30:00+03:00").Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	var begin string
	for _, call := range f.callsTo(p2pPath) {
		begin = call.Params["beginTime"]
		break
	}
	if want := at(first).Add(-72 * time.Hour); begin != msParam(want) {
		t.Fatalf("P2P restarts at %s", begin)
	}
}

func TestBackfillLeavesWatermarksAlone(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	f.deposits = []map[string]any{deposit("1", "2026-08-05T10:00:00+03:00", "100", "TfromA")}
	st, err := collector(s, c, "2026-09-05T14:00:00+03:00").Run(ctx, RunOptions{Since: at("2026-08-01"), Until: at("2026-08-10")})
	if err != nil || st.NewEvents != 1 || !st.Backfill {
		t.Fatal(err, st)
	}
	if _, ok, _ := watermark(ctx, s.RW, cursorMovements); ok {
		t.Fatal("a backfill moved the watermark")
	}
	if v, _ := getCursor(ctx, s.RW, cursorStatus); v != "" {
		t.Fatal("a backfill wrote the status")
	}
}

func TestCustomStartDate(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('bybit_start', '2026-09-01', 'x')`); err != nil {
		t.Fatal(err)
	}
	f.deposits = []map[string]any{deposit("1", "2026-08-20T10:00:00+03:00", "100", "TfromA")}
	if _, err := collector(s, c, "2026-09-05T14:00:00+03:00").Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.callsTo(depositPath)[0].Params["startTime"]; got != ms("2026-09-01") {
		t.Fatalf("start %s", got)
	}
}

func TestKeyThatCanTradeStopsCollection(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	f.readOnly = 0
	f.deposits = []map[string]any{deposit("1", "2026-09-05T10:00:00+03:00", "100", "TfromA")}
	st, err := collector(s, c, "2026-09-05T14:00:00+03:00").Run(ctx, RunOptions{})
	if err == nil || st.OK || st.Key == nil || st.Key.ReadOnly {
		t.Fatalf("err %v status %+v", err, st)
	}
	if count(t, s, `SELECT count(*) FROM events`) != 0 {
		t.Fatal("collected with a key that can trade")
	}
	if len(f.callsTo(depositPath)) != 0 {
		t.Fatal("asked Bybit with a key that can trade")
	}
}

func TestFundingCheckGoesToStatus(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	f.deposits = []map[string]any{deposit("1", "2026-09-05T10:00:00+03:00", "100", "TfromA")}
	f.withdrawals = []map[string]any{withdrawal("2", "2026-09-05T11:00:00+03:00", "30", "1", "TtoA", "success")}
	f.funding = []map[string]any{
		fundingRecord("a", "2026-09-05T10:00:01+03:00", "USDT", "100", "I", "100", "Deposit"),
		fundingRecord("b", "2026-09-05T11:00:01+03:00", "USDT", "31", "O", "69", "Withdraw"),
		fundingRecord("c", "2026-09-05T12:00:00+03:00", "USDT", "9", "O", "50", "Convert"), // no operation, and the chain breaks
	}
	st, err := collector(s, c, "2026-09-05T14:00:00+03:00").Run(ctx, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Funding == nil || st.Funding.Rows != 3 || st.Funding.Matched != 2 || len(st.Funding.ChainGaps) != 1 ||
		len(st.Funding.UnmatchedFunding) != 1 || st.Funding.UnmatchedFunding[0].ID != "c" || len(st.Funding.UnmatchedOps) != 0 {
		t.Fatalf("funding %+v", st.Funding)
	}
	if count(t, s, `SELECT count(*) FROM events WHERE source = 'bybit' AND kind = 'funding' AND source_id = 'funding:a'`) != 1 {
		t.Fatal("funding rows are events")
	}
	if count(t, s, `SELECT count(*) FROM proposals WHERE key LIKE 'funding:%'`) != 0 {
		t.Fatal("funding is an observation: no proposals")
	}
	raw, _ := getCursor(ctx, s.RW, cursorStatus)
	if !strings.Contains(raw, `"chain_gaps":[{`) {
		t.Fatalf("status cursor %s", raw)
	}
}

func TestBalanceObservation(t *testing.T) {
	s, ctx := openDB(t)
	f, c := newFake(t)
	f.unified = []map[string]any{{"coin": "USDT", "walletBalance": "100.1234567"}, {"coin": "BTC", "walletBalance": "0.5"}, {"coin": "USDC", "walletBalance": "0.009"}}
	f.fund = []map[string]any{{"coin": "USDT", "walletBalance": "50"}, {"coin": "USDC", "walletBalance": "25.5"}, {"coin": "XRP", "walletBalance": ""}}
	now := at("2026-09-05T14:00:00+03:00")
	b, err := CollectBalance(ctx, s, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.StableTotal != "175.623457" {
		t.Fatalf("stable total %s", b.StableTotal)
	}
	var value int64
	var payload, observed string
	if err := s.RW.QueryRow(`SELECT value, payload, observed_at FROM observations WHERE kind = 'bybit_balance' AND subject = 'bybit' AND source = 'bybit_api'`).
		Scan(&value, &payload, &observed); err != nil {
		t.Fatal(err)
	}
	if value != 175_623_457 || observed != "2026-09-05T14:00:00+03:00" {
		t.Fatalf("value %d at %s", value, observed)
	}
	var detail Balance
	if err := json.Unmarshal([]byte(payload), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Wallets["UNIFIED"]["BTC"] != "0.5" || detail.Wallets["FUND"]["USDC"] != "25.5" || detail.Wallets["UNIFIED"]["USDC"] != "" {
		t.Fatalf("detail %+v", detail)
	}

	f.failCode["/v5/asset/transfer/query-account-coins-balance"] = 10005
	if _, err := CollectBalance(ctx, s, c, now.Add(time.Hour)); err == nil {
		t.Fatal("a failed wallet must fail the read")
	}
	if count(t, s, `SELECT count(*) FROM observations`) != 1 {
		t.Fatal("a partial total must not become an observation")
	}
	st, err := ReadStatus(ctx, s.RW)
	if err != nil || !strings.Contains(string(st["balance"]), `"ok":false`) || !strings.Contains(string(st["balance"]), `"last_ok_at":"2026-09-05T14:00:00+03:00"`) {
		t.Fatal(string(st["balance"]), err)
	}
}

func TestNoKeysLogsOnceAndDoesNothing(t *testing.T) {
	t.Setenv("LEDGER_BYBIT_API_KEY", "")
	t.Setenv("LEDGER_BYBIT_API_SECRET", "x")
	if _, ok := KeysFromEnv(); ok {
		t.Fatal("half a key pair accepted")
	}
	store, ctx := openDB(t)
	var logs bytes.Buffer
	srv := &server.Server{Store: store, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	for i := 0; i < 3; i++ {
		if err := collectJob(ctx, srv); err != nil {
			t.Fatal(err)
		}
		if err := balanceJob(ctx, srv); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "LEDGER_BYBIT_API_KEY"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
	if count(t, store, `SELECT count(*) FROM cursors`) != 0 || count(t, store, `SELECT count(*) FROM events`) != 0 {
		t.Fatal("the job did something without keys")
	}
	t.Setenv("LEDGER_BYBIT_API_KEY", "k")
	if k, ok := KeysFromEnv(); !ok || k.Key != "k" {
		t.Fatal("key pair not read")
	}
}
