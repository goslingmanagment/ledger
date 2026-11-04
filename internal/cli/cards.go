package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/cards"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "collect cards", Run: collectCards,
		Summary: "бот CinCin через tg history → события в API: [--dry-run] [--local-db] [--since] [--config файл] [--spool папка]"})
	Command(Cmd{Name: "cards process", Summary: "разобрать события бота сейчас: проводки загрузок, анкоры, предложения, связи депозитов", Run: cardsProcess})
	Command(Cmd{Name: "cards balances", Summary: "карты и кошелёк в журнале против последних анкоров бота [--json]", Run: cardsBalances})
	Command(Cmd{Name: "cards anchor", Summary: "баланс со скрина приложения: ledger cards anchor <номер карты|wallet> <сумма> [--at время]", Run: cardsAnchor})
	Command(Cmd{Name: "cards link", Summary: "связать депозит с выводом по уликам: ledger cards link <msg_id> <withdrawal:N>", Run: cardsLink})
}

func cardsProcess(ctx context.Context, args []string) error {
	if !local() {
		return Exit{Code: 2, Msg: "только рядом с базой (LEDGER_DB): на сервере это делает джоб"}
	}
	var rep cards.Report
	err := writeLocal(ctx, func(tx *sql.Tx) error {
		var err error
		rep, err = cards.Process(ctx, tx, time.Now())
		return err
	})
	if err != nil {
		return err
	}
	return printJSON(rep)
}

func cardsBalances(ctx context.Context, args []string) error {
	fs := Flags("cards balances")
	asJSON := fs.Bool("json", false, "JSON вместо строк")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var list []cards.Balance
	if local() {
		err := readLocal(ctx, func(store *db.Store) error {
			var err error
			list, err = cards.Balances(ctx, store.RO)
			return err
		})
		if err != nil {
			return err
		}
	} else if err := call(ctx, http.MethodGet, "/v1/cards/balances", nil, &list); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(list)
	}
	for _, b := range list {
		line := fmt.Sprintf("%-14s журнал %10s", b.Account, b.Ledger)
		if b.Anchor != "" {
			line += fmt.Sprintf("  анкор %10s (%s)  разница %s", b.Anchor, b.AnchorAt, b.Difference)
		} else {
			line += "  анкора нет"
		}
		fmt.Println(line)
	}
	return nil
}

func cardsLink(ctx context.Context, args []string) error {
	fs := Flags("cards link")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return Exit{Code: 2, Msg: "нужно: ledger cards link <msg_id> <withdrawal:N>"}
	}
	msg, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return Exit{Code: 2, Msg: fmt.Sprintf("«%s» не номер сообщения", pos[0])}
	}
	var rep cards.Report
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			var err error
			rep, err = cards.LinkDeposit(ctx, tx, msg, pos[1], time.Now())
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, "/v1/cards/link", map[string]any{"msg_id": msg, "withdrawal": pos[1]}, &rep); err != nil {
		return err
	}
	return printJSON(rep)
}

// collectCards reads the bot's chat through `tg history`, read-only, and sends what it means to the API
// through a local spool: a message read twice or sent twice is one event.
func collectCards(ctx context.Context, args []string) error {
	fs := Flags("collect cards")
	dry := fs.Bool("dry-run", false, "напечатать, что ушло бы, ничего не отправляя и не трогая очередь")
	localDB := fs.Bool("local-db", false, "писать прямо в LEDGER_DB, без API")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "ledger")
	configPath := fs.String("config", filepath.Join(dir, "cards.json"), `конфиг: {"peer", "account", "tg_dir"}`)
	spoolDir := fs.String("spool", filepath.Join(dir, "spool-cards"), "очередь событий")
	statePath := fs.String("state", filepath.Join(dir, "cards-state.json"), "докуда прочитано")
	since := fs.String("since", "", "читать с даты; по умолчанию от последнего прочитанного минус сутки")
	limit := fs.Int("limit", cards.DefaultLimit, "сообщений за один прогон")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := cards.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	from := *since
	if from == "" {
		from = cards.SinceFrom(*statePath, cfg, ts.Now())
	}
	got, readErr := cards.Collect(ctx, cfg, cards.TgReader(cfg.TgDir), from, *limit)
	if readErr != nil && *dry {
		return readErr
	}
	list := make([]events.Event, 0, len(got.Events))
	for _, p := range got.Events {
		e, err := p.Event()
		if err != nil {
			return err
		}
		list = append(list, e)
	}
	if *dry {
		for _, e := range list {
			fmt.Printf("%s  %s  %s\n", e.SourceID, e.OccurredAt, e.Kind)
		}
		return printJSON(map[string]any{"since": from, "messages": got.Messages, "events": len(list)})
	}
	// A failed read is reported at the end: what waits in the spool from earlier runs still goes.
	spool := events.Spool{Dir: *spoolDir}
	if err := spool.Put(list); err != nil {
		return fmt.Errorf("очередь: %w", err)
	}
	if readErr == nil {
		if err := cards.SaveState(*statePath, got.Last); err != nil {
			return err
		}
	}
	var send events.Sender
	if *localDB {
		store, err := OpenStore(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		send = func(ctx context.Context, batch []events.Event) (events.Answer, error) {
			return events.Accept(ctx, store, batch, ts.Now())
		}
	} else {
		c, err := apiClient()
		if err != nil {
			return err
		}
		send = func(ctx context.Context, batch []events.Event) (events.Answer, error) {
			var ans events.Answer
			err := c.Do(ctx, http.MethodPost, "/v1/events", map[string]any{"events": batch}, &ans)
			return ans, err
		}
	}
	rep, err := spool.Flush(ctx, send)
	if perr := printJSON(map[string]any{"ok": err == nil && readErr == nil && len(rep.Conflicts) == 0, "since": from, "messages": got.Messages, "read": len(list), "flush": rep}); perr != nil {
		return perr
	}
	if err != nil {
		var bad *events.BadRequest
		if errors.As(err, &bad) {
			return Exit{Code: 2, Msg: bad.Msg}
		}
		e := apiError(err)
		var ex Exit
		if errors.As(e, &ex) {
			return Exit{Code: ex.Code, Msg: fmt.Sprintf("%s; в очереди %s: %d", ex.Msg, *spoolDir, rep.Pending)}
		}
		return e
	}
	if len(rep.Conflicts) > 0 {
		return Exit{Code: 2, Msg: "конфликт событий:\n  " + strings.Join(rep.Conflicts, "\n  ")}
	}
	if len(rep.Settle) > 0 {
		return Exit{Code: 2, Msg: "события приняты, разбор не прошёл:\n  " + strings.Join(rep.Settle, "\n  ")}
	}
	return readErr
}

func cardsAnchor(ctx context.Context, args []string) error {
	fs := Flags("cards anchor")
	at := fs.String("at", "", "когда снят скрин, по умолчанию сейчас")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return Exit{Code: 2, Msg: "нужны карта и сумма: ledger cards anchor 1234 56.78"}
	}
	out := map[string]any{}
	if err := call(ctx, http.MethodPost, "/v1/cards/anchor", map[string]any{"subject": pos[0], "amount": pos[1], "at": *at}, &out); err != nil {
		return err
	}
	return printJSON(out)
}
