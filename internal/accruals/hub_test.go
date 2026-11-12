package accruals

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
)

// The reports handed in in Hub (ecosystem F2) on the week of reportWeek: what they change while
// settings.payroll_basis_hub_from is empty (nothing), and what from it on (the person's day accrues from Hub's report
// when one exists, else from the post; a paid day waits for the owner).

// hubPage is a page of a handed-in version and its amount in whole dollars: one counted sale.
type hubPage struct {
	page    string
	dollars int64
}

// hubVersion is a version of Hub's feed by its documented shape.
func hubVersion(id, shift int64, version int, person, date string, pages ...hubPage) json.RawMessage {
	var ps, ls []any
	total := int64(0)
	for i, p := range pages {
		m := p.dollars * 1000
		total += m
		ps = append(ps, map[string]any{"pageLabel": p.page, "amountMills": m})
		ls = append(ls, map[string]any{"transactionId": id*100 + int64(i), "pageLabel": p.page, "group": "mine", "netMills": m,
			"ruleShareMills": m, "ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
			"amountMills": m, "added": false, "disputed": false})
	}
	b, _ := json.Marshal(map[string]any{"id": id, "person": person, "hubUserId": 100 + shift, "shiftId": shift, "reportDate": date,
		"version": version, "submittedAt": date + "T21:00:00.000Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": total,
		"pages": ps, "lines": ls, "offPlatform": []any{}, "comment": "", "dayPaid": nil, "ruleVersion": "c2"})
	return b
}

// handIn serves the versions as Hub's feed and reads it the way the job does; the accruals follow.
func handIn(t *testing.T, s *db.Store, versions ...json.RawMessage) reports.HubStatus {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		out, next := []json.RawMessage{}, since
		for _, v := range versions {
			var x struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(v, &x)
			if x.ID > since {
				out, next = append(out, v), x.ID
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"reports": out, "next": strconv.FormatInt(next, 10), "serverNow": "2026-11-17T09:00:00Z"})
	}))
	defer srv.Close()
	st, err := reports.SyncHub(context.Background(), s, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "k"}, nil), reportNow)
	if err != nil {
		t.Fatalf("hand in: %+v %v", st, err)
	}
	return st
}

func setHub(t *testing.T, s *db.Store, ctx context.Context, day string) {
	t.Helper()
	if err := payroll.SetHubFrom(ctx, s.RW, day, reportNow); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildReports(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
}

// hubWeek are Hub's versions of reportWeek's 10th and 12th: Альфа handed in what she posted (100 + 40) and corrected it
// to 110 + 40 after posting; Браво worked the 10th in two shifts, 30 and 20 — her post's 50 — and handed in the 12th, 50,
// which she never posted.
func hubWeek() []json.RawMessage {
	return []json.RawMessage{
		hubVersion(1, 1, 1, "alfa", "2026-11-10", hubPage{"page-a", 100}, hubPage{"page-b", 40}),
		hubVersion(2, 1, 2, "alfa", "2026-11-10", hubPage{"page-a", 110}, hubPage{"page-b", 40}),
		hubVersion(3, 2, 1, "bravo", "2026-11-10", hubPage{"page-a", 30}),
		hubVersion(4, 3, 1, "bravo", "2026-11-10", hubPage{"page-a", 20}),
		hubVersion(5, 4, 1, "bravo", "2026-11-12", hubPage{"page-a", 50}),
	}
}

// everything is what the accruals are: the debts, every accrual entry, report_accruals as stored, the sheets mirror's
// base and the payouts Hub reads.
func everything(t *testing.T, s *db.Store, ctx context.Context) string {
	t.Helper()
	mirror, err := payroll.LoadMirrorBase(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	payouts, err := payroll.ReadPayouts(ctx, s.RO, "2026-11-09", "2026-11-15", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(mirror)
	p, _ := json.Marshal(payouts)
	return books(t, s) + "\n==\n" +
		rows(t, s, `SELECT id, kind, source_key, effective_at, recorded_at FROM entries ORDER BY id`) + "\n==\n" +
		rows(t, s, `SELECT * FROM report_accruals ORDER BY person, model, day`) + "\n==\n" +
		rows(t, s, `SELECT * FROM payroll_unentered ORDER BY model, name, date, page`) + "\n==\n" + string(m) + "\n==\n" + string(p)
}

func TestHubReportsChangeNoAccrualWhileTheSettingIsOff(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10") // as production: the posts accrue
	before := everything(t, s, ctx)

	st := handIn(t, s, hubWeek()...)
	if st.Stored != 5 || st.Problems != 0 {
		t.Fatalf("hand in %+v", st)
	}
	if after := everything(t, s, ctx); after != before {
		t.Fatalf("Hub's reports moved the accruals with the setting off:\n%s\nwas\n%s", after, before)
	}
	// The comparison has the column: Альфа's page-a differs, Браво's two shifts equal her post, her 12th is Hub's only.
	if got, want := cols(t, s, `SELECT person, report_date, page, COALESCE(posted_micros, '-'), hub_micros, COALESCE(hub_diff_micros, '-'),
		hub_state, basis FROM v_report_pages WHERE hub_state IS NOT NULL ORDER BY person, report_date, page`), strings.Join([]string{
		"alfa 2026-11-10 page-a 100000000 110000000 -10000000 hub_changed post",
		"alfa 2026-11-10 page-b 40000000 40000000 0 hub_equal post",
		"bravo 2026-11-10 page-a 50000000 50000000 0 hub_equal post",
		"bravo 2026-11-12 page-a - 50000000 - hub_only post",
	}, "\n"); got != want {
		t.Fatalf("the pay base:\n%s\nwant\n%s", got, want)
	}
	d, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := hubDays(d); got != "alfa 2026-11-10 post 140000000 150000000|bravo 2026-11-12 post - 50000000" || d.HubFrom != "" {
		t.Fatalf("hub days %s (from %q)", got, d.HubFrom)
	}
}

func hubDays(d Diff) string {
	var out []string
	for _, x := range d.HubDays {
		post := "-"
		if x.PostMicros != nil {
			post = fmt.Sprint(*x.PostMicros)
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %d", x.Person, x.Day, x.Basis, post, *x.HubMicros))
	}
	return strings.Join(out, "|")
}

func TestFromTheHubDayAPersonsDayAccruesFromTheirHandedInReport(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	handIn(t, s, hubWeek()...)
	ninth := dayOf(t, s, "alfa", "2026-11-09")
	setHub(t, s, ctx, "2026-11-10")

	// Альфа's 10th by her corrected version, 150 × 25%; Браво's 10th by her two shifts added, 50 × 20% + the fix once;
	// her 12th, never posted, by Hub's report — no longer held. The 9th is the sheet's.
	for _, c := range []struct{ person, date, want string }{
		{"alfa", "2026-11-10", "37500000 0 37500000 reports"},
		{"bravo", "2026-11-10", "20000000 0 20000000 reports"},
		{"bravo", "2026-11-12", "20000000 0 20000000 reports"},
		{"alfa", "2026-11-09", ninth},
	} {
		if got := dayOf(t, s, c.person, c.date); got != c.want {
			t.Fatalf("%s %s: %s, want %s\n%s", c.person, c.date, got, c.want, books(t, s))
		}
	}
	if got, want := cols(t, s, `SELECT person, day, net_micros, posts, COALESCE(hub_reports, '-') FROM report_accruals ORDER BY person, day`),
		strings.Join([]string{
			"alfa 2026-11-10 150000000 [] [2]",
			"bravo 2026-11-10 50000000 [] [3,4]",
			"bravo 2026-11-12 50000000 [] [5]",
		}, "\n"); got != want {
		t.Fatalf("report_accruals:\n%s\nwant\n%s", got, want)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE date >= '2026-11-10'`); n != 0 {
		t.Fatalf("«не занесено» from the hub day:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	// The sheets mirror writes what accrues: Hub's amounts.
	base, err := payroll.LoadMirrorBase(ctx, s.RO, "")
	if err != nil {
		t.Fatal(err)
	}
	var mirror []string
	for _, p := range base.Posts {
		mirror = append(mirror, fmt.Sprintf("%s %s %s %d", p.Person, p.Date, p.Page, p.Micros))
	}
	if got, want := strings.Join(mirror, "|"), "alfa 2026-11-10 page-a 110000000|alfa 2026-11-10 page-b 40000000|"+
		"bravo 2026-11-10 page-a 50000000|bravo 2026-11-12 page-a 50000000"; got != want {
		t.Fatalf("mirror %s\nwant %s", got, want)
	}
	// `accruals diff` lists exactly the days where Hub's report and the post differ.
	d, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := hubDays(d); got != "alfa 2026-11-10 hub 140000000 150000000|bravo 2026-11-12 hub - 50000000" || d.HubFrom != "2026-11-10" {
		t.Fatalf("hub days %s (from %q)", got, d.HubFrom)
	}
	// Idempotent.
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	if rep, err := payroll.Rebuild(ctx, s, reportNow); err != nil || rep.Changed() || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("a second rebuild %+v %v", rep, err)
	}

	// A newer version Hub served that cannot be stored (a page ledger does not know) holds the day as posted, unknown.
	if st := handIn(t, s, append(hubWeek(), hubVersion(6, 1, 3, "alfa", "2026-11-10", hubPage{"page-z", 5}))...); st.Problems != 1 {
		t.Fatalf("hand in the bad version %+v", st)
	}
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "37500000 0 37500000 reports" {
		t.Fatalf("the held 10th: %s", got)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'alfa' AND date = '2026-11-10' AND status = 'unknown'
		AND reason LIKE 'по отчёту не начислено: отчёт хаба не сохранён: версия 6 — страницы «page-z» нет в hub_pages%'`); n != 1 {
		t.Fatalf("no mark of the bad version:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	if r := checks.HubReports(ctx, checks.Env{Q: s.RO, Now: reportNow}); r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "версия 6 (смена 1, alfa, 2026-11-10)") {
		t.Fatalf("health %+v", r)
	}

	// Off: the posts again — Альфа's 10th by her post; Браво's 12th has none and keeps what is posted.
	setHub(t, s, ctx, "")
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "35000000 0 35000000 reports" {
		t.Fatalf("Альфа's 10th after off: %s", got)
	}
	if got := dayOf(t, s, "bravo", "2026-11-12"); !strings.HasPrefix(got, "20000000 0 20000000") {
		t.Fatalf("Браво's 12th after off: %s", got)
	}
}

func TestAPaidDayDoesNotFollowHubsReportByItself(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	// A payout of 50 pays the 9th (20) and 30 of the 10th (35).
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "50.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	fifo := books(t, s)
	// Hub's first version equals the post: from the hub day nothing moves.
	handIn(t, s, hubWeek()[0])
	setHub(t, s, ctx, "2026-11-10")
	if got := books(t, s); got != fifo || count(t, s, `SELECT count(*) FROM payroll_pending_changes`) != 0 {
		t.Fatalf("an equal report moved the paid day:\n%s\nwas\n%s\n%s", got, fifo, pendingRows(t, s))
	}
	// The corrected version after the payout: the books stay, the change waits for the owner.
	handIn(t, s, hubWeek()[:2]...)
	if got := pendingRows(t, s); got != "1 alfa 2026-11-10 35000000 37500000 post_edited open  " {
		t.Fatalf("pending:\n%s", got)
	}
	if got := books(t, s); got != fifo {
		t.Fatalf("a paid day changed by itself:\n%s\nwas\n%s", got, fifo)
	}
}

func TestHubsReportIsABasisOfTheReportsOnly(t *testing.T) {
	s, ctx := reportBook(t)
	if err := payroll.SetHubFrom(ctx, s.RW, "2026-11-10", reportNow); err == nil || !strings.Contains(err.Error(), "отчёты смен не начисляют") {
		t.Fatalf("Hub without the reports: %v", err)
	}
	if from, err := payroll.HubFrom(ctx, s.RO); err != nil || from != "" {
		t.Fatalf("off by default: %q %v", from, err)
	}
	if err := payroll.SetReportsFrom(ctx, s.RW, "2026-11-10", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := payroll.SetHubFrom(ctx, s.RW, "2026-11-09", reportNow); err == nil || !strings.Contains(err.Error(), "раньше начала отчётов") {
		t.Fatalf("Hub before the reports: %v", err)
	}
	if err := payroll.SetHubFrom(ctx, s.RW, "2026-11-10", reportNow); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"", "2026-11-11"} {
		if err := payroll.SetReportsFrom(ctx, s.RW, day, reportNow); err == nil || !strings.Contains(err.Error(), "hub-from --off") {
			t.Fatalf("the reports to %q under Hub: %v", day, err)
		}
	}
	if from, _, err := payroll.AfterFrom(ctx, s.RO); err != nil || from != "2026-11-10" {
		t.Fatalf("after from %q %v", from, err)
	}
	// Cleared by hand under it: the rebuild refuses rather than guess.
	exec(t, s, `UPDATE settings SET value = '' WHERE key = ?`, payroll.SettingReportsFrom)
	if _, _, err := payroll.AfterFrom(ctx, s.RO); err == nil || !strings.Contains(err.Error(), "только внутри отчётов") {
		t.Fatalf("Hub without the reports set by hand: %v", err)
	}
	if err := payroll.SetHubFrom(ctx, s.RW, "", reportNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := payroll.AfterFrom(ctx, s.RO); err != nil {
		t.Fatal(err)
	}
}

// A day accrued from Hub's report (basis hub) that leaves out a page the post has loses that page: the day is one basis.
// The loss is not silent: the health check of Hub's reports fails, naming the person, the day and the page, until the
// report or the post is put right.
func TestAHubDayMissingAPostedPageFailsTheCheck(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	// Альфа posted page-a 100 + page-b 40 for the 10th; her Hub report has page-a only.
	handIn(t, s, hubVersion(1, 1, 1, "alfa", "2026-11-10", hubPage{"page-a", 100}))
	if r := checks.HubReports(ctx, checks.Env{Q: s.RO, Now: reportNow}); !r.OK {
		t.Fatalf("before the hub day: %+v", r)
	}
	setHub(t, s, ctx, "2026-11-10")
	if got := cols(t, s, `SELECT page, COALESCE(posted_micros, '-'), COALESCE(accrue_micros, '-'), hub_state, basis FROM v_report_pages
		WHERE person = 'alfa' AND report_date = '2026-11-10' ORDER BY page`); got != "page-a 100000000 100000000 hub_equal hub\npage-b 40000000 - hub_missing hub" {
		t.Fatalf("the pay base:\n%s", got)
	}
	r := checks.HubReports(ctx, checks.Env{Q: s.RO, Now: reportNow})
	if r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "alfa 2026-11-10 page-b") {
		t.Fatalf("the lost page is silent: %+v", r)
	}
	// Hub's corrected report has both pages: the check passes again.
	handIn(t, s, hubVersion(1, 1, 1, "alfa", "2026-11-10", hubPage{"page-a", 100}),
		hubVersion(2, 1, 2, "alfa", "2026-11-10", hubPage{"page-a", 100}, hubPage{"page-b", 40}))
	if r := checks.HubReports(ctx, checks.Env{Q: s.RO, Now: reportNow}); !r.OK {
		t.Fatalf("after the corrected report: %+v", r)
	}
}
