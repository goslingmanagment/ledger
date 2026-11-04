package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "health", Summary: "проверки: ledger health [--now] [--urgent] [--quiet] [--json]", Run: health})
	Command(Cmd{Name: "trail", Summary: "путь доллара по переводу: ledger trail <номер строки перевода>", Run: trail})
	Command(Cmd{Name: "mark-backup", Summary: "отметить копию базы на маке (вызывает pull-backup.sh)", Run: markBackup})
}

func health(ctx context.Context, args []string) error {
	fs := Flags("health")
	now := fs.Bool("now", false, "прогнать проверки сейчас, а не показать последний прогон")
	urgent := fs.Bool("urgent", false, "только новые срочные сбои; строк нет — пусто")
	quiet := fs.Bool("quiet", false, "только сбои")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var rs []checks.Result
	at := ""
	if local() {
		store, err := openDirect(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		if *urgent {
			latest, _, err := checks.Latest(ctx, store.RO)
			if err != nil {
				return err
			}
			if rs, err = checks.Urgent(ctx, store.RW, latest, time.Now(), 6*time.Hour); err != nil {
				return err
			}
		} else if *now {
			t := time.Now()
			rs, at = checks.Run(ctx, checks.Env{Q: store.RO, Now: t}), ts.Format(t)
			if err := checks.Save(ctx, store.RW, rs, t, time.Now()); err != nil {
				return err
			}
		} else if rs, at, err = checks.Latest(ctx, store.RO); err != nil {
			return err
		}
	} else {
		c, err := apiClient()
		if err != nil {
			return err
		}
		var out struct {
			At     string          `json:"at"`
			Checks []checks.Result `json:"checks"`
			Alerts []checks.Result `json:"alerts"`
		}
		switch {
		case *urgent:
			err = c.Do(ctx, "POST", "/v1/health/urgent", nil, &out)
			rs = out.Alerts
		case *now:
			err = c.Do(ctx, "GET", "/v1/health/checks?now=1", nil, &out)
			rs = out.Checks
		default:
			err = c.Do(ctx, "GET", "/v1/health/checks", nil, &out)
			rs = out.Checks
		}
		if err != nil {
			return apiError(err)
		}
		at = out.At
	}
	if *asJSON {
		return printJSON(map[string]any{"at": at, "checks": rs})
	}
	failed := 0
	for _, r := range rs {
		if r.OK {
			if !*quiet && !*urgent {
				fmt.Printf("ok    %s: %s\n", r.Name, r.Summary)
			}
			continue
		}
		failed++
		sign := "СБОЙ"
		if r.Severity == "urgent" {
			sign = "СРОЧНО"
		}
		fmt.Printf("%s %s: %s\n", sign, r.Name, r.Summary)
	}
	if !*quiet && !*urgent {
		fmt.Printf("прогон: %s\n", at)
	}
	if failed > 0 && !*urgent {
		return Exit{Code: 3, Msg: fmt.Sprintf("сбоев: %d", failed)}
	}
	return nil
}

func trail(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен номер строки перевода")
	}
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	var model, date, cut, deposits, bad string
	var amount, held, fee, est float64
	var k *float64
	err = store.RO.QueryRowContext(ctx, `SELECT model, date, cut, amount, held, fee, est, k, deposits, bad FROM coverage_transfers WHERE num = ?`, args[0]).
		Scan(&model, &date, &cut, &amount, &held, &fee, &est, &k, &deposits, &bad)
	if err != nil {
		return fmt.Errorf("перевод #%s: %w", args[0], err)
	}
	fmt.Printf("Перевод #%s, %s, %s: записано %.2f (удержано %.2f, комиссия %.2f), срез %s\n", args[0], model, date, amount, held, fee, cut)
	rows, err := store.RO.QueryContext(ctx, `SELECT q.page_label, q.requested_at, q.amount, q.method, q.avail,
		(SELECT COUNT(*) FROM coverage_parts p WHERE p.request_key = q.key AND p.page_label = q.page_label),
		(SELECT ROUND(SUM(p.part), 2) FROM coverage_parts p WHERE p.request_key = q.key AND p.page_label = q.page_label)
		FROM coverage_requests q WHERE q.transfer_num = ? ORDER BY q.requested_at`, args[0])
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var page, at, method string
		var amt, avail float64
		var n int
		var net *float64
		if err := rows.Scan(&page, &at, &amt, &method, &avail, &n, &net); err != nil {
			return err
		}
		v := 0.0
		if net != nil {
			v = *net
		}
		fmt.Printf("  заявка %s %s %.2f (%s), доступно %.2f, покрыто платежей %d на net %.2f\n", page, at[:16], amt, method, avail, n, v)
	}
	kv := "—"
	if k != nil {
		kv = fmt.Sprintf("%.4f", *k)
	}
	fmt.Printf("  оценка покрытого %.2f, депозиты из пометки %s, k %s", est, deposits, kv)
	if bad != "[]" {
		fmt.Printf(", НЕ ПРИНЯТ: %s", strings.Trim(bad, "[]"))
	}
	fmt.Println()
	return nil
}

func markBackup(ctx context.Context, args []string) error {
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	now := ts.Format(time.Now())
	_, err = store.RW.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES ('backup:mac', ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, now, now)
	return err
}
