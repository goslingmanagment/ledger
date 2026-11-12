package hub

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (b *bench) balances(page string) {
	h := b.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, day := range []string{"2026-11-02", "2026-11-03"} {
		at := mustTime(day + "T00:00:00Z")
		h.rows[page+"/page_profile_daily"] = append(h.rows[page+"/page_profile_daily"], fakeRow{key: "d" + day, at: at,
			fields: map[string]any{"platform": "fansly", "day": day, "followers": 10, "earningsBalanceMills": int64(100_000 * (i + 1)),
				"capturedAt": hubTime(at.Add(23 * time.Hour))}})
	}
	seen := mustTime("2026-11-03T08:30:49.091Z")
	h.rows[page+"/pending_balance"] = []fakeRow{{key: "pb1", at: seen, fields: map[string]any{"platform": "fansly",
		"pendingMills": int64(42_500), "firstSeenAt": "2026-10-31T05:56:10.620Z", "lastSeenAt": hubTime(seen), "current": true}}}
	// The settings journal keeps only its latest versions: its floor is the newest sighting.
	h.floors[page+"/fansly_page_settings_versions"] = seen
}

func TestSyncMirrorsEveryDatasetAndIsIdempotent(t *testing.T) {
	b := newBench(t)
	h := b.hub
	h.tx("alfa-1", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	h.tx("alfa-1", "2", "2026-09-03T11:00:00.250Z", "subscription", 20_500, "USD", "102")
	h.tx("alfa-1", "3", "2026-10-30T12:00:00Z", "message_purchase", 7_000, "USD", "103")
	h.tx("alfa-1", "4", "2026-10-31T12:00:00Z", "payout_reversal", 50_000, "USD", "104")
	h.tx("alfa-of", "9", "2026-10-01T09:00:00Z", "tip", 3_000, "USD", "901")
	h.payout("alfa-1", "1", "2026-09-10T08:00:00Z", 25_000, "Processed", "crypto")
	h.ofPayout("alfa-of", "1", "2026-09-12T08:00:00Z", 2_000, "USD", "approved")
	b.balances("alfa-1")

	rep := b.sync()
	if n := rep.Unknown(); n != 0 {
		t.Fatalf("unknown after a clean sync: %+v", rep.Results)
	}
	if rep.Requests != h.calls+1 { // the dataset queries and the capabilities call
		t.Fatalf("report counts %d requests, the hub saw %d + capabilities", rep.Requests, h.calls)
	}
	if r := b.result(rep, "alfa-1", "transactions"); r.Mode != "full" || r.Rows != 3 || r.Events != 3 {
		t.Fatalf("first transactions run %+v", r)
	}
	if n := b.count(`SELECT count(*) FROM hub_transactions WHERE type = 'payout_reversal'`); n != 0 {
		t.Fatal("payout_reversal mirrored")
	}
	events := b.count(`SELECT count(*) FROM events WHERE source = 'hub'`)
	if events != 3+1+1+1+2+1 {
		t.Fatalf("%d hub events", events)
	}
	if n := b.count(`SELECT count(*) FROM observations WHERE source = 'hub'`); n != 3 {
		t.Fatalf("%d observations", n)
	}
	if n := b.count(`SELECT count(*) FROM observations WHERE kind = 'hub_pending' AND subject = 'alfa-1' AND value = 42500000
		AND observed_at = '2026-11-03T11:30:49+03:00'`); n != 1 {
		t.Fatal("pending balance observation missing")
	}
	var id string
	b.store.RW.QueryRow(`SELECT source_id FROM events WHERE kind = 'transactions' AND source_id LIKE 'alfa-1/transactions/2/%'`).Scan(&id)
	if len(id) != len("alfa-1/transactions/2/")+12 {
		t.Fatalf("source_id %q", id)
	}
	var occurred string
	b.store.RW.QueryRow(`SELECT occurred_at FROM hub_transactions WHERE key = '2'`).Scan(&occurred)
	if occurred != "2026-09-03T14:00:00.250+03:00" {
		t.Fatalf("occurred_at %s", occurred)
	}

	// An hour later nothing changed: only the tail is fetched and no event repeats.
	b.clock.advance(time.Hour)
	rep = b.sync()
	if r := b.result(rep, "alfa-1", "transactions"); r.Mode != "tail" || r.Events != 0 || r.Status != "ok" {
		t.Fatalf("second run %+v", r)
	}
	if n := b.count(`SELECT count(*) FROM events WHERE source = 'hub'`); n != events {
		t.Fatalf("re-sync added events: %d → %d", events, n)
	}
	if n := b.count(`SELECT count(*) FROM observations`); n != 3 {
		t.Fatalf("re-sync added observations: %d", n)
	}

	// A changed row is a new version: one event, the mirror follows.
	h.set("alfa-1", "transactions", "3", "netMills", int64(6_500))
	h.set("alfa-1", "payouts", "1", "statusLabel", "Pending")
	b.clock.advance(time.Hour)
	rep = b.sync()
	if r := b.result(rep, "alfa-1", "transactions"); r.Events != 1 {
		t.Fatalf("changed row %+v", r)
	}
	if r := b.result(rep, "alfa-1", "payouts"); r.Events != 1 {
		t.Fatalf("changed payout %+v", r)
	}
	if n := b.count(`SELECT net_mills FROM hub_transactions WHERE page_label = 'alfa-1' AND key = '3'`); n != 6500 {
		t.Fatalf("mirror net %d", n)
	}
	if n := b.count(`SELECT count(*) FROM events WHERE source_id LIKE 'alfa-1/transactions/3/%'`); n != 2 {
		t.Fatalf("%d versions of row 3", n)
	}

	// A row that left the hub leaves the mirror; its events stay.
	h.remove("alfa-1", "transactions", "3")
	b.clock.advance(time.Hour)
	rep = b.sync()
	if r := b.result(rep, "alfa-1", "transactions"); r.Removed != 1 || r.Rows != 2 {
		t.Fatalf("removed row %+v", r)
	}
	if n := b.count(`SELECT count(*) FROM events WHERE source_id LIKE 'alfa-1/transactions/3/%'`); n != 2 {
		t.Fatal("events of a removed row changed")
	}
}

func TestCursorTraversalSendsOnlyTheCursor(t *testing.T) {
	b := newBench(t)
	start := mustTime("2026-09-01T00:00:00Z")
	for i := 0; i < 450; i++ {
		b.hub.tx("alfa-1", fmt.Sprintf("%04d", i), hubTime(start.Add(time.Duration(i)*time.Minute)),
			"tip", 1_000, "USD", itoa(1000+i))
	}
	rep := b.sync("alfa-1")
	r := b.result(rep, "alfa-1", "transactions")
	if r.Status != "ok" || r.Rows != 450 {
		t.Fatalf("%+v", r)
	}
	var lists []map[string]any
	for _, body := range b.hub.bodies["alfa-1/transactions"] {
		if body["summary"] == nil {
			lists = append(lists, body)
		}
	}
	if len(lists) != 3 {
		t.Fatalf("%d list requests, want 3 pages of 200", len(lists))
	}
	first := lists[0]
	for _, k := range []string{"from", "to", "claim", "sort", "limit", "filters"} {
		if first[k] == nil {
			t.Fatalf("first page without %s: %v", k, first)
		}
	}
	for _, body := range lists[1:] {
		if len(body) != 1 || body["cursor"] == nil {
			t.Fatalf("continuation resent the scope: %v", body)
		}
	}
	st, _ := loadStatus(b.ctx, b.store.RW, "alfa-1", "transactions")
	if strings.Join(st.Blockers, ",") != "delivery_not_exhausted,mutable_sort_key_traversal" {
		t.Fatalf("blockers %v", st.Blockers)
	}
	if n := b.count(`SELECT count(*) FROM hub_transactions`); n != 450 {
		t.Fatalf("mirror %d", n)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestRefusalsLeaveThePageUnknown(t *testing.T) {
	cases := []struct {
		name   string
		inject func(h *fakeHub)
		want   string
	}{
		{"unknown blocker", func(h *fakeHub) { h.extra["alfa-1/transactions"] = []string{"plane_not_read"} }, "blockers ['plane_not_read']"},
		{"gap", func(h *fakeHub) { h.fault("alfa-1", "transactions", "gap") }, "internal_capture_gap"},
		{"source errors", func(h *fakeHub) { h.fault("alfa-1", "transactions", "sourceErrors") }, "sourceErrors"},
		{"unapplied filter", func(h *fakeHub) { h.fault("alfa-1", "transactions", "unapplied") }, "фильтр не применён"},
		{"non-USD summary", func(h *fakeHub) { h.fault("alfa-1", "transactions", "nonUSD") }, "не-USD"},
		{"window before the floor", func(h *fakeHub) { h.floors["alfa-1/transactions"] = mustTime("2026-06-01T00:00:00Z") },
			"window_before_capture_floor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			b.hub.tx("alfa-1", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
			if rep := b.sync("alfa-1"); b.result(rep, "alfa-1", "transactions").Status != "ok" {
				t.Fatal("clean sync refused")
			}
			c.inject(b.hub) // the fake serves nothing between runs
			b.clock.advance(time.Hour)
			r := b.result(b.sync("alfa-1"), "alfa-1", "transactions")
			if r.Status != "unknown" || len(r.Reasons) != 1 || !strings.Contains(r.Reasons[0], c.want) {
				t.Fatalf("%+v, want a refusal with %q", r, c.want)
			}
			st, _ := loadStatus(b.ctx, b.store.RW, "alfa-1", "transactions")
			if st.Status != "unknown" || !strings.Contains(strings.Join(st.Reasons, " "), c.want) || st.OkAt == "" {
				t.Fatalf("status %+v", st)
			}
			if _, err := Rows(b.ctx, b.store.RO, "alfa-1", mustTime("2026-09-01"), mustTime("2026-10-01")); !IsRefusal(err) ||
				!strings.Contains(err.Error(), c.want) {
				t.Fatalf("rows of an unproven page: %v", err)
			}
		})
	}
}

func TestListSummaryMismatchRetriesThenRefuses(t *testing.T) {
	b := newBench(t)
	b.hub.tx("alfa-1", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	b.hub.drift["alfa-1"] = 1 // a row lands between the list and the summary once
	r := b.result(b.sync("alfa-1"), "alfa-1", "transactions")
	if r.Status != "ok" || r.Mode != "full" || r.Requests != 4 {
		t.Fatalf("one retry expected: %+v", r)
	}

	// With a mirror, two tail attempts fail, then the full fetch proves the window.
	b.hub.drift["alfa-1"] = 2
	b.clock.advance(time.Hour)
	r = b.result(b.sync("alfa-1"), "alfa-1", "transactions")
	if r.Status != "ok" || r.Mode != "full" || r.Requests != 6 {
		t.Fatalf("tail ×2 then full: %+v", r)
	}

	// A summary that never matches refuses after the full fetch's retry; the mirror keeps its rows.
	b.hub.drift["alfa-1"] = 10
	b.clock.advance(time.Hour)
	r = b.result(b.sync("alfa-1"), "alfa-1", "transactions")
	if r.Status != "unknown" || !strings.Contains(r.Reasons[0], "список строк (1) не сходится с --summary (2, 10.00)") {
		t.Fatalf("%+v", r)
	}
	if n := b.count(`SELECT count(*) FROM hub_transactions`); n != 1 {
		t.Fatalf("mirror %d", n)
	}
}

func TestPayoutListMustBeProvenComplete(t *testing.T) {
	b := newBench(t)
	start := mustTime("2026-06-01T00:00:00Z")
	for i := 0; i < 201; i++ {
		at := hubTime(start.Add(time.Duration(i) * time.Hour))
		b.hub.payout("alfa-1", itoa(i), at, 1_000, "Processed", "crypto")
		b.hub.ofPayout("alfa-of", itoa(i), at, 1_000, "USD", "approved")
	}
	rep := b.sync()
	if r := b.result(rep, "alfa-1", "payouts"); r.Status != "unknown" || !strings.Contains(r.Reasons[0], "не доказан полным") {
		t.Fatalf("two pages of payouts accepted: %+v", r)
	}
	if r := b.result(rep, "alfa-of", "ofapi_payout_requests"); r.Status != "ok" || r.Rows != 201 {
		t.Fatalf("OnlyFans requests need no single-page proof: %+v", r)
	}
	b.hub.remove("alfa-1", "payouts", "200")
	b.hub.fault("alfa-1", "payouts", "inexact")
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "payouts"); r.Status != "unknown" {
		t.Fatalf("inexact count accepted: %+v", r)
	}
	b.hub.clearFaults()
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "payouts"); r.Status != "ok" || r.Rows != 200 {
		t.Fatalf("%+v", r)
	}
	ps, err := Payouts(b.ctx, b.store.RO, "alfa-1", mustTime("2026-05-01"), b.clock.Now().Truncate(time.Second))
	if err != nil || len(ps) != 200 || ps[0].AmountMills != 1000 || ps[0].Updated.Sub(ps[0].At) != time.Hour {
		t.Fatalf("payouts %d %v", len(ps), err)
	}
}

func TestPayoutFloorException(t *testing.T) {
	b := newBench(t)
	b.hub.payout("alfa-1", "1", "2026-06-15T10:00:00Z", 5_000, "Processed", "crypto")
	b.hub.floors["alfa-1/page_payout_requests"] = mustTime("2026-06-15T10:00:00Z")
	if r := b.result(b.sync("alfa-1"), "alfa-1", "payouts"); r.Status != "unknown" ||
		!strings.Contains(r.Reasons[0], "window_before_capture_floor") {
		t.Fatalf("journal starting late accepted without a decision: %+v", r)
	}
	importExceptions(t, b, "floor", "| Страница | Заявки с первой | Основание |\n|---|---|---|\n| alfa-1 | 2026-06-14 | синтетика |\n")
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "payouts"); r.Status != "unknown" {
		t.Fatalf("floor of another day accepted: %+v", r)
	}
	importExceptions(t, b, "floor", "| Страница | Заявки с первой | Основание |\n|---|---|---|\n| alfa-1 | 2026-06-15 | синтетика |\n")
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "payouts"); r.Status != "ok" || r.Rows != 1 {
		t.Fatalf("floor exception refused: %+v", r)
	}
	st, _ := loadStatus(b.ctx, b.store.RW, "alfa-1", "payouts")
	if !strings.Contains(strings.Join(st.Blockers, ","), "window_before_capture_floor") {
		t.Fatalf("tolerated blockers not kept: %v", st.Blockers)
	}
	b.hub.fault("alfa-1", "payouts", "gap")
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "payouts"); r.Status != "unknown" {
		t.Fatalf("another gap beside the floor accepted: %+v", r)
	}
}

func importExceptions(t *testing.T, b *bench, source, text string) {
	t.Helper()
	ex, err := ParseExceptions("hub-exceptions.md", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportExceptions(b.ctx, b.store.RW, ex, source, b.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestStartExceptionStartsTheWindow(t *testing.T) {
	b := newBench(t)
	b.hub.tx("alfa-1", "1", "2026-08-20T12:00:00Z", "tip", 4_000, "USD", "101")
	b.hub.tx("alfa-1", "2", "2026-09-01T12:00:00Z", "tip", 6_000, "USD", "102")
	b.hub.floors["alfa-1/transactions"] = mustTime("2026-08-20T12:00:00Z")
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "unknown" {
		t.Fatalf("late page without a start accepted: %+v", r)
	}
	importExceptions(t, b, "x", "| Страница | Начало UTC | Основание |\n|---|---|---|\n| alfa-1 | 2026-08-20 12:00:00 | синтетика |\n")
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "ok" || r.Rows != 2 {
		t.Fatalf("%+v", r)
	}
	c, err := Rows(b.ctx, b.store.RO, "alfa-1", mustTime("2026-08-01"), mustTime("2026-09-01"))
	if err != nil || len(c.Rows) != 1 || c.Rows[0].NetMills != 4000 {
		t.Fatalf("rows before the start: %+v %v", c, err)
	}
	if _, err := Rows(b.ctx, b.store.RO, "alfa-of", mustTime("2026-08-01"), mustTime("2026-09-01")); !IsRefusal(err) {
		t.Fatalf("a page never synced answered: %v", err)
	}
	if fl, ok, err := CaptureFloor(b.ctx, b.store.RO, "alfa-1"); err != nil || !ok || !fl.Equal(mustTime("2026-08-20T12:00:00Z")) {
		t.Fatalf("floor %v %v %v", fl, ok, err)
	}
}

func TestRetriesAndRefusalsOfCalls(t *testing.T) {
	b := newBench(t)
	b.hub.tx("alfa-1", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	b.hub.status = []fakeStatus{{503, ""}, {502, ""}}
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "ok" {
		t.Fatalf("503, 502 then 200: %+v", r)
	}
	if got := sleepsOf(b.clock, 30*time.Second); got != "30s,1m0s" {
		t.Fatalf("pauses %s", got)
	}
	b.clock.sleeps = nil
	b.hub.status = []fakeStatus{{429, "agent_budget_exhausted"}}
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "unknown" ||
		!strings.Contains(r.Reasons[0], "429 agent_budget_exhausted") {
		t.Fatalf("budget refusal: %+v", r)
	}
	if got := sleepsOf(b.clock, 30*time.Second); got != "" {
		t.Fatalf("a spent budget was retried: %s", got)
	}
	b.hub.status = []fakeStatus{{503, ""}, {503, ""}, {503, ""}, {503, ""}}
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "unknown" || r.Requests != 4 {
		t.Fatalf("four attempts: %+v", r)
	}
	if got := sleepsOf(b.clock, 30*time.Second); got != "30s,1m0s,1m30s" {
		t.Fatalf("pauses %s", got)
	}
	b.hub.status = []fakeStatus{{400, "validation"}}
	b.clock.sleeps = nil
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "unknown" || r.Requests != 1 {
		t.Fatalf("400 retried: %+v", r)
	}
}

// sleepsOf lists the pauses of at least min (retry pauses, not throttle waits).
func sleepsOf(c *fakeClock, min time.Duration) string {
	var out []string
	for _, d := range c.sleeps {
		if d >= min && d%(30*time.Second) == 0 {
			out = append(out, d.String())
		}
	}
	return strings.Join(out, ",")
}

func TestThrottleCountsTheRollingMinuteAcrossRuns(t *testing.T) {
	b := newBench(t)
	th := &Throttle{DB: b.store.RW, RPM: 3, Now: b.clock.Now, Sleep: b.clock.Sleep}
	for i := 0; i < 3; i++ {
		if err := th.Wait(b.ctx); err != nil {
			t.Fatal(err)
		}
		b.clock.advance(time.Second)
	}
	if len(b.clock.sleeps) != 0 {
		t.Fatalf("waited inside the quota: %v", b.clock.sleeps)
	}
	again := &Throttle{DB: b.store.RW, RPM: 3, Now: b.clock.Now, Sleep: b.clock.Sleep} // the next run
	if err := again.Wait(b.ctx); err != nil {
		t.Fatal(err)
	}
	if len(b.clock.sleeps) != 1 || b.clock.sleeps[0] != 57500*time.Millisecond {
		t.Fatalf("waits %v, want the first request's minute (60.5 s) to pass", b.clock.sleeps)
	}
}

func TestTailAndFullFetches(t *testing.T) {
	b := newBench(t)
	b.hub.tx("alfa-1", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	b.hub.tx("alfa-1", "2", "2026-10-30T12:00:00Z", "tip", 2_000, "USD", "102")
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Mode != "full" {
		t.Fatalf("%+v", r)
	}
	b.hub.bodies = map[string][]map[string]any{}
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Mode != "tail" {
		t.Fatalf("%+v", r)
	}
	// The tail starts hub_tail_days (10) before the newest mirrored row.
	if from := b.hub.bodies["alfa-1/transactions"][0]["from"]; from != "2026-10-20T15:00:00+03:00" {
		t.Fatalf("tail from %v", from)
	}
	// A row older than the tail changes its type without its net: only the weekly full fetch sees it.
	b.hub.set("alfa-1", "transactions", "1", "transactionType", "subscription")
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Mode != "tail" || r.Events != 0 {
		t.Fatalf("%+v", r)
	}
	b.clock.advance(6 * 24 * time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Mode != "full" || r.Events != 1 {
		t.Fatalf("weekly full fetch: %+v", r)
	}
	// A window that starts before the mirror is fetched whole.
	b.exec(`UPDATE settings SET value = '2026-04-01T00:00:00+03:00' WHERE key = 'hub_window_start'`)
	b.clock.advance(time.Hour)
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Mode != "full" {
		t.Fatalf("earlier window: %+v", r)
	}
}

func TestGrantChanges(t *testing.T) {
	b := newBench(t)
	b.hub.tx("alfa-of", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	b.sync()
	b.hub.grant = b.hub.grant[:1] // alfa-of leaves the key's grant
	b.clock.advance(time.Hour)
	b.sync()
	st, _ := loadStatus(b.ctx, b.store.RW, "alfa-of", "transactions")
	if st.Status != "unknown" || !strings.Contains(st.Reasons[0], "нет в гранте") {
		t.Fatalf("%+v", st)
	}
	if _, err := b.sy.Run(b.ctx, []string{"zeta-1"}); err == nil {
		t.Fatal("a page outside the grant was synced")
	}
	pages, err := GrantPages(b.ctx, b.store.RO)
	if err != nil || len(pages) != 1 || pages[0].Label != "alfa-1" {
		t.Fatalf("%+v %v", pages, err)
	}
}

// Two syncs at once: the second waits its turn instead of writing over the first.
func TestOneSyncAtATime(t *testing.T) {
	b := newBench(t)
	ctx := b.ctx
	sy := &Syncer{Store: b.store, Now: func() time.Time { return mustTime("2026-11-04") }}
	release, err := sy.lease(ctx, sy.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sy.lease(ctx, sy.Now().Add(time.Minute)); err == nil || !IsRefusal(err) {
		t.Fatalf("a second sync must wait: %v", err)
	}
	if _, err := sy.lease(ctx, sy.Now().Add(2*leaseFor)); err != nil {
		t.Fatalf("a dead run's lease must give way: %v", err)
	}
	release()
	if _, err := sy.lease(ctx, sy.Now()); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// A page of a platform without sales (Telegram) in the key's grant is no source: nothing is fetched for it and no
// status row says «неизвестно». A page that left the grant without ever being proven leaves no status behind; one
// proven before keeps alarming (a lost grant of a real source must be noticed).
func TestAPageWithoutSalesOrNeverProvenIsNoSource(t *testing.T) {
	b := newBench(t)
	b.hub.tx("alfa-of", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	b.hub.grant = append(b.hub.grant, GrantPage{PageLabel: "alfa-tg", Platform: "telegram", ModelSlug: "Alfa", ModelName: "Alfa"})
	b.sync()
	if st, _ := loadStatus(b.ctx, b.store.RW, "alfa-tg", "transactions"); st != nil {
		t.Fatalf("a page without sales was fetched: %+v", st)
	}
	// As an older ledger left it: a status row of a page never proven, which then leaves the grant.
	b.exec(`INSERT INTO hub_sync(page_label, dataset, status, reasons, checked_at) VALUES ('beta-x', 'transactions', 'unknown',
		'["no dataset"]', '2026-11-04T06:00:00+03:00')`)
	b.hub.grant = b.hub.grant[:1] // alfa-of and alfa-tg leave the grant
	b.clock.advance(time.Hour)
	b.sync()
	sts, err := Statuses(b.ctx, b.store.RO)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, s := range sts {
		if s.Status != "ok" {
			left = append(left, s.Page+"/"+s.Dataset)
		}
	}
	if strings.Join(left, ",") != "alfa-of/ofapi_payout_requests,alfa-of/transactions" {
		t.Fatalf("statuses not ok: %v", left)
	}
}
