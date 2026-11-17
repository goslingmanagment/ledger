package accruals

import (
	"strings"
	"testing"
)

// The board «Отчёт» as the basis of a day (П2). Who hands in on the board comes from Hub's people registry: the right
// submits_shift with when it was granted (rightGrants). From settings.payroll_basis_hub_from such a person's day — from the
// day after the grant on — accrues only from their counted Hub report: without one the day waits in «не занесено», and
// the post does not stand in for it. Everyone else, and the grant's own day, go as before.

// grant puts the right submits_shift on the person in the registry stub, granted at the time given.
func grant(r *registry, code, at string) {
	p := r.person(code)
	p["rights"] = []any{"submits_shift"}
	p["rightGrants"] = []any{map[string]any{"right": "submits_shift", "grantedAt": at}}
}

func TestABoardPersonsDayWaitsForTheirCountedHubReport(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	// Альфа hands in on the board since the 9th (Moscow): from the 10th on her days are the board's. Браво since the 12th:
	// her 10th is an ordinary day, the 12th the grant's own day.
	r := loadRegistry(t)
	grant(r, "alfa", "2026-11-09T09:00:00.000Z")
	grant(r, "bravo", "2026-11-12T09:00:00.000Z")
	r.read(t, s)
	if got, want := cols(t, s, `SELECT person, from_day, COALESCE(to_day, '-') FROM v_board_spans ORDER BY person`),
		"alfa 2026-11-10 -\nbravo 2026-11-13 -"; got != want {
		t.Fatalf("board spans:\n%s\nwant\n%s", got, want)
	}
	setHub(t, s, ctx, "2026-11-10")

	// Альфа's 10th has her post (140) and her sheet (100), no Hub report: nothing accrues, the day waits.
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "" {
		t.Fatalf("Альфа's 10th accrued without a Hub report: %s\n%s", got, books(t, s))
	}
	waits := func(person, date string) bool {
		return count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = ? AND date = ? AND status = 'unknown'
			AND reason LIKE 'по отчёту не начислено: сдаёт смену доской%'`, person, date) == 1
	}
	if !waits("alfa", "2026-11-10") {
		t.Fatalf("Альфа's 10th does not wait:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	// A post of a board day that comes later does not accrue either.
	post(t, s, 3, "501", "2026-11-11", postText("2026-11-11", "", "A - $60"))
	if got := dayOf(t, s, "alfa", "2026-11-11"); got != "" || !waits("alfa", "2026-11-11") {
		t.Fatalf("Альфа's posted 11th: %q\n%s", got, rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	// Браво: her 10th is not a board day — by her post, as before; the 12th (the grant's day, sheet only) as before: held.
	if got := dayOf(t, s, "bravo", "2026-11-10"); got != "20000000 0 20000000 reports" {
		t.Fatalf("Браво's 10th: %s", got)
	}
	if got := dayOf(t, s, "bravo", "2026-11-12"); !strings.HasPrefix(got, "20000000 0 20000000") || waits("bravo", "2026-11-12") {
		t.Fatalf("Браво's 12th: %s\n%s", got, rows(t, s, `SELECT * FROM payroll_unentered`))
	}

	// Альфа confirms her 10th and 11th on the board: each day accrues from its Hub report alone, 25 %, and waits no more.
	handIn(t, s, hubVersion(1, 1, 1, "alfa", "2026-11-10", hubPage{"page-a", 110}, hubPage{"page-b", 40}),
		hubVersion(2, 2, 1, "alfa", "2026-11-11", hubPage{"page-a", 60}))
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "37500000 0 37500000 reports" {
		t.Fatalf("Альфа's 10th from Hub: %s", got)
	}
	if got := dayOf(t, s, "alfa", "2026-11-11"); got != "15000000 0 15000000 reports" {
		t.Fatalf("Альфа's 11th from Hub: %s", got)
	}
	if waits("alfa", "2026-11-10") || waits("alfa", "2026-11-11") {
		t.Fatalf("a counted day still waits:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}

	// The pay base says which days are the board's.
	want := strings.Join([]string{"alfa 2026-11-10 1 hub", "alfa 2026-11-11 1 hub", "bravo 2026-11-10 0 post"}, "\n")
	if got := cols(t, s, `SELECT DISTINCT person, report_date, board, basis FROM v_report_pages WHERE report_date >= '2026-11-10'
		ORDER BY person, report_date`); got != want {
		t.Fatalf("board days:\n%s\nwant\n%s", got, want)
	}
}

// A night shift's newer version may move its report to the next day (hub#1279). The day it left has no Hub report any
// more: it keeps no accrual — not held as posted, not in «не занесено» — and a paid one waits for the owner as any change.
func TestADayWhoseHubReportMovedKeepsNoAccrual(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	setHub(t, s, ctx, "2026-11-10")
	// Браво's night shift 9: handed in for the 13th (30), no post that day; then its next version for the 14th (45).
	first := hubVersion(1, 9, 1, "bravo", "2026-11-13", hubPage{"page-a", 30})
	handIn(t, s, first)
	if got := dayOf(t, s, "bravo", "2026-11-13"); got != "16000000 0 16000000 reports" { // 30 × 20% + the fix
		t.Fatalf("the 13th by the first version: %s", got)
	}
	handIn(t, s, first, hubVersion(2, 9, 2, "bravo", "2026-11-14", hubPage{"page-a", 45}))
	if got := dayOf(t, s, "bravo", "2026-11-13"); got != "" {
		t.Fatalf("the 13th kept its accrual after the report moved: %s\n%s", got, books(t, s))
	}
	if got := dayOf(t, s, "bravo", "2026-11-14"); got != "19000000 0 19000000 reports" {
		t.Fatalf("the 14th: %s", got)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'bravo' AND date = '2026-11-13'`); n != 0 {
		t.Fatalf("the 13th in «не занесено»:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}

	// On the board the same: Альфа (board since the 9th) posted her 11th from the board, and the report moved to the 12th.
	// The 11th is not paid by the post and does not wait either: its report is the 12th's.
	r := loadRegistry(t)
	grant(r, "alfa", "2026-11-09T09:00:00.000Z")
	r.read(t, s)
	night := hubVersion(3, 10, 1, "alfa", "2026-11-11", hubPage{"page-a", 60})
	handIn(t, s, first, night)
	post(t, s, 3, "501", "2026-11-11", postText("2026-11-11", "", "A - $60"))
	if got := dayOf(t, s, "alfa", "2026-11-11"); got != "15000000 0 15000000 reports" {
		t.Fatalf("Альфа's 11th from Hub: %s", got)
	}
	handIn(t, s, first, night, hubVersion(4, 10, 2, "alfa", "2026-11-12", hubPage{"page-a", 80}))
	if got := dayOf(t, s, "alfa", "2026-11-11"); got != "" {
		t.Fatalf("Альфа's 11th after the move: %s", got)
	}
	if got := dayOf(t, s, "alfa", "2026-11-12"); got != "20000000 0 20000000 reports" {
		t.Fatalf("Альфа's 12th: %s", got)
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'alfa' AND date = '2026-11-11'`); n != 0 {
		t.Fatalf("Альфа's 11th in «не занесено»:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
}
