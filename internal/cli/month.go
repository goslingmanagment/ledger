package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/goslingmanagment/ledger/internal/month"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "month show", Summary: "карточка месяца: ledger month show ГГГГ-ММ [--as-of] [--docs-as-of] [--tabs файл] [--json]", Run: monthShow})
}

// loadCardInputs reads the journal as of docsAsOf and tabs from a local JSON file if given.
func loadCardInputs(ctx context.Context, q *sql.DB, docsAsOf, tabsFile string) (month.Config, *month.DBSource, []month.Tab, error) {
	cfg, err := month.LoadConfig(ctx, q)
	if err != nil {
		return cfg, nil, nil, err
	}
	var tabs []month.Tab
	if tabsFile != "" {
		raw, err := os.ReadFile(tabsFile)
		if err != nil {
			return cfg, nil, nil, err
		}
		if err := json.Unmarshal(raw, &tabs); err != nil {
			return cfg, nil, nil, fmt.Errorf("--tabs: %w", err)
		}
	}
	return cfg, &month.DBSource{Ctx: ctx, Q: q}, tabs, nil
}

func monthShow(ctx context.Context, args []string) error {
	fs := Flags("month show")
	asOf := fs.String("as-of", "", "срез (по умолчанию сейчас)")
	docsAsOf := fs.String("docs-as-of", "", "журнал на это время (по умолчанию действующие строки)")
	tabsFile := fs.String("tabs", "", "вкладки выплат из JSON-файла вместо таблицы базы")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен месяц ГГГГ-ММ")
	}
	when := time.Now()
	if *asOf != "" {
		if when, err = ts.Parse(*asOf); err != nil {
			return err
		}
	}
	cut := ""
	if *docsAsOf != "" {
		t, err := ts.Parse(*docsAsOf)
		if err != nil {
			return err
		}
		cut = ts.Format(t)
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	cfg, src, tabs, err := loadCardInputs(ctx, store.RO, cut, *tabsFile)
	if err != nil {
		return err
	}
	rows, err := month.JournalRows(ctx, store.RO, cut)
	if err != nil {
		return err
	}
	cv, err := month.BuildCover(cfg, src, rows, when)
	if err != nil {
		return err
	}
	card, err := month.BuildCard(cv, pos[0], tabs)
	if err != nil {
		return err
	}
	out := card.Snapshot()
	out["checks"] = card.Checks("")
	out["notes"] = card.Notes
	return printJSON(out)
}
