package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/version"
)

func init() {
	Command(Cmd{Name: "serve", Summary: "API и планировщик (контейнер на сервере)", Run: serve})
	Command(Cmd{Name: "migrate", Summary: "применить миграции к LEDGER_DB", Run: migrate})
	Command(Cmd{Name: "backup", Summary: "согласованная копия базы: ledger backup <файл>", Run: backup})
	Command(Cmd{Name: "verify", Summary: "проверить копию: ledger verify <файл>", Run: verify})
	Command(Cmd{Name: "probe", Summary: "проверить /health: ledger probe [--url] [ключ=значение…]", Run: probe})
	Command(Cmd{Name: "version", Summary: "сборка бинарника", Run: func(context.Context, []string) error {
		fmt.Println(version.Get())
		return nil
	}})
}

// OpenStore opens LEDGER_DB and applies migrations.
func OpenStore(ctx context.Context) (*db.Store, error) {
	cfg := config.LoadServer()
	if !local() {
		if _, err := os.Stat(filepath.Dir(cfg.DB)); err != nil {
			return nil, errors.New("эта команда работает рядом с базой: контейнер на сервере или LEDGER_DB; с машины агента — ledger sql и ledger schema")
		}
	}
	s, err := db.Open(cfg.DB)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func serve(ctx context.Context, args []string) error {
	fs := Flags("serve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := config.LoadServer()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	// An agent's hub key is accepted only with ledger's own hub key for the question «who is this key».
	hk := auth.HubKeysFromEnv()
	auth.UseHubKeys(hk)
	log.Info("hub keys", "accepted", hk != nil)
	return server.New(store, log, cfg.Deployment).Run(ctx, cfg.Addr, cfg.Scheduler)
}

func migrate(ctx context.Context, args []string) error {
	cfg := config.LoadServer()
	s, err := db.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer s.Close()
	done, err := s.Migrate(ctx)
	if err != nil {
		return err
	}
	v, _ := db.SchemaVersion(ctx, s.RW)
	return printJSON(map[string]any{"ok": true, "applied": done, "schemaVersion": v})
}

func backup(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен путь копии: ledger backup /data/backups/<имя>.db")
	}
	s, err := db.Open(config.LoadServer().DB)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Backup(ctx, args[0]); err != nil {
		return err
	}
	v, _ := db.SchemaVersion(ctx, s.RO)
	st, _ := os.Stat(args[0])
	return printJSON(map[string]any{"ok": true, "path": args[0], "bytes": st.Size(), "schemaVersion": v})
}

func verify(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен путь: ledger verify <файл>")
	}
	s, err := db.OpenReadOnly(args[0])
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Integrity(ctx); err != nil {
		return err
	}
	v, err := db.SchemaVersion(ctx, s.RO)
	if err != nil {
		return err
	}
	counts := map[string]int64{}
	rows, err := s.RO.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, t := range tables {
		var n int64
		if err := s.RO.QueryRowContext(ctx, `SELECT count(*) FROM "`+t+`"`).Scan(&n); err != nil {
			return err
		}
		counts[t] = n
	}
	return printJSON(map[string]any{"ok": true, "integrity": "ok", "schemaVersion": v, "rows": counts})
}

// probe checks the running service: exit 0 when /health answers ok and every key=value matches.
func probe(ctx context.Context, args []string) error {
	fs := Flags("probe")
	url := fs.String("url", "http://127.0.0.1:8080/health", "адрес /health")
	print := fs.Bool("print", false, "напечатать ответ")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, *url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Exit{Code: 1, Msg: err.Error()}
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Exit{Code: 1, Msg: "ответ не JSON: " + err.Error()}
	}
	if ok, _ := body["ok"].(bool); !ok || resp.StatusCode != 200 {
		return Exit{Code: 1, Msg: fmt.Sprintf("status %d ok=%v", resp.StatusCode, body["ok"])}
	}
	for _, kv := range fs.Args() {
		k, v, _ := strings.Cut(kv, "=")
		if got := fmt.Sprint(body[k]); got != v {
			return Exit{Code: 1, Msg: fmt.Sprintf("%s=%s, ожидалось %s", k, got, v)}
		}
	}
	if *print {
		return printJSON(body)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	return enc.Encode(v)
}
