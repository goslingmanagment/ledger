package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "collect sheets", Run: collectSheets,
		Summary: "таблицы выплат через gog → события в API: [--dry-run] [--local-db] [--config файл] [--spool папка]"})
	Command(Cmd{Name: "import people", Summary: "карточки людей из people/*.md в базу: <папка>", Run: importPeople})
	Command(Cmd{Name: "payroll open", Summary: "заморозить стартовые долги: сначала таблица [--dry-run] [--force]", Run: payrollOpen})
	Command(Cmd{Name: "payroll rebuild", Summary: "свести начисления, клиринг, дни и «не занесено» сейчас", Run: payrollRebuild})
	Command(Cmd{Name: "payables", Summary: "кому сколько должны и за какие дни: [--person имя] [--json]", Run: payables})
}

func collectSheets(ctx context.Context, args []string) error {
	fs := Flags("collect sheets")
	dry := fs.Bool("dry-run", false, "напечатать, что ушло бы, ничего не отправляя и не трогая очередь")
	local := fs.Bool("local-db", false, "писать прямо в LEDGER_DB, без API")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := fs.String("config", filepath.Join(home, ".config", "ledger", "sheets.json"), "конфиг: {\"account\", \"tables\"}")
	spoolDir := fs.String("spool", filepath.Join(home, ".config", "ledger", "spool"), "очередь событий")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := payroll.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	now := ts.Now()
	got, err := payroll.Collect(ctx, cfg, payroll.Gog(cfg.Account), now)
	if err != nil {
		return err
	}
	if *dry {
		out := bufio.NewWriter(os.Stdout)
		for _, e := range got.Events {
			fmt.Fprintf(out, "%s  %s\n", e.SourceID, payroll.Describe(e))
		}
		if err := out.Flush(); err != nil {
			return err
		}
		if s := got.Summary(); s != "" {
			return Exit{Code: 3, Msg: s}
		}
		return nil
	}
	spool := payroll.Spool{Dir: *spoolDir}
	if err := spool.Put(got.Events); err != nil {
		return fmt.Errorf("очередь: %w", err)
	}
	var send payroll.Sender
	if *local {
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
	if perr := printJSON(map[string]any{"ok": err == nil && len(got.Errors) == 0 && len(rep.Conflicts) == 0, "read": len(got.Events),
		"errors": len(got.Errors), "flush": rep}); perr != nil {
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
		return Exit{Code: 2, Msg: "события приняты, пересчёт не прошёл:\n  " + strings.Join(rep.Settle, "\n  ")}
	}
	if s := got.Summary(); s != "" {
		return Exit{Code: 3, Msg: s}
	}
	return nil
}

func importPeople(ctx context.Context, args []string) error {
	fs := Flags("import people")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("нужна папка с карточками: ledger import people <папка people>")
	}
	people, skipped, err := payroll.ReadPeople(fs.Arg(0))
	if err != nil {
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
	if err := payroll.PutPeople(ctx, tx, people, "people/*.md", time.Now()); err != nil {
		return err
	}
	var tabs int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payroll_tabs`).Scan(&tabs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if tabs > 0 { // who the tabs belong to may have changed
		if _, err := payroll.Rebuild(ctx, store, ts.Now()); err != nil {
			return err
		}
	}
	if skipped == nil {
		skipped = []string{}
	}
	return printJSON(map[string]any{"ok": true, "people": len(people), "skipped": skipped})
}

func payrollRebuild(ctx context.Context, args []string) error {
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	rep, err := payroll.Rebuild(ctx, store, ts.Now())
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "report": rep})
}

func amount(m money.Micros) string { return m.Cents() }

func payrollOpen(ctx context.Context, args []string) error {
	fs := Flags("payroll open")
	dry := fs.Bool("dry-run", false, "только таблица, ничего не записывать")
	force := fs.Bool("force", false, "заморозить, даже если есть несопоставленные строки или вкладки")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := payroll.Rebuild(ctx, store, ts.Now()); err != nil {
		return err
	}
	plan, from, err := payroll.PlanOpenings(ctx, store.RO)
	if err != nil {
		return err
	}
	check, err := payroll.Check(ctx, store.RO)
	if err != nil {
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "Стартовые долги на день перед %s: «К выплате» вкладок − (начислено − выплачено по таблице)\n\n", from)
	rows := make([][]string, 0, len(plan))
	right := []bool{false, true, true, true, true, false, false}
	var total money.Micros
	for _, o := range plan {
		state := ""
		switch {
		case o.Frozen:
			state = "заморожен"
		case o.Skip != "":
			state = "пропущен"
		}
		note := strings.Join(o.Flags, "; ")
		if o.Skip != "" {
			note = o.Skip
		}
		rows = append(rows, []string{o.Name + " (" + o.Person + ")", fmt.Sprintf("%.2f", o.KV), amount(o.Accrued), amount(o.Paid), amount(o.Amount), state, note})
		if o.Skip == "" {
			total += o.Amount
		}
	}
	writeTable(out, []string{"человек", "к выплате", "начислено", "выплачено", "старт", "статус", "пометки"}, rows, right)
	fmt.Fprintf(out, "\nИтого старт: %s\n", amount(total))
	problems := len(check.Rows) + len(check.Tabs)
	printProblems(out, check)
	if err := out.Flush(); err != nil {
		return err
	}
	if *dry {
		return nil
	}
	if problems > 0 && !*force {
		return Exit{Code: 3, Msg: fmt.Sprintf("есть несопоставленное (%d): заморозка считала бы неполные данные; разберите или --force", problems)}
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n, err := payroll.PostOpenings(ctx, tx, plan, from, ts.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := payroll.Rebuild(ctx, store, ts.Now()); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "frozen": n})
}

func printProblems(w *bufio.Writer, c payroll.CheckReport) {
	if len(c.Rows) > 0 {
		fmt.Fprintf(w, "\nСтроки журнала без человека (деньги ждут на suspense:accrual):\n")
		for _, r := range c.Rows {
			fmt.Fprintf(w, "  #%d %s %s (%s) %s: %s\n", r.Num, r.Date, r.Name, r.Model, r.Amount, r.Reason)
		}
	}
	if len(c.Tabs) > 0 {
		fmt.Fprintf(w, "\nВкладки и таблицы:\n")
		for _, t := range c.Tabs {
			where := t.Model
			if t.Name != "" {
				where += "/" + t.Name
			}
			fmt.Fprintf(w, "  %s: %s\n", where, t.Reason)
		}
	}
}

func payables(ctx context.Context, args []string) error {
	fs := Flags("payables")
	person := fs.String("person", "", "человек: код или имя")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	path := "/v1/payables"
	if *person != "" {
		path += "?person=" + url.QueryEscape(*person)
	}
	var ans payroll.Answer
	if err := c.Do(ctx, http.MethodGet, path, nil, &ans); err != nil {
		return apiError(err)
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	printPayables(out, ans)
	return out.Flush()
}

// quietPayable is a row with nothing to say: no debt, nothing to pay, no doubts. --json still has it.
func quietPayable(p payroll.Payable) bool {
	return p.BalanceMicros == 0 && (p.ToPay == nil || *p.ToPay == 0) && p.UnenteredDays == 0 && p.UnenteredUnknown == 0 && p.TabsStatus != "error"
}

// printPayables renders the table of debts, the unpaid days and what the debts could not take in.
func printPayables(out *bufio.Writer, ans payroll.Answer) {
	dash := func(p *float64) string {
		if p == nil {
			return "—"
		}
		return fmt.Sprintf("%.2f", *p)
	}
	rows := make([][]string, 0, len(ans.Payables))
	for _, p := range ans.Payables {
		if quietPayable(p) && len(ans.Payables) > 1 {
			continue
		}
		blank := "—"
		var parts []string
		if p.UnenteredDays > 0 {
			parts = append(parts, fmt.Sprintf("%d дн. %.2f (доля %.2f)", p.UnenteredDays, p.UnenteredNet, p.UnenteredShare))
		}
		if p.UnenteredUnknown > 0 {
			parts = append(parts, fmt.Sprintf("неизвестно: %d", p.UnenteredUnknown))
		}
		if len(parts) > 0 {
			blank = strings.Join(parts, ", ")
		}
		oldest, tabs := p.OldestUnpaid, p.TabsStatus
		if oldest == "" {
			oldest = "—"
		}
		if tabs == "" {
			tabs = "—"
		}
		fetched := p.FetchedAt
		if fetched == "" {
			fetched = "—"
		}
		rows = append(rows, []string{p.Name + " (" + p.Person + ")", fmt.Sprintf("%.2f", p.Balance), dash(p.ToPay), dash(p.Difference), oldest, blank, tabs, fetched})
	}
	writeTable(out, []string{"человек", "долг", "к выплате", "разница", "старый день", "не занесено", "вкладки", "прочитаны"}, rows,
		[]bool{false, true, true, true, false, false, false, false})
	for _, p := range ans.Payables {
		if p.UnpaidDays != "" {
			fmt.Fprintf(out, "\n%s: %s\n", p.Name, p.UnpaidDays)
		}
	}
	for _, c := range ans.Check.People {
		if !c.OK {
			fmt.Fprintf(out, "\n! %s: %s\n", c.Name, c.Reason)
		}
	}
	printProblems(out, ans.Check)
}
