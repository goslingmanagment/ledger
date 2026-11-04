package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/month"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "transit refresh", Summary: "пересчитать деньги в пути и проводки по ним [--as-of] [--dry-run]", Run: transitRefresh})
}

func transitRefresh(ctx context.Context, args []string) error {
	fs := Flags("transit refresh")
	asOfFlag := fs.String("as-of", "", "срез (по умолчанию последний доказанный хабом)")
	dry := fs.Bool("dry-run", false, "посчитать и откатить")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var when time.Time
	if *asOfFlag != "" {
		if when, err = ts.Parse(*asOfFlag); err != nil {
			return err
		}
	} else {
		cfg, err := month.LoadConfig(ctx, tx)
		if err != nil {
			return err
		}
		if when, err = month.LiveAsOf(ctx, tx, cfg); err != nil {
			if hub.IsRefusal(err) {
				return Exit{Code: 2, Msg: fmt.Sprintf("неизвестно: %v", err)}
			}
			return err
		}
	}
	rep, err := month.Refresh(ctx, tx, when, time.Now())
	if err != nil {
		return err
	}
	var unbalanced int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM v_unbalanced`).Scan(&unbalanced); err != nil || unbalanced != 0 {
		return fmt.Errorf("несходящихся проводок: %d %v", unbalanced, err)
	}
	if !*dry {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	rep.Notes = nil
	return printJSON(map[string]any{"ok": rep.Status == "ok", "dryRun": *dry, "report": rep})
}
