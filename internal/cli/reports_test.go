package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func reportsTestServer(t *testing.T) {
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
	}, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
		VALUES ('page-a', 'fansly', 'model-x', 'Model X', ?)`, ts.Format(now)); err != nil {
		t.Fatal(err)
	}
	agentRaw, _ := json.Marshal(map[string]any{
		"hub_username": "anna_hub", "chatter": "k1", "report_date": "2026-11-08",
		"window_from": "2026-11-08T16:00:00+03:00", "window_to": "2026-11-09T00:00:00+03:00", "cut_at": "2026-11-08T23:50:00+03:00",
		"source": "agent", "total_mills": 28000,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 28000}},
		"lines": []any{map[string]any{"n": 1, "transaction_ref": "tx-1", "page": "page-a", "fan_name": "nick",
			"kind": "message_purchase", "net_mills": 28000, "counted": true, "decision": "auto"}},
	})
	if _, err := reports.Save(ctx, s, agentRaw, "agent", now); err != nil {
		t.Fatal(err)
	}
	// Seed two Hub versions through SyncHub: a stored version (id=103) and a problem version (id=106).
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("since") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"reports": []any{}, "next": r.URL.Query().Get("since"), "serverNow": "2026-11-12T10:00:00Z"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"reports": []any{
				map[string]any{
					"id": 103, "person": "anna", "hubUserId": 41, "shiftId": 11, "reportDate": "2026-11-10", "version": 1,
					"submittedAt": "2026-11-11T09:40:00Z", "door": "telegram", "telegramMessageRef": nil, "totalMills": 100000,
					"pages": []any{map[string]any{"pageLabel": "page-a", "amountMills": 100000}},
					"lines": []any{
						map[string]any{"transactionId": 9001, "pageLabel": "page-a", "group": "mine", "netMills": 80000, "ruleShareMills": 80000,
							"ruleHome": "shift", "ruleReason": "holder", "statement": nil, "shareWithUserId": nil, "note": nil,
							"amountMills": 80000, "claimedMills": nil, "added": false, "disputed": false},
						map[string]any{"transactionId": 9002, "pageLabel": "page-a", "group": "near", "netMills": 20000, "ruleShareMills": 0,
							"ruleHome": "shift", "ruleReason": "last_writer", "statement": "mine", "shareWithUserId": nil, "note": "спор",
							"amountMills": 0, "claimedMills": 20000, "added": false, "disputed": true},
						map[string]any{"transactionId": 9004, "pageLabel": "page-a", "group": "mine", "netMills": 40000, "ruleShareMills": 40000,
							"ruleHome": "shift", "ruleReason": "holder", "statement": "share", "shareWithUserId": 42, "note": "пополам",
							"amountMills": 20000, "claimedMills": nil, "added": true, "disputed": false},
					},
					"offPlatform": []any{
						map[string]any{"id": 1, "amountMills": 15000, "fromWhom": "фан-1", "whatFor": "кастом", "state": "pending"},
					},
					"comment": "добавила долёт", "dayPaid": false, "ruleVersion": "c2",
				},
				map[string]any{
					"id": 106, "person": "anna", "hubUserId": 41, "shiftId": 12, "reportDate": "2026-11-11", "version": 1,
					"submittedAt": "2026-11-12T00:30:00Z", "door": "workboard", "telegramMessageRef": nil, "totalMills": 7000,
					"pages": []any{map[string]any{"pageLabel": "page-z", "amountMills": 7000}},
					"lines": []any{}, "offPlatform": []any{}, "comment": "", "dayPaid": false, "ruleVersion": "c2",
				},
			},
			"next": "106", "serverNow": "2026-11-12T10:00:00Z",
		})
	}))
	t.Cleanup(hubSrv.Close)
	if _, err := reports.SyncHub(ctx, s, hub.NewClient(hub.Config{BaseURL: hubSrv.URL, Key: "k"}, nil), time.Date(2026, 11, 12, 13, 0, 0, 0, ts.MSK)); err != nil {
		t.Fatal(err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	t.Cleanup(srv.Close)
	token, _, err := auth.Create(ctx, s.RW, "reader", []string{auth.ScopeRead}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_URL", srv.URL)
	t.Setenv("LEDGER_KEY", token)
}

func TestCLIReportListAndShowHub(t *testing.T) {
	reportsTestServer(t)
	ctx := t.Context()

	// Default report list: only agent draft, includes source column.
	var buf bytes.Buffer
	if err := runReportList(ctx, []string{"--from", "2026-11-01", "--to", "2026-11-15"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "источник") || !strings.Contains(out, "agent") || strings.Contains(out, "hub:103") {
		t.Fatalf("default list:\n%s", out)
	}

	// report list --source all: shows agent draft AND Hub versions with hub:<id>, source column, and problem.
	buf.Reset()
	if err := runReportList(ctx, []string{"--from", "2026-11-01", "--to", "2026-11-15", "--source", "all"}, &buf); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	for _, want := range []string{"источник", "agent", "hub:103", "hub", "submitted", "hub:106", "problem", "page-z"} {
		if !strings.Contains(out, want) {
			t.Errorf("list --source all missing %q:\n%s", want, out)
		}
	}

	// report list --source hub --json: only Hub versions in JSON.
	buf.Reset()
	if err := runReportList(ctx, []string{"--from", "2026-11-01", "--to", "2026-11-15", "--source", "hub", "--json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var listAns reports.ListAnswer
	if err := json.Unmarshal(buf.Bytes(), &listAns); err != nil || len(listAns.Reports) != 2 {
		t.Fatalf("list --source hub --json: %v %+v", err, listAns)
	}
	for _, r := range listAns.Reports {
		if r.Source != reports.SourceHub {
			t.Errorf("list --source hub --json returned source %q", r.Source)
		}
	}

	// report list --source invalid: exit code 2 without calling API.
	buf.Reset()
	err := runReportList(ctx, []string{"--from", "2026-11-01", "--to", "2026-11-15", "--source", "post"}, &buf)
	var ex Exit
	if !errors.As(err, &ex) || ex.Code != 2 {
		t.Fatalf("invalid --source: %v, want Exit{2}", err)
	}

	// report show hub:103: prints Hub metadata, share amount, disputed claim, долёт, and off-platform add.
	buf.Reset()
	if err := runReportShow(ctx, []string{"hub:103"}, &buf); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	for _, want := range []string{
		"отчёт hub:103 (хаб)", "смена 11", "дверь telegram", "комментарий: добавила долёт",
		"page-a", "model-x", "100.00", "продажа 9001", "спорно (заявлено 20.00)",
		"да (20.00)", "share", "долёт", "фан-1 — кастом", "pending", "мимо площадки",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show hub:103 missing %q:\n%s", want, out)
		}
	}

	// report show hub:103 --json: valid JSON with source "hub".
	buf.Reset()
	if err := runReportShow(ctx, []string{"hub:103", "--json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var rep reports.Report
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil || rep.Source != reports.SourceHub || rep.ID != 103 || len(rep.Lines) != 4 {
		t.Fatalf("show hub:103 --json: %v %+v", err, rep)
	}

	// report show hub:106: prints problem cleanly.
	buf.Reset()
	if err := runReportShow(ctx, []string{"hub:106"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "проблема:") || !strings.Contains(buf.String(), "page-z") {
		t.Fatalf("show hub:106:\n%s", buf.String())
	}

	// report show 1: agent report still works.
	buf.Reset()
	if err := runReportShow(ctx, []string{"1"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "отчёт 1:") || !strings.Contains(buf.String(), "message_purchase nick") {
		t.Fatalf("show 1:\n%s", buf.String())
	}

	// report show hub:9999: 404 -> Exit{Code: 2}.
	buf.Reset()
	err = runReportShow(ctx, []string{"hub:9999"}, &buf)
	if !errors.As(err, &ex) || ex.Code != 2 || !strings.Contains(ex.Msg, "404") {
		t.Fatalf("show hub:9999: %v", err)
	}

	// report show hub:bad: Exit{Code: 2}.
	err = runReportShow(ctx, []string{"hub:bad"}, &buf)
	if !errors.As(err, &ex) || ex.Code != 2 {
		t.Fatalf("show hub:bad: %v", err)
	}
}
