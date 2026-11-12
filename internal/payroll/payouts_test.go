package payroll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/goslingmanagment/ledger/internal/agentsql" // POST /v1/sql, GET /v1/schema
	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	_ "github.com/goslingmanagment/ledger/internal/free" // GET /v1/free
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	_ "github.com/goslingmanagment/ledger/internal/month" // GET /v1/month/{month}, /v1/months
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// payoutsBook seeds Альфа with two tabs, a payout of 90 by the table (+5 above it), and next to her the
// owner's card with a debt of its own and a model's share of a deal: neither has a tab.
func payoutsBook(t *testing.T) (*db.Store, *fakeSheets) {
	t.Helper()
	s, ctx := openStore(t)
	if err := PutPeople(ctx, s.RW, []Person{{Slug: "boss", Name: "Владелец", Status: "active", Workdays: []string{}}}, "test", t0("2026-09-01")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []ledger.Entry{
		{EffectiveAt: "2026-09-09", Kind: "test", Author: "test", Memo: "долг владельцу",
			Lines: []ledger.Line{{Account: "owner:draw", Amount: money.MustParse("500")}, {Account: "payable:boss", Amount: money.MustParse("-500")}}},
		{EffectiveAt: "2026-09-09", Kind: "test", Author: "test", Memo: "доля модели по сделке",
			Lines: []ledger.Line{{Account: "expense:other", Amount: money.MustParse("70")}, {Account: "payable:alpha", Amount: money.MustParse("-70")}}},
	} {
		if _, err := ledger.Post(ctx, tx, e, t0("2026-09-09T12:00:00+03:00")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f := newSheets("Alpha", "Beta")
	// 2026-09-07: 25; 09-08: 50 + 10; 09-09: worked, 0; 09-10: 20; 09-11: 30.
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(30, 75, week("2026-09-07", map[string][7]any{"page-1": {100, 200, 0, nil, 120, nil, nil}}))})
	f.set("Beta", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(30, 0, week("2026-09-07", map[string][7]any{"page-2": {nil, 40, nil, 80, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	importJournal(t, s, ctx, "2026-09-15T09:00:00+03:00",
		payout(1, "2026-09-15", "Альфа", "Alpha", "95.00", "по таблице {сверх таблицы: 5}"))
	rebuild(t, s, ctx, "2026-09-15T10:00:00+03:00")
	return s, f
}

func TestPayoutsEqualTheDebtsOfChattersOnly(t *testing.T) {
	s, f := payoutsBook(t)
	ctx := t.Context()
	now := t0("2026-09-15T10:30:00+03:00")
	ans, err := ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-15", now)
	if err != nil {
		t.Fatal(err)
	}
	// v_payables has the owner and the model too; the payouts have only the person with tabs.
	all, err := Payables(ctx, s.RO, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]Payable{}
	for _, p := range all {
		seen[p.Person] = p
	}
	if _, ok := seen["boss"]; !ok {
		t.Fatalf("the owner's debt should be in v_payables: %+v", all)
	}
	if _, ok := seen["alpha"]; !ok {
		t.Fatalf("the model's share should be in v_payables: %+v", all)
	}
	if len(ans.People) != 1 || ans.People[0].Slug != "alfa" {
		t.Fatalf("people %+v", ans.People)
	}
	p := ans.People[0]
	m := func(s string) int64 { return int64(money.MustParse(s)) }
	wantDays := []PayoutDay{
		{Date: "2026-09-07", Model: "Alpha", AmountMicros: m("25"), Worked: true, Pages: []PayoutPage{{"page-1", m("100")}}, Source: "sheet"},
		{Date: "2026-09-08", Model: "Alpha", AmountMicros: m("50"), Worked: true, Pages: []PayoutPage{{"page-1", m("200")}}, Source: "sheet"},
		{Date: "2026-09-08", Model: "Beta", AmountMicros: m("10"), Worked: true, Pages: []PayoutPage{{"page-2", m("40")}}, Source: "sheet"},
		{Date: "2026-09-09", Model: "Alpha", AmountMicros: 0, Worked: true, Pages: []PayoutPage{{"page-1", 0}}, Source: "sheet"},
		{Date: "2026-09-10", Model: "Beta", AmountMicros: m("20"), Worked: true, Pages: []PayoutPage{{"page-2", m("80")}}, Source: "sheet"},
		{Date: "2026-09-11", Model: "Alpha", AmountMicros: m("30"), Worked: true, Pages: []PayoutPage{{"page-1", m("120")}}, Source: "sheet"},
	}
	if !reflect.DeepEqual(p.Days, wantDays) {
		t.Fatalf("days\n got %+v\nwant %+v", p.Days, wantDays)
	}
	if p.Name != "Альфа" || p.Status != "active" || p.AccruedMicros != m("135") || p.PaidMicros != m("90") || p.BalanceMicros != m("45") ||
		p.ToPayMicros == nil || *p.ToPayMicros != m("60") || p.OldestUnpaidDate == nil || *p.OldestUnpaidDate != "2026-09-10" {
		t.Fatalf("person %+v", p)
	}
	wantUnpaid := []UnpaidDay{{Date: "2026-09-10", Kind: "day", Model: "Beta", UnpaidMicros: m("15")}, {Date: "2026-09-11", Kind: "day", Model: "Alpha", UnpaidMicros: m("30")}}
	if !reflect.DeepEqual(p.UnpaidDays, wantUnpaid) {
		t.Fatalf("unpaid %+v", p.UnpaidDays)
	}
	// «Не занесено» comes as v_payables counts it; one more empty day with money and one unproved day show up.
	before := PayoutUnentered{Days: seen["alfa"].UnenteredDays, ShareMicros: int64(money.FromFloat(seen["alfa"].UnenteredShare)), Unknown: seen["alfa"].UnenteredUnknown}
	if p.Unentered != before {
		t.Fatalf("unentered %+v, v_payables %+v", p.Unentered, before)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO payroll_unentered(model, name, person, date, page, window_from, window_to, status, reason, net_mills, share, checked_at)
		VALUES ('Alpha', 'Альфа', 'alfa', '2026-09-14', 'page-1', '2026-09-14T00:00:00+03:00', '2026-09-14T08:00:00+03:00', 'ok', '', 50000, 12.5, '2026-09-15T10:00:00+03:00'),
		       ('Alpha', 'Альфа', 'alfa', '2026-09-15', 'page-1', '2026-09-15T00:00:00+03:00', '2026-09-15T08:00:00+03:00', 'unknown', 'хаб не доказал', NULL, NULL, '2026-09-15T10:00:00+03:00')`); err != nil {
		t.Fatal(err)
	}
	again, err := ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-15", now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := again.People[0].Unentered, (PayoutUnentered{Days: before.Days + 1, ShareMicros: before.ShareMicros + m("12.5"), Unknown: before.Unknown + 1}); got != want {
		t.Fatalf("unentered %+v, want %+v", got, want)
	}
	// The same numbers as the books: v_payables, payable_days and the accrual postings.
	if p.BalanceMicros != seen["alfa"].BalanceMicros {
		t.Fatalf("balance %d, v_payables %d", p.BalanceMicros, seen["alfa"].BalanceMicros)
	}
	if got := int64(count(t, s, `SELECT SUM(unpaid) FROM payable_days WHERE person = 'alfa' AND unpaid > 0`)); got != m("45") {
		t.Fatalf("payable_days unpaid %d", got)
	}
	if got := int64(count(t, s, `SELECT -SUM(l.amount) FROM lines l JOIN entries e ON e.id = l.entry_id
		WHERE e.kind = ? AND l.account = 'payable:alfa'`, KindAccrual)); got != p.AccruedMicros {
		t.Fatalf("accrual postings %d, accrued %d", got, p.AccruedMicros)
	}
	if ans.AccrualFrom != "2026-09-01" || ans.GeneratedAt != "2026-09-15T10:30:00+03:00" {
		t.Fatalf("%+v", ans)
	}
	// The sheets were last seen a day ago: stale. A run of repeats is a contact, and the debts were rebuilt
	// half an hour ago: fresh.
	if !ans.Checks.Stale || *ans.Checks.LastSheetsCollectAt != "2026-09-14T10:00:00+03:00" || *ans.Checks.LastRebuildAt != "2026-09-15T10:00:00+03:00" {
		t.Fatalf("checks %+v %s %s", ans.Checks, *ans.Checks.LastSheetsCollectAt, *ans.Checks.LastRebuildAt)
	}
	f.run(t, s, ctx, "2026-09-15T10:20:00+03:00")
	if ans, err = ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-15", now); err != nil || ans.Checks.Stale {
		t.Fatalf("after a run: %+v %v", ans.Checks, err)
	}

	// A narrower window: its days and no payout; the debt is the same, it is now.
	ans, err = ReadPayouts(ctx, s.RO, "2026-09-08", "2026-09-10", now)
	if err != nil {
		t.Fatal(err)
	}
	p = ans.People[0]
	if len(p.Days) != 4 || p.AccruedMicros != m("80") || p.PaidMicros != 0 || p.BalanceMicros != m("45") || len(p.UnpaidDays) != 2 {
		t.Fatalf("narrow %+v", p)
	}
}

func TestPayoutsKeySeesOnlyPayouts(t *testing.T) {
	s, _ := payoutsBook(t)
	ctx := t.Context()
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	key := func(name, scope string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, []string{scope}, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	hub, agent, mac := key("hub", auth.ScopePayouts), key("agent", auth.ScopeRead), key("mac", auth.ScopeCollect)
	status := func(err error) int {
		t.Helper()
		if err == nil {
			return http.StatusOK
		}
		e, ok := err.(*client.Error)
		if !ok {
			t.Fatalf("not an HTTP answer: %v", err)
		}
		return e.Status
	}
	const window = "/v1/payouts?from=2026-09-07&to=2026-09-15"
	want, err := ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-15", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*client.Client{"payouts": hub, "read": agent} {
		var got PayoutsAnswer
		if err := c.Do(ctx, http.MethodGet, window, nil, &got); err != nil {
			t.Fatalf("%s key: %v", name, err)
		}
		if !got.OK || got.From != "2026-09-07" || got.To != "2026-09-15" || !reflect.DeepEqual(got.People, want.People) {
			t.Fatalf("%s key: %+v", name, got)
		}
		if _, err := ts.Parse(got.GeneratedAt); err != nil || got.GeneratedAt[len(got.GeneratedAt)-6:] != "+03:00" {
			t.Fatalf("generatedAt %q", got.GeneratedAt)
		}
	}
	if code := status(mac.Do(ctx, http.MethodGet, window, nil, nil)); code != http.StatusForbidden {
		t.Fatalf("collect key on payouts: %d", code)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sql"}, {http.MethodGet, "/v1/schema"}, {http.MethodGet, "/v1/payables"}, {http.MethodGet, "/v1/free"},
		{http.MethodGet, "/v1/month/2026-09"}, {http.MethodGet, "/v1/months"}, {http.MethodGet, "/v1/month-brief"},
		{http.MethodGet, "/v1/export/ledger-md"}, {http.MethodPost, "/v1/events"},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{"sql": "SELECT * FROM v_balances"}
		}
		if code := status(hub.Do(ctx, r.method, r.path, body, nil)); code != http.StatusForbidden {
			t.Errorf("payouts key on %s %s: %d, want 403", r.method, r.path, code)
		}
	}
	for q, code := range map[string]int{
		"":                                     http.StatusBadRequest,
		"?from=2026-09-07":                     http.StatusBadRequest,
		"?from=2026-9-7&to=2026-09-15":         http.StatusBadRequest,
		"?from=2026-09-15&to=2026-09-07":       http.StatusBadRequest,
		"?from=2026-09-01&to=2026-11-02":       http.StatusBadRequest, // 63 days
		"?from=2026-09-01&to=2026-11-01":       http.StatusOK,         // 62
		"?from=2026-09-07&to=2026-09-07":       http.StatusOK,
		"?from=2026-02-30&to=2026-03-01":       http.StatusBadRequest,
		"?from=2026-09-07T00:00&to=2026-09-08": http.StatusBadRequest,
	} {
		if got := status(hub.Do(ctx, http.MethodGet, "/v1/payouts"+q, nil, nil)); got != code {
			t.Errorf("window %q: %d, want %d", q, got, code)
		}
	}
	nobody := client.New(config.Client{URL: srv.URL, Key: "ldg_nobody_x"})
	if code := status(nobody.Do(ctx, http.MethodGet, window, nil, nil)); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
}

// The payouts read tells Hub's owner screens which source prices a day: from payroll_terms_hub_from the people
// registry's terms, from payroll_basis_hub_from Hub's handed-in reports; null while a switch is empty.
func TestPayoutsSayWhetherHubsTermsAndReportsPriceTheDays(t *testing.T) {
	s, _ := payoutsBook(t)
	ctx := t.Context()
	now := t0("2026-09-15T10:30:00+03:00")
	ans, err := ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-15", now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ans)
	if ans.TermsHubFrom != nil || ans.BasisHubFrom != nil || !strings.Contains(string(raw), `"termsHubFrom":null,"basisHubFrom":null`) {
		t.Fatalf("switches while empty: %s", raw)
	}
	if err := SetTermsHubFrom(ctx, s.RW, "2026-09-14", now); err != nil {
		t.Fatal(err)
	}
	if err := SetReportsFrom(ctx, s.RW, "2026-09-15", now); err != nil {
		t.Fatal(err)
	}
	if err := SetHubFrom(ctx, s.RW, "2026-09-15", now); err != nil {
		t.Fatal(err)
	}
	if ans, err = ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-15", now); err != nil || ans.TermsHubFrom == nil || *ans.TermsHubFrom != "2026-09-14" ||
		ans.BasisHubFrom == nil || *ans.BasisHubFrom != "2026-09-15" {
		t.Fatalf("switches set: %+v %v", ans, err)
	}
}
