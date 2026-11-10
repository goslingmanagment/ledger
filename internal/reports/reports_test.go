package reports

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/goslingmanagment/ledger/internal/agentsql" // POST /v1/sql, GET /v1/schema
	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// book is a store with two cards: anna (Hub name «anna_hub») and boris (hub:42), and a Hub page of a model.
func book(t *testing.T) *db.Store {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := ts.Now()
	if err := payroll.PutPeople(ctx, s.RW, []payroll.Person{
		{Slug: "anna", Name: "Анна", Aliases: []string{"anna_hub"}, Status: "active", Workdays: []string{}},
		{Slug: "boris", Name: "Борис", Aliases: []string{"hub:42"}, Status: "active", Workdays: []string{}},
	}, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
		VALUES ('page-a', 'fansly', 'model-x', 'Model X', ?)`, ts.Format(now)); err != nil {
		t.Fatal(err)
	}
	// Hub's team facts know «carl» as user 77; no card names him.
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_works(window_from, ref, user_id, username, role, kind, started_at,
		ended_at, ongoing, worked_minutes, day, payload, read_at) VALUES ('2026-11-02T00:00:00+03:00', 'w-1', 77, 'Carl', 'chatter',
		'shift', '2026-11-08T16:00:00+03:00', '2026-11-09T00:00:00+03:00', 0, 480, '2026-11-08', '{}', ?)`, ts.Format(now)); err != nil {
		t.Fatal(err)
	}
	return s
}

// sample is a valid report: page-a 31.52 = 28.00 counted + 3.52 added (a 5.00 sale held out); page-b 181.60.
func sample(user string) map[string]any {
	return map[string]any{
		"hub_username": user, "chatter": "k1", "report_date": "2026-11-08",
		"window_from": "2026-11-08T16:00:00+03:00", "window_to": "2026-11-09T00:00:00+03:00", "cut_at": "2026-11-08T23:53:00+03:00",
		"source": "agent", "total_mills": 213120,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 31520}, map[string]any{"page": "page-b", "amount_mills": 181600}},
		"lines": []any{
			map[string]any{"n": 1, "transaction_ref": "tx-1", "page": "page-a", "fan_ref": "fan-1", "fan_name": "nick",
				"kind": "message_purchase", "occurred_at": "2026-11-08T17:28:00+03:00", "net_mills": 28000, "gross_mills": 35000,
				"counted": true, "decision": "auto", "flags": []any{}, "notes": []any{"text"}, "ppv_author": "anna", "ppv_sent_at": "08.11 17:26"},
			map[string]any{"n": 2, "transaction_ref": "tx-2", "page": "page-a", "net_mills": 5000, "counted": false,
				"decision": "exclude", "flags": []any{"чужой PPV"}},
			map[string]any{"n": 3, "transaction_ref": "tx-3", "page": "page-b", "net_mills": 181600, "counted": true, "decision": "auto"},
		},
		"adds": []any{map[string]any{"page": "page-a", "amount_mills": 3520, "what": "вне окна"}},
	}
}

func raw(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func count(t *testing.T, s *db.Store, table string) int {
	t.Helper()
	var n int
	if err := s.RO.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestValidationRefusesAndWritesNothing(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	cases := map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"page sum ≠ total": {func(m map[string]any) { m["total_mills"] = 213121 }, "сумма страниц"},
		"page lines ≠ amount": {func(m map[string]any) {
			m["pages"].([]any)[0].(map[string]any)["amount_mills"] = 31519
			m["total_mills"] = 213119
		}, "страница page-a"},
		"held-out line counted": {func(m map[string]any) {
			m["lines"].([]any)[1].(map[string]any)["counted"] = true
		}, "страница page-a"},
		"line on unknown page": {func(m map[string]any) {
			m["lines"].([]any)[2].(map[string]any)["page"] = "page-z"
		}, "page-z"},
		"add on unknown page": {func(m map[string]any) {
			m["adds"].([]any)[0].(map[string]any)["page"] = "page-z"
		}, "page-z"},
		"unknown field":    {func(m map[string]any) { m["bonus"] = 1 }, "bonus"},
		"no pages":         {func(m map[string]any) { m["pages"] = []any{} }, "страниц"},
		"duplicate page":   {func(m map[string]any) { m["pages"] = append(m["pages"].([]any), m["pages"].([]any)[0]) }, "повторяется"},
		"bad date":         {func(m map[string]any) { m["report_date"] = "2026-11-31" }, "report_date"},
		"bad time":         {func(m map[string]any) { m["cut_at"] = "23:53" }, "cut_at"},
		"wrong source":     {func(m map[string]any) { m["source"] = "post" }, "source"},
		"no username":      {func(m map[string]any) { m["hub_username"] = " " }, "hub_username"},
		"no total":         {func(m map[string]any) { delete(m, "total_mills") }, "total_mills"},
		"bad decision":     {func(m map[string]any) { m["lines"].([]any)[0].(map[string]any)["decision"] = "maybe" }, "decision"},
		"add without what": {func(m map[string]any) { m["adds"].([]any)[0].(map[string]any)["what"] = "" }, "what"},
		"trailing data":    {nil, "лишние"},
	}
	for name, c := range cases {
		m := sample("anna_hub")
		body := raw(t, m)
		if c.edit != nil {
			c.edit(m)
			body = raw(t, m)
		} else {
			body = append(body, []byte(` {}`)...)
		}
		_, err := Save(ctx, s, body, "agent", ts.Now())
		r, ok := err.(*Refusal)
		if !ok {
			t.Errorf("%s: %v, want a refusal", name, err)
			continue
		}
		if !strings.Contains(r.Msg, c.want) {
			t.Errorf("%s: %q, want it to name %q", name, r.Msg, c.want)
		}
	}
	for _, tbl := range []string{"shift_reports", "shift_report_pages", "shift_report_lines"} {
		if n := count(t, s, tbl); n != 0 {
			t.Errorf("%s: %d rows after refusals", tbl, n)
		}
	}
	// The refusal names the page and both numbers.
	m := sample("anna_hub")
	m["pages"].([]any)[1].(map[string]any)["amount_mills"] = 181000
	m["total_mills"] = 212520
	_, err := Save(ctx, s, raw(t, m), "agent", ts.Now())
	if err == nil || !strings.Contains(err.Error(), "page-b") || !strings.Contains(err.Error(), "181.60") || !strings.Contains(err.Error(), "181.00") {
		t.Errorf("page refusal: %v", err)
	}
}

func TestVersions(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	first, err := Save(ctx, s, raw(t, sample("anna_hub")), "agent", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Version != 1 || first.Person == nil || *first.Person != "anna" {
		t.Fatalf("first: %+v", first)
	}
	// The same content, the name in another case and a time written another way: unchanged.
	m := sample("ANNA_hub")
	m["window_from"] = "2026-11-08T13:00:00Z"
	again, err := Save(ctx, s, raw(t, m), "agent", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.ID != first.ID || again.Version != 1 {
		t.Fatalf("again: %+v", again)
	}
	if n := count(t, s, "shift_reports"); n != 1 {
		t.Fatalf("%d versions after the same payload", n)
	}
	// A changed amount under the name in another case: version 2, version 1 superseded.
	m = sample("Anna_Hub")
	m["pages"].([]any)[1].(map[string]any)["amount_mills"] = 181000
	m["lines"].([]any)[2].(map[string]any)["net_mills"] = 181000
	m["total_mills"] = 212520
	second, err := Save(ctx, s, raw(t, m), "agent", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created || second.Version != 2 || second.ID == first.ID {
		t.Fatalf("second: %+v", second)
	}
	old, err := Get(ctx, s.RO, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := Get(ctx, s.RO, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusSuperseded || cur.Status != StatusDraft || cur.Versions != 2 || cur.TotalMicros != 212_520_000 {
		t.Fatalf("old %s, current %s, versions %d, total %d", old.Status, cur.Status, cur.Versions, cur.TotalMicros)
	}
	var vn int
	if err := s.RO.QueryRow(`SELECT version FROM v_shift_reports`).Scan(&vn); err != nil || vn != 2 {
		t.Fatalf("v_shift_reports: %d %v", vn, err)
	}
	// The breakdown as stored: adds are counted lines without n or ref.
	if len(cur.Pages) != 2 || cur.Pages[0].Model != "model-x" || cur.Pages[1].Model != "" || cur.Pages[0].AmountMicros != 31_520_000 {
		t.Fatalf("pages %+v", cur.Pages)
	}
	if len(cur.Lines) != 4 {
		t.Fatalf("lines %+v", cur.Lines)
	}
	l, add := cur.Lines[0], cur.Lines[3]
	if *l.N != 1 || *l.TransactionRef != "tx-1" || l.NetMicros != 28_000_000 || *l.GrossMicros != 35_000_000 || !l.Counted ||
		l.Notes[0] != "text" || *l.OccurredAt != "2026-11-08T17:28:00+03:00" || l.PPVSentAt != "08.11 17:26" {
		t.Fatalf("line %+v", l)
	}
	if cur.Lines[1].Counted || cur.Lines[1].Flags[0] != "чужой PPV" {
		t.Fatalf("held out %+v", cur.Lines[1])
	}
	if !add.Added || !add.Counted || add.N != nil || add.TransactionRef != nil || add.NetMicros != 3_520_000 || add.What != "вне окна" {
		t.Fatalf("add %+v", add)
	}
}

func TestPersonResolution(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	for _, c := range []struct {
		user   string
		id     any
		person string
		userID int64
	}{
		{"anna_hub", nil, "anna", 0}, // a Hub name in the card's aliases
		{"Boris", 42, "boris", 42},   // hub:42; the card's code is the login too
		{"someone", nil, "", 0},      // unknown: stored without a person
		{"carl", nil, "", 77},        // unknown card, but Hub's facts know the id
		{"dora", 99, "", 99},         // unknown id
	} {
		m := sample(c.user)
		if c.id != nil {
			m["hub_user_id"] = c.id
		}
		out, err := Save(ctx, s, raw(t, m), "agent", ts.Now())
		if err != nil {
			t.Fatalf("%s: %v", c.user, err)
		}
		r, err := Get(ctx, s.RO, out.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if r.Person != nil {
			got = *r.Person
		}
		var id int64
		if r.HubUserID != nil {
			id = *r.HubUserID
		}
		if got != c.person || id != c.userID {
			t.Errorf("%s: person %q id %d, want %q %d", c.user, got, id, c.person, c.userID)
		}
	}
}

func TestScopesAndFilters(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	agent, combo := key("agent", auth.ScopeReports), key("combo", auth.ScopeRead, auth.ScopePropose, auth.ScopeReports)
	hub, reader := key("hub", auth.ScopePayouts), key("reader", auth.ScopeRead)
	status := func(err error) int {
		t.Helper()
		if err == nil {
			return http.StatusOK
		}
		e, ok := err.(*client.Error)
		if !ok {
			t.Fatalf("not an HTTP answer: %v", err)
		}
		return e.Status
	}
	post := func(c *client.Client, body any) (SaveResult, int) {
		var out SaveResult
		code := status(c.Do(ctx, http.MethodPost, "/v1/reports", json.RawMessage(raw(t, body)), &out))
		return out, code
	}
	// anna under her Hub name, anna again under her code as a login on another day, boris, and an unknown user.
	annaHub, code := post(agent, sample("anna_hub"))
	if code != http.StatusOK || !annaHub.Created {
		t.Fatalf("reports key POST: %d %+v", code, annaHub)
	}
	m := sample("ANNA")
	m["report_date"] = "2026-11-09"
	annaLogin, code := post(combo, m)
	if code != http.StatusOK || annaLogin.Person == nil || *annaLogin.Person != "anna" {
		t.Fatalf("combined key POST: %d %+v", code, annaLogin)
	}
	m = sample("boris")
	m["hub_user_id"] = 42
	if _, code := post(agent, m); code != http.StatusOK {
		t.Fatalf("boris: %d", code)
	}
	if _, code := post(agent, sample("someone")); code != http.StatusOK {
		t.Fatalf("someone: %d", code)
	}
	bad := sample("anna_hub")
	bad["total_mills"] = 1
	if _, code := post(agent, bad); code != http.StatusBadRequest {
		t.Fatalf("invalid report: %d, want 400", code)
	}

	const window = "/v1/reports?from=2026-11-01&to=2026-11-10"
	// reports-only: POST and nothing else.
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, window}, {http.MethodGet, "/v1/reports/1"}, {http.MethodPost, "/v1/sql"}, {http.MethodGet, "/v1/schema"},
		{http.MethodGet, "/v1/payouts?from=2026-11-01&to=2026-11-10"},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{"sql": "SELECT 1"}
		}
		if code := status(agent.Do(ctx, r.method, r.path, body, nil)); code != http.StatusForbidden {
			t.Errorf("reports key on %s %s: %d, want 403", r.method, r.path, code)
		}
	}
	// payouts and read: GET, no POST.
	for name, c := range map[string]*client.Client{"payouts": hub, "read": reader} {
		if _, code := post(c, sample("anna_hub")); code != http.StatusForbidden {
			t.Errorf("%s key POST: %d, want 403", name, code)
		}
		var ans ListAnswer
		if err := c.Do(ctx, http.MethodGet, window, nil, &ans); err != nil || len(ans.Reports) != 4 {
			t.Errorf("%s key GET: %v, %d reports", name, err, len(ans.Reports))
		}
		var one Report
		if err := c.Do(ctx, http.MethodGet, "/v1/reports/"+itoa(annaHub.ID), nil, &one); err != nil || one.ID != annaHub.ID {
			t.Errorf("%s key GET one: %v %+v", name, err, one)
		}
	}
	if code := status(hub.Do(ctx, http.MethodPost, "/v1/sql", map[string]any{"sql": "SELECT 1"}, nil)); code != http.StatusForbidden {
		t.Errorf("payouts key on SQL: %d", code)
	}
	if code := status(hub.Do(ctx, http.MethodGet, "/v1/reports/999", nil, nil)); code != http.StatusNotFound {
		t.Errorf("missing report: %d", code)
	}

	users := func(q string) []string {
		t.Helper()
		var ans ListAnswer
		if err := hub.Do(ctx, http.MethodGet, window+q, nil, &ans); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var out []string
		for _, r := range ans.Reports {
			out = append(out, r.ReportDate+" "+r.HubUsername)
		}
		return out
	}
	same := func(q string, want ...string) {
		t.Helper()
		got := users(q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", q, got, want)
		}
	}
	// anna asks by either of her names: both her reports, never boris's or the unknown user's.
	same("&hub_username=anna_hub", "2026-11-08 anna_hub", "2026-11-09 ANNA")
	same("&hub_username=Anna", "2026-11-08 anna_hub", "2026-11-09 ANNA")
	same("&person=anna", "2026-11-08 anna_hub", "2026-11-09 ANNA")
	// boris by his Hub id, with or without a name.
	same("&hub_user_id=42", "2026-11-08 boris")
	same("&hub_user_id=42&hub_username=boris", "2026-11-08 boris")
	// An unknown user sees only the report saved under their own name.
	same("&hub_username=someone", "2026-11-08 someone")
	same("&hub_username=nobody")
	same("&hub_user_id=5")
	if code := status(hub.Do(ctx, http.MethodGet, window+"&hub_user_id=x", nil, nil)); code != http.StatusBadRequest {
		t.Errorf("bad hub_user_id: %d", code)
	}
	if code := status(hub.Do(ctx, http.MethodGet, "/v1/reports?from=2026-09-01&to=2026-11-10", nil, nil)); code != http.StatusBadRequest {
		t.Errorf("window over 62 days: %d", code)
	}
	// all_versions.
	m = sample("anna_hub")
	m["chatter"] = "k2"
	if out, code := post(agent, m); code != http.StatusOK || out.Version != 2 {
		t.Fatalf("version 2: %d %+v", code, out)
	}
	same("&hub_username=anna_hub&person=anna", "2026-11-08 anna_hub", "2026-11-09 ANNA")
	same("&hub_username=anna_hub&all_versions=1", "2026-11-08 anna_hub", "2026-11-08 anna_hub", "2026-11-09 ANNA")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
