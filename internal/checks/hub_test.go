package checks

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/payouts/checks opens to the payouts scope only, and nothing else of read opens with it; a failing check says
// since when it fails without a passing run between.
func TestHubChecksOpenOnlyToThePayoutsScope(t *testing.T) {
	s, ctx := openDB(t)
	t0 := time.Now().Add(-3 * time.Hour)
	run := func(at time.Time, rs ...Result) {
		t.Helper()
		if err := Save(ctx, s.RW, rs, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// Balanced fails, passes, then fails twice: it fails since the third run. Copies fails from the start.
	run(t0, Result{Name: "проводки сходятся", Severity: "urgent", Summary: "x"}, Result{Name: "копии базы", Severity: "warn", Summary: "y"},
		Result{Name: weekColumnsCheck, OK: true, Severity: "info", Summary: "z"})
	run(t0.Add(time.Hour), Result{Name: "проводки сходятся", OK: true, Severity: "info", Summary: "ok"},
		Result{Name: "копии базы", Severity: "warn", Summary: "y"})
	run(t0.Add(2*time.Hour), Result{Name: "проводки сходятся", Severity: "urgent", Summary: "x"},
		Result{Name: "копии базы", Severity: "warn", Summary: "y"})
	third := t0.Add(150 * time.Minute)
	run(third, Result{Name: "проводки сходятся", Severity: "urgent", Summary: "x2"},
		Result{Name: weekColumnsCheck, OK: true, Severity: "info", Summary: "z2",
			Details: []TabColumn{{Model: "Alpha", Tab: "Альфа", Person: "alfa", Tomorrow: "2026-11-25", LastColumn: "2026-11-24"}}})

	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
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
	const path = "/v1/payouts/checks"
	hub := key("hub", auth.ScopePayouts)
	var got HubChecks
	if err := hub.Do(ctx, http.MethodGet, path, nil, &got); err != nil {
		t.Fatal(err)
	}
	by := map[string]HubCheck{}
	for _, c := range got.Checks {
		by[c.Key] = c
	}
	if c := by["balanced"]; c.OK || c.Severity != "urgent" || c.Summary != "x2" || c.FailingSince == nil || *c.FailingSince != ts.Format(t0.Add(2*time.Hour)) {
		t.Fatalf("balanced %+v", c)
	}
	if c := by["copies"]; c.OK || c.FailingSince == nil || *c.FailingSince != ts.Format(t0) {
		t.Fatalf("copies %+v", c)
	}
	if c := by["week_columns"]; !c.OK || c.FailingSince != nil || len(got.WeekColumns.Tabs) != 1 || got.WeekColumns.Tabs[0].Tab != "Альфа" {
		t.Fatalf("week columns %+v %+v", c, got.WeekColumns)
	}
	if got.At != ts.Format(third) || len(got.Checks) != 3 {
		t.Fatalf("answer %+v", got)
	}
	// Every other scope is refused, read included; no key or an unknown one is 401.
	for _, sc := range []string{auth.ScopeRead, auth.ScopePropose, auth.ScopeDecide, auth.ScopeCollect, auth.ScopeImport, auth.ScopeReports} {
		if code := status(key("k-"+sc, sc).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusForbidden {
			t.Errorf("%s key: %d, want 403", sc, code)
		}
	}
	if code := status(client.New(config.Client{URL: srv.URL, Key: "ldg_nobody_x"}).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("unknown key: %d", code)
	}
	if code := status(client.New(config.Client{URL: srv.URL}).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}
	// The payouts key still opens nothing of read: not the health checks with their details, not a run of them.
	for _, p := range []string{"/v1/health/checks", "/v1/health/checks?now=1"} {
		if code := status(hub.Do(ctx, http.MethodGet, p, nil, nil)); code != http.StatusForbidden {
			t.Errorf("payouts key on %s: %d, want 403", p, code)
		}
	}
	// Every check ledger runs has a key of its own.
	for _, r := range append(Run(ctx, Env{Q: s.RO, Now: time.Now()}), WithStale(nil, "", time.Now())...) {
		if _, ok := Keys[r.Name]; !ok {
			t.Errorf("check %q has no key", r.Name)
		}
	}
}

// GET /v1/health/jobs opens to the payouts scope only and lists every registered job, run or not.
func TestHubJobsOpenOnlyToThePayoutsScope(t *testing.T) {
	s, ctx := openDB(t)
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
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
	const path = "/v1/health/jobs"
	var got server.JobsAnswer
	if err := key("hub", auth.ScopePayouts).Do(ctx, http.MethodGet, path, nil, &got); err != nil {
		t.Fatal(err)
	}
	var job *server.JobStatus
	for i := range got.Jobs {
		if got.Jobs[i].Job == "checks" {
			job = &got.Jobs[i]
		}
	}
	if !got.OK || got.Scheduler || got.StartedAt == "" || job == nil || job.IntervalSeconds != 3600 || job.LastStartedAt != nil ||
		job.LastDoneAt != nil || job.LastFailedAt != nil || job.LastError != nil || job.FailedInRow != 0 {
		t.Fatalf("jobs %+v", got)
	}
	for _, sc := range []string{auth.ScopeRead, auth.ScopePropose, auth.ScopeDecide, auth.ScopeCollect, auth.ScopeImport, auth.ScopeReports} {
		if code := status(key("k-"+sc, sc).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusForbidden {
			t.Errorf("%s key: %d, want 403", sc, code)
		}
	}
	if code := status(client.New(config.Client{URL: srv.URL}).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}
}

// A version Hub holds back from ledger's key fails «сданные в хабе отчёты сохраняются» — the check Hub raises as a matter —
// naming the person, the report date and the pages outside the key, until the version comes or leaves Hub's list.
func TestAHeldVersionFailsTheHubReportsCheck(t *testing.T) {
	s, ctx := openDB(t)
	exec(t, s, `INSERT INTO hub_shift_report_held(id, seq, shift_id, person, report_date, reason, page_labels, first_seen_at)
		VALUES (12, 2, 70, 'alfa', '2026-11-10', 'page_outside_key', '["page-x","page-y"]', '2026-11-12T10:00:00+03:00'),
		       (13, 3, 71, NULL, '2026-11-11', 'page_outside_key', '["page-x"]', '2026-11-12T10:00:00+03:00')`)
	r := HubReports(ctx, Env{Q: s.RO, Now: now})
	if r.OK || r.Severity != "warn" || KeyOf(r.Name) != "hub_reports" ||
		!strings.Contains(r.Summary, "2 сданных в хабе версий хаб придержал") ||
		!strings.Contains(r.Summary, "версия 12 (смена 70, alfa, 2026-11-10): page-x, page-y") ||
		!strings.Contains(r.Summary, "версия 13 (смена 71, логин без человека, 2026-11-11): page-x") {
		t.Fatalf("held versions: %+v", r)
	}
	exec(t, s, `UPDATE hub_shift_report_held SET cleared_at = '2026-11-12T11:00:00+03:00'`)
	if r := HubReports(ctx, Env{Q: s.RO, Now: now}); !r.OK {
		t.Fatalf("after the versions came: %+v", r)
	}
}
