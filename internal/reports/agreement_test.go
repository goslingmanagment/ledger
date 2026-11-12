package reports

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Defect 5. A line of the agent's report names its sale by the platform's reference ("pf-7002"); Hub's decision about
// whose the sale is names it by Hub's id ("7002"). From the switch day the line's agreement is Hub's decision of its
// sale all the same; and ledger's own decision of a sale of before the switch is the line's whichever of the sale's
// two references it was written with. The mirror of Hub's transactions holds both references.
func TestALinesAgreementIsTheDecisionOfItsSaleByEitherReference(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.RW.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	for _, id := range []string{"7001", "7002", "7003"} {
		exec(`INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
			VALUES ('hub', ?, 'transactions', '2026-11-12', '2026-11-12', '{}')`, "page-a/transactions/"+id)
		exec(`INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash, event_id,
			first_seen_at, updated_at) VALUES ('page-a', ?, '2026-11-12T10:00:00.000+03:00', 1000, 'USD', 'message_purchase', ?, '{}', 'h',
			last_insert_rowid(), '2026-11-12', '2026-11-12')`, id, "pf-"+id)
	}
	saved, err := Save(ctx, s, raw(t, map[string]any{
		"hub_username": "anna_hub", "report_date": "2026-11-12", "window_from": "2026-11-10T16:00:00+03:00",
		"window_to": "2026-11-13T00:00:00+03:00", "cut_at": "2026-11-12T23:50:00+03:00", "source": "agent", "total_mills": 25000,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 25000}},
		"lines": []any{
			map[string]any{"n": 1, "transaction_ref": "pf-7001", "page": "page-a", "occurred_at": "2026-11-10T21:15:00+03:00", "net_mills": 25000, "counted": true},
			map[string]any{"n": 2, "transaction_ref": "pf-7002", "page": "page-a", "occurred_at": "2026-11-12T10:00:00+03:00", "net_mills": 1000, "counted": false},
			map[string]any{"n": 3, "transaction_ref": "pf-7003", "page": "page-a", "occurred_at": "2026-11-12T11:00:00+03:00", "net_mills": 2000, "counted": false},
		},
	}), "agent", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Ledger's own decision on the sale of the 10th, written before the switch with Hub's id of the sale; Hub's
	// decisions: on the same sale (a day before the switch: not read) and on the sale of the 12th.
	exec(`INSERT INTO sale_decisions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, kind, shares, said_by, said_at, note,
		created_at, key_name) VALUES (5, 'page-a', 'fan-1', '', '7001', 25000000, 'owner', '[{"person":"anna","percent":100}]', 'владелец',
		'2026-11-11', 'продала Анна', '2026-11-11', 'agent')`)
	exec(`INSERT INTO hub_sale_decisions(id, transaction_ref, page_label, kind, shares, state, source, said_by, note, applied_person,
		applied_report_date, seq, change, changed_at, read_at) VALUES
		(21, '7001', 'page-a', 'exclude', '[]', 'active', 'agent', 'владелец', 'хаб: никому', NULL, NULL, 1, 'made', '2026-11-12', '2026-11-12'),
		(22, '7002', 'page-a', 'split', '[{"person":"anna","userId":6,"percent":50},{"person":null,"userId":9,"percent":50}]', 'active', 'agent',
		 'Анна и Борис', 'договорились пополам', 'anna', '2026-11-12', 2, 'made', '2026-11-12', '2026-11-12')`)
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-11", ts.Now()); err != nil {
		t.Fatal(err)
	}
	rep, err := Get(ctx, s.RO, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range rep.Lines {
		a := "∅"
		if l.Agreement != nil {
			b, _ := json.Marshal(l.Agreement)
			a = string(b)
		}
		got = append(got, *l.TransactionRef+" "+a)
	}
	want := []string{
		// The sale of the 10th is before the switch day: ledger's own decision, found by the sale's other reference.
		`pf-7001 {"id":5,"kind":"owner","shares":[{"person":"anna","percent":100}],"status":"open","said_by":"владелец","note":"продала Анна"}`,
		// The sale of the 12th: Hub's active decision of it.
		`pf-7002 {"id":22,"kind":"split","shares":[{"person":"anna","percent":50},{"person":"","percent":50,"hub_user_id":9}],"status":"applied","said_by":"Анна и Борис","note":"договорились пополам","source":"hub"}`,
		"pf-7003 ∅",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the lines' agreements:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
