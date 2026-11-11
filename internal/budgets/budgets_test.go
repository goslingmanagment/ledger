package budgets_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/budgets"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, ts.MSK)

// book is a store with a synthetic journal of October and November: every kind of row, two traffic channels (one of
// them a contractor), a transfer and a cancelled row that are no part of a budget.
func book(t *testing.T) (*db.Store, context.Context) {
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
	for k, v := range map[string]string{
		"full_ledger_from":             "2026-10-15T00:00:00+03:00",
		"category_caps":                `{"AI и разработка": 300}`,
		"owner_salary_rate":            `{"2026-10-01": 1000, "2026-11-01": 1200}`,
		budgets.SettingTrafficChannels: `{"Вендор SFS": "SFS", "Студия": "Сайт"}`,
	} {
		if _, err := s.RW.ExecContext(ctx, `INSERT OR REPLACE INTO settings(key, value, updated_at) VALUES (?, ?, ?)`, k, v, ts.Format(now)); err != nil {
			t.Fatal(err)
		}
	}
	docs := []ledger.Document{
		{Num: 1, Date: "2026-10-03", Type: "income", Category: "Платформы", Counterparty: "Модель А", Model: "Alpha", Amount: "1000.00"},
		{Num: 2, Date: "2026-10-05", Type: "expense", Category: "Чаттер зарплата", Counterparty: "Чаттер", Model: "Alpha", Amount: "250.50"},
		{Num: 3, Date: "2026-10-20", Type: "expense", Category: "Трафик", Counterparty: "Студия", Model: "Alpha", Amount: "100.00"},
		{Num: 4, Date: "2026-10-21", Type: "expense", Category: "Подрядчик", Counterparty: "Вендор SFS", Model: "Alpha", Amount: "30.00"},
		{Num: 5, Date: "2026-10-22", Type: "expense", Category: "Трафик", Counterparty: "Площадка", Model: "Beta", Amount: "40.00"},
		{Num: 6, Date: "2026-10-25", Type: "expense", Category: "Owner draw", Counterparty: "Владелец", Model: "—", Amount: "500.00"},
		// The journal once wrote an injection as an expense: it is the owner's money in all the same.
		{Num: 7, Date: "2026-10-26", Type: "expense", Category: "Owner вливание", Counterparty: "Владелец", Model: "—", Amount: "300.00"},
		{Num: 8, Date: "2026-10-27", Type: "income", Category: "Возврат расхода", Counterparty: "Сервис", Model: "—", Amount: "12.34"},
		{Num: 9, Date: "2026-10-28", Type: "transfer", Category: "Внутренний перевод", Counterparty: "—", Model: "—", Amount: "70.00"},
		{Num: 10, Date: "2026-10-29", Type: "expense", Category: "Прочее", Counterparty: "Магазин", Model: "—", Amount: "9.00", Status: "cancelled"},
		{Num: 11, Date: "2026-11-02", Type: "expense", Category: "AI и разработка", Counterparty: "Сервис", Model: "—", Amount: "80.00"},
		{Num: 12, Date: "2026-11-03", Type: "income", Category: "Custom", Counterparty: "Фан", Model: "Alpha", Amount: "45.00"},
		// The network fee of a traffic payment is a fee, not traffic.
		{Num: 13, Date: "2026-11-04", Type: "expense", Category: "Комиссии", Counterparty: "Студия", Model: "—", Amount: "1.50"},
		// An advance of the CEO's salary for November paid in October, in part; a fee for October paid in November.
		{Num: 14, Date: "2026-10-30", Type: "expense", Category: "Owner зарплата", Counterparty: "Владелец", Model: "—", Amount: "500.00",
			Tags: []byte(`{"за": [{"месяц": "2026-11", "сумма": 200}]}`)},
		{Num: 15, Date: "2026-11-05", Type: "expense", Category: "Комиссии", Counterparty: "Биржа", Model: "—", Amount: "4.00",
			Tags: []byte(`{"за": [{"месяц": "2026-10", "сумма": 3}]}`)},
	}
	for _, d := range docs {
		if d.Status == "" {
			d.Status = "paid"
		}
		d.Currency, d.Method, d.Author, d.Origin = "USD", "USDT TRC-20", "test", "test"
		if _, _, err := ledger.PutDocument(ctx, s.RW, d, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO month_plans(month, page_label, per_day, created_at, source) VALUES
		('2026-11', 'page-a', 120.5, ?, 'test'), ('2026-11', 'page-b', 30, ?, 'test')`, ts.Format(now), ts.Format(now)); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestReadKindsChannelsPlans(t *testing.T) {
	s, ctx := book(t)
	if _, err := budgets.SetPlans(ctx, s.RW, budgets.SetInput{Source: "test", Plans: []budgets.PlanInput{
		{Month: "2026-10", Item: "Трафик", Amount: "150"},
		{Month: "2026-10", Item: "AI и разработка", Amount: "250.25", Note: "меньше лимита"},
	}}, "owner", now); err != nil {
		t.Fatal(err)
	}
	ans, err := budgets.Read(ctx, s.RO, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if ans.From != "2026-10" || ans.To != "2026-11" || len(ans.Months) != 2 || ans.FullLedgerFrom != "2026-10-15" {
		t.Fatalf("window: %s..%s, %d months, full from %q", ans.From, ans.To, len(ans.Months), ans.FullLedgerFrom)
	}
	oct, nov := ans.Months[0], ans.Months[1]
	if oct.Days != 31 || nov.Days != 30 || oct.Complete || !nov.Complete {
		t.Errorf("days %d/%d, complete %v/%v", oct.Days, nov.Days, oct.Complete, nov.Complete)
	}
	got := map[int]budgets.Row{}
	for _, r := range oct.Rows {
		got[r.Num] = r
	}
	if len(oct.Rows) != 9 {
		t.Errorf("October rows: %d, want 9 (no transfer, no cancelled row)", len(oct.Rows))
	}
	if got[14].AmountMicros != 300_000_000 || got[14].AdvanceFrom != "" {
		t.Errorf("#14 in October: %+v", got[14])
	}
	novRows := map[int]budgets.Row{}
	for _, r := range nov.Rows {
		novRows[r.Num] = r
	}
	if r := novRows[14]; r.AmountMicros != 200_000_000 || r.AdvanceFrom != "2026-10" || r.Date != "2026-10-30" {
		t.Errorf("#14's advance in November: %+v", r)
	}
	if r := novRows[15]; r.AmountMicros != 4_000_000 || r.AdvanceFrom != "" {
		t.Errorf("#15, a fee for an earlier month, stays where paid: %+v", r)
	}
	for num, want := range map[int][2]string{
		1: {budgets.KindIncome, ""}, 2: {budgets.KindExpense, ""}, 3: {budgets.KindExpense, "Сайт"},
		4: {budgets.KindExpense, "SFS"}, 5: {budgets.KindExpense, "Площадка"}, 6: {budgets.KindOwnerOut, ""},
		7: {budgets.KindOwnerIn, ""}, 8: {budgets.KindRefund, ""},
	} {
		if r := got[num]; r.Kind != want[0] || r.Channel != want[1] {
			t.Errorf("#%d: kind %q channel %q, want %q %q", num, r.Kind, r.Channel, want[0], want[1])
		}
	}
	if got[2].AmountMicros != 250_500_000 || got[6].Model != "" {
		t.Errorf("#2 %d micros, #6 model %q", got[2].AmountMicros, got[6].Model)
	}
	// A cap is no plan; the CEO's salary without a plan of its own is the rate of the month's first day, in a month of
	// full accounting only (October here is before it).
	plans := func(ps []budgets.Plan) string {
		var out []string
		for _, p := range ps {
			out = append(out, fmt.Sprintf("%s %d %s", p.Item, p.PlanMicros/1_000_000, p.Source))
		}
		return strings.Join(out, "; ")
	}
	if got, want := plans(oct.Plans), "AI и разработка 250 plan; Трафик 150 plan"; got != want {
		t.Errorf("October plans: %s, want %s", got, want)
	}
	if oct.Plans[0].SetBy != "owner" || oct.Plans[0].Note != "меньше лимита" {
		t.Errorf("October AI plan: %+v", oct.Plans[0])
	}
	if got, want := plans(nov.Plans), "Owner зарплата 1200 rate"; got != want {
		t.Errorf("November plans: %s, want %s", got, want)
	}
	for _, r := range nov.Rows {
		if r.Num == 13 && (r.Kind != budgets.KindExpense || r.Channel != "") {
			t.Errorf("a traffic payment's fee: %+v", r)
		}
	}
	if len(nov.PagePlans) != 2 || nov.PagePlans[0].Page != "page-a" || nov.PagePlans[0].PerDayMicros != 120_500_000 {
		t.Errorf("November page plans: %+v", nov.PagePlans)
	}
	if oct.Result != nil || oct.Closed {
		t.Errorf("no card, no period: result %+v closed %v", oct.Result, oct.Closed)
	}
}

func TestSetPlansAppendOnly(t *testing.T) {
	s, ctx := book(t)
	set := func(amount, note string) budgets.SetAnswer {
		t.Helper()
		ans, err := budgets.SetPlans(ctx, s.RW, budgets.SetInput{Plans: []budgets.PlanInput{{Month: "2026-11", Item: "Трафик", Amount: amount, Note: note}}}, "owner", now)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	if a := set("200", ""); a.Written != 1 {
		t.Errorf("first: %+v", a)
	}
	if a := set("200.00", ""); a.Written != 0 || a.Unchanged != 1 {
		t.Errorf("same again: %+v", a)
	}
	if a := set("250", "больше"); a.Written != 1 {
		t.Errorf("changed: %+v", a)
	}
	if a := set("-", ""); a.Written != 1 {
		t.Errorf("removed: %+v", a)
	}
	var rows, live int
	_ = s.RO.QueryRowContext(ctx, `SELECT count(*) FROM budget_plans`).Scan(&rows)
	_ = s.RO.QueryRowContext(ctx, `SELECT count(*) FROM v_budget_plans`).Scan(&live)
	if rows != 3 || live != 0 {
		t.Errorf("rows %d live %d, want 3 and 0", rows, live)
	}
	for _, bad := range []budgets.PlanInput{
		{Month: "2026-13", Item: "Трафик", Amount: "1"},
		{Month: "2026-11", Item: " ", Amount: "1"},
		{Month: "2026-11", Item: "Трафик", Amount: "-5"},
		{Month: "2026-11", Item: "Трафик", Amount: "abc"},
	} {
		if _, err := budgets.SetPlans(ctx, s.RW, budgets.SetInput{Plans: []budgets.PlanInput{bad}}, "owner", now); !budgets.IsFail(err) {
			t.Errorf("%+v: %v, want a refusal", bad, err)
		}
	}
	if _, err := s.RW.ExecContext(ctx, `UPDATE budget_plans SET plan_micros = 1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("update allowed: %v", err)
	}
}

func TestWindow(t *testing.T) {
	s, ctx := book(t)
	for _, c := range []struct{ from, to string }{{"2026-11", "2026-10"}, {"2026-1", ""}, {"2023-01", "2026-11"}} {
		if _, err := budgets.Read(ctx, s.RO, c.from, c.to, now); !budgets.IsFail(err) {
			t.Errorf("%s..%s: %v, want a refusal", c.from, c.to, err)
		}
	}
	ans, err := budgets.Read(ctx, s.RO, "2026-09", "2026-09", now)
	if err != nil || len(ans.Months) != 1 || len(ans.Months[0].Rows) != 0 {
		t.Errorf("an empty month: %+v %v", ans.Months, err)
	}
	// November alone still carries the advance paid in October.
	ans, err = budgets.Read(ctx, s.RO, "2026-11", "2026-11", now)
	var advance int64
	for _, r := range ans.Months[0].Rows {
		if r.Num == 14 {
			advance += r.AmountMicros
		}
	}
	if err != nil || advance != 200_000_000 {
		t.Errorf("November alone: advance %d, %v", advance, err)
	}
}

func TestScopes(t *testing.T) {
	s, ctx := book(t)
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, now)
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	hub, payouts, reader := key("hub", auth.ScopeBudgets), key("payouts", auth.ScopePayouts), key("reader", auth.ScopeRead)
	status := func(err error) int {
		if err == nil {
			return http.StatusOK
		}
		if e, ok := err.(*client.Error); ok {
			return e.Status
		}
		t.Fatal(err)
		return 0
	}
	var ans budgets.Answer
	plan := budgets.SetInput{Source: "console", Plans: []budgets.PlanInput{{Month: "2026-11", Item: "Трафик", Amount: "10"}}}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"budgets reads", status(hub.Do(ctx, http.MethodGet, "/v1/budgets?from=2026-10&to=2026-11", nil, &ans)), http.StatusOK},
		{"read reads", status(reader.Do(ctx, http.MethodGet, "/v1/budgets", nil, &ans)), http.StatusOK},
		{"payouts does not read", status(payouts.Do(ctx, http.MethodGet, "/v1/budgets", nil, &ans)), http.StatusForbidden},
		{"budgets writes a plan", status(hub.Do(ctx, http.MethodPost, "/v1/budgets/plans", plan, nil)), http.StatusOK},
		{"read writes no plan", status(reader.Do(ctx, http.MethodPost, "/v1/budgets/plans", plan, nil)), http.StatusForbidden},
		{"a bad window", status(hub.Do(ctx, http.MethodGet, "/v1/budgets?from=2026-11&to=2026-10", nil, &ans)), http.StatusBadRequest},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d", c.name, c.got, c.want)
		}
	}
	var by string
	_ = s.RO.QueryRowContext(ctx, `SELECT author || '/' || source FROM budget_plans`).Scan(&by)
	if by != "hub/console" {
		t.Errorf("plan written by %q", by)
	}
}
