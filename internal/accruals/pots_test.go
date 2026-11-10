package accruals

import (
	"context"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/pots"
)

// The pots (R3) in the day's revenue, on the synthetic agency of reports_test.go: a share of a pot of the model is
// revenue of the person's tab beside the posted pages, paid at the tab's rate; a day with a share only gets no fix.

func declare(t *testing.T, s *db.Store, ctx context.Context, slug, name, model string, pages []string, people ...string) {
	t.Helper()
	in := pots.DeclareInput{Slug: slug, Name: name, Model: model, Pages: pages, StartsAt: "2026-11-09", DeclaredBy: "владелец",
		DeclaredAt: "2026-11-09"}
	for _, p := range people {
		in.Participants = append(in.Participants, pots.Participant{Person: p})
	}
	if _, err := pots.Declare(ctx, s, in, "owner", reportNow); err != nil {
		t.Fatal(err)
	}
}

func potSale(t *testing.T, s *db.Store, ctx context.Context, slug, ref, page, at string, mills int64) {
	t.Helper()
	if _, err := pots.AddSale(ctx, s, slug, pots.SaleInput{TransactionRef: ref, PageLabel: page, OccurredAt: at, NetMills: &mills},
		pots.SourceAgent, "agent", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := pots.Changed(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
}

func TestAPotShareIsRevenueOfTheTabAtItsRate(t *testing.T) {
	s, ctx := reportBook(t)
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.25, 0, [7]any{nil, nil, nil, nil, nil, nil, nil}),
			"Браво": pageTab("2026-11-09", "Page A", 0.2, 10, [7]any{nil, nil, nil, nil, nil, nil, nil})},
		"Beta": {"Альфа": pageTab("2026-11-09", "Page C", 0.3, 0, [7]any{nil, nil, nil, nil, nil, nil, nil})},
	})
	post(t, s, 1, "502", "2026-11-10", postText("2026-11-10", "", "A - $50"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	before, entries := books(t, s), count(t, s, `SELECT count(*) FROM entries`)

	declare(t, s, ctx, "p-one", "Набор один", "Alpha", []string{"page-a"}, "alfa", "bravo")
	declare(t, s, ctx, "p-two", "Набор два", "Alpha", []string{"page-b"}, "bravo")
	declare(t, s, ctx, "p-beta", "Набор бета", "Beta", []string{"page-c"}, "alfa", "bravo")
	potSale(t, s, ctx, "p-one", "tx-1", "page-a", "2026-11-10T12:00:00+03:00", 30_000) // 15 each
	potSale(t, s, ctx, "p-two", "tx-2", "page-b", "2026-11-10T13:00:00+03:00", 20_000) // Браво's
	potSale(t, s, ctx, "p-one", "tx-3", "page-a", "2026-11-11T12:00:00+03:00", 10_000) // a day nobody posted
	potSale(t, s, ctx, "p-beta", "tx-4", "page-c", "2026-11-12T12:00:00+03:00", 40_000)

	want := strings.Join([]string{
		// A pot-only day: the share at the rate.
		"alfa Alpha 2026-11-10 15000000 3750000 Альфа 15000000 0",
		"alfa Alpha 2026-11-11 5000000 1250000 Альфа 5000000 0",
		"alfa Beta 2026-11-12 20000000 6000000 Альфа 20000000 0",
		// The posted page plus two pots: (50 + 15 + 20) × 20% + the fix once.
		"bravo Alpha 2026-11-10 85000000 27000000 Браво 35000000 10000000",
		// The share only: no fix.
		"bravo Alpha 2026-11-11 5000000 1000000 Браво 5000000 0",
		// No tab of the pot's model: listed, not accrued.
		"bravo Beta 2026-11-12 20000000 -1 - 20000000 0",
	}, "\n")
	got := cols(t, s, `SELECT person, model, day, net_micros, COALESCE(amount_micros, -1), COALESCE(tab_name, '-'), pot_micros, fix_micros
		FROM report_accruals ORDER BY person, day, model`)
	if got != want {
		t.Fatalf("report_accruals:\n%s\nwant\n%s", got, want)
	}
	if got := cols(t, s, `SELECT pots, pages FROM report_accruals WHERE person = 'bravo' AND day = '2026-11-10'`); got !=
		`[{"pot":"p-one","name":"Набор один","micros":15000000},{"pot":"p-two","name":"Набор два","micros":20000000}] [{"page":"page-a","micros":50000000}]` {
		t.Fatalf("Браво's 10th: %s", got)
	}
	if n := count(t, s, `SELECT count(*) FROM report_accruals WHERE person = 'bravo' AND model = 'Beta' AND reason LIKE 'нет вкладки%'`); n != 1 {
		t.Fatal("no reason for the share without a tab")
	}
	// Shadow: the switch is off, not a posting more, the debts as they were.
	if after := books(t, s); after != before || count(t, s, `SELECT count(*) FROM entries`) != entries {
		t.Fatalf("the pots moved the books:\n%s\nwas\n%s", after, before)
	}
	// The diff says the pot part on the day.
	d, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, p := range d.People {
		for _, x := range p.Days {
			for _, n := range x.PotNotes {
				notes = append(notes, p.Person+" "+x.Day+" "+n)
			}
		}
	}
	if got, want := strings.Join(notes, "|"), "alfa 2026-11-10 в т.ч. общак Набор один: база 15.00|alfa 2026-11-11 в т.ч. общак Набор один: база 5.00|"+
		"alfa 2026-11-12 в т.ч. общак Набор бета: база 20.00|bravo 2026-11-10 в т.ч. общак Набор один: база 15.00|"+
		"bravo 2026-11-10 в т.ч. общак Набор два: база 20.00|bravo 2026-11-11 в т.ч. общак Набор один: база 5.00|"+
		"bravo 2026-11-12 в т.ч. общак Набор бета: база 20.00"; got != want {
		t.Fatalf("diff notes:\n%s\nwant\n%s", got, want)
	}
	// Removed, the share is gone from the day.
	sh, err := pots.Show(ctx, s.RO, "p-two")
	if err != nil || len(sh.Sales) != 1 {
		t.Fatalf("show %+v %v", sh, err)
	}
	if _, err := pots.RemoveSale(ctx, s, sh.Sales[0].ID, pots.RemoveInput{Note: "не тот набор"}, "agent", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := pots.Changed(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	if got := cols(t, s, `SELECT net_micros, amount_micros, pot_micros FROM report_accruals WHERE person = 'bravo' AND day = '2026-11-10'`); got != "65000000 23000000 15000000" {
		t.Fatalf("after remove: %s", got)
	}
}

// potWeek is reportWeek with a pot of Alpha's page-a shared by both: 20 on the 10th (10 each), 8 on the 11th, a day
// nobody posted (4 each).
func potWeek(t *testing.T, s *db.Store, ctx context.Context) {
	t.Helper()
	declare(t, s, ctx, "p-one", "Набор один", "Alpha", []string{"page-a"}, "alfa", "bravo")
	potSale(t, s, ctx, "p-one", "tx-1", "page-a", "2026-11-10T12:00:00+03:00", 20_000)
	potSale(t, s, ctx, "p-one", "tx-2", "page-a", "2026-11-11T12:00:00+03:00", 8_000)
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
}

func TestUnderTheReportsSwitchThePotSharesArePostedAndOffTheBooksReturn(t *testing.T) {
	s, ctx := reportWeek(t)
	before, entries := books(t, s), count(t, s, `SELECT count(*) FROM entries`)
	potWeek(t, s, ctx)
	if after := books(t, s); after != before || count(t, s, `SELECT count(*) FROM entries`) != entries {
		t.Fatalf("the switch is unset and the pots moved the books:\n%s\nwas\n%s", after, before)
	}

	setReports(t, s, ctx, "2026-11-10")
	for _, c := range []struct{ person, date, want string }{
		{"alfa", "2026-11-10", "37500000 0 37500000 reports"},  // (140 + 10) × 25%
		{"alfa", "2026-11-11", "1000000 0 1000000 reports"},    // 4 × 25%, no report: the share alone
		{"bravo", "2026-11-10", "22000000 0 22000000 reports"}, // (50 + 10) × 20% + the fix
		{"bravo", "2026-11-11", "800000 0 800000 reports"},     // 4 × 20%, no fix
	} {
		if got := dayOf(t, s, c.person, c.date); got != c.want {
			t.Fatalf("%s %s: %s, want %s\n%s", c.person, c.date, got, c.want, books(t, s))
		}
	}
	b, err := payroll.PreparePayout(ctx, s.RO, "alfa", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range b.Lines {
		lines = append(lines, l.Text)
	}
	if got, want := strings.Join(lines, "|"), "09.11: 80 × 25% = 20|10–11.11: 150+4 = 154 × 25% = 38.50 (в т.ч. общак Набор один 10.11: база 10; общак Набор один 11.11: база 4)"; got != want {
		t.Fatalf("Альфа's lines\n%q\nwant\n%q", got, want)
	}
	if !strings.Contains(b.Message, "в т.ч. общак набор один 10.11: база 10") {
		t.Fatalf("message: %s", b.Message)
	}
	p, err := payroll.ReadPayouts(ctx, s.RO, "2026-11-09", "2026-11-15", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, pp := range p.People {
		for _, d := range pp.Days {
			if pp.Slug == "alfa" && d.Date == "2026-11-11" && (d.Worked || len(d.Pots) != 1 || d.Pots[0].RevenueMicros != 4_000_000) {
				t.Fatalf("the pot-only day in payouts: %+v", d)
			}
		}
	}

	setReports(t, s, ctx, "")
	if after := books(t, s); after != before {
		t.Fatalf("off: the books are not the sheets':\n%s\nwas\n%s", after, before)
	}
}

func TestAPotShareOnAPaidDayWaitsForTheOwner(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "50.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	fifo := books(t, s)
	declare(t, s, ctx, "p-one", "Набор один", "Alpha", []string{"page-a"}, "alfa", "bravo")
	potSale(t, s, ctx, "p-one", "tx-1", "page-a", "2026-11-10T12:00:00+03:00", 20_000)
	// The 10th was paid in part: the share waits, 35 → (140 + 10) × 25%; Браво's unpaid 10th follows at once.
	if got := pendingRows(t, s); got != "1 alfa 2026-11-10 35000000 37500000 post_edited open  " {
		t.Fatalf("pending:\n%s", got)
	}
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "35000000 30000000 5000000 reports" {
		t.Fatalf("the paid day moved: %s\n%s\nwas\n%s", got, books(t, s), fifo)
	}
	if got := dayOf(t, s, "bravo", "2026-11-10"); got != "22000000 0 22000000 reports" {
		t.Fatalf("Браво's unpaid 10th: %s", got)
	}
}
