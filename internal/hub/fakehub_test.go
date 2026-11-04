package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// fakeHub speaks the Agent Read Plane contract over HTTP with synthetic data: windows, the
// payout_reversal filter, keyset pages with opaque cursors, summaries by currency, capture floors
// that open before_capture_floor gaps, and injectable faults.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	grant  []GrantPage
	rows   map[string][]fakeRow        // "page/dataset" → rows
	floors map[string]time.Time        // "page/plane" → capture floor
	faults map[string]map[string]bool  // "page/dataset" → fault names
	extra  map[string][]string         // "page/dataset" → extra blockers on lists
	status []fakeStatus                // answers to the next dataset calls, then 200
	drift  map[string]int              // page → next summaries that count one row more
	bodies map[string][]map[string]any // "page/dataset" → request bodies
	calls  int
}

type fakeRow struct {
	key    string
	at     time.Time
	fields map[string]any
}

type fakeStatus struct {
	code  int
	error string
}

func newFakeHub(t *testing.T) *fakeHub {
	f := &fakeHub{t: t, rows: map[string][]fakeRow{}, floors: map[string]time.Time{}, faults: map[string]map[string]bool{},
		extra: map[string][]string{}, drift: map[string]int{}, bodies: map[string][]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) fault(page, ds, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := page + "/" + ds
	if f.faults[k] == nil {
		f.faults[k] = map[string]bool{}
	}
	f.faults[k][name] = true
}

func (f *fakeHub) clearFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults, f.extra = map[string]map[string]bool{}, map[string][]string{}
}

func hubTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// tx adds a transaction row.
func (f *fakeHub) tx(page, key, at, typ string, net int64, cur, ref string) {
	t := mustTime(at)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[page+"/transactions"] = append(f.rows[page+"/transactions"], fakeRow{key: key, at: t, fields: map[string]any{
		"platform": "fansly", "platformUserId": "fan-" + key, "transactionRef": ref, "transactionType": typ,
		"transactionState": "posted", "occurredAt": hubTime(t), "grossMills": net * 5 / 4, "netMills": net,
		"feeMills": nil, "currency": cur, "relatedMessageRef": nil, "correlationRef": nil}})
}

func (f *fakeHub) payout(page, key, at string, amount int64, status, method string) {
	t := mustTime(at)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[page+"/payouts"] = append(f.rows[page+"/payouts"], fakeRow{key: key, at: t, fields: map[string]any{
		"platform": "fansly", "payoutRef": "p" + key, "amountMills": amount, "statusCode": 8, "statusLabel": status,
		"statusConfidence": "observed", "methodRef": "m1", "methodProviderId": 1, "methodProviderLabel": method,
		"methodMaskedLabel": nil, "requestedAt": hubTime(t), "updatedAtPlatform": hubTime(t.Add(time.Hour))}})
}

func (f *fakeHub) ofPayout(page, key, at string, amount int64, cur, state string) {
	t := mustTime(at)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[page+"/ofapi_payout_requests"] = append(f.rows[page+"/ofapi_payout_requests"], fakeRow{key: key, at: t,
		fields: map[string]any{"platform": "onlyfans", "payoutRef": "o" + key, "amountMills": amount, "currency": cur,
			"state": state, "rejectReason": nil, "requestedAt": hubTime(t), "lastObservedAt": hubTime(t),
			"observationRef": "1"}})
}

func (f *fakeHub) set(page, ds, key string, field string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rows[page+"/"+ds] {
		if r.key == key {
			f.rows[page+"/"+ds][i].fields[field] = v
			return
		}
	}
	f.t.Fatalf("no row %s/%s/%s", page, ds, key)
}

func (f *fakeHub) remove(page, ds, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rs := f.rows[page+"/"+ds]
	for i, r := range rs {
		if r.key == key {
			f.rows[page+"/"+ds] = append(rs[:i], rs[i+1:]...)
			return
		}
	}
}

func (f *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-key" {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"agent_key_invalid","message":"bad key"}`))
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/capabilities" {
		json.NewEncoder(w).Encode(map[string]any{"grant": map[string]any{"pages": f.grant, "totalPages": len(f.grant) + 1}})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agent/pages/"), "/")
	if r.Method != http.MethodPost || len(parts) != 4 || parts[1] != "datasets" || parts[3] != "query" {
		w.WriteHeader(404)
		return
	}
	f.calls++
	if len(f.status) > 0 {
		s := f.status[0]
		f.status = f.status[1:]
		w.WriteHeader(s.code)
		json.NewEncoder(w).Encode(map[string]any{"error": s.error, "message": "synthetic failure"})
		return
	}
	page, ds := parts[0], parts[2]
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		return
	}
	f.bodies[page+"/"+ds] = append(f.bodies[page+"/"+ds], body)
	resp, code := f.answer(page, ds, body)
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}

type fakeCursor struct {
	From, To string
	Offset   int
	Limit    int
	Filters  []any
	Claimed  bool // the cursor carries the claim, as the real one does
}

func (f *fakeHub) answer(page, ds string, body map[string]any) (map[string]any, int) {
	key := page + "/" + ds
	faults := f.faults[key]
	cursor, _ := body["cursor"].(string)
	var cur fakeCursor
	consumed := cursor != ""
	if consumed {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &cur) != nil || len(body) != 1 {
			return map[string]any{"error": "validation", "message": "cursor pins the query"}, 400
		}
	} else {
		cur.From, _ = body["from"].(string)
		cur.To, _ = body["to"].(string)
		cur.Filters, _ = body["filters"].([]any)
		cur.Limit = 50
		if l, ok := body["limit"].(float64); ok {
			cur.Limit = int(l)
		}
	}
	from, err1 := time.Parse(time.RFC3339, cur.From)
	to, err2 := time.Parse(time.RFC3339, cur.To)
	if err1 != nil || err2 != nil || !from.Before(to) {
		return map[string]any{"error": "validation", "message": "both window bounds are required"}, 400
	}
	summary, _ := body["summary"].(bool)
	if !consumed {
		_, cur.Claimed = body["claim"]
	}
	claimed := cur.Claimed
	var match []fakeRow
	for _, r := range f.rows[key] {
		if r.at.Before(from) || !r.at.Before(to) {
			continue
		}
		if len(cur.Filters) > 0 && !faults["unapplied"] && r.fields["transactionType"] == "payout_reversal" {
			continue
		}
		match = append(match, r)
	}
	sort.SliceStable(match, func(i, j int) bool {
		if !match[i].at.Equal(match[j].at) {
			return match[i].at.Before(match[j].at)
		}
		return match[i].key < match[j].key
	})
	pl := floorPlane[ds]
	blockers := []string{}
	gaps := []any{}
	planeFloor := map[string]any{"at": nil, "kind": "unknown"}
	if fl, ok := f.floors[page+"/"+pl]; ok {
		planeFloor = map[string]any{"at": hubTime(fl), "kind": "oldest_stored_row"}
		if from.Before(fl) {
			gaps = append(gaps, map[string]any{"kind": "before_capture_floor", "from": hubTime(from), "to": hubTime(fl),
				"plane": pl, "remedy": map[string]any{"kind": "none", "reason": "journal_before_capture_start"}})
			blockers = append(blockers, "window_before_capture_floor", "gaps_present")
		}
	} else if len(match) > 0 {
		planeFloor = map[string]any{"at": hubTime(match[0].at), "kind": "oldest_stored_row"}
	}
	if faults["gap"] {
		gaps = append(gaps, map[string]any{"kind": "internal_capture_gap", "from": hubTime(from), "to": hubTime(to),
			"plane": pl, "remedy": map[string]any{"kind": "none", "reason": "no_remedy_exists"}})
		blockers = append(blockers, "gaps_present")
	}
	srcErr := []any{}
	if faults["sourceErrors"] {
		srcErr = append(srcErr, map[string]any{"source": pl, "code": "statement_timeout", "excludedFromCounts": true})
		blockers = append(blockers, "source_errors_present")
	}
	filterReq := len(cur.Filters) > 0
	predicates := []any{
		map[string]any{"name": "window", "requested": true, "applied": true, "reason": "applied"},
		map[string]any{"name": "datasetFilter", "requested": filterReq, "applied": filterReq && !faults["unapplied"],
			"reason": map[bool]string{true: "applied", false: "not_requested"}[filterReq]},
	}
	if faults["unapplied"] {
		predicates[1].(map[string]any)["reason"] = "unsupported_for_plane"
	}
	resp := map[string]any{"datasetRef": ds, "pageLabel": page, "platform": "fansly",
		"window": map[string]any{"from": cur.From, "to": cur.To}, "predicates": predicates,
		"capture": map[string]any{"planes": []any{map[string]any{"plane": pl, "state": "read", "captureFloor": planeFloor}},
			"observedRowFloor": nil, "gaps": gaps, "sourceErrors": srcErr,
			"scopeNarrowing": map[string]any{"keyGrantExcludedPages": 0, "totalPagesForQuery": 1}, "scopeFieldStates": map[string]any{}}}
	if summary {
		groups := map[string]*[2]int64{}
		var order []string
		for _, r := range match {
			c, _ := r.fields["currency"].(string)
			if groups[c] == nil {
				groups[c] = &[2]int64{}
				order = append(order, c)
			}
			groups[c][0]++
			if n, ok := r.fields["netMills"].(int64); ok {
				groups[c][1] += n
			}
		}
		if f.drift[page] > 0 {
			f.drift[page]--
			if len(order) == 0 {
				groups["USD"], order = &[2]int64{}, []string{"USD"}
			}
			groups[order[0]][0]++
		}
		out := []any{}
		for _, c := range order {
			out = append(out, map[string]any{"currency": c, "transactionCount": groups[c][0], "grossMills": 0,
				"netMills": groups[c][1], "feeMills": nil})
		}
		resp["items"] = []any{}
		resp["summary"] = map[string]any{"basis": "matching_rows_in_hub", "matchedRows": len(match), "groups": out}
		resp["delivery"] = map[string]any{"returned": len(out), "matchedInScope": map[string]any{"value": len(out),
			"exact": true, "countBasis": "post_dedup"}, "cappedBy": nil, "nextCursor": nil, "snapshotExhausted": true, "caveats": []any{}}
	} else {
		end := cur.Offset + cur.Limit
		if end > len(match) {
			end = len(match)
		}
		chunk := match[cur.Offset:end]
		items := []any{}
		for _, r := range chunk {
			items = append(items, map[string]any{"datasetRef": ds, "key": r.key, "occurredAt": hubTime(r.at),
				"fanPlatformUserId": nil, "fields": r.fields, "fieldStates": map[string]any{},
				"provenance": map[string]any{"ingestPaths": []any{"unknown"}, "convergence": "final", "observationRef": nil}})
		}
		var next any
		var capped any
		if end < len(match) {
			c := cur
			c.Offset = end
			raw, _ := json.Marshal(c)
			next, capped = base64.RawURLEncoding.EncodeToString(raw), "limit"
		}
		exact := next == nil && !faults["inexact"]
		resp["items"] = items
		resp["delivery"] = map[string]any{"returned": len(items), "matchedInScope": map[string]any{"value": len(match),
			"exact": exact, "countBasis": "post_dedup"}, "cappedBy": capped, "nextCursor": next, "snapshotExhausted": false,
			"caveats": []any{"mutable_sort_key", "no_frozen_snapshot"}}
		blockers = append(blockers, "delivery_not_exhausted")
		if consumed {
			blockers = append(blockers, "mutable_sort_key_traversal")
		}
		if !claimed {
			blockers = append(blockers, "claim_not_declared")
		}
		blockers = append(blockers, f.extra[key]...)
	}
	if faults["nonUSD"] && summary {
		g := resp["summary"].(map[string]any)["groups"].([]any)
		resp["summary"].(map[string]any)["groups"] = append(g, map[string]any{"currency": "EUR", "transactionCount": 1,
			"grossMills": 0, "netMills": 1000, "feeMills": nil})
		resp["delivery"].(map[string]any)["returned"] = len(g) + 1
	}
	resp["conclusion"] = map[string]any{"blockers": blockers}
	return resp, 200
}

// ---- test bench ----

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func mustTime(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

type bench struct {
	t     *testing.T
	ctx   context.Context
	store *db.Store
	hub   *fakeHub
	clock *fakeClock
	sy    *Syncer
}

// newBench opens a migrated database with the synthetic seed of testdata/business.json and a
// syncer pointed at a fake hub whose clock and pauses are simulated.
func newBench(t *testing.T) *bench {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/business.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportBusiness(ctx, s.RW, raw, mustTime("2026-11-01")); err != nil {
		t.Fatal(err)
	}
	h := newFakeHub(t)
	h.grant = []GrantPage{
		{PageLabel: "alfa-1", Platform: "fansly", ModelSlug: "Alfa", ModelName: "Alfa"},
		{PageLabel: "alfa-of", Platform: "onlyfans", ModelSlug: "Alfa", ModelName: "Alfa"},
	}
	clock := &fakeClock{now: mustTime("2026-11-04T06:00:00+03:00")}
	sy := NewSyncer(s, Config{BaseURL: h.srv.URL, Key: "test-key"})
	sy.Now = clock.Now
	sy.Client.Sleep = clock.Sleep
	sy.Client.Throttle.Now, sy.Client.Throttle.Sleep = clock.Now, clock.Sleep
	return &bench{t: t, ctx: ctx, store: s, hub: h, clock: clock, sy: sy}
}

func (b *bench) sync(pages ...string) Report {
	b.t.Helper()
	rep, err := b.sy.Run(b.ctx, pages)
	if err != nil {
		b.t.Fatal(err)
	}
	return rep
}

func (b *bench) result(rep Report, page, ds string) Result {
	b.t.Helper()
	for _, r := range rep.Results {
		if r.Page == page && r.Dataset == ds {
			return r
		}
	}
	b.t.Fatalf("no result %s %s in %+v", page, ds, rep.Results)
	return Result{}
}

func (b *bench) count(query string, args ...any) int {
	b.t.Helper()
	var n int
	if err := b.store.RW.QueryRow(query, args...).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

func (b *bench) exec(query string, args ...any) {
	b.t.Helper()
	if _, err := b.store.RW.Exec(query, args...); err != nil {
		b.t.Fatal(err)
	}
}
