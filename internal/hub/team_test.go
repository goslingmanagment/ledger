package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/split"
)

// fakeTeam is Hub's agentTeamSales over synthetic works and sales: the window's sales, the works around it (a day
// before, 7 days after or Hub's now), the grant's pages, and injectable faults.
type fakeTeam struct {
	now    func() time.Time
	works  []map[string]any
	sales  []map[string]any
	status []fakeStatus     // answers to the next calls, then 200
	fault  map[string]bool  // "holder": a holder not among the works; "window": the answer ends an hour early
	bodies []map[string]any // request bodies
	calls  int
}

func (f *fakeHub) serveTeam(w http.ResponseWriter, r *http.Request) {
	t := &f.team
	t.calls++
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		return
	}
	t.bodies = append(t.bodies, body)
	if len(t.status) > 0 {
		s := t.status[0]
		t.status = t.status[1:]
		w.WriteHeader(s.code)
		json.NewEncoder(w).Encode(map[string]any{"error": s.error, "message": "synthetic failure"})
		return
	}
	from, err1 := time.Parse(time.RFC3339, body["from"].(string))
	to, err2 := time.Parse(time.RFC3339, body["to"].(string))
	if err1 != nil || err2 != nil || !from.Before(to) || to.Sub(from) > 31*24*time.Hour {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"error": "validation", "message": "bad window"})
		return
	}
	now := t.now()
	workFrom, workTo := from.Add(-24*time.Hour), to.Add(7*24*time.Hour)
	if now.Before(workTo) {
		workTo = now
	}
	works := []any{}
	for _, x := range t.works {
		a, b := mustTime(x["startedAt"].(string)), mustTime(x["endedAt"].(string))
		if a.Before(workTo) && b.After(workFrom) {
			works = append(works, x)
		}
	}
	sales := []any{}
	for _, x := range t.sales {
		at := mustTime(x["occurredAt"].(string))
		if at.Before(from) || !at.Before(to) {
			continue
		}
		if t.fault["holder"] {
			c := map[string]any{}
			for k, v := range x {
				c[k] = v
			}
			c["holders"] = append(append([]any{}, x["holders"].([]any)...), "999:2026-11-05T10:00:00.000Z")
			x = c
		}
		sales = append(sales, x)
	}
	pages := []any{}
	for _, p := range f.grant {
		pages = append(pages, p.PageLabel)
	}
	end := to
	if t.fault["window"] {
		end = to.Add(-time.Hour)
	}
	json.NewEncoder(w).Encode(map[string]any{"from": hubTime(from), "to": hubTime(end), "workFrom": hubTime(workFrom),
		"workTo": hubTime(workTo), "pages": pages, "works": works, "sales": sales, "serverNow": hubTime(now)})
}

func teamShift(user int64, kind, from, to string, pages ...string) map[string]any {
	a, b := mustTime(from), mustTime(to)
	ps := []any{}
	for _, p := range pages {
		ps = append(ps, map[string]any{"pageLabel": p, "messages": 10, "salesFrom": hubTime(a)})
	}
	return map[string]any{"ref": refOf(user, a), "userId": user, "username": "user" + itoa(int(user)), "role": "chatter", "kind": kind,
		"startedAt": hubTime(a), "endedAt": hubTime(b), "ongoing": false, "workedMinutes": int(b.Sub(a).Minutes()), "pages": ps}
}

func teamSaleOf(ref, page, kind, at string, net int64, holders ...map[string]any) map[string]any {
	hs := []any{}
	for _, h := range holders {
		hs = append(hs, h["ref"])
	}
	return map[string]any{"transactionRef": ref, "pageLabel": page, "platform": "fansly", "fanRef": "fan-" + ref, "kind": kind,
		"netMills": net, "occurredAt": hubTime(mustTime(at)), "holders": hs, "ppvSender": nil, "ppvSentAt": nil,
		"ppvWithoutAuthor": false, "postTip": false, "campaign": false, "lastWriterUserId": nil, "writers": []any{},
		"marks": []any{}, "resolution": nil}
}

func refOf(user int64, at time.Time) string { return itoa(int(user)) + ":" + hubTime(at) }

func newTeamBench(t *testing.T, now string) (*bench, *TeamSyncer) {
	t.Helper()
	b := newBench(t)
	b.clock.now = mustTime(now)
	b.hub.team.now = b.clock.Now
	b.hub.team.fault = map[string]bool{}
	for _, p := range b.hub.grant {
		b.exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES (?, ?, ?, ?, ?)`,
			p.PageLabel, p.Platform, p.ModelSlug, p.ModelName, now)
	}
	setTeamSince(b, "2026-11-02") // the rule's start; TestTeamSyncReadsTheWeeksAndKeepsEachWindowsSnapshot reads earlier
	sy := NewTeamSyncer(b.store, Config{BaseURL: b.hub.srv.URL, Key: "test-key"})
	sy.Now = b.clock.Now
	sy.Client.Sleep = b.clock.Sleep
	sy.Client.Throttle.Now, sy.Client.Throttle.Sleep = b.clock.Now, b.clock.Sleep
	return b, sy
}

func setTeamSince(b *bench, day string) {
	b.exec(`INSERT INTO settings(key, value, updated_at) VALUES ('hub_team_since', ?, '2026-11-01')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, day)
}

func TestTeamAnswerMapsOntoTheSplitRules(t *testing.T) {
	raw, err := os.ReadFile("testdata/team_sales.json")
	if err != nil {
		t.Fatal(err)
	}
	a, err := decodeTeam(raw)
	if err != nil {
		t.Fatal(err)
	}
	from, to := mustTime("2026-11-02T00:00:00+03:00"), mustTime("2026-11-09T00:00:00+03:00")
	if r := a.whole(from, to, []string{"alfa-1", "alfa-of"}); len(r) != 0 {
		t.Fatalf("a whole answer refused: %v", r)
	}
	shift := a.works[0].Split()
	if shift.Kind != split.Shift || shift.UserID != 101 || shift.Role != split.RoleChatter ||
		!reflect.DeepEqual(shift.Pages, []split.WorkPage{{PageLabel: "alfa-1", From: mustTime("2026-11-05T10:00:00Z")}}) {
		t.Errorf("work = %+v (a page it holds no sales of must not hold them)", shift)
	}
	if a.works[1].Split().Kind != split.Visit || !a.works[2].Split().Ongoing || a.works[2].Split().Role != "team_lead" {
		t.Errorf("visit or ongoing team lead's shift lost: %+v %+v", a.works[1], a.works[2])
	}
	// 10:00–17:00 UTC is 13:00–20:00 in Moscow: the 5th.
	if d := a.works[0].Day(); d != "2026-11-05" {
		t.Errorf("day of work %s", d)
	}

	ppv := a.sales[0].Split()
	sentAt := mustTime("2026-11-05T09:20:00Z")
	if ppv.Kind != split.SalePPV || ppv.NetMills != 24_000 || ppv.FanRef != "fan-11" || ppv.PPVSender == nil ||
		*ppv.PPVSender != (split.Person{UserID: 102, Username: "user102", Role: split.RoleChatter}) ||
		!ppv.PPVSentAt.Equal(sentAt) || ppv.LastWriter == nil || *ppv.LastWriter != 102 ||
		!reflect.DeepEqual(ppv.Holders, []string{"101:2026-11-05T10:00:00.000Z"}) || ppv.Marks != nil || ppv.Resolution != nil {
		t.Errorf("ppv = %+v", ppv)
	}

	tip := a.sales[1].Split()
	if tip.FanRef != "" || !tip.Campaign || tip.LastWriter != nil || tip.Platform != "onlyfans" {
		t.Errorf("tip = %+v", tip)
	}
	colleague, own := int64(201), int64(10_000)
	wantMarks := []split.Mark{
		{UserID: 101, Mark: split.MarkSplit, ShareWith: &colleague, Confirmed: true}, // Hub's «share» is the split's «делим»
		{UserID: 201, Mark: split.MarkNotMine},                                       // a draft report
		{UserID: 102, Mark: split.MarkMine, OwnAmountMills: &own},                    // no `confirmed` (Hub before hub#1101): a draft
	}
	if !reflect.DeepEqual(tip.Marks, wantMarks) {
		t.Errorf("marks = %+v", tip.Marks)
	}
	if !reflect.DeepEqual(tip.Resolution, []split.OwnerShare{{UserID: 101, Percent: 40}, {UserID: 201, Percent: 60}}) {
		t.Errorf("resolution = %+v", tip.Resolution)
	}

	// The split reads the mapped facts: the chatter's PPV is not the holder's (its sender had only a look-in, over
	// before the purchase, and no shift within the week: nobody's), a Campaign contribution goes to the pot whatever the
	// marks, a renewal to the one shift holding it.
	works := []split.Work{a.works[0].Split(), a.works[1].Split(), a.works[2].Split()}
	homes := split.HomesOf([]split.Sale{ppv, tip, a.sales[2].Split()}, works, mustTime("2026-11-20T00:00:00Z"))
	if homes[0].Kind != split.Nobody || homes[0].Reason != split.ReasonPPVSenderNoShift || homes[1].Kind != split.Pot ||
		homes[2].Kind != split.Shift || homes[2].Reason != split.ReasonHolder || homes[2].Shares[0].UserID != 101 {
		t.Errorf("homes %+v", homes)
	}
}

func TestTeamAnswerOutOfContractIsRefused(t *testing.T) {
	raw, err := os.ReadFile("testdata/team_sales.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(map[string]any){
		"no works":     func(m map[string]any) { delete(m, "works") },
		"unknown mark": func(m map[string]any) { sale(m, 1)["marks"].([]any)[0].(map[string]any)["mark"] = "maybe" },
		"no net":       func(m map[string]any) { delete(sale(m, 0), "netMills") },
		"a sale kind":  func(m map[string]any) { sale(m, 0)["kind"] = "refund" },
		"work kind":    func(m map[string]any) { m["works"].([]any)[0].(map[string]any)["kind"] = "nap" },
	} {
		var m map[string]any
		json.Unmarshal(raw, &m)
		edit(m)
		b, _ := json.Marshal(m)
		if _, err := decodeTeam(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func sale(m map[string]any, i int) map[string]any { return m["sales"].([]any)[i].(map[string]any) }

func TestTeamSyncReadsTheWeeksAndKeepsEachWindowsSnapshot(t *testing.T) {
	b, sy := newTeamBench(t, "2026-11-25T12:00:00+03:00")
	h := &b.hub.team
	a := teamShift(101, "shift", "2026-11-05T13:00:00+03:00", "2026-11-05T20:00:00+03:00", "alfa-1")
	c := teamShift(102, "shift", "2026-11-09T01:00:00+03:00", "2026-11-09T09:00:00+03:00", "alfa-of")
	h.works = []map[string]any{a, c}
	h.sales = []map[string]any{
		teamSaleOf("1", "alfa-1", "tip", "2026-11-05T14:00:00+03:00", 10_000, a),
		teamSaleOf("2", "alfa-of", "subscription", "2026-11-09T02:00:00+03:00", 20_000, c),
		teamSaleOf("3", "alfa-1", "ppv", "2026-11-24T10:00:00+03:00", 5_000),
	}

	// The first run reads every week since hub_team_since — before the rule's start too; Hub's works begin on a
	// Wednesday here, and so does the first window: none says «nobody worked» where Hub has no data.
	setTeamSince(b, "2026-10-28")
	rep, err := sy.Hourly(b.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var froms []string
	for _, w := range rep.Windows {
		froms = append(froms, w.From[:10]+"/"+w.Status)
	}
	if !rep.Full || strings.Join(froms, " ") != "2026-10-28/ok 2026-11-02/ok 2026-11-09/ok 2026-11-16/ok 2026-11-23/ok" {
		t.Fatalf("first run %+v", rep)
	}
	if rep.Windows[4].To != "2026-11-25T12:00:00+03:00" || rep.Windows[0].To != "2026-11-02T00:00:00+03:00" ||
		rep.Windows[1].To != "2026-11-09T00:00:00+03:00" {
		t.Fatalf("window ends %+v", rep.Windows)
	}
	if body := h.bodies[0]; body["from"] != "2026-10-28T00:00:00+03:00" || body["to"] != "2026-11-02T00:00:00+03:00" {
		t.Fatalf("request body %v", body)
	}
	if n := b.count(`SELECT count(*) FROM hub_sales`); n != 3 {
		t.Fatalf("%d sales", n)
	}
	if n := b.count(`SELECT count(*) FROM hub_sales WHERE transaction_ref = '2' AND window_from = '2026-11-09T00:00:00+03:00'
		AND occurred_at = '2026-11-09T02:00:00.000+03:00' AND net_mills = 20000`); n != 1 {
		t.Fatal("sale 2 not mirrored in its week")
	}
	// The shift of the 5th is in its own week's snapshot; the night of the 9th is in two (a day before the second
	// window and 7 days after the first).
	if n := b.count(`SELECT count(*) FROM hub_works WHERE ref = ?`, c["ref"]); n != 2 {
		t.Fatalf("night shift in %d snapshots", n)
	}
	if n := b.count(`SELECT count(*) FROM hub_works WHERE ref = ? AND day = '2026-11-05' AND kind = 'shift'
		AND window_from = '2026-11-02T00:00:00+03:00'`, a["ref"]); n != 1 {
		t.Fatal("shift of the 5th")
	}
	if n := b.count(`SELECT count(*) FROM hub_team_sync WHERE status = 'ok' AND work_to = '2026-11-16T00:00:00.000+03:00'
		AND window_from = '2026-11-02T00:00:00+03:00'`); n != 1 {
		t.Fatal("work_to of a closed week is 7 days after it")
	}
	if n := b.count(`SELECT count(*) FROM cursors WHERE name = 'hub:sent'`); n != 1 {
		t.Fatal("the team's reads went past the hub_rpm throttle")
	}

	// An hour later only the weeks of the last 14 days are read; a sale that left the hub leaves its week.
	h.sales = h.sales[1:]
	b.clock.advance(time.Hour)
	rep, err = sy.Hourly(b.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Full || len(rep.Windows) != 3 || rep.Windows[0].From != "2026-11-09T00:00:00+03:00" {
		t.Fatalf("tail run %+v", rep)
	}
	if n := b.count(`SELECT count(*) FROM hub_sales`); n != 3 {
		t.Fatal("a week the tail did not read changed")
	}
	if _, err := sy.Run(b.ctx, mustTime("2026-11-03"), mustTime("2026-11-04")); err != nil {
		t.Fatal(err)
	}
	if n := b.count(`SELECT count(*) FROM hub_sales WHERE transaction_ref = '1'`); n != 0 {
		t.Fatal("a sale gone from the hub stayed")
	}
}

func TestTeamSyncWithoutReadTeamIsUnknownWithTheReason(t *testing.T) {
	b, sy := newTeamBench(t, "2026-11-12T12:00:00+03:00")
	h := &b.hub.team
	a := teamShift(101, "shift", "2026-11-05T13:00:00+03:00", "2026-11-05T20:00:00+03:00", "alfa-1")
	h.works, h.sales = []map[string]any{a}, []map[string]any{teamSaleOf("1", "alfa-1", "tip", "2026-11-05T14:00:00+03:00", 10_000, a)}
	if rep, err := sy.Hourly(b.ctx); err != nil || rep.Unknown() != 0 {
		t.Fatalf("first run %+v %v", rep, err)
	}

	// The owner took the right away: the first window answers 403, the rest are not asked.
	h.status = []fakeStatus{{403, "agent_capability_missing"}}
	calls := h.calls
	rep, err := sy.Run(b.ctx, mustTime("2026-11-02"), b.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unknown() != 2 || h.calls != calls+1 {
		t.Fatalf("403 run %+v, %d calls", rep, h.calls-calls)
	}
	for _, w := range rep.Windows {
		if len(w.Reasons) != 1 || !strings.Contains(w.Reasons[0], "read:team") || !strings.Contains(w.Reasons[0], "403") {
			t.Fatalf("reason %v", w.Reasons)
		}
	}
	sts, err := TeamStatuses(b.ctx, b.store.RO)
	if err != nil {
		t.Fatal(err)
	}
	sum := TeamSummary(sts)
	if sum["ok"] != false || sum["unknown"] != 2 || sts[0].OkAt == "" || *sts[0].Sales != 1 {
		t.Fatalf("status %+v %+v", sum, sts[0])
	}
	// What the window proved before stays: unknown is not zero.
	if n := b.count(`SELECT count(*) FROM hub_sales`); n != 1 {
		t.Fatal("a refusal emptied the mirror")
	}
}

func TestTeamSyncRefusesAPartialWindow(t *testing.T) {
	b, sy := newTeamBench(t, "2026-11-12T12:00:00+03:00")
	h := &b.hub.team
	a := teamShift(101, "shift", "2026-11-05T13:00:00+03:00", "2026-11-05T20:00:00+03:00", "alfa-1")
	h.works = []map[string]any{a}
	h.sales = []map[string]any{teamSaleOf("1", "alfa-1", "tip", "2026-11-05T14:00:00+03:00", 10_000, a)}

	for _, fault := range []string{"holder", "window"} {
		h.fault = map[string]bool{fault: true}
		rep, err := sy.Run(b.ctx, mustTime("2026-11-02"), b.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		// The week with the sale is partial; the week without one is whole either way for "holder".
		if rep.Windows[0].Status != "unknown" || len(rep.Windows[0].Reasons) == 0 {
			t.Fatalf("%s: %+v", fault, rep.Windows)
		}
		if n := b.count(`SELECT count(*) FROM hub_sales`); n != 0 {
			t.Fatalf("%s: a partial answer was mirrored", fault)
		}
	}
	if !strings.Contains(strings.Join(mustRun(t, b, sy, "holder").Windows[0].Reasons, " "), "нет среди работ") {
		t.Fatal("the reason does not name the missing holder")
	}

	// A page of the grant missing from the answer is partial too.
	h.fault = map[string]bool{}
	b.exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('beta-1', 'fansly', 'Beta', 'Beta', '2026-11-12')`)
	rep := mustRun(t, b, sy, "")
	if rep.Unknown() != 2 || !strings.Contains(rep.Windows[1].Reasons[0], "beta-1") {
		t.Fatalf("missing page %+v", rep.Windows)
	}
	b.exec(`DELETE FROM hub_pages WHERE page_label = 'beta-1'`)
	if rep := mustRun(t, b, sy, ""); rep.Unknown() != 0 || b.count(`SELECT count(*) FROM hub_sales`) != 1 {
		t.Fatalf("whole again %+v", rep.Windows)
	}
}

func mustRun(t *testing.T, b *bench, sy *TeamSyncer, fault string) TeamReport {
	t.Helper()
	b.hub.team.fault = map[string]bool{fault: fault != ""}
	rep, err := sy.Run(b.ctx, mustTime("2026-11-02"), b.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}
