package accruals

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/split"
)

// The switch of the accruals to Hub's facts (L4), on the synthetic week of 9 November: from the 10th on the days come
// from fact_accruals, the 9th stays the sheet's.

const switchDay = "2026-11-10"

var switchNow = at("2026-11-17T12:00:00+03:00")

// switchWeek is the week of 9 November with «6», a tip nobody's shift held, and the sheets: Альфа's 80 on the 9th and an
// empty 10th (Hub has her tip of 100 there), Браво's shift of the 10th at the fix and 50 on the 12th (Hub has her PPV of
// 40).
func switchWeek(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := open(t)
	week(t, s)
	putSale(t, s, "2026-11-09", sale("6", split.SaleTip, "page-a", "2026-11-11 03:00", 50_000))
	sheets(t, s, ctx, map[string][][]payroll.Cell{
		"Альфа": tab("2026-11-09", 0.25, 0, [7]any{80, nil, nil, nil, nil, nil, nil}),
		"Браво": tab("2026-11-09", 0.2, 10, [7]any{nil, 0, nil, 50, nil, nil, nil}),
	})
	if _, err := Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	if _, err := payroll.Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func setSwitch(t *testing.T, s *db.Store, ctx context.Context, day string) {
	t.Helper()
	if err := payroll.SetFactsFrom(ctx, s.RW, day, switchNow); err != nil {
		t.Fatal(err)
	}
	if _, err := Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	if _, err := payroll.Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
}

// rows renders a query's rows, one per line.
func rows(t *testing.T, s *db.Store, q string, args ...any) string {
	t.Helper()
	r, err := s.RO.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer r.Close()
	cols, _ := r.Columns()
	var out []string
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	return strings.Join(out, "\n")
}

// books is what the debts are: the FIFO of days and each person's balance against the sheets.
func books(t *testing.T, s *db.Store) string {
	return rows(t, s, `SELECT person, seq, kind, date, model, name, amount, paid, unpaid, paid_by FROM payable_days ORDER BY person, seq`) +
		"\n--\n" + rows(t, s, `SELECT person, balance_micros, to_pay, difference, oldest_unpaid, unpaid_days FROM v_payables ORDER BY person`)
}

func balance(t *testing.T, s *db.Store, person string) int64 {
	return count(t, s, `SELECT balance_micros FROM v_payables WHERE person = ?`, person)
}

func TestTheSwitchAccruesFromTheFactsAndTheSheetKeepsTheDaysBefore(t *testing.T) {
	s, ctx := switchWeek(t)
	before := books(t, s)
	beforeEntries := rows(t, s, `SELECT id, source_key, effective_at, recorded_at FROM entries WHERE kind = 'accrual:payroll'
		AND effective_at < ? ORDER BY id`, switchDay)
	if balance(t, s, "alfa") != 20_000_000 || balance(t, s, "bravo") != 30_000_000 {
		t.Fatalf("the sheets' debts:\n%s", before)
	}
	// Before the switch Альфа's empty 10th is «не занесено» by her sales.
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'alfa' AND date = '2026-11-10' AND status = 'ok'`); n != 1 {
		t.Fatal("the 10th is not «не занесено» before the switch")
	}

	setSwitch(t, s, ctx, switchDay)
	// The 9th is the sheet's, untouched: the same entry, the same FIFO row.
	if got := rows(t, s, `SELECT id, source_key, effective_at, recorded_at FROM entries WHERE kind = 'accrual:payroll'
		AND effective_at < ? AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = entries.id) ORDER BY id`, switchDay); got != beforeEntries {
		t.Fatalf("the days before the switch moved:\n%s\nwas\n%s", got, beforeEntries)
	}
	// From the 10th the facts: Альфа's tip at 25%, Браво's fix of the 10th and PPV with the fix on the 12th, keyed to their
	// tabs; the sheets' 10th and 12th of Браво are reversed even where the amount is the same — the day says its source.
	for _, c := range []struct {
		person, date string
		amount       int64
	}{{"alfa", "2026-11-09", 20_000_000}, {"alfa", "2026-11-10", 25_000_000}, {"bravo", "2026-11-10", 10_000_000}, {"bravo", "2026-11-12", 18_000_000}} {
		source := payroll.SourceFacts
		if c.date < switchDay {
			source = payroll.SourceSheet
		}
		if n := count(t, s, `SELECT count(*) FROM payable_days WHERE person = ? AND date = ? AND kind = 'day' AND amount = ? AND source = ?`,
			c.person, c.date, c.amount, source); n != 1 {
			t.Fatalf("%s %s: want %d from %s:\n%s", c.person, c.date, c.amount, source, books(t, s))
		}
	}
	if n := count(t, s, `SELECT count(*) FROM entries e WHERE kind = 'accrual:payroll' AND source_key LIKE '%:facts'
		AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id) AND NOT EXISTS (SELECT 1 FROM entry_events x WHERE x.entry_id = e.id)`); n != 3 {
		t.Fatalf("%d active facts accruals, want 3", n)
	}
	if balance(t, s, "alfa") != 45_000_000 || balance(t, s, "bravo") != 28_000_000 {
		t.Fatalf("debts after the switch:\n%s", books(t, s))
	}
	// The общак, the tip waiting for a decision and the unknown user's tip are nobody's debt.
	if n := count(t, s, `SELECT COALESCE(SUM(-l.amount), 0) FROM lines l JOIN entries e ON e.id = l.entry_id WHERE e.kind = 'accrual:payroll'
		AND l.account LIKE 'payable:%' AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`); n != 73_000_000 {
		t.Fatalf("accrued in all %d, want 20 + 25 + 10 + 18", n)
	}
	// «Не занесено» no longer asks the sheet for the 10th: the facts accrued it.
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE date >= ?`, switchDay); n != 0 {
		t.Fatalf("«не занесено» from the switch on:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	// A rebuild over the same facts posts nothing.
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	if rep, err := payroll.Rebuild(ctx, s, switchNow); err != nil || rep.Changed() || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("a second rebuild %+v %v", rep, err)
	}

	// The health check compares what the sheet governs: the 9th, and the days from the switch as the sheet has them. The
	// mirror's difference from the facts is said, not a failure.
	c, err := payroll.Check(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	var mirror []string
	for _, d := range c.Mirror {
		mirror = append(mirror, fmt.Sprintf("%s %s %d %d", d.Person, d.Date, d.SheetMicros, d.BooksMicros))
	}
	if got, want := strings.Join(mirror, "|"), "alfa 2026-11-10 0 25000000|bravo 2026-11-12 20000000 18000000"; got != want {
		t.Fatalf("mirror %s, want %s", got, want)
	}
	if len(c.Unaccrued) != 1 || c.Unaccrued[0].UserID != 103 || c.Unaccrued[0].NetMicros != 12_000_000 {
		t.Fatalf("unaccrued %+v", c.Unaccrued)
	}
	r := checks.Payables(ctx, checks.Env{Q: s.RO, Now: switchNow})
	if !r.OK || !strings.Contains(r.Summary, "таблица-зеркало расходится в 2 дн.") || !strings.Contains(r.Summary, "user103") {
		t.Fatalf("health %+v", r)
	}

	// The tip that waited for a decision is Альфа's by the owner's split: her 10th grows by its 25%.
	exec(t, s, `UPDATE hub_sales SET payload = json_set(payload, '$.resolution', json('[{"userId": 101, "percent": 100}]'))
		WHERE transaction_ref = '4'`)
	if _, err := Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT amount FROM payable_days WHERE person = 'alfa' AND date = '2026-11-10'`); n != 30_000_000 {
		t.Fatalf("Альфа's 10th after the owner's split: %d", n)
	}
	// The owner's claim of the nobody's tip is Браво's, on the sale's day.
	if _, err := Claim(ctx, s, ClaimInput{Ref: "6", Person: "bravo", Note: "по слову владельца"}, "test", switchNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT amount FROM payable_days WHERE person = 'bravo' AND date = '2026-11-11' AND source = 'facts'`); n != 10_000_000 {
		t.Fatalf("Браво's claimed tip: %d\n%s", n, books(t, s))
	}
	if balance(t, s, "alfa") != 50_000_000 || balance(t, s, "bravo") != 38_000_000 {
		t.Fatalf("debts with the split and the claim:\n%s", books(t, s))
	}

	// The payout brief and the payouts carry the days from the facts.
	b, err := payroll.PreparePayout(ctx, s.RO, "bravo", switchNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Lines) != 1 || b.Lines[0].Source != payroll.SourceFacts || b.Lines[0].Text != "10–12.11: 0+50+40 = 90 × 20% + фикс 20 = 38" {
		t.Fatalf("Браво's lines %+v", b.Lines)
	}
	if !strings.Contains(b.Message, "10–12.11: 0+50+40 = 90 × 20% + фикс 20 = 38") || !strings.Contains(b.Message, "итого 38") {
		t.Fatalf("message:\n%s", b.Message)
	}
	for _, f := range b.Flags {
		if f.Blocker {
			t.Fatalf("a blocker on a debt from proven facts: %+v", b.Flags)
		}
	}
	a, err := payroll.PreparePayout(ctx, s.RO, "alfa", switchNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range a.Lines {
		lines = append(lines, l.Source+" "+l.Text)
	}
	if got, want := strings.Join(lines, "|"), " 09.11: 80 × 25% = 20|facts 10.11: 120 × 25% = 30"; got != want {
		t.Fatalf("Альфа's lines %q, want %q", got, want)
	}
	p, err := payroll.ReadPayouts(ctx, s.RO, "2026-11-09", "2026-11-15", switchNow)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, pp := range p.People {
		for _, d := range pp.Days {
			days = append(days, fmt.Sprintf("%s %s %s %d", pp.Slug, d.Date, d.Source, d.AmountMicros))
		}
	}
	if got, want := strings.Join(days, "|"), "alfa 2026-11-09 sheet 20000000|alfa 2026-11-10 facts 30000000|"+
		"bravo 2026-11-10 facts 10000000|bravo 2026-11-11 facts 10000000|bravo 2026-11-12 facts 18000000"; got != want {
		t.Fatalf("payouts %s\nwant %s", got, want)
	}

	// Rollback: the setting cleared, the books are the sheets' exactly as before the switch.
	setSwitch(t, s, ctx, "")
	if after := books(t, s); after != before {
		t.Fatalf("rollback:\n%s\nwas\n%s", after, before)
	}
	if n := count(t, s, `SELECT count(*) FROM entries e WHERE source_key LIKE '%:facts' AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`); n != 0 {
		t.Fatalf("%d facts accruals stand after the rollback", n)
	}
}

// A day from the switch on that the facts cannot answer for is never a silent zero: its week unproven, it stays as posted
// and is unknown in «не занесено»; a person no Hub user gets no accrual from the sheet, and is unknown too.
func TestAnUnprovenWeekOrAnUnmappedPersonIsUnknownNotZero(t *testing.T) {
	s, ctx := switchWeek(t)
	setSwitch(t, s, ctx, switchDay)
	alfa, bravo := balance(t, s, "alfa"), balance(t, s, "bravo")

	exec(t, s, `UPDATE hub_team_sync SET status = 'unknown', reasons = '["хаб: 503"]' WHERE window_from = '2026-11-16T00:00:00+03:00'`)
	// What the facts say meanwhile does not move the held days.
	exec(t, s, `UPDATE hub_sales SET payload = json_set(payload, '$.resolution', json('[{"userId": 101, "percent": 100}]'))
		WHERE transaction_ref = '4'`)
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	if _, err := Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	if balance(t, s, "alfa") != alfa || balance(t, s, "bravo") != bravo || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("an unproven week moved the debts:\n%s", books(t, s))
	}
	for _, person := range []string{"alfa", "bravo"} {
		if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = ? AND date = ? AND status = 'unknown'
			AND reason LIKE 'факты хаба не начислили: неделя 2026-11-09 не доказана%хаб: 503%'`, person, switchDay); n != 1 {
			t.Fatalf("%s: no unknown mark:\n%s", person, rows(t, s, `SELECT * FROM payroll_unentered`))
		}
		// The week of the 16th, begun, is not proven either: a mark per tab and week.
		if n := count(t, s, `SELECT unentered_unknown FROM v_payables WHERE person = ?`, person); n != 2 {
			t.Fatalf("%s: v_payables unentered_unknown %d\n%s", person, n, rows(t, s, `SELECT * FROM payroll_unentered`))
		}
	}
	b, err := payroll.PreparePayout(ctx, s.RO, "alfa", switchNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	pending := false
	for _, f := range b.Flags {
		pending = pending || (f.Kind == "facts_pending" && f.Blocker && strings.Contains(f.Text, "не доказана"))
	}
	if !pending {
		t.Fatalf("the brief does not stop on the unknown week: %+v", b.Flags)
	}
	// Proven again: the facts move the day.
	exec(t, s, `UPDATE hub_team_sync SET status = 'ok', reasons = '[]' WHERE window_from = '2026-11-16T00:00:00+03:00'`)
	if _, err := Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	if balance(t, s, "alfa") != alfa+5_000_000 || count(t, s, `SELECT count(*) FROM payroll_unentered WHERE date >= ?`, switchDay) != 0 {
		t.Fatalf("proven again:\n%s", books(t, s))
	}

	// Чарли has a tab with money on the 11th, a card naming the model, and no Hub user: from the switch the sheet does not
	// accrue her, and her week is unknown, not zero.
	if err := payroll.PutPeople(ctx, s.RW, []payroll.Person{{Slug: "charlie", Name: "Чарли", Aliases: []string{}, Status: "active",
		Models: []string{"Alpha"}}}, "test", switchNow); err != nil {
		t.Fatal(err)
	}
	sheets(t, s, ctx, map[string][][]payroll.Cell{
		"Альфа": tab("2026-11-09", 0.25, 0, [7]any{80, nil, nil, nil, nil, nil, nil}),
		"Браво": tab("2026-11-09", 0.2, 10, [7]any{nil, 0, nil, 50, nil, nil, nil}),
		"Чарли": tab("2026-11-09", 0.25, 0, [7]any{40, nil, 60, nil, nil, nil, nil}),
	})
	if _, err := payroll.Rebuild(ctx, s, switchNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT COALESCE(SUM(amount), 0) FROM payable_days WHERE person = 'charlie'`); n != 10_000_000 {
		t.Fatalf("Чарли: %d, want her 9th from the sheet alone\n%s", n, books(t, s))
	}
	if n := count(t, s, `SELECT count(*) FROM payroll_unentered WHERE person = 'charlie' AND date = ? AND status = 'unknown'
		AND reason LIKE '%не сопоставлен с пользователем хаба%'`, switchDay); n != 1 {
		t.Fatalf("Чарли: no unknown mark:\n%s", rows(t, s, `SELECT * FROM payroll_unentered`))
	}
	r := checks.Payables(ctx, checks.Env{Q: s.RO, Now: switchNow})
	if !r.OK {
		t.Fatalf("health with a mirror-only day: %+v", r)
	}
}

func TestTheSwitchDayCannotPrecedeTheRule(t *testing.T) {
	s, ctx := open(t)
	if err := payroll.SetFactsFrom(ctx, s.RW, "2026-11-01", switchNow); err == nil || !strings.Contains(err.Error(), "раньше начала правил") {
		t.Fatalf("a switch before the rule: %v", err)
	}
	if err := payroll.SetFactsFrom(ctx, s.RW, "10.11.2026", switchNow); err == nil {
		t.Fatal("a bad date")
	}
	if from, err := payroll.FactsFrom(ctx, s.RO); err != nil || from != "" {
		t.Fatalf("the switch is off by default: %q %v", from, err)
	}
}
