package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "mirror sheets", Run: mirrorSheets,
		Summary: "таблицы выплат — зеркало отчётов: цифры страниц в ячейки через gog; по умолчанию только план, --apply пишет " +
			"[--model] [--person] [--from ГГГГ-ММ-ДД, только план] [--yes-many] [--local-db] [--config] [--json]"})
}

// mirrorOpts are the flags of a run.
type mirrorOpts struct {
	apply, many, json bool
	from              string
	filter            payroll.MirrorFilter
	journal           string
}

func mirrorSheets(ctx context.Context, args []string) error {
	fs := Flags("mirror sheets")
	var o mirrorOpts
	fs.BoolVar(&o.apply, "apply", false, "записать (без флага — только план)")
	fs.BoolVar(&o.many, "yes-many", false, fmt.Sprintf("разрешить больше %d ячеек за прогон", payroll.MirrorMaxCells))
	fs.BoolVar(&o.json, "json", false, "план и итог JSON")
	fs.StringVar(&o.from, "from", "", "с какого дня план (только без --apply): предпросмотр до переключения")
	fs.StringVar(&o.filter.Model, "model", "", "только таблица модели")
	fs.StringVar(&o.filter.Person, "person", "", "только вкладки человека (код)")
	localDB := fs.Bool("local-db", false, "база из LEDGER_DB, без API")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := fs.String("config", filepath.Join(home, ".config", "ledger", "sheets.json"), "конфиг: {\"account\", \"tables\"}")
	fs.StringVar(&o.journal, "journal", filepath.Join(home, ".config", "ledger", "sheets-mirror.jsonl"), "журнал записанных ячеек")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.from != "" {
		if t, err := time.Parse(ts.DateLayout, o.from); err != nil || t.Format(ts.DateLayout) != o.from {
			return Exit{Code: 2, Msg: fmt.Sprintf("--from: нужна дата ГГГГ-ММ-ДД, а не %q", o.from)}
		}
	}
	if o.apply && o.from != "" {
		return Exit{Code: 2, Msg: "--from — только для плана: --apply пишет с payroll_reports_from"}
	}
	cfg, err := payroll.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	var base payroll.MirrorBase
	if *localDB {
		store, err := OpenStore(ctx)
		if err != nil {
			return err
		}
		base, err = payroll.LoadMirrorBase(ctx, store.RO, o.from)
		store.Close()
		if err != nil {
			return err
		}
	} else {
		q := url.Values{}
		if o.from != "" {
			q.Set("from", o.from)
		}
		if err := call(ctx, http.MethodGet, "/v1/sheets-mirror?"+q.Encode(), nil, &base); err != nil {
			return err
		}
	}
	return runMirror(ctx, o, cfg, base, payroll.GogSheets{Account: cfg.Account}, os.Stdout, ts.Now())
}

// runMirror is the run after the flags: the switch, the plan, its print, and with --apply the write.
func runMirror(ctx context.Context, o mirrorOpts, cfg payroll.Config, base payroll.MirrorBase, sheets payroll.SheetIO, w io.Writer, now time.Time) error {
	if base.ReportsFrom == "" && (o.apply || o.from == "") {
		// The job runs every hour: with the switch unset it has nothing to do, and that is not a failure.
		fmt.Fprintln(w, "зеркало выключено: payroll_reports_from не задан — в таблицы ничего не пишу (план: --from ГГГГ-ММ-ДД)")
		return nil
	}
	plan, err := payroll.PlanMirror(ctx, cfg, base, sheets, o.filter, now)
	if err != nil {
		return err
	}
	var applyErr error
	if o.apply {
		applyErr = payroll.ApplyMirror(ctx, &plan, sheets, o.journal, o.many, now)
	}
	out := bufio.NewWriter(w)
	if o.json {
		if err := printJSONTo(out, map[string]any{"ok": applyErr == nil && tabErrors(plan) == 0, "applied": o.apply && applyErr == nil,
			"cells": plan.Cells(), "plan": plan, "error": errText(applyErr)}); err != nil {
			return err
		}
	} else {
		printMirrorPlan(out, plan, o.apply)
	}
	if err := out.Flush(); err != nil {
		return err
	}
	switch {
	case errors.Is(applyErr, payroll.ErrMirrorTooMany):
		return Exit{Code: 2, Msg: applyErr.Error()}
	case applyErr != nil:
		return Exit{Code: 3, Msg: "запись в таблицы:\n  " + applyErr.Error()}
	case o.apply && tabErrors(plan) > 0:
		return Exit{Code: 3, Msg: fmt.Sprintf("вкладок не тронуто из-за ошибок: %d (см. выше)", tabErrors(plan))}
	}
	return nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func tabErrors(p payroll.MirrorPlan) int {
	n := 0
	for _, t := range p.Tabs {
		if t.Error != "" {
			n++
		}
	}
	return n
}

func cellText(c payroll.Cell) string {
	switch x := c.(type) {
	case nil:
		return "пусто"
	case string:
		if x == "" {
			return "пусто"
		}
		return fmt.Sprintf("%q", x)
	}
	return fmt.Sprint(c)
}

func printMirrorPlan(w *bufio.Writer, p payroll.MirrorPlan, applied bool) {
	if p.ReportsFrom == "" {
		fmt.Fprintf(w, "Предпросмотр с %s: payroll_reports_from не задан, --apply откажет.\n", p.From)
	} else {
		fmt.Fprintf(w, "Зеркало отчётов с %s (payroll_reports_from %s).\n", p.From, p.ReportsFrom)
	}
	verb := "Запишу"
	if applied {
		verb = "Записываю"
	}
	cells, correct, skips := 0, 0, 0
	tabs := 0
	for _, t := range p.Tabs {
		correct += t.Correct
		skips += len(t.Skips)
		if len(t.Writes) == 0 {
			continue
		}
		tabs++
		if cells == 0 {
			fmt.Fprintf(w, "\n%s:\n  %-24s %-6s %-10s %-16s %s\n", verb, "вкладка", "ячейка", "дата", "страница", "было → станет")
		}
		for _, c := range t.Writes {
			cells++
			fmt.Fprintf(w, "  %-24s %-6s %-10s %-16s %s → %s\n", t.Model+"/"+t.Name, c.Cell, c.Date, c.Page, cellText(c.Was),
				money.Micros(*c.Want).String())
		}
	}
	if cells == 0 {
		fmt.Fprintln(w, "\nПисать нечего: ячейки уже совпадают с отчётами.")
	}
	var notes []string
	for _, t := range p.Left {
		notes = append(notes, fmt.Sprintf("%s/%s: %s", t.Model, t.Name, t.Error))
	}
	for _, t := range p.Tabs {
		if t.Error != "" {
			notes = append(notes, fmt.Sprintf("%s/%s: %s", t.Model, t.Name, t.Error))
		}
		for _, c := range t.Skips {
			where := c.Cell
			if where == "" {
				where = "—"
			}
			line := fmt.Sprintf("%s/%s %s %s %s: %s", t.Model, t.Name, where, c.Date, c.Page, c.Reason)
			if c.Want != nil {
				line += fmt.Sprintf(" (в ячейке %s, по отчёту %s)", cellText(c.Was), money.Micros(*c.Want).String())
			} else {
				line += fmt.Sprintf(" (в ячейке %s)", cellText(c.Was))
			}
			notes = append(notes, line)
		}
	}
	if len(notes) > 0 {
		fmt.Fprintln(w, "\nНе трогаю:")
		for _, n := range notes {
			fmt.Fprintln(w, "  "+n)
		}
	}
	if applied {
		for _, t := range p.Tabs {
			if len(t.Written) > 0 || t.Failed != "" {
				fmt.Fprintf(w, "\n%s/%s: записано %d", t.Model, t.Name, len(t.Written))
				if len(t.Unwritten) > 0 {
					fmt.Fprintf(w, ", не записано %d (%v)", len(t.Unwritten), t.Unwritten)
				}
				if t.Failed != "" {
					fmt.Fprintf(w, " — %s", t.Failed)
				}
				fmt.Fprintln(w)
			}
		}
	}
	fmt.Fprintf(w, "\nИтого: к записи %d ячеек во вкладках %d; уже верно %d; не трогаю %d; вкладок не тронуто %d.\n",
		cells, tabs, correct, skips, tabErrors(p)+len(p.Left))
	if !applied && cells > 0 && p.ReportsFrom != "" {
		fmt.Fprintln(w, "Записать: ledger mirror sheets --apply")
	}
}

func printJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	return enc.Encode(v)
}
