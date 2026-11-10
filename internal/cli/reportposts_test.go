package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goslingmanagment/ledger/internal/reports"
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
