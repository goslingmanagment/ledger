package accruals

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/payroll"
)

// One person's pay under the reports (GET /v1/payouts/person): a day of the reports names the posts it came from, the
// FIFO of payouts says what of it is paid and by which row, and the debt is the one of v_payables.
func TestPersonPayNamesThePostsOfAReportsDay(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10")
	// A payout of 50 pays the 9th (20, the sheet's) and 30 of the 10th (35, the report's).
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "50.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	postOf := func(msg int) int64 {
		return count(t, s, `SELECT id FROM shift_report_posts WHERE msg_id = ? AND current = 1`, msg)
	}
	render := func(a payroll.PersonPayAnswer) string {
		var out []string
		for _, d := range a.Days {
			out = append(out, fmt.Sprintf("%s %s %s %d %v %s %d %d %v", d.Date, d.Model, d.Source, d.AmountMicros, d.Posts, d.State,
				d.PaidMicros, d.UnpaidMicros, d.PaidBy))
		}
		return strings.Join(out, "\n")
	}

	a, err := payroll.ReadPersonPay(ctx, s.RO, "alfa", "2026-11-09", "2026-11-15", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("2026-11-09 Alpha sheet 20000000 [] paid 20000000 0 [1]\n"+
		"2026-11-10 Alpha reports 35000000 [%d] to_pay 30000000 5000000 [1]", postOf(1))
	if got := render(a); got != want {
		t.Fatalf("Альфа's days\n%s\nwant\n%s", got, want)
	}
	if a.BalanceMicros != balance(t, s, "alfa") || a.BalanceMicros != 5_000_000 || a.AccruedMicros != 55_000_000 || a.PaidMicros != 50_000_000 ||
		a.ReportsFrom != "2026-11-10" || !reflect.DeepEqual(a.Payouts, []payroll.PersonPayout{{Date: "2026-11-12", Document: 1, AmountMicros: 50_000_000}}) {
		t.Fatalf("Альфа %+v", a)
	}

	// Браво: the 10th from her post; the 12th has no post and keeps the sheet's posting, a day still to pay.
	b, err := payroll.ReadPersonPay(ctx, s.RO, "bravo", "2026-11-09", "2026-11-15", reportNow)
	if err != nil {
		t.Fatal(err)
	}
	want = fmt.Sprintf("2026-11-10 Alpha reports 20000000 [%d] to_pay 0 20000000 []\n"+
		"2026-11-12 Alpha sheet 20000000 [] to_pay 0 20000000 []", postOf(2))
	if got := render(b); got != want || b.BalanceMicros != balance(t, s, "bravo") || b.LastPayout != nil || len(b.Payouts) != 0 {
		t.Fatalf("Браво's days\n%s\nwant\n%s\n%+v", got, want, b)
	}
}
