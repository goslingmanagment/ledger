package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
