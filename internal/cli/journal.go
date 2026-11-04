package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "journal bundle", Summary: "история ledger.md из git: --git <репо> [--path] [--worktree] > bundle.json", Run: journalBundle})
	Command(Cmd{Name: "journal check", Summary: "строгая проверка ledger.md без базы: формат строк и теги", Run: journalCheck})
	Command(Cmd{Name: "import ledger-md", Summary: "импорт журнала: bundle.json или ledger.md [--align счёт@дата=сумма] [--overrides]", Run: importLedgerMD})
	Command(Cmd{Name: "export ledger-md", Summary: "зеркало ledger.md из базы [--out файл]", Run: exportLedgerMD})
	Command(Cmd{Name: "balance", Summary: "остаток счёта: ledger balance <счёт> [--at ГГГГ-ММ-ДД] [--recorded-by время]", Run: balance})
}

func journalBundle(ctx context.Context, args []string) error {
	fs := Flags("journal bundle")
	repo := fs.String("git", "", "репозиторий с журналом")
	path := fs.String("path", "finance/ledger.md", "путь файла в репозитории")
	worktree := fs.Bool("worktree", false, "добавить незакоммиченную рабочую копию последней версией")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("нужен --git")
	}
	b, err := journal.FromGit(ctx, *repo, *path, *worktree)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(b)
}

func journalCheck(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен путь к ledger.md")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	f, errs := journal.Parse(string(raw))
	errs = append(errs, journal.Lint(f.Rows)...)
	for _, e := range errs {
		fmt.Println(e)
	}
	if len(errs) > 0 {
		return Exit{Code: 2, Msg: fmt.Sprintf("%d ошибок", len(errs))}
	}
	fmt.Printf("ledger.md: %d строк, ошибок нет\n", len(f.Rows))
	return nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func importLedgerMD(ctx context.Context, args []string) error {
	fs := Flags("import ledger-md")
	var aligns multi
	fs.Var(&aligns, "align", "счёт@ГГГГ-ММ-ДД[#N]=сумма: остаток по снимку на конец дня или сразу после строки #N этой даты")
	overrides := fs.String("overrides", "", "JSON {\"<номер>\": {\"money\": [{\"account\": …, \"amount\": …}]}}")
	accrual := fs.String("accrual-from", "", "с какой даты учёт по начислению (по умолчанию settings.accrual_from)")
	at := fs.String("at", "", "время записи для простого файла (по умолчанию сейчас)")
	dry := fs.Bool("dry-run", false, "проверить и откатить")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен bundle.json или ledger.md")
	}
	raw, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	var b journal.Bundle
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("bundle: %w", err)
		}
	} else {
		when := time.Now()
		if *at != "" {
			if when, err = ts.Parse(*at); err != nil {
				return err
			}
		}
		b = journal.FromFile(string(raw), "file", when)
	}
	opts := journal.Options{AccrualFrom: *accrual}
	for _, a := range aligns {
		acct, rest, ok1 := strings.Cut(a, "@")
		date, amt, ok2 := strings.Cut(rest, "=")
		v, err := money.Parse(amt)
		if !ok1 || !ok2 || err != nil {
			return fmt.Errorf("--align %q: нужно счёт@ГГГГ-ММ-ДД[#N]=сумма", a)
		}
		upTo := 0
		if d, n, ok := strings.Cut(date, "#"); ok {
			if _, err := fmt.Sscanf(n, "%d", &upTo); err != nil {
				return fmt.Errorf("--align %q: номер строки после #", a)
			}
			date = d
		}
		opts.Align = append(opts.Align, journal.Align{Account: acct, Date: date, UpTo: upTo, Amount: v})
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
	if opts.AccrualFrom == "" {
		_ = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'accrual_from'`).Scan(&opts.AccrualFrom)
	}
	if opts.AccrualFrom == "" {
		return fmt.Errorf("нет settings.accrual_from: ledger import business или --accrual-from")
	}
	if *overrides != "" {
		raw, err := os.ReadFile(*overrides)
		if err != nil {
			return err
		}
		var items map[int]journal.Override
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("--overrides: %w", err)
		}
		if err := journal.PutOverrides(ctx, tx, items, *overrides, time.Now()); err != nil {
			return err
		}
	}
	rep, err := journal.Import(ctx, tx, b, opts)
	if err != nil {
		var pe journal.ProblemsError
		if errors.As(err, &pe) {
			return Exit{Code: 2, Msg: pe.Error()}
		}
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
	return printJSON(map[string]any{"ok": true, "dryRun": *dry, "report": rep})
}

func exportLedgerMD(ctx context.Context, args []string) error {
	fs := Flags("export ledger-md")
	out := fs.String("out", "", "куда записать (по умолчанию stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	text, err := journal.Export(ctx, store.RO)
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.WriteString(text)
		return err
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, *out)
}

func balance(ctx context.Context, args []string) error {
	fs := Flags("balance")
	atDate := fs.String("at", "", "на конец дня ГГГГ-ММ-ДД (по умолчанию всё)")
	recorded := fs.String("recorded-by", "", "только проводки, записанные до этого времени")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен счёт")
	}
	account := pos[0]
	before := ""
	if *atDate != "" {
		t, err := ts.Parse(*atDate)
		if err != nil {
			return err
		}
		before = ts.Date(t.AddDate(0, 0, 1))
	}
	rb := ""
	if *recorded != "" {
		t, err := ts.Parse(*recorded)
		if err != nil {
			return err
		}
		rb = ts.Format(t)
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	v, err := ledger.Balance(ctx, store.RO, account, before, rb)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"account": account, "at": *atDate, "balance": v.Cents(), "micros": int64(v)})
}
