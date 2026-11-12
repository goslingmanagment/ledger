package payroll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// personBook is payoutsBook with a second person: Альфа has her two tabs, a payout of 90 by the table (+5 above it) and a
// bonus of 7 posted by hand; Браво has a tab of Alpha with 100 on 09-09 and a payout of 60. Next to them the owner's card
// and a model's share of a deal, with debts and no tab.
func personBook(t *testing.T) *db.Store {
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
		{EffectiveAt: "2026-09-12", Kind: "test", Author: "test", Memo: "премия за неделю",
			Lines: []ledger.Line{{Account: "expense:other", Amount: money.MustParse("7")}, {Account: "payable:alfa", Amount: money.MustParse("-7")}}},
	} {
		if _, err := ledger.Post(ctx, tx, e, t0("2026-09-12T12:00:00+03:00")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f := newSheets("Alpha", "Beta")
	f.set("Alpha", []string{"Альфа", "Браво"}, map[string][][]Cell{
		"Альфа": sheet(30, 75, week("2026-09-07", map[string][7]any{"page-1": {100, 200, 0, nil, 120, nil, nil}})),
		"Браво": sheet(40, 60, week("2026-09-07", map[string][7]any{"page-1": {nil, nil, 400, nil, nil, nil, nil}})),
	})
	f.set("Beta", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(30, 0, week("2026-09-07", map[string][7]any{"page-2": {nil, 40, nil, 80, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-16T10:00:00+03:00")
	importJournal(t, s, ctx, "2026-09-16T09:00:00+03:00",
		payout(1, "2026-09-15", "Альфа", "Alpha", "95.00", "по таблице {сверх таблицы: 5}"),
		payout(2, "2026-09-16", "Браво", "Alpha", "60.00", "по таблице"))
	rebuild(t, s, ctx, "2026-09-16T10:00:00+03:00")
	return s
}

func TestPersonPayIsTheBooksOfThatPersonOnly(t *testing.T) {
	s := personBook(t)
	ctx := t.Context()
	now := t0("2026-09-16T10:30:00+03:00")
	m := func(s string) int64 { return int64(money.MustParse(s)) }
	str := func(s string) *string { return &s }

	got, err := ReadPersonPay(ctx, s.RO, "alfa", "2026-09-07", "2026-09-16", now)
	if err != nil {
		t.Fatal(err)
	}
	// The payout of 90 pays 09-07, both 09-08 and 5 of 09-10 (FIFO); 09-09 accrued nothing; the bonus is a posting of its own.
	want := PersonPayAnswer{OK: true, Person: "alfa", Name: "Альфа", From: "2026-09-07", To: "2026-09-16", AccrualFrom: "2026-09-01",
		GeneratedAt: "2026-09-16T10:30:00+03:00", AccruedMicros: m("135"), PaidMicros: m("90"), BalanceMicros: m("52"),
		OldestUnpaidDate: str("2026-09-10"), LastPayout: &PersonPayout{Date: "2026-09-15", Document: 1, AmountMicros: m("90")},
		Days: []PersonDay{
			{Date: "2026-09-07", Model: "Alpha", AmountMicros: m("25"), Worked: true, Source: "sheet", Posts: []int64{}, State: DayPaid,
				PaidMicros: m("25"), PaidBy: []int{1}},
			{Date: "2026-09-08", Model: "Alpha", AmountMicros: m("50"), Worked: true, Source: "sheet", Posts: []int64{}, State: DayPaid,
				PaidMicros: m("50"), PaidBy: []int{1}},
			{Date: "2026-09-08", Model: "Beta", AmountMicros: m("10"), Worked: true, Source: "sheet", Posts: []int64{}, State: DayPaid,
				PaidMicros: m("10"), PaidBy: []int{1}},
			{Date: "2026-09-09", Model: "Alpha", Worked: true, Source: "sheet", Posts: []int64{}, PaidBy: []int{}},
			{Date: "2026-09-10", Model: "Beta", AmountMicros: m("20"), Worked: true, Source: "sheet", Posts: []int64{}, State: DayToPay,
				PaidMicros: m("5"), UnpaidMicros: m("15"), PaidBy: []int{1}},
			{Date: "2026-09-11", Model: "Alpha", AmountMicros: m("30"), Worked: true, Source: "sheet", Posts: []int64{}, State: DayToPay,
				UnpaidMicros: m("30"), PaidBy: []int{}},
		},
		Adjustments: []PersonAdjustment{{Date: "2026-09-12", AmountMicros: m("7"), Memo: "премия за неделю"}},
		Payouts:     []PersonPayout{{Date: "2026-09-15", Document: 1, AmountMicros: m("90")}},
		Checks:      PersonPayChecks{LastRebuildAt: str("2026-09-16T10:00:00+03:00")},
	}
	if !reflect.DeepEqual(got, want) {
		a, _ := json.MarshalIndent(got, "", " ")
		b, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("alfa\n got %s\nwant %s", a, b)
	}
	// The same money as the books: v_payables, the owner's GET /v1/payouts and the unpaid days of payable_days.
	all, err := ReadPayouts(ctx, s.RO, "2026-09-07", "2026-09-16", now)
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]PayoutPerson{}
	for _, p := range all.People {
		owner[p.Slug] = p
	}
	if p := owner["alfa"]; p.BalanceMicros != got.BalanceMicros || p.AccruedMicros != got.AccruedMicros || p.PaidMicros != got.PaidMicros {
		t.Fatalf("owner's view %+v, the person's %+v", p, got)
	}
	var unpaid int64
	for _, d := range got.Days {
		unpaid += d.UnpaidMicros
	}
	for _, a := range got.Adjustments {
		unpaid += a.AmountMicros // the bonus is unpaid: the payout went to older days
	}
	if unpaid != got.BalanceMicros || int64(count(t, s, `SELECT SUM(unpaid) FROM payable_days WHERE person = 'alfa'`)) != got.BalanceMicros {
		t.Fatalf("unpaid days %d, balance %d", unpaid, got.BalanceMicros)
	}

	// Браво sees her own day and payout, none of Альфа's.
	b, err := ReadPersonPay(ctx, s.RO, "bravo", "2026-09-07", "2026-09-16", now)
	if err != nil {
		t.Fatal(err)
	}
	wantDays := []PersonDay{{Date: "2026-09-09", Model: "Alpha", AmountMicros: m("100"), Worked: true, Source: "sheet", Posts: []int64{},
		State: DayToPay, PaidMicros: m("60"), UnpaidMicros: m("40"), PaidBy: []int{2}}}
	if !reflect.DeepEqual(b.Days, wantDays) || b.BalanceMicros != m("40") || b.AccruedMicros != m("100") || b.PaidMicros != m("60") ||
		len(b.Adjustments) != 0 || !reflect.DeepEqual(b.Payouts, []PersonPayout{{Date: "2026-09-16", Document: 2, AmountMicros: m("60")}}) ||
		b.LastPayout == nil || b.LastPayout.Document != 2 || owner["bravo"].BalanceMicros != b.BalanceMicros {
		t.Fatalf("bravo %+v", b)
	}
	// A narrower window: its days and payouts; the debt and the last payout are now.
	n, err := ReadPersonPay(ctx, s.RO, "alfa", "2026-09-10", "2026-09-12", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Days) != 2 || n.AccruedMicros != m("50") || n.PaidMicros != 0 || len(n.Payouts) != 0 || len(n.Adjustments) != 1 ||
		n.BalanceMicros != m("52") || n.LastPayout == nil || n.LastPayout.Document != 1 {
		t.Fatalf("narrow %+v", n)
	}
	// No pay terms in the answer: no rate, no revenue, no pages or pots, no «К выплате» of a tab, no estimate.
	raw, _ := json.Marshal(got)
	for _, k := range []string{`"rate`, `"revenue`, `"pages"`, `"pots"`, `"toPay`, `"share`, `"unentered`, `"fix`, `"kv"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("the answer has %s: %s", k, raw)
		}
	}
	// Nobody with a tab: the owner, a model's share, a departed card without a tab.
	for _, slug := range []string{"boss", "alpha", "charlie", "nobody"} {
		if _, err := ReadPersonPay(ctx, s.RO, slug, "2026-09-07", "2026-09-16", now); err == nil || !strings.Contains(err.Error(), ErrNoPayoutPerson.Error()) {
			t.Errorf("%s: %v", slug, err)
		}
	}
}

func TestPersonPayOpensOnlyToThePayoutsScope(t *testing.T) {
	s := personBook(t)
	ctx := t.Context()
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
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
	hub := key("hub", auth.ScopePayouts)
	const path = "/v1/payouts/person?person=alfa&from=2026-09-07&to=2026-09-16"
	var got PersonPayAnswer
	if err := hub.Do(ctx, http.MethodGet, path, nil, &got); err != nil {
		t.Fatal(err)
	}
	want, err := ReadPersonPay(ctx, s.RO, "alfa", "2026-09-07", "2026-09-16", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	got.GeneratedAt, want.GeneratedAt = "", ""
	got.Checks, want.Checks = PersonPayChecks{}, PersonPayChecks{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("over HTTP %+v\nwant %+v", got, want)
	}
	// Every other scope is refused, read included: only the payouts scope opens one person's pay.
	for _, sc := range []string{auth.ScopeRead, auth.ScopePropose, auth.ScopeDecide, auth.ScopeCollect, auth.ScopeImport, auth.ScopeReports} {
		if code := status(key("k-"+sc, sc).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusForbidden {
			t.Errorf("%s key: %d, want 403", sc, code)
		}
	}
	if code := status(key("both", auth.ScopeRead, auth.ScopePayouts).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusOK {
		t.Errorf("read+payouts key: %d", code)
	}
	nobody := client.New(config.Client{URL: srv.URL, Key: "ldg_nobody_x"})
	if code := status(nobody.Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("unknown key: %d", code)
	}
	if code := status(client.New(config.Client{URL: srv.URL}).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}
	// One person per request: the first person parameter, never a name, never someone without a tab.
	var twice PersonPayAnswer
	if err := hub.Do(ctx, http.MethodGet, "/v1/payouts/person?person=alfa&person=bravo&from=2026-09-07&to=2026-09-16", nil, &twice); err != nil ||
		twice.Person != "alfa" || twice.BalanceMicros != want.BalanceMicros || len(twice.Days) != len(want.Days) {
		t.Fatalf("two persons: %+v %v", twice, err)
	}
	for q, code := range map[string]int{
		"?from=2026-09-07&to=2026-09-16":                         http.StatusBadRequest,
		"?person=Альфа&from=2026-09-07&to=2026-09-16":            http.StatusBadRequest,
		"?person=alfa%27%20OR%201&from=2026-09-07&to=2026-09-16": http.StatusBadRequest,
		"?person=boss&from=2026-09-07&to=2026-09-16":             http.StatusNotFound,
		"?person=alpha&from=2026-09-07&to=2026-09-16":            http.StatusNotFound,
		"?person=charlie&from=2026-09-07&to=2026-09-16":          http.StatusNotFound,
		"?person=alfa&from=2026-09-07":                           http.StatusBadRequest,
		"?person=alfa&from=2026-09-16&to=2026-09-07":             http.StatusBadRequest,
		"?person=alfa&from=2026-09-01&to=2026-11-02":             http.StatusBadRequest, // 63 days
		"?person=bravo&from=2026-09-01&to=2026-11-01":            http.StatusOK,
	} {
		if got := status(hub.Do(ctx, http.MethodGet, "/v1/payouts/person"+q, nil, nil)); got != code {
			t.Errorf("%q: %d, want %d", q, got, code)
		}
	}
}
