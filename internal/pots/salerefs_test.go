package pots_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/pots"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// One sale, two references. Hub names a sale by its own id — its facts, its decisions and a report handed in in Hub
// carry "7001" —, the platform by the reference of the transaction, and that is what the agent's report of a post
// carries: "pf-7001". The mirror of Hub's transactions holds both. The sales here have both, as production's do; the
// tests before these gave both sides one string and never saw the two kinds meet.

// hubFact puts a sale into ledger's mirror of Hub's sales (hub_sales), which names it by Hub's id.
func hubFact(t *testing.T, s *db.Store, id, page, at string, mills int64) {
	t.Helper()
	if _, err := s.RW.Exec(`INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at,
		post_tip, campaign, payload, first_seen_at, read_at) VALUES (?, '2026-11-09T00:00:00+03:00', ?, 'fansly', 'ppv', ?, ?, 0, 0, ?,
		'2026-11-12', '2026-11-12')`, id, page, mills, at, `{"fanRef": "fan-`+id+`"}`); err != nil {
		t.Fatal(err)
	}
}

// hubTransaction puts the sale into the mirror of Hub's transactions, the one row that holds both of its references:
// Hub's id (key) and the platform's reference (ref; "" — the row has none).
func hubTransaction(t *testing.T, s *db.Store, id, platformRef, page, at string, mills int64) {
	t.Helper()
	res, err := s.RW.Exec(`INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
		VALUES ('hub', ?, 'transactions', ?, ?, '{}')`, page+"/transactions/"+id, at, at)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := res.LastInsertId()
	var ref any
	if platformRef != "" {
		ref = platformRef
	}
	if _, err := s.RW.Exec(`INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash,
		event_id, first_seen_at, updated_at) VALUES (?, ?, ?, ?, 'USD', 'message_purchase', ?, '{}', 'h', ?, ?, ?)`,
		page, id, at, mills, ref, event, at, at); err != nil {
		t.Fatal(err)
	}
}

// hubSale is a sale both of Hub's mirrors have: the fact under Hub's id and the transaction with both references.
func hubSale(t *testing.T, s *db.Store, id, platformRef, page, at string, mills int64) {
	t.Helper()
	hubFact(t, s, id, page, at, mills)
	hubTransaction(t, s, id, platformRef, page, at, mills)
}

// potDecision puts Hub's active decision «в общак» of a sale, named by Hub's id, into the mirror of Hub's decisions
// the way the job hub-decisions leaves it; read is when it came.
func potDecision(t *testing.T, s *db.Store, id int64, sale, pot string, read time.Time) {
	t.Helper()
	at := ts.Format(read)
	if _, err := s.RW.Exec(`INSERT INTO hub_sale_decisions(id, transaction_ref, page_label, kind, pot, state, source, decided_at, seq,
		change, changed_at, read_at) VALUES (?, ?, 'page-a', 'pot', ?, 'active', 'agent', ?, ?, 'made', ?, ?)`,
		id, sale, pot, at, id, at, at); err != nil {
		t.Fatal(err)
	}
}

// line is a sale of the agent's report of a post: named by the platform's reference.
type line struct {
	n       int64
	ref, at string
	mills   int64
	counted bool
}

// potFlag is the flag the agent's script puts on a sale it holds out of the total as a pot's.
const potFlag = "Общак: набор, не в итоге"

// post saves the agent's report of anna's post of a day: the lines on page-a, the page's amount what they count. A
// line held out of the total carries the pot's flag.
func post(t *testing.T, s *db.Store, date string, lines ...line) int64 {
	t.Helper()
	return postWith(t, s, date, nil, lines...)
}

// late is an amount added to a report by hand: a sale paid after the report was composed. ref names the sale — by
// either of its two references — or is "": nothing ties the amount to a sale, as before an add could name one.
type late struct {
	ref   string
	mills int64
}

// postWith is post with amounts added by hand: they are in the page's amount beside the counted lines.
func postWith(t *testing.T, s *db.Store, date string, adds []late, lines ...line) int64 {
	t.Helper()
	var total int64
	out, added := []any{}, []any{}
	for _, l := range lines {
		flags := []any{}
		if l.counted {
			total += l.mills
		} else {
			flags = append(flags, potFlag)
		}
		out = append(out, map[string]any{"n": l.n, "transaction_ref": l.ref, "page": "page-a", "fan_ref": "fan-1", "fan_name": "nick-1",
			"occurred_at": l.at, "net_mills": l.mills, "counted": l.counted, "flags": flags})
	}
	for _, a := range adds {
		total += a.mills
		add := map[string]any{"page": "page-a", "amount_mills": a.mills, "what": "долёт: куплено после отчёта"}
		if a.ref != "" {
			add["transaction_ref"] = a.ref
		}
		added = append(added, add)
	}
	raw, _ := json.Marshal(map[string]any{"hub_username": "anna_hub", "report_date": date, "window_from": date + "T00:00:00+03:00",
		"window_to": date + "T23:59:00+03:00", "cut_at": date + "T23:50:00+03:00", "source": "agent", "total_mills": total,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": total}}, "lines": out, "adds": added})
	saved, err := reports.Save(context.Background(), s, raw, "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	return saved.ID
}

// inPots are the purchases that are in a pot now: «pot reference», in the order they were written.
func inPots(t *testing.T, s *db.Store) string {
	t.Helper()
	rows, err := s.RO.Query(`SELECT pot, COALESCE(transaction_ref, '∅') FROM pot_sales WHERE removed_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pot, ref string
		if err := rows.Scan(&pot, &ref); err != nil {
			t.Fatal(err)
		}
		out = append(out, pot+" "+ref)
	}
	return strings.Join(out, ", ")
}

func switchDay(t *testing.T, s *db.Store, day string) {
	t.Helper()
	if err := decisions.SetHubFrom(context.Background(), s.RW, day, now); err != nil {
		t.Fatal(err)
	}
}

// handPot is a second pot of page-a, for a sale that sits in another pot.
func handPot() pots.DeclareInput {
	return pots.DeclareInput{Slug: "hand-pot", Name: "Другой набор", Model: "Alpha", Pages: []string{"page-a"}, StartsAt: "2026-11-01",
		DeclaredBy: "владелец", DeclaredAt: "2026-11-01", Participants: []pots.Participant{{Person: "anna"}}}
}

// Defect 1, the job. A person's post counts a sale — the agent's report names it by the platform's reference — and Hub's
// decision «в общак» names the same sale by Hub's id. The guard against paying a sale twice must know them as one
// sale: no purchase while the report counts it, and the purchase once the report's next version holds it out.
func TestHubsPotDecisionSeesTheSaleAPostCounts(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	switchDay(t, s, "2026-11-11")
	hubSale(t, s, "7101", "pf-7101", "page-a", "2026-11-12T10:00:00.000+03:00", 10_000)
	post(t, s, "2026-11-12", line{n: 1, ref: "pf-7101", at: "2026-11-12T10:00:00+03:00", mills: 10_000, counted: true})
	potDecision(t, s, 1, "7101", "set-one", now)

	rep, err := pots.FollowHub(ctx, s, now)
	if err != nil || rep.Added != 0 || len(rep.Problems) != 1 {
		t.Fatalf("a sale the post still pays to its person went to the pot (paid twice): added %d, purchases [%s], problems %+v, %v",
			rep.Added, inPots(t, s), rep.Problems, err)
	}
	if why := rep.Problems[0].Why; !strings.Contains(why, "продажа 7101 учтена в отчёте") || !strings.Contains(why, "(anna, 2026-11-12) строкой 1") ||
		!strings.Contains(why, "иначе оплачена дважды") {
		t.Fatalf("the problem: %q", why)
	}
	// The report's next version holds the sale out: the purchase is written, once, under the reference Hub named it by.
	post(t, s, "2026-11-12", line{n: 1, ref: "pf-7101", at: "2026-11-12T10:00:00+03:00", mills: 10_000})
	if rep, err = pots.FollowHub(ctx, s, now.Add(5*time.Minute)); err != nil || rep.Added != 1 || len(rep.Problems) != 0 {
		t.Fatalf("after the report let the sale go: %+v %v", rep, err)
	}
	if rep, err = pots.FollowHub(ctx, s, now.Add(10*time.Minute)); err != nil || rep.Added != 0 || rep.Kept != 1 {
		t.Fatalf("a second pass: %+v %v", rep, err)
	}
	if got := inPots(t, s); got != "set-one 7101" {
		t.Fatalf("purchases: %s", got)
	}
}

// Defect 1, by hand, both ways round: a sale named by Hub's id that a post counts by the platform's reference, and a
// sale named by the platform's reference that a report handed in in Hub counts by Hub's id, stay their people's unless
// forced.
func TestAddSaleSeesTheReportThatCountsTheSaleByTheOtherReference(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	hubSale(t, s, "7001", "pf-7001", "page-a", "2026-11-05T10:00:00.000+03:00", 10_000)
	hubSale(t, s, "7002", "pf-7002", "page-a", "2026-11-05T11:00:00.000+03:00", 20_000)
	saved := post(t, s, "2026-11-05", line{n: 1, ref: "pf-7001", at: "2026-11-05T10:00:00+03:00", mills: 10_000, counted: true})
	for _, q := range []string{
		`INSERT INTO hub_shift_reports(id, shift_id, version, person, hub_person, hub_user_id, report_date, submitted_at, door, total_micros,
			current, payload, received_at, stored_at) VALUES (501, 51, 1, 'boris', 'boris', 42, '2026-11-05', '2026-11-05T21:00:00+03:00',
			'workboard', 20000000, 1, '{}', '2026-11-05T21:01:00+03:00', '2026-11-05T21:01:00+03:00')`,
		`INSERT INTO hub_shift_report_lines(report_id, seq, transaction_ref, page_label, grp, net_micros, rule_share_micros, amount_micros,
			counted, added, disputed) VALUES (501, 1, '7002', 'page-a', 'mine', 20000000, 20000000, 20000000, 1, 0, 0)`,
	} {
		if _, err := s.RW.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// Hub's id against the post's line.
	in := pots.SaleInput{TransactionRef: "7001"}
	_, err := add(t, s, ctx, in)
	refusal(t, err, "продажа 7001 учтена в отчёте "+itoa(saved)+" (anna, 2026-11-05) строкой 1")
	in.Force = true
	if out, err := add(t, s, ctx, in); err != nil || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "учтена в отчёте "+itoa(saved)) {
		t.Fatalf("forced %+v %v", out, err)
	}
	// The platform's reference against the line of a report handed in in Hub.
	in = pots.SaleInput{TransactionRef: "pf-7002", PageLabel: "page-a", OccurredAt: "2026-11-05T11:00:00+03:00", NetMills: mills(20_000)}
	_, err = add(t, s, ctx, in)
	refusal(t, err, "продажа pf-7002 учтена в отчёте, сданном в хабе: версия 501 (boris, 2026-11-05)")
	// Named by the platform's reference, the sale is still read from Hub's mirror: nothing but the reference is needed.
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "pf-7002"})
	refusal(t, err, "учтена в отчёте, сданном в хабе")
	if got := inPots(t, s); got != "set-one 7001" {
		t.Fatalf("purchases: %s", got)
	}
}

// An amount added to a report by hand that names its sale pays that sale to the person as a counted line does — most
// adds are sales of Hub paid after the report was composed. The guard against paying a sale twice sees it, by hand and
// in the job, whichever of the sale's two references the add carries and whichever the pot is asked with; an add that
// names no sale guards nothing, as every add did before.
func TestAnAddThatNamesItsSaleGuardsItFromThePot(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	switchDay(t, s, "2026-11-11")
	hubSale(t, s, "7701", "pf-7701", "page-a", "2026-11-05T10:00:00.000+03:00", 10_000) // before the switch day: a pot's by hand
	hubSale(t, s, "7702", "pf-7702", "page-a", "2026-11-12T10:00:00.000+03:00", 20_000) // from the switch day: by Hub's decision
	hubSale(t, s, "7703", "pf-7703", "page-a", "2026-11-05T11:00:00.000+03:00", 30_000) // held out as the pot's, then added by word
	// anna's report of the 12th pays her the three by adds: one named by the platform's reference, two by Hub's id — and
	// the third is also a line of the window, held out of the total.
	held := line{n: 1, ref: "pf-7703", at: "2026-11-05T11:00:00+03:00", mills: 30_000}
	saved := postWith(t, s, "2026-11-12", []late{{"pf-7701", 10_000}, {"7702", 20_000}, {"7703", 30_000}}, held)
	counted := " учтена в отчёте " + itoa(saved) + " (anna, 2026-11-12) добавленной суммой"

	// By hand: asked by either reference, and from the held-out line whose sale the add names.
	_, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "7701"})
	refusal(t, err, "продажа 7701"+counted)
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "pf-7701"})
	refusal(t, err, "продажа pf-7701"+counted)
	_, err = add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 1}})
	refusal(t, err, "продажа pf-7703"+counted)
	// The job: Hub's decision gives the sale to the pot while the report pays it to its person.
	potDecision(t, s, 1, "7702", "set-one", now)
	rep, err := pots.FollowHub(ctx, s, now)
	if err != nil || rep.Added != 0 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0].Why, "продажа 7702"+counted) ||
		!strings.Contains(rep.Problems[0].Why, "иначе оплачена дважды") {
		t.Fatalf("a sale an add pays to its person went to the pot (paid twice): added %d, purchases [%s], problems %+v, %v",
			rep.Added, inPots(t, s), rep.Problems, err)
	}
	if got := inPots(t, s); got != "" {
		t.Fatalf("purchases: %s", got)
	}

	// The report's next version adds the same amounts and names no sale: nothing ties them to the sales, and the guard
	// has nothing to see — the purchases are written.
	postWith(t, s, "2026-11-12", []late{{"", 10_000}, {"", 20_000}, {"", 30_000}}, held)
	if rep, err = pots.FollowHub(ctx, s, now.Add(5*time.Minute)); err != nil || rep.Added != 1 || len(rep.Problems) != 0 {
		t.Fatalf("after the adds stopped naming the sales: %+v %v", rep, err)
	}
	if _, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "7701"}); err != nil {
		t.Fatalf("by hand, after the add stopped naming the sale: %v", err)
	}
	if got := inPots(t, s); got != "set-one 7702, set-one 7701" {
		t.Fatalf("purchases: %s", got)
	}
}

// Defect 2, the job. A sale put in a pot by hand from a report's line is there under the platform's reference; Hub's
// decision names it by Hub's id. In the same pot it is in place — not bought a second time; in another pot it is a
// problem, as it is when both carry one reference.
func TestHubsPotDecisionSeesThePurchaseMadeByHandUnderTheOtherReference(t *testing.T) {
	s, ctx := book(t)
	for _, in := range []pots.DeclareInput{pot(), handPot()} {
		if _, err := pots.Declare(ctx, s, in, "owner", now); err != nil {
			t.Fatal(err)
		}
	}
	hubSale(t, s, "7201", "pf-7201", "page-a", "2026-11-12T10:00:00.000+03:00", 10_000)
	hubSale(t, s, "7202", "pf-7202", "page-a", "2026-11-12T11:00:00.000+03:00", 20_000)
	saved := post(t, s, "2026-11-12", line{n: 1, ref: "pf-7201", at: "2026-11-12T10:00:00+03:00", mills: 10_000},
		line{n: 2, ref: "pf-7202", at: "2026-11-12T11:00:00+03:00", mills: 20_000})
	// By hand, before the switch day was set.
	if _, err := add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pots.AddSale(ctx, s, "hand-pot", pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 2}}, pots.SourceAgent, "agent", now); err != nil {
		t.Fatal(err)
	}
	switchDay(t, s, "2026-11-11")
	potDecision(t, s, 1, "7201", "set-one", now)
	potDecision(t, s, 2, "7202", "set-one", now)

	rep, err := pots.FollowHub(ctx, s, now)
	if err != nil || rep.Added != 0 {
		t.Fatalf("a sale that is in a pot already was bought again: added %d, purchases [%s], %v", rep.Added, inPots(t, s), err)
	}
	if rep.Kept != 1 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0].Why, "продажа 7202 уже в общаке hand-pot (покупка 2, записана вручную), "+
		"а решение хаба 2 отдаёт её общаку set-one: снимите покупку — ledger pot remove-sale 2") {
		t.Fatalf("kept %d, problems %+v", rep.Kept, rep.Problems)
	}
	if got := inPots(t, s); got != "set-one pf-7201, hand-pot pf-7202" {
		t.Fatalf("purchases: %s", got)
	}
}

// Defect 2, by hand, both ways round: a sale in a pot under one of its references is not added again under the other —
// neither by --tx nor from a report's line.
func TestAddSaleSeesThePurchaseUnderTheOtherReference(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	hubSale(t, s, "7301", "pf-7301", "page-a", "2026-11-05T10:00:00.000+03:00", 10_000)
	hubSale(t, s, "7302", "pf-7302", "page-a", "2026-11-05T11:00:00.000+03:00", 20_000)
	saved := post(t, s, "2026-11-05", line{n: 1, ref: "pf-7301", at: "2026-11-05T10:00:00+03:00", mills: 10_000},
		line{n: 2, ref: "pf-7302", at: "2026-11-05T11:00:00+03:00", mills: 20_000})
	// In the pot under Hub's id; asked again by the platform's reference and from the line that carries it.
	if _, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "7301"}); err != nil {
		t.Fatal(err)
	}
	_, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "pf-7301", PageLabel: "page-a", OccurredAt: "2026-11-05T10:00:00+03:00", NetMills: mills(10_000)})
	refusal(t, err, "продажа pf-7301 уже в общаке set-one (покупка 1, записана под другой ссылкой той же продажи — 7301)")
	_, err = add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 1}})
	refusal(t, err, "уже в общаке set-one (покупка 1")
	// In the pot from the line, under the platform's reference; asked again by Hub's id — alone, and beside the line,
	// which is no contradiction: the two references name one sale.
	if _, err := add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 2}}); err != nil {
		t.Fatal(err)
	}
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "7302"})
	refusal(t, err, "продажа 7302 уже в общаке set-one (покупка 2, записана под другой ссылкой той же продажи — pf-7302)")
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "7302", FromLine: &pots.LineRef{ReportID: saved, N: 2}})
	refusal(t, err, "уже в общаке set-one (покупка 2)")
	// Another sale's reference beside the line is a contradiction still.
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "7301", FromLine: &pots.LineRef{ReportID: saved, N: 2}})
	refusal(t, err, `transaction_ref: в запросе "7301", а у строки`)
	if got := inPots(t, s); got != "set-one 7301, set-one pf-7302" {
		t.Fatalf("purchases: %s", got)
	}
}

// Defect 3. From the switch day's sales only Hub's decision gives a sale to a pot: by hand it is refused whichever
// reference names it — the platform's by --tx, or a report's line, which carries the platform's — and the refusal says
// what to do instead, with Hub's id of the sale. A sale of a day before the switch, and money Hub does not see, are
// added by hand as before.
func TestAddSaleRefusesASaleOfTheSwitchDayByEitherReferenceAndFromALine(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	switchDay(t, s, "2026-11-11")
	hubSale(t, s, "7401", "pf-7401", "page-a", "2026-11-10T22:00:00.000+03:00", 10_000) // the day before the switch
	hubSale(t, s, "7402", "pf-7402", "page-a", "2026-11-11T00:30:00.000+03:00", 20_000) // the switch day
	saved := post(t, s, "2026-11-10", line{n: 1, ref: "pf-7401", at: "2026-11-10T22:00:00+03:00", mills: 10_000},
		line{n: 2, ref: "pf-7402", at: "2026-11-11T00:30:00+03:00", mills: 20_000})

	_, err := add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 2}})
	refusal(t, err, "продажу хаба 7402 (2026-11-11, ссылка площадки pf-7402) с 2026-11-11 в общак отдаёт только решение в хабе: "+
		"hub sale decide --sale 7402 --kind pot --pot set-one")
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "pf-7402", PageLabel: "page-a", OccurredAt: "2026-11-11T00:30:00+03:00", NetMills: mills(20_000)})
	refusal(t, err, "hub sale decide --sale 7402 --kind pot --pot set-one")
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "7402"})
	refusal(t, err, "продажу хаба 7402 (2026-11-11) с 2026-11-11 в общак отдаёт только решение в хабе: hub sale decide --sale 7402 --kind pot --pot set-one")

	// The line of the day before the switch: by hand, and the purchase keeps the reference the line carries.
	out, err := add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved, N: 1}})
	if err != nil || out.Sale.Source != "report_line" || *out.Sale.TransactionRef != "pf-7401" || out.Sale.NetMicros != 10_000_000 {
		t.Fatalf("the line of the day before the switch: %+v %v", out.Sale, err)
	}
	if _, err := add(t, s, ctx, pots.SaleInput{PageLabel: "page-a", OccurredAt: "2026-11-12T12:00:00+03:00", NetMills: mills(5_000),
		Note: "перевод мимо хаба"}); err != nil {
		t.Fatalf("money Hub does not see: %v", err)
	}
	if got := inPots(t, s); got != "set-one pf-7401, set-one ∅" {
		t.Fatalf("purchases: %s", got)
	}
}

// Defect 4. A report's line whose sale is in the pot already — under Hub's id, where Hub's decision put it — is not
// offered again; and every line offered says Hub's id of its sale and which way it goes to a pot: by Hub's decision
// (the sale is Hub's of the switch day or later) or by hand.
func TestCandidatesKnowTheSaleByBothReferencesAndSayTheWay(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	switchDay(t, s, "2026-11-11")
	hubSale(t, s, "7501", "pf-7501", "page-a", "2026-11-10T22:00:00.000+03:00", 10_000) // before the switch day
	hubSale(t, s, "7502", "pf-7502", "page-a", "2026-11-12T10:00:00.000+03:00", 20_000)
	hubSale(t, s, "7503", "pf-7503", "page-a", "2026-11-12T11:00:00.000+03:00", 30_000)
	post(t, s, "2026-11-12", line{n: 1, ref: "pf-7501", at: "2026-11-10T22:00:00+03:00", mills: 10_000},
		line{n: 2, ref: "pf-7502", at: "2026-11-12T10:00:00+03:00", mills: 20_000},
		line{n: 3, ref: "pf-7503", at: "2026-11-12T11:00:00+03:00", mills: 30_000},
		line{n: 4, ref: "pf-7504", at: "2026-11-12T12:00:00+03:00", mills: 40_000}) // a sale Hub's mirrors do not have
	potDecision(t, s, 1, "7503", "set-one", now)
	if rep, err := pots.FollowHub(ctx, s, now); err != nil || rep.Added != 1 {
		t.Fatalf("the purchase of 7503: %+v %v", rep, err)
	}
	c, err := pots.Candidates(ctx, s.RO, "set-one")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range c {
		// The new fields as the answer carries them.
		b, _ := json.Marshal(x)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		id, _ := m["hub_sale"].(string)
		got = append(got, strings.Join([]string{itoa(*x.N), *x.TransactionRef, "hub_sale=" + id, "hub_decides=" + jsonText(m["hub_decides"])}, " "))
	}
	if len(c) != 3 {
		t.Fatalf("the line of a sale that is in the pot under Hub's id is offered again: %s", strings.Join(got, " | "))
	}
	want := []string{
		"1 pf-7501 hub_sale=7501 hub_decides=false", // by hand: --from-line
		"2 pf-7502 hub_sale=7502 hub_decides=true",  // hub sale decide --sale 7502 --kind pot
		"4 pf-7504 hub_sale= hub_decides=false",     // ledger's mirrors of Hub do not know the sale
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("candidates:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The rule of the wait. While the mirror of Hub's transactions does not hold a sale — or holds it without the
// platform's reference — its other reference is not known and the guards cannot look for it: the decision is not acted
// on. It waits like a decision whose sale has not come, is a problem after the same three hours, and once the mirror
// has the sale the guards decide: never a purchase written without them.
func TestHubsPotDecisionWaitsWhileTheSalesOtherReferenceIsUnknown(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	switchDay(t, s, "2026-11-11")
	// The team's facts have the three sales; the mirror of transactions has none of the first two yet, and the third
	// without the platform's reference.
	hubFact(t, s, "7601", "page-a", "2026-11-12T10:00:00.000+03:00", 10_000)
	hubFact(t, s, "7602", "page-a", "2026-11-12T11:00:00.000+03:00", 20_000)
	hubFact(t, s, "7603", "page-a", "2026-11-12T12:00:00.000+03:00", 30_000)
	hubTransaction(t, s, "7603", "", "page-a", "2026-11-12T12:00:00.000+03:00", 30_000)
	// A post counts the first sale to its person.
	post(t, s, "2026-11-12", line{n: 1, ref: "pf-7601", at: "2026-11-12T10:00:00+03:00", mills: 10_000, counted: true})
	for i, sale := range []string{"7601", "7602", "7603"} {
		potDecision(t, s, int64(i+1), sale, "set-one", now)
	}

	rep, err := pots.FollowHub(ctx, s, now.Add(5*time.Minute))
	if err != nil || rep.Added != 0 {
		t.Fatalf("a purchase was written while the sale's other reference is unknown — without the guard: added %d, purchases [%s], %v",
			rep.Added, inPots(t, s), err)
	}
	if rep.Waiting != 3 || len(rep.Problems) != 0 {
		t.Fatalf("waiting %d, problems %+v", rep.Waiting, rep.Problems)
	}
	plan, err := pots.PlanHub(ctx, s.RO, now.Add(5*time.Minute))
	if err != nil || len(plan.Waiting) != 3 || !strings.Contains(plan.Waiting[0].Why, "у продажи 7601 ещё нет ссылки площадки в зеркале транзакций хаба (hub_transactions)") {
		t.Fatalf("the plan while waiting: %+v %v", plan, err)
	}
	// Three hours on it is a problem of the check, still no purchase.
	plan, err = pots.PlanHub(ctx, s.RO, now.Add(4*time.Hour))
	if err != nil || len(plan.Waiting) != 0 || len(plan.Add) != 0 || len(plan.Problems) != 3 ||
		!strings.Contains(plan.Problems[1].Why, "у продажи 7602 больше 3 часов после решения нет ссылки площадки в зеркале транзакций хаба") {
		t.Fatalf("after the wait: %+v %v", plan, err)
	}
	// The hourly read brings the first two: the one the post counts is the guard's problem, the other is bought.
	hubTransaction(t, s, "7601", "pf-7601", "page-a", "2026-11-12T10:00:00.000+03:00", 10_000)
	hubTransaction(t, s, "7602", "pf-7602", "page-a", "2026-11-12T11:00:00.000+03:00", 20_000)
	rep, err = pots.FollowHub(ctx, s, now.Add(time.Hour))
	if err != nil || rep.Added != 1 || rep.Waiting != 1 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0].Why, "продажа 7601 учтена в отчёте") {
		t.Fatalf("once the mirror has the sales: %+v %v", rep, err)
	}
	if got := inPots(t, s); got != "set-one 7602" {
		t.Fatalf("purchases: %s", got)
	}
}
