package accruals

import (
	"encoding/json"
	"testing"

	"github.com/goslingmanagment/ledger/internal/reports"
)

// An amount added to a draft by hand may name the sale it pays (adds[].transaction_ref). The reference is no money: an
// add is in its page by its amount, named or not. On the week of reports_test.go, with the reports accruing and a pot
// share in the day: the drafts saved with bare adds, then the same drafts with the adds naming their sales — new
// versions — leave everything the money is as it was, to the byte: the drafts' totals, pages and lines, the pay base,
// the accruals from the reports, the entries, the debts, the sheets mirror's base and the payouts Hub reads.
func TestAnAddThatNamesItsSaleIsTheSameMoney(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-10") // as production: the posts accrue
	declare(t, s, ctx, "p-one", "Набор один", "Alpha", []string{"page-a"}, "alfa", "bravo")
	potSale(t, s, ctx, "p-one", "tx-pot", "page-a", "2026-11-10T12:00:00+03:00", 20_000)
	// The mirror of Hub's transactions holds both references of the two sales the adds will name.
	for _, id := range []string{"9001", "9002"} {
		exec(t, s, `INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
			VALUES ('hub', ?, 'transactions', '2026-11-10', '2026-11-10', '{}')`, "page-a/transactions/"+id)
		exec(t, s, `INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash, event_id,
			first_seen_at, updated_at) VALUES ('page-a', ?, '2026-11-10T20:00:00.000+03:00', 1000, 'USD', 'message_purchase', ?, '{}', 'h',
			last_insert_rowid(), '2026-11-10', '2026-11-10')`, id, "pf-"+id)
	}
	// Альфа's draft of the 10th equals her post, 100 + 40: page-a is a counted sale and a sale paid after the report was
	// composed; page-b a counted sale and one the script held out and the chatter's word added. Браво's is a counted sale
	// and money Hub does not see. named: Альфа's adds name their sales — by Hub's id, and by the platform's reference of
	// the held-out line.
	drafts := func(named bool) {
		t.Helper()
		add := func(page string, mills int64, what, ref string) map[string]any {
			m := map[string]any{"page": page, "amount_mills": mills, "what": what}
			if named && ref != "" {
				m["transaction_ref"] = ref
			}
			return m
		}
		for _, d := range []map[string]any{
			{"hub_username": "alfa", "total_mills": 140_000,
				"pages": []any{map[string]any{"page": "page-a", "amount_mills": 100_000}, map[string]any{"page": "page-b", "amount_mills": 40_000}},
				"lines": []any{
					map[string]any{"n": 1, "transaction_ref": "pf-1", "page": "page-a", "net_mills": 70_000, "counted": true},
					map[string]any{"n": 2, "transaction_ref": "pf-2", "page": "page-b", "net_mills": 15_000, "counted": true},
					map[string]any{"n": 3, "transaction_ref": "pf-9002", "page": "page-b", "net_mills": 25_000, "counted": false, "flags": []any{"чужой PPV"}},
				},
				"adds": []any{add("page-a", 30_000, "долёт: PPV куплен после отчёта", "9001"), add("page-b", 25_000, "по слову чаттера", "pf-9002")}},
			{"hub_username": "bravo", "hub_user_id": 102, "total_mills": 50_000,
				"pages": []any{map[string]any{"page": "page-a", "amount_mills": 50_000}},
				"lines": []any{map[string]any{"n": 1, "transaction_ref": "pf-3", "page": "page-a", "net_mills": 45_000, "counted": true}},
				"adds":  []any{add("page-a", 5_000, "перевод мимо хаба", "")}},
		} {
			d["report_date"], d["source"] = "2026-11-10", "agent"
			d["window_from"], d["window_to"], d["cut_at"] = "2026-11-10T16:00:00+03:00", "2026-11-11T00:00:00+03:00", "2026-11-10T23:50:00+03:00"
			raw, _ := json.Marshal(d)
			if _, err := reports.Save(ctx, s, raw, "agent", reportNow); err != nil {
				t.Fatal(err)
			}
		}
		// As POST /v1/reports does after a save: the accruals from the reports and the debts follow.
		if err := reports.Changed(ctx, s, reportNow); err != nil {
			t.Fatal(err)
		}
	}
	// money is everything the accruals are, and the current drafts as the pay base sees them — all but which version it
	// is and the sale an add names.
	money := func() string {
		t.Helper()
		return everything(t, s, ctx) + "\n==\n" +
			rows(t, s, `SELECT hub_username, person, report_date, status, total_micros, pages, lines, adds FROM v_shift_reports
				ORDER BY hub_username, report_date`) + "\n==\n" +
			rows(t, s, `SELECT r.hub_username, p.seq, p.page_label, p.amount_micros FROM shift_report_pages p
				JOIN v_shift_reports r ON r.id = p.report_id ORDER BY r.hub_username, p.seq`) + "\n==\n" +
			rows(t, s, `SELECT r.hub_username, l.seq, l.n, l.page_label, l.net_micros, l.counted, l.added, l.what FROM shift_report_lines l
				JOIN v_shift_reports r ON r.id = l.report_id ORDER BY r.hub_username, l.seq`) + "\n==\n" +
			rows(t, s, `SELECT person, report_date, page, model, posted_micros, draft_micros, diff_micros, state, posted_at, post_id,
				hub_micros, hub_diff_micros, hub_state, basis, accrue_micros FROM v_report_pages ORDER BY person, report_date, page`)
	}
	added := func() string {
		t.Helper()
		return cols(t, s, `SELECT r.hub_username, r.version, r.status, l.page_label, l.net_micros, COALESCE(l.transaction_ref, '∅')
			FROM shift_report_lines l JOIN v_shift_reports r ON r.id = l.report_id WHERE l.added = 1 ORDER BY r.hub_username, l.seq`)
	}

	drafts(false)
	if got, want := added(), "alfa 1 posted page-a 30000000 ∅\nalfa 1 posted page-b 25000000 ∅\nbravo 1 posted page-a 5000000 ∅"; got != want {
		t.Fatalf("the adds, bare:\n%s\nwant\n%s", got, want)
	}
	// Альфа's 10th: (140 + her half of the pot's 20) × 25%; Браво's: (50 + 10) × 20% and the fix.
	if a, b := dayOf(t, s, "alfa", "2026-11-10"), dayOf(t, s, "bravo", "2026-11-10"); a != "37500000 0 37500000 reports" || b != "22000000 0 22000000 reports" {
		t.Fatalf("the days: Альфа %s, Браво %s", a, b)
	}
	before := money()

	drafts(true)
	if got, want := added(), "alfa 2 posted page-a 30000000 9001\nalfa 2 posted page-b 25000000 pf-9002\nbravo 1 posted page-a 5000000 ∅"; got != want {
		t.Fatalf("the adds, their sales named:\n%s\nwant\n%s", got, want)
	}
	if after := money(); after != before {
		t.Fatalf("an add that names its sale moved the money:\n%s\nwas\n%s", after, before)
	}
}
