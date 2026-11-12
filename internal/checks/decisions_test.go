package checks

import (
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// «Решения хаба о продажах доходят»: while settings.sale_decisions_hub_from is empty nothing reads Hub's decisions and the
// check never fails; from the day it is set it fails for a feed not read, a change Hub holds back from ledger's key and a
// pot decision that cannot become a purchase — an unknown pot is a problem, not a silent skip.
func TestHubDecisionsCheck(t *testing.T) {
	s, ctx := openDB(t)
	check := func() Result { return HubDecisions(ctx, Env{Q: s.RO, Now: now}) }
	read := func(ok bool, lastOK string) {
		t.Helper()
		v := `{"at": "` + ts.Format(now) + `", "ok": true, "since": "3", "next": "5", "lastOkAt": "` + lastOK + `"}`
		if !ok {
			v = `{"at": "` + ts.Format(now) + `", "ok": false, "error": "хаб ответил 503", "since": "5", "next": "5", "lastOkAt": "` + lastOK + `"}`
		}
		exec(t, s, `INSERT INTO cursors(name, value, updated_at) VALUES ('hub:sale_decisions:status', ?, ?)
			ON CONFLICT(name) DO UPDATE SET value = excluded.value`, v, ts.Format(now))
	}
	// Never read, the setting empty: a line, not a failure.
	if r := check(); !r.OK || KeyOf(r.Name) != "hub_decisions" || !strings.Contains(r.Summary, "в тени") || !strings.Contains(r.Summary, "ещё не читалась") {
		t.Fatalf("never read, in shadow: %+v", r)
	}
	// The mirror holds a pot decision of an unknown pot on a sale of the 3rd, and Hub holds a change back. The sale is in
	// both of Hub's mirrors: the fact under Hub's id and the transaction with the platform's reference too.
	exec(t, s, `INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at, post_tip, campaign,
		payload, first_seen_at, read_at) VALUES ('7001', '2026-11-02T00:00:00+03:00', 'page-a', 'fansly', 'ppv', 10000,
		'2026-11-03T10:00:00.000+03:00', 0, 0, '{}', '2026-11-03', '2026-11-03')`)
	exec(t, s, `INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
		VALUES ('hub', 'page-a/transactions/7001', 'transactions', '2026-11-03T10:00:00.000+03:00', '2026-11-03', '{}')`)
	exec(t, s, `INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash, event_id,
		first_seen_at, updated_at) VALUES ('page-a', '7001', '2026-11-03T10:00:00.000+03:00', 10000, 'USD', 'message_purchase', 'pf-7001',
		'{}', 'h', last_insert_rowid(), '2026-11-03', '2026-11-03')`)
	exec(t, s, `INSERT INTO hub_sale_decisions(id, transaction_ref, page_label, kind, pot, state, source, decided_at, seq, change, changed_at, read_at)
		VALUES (4, '7001', 'page-a', 'pot', 'no-such-pot', 'active', 'agent', '2026-11-03T11:00:00+03:00', 5, 'made',
		'2026-11-03T11:00:00+03:00', '2026-11-03T11:05:00+03:00')`)
	exec(t, s, `INSERT INTO hub_sale_decisions_held(seq, decision_id, page_label, reason, first_seen_at)
		VALUES (2, 15, 'page-x', 'page_outside_key', '2026-11-03T11:05:00+03:00'), (3, 16, NULL, 'page_outside_key', '2026-11-03T11:05:00+03:00')`)
	read(true, ts.Format(now.Add(-10*60*1e9)))
	if r := check(); !r.OK || !strings.Contains(r.Summary, "в тени") || !strings.Contains(r.Summary, "решение 15 (page-x)") {
		t.Fatalf("in shadow nothing fails: %+v", r)
	}

	// From the switch day.
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-02", now); err != nil {
		t.Fatal(err)
	}
	r := check()
	if r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "решения хаба читаются с 2026-11-02, но") ||
		!strings.Contains(r.Summary, "2 изменений решений хаб придержал") || !strings.Contains(r.Summary, "решение 15 (page-x), решение 16 (страница не названа)") ||
		!strings.Contains(r.Summary, "1 решений хаба «в общак» не стали покупками общака: решение 4: общака «no-such-pot» нет в ledger") {
		t.Fatalf("held and the unknown pot: %+v", r)
	}
	// The held changes come, the decision is cancelled: the check passes.
	exec(t, s, `UPDATE hub_sale_decisions_held SET cleared_at = '2026-11-03T12:00:00+03:00'`)
	exec(t, s, `UPDATE hub_sale_decisions SET state = 'cancelled'`)
	if r := check(); !r.OK || !strings.Contains(r.Summary, "решения хаба читаются у продаж с 2026-11-02") || !strings.Contains(r.Summary, "курсор 5") {
		t.Fatalf("all done: %+v", r)
	}
	// The feed is not read: a failure pass, then a read too old.
	read(false, ts.Format(now.Add(-10*60*1e9)))
	if r := check(); r.OK || !strings.Contains(r.Summary, "лента решений хаба не прочитана") || !strings.Contains(r.Summary, "хаб ответил 503") {
		t.Fatalf("a failed read: %+v", r)
	}
	read(true, ts.Format(now.Add(-2*3600*1e9)))
	if r := check(); r.OK || !strings.Contains(r.Summary, "не читалась больше часа") {
		t.Fatalf("a stale read: %+v", r)
	}
}
