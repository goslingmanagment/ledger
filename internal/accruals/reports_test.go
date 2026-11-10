package accruals

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The accruals from the shift reports (R2), on the synthetic agency of accruals_test.go: Альфа (alfa, Telegram 501) and
// Браво (bravo, Telegram 502) post in one «Отчётность» topic; model Alpha has page-a and page-b, model Beta page-c;
// page-z is no Hub page.

const reportPeer = "-1001000000009"

var reportNow = at("2026-11-17T12:00:00+03:00")

func reportBook(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := open(t)
	exec(t, s, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-c', 'onlyfans', 'beta-hub', 'Beta', '2026-11-01')`)
	exec(t, s, `UPDATE settings SET value = '{"alpha-hub": "Alpha", "beta-hub": "Beta"}' WHERE key = 'hub_model_slugs'`)
	for k, v := range map[string]string{
		"report_topics":  `[{"peer": "` + reportPeer + `", "topic": 4, "pages": {"a": "page-a", "b": "page-b", "c": "page-c", "z": "page-z"}}]`,
		"report_authors": `{"tg:501": "alfa", "tg:502": "bravo"}`,
	} {
		exec(t, s, `INSERT INTO settings(key, value, updated_at) VALUES (?, ?, '2026-11-01')`, k, v)
	}
	if err := payroll.PutPeople(ctx, s.RW, []payroll.Person{
		{Slug: "alfa", Name: "Альфа", Aliases: []string{}, Status: "active", Models: []string{"Alpha", "Beta"}},
		{Slug: "bravo", Name: "Браво", Aliases: []string{"hub:102"}, Status: "active", Models: []string{"Alpha"}},
	}, "test", at("2026-11-01")); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

// tables reads the payout tables of several models at once, the way the collector does.
func tables(t *testing.T, s *db.Store, ctx context.Context, byModel map[string]map[string][][]payroll.Cell) {
	t.Helper()
	cfg := payroll.Config{Account: "a@example.com"}
	for model := range byModel {
		cfg.Tables = append(cfg.Tables, payroll.Table{Model: model, Sheet: "id-" + model})
	}
	read := func(_ context.Context, sheet, rng string) ([][]payroll.Cell, error) {
		tabs := byModel[strings.TrimPrefix(sheet, "id-")]
		name := strings.Trim(rng[:strings.Index(rng, "!")], "'")
		switch name {
		case "Итого":
			names := [][]payroll.Cell{{"Чаттер"}}
			for n := range tabs {
				names = append(names, []payroll.Cell{n})
			}
			return append(names, []payroll.Cell{"ИТОГО"}), nil
		case "Неактивные":
			return [][]payroll.Cell{{"Чаттер"}, {"ИТОГО"}}, nil
		}
		return tabs[name], nil
	}
	got, err := payroll.Collect(ctx, cfg, read, at("2026-11-17T10:00:00+03:00"))
	if err != nil || len(got.Errors) > 0 {
		t.Fatalf("collect: %v %v", err, got.Errors)
	}
	ans, err := events.Accept(ctx, s, got.Events, reportNow)
	if err != nil || len(ans.Settle) > 0 {
		t.Fatalf("accept: %v %v", err, ans.Settle)
	}
}

// pageTab is tab with the page row named.
func pageTab(start, page string, rate, perShift float64, days [7]any) [][]payroll.Cell {
	cells := tab(start, rate, perShift, days)
	cells[4][1] = page
	return cells
}

// postText is a report as the chatters post it; total "" — the sum of the lines.
func postText(date, total string, lines ...string) string {
	if total == "" {
		var sum int64
		for _, l := range lines {
			var v int64
			fmt.Sscanf(l[strings.Index(l, "$")+1:], "%d", &v)
			sum += v
		}
		total = fmt.Sprintf("$%d", sum)
	}
	body := make([]string, len(lines))
	for i, l := range lines {
		body[i] = "• " + l
	}
	return "🗓 Дата: " + date[8:10] + "." + date[5:7] + "." + date[:4] + "\n👤 Чаттер: #x\n\n📌 Аккаунты:\n" + strings.Join(body, "\n") +
		"\n\n💰Общий доход: " + total + "\n\n📝 Комментарий:"
}

// post sends a message of the topic as the collector would; the same msgID with another text is an edit.
func post(t *testing.T, s *db.Store, msgID int64, sender, date, text string) {
	t.Helper()
	e := reports.PostEvent{Peer: reportPeer, Topic: 4, MsgID: msgID, Date: date + "T23:30:00+03:00", Sender: "tg user", SenderID: sender, Text: text}
	payload, _ := json.Marshal(e)
	ans, err := events.Accept(context.Background(), s, []events.Event{{Source: reports.PostSource,
		SourceID: reports.PostSourceID(reportPeer, 4, msgID, text), Kind: "post", OccurredAt: e.Date, Payload: payload}}, reportNow)
	if err != nil || len(ans.Settle) > 0 {
		t.Fatalf("accept: %v %v", err, ans.Settle)
	}
}

// cols renders a query's rows, one per line, the values parted by a space.
func cols(t *testing.T, s *db.Store, q string, args ...any) string {
	t.Helper()
	r, err := s.RO.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer r.Close()
	names, _ := r.Columns()
	var out []string
	for r.Next() {
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return strings.Join(out, "\n")
}

func reportRows(t *testing.T, s *db.Store) string {
	return cols(t, s, `SELECT person, model, day, net_micros, COALESCE(amount_micros, -1), COALESCE(tab_name, '-'), COALESCE(reason, '')
		FROM report_accruals ORDER BY person, day, model`)
}

func TestTheReportsAccrueEachTabDayInShadow(t *testing.T) {
	s, ctx := reportBook(t)
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.25, 0, [7]any{80, nil, nil, nil, nil, nil, nil}),
			"Браво": pageTab("2026-11-09", "Page A", 0.2, 10, [7]any{nil, 50, nil, nil, nil, nil, nil})},
		"Beta": {"Альфа": pageTab("2026-11-09", "Page C", 0.3, 0, [7]any{nil, nil, 60, nil, nil, nil, nil})},
	})
	entries := count(t, s, `SELECT count(*) FROM entries`)
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $100", "B - $40"))       // two pages of one model
	post(t, s, 2, "502", "2026-11-10", postText("2026-11-10", "", "A - $50", "B - $25"))        // the fix once for the day
	post(t, s, 3, "501", "2026-11-11", postText("2026-11-11", "", "A - $100", "C - $60"))       // two models: two tab days
	post(t, s, 4, "502", "2026-11-11", postText("2026-11-11", "", "C - $30"))                   // no tab of Beta
	post(t, s, 5, "501", "2026-11-12", postText("2026-11-12", "", "Z - $10", "B - не в смене")) // a page of no model
	want := strings.Join([]string{
		"alfa Alpha 2026-11-10 140000000 35000000 Альфа ",
		"alfa Alpha 2026-11-11 100000000 25000000 Альфа ",
		"alfa Beta 2026-11-11 60000000 18000000 Альфа ",
		"alfa  2026-11-12 10000000 -1 - модель страницы неизвестна (page-z): нет в hub_pages — ставки нет",
		"bravo Alpha 2026-11-10 75000000 25000000 Браво ",
		"bravo Beta 2026-11-11 30000000 -1 - нет вкладки человека в таблице выплат модели «Beta»: ставки нет",
	}, "\n")
	if got := reportRows(t, s); got != want {
		t.Fatalf("report_accruals:\n%s\nwant\n%s", got, want)
	}
	if n := count(t, s, `SELECT count(*) FROM report_accruals WHERE person = 'bravo' AND day = '2026-11-10' AND fix_micros = 10000000
		AND rate = 0.2`); n != 1 {
		t.Fatal("Браво's terms on the day")
	}
	// A shadow: not a posting more, not a balance moved.
	if n := count(t, s, `SELECT count(*) FROM entries`); n != entries {
		t.Fatalf("the reports posted: %d entries, were %d", n, entries)
	}
	// Idempotent: the same pay base writes nothing.
	rep, err := RebuildReports(ctx, s, reportNow)
	if err != nil || rep.Written != 0 || rep.Removed != 0 || rep.Days != 6 || rep.Unpriced != 2 {
		t.Fatalf("again: %+v %v", rep, err)
	}
	// An edit replaces the day, the rest stays.
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $120", "B - $40"))
	if n := count(t, s, `SELECT amount_micros FROM report_accruals WHERE person = 'alfa' AND day = '2026-11-10'`); n != 40_000_000 {
		t.Fatalf("the edited day: %d", n)
	}
	// A post blocked by a flag is no pay base: its day goes.
	post(t, s, 2, "502", "2026-11-10", postText("2026-11-10", "$99", "A - $50", "B - $25"))
	if n := count(t, s, `SELECT count(*) FROM report_accruals WHERE person = 'bravo' AND day = '2026-11-10'`); n != 0 {
		t.Fatal("a blocked post still accrues")
	}
}

func TestTheWeekPutsTheReportsBesideTheSheetAndTheFacts(t *testing.T) {
	s, ctx := reportBook(t)
	week(t, s) // the facts: Альфа's tip of 100 on the 10th at her shift
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.25, 0, [7]any{80, 100, nil, nil, nil, nil, nil}),
			"Браво": pageTab("2026-11-09", "Page A", 0.2, 10, [7]any{nil, 50, nil, nil, nil, nil, nil})},
	})
	rebuild(t, s, ctx)
	post(t, s, 1, "501", "2026-11-09", postText("2026-11-09", "$80,40", "A - $80,40")) // the sheet holds 80: its rounding
	post(t, s, 2, "501", "2026-11-10", postText("2026-11-10", "", "A - $100", "B - $40"))
	post(t, s, 3, "501", "2026-11-13", postText("2026-11-13", "", "A - $20"))
	d, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	var alfa, bravo *PersonDiff
	for i := range d.People {
		switch d.People[i].Person {
		case "alfa":
			alfa = &d.People[i]
		case "bravo":
			bravo = &d.People[i]
		}
	}
	if alfa == nil || bravo == nil {
		t.Fatalf("people %+v", d.People)
	}
	// The existing columns stand: the sheet and the facts.
	if alfa.SheetMicros != 45_000_000 || alfa.FactsMicros == nil || *alfa.FactsMicros != 25_000_000 {
		t.Fatalf("Альфа's sheet and facts: %d %v", alfa.SheetMicros, alfa.FactsMicros)
	}
	// The reports: 80.40 × 25% = 20.10, 140 × 25% = 35, 20 × 25% = 5.
	if alfa.ReportsMicros != 60_100_000 || alfa.ReportsNetMicros != 240_400_000 {
		t.Fatalf("Альфа's reports %d net %d", alfa.ReportsMicros, alfa.ReportsNetMicros)
	}
	// Reports − sheet is 15.10, of which the 0.10 of the 9th is the sheet's rounding and is not counted.
	if alfa.SheetReportsDiffMicros != 15_100_000 || alfa.SheetReportsCountedMicros != 15_000_000 {
		t.Fatalf("Альфа's reports − sheet %d, counted %d", alfa.SheetReportsDiffMicros, alfa.SheetReportsCountedMicros)
	}
	if alfa.ReportsFactsDiffMicros == nil || *alfa.ReportsFactsDiffMicros != 25_000_000-60_100_000 {
		t.Fatalf("Альфа's facts − reports %v", alfa.ReportsFactsDiffMicros)
	}
	days := map[string]DayDiff{}
	for _, x := range alfa.Days {
		days[x.Day] = x
	}
	nine := days["2026-11-09"]
	if len(nine.Pages) != 1 || !nine.Pages[0].Rounding || *nine.Pages[0].SheetMicros != 80_000_000 || *nine.Pages[0].ReportMicros != 80_400_000 ||
		nine.RoundingMicros != 100_000 || nine.Presence != "" {
		t.Fatalf("the 9th %+v", nine)
	}
	ten := days["2026-11-10"]
	if ten.SheetReportsDiffMicros != 10_000_000 || len(ten.Pages) != 2 || ten.Pages[1].Page != "page-b" || ten.Pages[1].SheetMicros != nil ||
		ten.Pages[1].Rounding || ten.ReportsFactsDiffMicros == nil || *ten.ReportsFactsDiffMicros != -10_000_000 {
		t.Fatalf("the 10th %+v", ten)
	}
	if days["2026-11-13"].Presence != PresenceReportOnly {
		t.Fatalf("the 13th %+v", days["2026-11-13"])
	}
	// Браво has the sheet's 10th and no report.
	if len(bravo.Days) == 0 || bravo.Days[0].Presence != PresenceSheetOnly || bravo.SheetReportsCountedMicros != -20_000_000 {
		t.Fatalf("Браво %+v", bravo)
	}
	// The JSON keeps the fields that were read before.
	b, _ := json.Marshal(d)
	for _, f := range []string{`"sheetMicros"`, `"factsMicros"`, `"differenceMicros"`, `"reportsMicros"`, `"sheetReportsCountedMicros"`,
		`"reportsFactsDifferenceMicros"`, `"presence"`} {
		if !strings.Contains(string(b), f) {
			t.Errorf("no %s in the diff", f)
		}
	}
}

// reportWeek is the week of 9 November with the sheets and the posts: Альфа's 80 on the 9th and 100 on the 10th (her
// post says 100 + 40), Браво's 50 on the 10th (her post says the same) and 50 on the 12th (no post).
func reportWeek(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := reportBook(t)
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.25, 0, [7]any{80, 100, nil, nil, nil, nil, nil}),
			"Браво": pageTab("2026-11-09", "Page A", 0.2, 10, [7]any{nil, 50, nil, 50, nil, nil, nil})},
	})
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $100", "B - $40"))
	post(t, s, 2, "502", "2026-11-10", postText("2026-11-10", "", "A - $50"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func setReports(t *testing.T, s *db.Store, ctx context.Context, day string) {
	t.Helper()
	if err := payroll.SetReportsFrom(ctx, s.RW, day, reportNow); err != nil {
		t.Fatal(err)
	}
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
}

func dayOf(t *testing.T, s *db.Store, person, date string) string {
	return cols(t, s, `SELECT amount, paid, unpaid, source FROM payable_days WHERE person = ? AND date = ? AND kind = 'day'`, person, date)
}

func TestTheReportsSwitchAccruesFromThePostsAndBack(t *testing.T) {
	s, ctx := reportWeek(t)
	before := books(t, s)
	beforeNinth := rows(t, s, `SELECT id, source_key FROM entries WHERE kind = 'accrual:payroll' AND effective_at < ? ORDER BY id`, "2026-11-10")
	if balance(t, s, "alfa") != 45_000_000 || balance(t, s, "bravo") != 40_000_000 {
		t.Fatalf("the sheets' debts:\n%s", before)
	}

	setReports(t, s, ctx, "2026-11-10")
	// The 9th is the sheet's, the same entry.
	if got := rows(t, s, `SELECT id, source_key FROM entries WHERE kind = 'accrual:payroll' AND effective_at < ?
		AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = entries.id) ORDER BY id`, "2026-11-10"); got != beforeNinth {
		t.Fatalf("the 9th moved:\n%s\nwas\n%s", got, beforeNinth)
	}
	// Альфа's 10th by her post, 140 × 25%; Браво's 10th by hers, the same 20 now said to come from the report.
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "35000000 0 35000000 reports" {
		t.Fatalf("Альфа's 10th: %s", got)
	}
	if got := dayOf(t, s, "bravo", "2026-11-10"); got != "20000000 0 20000000 reports" {
		t.Fatalf("Браво's 10th: %s", got)
	}
	// Браво's 12th has a sheet value and no post: it keeps what is posted (the sheet's), and says why.
	if got := dayOf(t, s, "bravo", "2026-11-12"); got != "20000000 0 20000000 sheet" {
		t.Fatalf("Браво's 12th: %s", got)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'bravo' AND date = '2026-11-12' AND status = 'unknown'
		AND reason LIKE 'по отчёту не начислено: отчёта нет%'`); n != 1 {
		t.Fatalf("Браво's held 12th:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	if n := count(t, s, `SELECT unentered_unknown FROM v_payables WHERE person = 'bravo'`); n != 1 {
		t.Fatalf("payables unknown: %d", n)
	}
	b, err := payroll.PreparePayout(ctx, s.RO, "bravo", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	held := false
	for _, f := range b.Flags {
		held = held || (f.Kind == "reports_pending" && f.Blocker && strings.Contains(f.Text, "отчёта нет"))
	}
	if !held || b.ReportsFrom != "2026-11-10" {
		t.Fatalf("Браво's brief: %+v", b.Flags)
	}
	a, err := payroll.PreparePayout(ctx, s.RO, "alfa", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range a.Lines {
		lines = append(lines, l.Source+" "+l.Text)
	}
	if got, want := strings.Join(lines, "|"), " 09.11: 80 × 25% = 20|reports 10.11: 140 × 25% = 35"; got != want {
		t.Fatalf("Альфа's lines %q, want %q", got, want)
	}
	p, err := payroll.ReadPayouts(ctx, s.RO, "2026-11-09", "2026-11-15", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, pp := range p.People {
		for _, d := range pp.Days {
			days = append(days, fmt.Sprintf("%s %s %s %d %d", pp.Slug, d.Date, d.Source, d.AmountMicros, len(d.Pages)))
		}
	}
	if got, want := strings.Join(days, "|"), "alfa 2026-11-09 sheet 20000000 1|alfa 2026-11-10 reports 35000000 2|"+
		"bravo 2026-11-10 reports 20000000 1|bravo 2026-11-12 sheet 20000000 1"; got != want || p.ReportsFrom != "2026-11-10" {
		t.Fatalf("payouts %s\nwant %s", got, want)
	}
	// The sheet is a mirror: its 10th of Альфа differs, said and no failure.
	c, err := payroll.Check(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReportsFrom != "2026-11-10" || len(c.Mirror) != 1 || c.Mirror[0].Person != "alfa" || c.Mirror[0].BooksMicros != 35_000_000 {
		t.Fatalf("check %+v", c)
	}
	if r := checks.Payables(ctx, checks.Env{Q: s.RO, Now: reportNow}); !r.OK || !strings.Contains(r.Summary, "начисляют отчёты смен") {
		t.Fatalf("health %+v", r)
	}
	// Idempotent.
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	if rep, err := payroll.Rebuild(ctx, s, reportNow); err != nil || rep.Changed() || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("a second rebuild %+v %v", rep, err)
	}

	// Off: the books are the sheets' exactly.
	setReports(t, s, ctx, "")
	if after := books(t, s); after != before {
		t.Fatalf("rollback:\n%s\nwas\n%s", after, before)
	}
	if n := count(t, s, `SELECT count(*) FROM entries e WHERE source_key LIKE '%:reports' AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`); n != 0 {
		t.Fatalf("%d reports accruals stand after the rollback", n)
	}
}

func TestAHeldDayWaitsForItsPostAndAnUnpaidDayFollowsTheEdit(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	// The post edited into a sum that does not add up: blocked, the day keeps its 35 and says why.
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "$150", "A - $120", "B - $40"))
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "35000000 0 35000000 reports" || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("a blocked post moved the day: %s", got)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'alfa' AND date = '2026-11-10' AND status = 'unknown'
		AND reason LIKE 'по отчёту не начислено: отчёт не разобран%'`); n != 1 {
		t.Fatalf("no mark of the blocked post:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	// Put right: the unpaid day follows the post at once, 160 × 25%.
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $120", "B - $40"))
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "40000000 0 40000000 reports" {
		t.Fatalf("the corrected day: %s", got)
	}
	if balance(t, s, "alfa") != 60_000_000 || count(t, s, `SELECT count(*) FROM payroll_pending_changes`) != 0 {
		t.Fatalf("Альфа after the edit:\n%s", books(t, s))
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'alfa' AND date >= '2026-11-10'`); n != 0 {
		t.Fatal("the released day is still marked")
	}
}

func payoutRow(num int, date, name, model, amount string) string {
	return fmt.Sprintf("| %d | %s | expense | Чаттер зарплата | %s | %s | %s | USDT | Internal Transfer (Bybit) | paid | по таблице |",
		num, date, name, model, amount)
}

func importPayouts(t *testing.T, s *db.Store, ctx context.Context, rows ...string) {
	t.Helper()
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	text := "---\ntitle: Журнал\nupdated: 2026-11-15\n---\n\n# Журнал\n\n## Транзакции\n\n" +
		"| # | Дата | Тип | Категория | Кому/Откуда | Модель | Сумма | Валюта | Метод | Статус | Примечание |\n" +
		"|---|------|-----|-----------|-------------|--------|-------|--------|-------|--------|------------|\n" + strings.Join(rows, "\n") + "\n"
	if _, err := journal.Import(ctx, tx, journal.FromFile(text, "file", reportNow), journal.Options{AccrualFrom: "2026-11-01"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func pendingRows(t *testing.T, s *db.Store) string {
	return cols(t, s, `SELECT id, person, date, posted_micros, new_micros, reason, state, COALESCE(decided_by, ''), COALESCE(note, '')
		FROM payroll_pending_changes ORDER BY id`)
}

func TestAPaidDayWaitsForTheOwner(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	// A payout of 50 pays the 9th (20) and 30 of the 10th (35).
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "50.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "35000000 30000000 5000000 reports" || balance(t, s, "alfa") != 5_000_000 {
		t.Fatalf("the paid 10th: %s\n%s", got, books(t, s))
	}
	fifo := books(t, s)

	// The post edited after the payout: the books stay, the change waits.
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $140", "B - $40"))
	if got := pendingRows(t, s); got != "1 alfa 2026-11-10 35000000 45000000 post_edited open  " {
		t.Fatalf("pending:\n%s", got)
	}
	if after := books(t, s); after != fifo {
		t.Fatalf("a paid day changed by itself:\n%s\nwas\n%s", after, fifo)
	}
	// It is a warning, a blocker of the brief, and in the payables and the payouts.
	if r := checks.PendingChanges(ctx, checks.Env{Q: s.RO, Now: reportNow}); r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "#1 alfa 2026-11-10") {
		t.Fatalf("check %+v", r)
	}
	b, err := payroll.PreparePayout(ctx, s.RO, "alfa", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	for _, f := range b.Flags {
		flagged = flagged || (f.Kind == "pending_change" && f.Blocker && strings.Contains(f.Text, "изменение 1"))
	}
	if !flagged || len(b.PendingChanges) != 1 || b.PendingChanges[0].NewMicros != 45_000_000 {
		t.Fatalf("brief %+v %+v", b.Flags, b.PendingChanges)
	}
	if c, err := payroll.Check(ctx, s.RO); err != nil || len(c.Pending) != 1 {
		t.Fatalf("payables check %+v %v", c.Pending, err)
	}
	p, err := payroll.ReadPayouts(ctx, s.RO, "2026-11-09", "2026-11-15", reportNow)
	if err != nil || p.People[0].Slug != "alfa" || len(p.People[0].PendingChanges) != 1 {
		t.Fatalf("payouts %+v %v", p.People, err)
	}

	// A second edit replaces the open change, it does not stack.
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $160", "B - $40"))
	if got := pendingRows(t, s); got != "1 alfa 2026-11-10 35000000 50000000 post_edited open  " {
		t.Fatalf("pending after the second edit:\n%s", got)
	}

	// Dismissed: the books as they were, and the same change does not come back.
	out, err := payroll.DecideChange(ctx, s, 1, payroll.DecisionDismiss, "owner", "оставить как выплачено", reportNow)
	if err != nil || out.Change.State != "dismissed" {
		t.Fatalf("dismiss %+v %v", out, err)
	}
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	if after := books(t, s); after != fifo || count(t, s, `SELECT count(*) FROM payroll_pending_changes WHERE state = 'open'`) != 0 {
		t.Fatalf("after dismiss:\n%s\n%s", after, pendingRows(t, s))
	}
	if _, err := payroll.DecideChange(ctx, s, 1, payroll.DecisionApply, "owner", "", reportNow); err == nil {
		t.Fatal("a decided change decided again")
	}

	// Another edit opens a new change; applied, the day is reversed and posted anew, and the FIFO follows.
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $200", "B - $40"))
	if n := count(t, s, `SELECT id FROM payroll_pending_changes WHERE state = 'open' AND new_micros = 60000000`); n != 2 {
		t.Fatalf("the new change:\n%s", pendingRows(t, s))
	}
	out, err = payroll.DecideChange(ctx, s, 2, payroll.DecisionApply, "owner", "доплатить в следующей выплате", reportNow)
	if err != nil || out.Change.State != "applied" || !out.Report.Changed() {
		t.Fatalf("apply %+v %v", out, err)
	}
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "60000000 30000000 30000000 reports" || balance(t, s, "alfa") != 30_000_000 {
		t.Fatalf("after apply: %s\n%s", got, books(t, s))
	}
	if got := dayOf(t, s, "alfa", "2026-11-09"); got != "20000000 20000000 0 sheet" {
		t.Fatalf("the 9th after apply: %s", got)
	}
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	if rep, err := payroll.Rebuild(ctx, s, reportNow); err != nil || rep.Changed() || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("a rebuild after apply %+v %v", rep, err)
	}

	// New terms of the tab, the same posted revenue: a change of its own kind.
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.3, 0, [7]any{80, 100, nil, nil, nil, nil, nil}),
			"Браво": pageTab("2026-11-09", "Page A", 0.2, 10, [7]any{nil, 50, nil, 50, nil, nil, nil})},
	})
	if n := count(t, s, `SELECT count(*) FROM payroll_pending_changes WHERE id = 3 AND state = 'open' AND reason = 'terms_changed'
		AND posted_micros = 60000000 AND new_micros = 72000000`); n != 1 {
		t.Fatalf("terms changed:\n%s", pendingRows(t, s))
	}

	// Off: the sheets again, and what waited is closed by itself.
	setReports(t, s, ctx, "")
	if got := pendingRows(t, s); !strings.HasSuffix(got, "3 alfa 2026-11-10 60000000 72000000 terms_changed dismissed ledger источник выключен") {
		t.Fatalf("pending after off:\n%s", got)
	}
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "30000000 26000000 4000000 sheet" {
		t.Fatalf("the 10th from the sheet again: %s\n%s", got, books(t, s))
	}
}

func TestOnlyOneSourceAfterTheSheets(t *testing.T) {
	s, ctx := reportBook(t)
	if err := payroll.SetReportsFrom(ctx, s.RW, "2026-10-01", reportNow); err == nil || !strings.Contains(err.Error(), "раньше accrual_from") {
		t.Fatalf("before accrual_from: %v", err)
	}
	if from, err := payroll.ReportsFrom(ctx, s.RO); err != nil || from != "" {
		t.Fatalf("off by default: %q %v", from, err)
	}
	if err := payroll.SetReportsFrom(ctx, s.RW, "2026-11-10", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := payroll.SetFactsFrom(ctx, s.RW, "2026-11-10", reportNow); err == nil || !strings.Contains(err.Error(), "отчётов смен") {
		t.Fatalf("facts with the reports on: %v", err)
	}
	if err := payroll.SetReportsFrom(ctx, s.RW, "", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := payroll.SetFactsFrom(ctx, s.RW, "2026-11-10", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := payroll.SetReportsFrom(ctx, s.RW, "2026-11-10", reportNow); err == nil || !strings.Contains(err.Error(), "фактов хаба") {
		t.Fatalf("reports with the facts on: %v", err)
	}
	// Both set by hand is refused by the rebuild, not half followed.
	exec(t, s, `UPDATE settings SET value = '2026-11-10' WHERE key = 'payroll_reports_from'`)
	if _, err := payroll.Rebuild(ctx, s, reportNow); err == nil || !strings.Contains(err.Error(), "оба источника") {
		t.Fatalf("both on: %v", err)
	}
}

func TestAShiftWithoutAReport(t *testing.T) {
	s, ctx := reportBook(t)
	week(t, s) // Альфа's shift on the 10th, Браво's on the 10th and the 12th, user 103's (no card)
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.25, 0, [7]any{80, 100, nil, nil, nil, nil, nil}),
			"Браво": pageTab("2026-11-09", "Page A", 0.2, 10, [7]any{nil, 0, nil, nil, nil, nil, nil})},
	})
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $100"))
	gaps, err := payroll.ShiftsWithoutReport(ctx, s.RO, "2026-11-03", "2026-11-17", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range gaps {
		got = append(got, g.Person+" "+g.ReportDate+" "+g.Model)
	}
	if strings.Join(got, "|") != "bravo 2026-11-10 Alpha|bravo 2026-11-12 Alpha" {
		t.Fatalf("gaps %v", got)
	}
	// A shift ended less than a day ago is not asked about yet.
	if gaps, _ := payroll.ShiftsWithoutReport(ctx, s.RO, "2026-11-03", "2026-11-17", at("2026-11-13T10:00:00+03:00")); len(gaps) != 1 {
		t.Fatalf("a fresh shift: %+v", gaps)
	}
	// The sheets still pay: the shifts without a report are information, the check stays green.
	r := checks.ShiftReports(ctx, checks.Env{Q: s.RO, Now: reportNow})
	if !r.OK || !strings.Contains(r.Summary, "в тени") || !strings.Contains(r.Summary, "2 смен") {
		t.Fatalf("check in shadow %+v", r)
	}
	// Before the switch «не занесено» is as it was: no row of the reports.
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE reason LIKE 'смена без отчёта%'`); n != 0 {
		t.Fatal("a shift without a report before the switch")
	}
	// Under the reports a worked shift without a report is what «не занесено» means, and the check warns.
	setReports(t, s, ctx, "2026-11-10")
	r = checks.ShiftReports(ctx, checks.Env{Q: s.RO, Now: reportNow})
	if r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "bravo 2026-11-10") || !strings.Contains(r.Summary, "bravo 2026-11-12") {
		t.Fatalf("check %+v", r)
	}
	// The 12th: the shift and no report. The 10th has the sheet's fix and no report: held as posted, said first.
	if got := cols(t, s, `SELECT date, CASE WHEN reason LIKE 'по отчёту не начислено: отчёта нет%' THEN 'held'
		WHEN reason LIKE 'смена без отчёта: смена 12.11 10:00 — 12.11 18:00%' THEN 'shift' ELSE reason END
		FROM payroll_unentered WHERE person = 'bravo' AND status = 'unknown' ORDER BY date`); got != "2026-11-10 held\n2026-11-12 shift" {
		t.Fatalf("«не занесено» under the reports:\n%s", got)
	}
	b, err := payroll.PreparePayout(ctx, s.RO, "bravo", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, f := range b.Flags {
		if f.Blocker {
			kinds[f.Kind] = true
		}
	}
	if !kinds["no_report"] || !kinds["reports_pending"] {
		t.Fatalf("brief flags %+v", b.Flags)
	}
}

func TestTheChangeRoutesNeedTheDecideScope(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "50.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	post(t, s, 1, "501", "2026-11-10", postText("2026-11-10", "", "A - $140", "B - $40"))
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	token := func(name, scope string) string {
		tok, _, err := auth.Create(ctx, s.RW, name, []string{scope}, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	var e *client.Error
	var out payroll.DecideResult
	for _, scope := range []string{auth.ScopeRead, auth.ScopePayouts, auth.ScopeReports} {
		c := client.New(config.Client{URL: srv.URL, Key: token("k-"+scope, scope)})
		if err := c.Do(ctx, http.MethodPost, "/v1/payroll/changes/1/apply", map[string]string{}, &out); !asStatus(err, &e, 403) {
			t.Fatalf("%s may apply: %v", scope, err)
		}
	}
	owner := client.New(config.Client{URL: srv.URL, Key: token("owner", auth.ScopeDecide)})
	if err := owner.Do(ctx, http.MethodPost, "/v1/payroll/changes/9/dismiss", map[string]string{}, &out); !asStatus(err, &e, 422) {
		t.Fatalf("no such change: %v", err)
	}
	if err := owner.Do(ctx, http.MethodPost, "/v1/payroll/changes/1/dismiss", map[string]string{"note": "нет"}, &out); err != nil ||
		out.Change.State != "dismissed" || out.Change.DecidedBy != "owner" {
		t.Fatalf("dismiss %+v %v", out, err)
	}
	// Hub's payouts key sees the switch day.
	hubKey := client.New(config.Client{URL: srv.URL, Key: token("hub", auth.ScopePayouts)})
	var p payroll.PayoutsAnswer
	if err := hubKey.Do(ctx, http.MethodGet, "/v1/payouts?from=2026-11-09&to=2026-11-15", nil, &p); err != nil || p.ReportsFrom != "2026-11-10" {
		t.Fatalf("payouts %+v %v", p.ReportsFrom, err)
	}
}

// A day the sheet accrued and a payout reached before the switch: the report that says otherwise waits too.
func TestASheetDayPaidBeforeTheSwitchWaitsForTheOwner(t *testing.T) {
	s, ctx := reportWeek(t)
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "30.00")) // the 9th (20) and 10 of the 10th (25)
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	fifo := books(t, s)
	setReports(t, s, ctx, "2026-11-10")
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "25000000 10000000 15000000 sheet" {
		t.Fatalf("the paid sheet day moved: %s", got)
	}
	if got := pendingRows(t, s); got != "1 alfa 2026-11-10 25000000 35000000 new_post open  " {
		t.Fatalf("pending:\n%s", got)
	}
	// Браво's 10th was not paid: it follows the report at once (the same 20, now from the report).
	if got := dayOf(t, s, "bravo", "2026-11-10"); got != "20000000 0 20000000 reports" {
		t.Fatalf("Браво's 10th: %s", got)
	}
	// Off again: nothing waits, the books are the sheets'.
	setReports(t, s, ctx, "")
	if after := books(t, s); after != fifo {
		t.Fatalf("off:\n%s\nwas\n%s", after, fifo)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_pending_changes WHERE state = 'open'`); n != 0 {
		t.Fatal("a change still open with the reports off")
	}
}
