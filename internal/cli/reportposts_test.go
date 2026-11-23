package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func TestCollectReportsDryRunSendsNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "reports.json")
	if err := os.WriteFile(cfg, []byte(`{"topics": [{"peer": -1001000000001, "topic": 4}], "since_days": 3, "tg_dir": "`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	old := tgTopicReader
	tgTopicReader = func(string) reports.TopicReader {
		return func(_ context.Context, peer string, topic int64, _, _ string, _ int) ([]byte, error) {
			calls++
			if peer != "-1001000000001" || topic != 4 {
				t.Errorf("read %s/%d", peer, topic)
			}
			return []byte(`{"messages": [{"id": 1, "date": "2026-11-08T20:00:00Z", "sender_id": 501,
				"text": "Дата: 08.11.2026\nЧаттер: #x\nPage-1 - $1\nОбщий доход: $1"}]}`), nil
		}
	}
	defer func() { tgTopicReader = old }()
	t.Setenv("LEDGER_URL", "http://127.0.0.1:1") // nothing listens: a send would fail the run
	t.Setenv("LEDGER_KEY", "none")
	spool, state := filepath.Join(dir, "spool"), filepath.Join(dir, "sent.json")
	if err := collectReports(context.Background(), []string{"--dry-run", "--config", cfg, "--spool", spool, "--state", state}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("%d reads", calls)
	}
	for _, p := range []string{spool, state} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("--dry-run touched %s", p)
		}
	}
}

func TestCollectReportsWindowFromServerAndFallbacks(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "reports.json")
	if err := os.WriteFile(cfg, []byte(`{"topics": [{"peer": -1001000000001, "topic": 4}], "since_days": 3, "tg_dir": "`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNow := ts.Now
	ts.Now = func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, ts.MSK) }
	defer func() { ts.Now = oldNow }()

	var gotSince string
	oldReader := tgTopicReader
	tgTopicReader = func(string) reports.TopicReader {
		return func(_ context.Context, _ string, _ int64, _, since string, _ int) ([]byte, error) {
			gotSince = since
			return []byte(`{"messages": []}`), nil
		}
	}
	defer func() { tgTopicReader = oldReader }()

	var serverHits int
	var serverStatus int
	var serverSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/report-posts/since":
			serverHits++
			if serverStatus != 0 && serverStatus != http.StatusOK {
				http.Error(w, `{"error":"not found"}`, serverStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(reports.PostsSinceAnswer{Since: serverSince})
		case "/v1/events":
			_ = json.NewEncoder(w).Encode(events.Answer{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("LEDGER_URL", srv.URL)
	t.Setenv("LEDGER_KEY", "test-key")
	run := func(extra ...string) {
		t.Helper()
		gotSince = ""
		args := append([]string{"--config", cfg, "--spool", filepath.Join(dir, "spool"), "--state", filepath.Join(dir, "sent.json")}, extra...)
		if err := collectReports(context.Background(), args); err != nil {
			t.Fatalf("collectReports(%v): %v", extra, err)
		}
	}

	// 1. Server returns an earlier date (2026-11-12 < today - 3d = 2026-11-17) → collector uses server's date.
	serverStatus, serverSince = http.StatusOK, "2026-11-12"
	run("--dry-run")
	if gotSince != "2026-11-12" {
		t.Fatalf("earlier serverSince: got %q, want 2026-11-12", gotSince)
	}

	// 2. Server returns a newer date (2026-11-19 > 2026-11-17) → today - since_days wins.
	serverStatus, serverSince = http.StatusOK, "2026-11-19"
	run("--dry-run")
	if gotSince != "2026-11-17" {
		t.Fatalf("newer serverSince: got %q, want 2026-11-17", gotSince)
	}

	// 3. Explicit --since overrides server and does not even call /v1/report-posts/since.
	hitsBefore := serverHits
	serverStatus, serverSince = http.StatusOK, "2026-11-10"
	run("--dry-run", "--since", "2026-11-18")
	if gotSince != "2026-11-18" || serverHits != hitsBefore {
		t.Fatalf("explicit --since: got %q hits delta %d", gotSince, serverHits-hitsBefore)
	}

	// 4. Older server returning 404 falls back gracefully to since_days (both dry-run and live).
	serverStatus, serverSince = http.StatusNotFound, ""
	run("--dry-run")
	if gotSince != "2026-11-17" {
		t.Fatalf("404 dry-run fallback: got %q, want 2026-11-17", gotSince)
	}
	run()
	if gotSince != "2026-11-17" {
		t.Fatalf("404 live fallback: got %q, want 2026-11-17", gotSince)
	}

	// 5. --dry-run without credentials/network falls back gracefully to since_days.
	t.Setenv("LEDGER_URL", "")
	t.Setenv("LEDGER_KEY", "")
	t.Setenv("HOME", dir) // no ~/.config/ledger/credentials
	run("--dry-run")
	if gotSince != "2026-11-17" {
		t.Fatalf("no-credentials dry-run fallback: got %q, want 2026-11-17", gotSince)
	}
}

func TestCollectReportsLocalDBWindowAndFiveDayOldEdit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	t.Setenv("LEDGER_DB", dbPath)
	ctx := context.Background()

	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, ts.MSK)
	oldNow := ts.Now
	ts.Now = func() time.Time { return now }
	defer func() { ts.Now = oldNow }()

	const peer = "-1001000000009"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.RW.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO settings(key, value, updated_at) VALUES
		('accrual_from', '2026-11-01', '2026-11-01'),
		('hub_model_slugs', '{"alpha-hub": "Alpha"}', '2026-11-01'),
		('report_topics', '[{"peer": "` + peer + `", "topic": 4, "pages": {"a": "page-a"}}]', '2026-11-01'),
		('report_authors', '{"tg:501": "alfa"}', '2026-11-01')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
		VALUES ('page-a', 'onlyfans', 'alpha-hub', 'Alpha', '2026-11-01')`)
	if err := payroll.PutPeople(ctx, store.RW, []payroll.Person{
		{Slug: "alfa", Name: "Альфа", Aliases: []string{}, Status: "active", Models: []string{"Alpha"}},
	}, "test", now); err != nil {
		t.Fatal(err)
	}
	// Seed a tab with 25% rate so report_accruals prices Альфа's posts (week of 2026-11-09, Excel serial 46335).
	hdr := []payroll.Cell{"Работяга", "Страница",
		json.Number("46335"), json.Number("46336"), json.Number("46337"), json.Number("46338"),
		json.Number("46339"), json.Number("46340"), json.Number("46341"),
		"Итого за неделю", "З/П", "Выплачено", "Фикс"}
	tabCells := [][]payroll.Cell{
		{"К выплате:", "", "Заработано:", "", "Выплачено:", "", "", "", "", "", "", "", "Ставка %", "Фикс/см $"},
		{json.Number("0"), "", json.Number("0"), "", json.Number("0"), "", "", "", "", "", "", "", json.Number("0.25"), json.Number("0")},
		{},
		hdr,
		{"Кто-то", "Page A", "", "", "", "", "", "", "", json.Number("0"), json.Number("0"), "", json.Number("0")},
		{"", "Сумма", json.Number("0"), json.Number("0"), json.Number("0"), json.Number("0"), json.Number("0"), json.Number("0"), json.Number("0"), json.Number("0"), json.Number("0")},
		{},
	}
	gotSheets, err := payroll.Collect(ctx, payroll.Config{Account: "a@example.com", Tables: []payroll.Table{{Model: "Alpha", Sheet: "id-Alpha"}}},
		func(_ context.Context, _, rng string) ([][]payroll.Cell, error) {
			name := strings.Trim(rng[:strings.Index(rng, "!")], "'")
			switch name {
			case "Итого":
				return [][]payroll.Cell{{"Чаттер"}, {"Альфа"}, {"ИТОГО"}}, nil
			case "Неактивные":
				return [][]payroll.Cell{{"Чаттер"}, {"ИТОГО"}}, nil
			}
			return tabCells, nil
		}, now)
	if err != nil || len(gotSheets.Errors) > 0 {
		t.Fatalf("sheets collect: %v %v", err, gotSheets.Errors)
	}
	if ans, err := events.Accept(ctx, store, gotSheets.Events, now); err != nil || len(ans.Settle) > 0 {
		t.Fatalf("sheets accept: %v %v", err, ans.Settle)
	}
	if err := payroll.SetReportsFrom(ctx, store.RW, "2026-11-10", now); err != nil {
		t.Fatal(err)
	}
	// Initial post on 2026-11-10 (5 days before now = 2026-11-15): $100 → $25 accrual.
	orig := reports.PostEvent{Peer: peer, Topic: 4, MsgID: 10, Date: "2026-11-10T23:30:00+03:00", Sender: "a", SenderID: "501",
		Text: "🗓 Дата: 10.11.2026\n👤 Чаттер: #alfa\n\n📌 Аккаунты:\n• A - $100\n\n💰Общий доход: $100"}
	origRaw, _ := json.Marshal(orig)
	if ans, err := events.Accept(ctx, store, []events.Event{{Source: reports.PostSource,
		SourceID: reports.PostSourceID(peer, 4, 10, orig.Text, ""), Kind: reports.PostKindPost, OccurredAt: orig.Date, Payload: origRaw}}, now); err != nil || len(ans.Settle) > 0 {
		t.Fatalf("orig post: %v %v", err, ans.Settle)
	}
	// Pay Альфа's 10.11 ($25) on 2026-11-12 (3 days before now, within 14 days).
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	jmd := "---\ntitle: Журнал\nupdated: 2026-11-15\n---\n\n# Журнал\n\n## Транзакции\n\n" +
		"| # | Дата | Тип | Категория | Кому/Откуда | Модель | Сумма | Валюта | Метод | Статус | Примечание |\n" +
		"|---|------|-----|-----------|-------------|--------|-------|--------|-------|--------|------------|\n" +
		"| 1 | 2026-11-12 | expense | Чаттер зарплата | Альфа | Alpha | 25.00 | USDT | Internal Transfer (Bybit) | paid | по таблице |\n"
	if _, err := journal.Import(ctx, tx, journal.FromFile(jmd, "file", now), journal.Options{AccrualFrom: "2026-11-01"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := payroll.Rebuild(ctx, store, now); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(dir, "reports.json")
	if err := os.WriteFile(cfg, []byte(`{"topics": [{"peer": "`+peer+`", "topic": 4}], "since_days": 3, "tg_dir": "`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var readSince string
	oldReader := tgTopicReader
	tgTopicReader = func(string) reports.TopicReader {
		return func(_ context.Context, _ string, _ int64, _, since string, _ int) ([]byte, error) {
			readSince = since
			return []byte(`{"messages": [{"id": 10, "date": "2026-11-10T20:30:00Z", "edited": "2026-11-15T08:00:00Z", "sender": "a", "sender_id": 501,
				"text": "🗓 Дата: 10.11.2026\n👤 Чаттер: #alfa\n\n📌 Аккаунты:\n• A - $140\n\n💰Общий доход: $140"}]}`), nil
		}
	}
	defer func() { tgTopicReader = oldReader }()

	// 1. Live HTTP server with a collect-only key serves GET /v1/report-posts/since = 2026-11-10 to --dry-run.
	srv := httptest.NewServer(server.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	tok, _, err := auth.Create(ctx, store.RW, "mac", []string{auth.ScopeCollect}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_URL", srv.URL)
	t.Setenv("LEDGER_KEY", tok)
	spool, state := filepath.Join(dir, "spool"), filepath.Join(dir, "sent.json")
	if err := collectReports(ctx, []string{"--dry-run", "--config", cfg, "--spool", spool, "--state", state}); err != nil {
		t.Fatal(err)
	}
	if readSince != "2026-11-10" {
		t.Fatalf("HTTP --dry-run since = %q, want 2026-11-10", readSince)
	}
	store.Close()

	// 2. --local-db --dry-run computes the exact same lower bound via SQL directly.
	readSince = ""
	if err := collectReports(ctx, []string{"--local-db", "--dry-run", "--config", cfg, "--spool", spool, "--state", state}); err != nil {
		t.Fatal(err)
	}
	if readSince != "2026-11-10" {
		t.Fatalf("--local-db --dry-run since = %q, want 2026-11-10", readSince)
	}

	// 3. --local-db live run reads back 5 days, ingests the edited post and opens payroll_pending_changes!
	readSince = ""
	if err := collectReports(ctx, []string{"--local-db", "--config", cfg, "--spool", spool, "--state", state}); err != nil {
		t.Fatal(err)
	}
	if readSince != "2026-11-10" {
		t.Fatalf("--local-db since = %q, want 2026-11-10", readSince)
	}
	store, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	changes, err := payroll.OpenChanges(ctx, store.RO, "alfa")
	if err != nil || len(changes) != 1 || changes[0].Date != "2026-11-10" || changes[0].PostedMicros != 25_000_000 || changes[0].NewMicros != 35_000_000 {
		t.Fatalf("pending changes after 5-day-old collect: %+v err=%v", changes, err)
	}
}
