package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// stubHub answers Hub's feed of the handed-in shift reports by its documented shape (hub#1225): the versions after
// since, at most limit, next — the last id served or since. status other than 200 answers that status.
type stubHub struct {
	mu       sync.Mutex
	items    []json.RawMessage
	status   int
	requests []string
}

func newStubHub(t *testing.T, items []json.RawMessage) (*stubHub, *hub.Client) {
	t.Helper()
	h := &stubHub{items: items, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, r.URL.RawQuery)
		if r.Method != http.MethodGet || r.URL.Path != hub.ShiftReportsPath || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
			return
		}
		if h.status != http.StatusOK {
			w.WriteHeader(h.status)
			fmt.Fprintf(w, `{"error":"status_%d","message":"stub"}`, h.status)
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out := []json.RawMessage{}
		next := since
		for _, it := range h.items {
			var x struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(it, &x)
			if x.ID > since && len(out) < limit {
				out = append(out, it)
				next = x.ID
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reports": out, "next": strconv.FormatInt(next, 10), "serverNow": "2026-11-12T10:00:00.000Z"})
	}))
	t.Cleanup(srv.Close)
	return h, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "test-key"}, nil)
}

func fixture(t *testing.T) []json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/hub-shift-reports.json")
	if err != nil {
		t.Fatal(err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func lines(t *testing.T, s *db.Store, q string, args ...any) string {
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

var hubNow = time.Date(2026, 11, 12, 13, 0, 0, 0, ts.MSK)

// Three people's reports handed in in Hub: anna corrected hers (two versions of shift 11), boris worked two shifts on
// the 10th (21 and 22, on page-a both), a version names a person code people does not have, another a page hub_pages
// does not have.
func TestHubReportsAreStoredTheDayAddsAndTheNewerVersionSupersedes(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	if _, err := s.RW.Exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-b', 'onlyfans', 'model-x', 'Model X', '2026-11-01')`); err != nil {
		t.Fatal(err)
	}
	stub, c := newStubHub(t, fixture(t))
	defer func(n int) { hubLimit = n }(hubLimit)
	hubLimit = 2

	st, err := SyncHub(ctx, s, c, hubNow)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Read != 6 || st.Stored != 4 || st.Problems != 2 || st.Next != "106" || st.Requests != 4 {
		t.Fatalf("status %+v", st)
	}
	if got, want := strings.Join(stub.requests, " | "), "limit=2 | limit=2&since=102 | limit=2&since=104 | limit=2&since=106"; got != want {
		t.Fatalf("requests %s, want %s", got, want)
	}
	// The newer version of shift 11 is its report; the problems are kept, without pages.
	if got, want := lines(t, s, `SELECT id, shift_id, version, COALESCE(person, '∅'), current, total_micros, door, problem IS NOT NULL,
		(SELECT COUNT(*) FROM hub_shift_report_pages p WHERE p.report_id = r.id) FROM hub_shift_reports r ORDER BY id`), strings.Join([]string{
		"101 11 1 anna 0 100000000 workboard 0 1",
		"102 21 1 boris 1 30000000 extension 0 1",
		"103 11 2 anna 1 120000000 telegram 0 1",
		"104 22 1 boris 1 25000000 workboard 0 2",
		"105 31 1 ∅ 1 10000000 workboard 1 0",
		"106 12 1 anna 1 7000000 workboard 1 0",
	}, "\n"); got != want {
		t.Fatalf("versions:\n%s\nwant\n%s", got, want)
	}
	// The lines keep the sale, what it counts and why, and the person's word; «мимо площадки» is kept, never counted.
	if got, want := lines(t, s, `SELECT transaction_ref, grp, net_micros, amount_micros, counted, COALESCE(statement, '∅'),
		COALESCE(share_with_user_id, '∅'), added, disputed FROM hub_shift_report_lines WHERE report_id = 103 ORDER BY seq`), strings.Join([]string{
		"9001 mine 80000000 80000000 1 ∅ ∅ 0 0",
		"9002 near 20000000 20000000 1 mine ∅ 0 1",
		"9004 mine 40000000 20000000 1 share 42 1 0",
	}, "\n"); got != want {
		t.Fatalf("lines:\n%s\nwant\n%s", got, want)
	}
	if got := lines(t, s, `SELECT amount_micros, state, counted FROM hub_shift_report_adds WHERE report_id = 103`); got != "15000000 pending 0" {
		t.Fatalf("off platform: %s", got)
	}
	// The pay base: anna's corrected 120 alone; boris's two shifts add, a page rounded by Hub to the cent as it is.
	if got, want := lines(t, s, `SELECT person, report_date, page, hub_micros, state, COALESCE(hub_state, '∅'), basis,
		COALESCE(accrue_micros, '∅') FROM v_report_pages ORDER BY person, page`), strings.Join([]string{
		"anna 2026-11-10 page-a 120000000 hub_only hub_only post ∅",
		"boris 2026-11-10 page-a 35000000 hub_only hub_only post ∅",
		"boris 2026-11-10 page-b 20000000 hub_only hub_only post ∅",
	}, "\n"); got != want {
		t.Fatalf("v_report_pages:\n%s\nwant\n%s", got, want)
	}
	problems, err := HubProblems(ctx, s.RO)
	if err != nil || len(problems) != 2 || !strings.Contains(problems[0].Problem, "кода человека «vera» нет в people") ||
		!strings.Contains(problems[1].Problem, "страницы «page-z» нет в hub_pages") {
		t.Fatalf("problems %+v %v", problems, err)
	}

	// Read again from the cursor less the overlap: the same versions, nothing new. From the first: the same, no new rows.
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Read != 6 || st.Same != 6 || st.Stored != 0 || st.Problems != 0 || st.Next != "106" {
		t.Fatalf("again: %+v %v", st, err)
	}
	if _, err := s.RW.Exec(`DELETE FROM cursors WHERE name = ?`, hubCursor); err != nil {
		t.Fatal(err)
	}
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Read != 6 || st.Stored != 0 || st.Same != 6 || st.Problems != 0 {
		t.Fatalf("from the first: %+v %v", st, err)
	}
	if n := count(t, s, "hub_shift_reports"); n != 6 {
		t.Fatalf("%d versions after reading twice", n)
	}
	if n := count(t, s, "hub_shift_report_lines"); n != 8 {
		t.Fatalf("%d lines after reading twice", n)
	}

	// The card and the page come: the kept versions are stored on the next pass.
	if err := payroll.PutPeople(ctx, s.RW, []payroll.Person{{Slug: "vera", Name: "Вера", Aliases: []string{}, Status: "active", Workdays: []string{}}},
		"test", hubNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.Exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-z', 'fansly', 'model-x', 'Model X', '2026-11-01')`); err != nil {
		t.Fatal(err)
	}
	st, err = SyncHub(ctx, s, c, hubNow)
	if err != nil || st.Stored+st.Fixed != 2 { // read again in the overlap, or tried again
		t.Fatalf("fixed: %+v %v", st, err)
	}
	if problems, err := HubProblems(ctx, s.RO); err != nil || len(problems) != 0 {
		t.Fatalf("problems after the fix %+v %v", problems, err)
	}
	if got := lines(t, s, `SELECT person, hub_micros FROM v_report_pages WHERE person = 'vera'`); got != "vera 10000000" {
		t.Fatalf("vera: %s", got)
	}
}

// A version whose sums do not add up is kept with the problem, not stored: the page is not its lines' sum to the cent,
// or the pages are not the total.
func TestAHubVersionWhoseSumsDoNotAddUpIsAProblem(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	item := func(id int64, total, page, line int64) json.RawMessage {
		return raw(t, map[string]any{"id": id, "person": "anna", "hubUserId": 41, "shiftId": id, "reportDate": "2026-11-10", "version": 1,
			"submittedAt": "2026-11-10T20:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": total,
			"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": page}},
			"lines": []any{map[string]any{"transactionId": 1000 + id, "pageLabel": "page-a", "group": "mine", "netMills": line,
				"ruleShareMills": line, "ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
				"amountMills": line, "added": false, "disputed": false}},
			"offPlatform": []any{}, "comment": "", "dayPaid": nil, "ruleVersion": "c2"})
	}
	_, c := newStubHub(t, []json.RawMessage{
		item(1, 12350, 12350, 12345), // rounded half-up to the cent: stored
		item(2, 12350, 12350, 12340), // a cent off: a problem
		item(3, 12000, 12350, 12350), // the pages are not the total
	})
	st, err := SyncHub(ctx, s, c, hubNow)
	if err != nil || st.Stored != 1 || st.Problems != 2 {
		t.Fatalf("status %+v %v", st, err)
	}
	problems, err := HubProblems(ctx, s.RO)
	if err != nil || len(problems) != 2 || !strings.Contains(problems[0].Problem, "строки дают 12340 mills, у страницы 12350") ||
		!strings.Contains(problems[1].Problem, "сумма страниц 12350 mills не равна итогу 12000") {
		t.Fatalf("problems %+v %v", problems, err)
	}
}

// An older Hub has no feed: 404 is «nothing yet», a pass that succeeds and says so; a refused key fails the pass.
func TestAHubWithoutTheFeedIsNothingYet(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	stub, c := newStubHub(t, fixture(t))
	stub.status = http.StatusNotFound
	st, err := SyncHub(ctx, s, c, hubNow)
	if err != nil || !st.OK || !st.Absent || st.Read != 0 || !strings.Contains(st.Error, "404") {
		t.Fatalf("404: %+v %v", st, err)
	}
	saved, err := ReadHubStatus(ctx, s.RO)
	if err != nil || saved == nil || !saved.Absent || saved.LastOKAt != ts.Format(hubNow) {
		t.Fatalf("saved %+v %v", saved, err)
	}
	stub.status = http.StatusForbidden
	st, err = SyncHub(ctx, s, c, hubNow.Add(5*time.Minute))
	if err == nil || st.OK || !strings.Contains(st.Error, "read:team") || st.LastOKAt != ts.Format(hubNow) {
		t.Fatalf("403: %+v %v", st, err)
	}
	if n := count(t, s, "hub_shift_reports"); n != 0 {
		t.Fatalf("%d versions from a refusing hub", n)
	}
}

// «Мимо площадки» the owner confirmed (hub#1252): Hub makes a new version whose page sum includes the confirmed amount on
// the item's page. It counts as a line of that page, the day's Hub sum includes it, and it is kept as counted there. A
// pending one adds nothing, as before; a confirmed one on a page ledger does not know is a problem that names it.
func TestAConfirmedOffPlatformAmountCountsOnItsPage(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	// version: anna's shift on the day with one sale of 100 on page-a, the pages and the total as given, and its «мимо площадки».
	version := func(id, shift int64, v int, day string, pageMills int64, madeFor any, off ...map[string]any) json.RawMessage {
		offs := []any{}
		for _, o := range off {
			offs = append(offs, o)
		}
		return raw(t, map[string]any{"id": id, "person": "anna", "hubUserId": 41, "shiftId": shift, "reportDate": day, "version": v,
			"submittedAt": day + "T20:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": pageMills,
			"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": pageMills}},
			"lines": []any{map[string]any{"transactionId": 5000 + id, "pageLabel": "page-a", "group": "mine", "netMills": 100_000,
				"ruleShareMills": 100_000, "ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
				"amountMills": 100_000, "added": false, "disputed": false, "settled": nil}},
			"offPlatform": offs, "comment": "", "dayPaid": nil, "ruleVersion": "c2", "madeFor": madeFor})
	}
	off := func(id int64, state string, page any) map[string]any {
		return map[string]any{"id": id, "amountMills": 15_000, "fromWhom": "фан", "whatFor": "кастом", "state": state, "pageLabel": page}
	}
	byOwner := map[string]any{"kind": "off_platform", "claimId": nil, "offPlatformId": 71, "transactionId": nil, "outcome": "confirmed",
		"byUserId": 1}
	_, c := newStubHub(t, []json.RawMessage{
		version(1, 1, 1, "2026-11-10", 100_000, nil, off(71, "pending", nil)),
		version(2, 1, 2, "2026-11-10", 115_000, byOwner, off(71, "confirmed", "page-a")), // the owner's word: 100 + 15
		version(3, 2, 1, "2026-11-11", 100_000, nil, off(72, "pending", "page-a")),       // pending: as before
		version(4, 3, 1, "2026-11-12", 115_000, byOwner, off(73, "confirmed", "page-q")), // a page ledger does not know
	})
	st, err := SyncHub(ctx, s, c, hubNow)
	if err != nil || st.Stored != 3 || st.Problems != 1 {
		t.Fatalf("status %+v %v", st, err)
	}
	if got, want := lines(t, s, `SELECT report_id, hub_id, amount_micros, state, counted, COALESCE(page_label, '∅') FROM hub_shift_report_adds
		ORDER BY report_id`), strings.Join([]string{
		"1 71 15000000 pending 0 ∅",
		"2 71 15000000 confirmed 1 page-a",
		"3 72 15000000 pending 0 ∅",
	}, "\n"); got != want {
		t.Fatalf("off platform:\n%s\nwant\n%s", got, want)
	}
	if got, want := lines(t, s, `SELECT report_date, page, hub_micros FROM v_report_pages WHERE person = 'anna' ORDER BY report_date`),
		"2026-11-10 page-a 115000000\n2026-11-11 page-a 100000000"; got != want {
		t.Fatalf("the day's Hub sum:\n%s\nwant\n%s", got, want)
	}
	problems, err := HubProblems(ctx, s.RO)
	if err != nil || len(problems) != 1 || problems[0].ID != 4 ||
		!strings.Contains(problems[0].Problem, "«мимо площадки» 1 (запись 73): страницы «page-q» нет в hub_pages") {
		t.Fatalf("problems %+v %v", problems, err)
	}
}

// A version Hub serves late — committed after a version with a bigger id was read (an older Hub ordered its feed by id), or
// placed later in the order of a newer Hub (seq) — is read on the next pass, never lost behind the cursor.
func TestALateVersionBehindTheCursorIsReadOnTheNextPass(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	item := func(id, shift int64) json.RawMessage {
		return raw(t, map[string]any{"id": id, "seq": id, "person": "anna", "hubUserId": 41, "shiftId": shift, "reportDate": "2026-11-10",
			"version": 1, "submittedAt": "2026-11-10T20:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": 10_000,
			"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": 10_000}},
			"lines": []any{map[string]any{"transactionId": 7000 + id, "pageLabel": "page-a", "group": "mine", "netMills": 10_000,
				"ruleShareMills": 10_000, "ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
				"amountMills": 10_000, "added": false, "disputed": false}},
			"offPlatform": []any{}, "comment": "", "dayPaid": nil, "ruleVersion": "c2"})
	}
	stub, c := newStubHub(t, []json.RawMessage{item(1, 1), item(3, 3)})
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Stored != 2 || st.Next != "3" {
		t.Fatalf("first pass %+v %v", st, err)
	}
	// Version 2 commits now, behind the cursor 3.
	stub.mu.Lock()
	stub.items = []json.RawMessage{item(1, 1), item(2, 2), item(3, 3)}
	stub.mu.Unlock()
	st, err := SyncHub(ctx, s, c, hubNow.Add(5*time.Minute))
	if err != nil || st.Stored != 1 || st.Next != "3" {
		t.Fatalf("second pass %+v %v", st, err)
	}
	if n := count(t, s, "hub_shift_reports"); n != 3 {
		t.Fatalf("%d versions, want 3: the late one is lost", n)
	}
	// The cursor never moves back, and reading again writes nothing.
	if st, err := SyncHub(ctx, s, c, hubNow.Add(10*time.Minute)); err != nil || st.Stored != 0 || st.Next != "3" {
		t.Fatalf("third pass %+v %v", st, err)
	}
}

// A 404 after the feed has been read (a cursor past zero) is not «nothing yet»: the route went away or the address is
// wrong, and the pass fails until it answers again.
func TestA404AfterTheFeedWasReadIsAFailure(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	stub, c := newStubHub(t, fixture(t))
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Next != "106" {
		t.Fatalf("first pass %+v %v", st, err)
	}
	stub.status = http.StatusNotFound
	st, err := SyncHub(ctx, s, c, hubNow.Add(5*time.Minute))
	if err == nil || st.OK || st.Absent || !strings.Contains(st.Error, "404") || !strings.Contains(st.Error, "106") {
		t.Fatalf("404 past the cursor: %+v %v", st, err)
	}
	if st.LastOKAt != ts.Format(hubNow) {
		t.Fatalf("the last good read moved: %+v", st)
	}
}

// feedHub answers Hub's feed the way Hub serves it since hub#1279: the versions by their places (seq), `limit` counting
// places. A version with a page outside the key's pages is held back — kept for the key and listed in `held` on every
// read. Once the key may read its pages Hub gives the version a new place at the end of the feed, and it is served by
// the ordinary cursor. `next` is the last place read, held or not.
type feedHub struct {
	mu    sync.Mutex
	items []*feedItem
	grant map[string]bool // the key's pages
	kept  map[int64]bool  // the versions held back for the key
	gone  map[int64]bool  // versions Hub no longer has (a page erased)
}

type feedItem struct {
	id, seq, shift int64
	person, date   string
	page           string
	body           map[string]any
}

// feedVersion is a version of one sale of 10.00 on a page, at a place of the feed.
func feedVersion(id, seq, shift int64, version int, date, page string) *feedItem {
	return &feedItem{id: id, seq: seq, shift: shift, person: "anna", date: date, page: page,
		body: map[string]any{"id": id, "person": "anna", "hubUserId": 41, "shiftId": shift, "reportDate": date,
			"version": version, "submittedAt": date + "T20:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": 10_000,
			"pages": []any{map[string]any{"pageLabel": page, "amountMills": 10_000}},
			"lines": []any{map[string]any{"transactionId": 8000 + id, "pageLabel": page, "group": "mine", "netMills": 10_000,
				"ruleShareMills": 10_000, "ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
				"amountMills": 10_000, "claimedMills": nil, "added": false, "disputed": false, "settled": nil}},
			"offPlatform": []any{}, "comment": "", "dayPaid": nil, "ruleVersion": "c2", "madeFor": nil}}
}

func newFeedHub(t *testing.T, grant ...string) (*feedHub, *hub.Client) {
	t.Helper()
	f := &feedHub{grant: map[string]bool{}, kept: map[int64]bool{}, gone: map[int64]bool{}}
	for _, p := range grant {
		f.grant[p] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		// A version kept for the key that it may read now gets a new place at the end of the feed.
		last := int64(0)
		for _, it := range f.items {
			last = max(last, it.seq)
		}
		for _, it := range f.items {
			if f.gone[it.id] {
				delete(f.kept, it.id)
			} else if f.kept[it.id] && f.grant[it.page] {
				last++
				it.seq = last
				delete(f.kept, it.id)
			}
		}
		sort.Slice(f.items, func(i, j int) bool { return f.items[i].seq < f.items[j].seq })
		// The places after the cursor, at most limit of them: served, or held back.
		served, held, read, next := []any{}, []any{}, 0, since
		for _, it := range f.items {
			if f.gone[it.id] || it.seq <= since || read >= limit {
				continue
			}
			read, next = read+1, it.seq
			if f.grant[it.page] {
				it.body["seq"] = it.seq
				served = append(served, it.body)
			} else {
				f.kept[it.id] = true
			}
		}
		for _, it := range f.items {
			if f.kept[it.id] {
				held = append(held, map[string]any{"id": it.id, "seq": it.seq, "shiftId": it.shift, "person": it.person,
					"reportDate": it.date, "reason": "page_outside_key", "pageLabels": []string{it.page}})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reports": served, "held": held,
			"next": strconv.FormatInt(next, 10), "serverNow": "2026-11-12T10:00:00.000Z"})
	}))
	t.Cleanup(srv.Close)
	return f, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "test-key"}, nil)
}

func (f *feedHub) add(items ...*feedItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, items...)
}

// A version Hub holds back from ledger's key (a line on a page outside it) must not stop the feed: Hub's `limit` counts
// the held place too, so the page that holds it serves fewer versions than asked — and is not the end. The versions
// placed after it are read, and a new one placed after the cursor is read while the held one still waits. Held, the
// version stands as a problem; once the key may read the page it comes like any other and the problem is cleared.
func TestAHeldVersionDoesNotStopTheFeedAndIsAProblemUntilItComes(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	defer func(n int, o int64) { hubLimit, hubOverlap = n, o }(hubLimit, hubOverlap)
	hubLimit, hubOverlap = 2, 2
	feed, c := newFeedHub(t, "page-a")
	feed.add(
		feedVersion(11, 1, 1, 1, "2026-11-10", "page-a"),
		feedVersion(12, 2, 2, 1, "2026-11-10", "page-x"), // outside the key
		feedVersion(13, 3, 3, 1, "2026-11-10", "page-a"),
		feedVersion(14, 4, 4, 1, "2026-11-10", "page-a"),
		feedVersion(15, 5, 5, 1, "2026-11-10", "page-a"),
	)
	st, err := SyncHub(ctx, s, c, hubNow)
	if err != nil || st.Stored != 4 || st.Next != "5" || st.Held != 1 {
		t.Fatalf("first pass: the versions after the held one are not read: %+v %v", st, err)
	}
	// The held version stands as a problem: who, the report date, the pages outside the key.
	held, err := HeldVersions(ctx, s.RO)
	if err != nil || len(held) != 1 || held[0].ID != 12 || held[0].Seq != 2 || held[0].ShiftID != 2 || held[0].Person == nil ||
		*held[0].Person != "anna" || held[0].ReportDate != "2026-11-10" || strings.Join(held[0].PageLabels, ",") != "page-x" ||
		held[0].Since != ts.Format(hubNow) {
		t.Fatalf("held %+v %v", held, err)
	}
	// A new version is handed in while version 12 is still held, inside the window the next pass reads again.
	feed.add(feedVersion(16, 6, 6, 1, "2026-11-11", "page-a"))
	st, err = SyncHub(ctx, s, c, hubNow.Add(5*time.Minute))
	if err != nil || st.Stored != 1 || st.Next != "6" || st.Held != 1 {
		t.Fatalf("second pass: a held version stopped the feed: %+v %v", st, err)
	}
	// Listed again, the held version is the same row: held since the first pass, nothing rewritten.
	if got, want := lines(t, s, `SELECT id, first_seen_at, COALESCE(cleared_at, '∅') FROM hub_shift_report_held`), "12 "+ts.Format(hubNow)+" ∅"; got != want {
		t.Fatalf("held rows: %q, want %q", got, want)
	}
	// The key gets the page: Hub gives version 12 a new place at the end of the feed, 7.
	if _, err := s.RW.Exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-x', 'fansly', 'model-x', 'Model X', '2026-11-01')`); err != nil {
		t.Fatal(err)
	}
	feed.mu.Lock()
	feed.grant["page-x"] = true
	feed.mu.Unlock()
	st, err = SyncHub(ctx, s, c, hubNow.Add(10*time.Minute))
	if err != nil || st.Stored != 1 || st.Next != "7" || st.Held != 0 {
		t.Fatalf("third pass: the version Hub released: %+v %v", st, err)
	}
	// It came: the problem is cleared, the row stays as history.
	if held, err := HeldVersions(ctx, s.RO); err != nil || len(held) != 0 {
		t.Fatalf("still held %+v %v", held, err)
	}
	if got, want := lines(t, s, `SELECT id, cleared_at, id IN (SELECT id FROM hub_shift_reports) FROM hub_shift_report_held`),
		"12 "+ts.Format(hubNow.Add(10*time.Minute))+" 1"; got != want {
		t.Fatalf("held rows: %q, want %q", got, want)
	}
	if got, want := lines(t, s, `SELECT id FROM hub_shift_reports WHERE problem IS NULL ORDER BY id`), "11\n12\n13\n14\n15\n16"; got != want {
		t.Fatalf("versions stored:\n%s\nwant\n%s", got, want)
	}
	if got := lines(t, s, `SELECT page, hub_micros FROM v_report_pages WHERE person = 'anna' AND page = 'page-x'`); got != "page-x 10000000" {
		t.Fatalf("the late version in the pay base: %q", got)
	}
}

// A held version Hub stops naming without ever serving it (its page was erased in Hub) is no longer a problem; a version
// ledger already has is not held when Hub lists it (its page left the key after it was read); and an answer without the
// list (a Hub before hub#1279) says nothing about what is held.
func TestAHeldVersionThatLeavesTheListIsCleared(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	feed, c := newFeedHub(t, "page-a")
	feed.add(
		feedVersion(11, 1, 1, 1, "2026-11-10", "page-a"),
		feedVersion(12, 2, 2, 1, "2026-11-10", "page-x"),
		feedVersion(13, 3, 3, 1, "2026-11-10", "page-y"),
	)
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Stored != 1 || st.Held != 2 {
		t.Fatalf("first pass %+v %v", st, err)
	}
	// An older Hub answers without `held`: the two stay held.
	_, old := newStubHub(t, []json.RawMessage{})
	if st, err := SyncHub(ctx, s, old, hubNow.Add(time.Minute)); err != nil || st.Held != 2 {
		t.Fatalf("an answer without the list %+v %v", st, err)
	}
	// Hub erases page-x: version 12 is gone from its list. The key loses page-a: Hub lists version 11, which ledger has.
	feed.mu.Lock()
	feed.gone[12] = true
	delete(feed.grant, "page-a")
	feed.mu.Unlock()
	st, err := SyncHub(ctx, s, c, hubNow.Add(5*time.Minute))
	if err != nil || st.Held != 1 {
		t.Fatalf("second pass %+v %v", st, err)
	}
	if got, want := lines(t, s, `SELECT id, cleared_at IS NOT NULL FROM hub_shift_report_held ORDER BY id`), "12 1\n13 0"; got != want {
		t.Fatalf("held rows:\n%s\nwant\n%s", got, want)
	}
}

// A disputed «моё» (hub#1279) counts nothing until it is settled: the line's amountMills is 0, claimedMills says what it
// claims, and the page is still its lines' amountMills. The claim is kept with the line; the settling version drops it.
func TestADisputedLineCountsNothingAndKeepsItsClaim(t *testing.T) {
	s := book(t)
	ctx := context.Background()
	// version: anna's shift 1 with her own sale of 80 and a colleague's sale of 20 she calls hers.
	version := func(id int64, v int, total, amount int64, claimed any, disputed bool) json.RawMessage {
		line := func(tx, net, share, amount int64, group string, statement, claimed any, disputed bool) map[string]any {
			return map[string]any{"transactionId": tx, "pageLabel": "page-a", "group": group, "netMills": net, "ruleShareMills": share,
				"ruleHome": "shift", "ruleReason": "holder", "statement": statement, "shareWithUserId": nil, "note": nil,
				"amountMills": amount, "claimedMills": claimed, "added": false, "disputed": disputed, "settled": nil}
		}
		return raw(t, map[string]any{"id": id, "seq": id, "person": "anna", "hubUserId": 41, "shiftId": 1, "reportDate": "2026-11-10",
			"version": v, "submittedAt": "2026-11-10T20:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": total,
			"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": total}},
			"lines": []any{line(9001, 80_000, 80_000, 80_000, "mine", nil, nil, false),
				line(9002, 20_000, 0, amount, "near", "mine", claimed, disputed)},
			"offPlatform": []any{}, "comment": "", "dayPaid": nil, "ruleVersion": "c2", "madeFor": nil})
	}
	_, c := newStubHub(t, []json.RawMessage{version(1, 1, 80_000, 0, 20_000, true)})
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Stored != 1 || st.Problems != 0 {
		t.Fatalf("the disputed version: %+v %v", st, err)
	}
	if got, want := lines(t, s, `SELECT transaction_ref, amount_micros, counted, disputed, COALESCE(claimed_micros, '∅')
		FROM hub_shift_report_lines WHERE report_id = 1 ORDER BY seq`), "9001 80000000 1 0 ∅\n9002 0 0 1 20000000"; got != want {
		t.Fatalf("lines:\n%s\nwant\n%s", got, want)
	}
	if got := lines(t, s, `SELECT hub_micros FROM v_report_pages WHERE person = 'anna'`); got != "80000000" {
		t.Fatalf("the day's Hub sum counts the claim: %s", got)
	}
	// The owner gives her the sale: the settling version counts it and claims nothing.
	_, c = newStubHub(t, []json.RawMessage{version(1, 1, 80_000, 0, 20_000, true), version(2, 2, 100_000, 20_000, nil, false)})
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Stored != 1 {
		t.Fatalf("the settling version: %+v %v", st, err)
	}
	if got, want := lines(t, s, `SELECT transaction_ref, amount_micros, counted, disputed, COALESCE(claimed_micros, '∅')
		FROM hub_shift_report_lines WHERE report_id = 2 ORDER BY seq`), "9001 80000000 1 0 ∅\n9002 20000000 1 0 ∅"; got != want {
		t.Fatalf("settled lines:\n%s\nwant\n%s", got, want)
	}
	if got := lines(t, s, `SELECT hub_micros FROM v_report_pages WHERE person = 'anna'`); got != "100000000" {
		t.Fatalf("the day's Hub sum after the decision: %s", got)
	}
	// A claim out of range is not money to keep: the version waits with its problem.
	_, c = newStubHub(t, []json.RawMessage{version(3, 3, 80_000, 0, int64(9_000_000_000_000_000), true)})
	if _, err := s.RW.Exec(`DELETE FROM cursors WHERE name = ?`, hubCursor); err != nil {
		t.Fatal(err)
	}
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Problems != 1 {
		t.Fatalf("a claim out of range: %+v %v", st, err)
	}
	if p, err := HubProblems(ctx, s.RO); err != nil || len(p) != 1 || !strings.Contains(p[0].Problem, "claimedMills") {
		t.Fatalf("problems %+v %v", p, err)
	}
}
