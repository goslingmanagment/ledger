package accruals

import (
	"context"
	"encoding/json"
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
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Synthetic agency: chatters Альфа (alfa, Hub «Alfa» by the card's code) and Браво (bravo, Hub user 102 by hub:102), an unknown Hub
// user 103; model Alpha with pages page-a and page-b; the week of Monday 2026-11-09.

func at(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func open(t *testing.T) (*db.Store, context.Context) {
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
	for k, v := range map[string]string{"accrual_from": "2026-11-01", "hold_days": "5", "hold_types": "tip,subscription",
		"hub_model_slugs": `{"alpha-hub": "Alpha"}`} {
		exec(t, s, `INSERT INTO settings(key, value, updated_at) VALUES (?, ?, '2026-11-01')`, k, v)
	}
	for _, p := range []string{"page-a", "page-b"} {
		exec(t, s, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES (?, 'fansly', 'alpha-hub', 'Alpha', '2026-11-01')`, p)
	}
	if err := payroll.PutPeople(ctx, s.RW, []payroll.Person{
		{Slug: "alfa", Name: "Альфа", Aliases: []string{}, Status: "active", Models: []string{"Alpha"}},
		{Slug: "bravo", Name: "Браво", Aliases: []string{"hub:102"}, Status: "active", Models: []string{"Alpha"}},
	}, "test", at("2026-11-01")); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func exec(t *testing.T, s *db.Store, q string, args ...any) {
	t.Helper()
	if _, err := s.RW.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, s *db.Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.RO.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// ---- the sheets ----

func num(v any) json.Number { return json.Number(fmt.Sprint(v)) }

func serial(date string) int {
	d, _ := time.Parse(ts.DateLayout, date)
	return int(d.Sub(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)).Hours() / 24)
}

// tab is a payout tab of one week on one page: the day cells (nil — empty, 0 — a day worked without revenue), the
// share of the revenue and the fix per shift of the header, the fix of the week as the sheet keeps it.
func tab(start string, rate, perShift float64, days [7]any) [][]payroll.Cell {
	s := serial(start)
	hdr := []payroll.Cell{"Работяга", "Страница"}
	for k := range 7 {
		hdr = append(hdr, num(s+k))
	}
	hdr = append(hdr, "Итого за неделю", "З/П", "Выплачено", "Фикс")
	row := []payroll.Cell{"Кто-то", "Page A"}
	rev, worked := 0.0, 0
	for _, c := range days {
		if c == nil {
			row = append(row, "")
			continue
		}
		row = append(row, num(c))
		var f float64
		fmt.Sscan(fmt.Sprint(c), &f)
		rev += f
		worked++
	}
	fix := perShift * float64(worked)
	pay := rev*rate + fix
	row = append(row, num(rev), num(rev*rate), "", num(fix))
	sum := []payroll.Cell{"", "Сумма", num(0), num(0), num(0), num(0), num(0), num(0), num(0), num(0), num(pay)}
	head := []payroll.Cell{"К выплате:", "", "Заработано:", "", "Выплачено:", "", "", "", "", "", "", "", "Ставка %", "Фикс/см $"}
	vals := []payroll.Cell{num(pay), "", num(0), "", num(0), "", "", "", "", "", "", "", num(rate), num(perShift)}
	return [][]payroll.Cell{head, vals, {}, hdr, row, sum, {}}
}

// sheets reads the Alpha table with the tabs given, the way the collector does, into the database.
func sheets(t *testing.T, s *db.Store, ctx context.Context, tabs map[string][][]payroll.Cell) {
	t.Helper()
	names := [][]payroll.Cell{{"Чаттер"}}
	for n := range tabs {
		names = append(names, []payroll.Cell{n})
	}
	names = append(names, []payroll.Cell{"ИТОГО"})
	read := func(_ context.Context, _, rng string) ([][]payroll.Cell, error) {
		name := strings.Trim(rng[:strings.Index(rng, "!")], "'")
		switch name {
		case "Итого":
			return names, nil
		case "Неактивные":
			return [][]payroll.Cell{{"Чаттер"}, {"ИТОГО"}}, nil
		}
		return tabs[name], nil
	}
	cfg := payroll.Config{Account: "a@example.com", Tables: []payroll.Table{{Model: "Alpha", Sheet: "id-alpha"}}}
	got, err := payroll.Collect(ctx, cfg, read, at("2026-11-17T10:00:00+03:00"))
	if err != nil || len(got.Errors) > 0 {
		t.Fatalf("collect: %v %v", err, got.Errors)
	}
	ans, err := events.Accept(ctx, s, got.Events, at("2026-11-17T10:00:00+03:00"))
	if err != nil || len(ans.Settle) > 0 {
		t.Fatalf("accept: %v %v", err, ans.Settle)
	}
}

// ---- the facts ----

// username is the Hub login: 101 has the card's code in another case, the others a login no card names.
func username(user int64) string {
	if user == 101 {
		return "Alfa"
	}
	return fmt.Sprintf("user%d", user)
}

func work(user int64, kind, from, to string, pages ...string) hub.TeamWork {
	a, b := at(from), at(to)
	w := hub.TeamWork{Ref: fmt.Sprintf("%d:%s", user, a.UTC().Format("2006-01-02T15:04:05.000Z")), UserID: user,
		Username: username(user), Role: split.RoleChatter, Kind: kind, StartedAt: a, EndedAt: b,
		WorkedMinutes: int(b.Sub(a).Minutes()), Pages: []hub.TeamWorkPage{}}
	for _, p := range pages {
		from := a
		w.Pages = append(w.Pages, hub.TeamWorkPage{PageLabel: p, Messages: 10, SalesFrom: &from})
	}
	return w
}

func sale(ref, kind, page, when string, net int64, holders ...hub.TeamWork) hub.TeamSale {
	s := hub.TeamSale{TransactionRef: ref, PageLabel: page, Platform: "fansly", Kind: kind, NetMills: &net, OccurredAt: at(when),
		Holders: []string{}, Writers: []hub.TeamWriter{}, Marks: []hub.TeamMark{}}
	for _, h := range holders {
		s.Holders = append(s.Holders, h.Ref)
	}
	return s
}

// putWindow puts a proven week of facts into the mirror, as the collector leaves it.
func putWindow(t *testing.T, s *db.Store, from string, works []hub.TeamWork, sales []hub.TeamSale) {
	t.Helper()
	a := at(from)
	key := ts.Format(a)
	exec(t, s, `INSERT INTO hub_team_sync(window_from, window_to, status, works, sales, net_mills, work_from, work_to, read_at, ok_at)
		VALUES (?, ?, 'ok', ?, ?, 0, ?, ?, '2026-11-24T10:00:00+03:00', '2026-11-24T10:00:00+03:00')`,
		key, ts.Format(a.AddDate(0, 0, 7)), len(works), len(sales), ts.FormatMilli(a.AddDate(0, 0, -1)), ts.FormatMilli(a.AddDate(0, 0, 14)))
	for _, w := range works {
		b, _ := json.Marshal(w)
		exec(t, s, `INSERT INTO hub_works(window_from, ref, user_id, username, role, kind, started_at, ended_at, ongoing, worked_minutes,
			day, payload, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, '2026-11-24T10:00:00+03:00')`,
			key, w.Ref, w.UserID, w.Username, w.Role, w.Kind, ts.FormatMilli(w.StartedAt), ts.FormatMilli(w.EndedAt), w.WorkedMinutes,
			w.Day(), string(b))
	}
	for _, x := range sales {
		b, _ := json.Marshal(x)
		exec(t, s, `INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at, post_tip,
			campaign, payload, first_seen_at, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, '2026-11-24', '2026-11-24')`,
			x.TransactionRef, key, x.PageLabel, x.Platform, x.Kind, *x.NetMills, ts.FormatMilli(x.OccurredAt), x.PostTip, string(b))
	}
}

// week sets up the week of 9 November: Альфа's shift on the 10th and four sales it held — a tip of hers, a post
// tip, a tip held with user 103's shift that nobody wrote last on, — Браво's PPV bought on the 11th between her
// shifts, user 103's own tip; and the weeks around it, proven and empty.
func week(t *testing.T, s *db.Store) {
	t.Helper()
	alfa := work(101, split.Shift, "2026-11-10 10:00", "2026-11-10 18:00", "page-a")
	bravo := work(102, split.Shift, "2026-11-10 12:00", "2026-11-10 20:00", "page-b")
	bravoNext := work(102, split.Shift, "2026-11-12 10:00", "2026-11-12 18:00", "page-a")
	other := work(103, split.Shift, "2026-11-10 14:00", "2026-11-10 22:00", "page-a")
	post := sale("3", split.SaleTip, "page-a", "2026-11-10 15:00", 30_000, alfa, other)
	post.PostTip = true
	ppv := sale("2", split.SalePPV, "page-a", "2026-11-11 21:00", 40_000)
	ppv.PPVSender = &hub.TeamPerson{UserID: 102, Username: "user102", Role: split.RoleChatter}
	putWindow(t, s, "2026-11-02", nil, nil)
	putWindow(t, s, "2026-11-09", []hub.TeamWork{alfa, bravo, other, bravoNext}, []hub.TeamSale{
		sale("1", split.SaleTip, "page-a", "2026-11-10 11:00", 100_000, alfa),
		ppv,
		post,
		sale("4", split.SaleTip, "page-a", "2026-11-10 17:00", 20_000, alfa, other),
		sale("5", split.SaleTip, "page-a", "2026-11-10 20:00", 12_000, other),
	})
	putWindow(t, s, "2026-11-16", nil, nil)
}

func rebuild(t *testing.T, s *db.Store, ctx context.Context) Report {
	t.Helper()
	rep, err := Rebuild(ctx, s, at("2026-11-17T12:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestEachShareLandsOnItsPersonsDayAtTheirRate(t *testing.T) {
	s, ctx := open(t)
	week(t, s)
	sheets(t, s, ctx, map[string][][]payroll.Cell{
		"Альфа": tab("2026-11-09", 0.25, 0, [7]any{nil, 100, nil, nil, nil, nil, nil}),
		"Браво": tab("2026-11-09", 0.2, 10, [7]any{nil, 0, nil, 40, nil, nil, nil}),
	})
	rep := rebuild(t, s, ctx)
	if len(rep.Unknown) != 0 || rep.Sales != 5 {
		t.Fatalf("report %+v", rep)
	}
	// Альфа's tip on her day, at the 25% of her tab: as the sheet counts the day's revenue.
	if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE person = 'alfa' AND day = '2026-11-10' AND page_label = 'page-a'
		AND kind = 'sales' AND net_micros = 100000000 AND rate = 0.25 AND amount_micros = 25000000`); n != 1 {
		t.Fatal("Альфа's accrual of the 10th")
	}
	// Браво's PPV, bought on the 11th, lands on her next shift's day, the 12th, at her 20%; her fix per shift is paid
	// for both days with a shift, the one without sales too.
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '2' AND person = 'bravo' AND day = '2026-11-12'
		AND reason = ?`, split.ReasonPPVSenderNextShift); n != 1 {
		t.Fatal("the PPV did not land on its sender's day")
	}
	if n := count(t, s, `SELECT SUM(amount_micros) FROM fact_accruals WHERE person = 'bravo' AND week = '2026-11-09'`); n != 28_000_000 {
		t.Fatalf("Браво's week %d, want 8 + 2 × 10", n)
	}
	// The post tip is the общак, no person's; the tip two shifts held with nobody writing last waits for a decision.
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '3' AND home = 'pot' AND pot = 'obshak'
		AND person IS NULL AND hub_user_id IS NULL`); n != 1 {
		t.Fatal("post tip not in the pot")
	}
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '4' AND home = 'decide' AND share_micros = 20000000`); n != 1 {
		t.Fatal("the disputed tip")
	}
	if n := count(t, s, `SELECT COALESCE(SUM(net_micros), 0) FROM fact_accruals WHERE person = 'alfa'`); n != 100_000_000 {
		t.Fatalf("the pot or the decision was accrued to Альфа: %d", n)
	}
	// User 103 has no card: their tip is kept and named, not dropped and not priced.
	if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE hub_user_id = 103 AND person IS NULL AND net_micros = 12000000
		AND amount_micros IS NULL AND reason LIKE 'не сопоставлен%hub:103'`); n != 1 {
		t.Fatal("the unmapped user's tip")
	}
	if len(rep.Unmapped) != 1 || rep.Unmapped[0] != "user103 (hub 103)" {
		t.Fatalf("unmapped %v", rep.Unmapped)
	}
	// Nothing is posted: the facts change no debt.
	if n := count(t, s, `SELECT count(*) FROM entries WHERE kind NOT IN ('accrual:payroll')`); n != 0 {
		t.Fatalf("%d entries other than the sheets'", n)
	}
	before := count(t, s, `SELECT count(*) FROM entries`)
	if again := rebuild(t, s, ctx); again.Rewritten != 0 || count(t, s, `SELECT count(*) FROM entries`) != before ||
		count(t, s, `SELECT count(*) FROM fact_homes`) != 5 {
		t.Fatalf("a rebuild over the same facts rewrote %d weeks or posted", again.Rewritten)
	}
	// Браво's next shift was not there yet: the PPV waited on the day of its sale; once it is, the row moves weeks.
	exec(t, s, `UPDATE hub_works SET started_at = '2026-11-16T10:00:00.000+03:00' WHERE user_id = 102 AND kind = 'shift'
		AND started_at LIKE '2026-11-12%'`)
	exec(t, s, `UPDATE hub_works SET payload = json_set(payload, '$.startedAt', '2026-11-16T07:00:00Z', '$.endedAt', '2026-11-16T15:00:00Z')
		WHERE user_id = 102 AND started_at LIKE '2026-11-16%'`)
	if moved := rebuild(t, s, ctx); moved.Rewritten != 2 {
		t.Fatalf("the PPV moved to the next week: %d weeks rewritten", moved.Rewritten)
	}
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '2' AND person = 'bravo' AND week = '2026-11-16'`); n != 1 {
		t.Fatal("the PPV did not follow its sender's shift")
	}
}

func TestTheDiffShowsTheSheetBesideTheFactsAndTheSalesThatMakeIt(t *testing.T) {
	s, ctx := open(t)
	week(t, s)
	// Альфа's tab counts the post tip and the disputed tip as hers: 150 on the 10th.
	sheets(t, s, ctx, map[string][][]payroll.Cell{
		"Альфа": tab("2026-11-09", 0.25, 0, [7]any{nil, 150, nil, nil, nil, nil, nil}),
		"Браво": tab("2026-11-09", 0.2, 10, [7]any{nil, 0, nil, 40, nil, nil, nil}),
	})
	rebuild(t, s, ctx)
	d, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if !d.OK || d.Facts.Status != "ok" || len(d.People) != 2 {
		t.Fatalf("diff %+v", d)
	}
	alfa, bravo := d.People[0], d.People[1]
	if alfa.Person != "alfa" || alfa.SheetMicros != 37_500_000 || *alfa.FactsMicros != 25_000_000 || *alfa.DiffMicros != -12_500_000 {
		t.Fatalf("Альфа %+v", alfa)
	}
	if bravo.SheetMicros != 28_000_000 || *bravo.DiffMicros != 0 || len(bravo.Sales) != 0 {
		t.Fatalf("Браво %+v", bravo)
	}
	if len(alfa.Days) != 1 || alfa.Days[0].SheetRevenue != 150 || alfa.Days[0].FactsNetMicros != 100_000_000 {
		t.Fatalf("Альфа's days %+v", alfa.Days)
	}
	var got []string
	for _, l := range alfa.Sales {
		amount := "-"
		if l.AmountMicros != nil {
			amount = fmt.Sprint(*l.AmountMicros)
		}
		got = append(got, fmt.Sprintf("%s %s %d %s %s", l.Sign, l.Ref, l.ShareMicros, amount, l.To))
	}
	want := []string{"+ 1 100000000 25000000 ", "− 3 30000000 - общак", "− 4 20000000 - ждёт решения"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sales of the difference:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if d.Pot.Micros != 30_000_000 || len(d.Pot.Sales) != 1 || len(d.Decide.Sales) != 1 || d.Decide.Sales[0].Reason == "" {
		t.Fatalf("pot %+v decide %+v", d.Pot, d.Decide)
	}
	if len(d.Unmapped) != 1 || d.Unmapped[0].UserID != 103 || d.Unmapped[0].NetMicros != 12_000_000 {
		t.Fatalf("unmapped %+v", d.Unmapped)
	}

	// The week after is not proven: its facts are unknown, not zero.
	exec(t, s, `UPDATE hub_team_sync SET status = 'unknown', reasons = '["хаб: 403"]' WHERE window_from = '2026-11-16T00:00:00+03:00'`)
	rebuild(t, s, ctx)
	d, err = WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if d.OK || d.Facts.Status != "unknown" || !strings.Contains(strings.Join(d.Facts.Reasons, " "), "2026-11-16: хаб: 403") ||
		d.People[0].FactsMicros != nil || d.People[0].SheetMicros != 37_500_000 {
		t.Fatalf("unknown week %+v %+v", d.Facts, d.People[0])
	}
}

func TestTheDiffRouteNeedsTheReadScope(t *testing.T) {
	s, ctx := open(t)
	week(t, s)
	rebuild(t, s, ctx)
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	token := func(name, scope string) string {
		tok, _, err := auth.Create(ctx, s.RW, name, []string{scope}, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	reader := client.New(config.Client{URL: srv.URL, Key: token("agent", auth.ScopeRead)})
	var d Diff
	if err := reader.Do(ctx, http.MethodGet, "/v1/accruals/diff?week=2026-11-09", nil, &d); err != nil || d.Pot.Micros != 30_000_000 {
		t.Fatalf("diff %+v %v", d.Pot, err)
	}
	var e *client.Error
	if err := reader.Do(ctx, http.MethodGet, "/v1/accruals/diff?week=2026-11-10", nil, &d); !asStatus(err, &e, 400) {
		t.Fatalf("a Tuesday: %v", err)
	}
	collector := client.New(config.Client{URL: srv.URL, Key: token("mac", auth.ScopeCollect)})
	if err := collector.Do(ctx, http.MethodGet, "/v1/accruals/diff?week=2026-11-09", nil, &d); !asStatus(err, &e, 403) {
		t.Fatalf("a key without read: %v", err)
	}
}

func asStatus(err error, e **client.Error, status int) bool {
	ce, ok := err.(*client.Error)
	if ok {
		*e = ce
	}
	return ok && ce.Status == status
}
