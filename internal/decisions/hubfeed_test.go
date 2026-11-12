package decisions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/pots"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// decisionFeed answers Hub's feed of decisions the way Hub serves it (hub#1280): every change by its place (seq), `limit`
// counting places, each change with the decision as it is now. A change of a decision on a page outside the key's pages
// (or with no page) is held back — kept for the key and listed in `held` on every read. Once the key may read it Hub
// gives the change a new place at the end of the feed, and it is served by the ordinary cursor. `next` is the last place
// read, held or not.
type decisionFeed struct {
	mu        sync.Mutex
	decisions map[int64]map[string]any
	changes   []*feedChange
	grant     map[string]bool
	kept      map[*feedChange]bool // the changes held back for the key
	status    int
	requests  []string
}

type feedChange struct {
	seq, id    int64
	change, at string
}

func newDecisionFeed(t *testing.T, grant ...string) (*decisionFeed, *hub.Client) {
	t.Helper()
	f := &decisionFeed{decisions: map[int64]map[string]any{}, grant: map[string]bool{}, kept: map[*feedChange]bool{}, status: http.StatusOK}
	for _, p := range grant {
		f.grant[p] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "test-key"}, nil)
}

func (f *decisionFeed) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.RawQuery)
	if r.Method != http.MethodGet || r.URL.Path != hub.SaleDecisionsPath || r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
		return
	}
	if f.status != http.StatusOK {
		w.WriteHeader(f.status)
		fmt.Fprintf(w, `{"error":"status_%d","message":"stub"}`, f.status)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	readable := func(c *feedChange) bool {
		page, _ := f.decisions[c.id]["pageLabel"].(string)
		return page != "" && f.grant[page]
	}
	// A change kept for the key that it may read now gets a new place at the end of the feed.
	last := int64(0)
	for _, c := range f.changes {
		last = max(last, c.seq)
	}
	for _, c := range f.changes {
		if f.kept[c] && readable(c) {
			last++
			c.seq = last
			delete(f.kept, c)
		}
	}
	sort.Slice(f.changes, func(i, j int) bool { return f.changes[i].seq < f.changes[j].seq })
	served, held, read, next := []any{}, []any{}, 0, since
	for _, c := range f.changes {
		if c.seq <= since || read >= limit {
			continue
		}
		read, next = read+1, c.seq
		if readable(c) {
			served = append(served, map[string]any{"seq": c.seq, "change": c.change, "at": c.at, "decision": f.decisions[c.id]})
		} else {
			f.kept[c] = true
		}
	}
	for _, c := range f.changes {
		if f.kept[c] {
			held = append(held, map[string]any{"seq": c.seq, "decisionId": c.id, "pageLabel": f.decisions[c.id]["pageLabel"], "reason": "page_outside_key"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"changes": served, "held": held,
		"next": strconv.FormatInt(next, 10), "serverNow": "2026-11-12T09:00:00.000Z"})
}

// put changes a decision — creates it with a «made», or sets the fields given — and places the change next in the feed.
func (f *decisionFeed) put(id int64, change string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.decisions[id]
	if d == nil {
		d = map[string]any{"id": id, "transactionId": nil, "pageLabel": "page-a", "kind": "owner", "shares": []any{}, "pot": nil,
			"outcome": "said", "ruleHome": nil, "state": "active", "source": "agent", "saidBy": "владелец", "saidAt": nil,
			"decidedAt": "2026-11-12T08:00:00.000Z", "note": nil, "applied": nil, "ledgerDecisionId": nil}
		f.decisions[id] = d
	}
	for k, v := range fields {
		d[k] = v
	}
	seq := int64(1)
	for _, c := range f.changes {
		seq = max(seq, c.seq+1)
	}
	f.changes = append(f.changes, &feedChange{seq: seq, id: id, change: change,
		at: time.Date(2026, 11, 12, 8, 0, len(f.changes)+1, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")})
}

// potDecision is the fields of a decision that gives a sale to a pot.
func potDecision(sale int64, pot string) map[string]any {
	return map[string]any{"transactionId": sale, "kind": "pot", "pot": pot, "ruleHome": "shift"}
}

func syncHub(t *testing.T, s *db.Store, c *hub.Client, at time.Time) decisions.HubStatus {
	t.Helper()
	st, err := decisions.SyncHub(context.Background(), s, c, at)
	if err != nil {
		t.Fatalf("the decisions feed: %+v %v", st, err)
	}
	return st
}

func table(t *testing.T, s *db.Store, q string, args ...any) string {
	t.Helper()
	rows, err := s.RO.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				v = "∅"
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return strings.Join(out, "\n")
}

// hubSale puts a sale into ledger's mirrors of Hub as the hourly reads leave it: the team's fact under Hub's id
// (hub_sales) and the transaction with both of the sale's references — Hub's id and the platform's, "pf-<id>"
// (hub_transactions).
func hubSale(t *testing.T, s *db.Store, ref int64, page, at string, mills int64) {
	t.Helper()
	hubFact(t, s, ref, page, at, mills)
	hubTransaction(t, s, ref, "pf-"+strconv.FormatInt(ref, 10), page, at, mills)
}

// Every change of a decision is taken in the feed's order and the mirror holds each decision as it is now; a change Hub
// holds back from ledger's key is named until Hub releases it, and then comes like any other.
func TestHubsDecisionsAreMirroredAndAHeldChangeIsNamedUntilItComes(t *testing.T) {
	s, _, _ := book(t)
	ctx := context.Background()
	feed, c := newDecisionFeed(t, "page-a")
	// 1: the owner gives sale 7001 to anna. 2: a decision on a page outside the key. 3: a pot decision, then cancelled.
	feed.put(1, "made", map[string]any{"transactionId": 7001, "shares": []any{map[string]any{"person": "anna", "userId": 6, "percent": 100}},
		"ruleHome": "nobody", "saidAt": "2026-11-12T06:00:00.000Z", "note": "ничья, продала Анна", "source": "dispute", "outcome": "first"})
	feed.put(2, "made", map[string]any{"transactionId": 7002, "pageLabel": "page-x", "kind": "exclude"})
	feed.put(3, "made", potDecision(7003, "set-one"))
	feed.put(3, "cancelled", map[string]any{"state": "cancelled"})
	feed.put(4, "made", map[string]any{"kind": "off_platform", "ledgerDecisionId": 12, "source": "ledger", "outcome": "imported",
		"applied": map[string]any{"person": "boris", "reportDate": "2026-11-10"}})

	st := syncHub(t, s, c, now)
	if !st.OK || st.Read != 4 || st.Changed != 4 || st.Held != 1 || st.Next != "5" {
		t.Fatalf("first pass %+v", st)
	}
	if got, want := table(t, s, `SELECT id, COALESCE(transaction_ref, '∅'), page_label, kind, shares, COALESCE(pot, '∅'), outcome,
		COALESCE(rule_home, '∅'), state, source, COALESCE(said_by, '∅'), COALESCE(said_at, '∅'), decided_at, COALESCE(note, '∅'),
		COALESCE(applied_person, '∅'), COALESCE(applied_report_date, '∅'), COALESCE(ledger_decision_id, '∅'), seq, change
		FROM hub_sale_decisions ORDER BY id`), strings.Join([]string{
		`1 7001 page-a owner [{"person":"anna","userId":6,"percent":100}] ∅ first nobody active dispute владелец 2026-11-12T09:00:00+03:00 2026-11-12T11:00:00+03:00 ничья, продала Анна ∅ ∅ ∅ 1 made`,
		// Two changes of decision 3 came in one answer, each with the decision as it is now: cancelled, at the later place.
		`3 7003 page-a pot [] set-one said shift cancelled agent владелец ∅ 2026-11-12T11:00:00+03:00 ∅ ∅ ∅ ∅ 4 cancelled`,
		`4 ∅ page-a off_platform [] ∅ imported ∅ active ledger владелец ∅ 2026-11-12T11:00:00+03:00 ∅ boris 2026-11-10 12 5 made`,
	}, "\n"); got != want {
		t.Fatalf("the mirror:\n%s\nwant\n%s", got, want)
	}
	held, err := decisions.HubHeld(ctx, s.RO)
	if err != nil || len(held) != 1 || held[0].Seq != 2 || held[0].DecisionID != 2 || held[0].PageLabel == nil || *held[0].PageLabel != "page-x" {
		t.Fatalf("held %+v %v", held, err)
	}
	// Nothing new: nothing is written, the held change stays the same row.
	if st := syncHub(t, s, c, now.Add(5*time.Minute)); st.Read != 0 || st.Changed != 0 || st.Held != 1 || st.Next != "5" {
		t.Fatalf("second pass %+v", st)
	}
	// A new change while one is held is read; then the key gets the page: Hub gives the held change a new place, 7.
	feed.put(1, "applied", map[string]any{"applied": map[string]any{"person": "anna", "reportDate": "2026-11-12"}})
	if st := syncHub(t, s, c, now.Add(10*time.Minute)); st.Read != 1 || st.Changed != 1 || st.Held != 1 || st.Next != "6" {
		t.Fatalf("third pass %+v", st)
	}
	feed.mu.Lock()
	feed.grant["page-x"] = true
	feed.mu.Unlock()
	if st := syncHub(t, s, c, now.Add(15*time.Minute)); st.Read != 1 || st.Changed != 1 || st.Held != 0 || st.Next != "7" {
		t.Fatalf("fourth pass %+v", st)
	}
	if got, want := table(t, s, `SELECT id, transaction_ref, page_label, kind, state, COALESCE(applied_person, '∅'), seq, change
		FROM hub_sale_decisions WHERE id IN (1, 2) ORDER BY id`), "1 7001 page-a owner active anna 6 applied\n2 7002 page-x exclude active ∅ 7 made"; got != want {
		t.Fatalf("after the held change came:\n%s\nwant\n%s", got, want)
	}
	if got, want := table(t, s, `SELECT seq, decision_id, cleared_at FROM hub_sale_decisions_held`), "2 2 "+ts.Format(now.Add(15*time.Minute)); got != want {
		t.Fatalf("held rows: %q, want %q", got, want)
	}
}

// Hub's `limit` counts places, held ones too: a page that serves fewer changes than asked is not the end of the feed. One
// held change must not stop the changes placed after it from being read.
func TestAHeldChangeDoesNotStopTheDecisionsFeed(t *testing.T) {
	s, _, _ := book(t)
	defer decisions.SetHubLimit(2)()
	feed, c := newDecisionFeed(t, "page-a")
	feed.put(1, "made", map[string]any{"transactionId": 7001})
	feed.put(2, "made", map[string]any{"transactionId": 7002, "pageLabel": "page-x"}) // outside the key
	feed.put(3, "made", map[string]any{"transactionId": 7003})
	feed.put(4, "made", map[string]any{"transactionId": 7004})
	feed.put(5, "made", map[string]any{"transactionId": 7005})
	st := syncHub(t, s, c, now)
	if st.Read != 4 || st.Changed != 4 || st.Held != 1 || st.Next != "5" {
		t.Fatalf("the changes after the held one are not read: %+v", st)
	}
	if got, want := table(t, s, `SELECT id FROM hub_sale_decisions ORDER BY id`), "1\n3\n4\n5"; got != want {
		t.Fatalf("the mirror:\n%s\nwant\n%s", got, want)
	}
}

// A Hub without the feed (404) is «nothing yet» before the feed was ever read and a failure after; a refused key fails.
func TestTheDecisionsFeedAbsentAndGone(t *testing.T) {
	s, _, _ := book(t)
	ctx := context.Background()
	feed, c := newDecisionFeed(t, "page-a")
	feed.status = http.StatusNotFound
	st, err := decisions.SyncHub(ctx, s, c, now)
	if err != nil || !st.OK || !st.Absent || !strings.Contains(st.Error, "404") {
		t.Fatalf("404 first: %+v %v", st, err)
	}
	feed.status = http.StatusOK
	feed.put(1, "made", map[string]any{"transactionId": 7001})
	if st := syncHub(t, s, c, now.Add(5*time.Minute)); st.Next != "1" {
		t.Fatalf("read %+v", st)
	}
	feed.status = http.StatusNotFound
	st, err = decisions.SyncHub(ctx, s, c, now.Add(10*time.Minute))
	if err == nil || st.OK || st.Absent || !strings.Contains(st.Error, "курсор 1") || st.LastOKAt != ts.Format(now.Add(5*time.Minute)) {
		t.Fatalf("404 after reading: %+v %v", st, err)
	}
	feed.status = http.StatusForbidden
	if st, err := decisions.SyncHub(ctx, s, c, now.Add(15*time.Minute)); err == nil || st.OK || !strings.Contains(st.Error, "read:money") {
		t.Fatalf("403: %+v %v", st, err)
	}
	saved, err := decisions.ReadHubStatus(ctx, s.RO)
	if err != nil || saved == nil || saved.OK || saved.LastOKAt != ts.Format(now.Add(5*time.Minute)) {
		t.Fatalf("saved %+v %v", saved, err)
	}
}

// potShares are the shares of the pot's purchases as the accruals read them.
func potShares(t *testing.T, s *db.Store) string {
	return table(t, s, `SELECT transaction_ref, person, share_micros FROM v_pot_shares ORDER BY CAST(transaction_ref AS INTEGER), person`)
}

func purchases(t *testing.T, s *db.Store) string {
	return table(t, s, `SELECT id, pot, transaction_ref, day, net_micros, source, key_name, COALESCE(hub_decision_id, '∅'), note,
		COALESCE(removed_by, '∅'), COALESCE(removed_note, '∅') FROM pot_sales ORDER BY id`)
}

// With settings.sale_decisions_hub_from empty Hub's decisions are only kept: no purchase follows a pot decision, the pots
// are written by hand as before, and nothing else moves.
func TestWithTheSettingEmptyHubsDecisionsMoveNoPot(t *testing.T) {
	s, ctx, _ := book(t)
	hubSale(t, s, 7003, "page-a", "2026-11-12T10:00:00.000+03:00", 30_000)
	feed, c := newDecisionFeed(t, "page-a")
	feed.put(3, "made", potDecision(7003, "set-one"))
	st := syncHub(t, s, c, now)
	if rep, ok := st.Pots.(pots.FollowReport); st.Changed != 1 || !ok || rep.From != "" || rep.Added != 0 || len(rep.Problems) != 0 {
		t.Fatalf("pass %+v", st)
	}
	if n := table(t, s, `SELECT COUNT(*) FROM pot_sales`); n != "0" {
		t.Fatalf("a purchase with the setting empty: %s", purchases(t, s))
	}
	plan, err := pots.PlanHub(ctx, s.RO, now)
	if err != nil || plan.From != "" || len(plan.Add)+len(plan.Remove)+len(plan.Problems)+len(plan.Waiting) != 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// By hand, as before.
	if _, err := pots.AddSale(ctx, s, "set-one", pots.SaleInput{TransactionRef: "7003"}, pots.SourceAgent, "agent", now); err != nil {
		t.Fatalf("by hand with the setting empty: %v", err)
	}
	syncHub(t, s, c, now.Add(5*time.Minute))
	if got, want := purchases(t, s), "1 set-one 7003 2026-11-12 30000000 agent agent ∅  ∅ ∅"; got != want {
		t.Fatalf("purchases:\n%s\nwant\n%s", got, want)
	}
}

// From the switch day's sales the pots follow Hub: an active decision of kind pot on a sale ledger's mirror has is a
// purchase of that pot, written by the job; cancelled or superseded, the purchase goes. A sale of a day before the switch
// stays by hand. What cannot become a purchase is a problem, never a silent skip.
func TestFromTheSwitchDayThePotsFollowHubsDecisions(t *testing.T) {
	s, ctx, _ := book(t)
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-11", now); err != nil {
		t.Fatal(err)
	}
	if _, err := pots.Declare(ctx, s, pots.DeclareInput{Slug: "set-two", Name: "Второй набор", Model: "Alpha", Pages: []string{"page-a"},
		StartsAt: "2026-11-01", DeclaredBy: "владелец", DeclaredAt: "2026-11-01",
		Participants: []pots.Participant{{Person: "anna"}}}, "owner", now); err != nil {
		t.Fatal(err)
	}
	hubSale(t, s, 7001, "page-a", "2026-11-10T22:00:00.000+03:00", 10_000) // the day before the switch
	hubSale(t, s, 7002, "page-a", "2026-11-11T00:30:00.000+03:00", 20_001) // the switch day
	hubSale(t, s, 7003, "page-a", "2026-11-12T10:00:00.000+03:00", 30_000)
	hubSale(t, s, 7004, "page-a", "2026-11-12T11:00:00.000+03:00", 40_000)
	feed, c := newDecisionFeed(t, "page-a")
	feed.put(1, "made", potDecision(7001, "set-one"))
	feed.put(2, "made", potDecision(7002, "set-one"))
	feed.put(3, "made", map[string]any{"transactionId": 7003, "kind": "pot", "pot": "set-one", "source": "dispute", "outcome": "nobody"})
	feed.put(4, "made", potDecision(7004, "no-such-pot"))
	feed.put(5, "made", potDecision(7005, "set-one")) // its sale has not reached the mirror yet
	feed.put(6, "made", map[string]any{"transactionId": 7002, "kind": "owner", "state": "superseded"})

	st := syncHub(t, s, c, now)
	rep, ok := st.Pots.(pots.FollowReport)
	if !ok || rep.From != "2026-11-11" || rep.Added != 2 || rep.Removed != 0 || rep.Waiting != 1 || len(rep.Problems) != 1 {
		t.Fatalf("first pass %+v", st)
	}
	// The sale of the 10th is not followed; the 11th and the 12th are: who decided is the source, the job is the key.
	if got, want := purchases(t, s), strings.Join([]string{
		"1 set-one 7002 2026-11-11 20001000 agent hub-decisions 2 решение хаба 2 ∅ ∅",
		"2 set-one 7003 2026-11-12 30000000 owner hub-decisions 3 решение хаба 3 ∅ ∅",
	}, "\n"); got != want {
		t.Fatalf("purchases:\n%s\nwant\n%s", got, want)
	}
	if got, want := potShares(t, s), "7002 anna 10000500\n7002 boris 10000500\n7003 anna 15000000\n7003 boris 15000000"; got != want {
		t.Fatalf("shares:\n%s\nwant\n%s", got, want)
	}
	// The shares are revenue of the day: the accruals from the reports followed the purchases.
	if got, want := table(t, s, `SELECT person, day, pot_micros FROM report_accruals ORDER BY person, day`),
		"anna 2026-11-11 10000500\nanna 2026-11-12 15000000\nboris 2026-11-11 10000500\nboris 2026-11-12 15000000"; got != want {
		t.Fatalf("report_accruals:\n%s\nwant\n%s", got, want)
	}
	if p := rep.Problems[0]; p.DecisionID != 4 || !strings.Contains(p.Why, "общака «no-such-pot» нет в ledger") {
		t.Fatalf("the unknown pot: %+v", rep.Problems)
	}
	// A second pass writes nothing.
	if st := syncHub(t, s, c, now.Add(5*time.Minute)); st.Pots.(pots.FollowReport).Added != 0 || st.Pots.(pots.FollowReport).Kept != 2 {
		t.Fatalf("second pass %+v", st)
	}

	// By hand: a sale Hub sees of the switch day or later is refused with Hub's command; a sale of before the day, and
	// money Hub does not see, are taken as before. A purchase the job wrote is not taken off by hand.
	_, err := pots.AddSale(ctx, s, "set-one", pots.SaleInput{TransactionRef: "7004"}, pots.SourceAgent, "agent", now)
	refusal(t, err, "hub sale decide --sale 7004 --kind pot --pot set-one")
	refusal(t, err, "с 2026-11-11")
	if _, err := pots.AddSale(ctx, s, "set-one", pots.SaleInput{TransactionRef: "7001"}, pots.SourceAgent, "agent", now); err != nil {
		t.Fatalf("a sale of before the switch day by hand: %v", err)
	}
	if _, err := pots.AddSale(ctx, s, "set-one", pots.SaleInput{PageLabel: "page-a", NetMills: mills(5_000),
		OccurredAt: "2026-11-12T12:00:00+03:00", Note: "перевод мимо хаба"}, pots.SourceAgent, "agent", now); err != nil {
		t.Fatalf("money Hub does not see: %v", err)
	}
	_, err = pots.RemoveSale(ctx, s, 1, pots.RemoveInput{Note: "ошибка"}, "agent", now)
	refusal(t, err, "hub sale cancel 2")

	// Hub: decision 2 is cancelled; 3 is superseded by 7 for another pot; the pot of 4 is declared; sale 7005 arrives.
	feed.put(2, "cancelled", map[string]any{"state": "cancelled"})
	feed.put(3, "superseded", map[string]any{"state": "superseded"})
	feed.put(7, "made", potDecision(7003, "set-two"))
	feed.put(4, "changed", map[string]any{"pot": "set-two"})
	hubSale(t, s, 7005, "page-a", "2026-11-12T13:00:00.000+03:00", 50_000)
	st = syncHub(t, s, c, now.Add(10*time.Minute))
	rep = st.Pots.(pots.FollowReport)
	if rep.Added != 3 || rep.Removed != 2 || rep.Waiting != 0 || len(rep.Problems) != 0 {
		t.Fatalf("third pass %+v", st)
	}
	if got, want := purchases(t, s), strings.Join([]string{
		"1 set-one 7002 2026-11-11 20001000 agent hub-decisions 2 решение хаба 2 hub-decisions решение хаба 2 отменено",
		"2 set-one 7003 2026-11-12 30000000 owner hub-decisions 3 решение хаба 3 hub-decisions решение хаба 3 заменено другим",
		"3 set-one 7001 2026-11-10 10000000 agent agent ∅  ∅ ∅",
		"4 set-one ∅ 2026-11-12 5000000 agent agent ∅ перевод мимо хаба ∅ ∅",
		"5 set-two 7004 2026-11-12 40000000 agent hub-decisions 4 решение хаба 4 ∅ ∅",
		"6 set-one 7005 2026-11-12 50000000 agent hub-decisions 5 решение хаба 5 ∅ ∅",
		"7 set-two 7003 2026-11-12 30000000 agent hub-decisions 7 решение хаба 7 ∅ ∅",
	}, "\n"); got != want {
		t.Fatalf("purchases:\n%s\nwant\n%s", got, want)
	}
	// The purchase of an active decision is still not taken off by hand.
	_, err = pots.RemoveSale(ctx, s, 6, pots.RemoveInput{Note: "ошибка"}, "agent", now)
	refusal(t, err, "hub sale cancel 5")

	// A decision superseded by another of the same pot keeps the purchase: it names the new decision.
	feed.put(5, "superseded", map[string]any{"state": "superseded"})
	feed.put(8, "made", potDecision(7005, "set-one"))
	st = syncHub(t, s, c, now.Add(15*time.Minute))
	if rep := st.Pots.(pots.FollowReport); rep.Added != 0 || rep.Removed != 0 {
		t.Fatalf("fourth pass %+v", st)
	}
	if got, want := table(t, s, `SELECT id, hub_decision_id, note, COALESCE(removed_at, '∅') FROM pot_sales WHERE transaction_ref = '7005'`),
		"6 8 решение хаба 8 ∅"; got != want {
		t.Fatalf("the purchase of sale 7005: %q, want %q", got, want)
	}

	// Off: the job follows nothing more, the purchases stay, and the pots are by hand again.
	if err := decisions.SetHubFrom(ctx, s.RW, "", now); err != nil {
		t.Fatal(err)
	}
	feed.put(8, "cancelled", map[string]any{"state": "cancelled"})
	st = syncHub(t, s, c, now.Add(20*time.Minute))
	if rep := st.Pots.(pots.FollowReport); rep.From != "" || rep.Removed != 0 {
		t.Fatalf("after off %+v", st)
	}
	if _, err := pots.RemoveSale(ctx, s, 6, pots.RemoveInput{Note: "решение отменено, переключатель снят"}, "agent", now); err != nil {
		t.Fatalf("by hand after off: %v", err)
	}
}

// What cannot become a purchase stands as a problem and is done once it can: a sale a report still counts in a person's
// own page (it would be paid twice), a sale already in another pot by hand, a closed pot, a page outside the pot, a
// sale that never reaches ledger's mirror.
func TestAPotDecisionThatCannotBecomeAPurchaseIsAProblem(t *testing.T) {
	s, ctx, _ := book(t)
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-11", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.Exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-b', 'fansly', 'Alpha', 'Alpha', '2026-11-01')`); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"closed-pot", "hand-pot"} {
		in := pots.DeclareInput{Slug: slug, Name: slug, Model: "Alpha", Pages: []string{"page-a"}, StartsAt: "2026-11-01",
			DeclaredBy: "владелец", DeclaredAt: "2026-11-01", Participants: []pots.Participant{{Person: "anna"}}}
		if slug == "closed-pot" {
			in.Status = pots.StatusClosed
		}
		if _, err := pots.Declare(ctx, s, in, "owner", now); err != nil {
			t.Fatal(err)
		}
	}
	hubSale(t, s, 7101, "page-a", "2026-11-12T10:00:00.000+03:00", 10_000) // counted in a report handed in in Hub
	hubSale(t, s, 7102, "page-a", "2026-11-12T11:00:00.000+03:00", 20_000) // in another pot by hand
	hubSale(t, s, 7103, "page-a", "2026-11-12T12:00:00.000+03:00", 30_000) // a closed pot
	hubSale(t, s, 7104, "page-b", "2026-11-12T13:00:00.000+03:00", 40_000) // a page outside the pot
	for _, q := range []string{
		`INSERT INTO hub_shift_reports(id, shift_id, version, person, hub_person, hub_user_id, report_date, submitted_at, door, total_micros,
			current, payload, received_at, stored_at) VALUES (501, 51, 1, 'boris', 'boris', 42, '2026-11-12', '2026-11-12T21:00:00+03:00',
			'workboard', 10000000, 1, '{}', '2026-11-12T21:01:00+03:00', '2026-11-12T21:01:00+03:00')`,
		`INSERT INTO hub_shift_report_lines(report_id, seq, transaction_ref, page_label, grp, net_micros, rule_share_micros, amount_micros,
			counted, added, disputed) VALUES (501, 1, '7101', 'page-a', 'mine', 10000000, 10000000, 10000000, 1, 0, 0)`,
		// Written by hand before the switch day was set.
		`INSERT INTO pot_sales(pot, transaction_ref, page_label, net_micros, occurred_at, day, source, added_at, key_name)
			VALUES ('hand-pot', '7102', 'page-a', 20000000, '2026-11-12T11:00:00+03:00', '2026-11-12', 'agent', '2026-11-12T11:05:00+03:00', 'agent')`,
	} {
		if _, err := s.RW.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	feed, c := newDecisionFeed(t, "page-a", "page-b")
	feed.put(1, "made", potDecision(7101, "set-one"))
	feed.put(2, "made", potDecision(7102, "set-one"))
	feed.put(3, "made", potDecision(7103, "closed-pot"))
	feed.put(4, "made", potDecision(7104, "set-one"))
	feed.put(5, "made", potDecision(7105, "set-one"))
	feed.put(6, "made", map[string]any{"transactionId": 7106, "kind": "pot", "pot": nil})
	hubSale(t, s, 7106, "page-a", "2026-11-12T14:00:00.000+03:00", 60_000)

	st := syncHub(t, s, c, now)
	rep := st.Pots.(pots.FollowReport)
	if rep.Added != 0 || rep.Waiting != 1 || len(rep.Problems) != 5 {
		t.Fatalf("first pass %+v", st)
	}
	for i, part := range []string{
		"продажа 7101 учтена в отчёте, сданном в хабе: версия 501 (boris, 2026-11-12): пока отчёт считает её человеку, в общак set-one она не берётся",
		"продажа 7102 уже в общаке hand-pot (покупка 1, записана вручную), а решение хаба 2 отдаёт её общаку set-one: снимите покупку — ledger pot remove-sale 1",
		"общак closed-pot закрыт",
		"решение хаба 4, продажа 7104: страница page-b не в общаке set-one",
		"решение хаба 6 отдаёт продажу 7106 в общак, но не называет общак",
	} {
		if !strings.Contains(rep.Problems[i].Why, part) {
			t.Fatalf("problem %d: %q, want %q", i+1, rep.Problems[i].Why, part)
		}
	}
	// A sale that does not reach the mirror waits three hours, then it is a problem too.
	plan, err := pots.PlanHub(ctx, s.RO, now.Add(4*time.Hour))
	if err != nil || len(plan.Waiting) != 0 || len(plan.Problems) != 6 || !strings.Contains(plan.Problems[4].Why, "продажи 7105 нет в фактах хаба (hub_sales) больше 3 часов") {
		t.Fatalf("after the wait %+v %v", plan, err)
	}
	// Hub's new version of the report no longer counts the sale; the hand purchase is taken off: both are done by the next pass.
	if _, err := s.RW.Exec(`UPDATE hub_shift_report_lines SET amount_micros = 0, counted = 0 WHERE transaction_ref = '7101'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pots.RemoveSale(ctx, s, 1, pots.RemoveInput{Note: "по решению хаба — другой общак"}, "agent", now); err != nil {
		t.Fatal(err)
	}
	st = syncHub(t, s, c, now.Add(5*time.Minute))
	if rep := st.Pots.(pots.FollowReport); rep.Added != 2 || len(rep.Problems) != 3 {
		t.Fatalf("second pass %+v", st)
	}
	if got, want := table(t, s, `SELECT transaction_ref, pot, hub_decision_id FROM pot_sales WHERE removed_at IS NULL ORDER BY transaction_ref`),
		"7101 set-one 1\n7102 set-one 2"; got != want {
		t.Fatalf("purchases:\n%s\nwant\n%s", got, want)
	}
}

// From the switch day a report line's agreement is Hub's decision of the sale: ledger's own decisions are frozen then,
// and a decision recorded in Hub must show there too. A sale of a day before the switch keeps ledger's.
func TestAfterTheSwitchAReportLinesAgreementIsHubsDecision(t *testing.T) {
	s, ctx, k := book(t)
	raw, _ := json.Marshal(map[string]any{
		"hub_username": "anna_hub", "report_date": "2026-11-12", "window_from": "2026-11-10T16:00:00+03:00",
		"window_to": "2026-11-13T00:00:00+03:00", "cut_at": "2026-11-12T23:50:00+03:00", "source": "agent", "total_mills": 25000,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 25000}},
		"lines": []any{
			map[string]any{"n": 1, "transaction_ref": "7001", "page": "page-a", "occurred_at": "2026-11-10T21:15:00+03:00", "net_mills": 25000, "counted": true},
			map[string]any{"n": 2, "transaction_ref": "7002", "page": "page-a", "occurred_at": "2026-11-12T10:00:00+03:00", "net_mills": 1000, "counted": false},
			map[string]any{"n": 3, "transaction_ref": "7003", "page": "page-a", "occurred_at": "2026-11-12T11:00:00+03:00", "net_mills": 2000, "counted": false},
			map[string]any{"n": 4, "transaction_ref": "7004", "page": "page-a", "occurred_at": "2026-11-12T12:00:00+03:00", "net_mills": 3000, "counted": false},
		},
	})
	saved, err := reports.Save(ctx, s, raw, "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	// Ledger's own decisions, written before the switch: on the sale of the 10th and on one of the 12th.
	var own, late decisions.Decision
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", owner("7001"), &own); err != nil {
		t.Fatal(err)
	}
	in := owner("7004")
	in.OccurredAt = "2026-11-12T12:00:00+03:00"
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", in, &late); err != nil {
		t.Fatal(err)
	}
	// Hub's decisions: on the sale of the 10th (another word than ledger's), on two of the 12th — one applied, one cancelled.
	feed, c := newDecisionFeed(t, "page-a")
	feed.put(21, "made", map[string]any{"transactionId": 7001, "kind": "exclude", "note": "хаб: никому"})
	feed.put(22, "made", map[string]any{"transactionId": 7002, "kind": "split", "saidBy": "Анна и Борис", "note": "договорились пополам",
		"shares":  []any{map[string]any{"person": "anna", "userId": 6, "percent": 50}, map[string]any{"person": nil, "userId": 9, "percent": 50}},
		"applied": map[string]any{"person": "anna", "reportDate": "2026-11-12"}})
	feed.put(23, "made", potDecision(7003, "set-one"))
	feed.put(23, "cancelled", map[string]any{"state": "cancelled"})
	syncHub(t, s, c, now)

	agreements := func() string {
		t.Helper()
		rep, err := reports.Get(ctx, s.RO, saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range rep.Lines {
			a := "∅"
			if l.Agreement != nil {
				b, _ := json.Marshal(l.Agreement)
				a = string(b)
			}
			out = append(out, *l.TransactionRef+" "+a)
		}
		return strings.Join(out, "\n")
	}
	ownNote := `"note":"PPV отправил Борис, но продала Анна в свою смену."`
	before := strings.Join([]string{
		`7001 {"id":` + itoa(own.ID) + `,"kind":"owner","shares":[{"person":"anna","percent":100}],"status":"open","said_by":"владелец",` + ownNote + `}`,
		"7002 ∅",
		"7003 ∅",
		`7004 {"id":` + itoa(late.ID) + `,"kind":"owner","shares":[{"person":"anna","percent":100}],"status":"open","said_by":"владелец",` + ownNote + `}`,
	}, "\n")
	// With the setting empty the mirror shows nowhere.
	if got := agreements(); got != before {
		t.Fatalf("with the setting empty:\n%s\nwant\n%s", got, before)
	}
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-11", now); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		// The sale of the 10th is before the switch day: ledger's own decision, as it was.
		`7001 {"id":` + itoa(own.ID) + `,"kind":"owner","shares":[{"person":"anna","percent":100}],"status":"open","said_by":"владелец",` + ownNote + `}`,
		// A sale of the 12th: Hub's active decision, its shares by people's codes; taken into a report — applied.
		`7002 {"id":22,"kind":"split","shares":[{"person":"anna","percent":50},{"person":"","percent":50,"hub_user_id":9}],"status":"applied","said_by":"Анна и Борис","note":"договорились пополам","source":"hub"}`,
		// Hub's cancelled decision is no agreement.
		"7003 ∅",
		// No decision of Hub on the sale: ledger's own row, written before the switch, still stands.
		`7004 {"id":` + itoa(late.ID) + `,"kind":"owner","shares":[{"person":"anna","percent":100}],"status":"open","said_by":"владелец",` + ownNote + `}`,
	}, "\n")
	if got := agreements(); got != want {
		t.Fatalf("after the switch:\n%s\nwant\n%s", got, want)
	}
	// Hub's decision on the sale of 7004 supersedes what ledger held; over HTTP the line reads the same.
	feed.put(24, "made", map[string]any{"transactionId": 7004, "kind": "pot", "pot": "set-one", "note": nil, "saidBy": nil})
	syncHub(t, s, c, now.Add(5*time.Minute))
	var r struct {
		Lines []struct {
			Agreement map[string]any `json:"agreement"`
		} `json:"lines"`
	}
	if err := k.hub.Do(ctx, http.MethodGet, "/v1/reports/"+itoa(saved.ID), nil, &r); err != nil {
		t.Fatal(err)
	}
	if a := r.Lines[3].Agreement; fmt.Sprint(a["id"]) != "24" || a["kind"] != "pot" || a["pot"] != "set-one" || a["source"] != "hub" || a["status"] != "open" {
		t.Fatalf("the line of 7004: %+v", a)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func refusal(t *testing.T, err error, part string) {
	t.Helper()
	var r *decisions.Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Msg, part) {
		t.Fatalf("want a refusal with %q, got %v", part, err)
	}
}
