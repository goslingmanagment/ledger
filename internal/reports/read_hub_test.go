package reports

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func seedHubAndAgent(t *testing.T) (*db.Store, SaveResult) {
	t.Helper()
	s := book(t)
	ctx := t.Context()
	if _, err := s.RW.Exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
		VALUES ('page-b', 'onlyfans', 'model-x', 'Model X', '2026-11-01')`); err != nil {
		t.Fatal(err)
	}
	// Agent draft on 2026-11-08 (id=1).
	saved, err := Save(ctx, s, raw(t, sample("anna_hub")), "agent", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Sync fixture Hub versions (ids 101..106) plus a version with id=1 (collides with agent id=1)
	// and a version for Carl (user_id 77 from hub_works, no people card) and a disputed claim + confirmed add.
	items := fixture(t)
	items = append(items,
		raw(t, map[string]any{
			"id": 1, "person": "anna", "hubUserId": 41, "shiftId": 10, "reportDate": "2026-11-09", "version": 1,
			"submittedAt": "2026-11-09T21:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": 95000,
			"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": 95000}},
			"lines": []any{
				map[string]any{"transactionId": 9101, "pageLabel": "page-a", "group": "mine", "netMills": 80000, "ruleShareMills": 80000,
					"ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
					"amountMills": 80000, "claimedMills": nil, "added": false, "disputed": false},
				map[string]any{"transactionId": 9102, "pageLabel": "page-a", "group": "near", "netMills": 20000, "ruleShareMills": 0,
					"ruleHome": "shift", "ruleReason": "last_writer", "statement": "mine", "shareWithUserId": nil, "note": "спорная",
					"amountMills": 0, "claimedMills": 20000, "added": false, "disputed": true},
			},
			"offPlatform": []any{
				map[string]any{"id": 9, "amountMills": 15000, "fromWhom": "фан-1", "whatFor": "кастом", "state": "confirmed", "pageLabel": "page-a"},
			},
			"comment": "сверка", "dayPaid": false, "ruleVersion": "c2",
		}),
		raw(t, map[string]any{
			"id": 107, "person": nil, "hubUserId": 77, "shiftId": 70, "reportDate": "2026-11-10", "version": 1,
			"submittedAt": "2026-11-10T21:00:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": 10000,
			"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": 10000}},
			"lines": []any{}, "offPlatform": []any{}, "comment": "", "dayPaid": false, "ruleVersion": "c2",
		}),
	)
	_, c := newStubHub(t, items)
	if st, err := SyncHub(ctx, s, c, hubNow); err != nil || st.Stored != 5 || st.Problems != 3 {
		t.Fatalf("sync hub: %+v %v", st, err)
	}
	// Enrich one Hub sale with hub_sales and an active Hub sale decision to verify join + agreement.
	if _, err := s.RW.Exec(`INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at,
		post_tip, campaign, payload, first_seen_at, read_at) VALUES ('9001', '2026-11-09T00:00:00+03:00', 'page-a', 'fansly', 'ppv',
		80000, '2026-11-10T18:15:00+03:00', 0, 0, '{}', '2026-11-10', '2026-11-10')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.Exec(`INSERT INTO hub_sale_decisions(id, transaction_ref, page_label, kind, shares, state, source, said_by, note,
		applied_person, applied_report_date, seq, change, changed_at, read_at) VALUES (31, '9001', 'page-a', 'owner',
		'[{"person":"anna","userId":41,"percent":100}]', 'active', 'agent', 'владелец', 'отдал Анне', 'anna', '2026-11-10',
		1, 'made', '2026-11-10', '2026-11-10')`); err != nil {
		t.Fatal(err)
	}
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-10", ts.Now()); err != nil {
		t.Fatal(err)
	}
	return s, saved
}

// Default GET /v1/reports (no ?source=) MUST return ONLY agent drafts with the exact pre-L5 JSON keys
// so Hub's strict Zod schema in ledger-reports.ts never breaks.
func TestReportsDefaultIsBackwardCompatible(t *testing.T) {
	s, saved := seedHubAndAgent(t)
	ctx := t.Context()
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	token, _, err := auth.Create(ctx, s.RW, "hub", []string{auth.ScopePayouts}, false, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(config.Client{URL: srv.URL, Key: token})

	for _, path := range []string{
		"/v1/reports?from=2026-11-01&to=2026-11-15",
		"/v1/reports?from=2026-11-01&to=2026-11-15&source=agent",
	} {
		rawText, err := c.Text(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var body struct {
			From    string           `json:"from"`
			To      string           `json:"to"`
			Reports []map[string]any `json:"reports"`
		}
		if err := json.Unmarshal([]byte(rawText), &body); err != nil {
			t.Fatalf("%s: json: %v", path, err)
		}
		if len(body.Reports) != 1 {
			t.Fatalf("%s: got %d reports, want only 1 agent draft", path, len(body.Reports))
		}
		rep := body.Reports[0]
		if rep["source"] != SourceAgent || int64(rep["id"].(float64)) != saved.ID {
			t.Fatalf("%s: got report %+v", path, rep)
		}
		// Assert exact top-level keys of an agent report: zero Hub-only keys leak into default response.
		wantReportKeys := []string{
			"chatter", "created_at", "cut_at", "hub_user_id", "hub_username", "id", "lines", "pages",
			"person", "posted_at", "report_date", "source", "status", "total_micros", "version", "versions",
			"window_from", "window_to",
		}
		gotReportKeys := make([]string, 0, len(rep))
		for k := range rep {
			gotReportKeys = append(gotReportKeys, k)
		}
		slices.Sort(gotReportKeys)
		if !slices.Equal(gotReportKeys, wantReportKeys) {
			t.Fatalf("%s: report keys %v, want %v", path, gotReportKeys, wantReportKeys)
		}
		lines := rep["lines"].([]any)
		if len(lines) != 4 {
			t.Fatalf("%s: %d lines, want 4", path, len(lines))
		}
		wantLineKeys := []string{
			"added", "agreement", "counted", "decision", "fan_name", "fan_ref", "flags", "gross_micros",
			"kind", "n", "net_micros", "notes", "occurred_at", "page", "ppv_author", "ppv_sent_at",
			"transaction_ref", "what",
		}
		for i, rawLine := range lines {
			m := rawLine.(map[string]any)
			gotLineKeys := make([]string, 0, len(m))
			for k := range m {
				gotLineKeys = append(gotLineKeys, k)
			}
			slices.Sort(gotLineKeys)
			if !slices.Equal(gotLineKeys, wantLineKeys) {
				t.Fatalf("%s line %d: keys %v, want %v", path, i, gotLineKeys, wantLineKeys)
			}
		}
	}

	// Invalid ?source= returns 400.
	err = c.Do(ctx, http.MethodGet, "/v1/reports?from=2026-11-01&to=2026-11-15&source=post", nil, nil)
	ce, ok := err.(*client.Error)
	if !ok || ce.Status != http.StatusBadRequest {
		t.Fatalf("invalid source: %v, want 400", err)
	}
}

func TestReportsSourceHubAllAndGetHub(t *testing.T) {
	s, saved := seedHubAndAgent(t)
	ctx := t.Context()
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()

	makeKey := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	reader := makeKey("reader", auth.ScopeRead)
	payouts := makeKey("hub", auth.ScopePayouts)
	agentOnly := makeKey("agent", auth.ScopeReports)

	statusOf := func(err error) int {
		if err == nil {
			return http.StatusOK
		}
		return err.(*client.Error).Status
	}

	// GET /v1/reports?source=hub returns ONLY Hub current versions (ids 1, 102, 103, 104, 105, 106, 107; NOT superseded 101).
	var hubAns ListAnswer
	if err := reader.Do(ctx, http.MethodGet, "/v1/reports?from=2026-11-01&to=2026-11-15&source=hub", nil, &hubAns); err != nil {
		t.Fatal(err)
	}
	var gotIDs []int64
	byID := map[int64]Report{}
	for _, r := range hubAns.Reports {
		if r.Source != SourceHub {
			t.Errorf("source=hub returned source %q (id %d)", r.Source, r.ID)
		}
		gotIDs = append(gotIDs, r.ID)
		byID[r.ID] = r
	}
	slices.Sort(gotIDs)
	wantIDs := []int64{1, 102, 103, 104, 105, 106, 107}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("source=hub ids %v, want %v", gotIDs, wantIDs)
	}

	// Verify rich Hub details on id=103 (v2/2, 3 lines + 1 pending off-platform add, enriched sale + agreement).
	r103 := byID[103]
	if r103.Version != 2 || r103.Versions != 2 || r103.Status != StatusSubmitted || r103.Door != "telegram" ||
		r103.Comment != "добавила долёт" || r103.ShiftID == nil || *r103.ShiftID != 11 || r103.TotalMicros != 120_000_000 {
		t.Fatalf("r103 header: %+v", r103)
	}
	if len(r103.Pages) != 1 || r103.Pages[0].Page != "page-a" || r103.Pages[0].Model != "model-x" || r103.Pages[0].PostState != StateHubOnly {
		t.Fatalf("r103 pages: %+v", r103.Pages)
	}
	if len(r103.Lines) != 4 {
		t.Fatalf("r103 lines len %d: %+v", len(r103.Lines), r103.Lines)
	}
	// Line 0: tx 9001 enriched from hub_sales + Hub sale decision agreement.
	l0 := r103.Lines[0]
	if *l0.TransactionRef != "9001" || l0.Kind != "ppv" || l0.OccurredAt == nil || *l0.OccurredAt != "2026-11-10T18:15:00+03:00" ||
		l0.Agreement == nil || l0.Agreement.Source != SourceHubDecision || l0.Agreement.ID != 31 {
		t.Fatalf("r103 line 0: %+v", l0)
	}
	// Line 2: tx 9004 share + added («долёт»): net 40, amount 20.
	l2 := r103.Lines[2]
	if *l2.TransactionRef != "9004" || !l2.Added || !l2.Counted || l2.Decision != "share" ||
		l2.NetMicros != 40_000_000 || l2.AmountMicros == nil || *l2.AmountMicros != 20_000_000 ||
		!slices.Contains(l2.Flags, "долёт") || !slices.Contains(l2.Flags, "с hub:42") {
		t.Fatalf("r103 line 2: %+v", l2)
	}
	// Line 3: pending off-platform add (counted=false, amount=0, net=15).
	l3 := r103.Lines[3]
	if !l3.Added || l3.Counted || l3.N != nil || l3.Kind != "off_platform" || l3.Decision != "pending" ||
		l3.NetMicros != 15_000_000 || l3.AmountMicros == nil || *l3.AmountMicros != 0 || l3.What != "фан с сайта — кастом" {
		t.Fatalf("r103 off-platform add: %+v", l3)
	}

	// Verify id=1 (disputed claimed_micros + confirmed off-platform add on page-a).
	r1 := byID[1]
	if len(r1.Lines) != 3 {
		t.Fatalf("r1 lines: %+v", r1.Lines)
	}
	if !r1.Lines[1].Disputed || r1.Lines[1].Counted || r1.Lines[1].ClaimedMicros == nil || *r1.Lines[1].ClaimedMicros != 20_000_000 ||
		r1.Lines[1].AmountMicros == nil || *r1.Lines[1].AmountMicros != 0 {
		t.Fatalf("r1 disputed line: %+v", r1.Lines[1])
	}
	if !r1.Lines[2].Added || !r1.Lines[2].Counted || r1.Lines[2].Page != "page-a" || r1.Lines[2].Decision != "confirmed" ||
		r1.Lines[2].AmountMicros == nil || *r1.Lines[2].AmountMicros != 15_000_000 {
		t.Fatalf("r1 confirmed add: %+v", r1.Lines[2])
	}

	// Verify problem versions (105 unknown person vera, 106 unknown page page-z).
	if r105 := byID[105]; r105.Status != StatusProblem || r105.Problem == nil || !strings.Contains(*r105.Problem, "vera") ||
		r105.Person != nil || r105.HubUsername != "vera" || len(r105.Pages) != 0 || len(r105.Lines) != 0 {
		t.Fatalf("r105 problem: %+v", r105)
	}
	if r106 := byID[106]; r106.Status != StatusProblem || r106.Problem == nil || !strings.Contains(*r106.Problem, "page-z") ||
		r106.Person == nil || *r106.Person != "anna" {
		t.Fatalf("r106 problem: %+v", r106)
	}
	// Carl (user 77 in hub_works, no people card, no hub_person): HubUsername comes from hub_works.
	if r107 := byID[107]; r107.HubUsername != "Carl" || r107.Person != nil || r107.Status != StatusProblem {
		t.Fatalf("r107 carl: %+v", r107)
	}

	// Filters on ?source=hub: from/to, person, hub_user_id, hub_username, all_versions.
	idsOf := func(q string) []int64 {
		t.Helper()
		var ans ListAnswer
		if err := payouts.Do(ctx, http.MethodGet, "/v1/reports?source=hub&"+q, nil, &ans); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var out []int64
		for _, r := range ans.Reports {
			out = append(out, r.ID)
		}
		return out
	}
	checkIDs := func(q string, want ...int64) {
		t.Helper()
		got := idsOf(q)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", q, got, want)
		}
	}
	checkIDs("from=2026-11-11&to=2026-11-11", 106)
	checkIDs("from=2026-11-01&to=2026-11-15&person=anna", 1, 103, 106)
	checkIDs("from=2026-11-01&to=2026-11-15&person=anna&all_versions=1", 1, 101, 103, 106)
	checkIDs("from=2026-11-01&to=2026-11-15&person=vera", 105)
	checkIDs("from=2026-11-01&to=2026-11-15&hub_user_id=41", 1, 103, 106)
	checkIDs("from=2026-11-01&to=2026-11-15&hub_user_id=42", 102, 104)
	checkIDs("from=2026-11-01&to=2026-11-15&hub_username=anna_hub", 1, 103, 106)
	checkIDs("from=2026-11-01&to=2026-11-15&hub_username=carl", 107)
	checkIDs("from=2026-11-01&to=2026-11-15&hub_username=vera", 105)
	checkIDs("from=2026-11-01&to=2026-11-15&hub_username=nobody")
	checkIDs("from=2026-11-01&to=2026-11-15&hub_user_id=42&person=anna")

	// ?source=all returns BOTH agent drafts AND Hub versions, even when agent id=1 and Hub id=1 collide.
	var allAns ListAnswer
	if err := reader.Do(ctx, http.MethodGet, "/v1/reports?from=2026-11-01&to=2026-11-15&person=anna&source=all", nil, &allAns); err != nil {
		t.Fatal(err)
	}
	if len(allAns.Reports) != 4 {
		t.Fatalf("source=all anna: got %d reports, want 4 (1 agent + 3 hub)", len(allAns.Reports))
	}
	if allAns.Reports[0].Source != SourceAgent || allAns.Reports[0].ID != saved.ID || len(allAns.Reports[0].Pages) != 2 {
		t.Fatalf("source=all[0] agent id=1 mixed up: %+v", allAns.Reports[0])
	}
	if allAns.Reports[1].Source != SourceHub || allAns.Reports[1].ID != 1 || len(allAns.Reports[1].Pages) != 1 || len(allAns.Reports[1].Lines) != 3 {
		t.Fatalf("source=all[1] hub id=1 mixed up: %+v", allAns.Reports[1])
	}

	// GET /v1/reports/hub/{id}: works for read and payouts, forbidden for reports-only, 404 on unknown, 400 on bad id.
	for name, c := range map[string]*client.Client{"read": reader, "payouts": payouts} {
		var rep Report
		if err := c.Do(ctx, http.MethodGet, "/v1/reports/hub/103", nil, &rep); err != nil {
			t.Fatalf("%s GET /v1/reports/hub/103: %v", name, err)
		}
		if rep.Source != SourceHub || rep.ID != 103 || len(rep.Lines) != 4 {
			t.Fatalf("%s GET hub/103: %+v", name, rep)
		}
	}
	// Superseded version is still readable directly by id.
	var old Report
	if err := reader.Do(ctx, http.MethodGet, "/v1/reports/hub/101", nil, &old); err != nil || old.Status != StatusSuperseded {
		t.Fatalf("GET hub/101: %v %+v", err, old)
	}
	if code := statusOf(agentOnly.Do(ctx, http.MethodGet, "/v1/reports/hub/103", nil, nil)); code != http.StatusForbidden {
		t.Errorf("reports-only key on GET /v1/reports/hub/103: %d, want 403", code)
	}
	if code := statusOf(reader.Do(ctx, http.MethodGet, "/v1/reports/hub/9999", nil, nil)); code != http.StatusNotFound {
		t.Errorf("missing hub report: %d, want 404", code)
	}
	if code := statusOf(reader.Do(ctx, http.MethodGet, "/v1/reports/hub/abc", nil, nil)); code != http.StatusBadRequest {
		t.Errorf("bad hub report id: %d, want 400", code)
	}
}
