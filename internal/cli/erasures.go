package cli

import (
	"context"
	"time"

	"github.com/goslingmanagment/ledger/internal/erasure"
	"github.com/goslingmanagment/ledger/internal/hub"
)

func init() {
	Command(Cmd{Name: "hub erasures-sync", Run: hubErasuresSync,
		Summary: "применить законченные стирания фанов хаба к копиям ledger сейчас, как джоб hub-erasures (рядом с базой)"})
}

func hubErasuresSync(ctx context.Context, args []string) error {
	if _, err := ParseArgs(Flags("hub erasures-sync"), args); err != nil {
		return err
	}
	cfg, err := hub.ConfigFromEnv()
	if err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	st, err := erasure.Sync(ctx, store, hub.NewClient(cfg, nil), time.Now())
	if perr := printJSON(st); perr != nil {
		return perr
	}
	if err != nil {
		return Exit{Code: 3, Msg: "лента стираний хаба не прочитана: " + err.Error()}
	}
	return nil
}
