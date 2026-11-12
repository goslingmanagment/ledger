package decisions_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/pots"
	"github.com/goslingmanagment/ledger/internal/reports"
)

// One sale, two references: Hub's id — what Hub's facts and decisions carry — and the platform's reference of the
// transaction — what the agent's report of a post carries. The mirror of Hub's transactions holds both.

// hubFact puts a sale into ledger's mirror of Hub's sales (hub_sales), under Hub's id.
func hubFact(t *testing.T, s *db.Store, ref int64, page, at string, mills int64) {
	t.Helper()
	id := strconv.FormatInt(ref, 10)
	if _, err := s.RW.Exec(`INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at,
		post_tip, campaign, payload, first_seen_at, read_at) VALUES (?, '2026-11-09T00:00:00+03:00', ?, 'fansly', 'ppv', ?, ?, 0, 0, ?,
		'2026-11-12', '2026-11-12')`, id, page, mills, at, `{"fanRef": "fan-`+id+`"}`); err != nil {
		t.Fatal(err)
	}
}

// hubTransaction puts the sale into the mirror of Hub's transactions, the one row that holds both of its references:
// Hub's id (key) and the platform's reference (ref).
func hubTransaction(t *testing.T, s *db.Store, ref int64, platformRef, page, at string, mills int64) {
	t.Helper()
	id := strconv.FormatInt(ref, 10)
	res, err := s.RW.Exec(`INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
		VALUES ('hub', ?, 'transactions', ?, ?, '{}')`, page+"/transactions/"+id, at, at)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := res.LastInsertId()
	if _, err := s.RW.Exec(`INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash,
		event_id, first_seen_at, updated_at) VALUES (?, ?, ?, ?, 'USD', 'message_purchase', ?, '{}', 'h', ?, ?, ?)`,
		page, id, at, mills, platformRef, event, at, at); err != nil {
		t.Fatal(err)
	}
}

// The review's reproduction, end to end. A sale of the switch day is a pot's line of a person's post: the agent's
// report holds it out of the total under the platform's reference. The agent records Hub's decision «в общак», which
// names the sale by Hub's id. The sale goes to the pot once: the job writes one purchase, `pot candidates` shows the
// line as Hub's to decide and then no longer offers it, `pot add-sale --from-line` of the line is refused before and
// after with Hub's command, and the shares are counted once.
func TestASaleOfAPostGoesToThePotOnceByHubsDecision(t *testing.T) {
	s, ctx, k := book(t)
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-11", now); err != nil {
		t.Fatal(err)
	}
	hubFact(t, s, 7003, "page-a", "2026-11-12T10:00:00.000+03:00", 30_000)
	hubTransaction(t, s, 7003, "pf-7003", "page-a", "2026-11-12T10:00:00.000+03:00", 30_000)
	raw, _ := json.Marshal(map[string]any{
		"hub_username": "anna_hub", "report_date": "2026-11-12", "window_from": "2026-11-12T08:00:00+03:00",
		"window_to": "2026-11-12T16:00:00+03:00", "cut_at": "2026-11-12T15:50:00+03:00", "source": "agent", "total_mills": 0,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 0}},
		"lines": []any{map[string]any{"n": 1, "transaction_ref": "pf-7003", "page": "page-a", "fan_ref": "fan-7003", "fan_name": "nick",
			"occurred_at": "2026-11-12T10:00:00+03:00", "net_mills": 30000, "counted": false, "flags": []any{"Общак: набор, не в итоге"}}},
	})
	saved, err := reports.Save(ctx, s, raw, "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	candidates := func() string {
		t.Helper()
		list, err := pots.Candidates(ctx, s.RO, "set-one")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range list {
			b, _ := json.Marshal(c) // the fields as the answer carries them
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			out = append(out, strings.Join([]string{itoa(c.ReportID) + ":" + itoa(*c.N), *c.TransactionRef, "hub_sale=" + jsonText(m["hub_sale"]),
				"hub_decides=" + jsonText(m["hub_decides"])}, " "))
		}
		return strings.Join(out, " | ")
	}
	fromLine := pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved.ID, N: 1}}

	// Before the decision: the line is a candidate that goes by Hub's decision, and is not added by hand.
	offered := candidates()
	_, byHand := pots.AddSale(ctx, s, "set-one", fromLine, pots.SourceAgent, "agent", now)
	// The agent records the decision in Hub; the job writes the purchase.
	feed, c := newDecisionFeed(t, "page-a")
	feed.put(1, "made", potDecision(7003, "set-one"))
	first := syncHub(t, s, c, now).Pots.(pots.FollowReport)
	// After it: not a candidate, still not added by hand — beside the database and over HTTP; a second pass writes nothing.
	left := candidates()
	_, again := pots.AddSale(ctx, s, "set-one", fromLine, pots.SourceAgent, "agent", now)
	overHTTP := k.agent.Do(ctx, http.MethodPost, "/v1/pots/set-one/sales", fromLine, nil)
	second := syncHub(t, s, c, now.Add(5*time.Minute)).Pots.(pots.FollowReport)

	if got, want := purchases(t, s), "1 set-one 7003 2026-11-12 30000000 agent hub-decisions 1 решение хаба 1 ∅ ∅"; got != want {
		t.Fatalf("the sale must be in the pot once, by Hub's decision:\n%s\nwant\n%s", got, want)
	}
	if got, want := potShares(t, s), "7003 anna 15000000\n7003 boris 15000000"; got != want {
		t.Fatalf("the shares:\n%s\nwant\n%s", got, want)
	}
	if got, want := table(t, s, `SELECT person, day, pot_micros FROM report_accruals ORDER BY person, day`),
		"anna 2026-11-12 15000000\nboris 2026-11-12 15000000"; got != want {
		t.Fatalf("the shares in the accruals:\n%s\nwant\n%s", got, want)
	}
	if first.Added != 1 || len(first.Problems) != 0 || second.Added != 0 || second.Kept != 1 {
		t.Fatalf("the passes: %+v, then %+v", first, second)
	}
	const command = "hub sale decide --sale 7003 --kind pot --pot set-one"
	refusal(t, byHand, command)
	refusal(t, again, command)
	if status(overHTTP) != http.StatusUnprocessableEntity || !strings.Contains(message(overHTTP), command) {
		t.Fatalf("over HTTP: %v", overHTTP)
	}
	if want := itoa(saved.ID) + `:1 pf-7003 hub_sale="7003" hub_decides=true`; offered != want {
		t.Fatalf("the candidate before the decision: %s, want %s", offered, want)
	}
	if left != "" {
		t.Fatalf("the line is offered again though its sale is in the pot: %s", left)
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
