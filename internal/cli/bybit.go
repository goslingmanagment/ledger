package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "bybit fetch", Summary: "прогон сбора Bybit сейчас: [--since ГГГГ-ММ-ДД] [--until …]; с --since водяные знаки не двигаются", Run: bybitFetch})
	Command(Cmd{Name: "bybit balance", Summary: "баланс Bybit сейчас: FUND + UNIFIED, запись в observations", Run: bybitBalance})
	Command(Cmd{Name: "bybit check-key", Summary: "права и срок ключа Bybit", Run: bybitCheckKey})
	Command(Cmd{Name: "bybit status", Summary: "статус последнего прогона сбора и чтения баланса", Run: bybitStatus})
	Command(Cmd{Name: "bybit unlinked", Summary: "движения без строки журнала и без решения personal/ignore: [--since 2026-08-01] [--json]", Run: bybitUnlinked})
	Command(Cmd{Name: "bybit link-notes", Summary: "связать события со строками журнала по ключам в примечаниях", Run: bybitLinkNotes})
	Command(Cmd{Name: "bybit link", Summary: "вручную: <ключ> --doc 387,388 | --decision personal|ignore [--note]", Run: bybitLink})
	Command(Cmd{Name: "rules import", Summary: "правила и заметки из каталога config ledger-sync: <каталог>", Run: rulesImport})
	Command(Cmd{Name: "import bybit-state", Summary: "состояние ledger-sync: история, Funding, карты, отметки брифов: <bybit.sqlite>", Run: importBybitState})
}

func bybitKeys() (bybit.Keys, error) {
	keys, ok := bybit.KeysFromEnv()
	if !ok {
		return keys, fmt.Errorf("нужны LEDGER_BYBIT_API_KEY и LEDGER_BYBIT_API_SECRET")
	}
	return keys, nil
}

func bybitFetch(ctx context.Context, args []string) error {
	fs := Flags("bybit fetch")
	since := fs.String("since", "", "читать с даты или времени (МСК); водяные знаки и статус не меняются")
	until := fs.String("until", "", "читать до даты или времени (МСК); по умолчанию сейчас")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys, err := bybitKeys()
	if err != nil {
		return err
	}
	var opt bybit.RunOptions
	if *since != "" {
		if opt.Since, err = ts.Parse(*since); err != nil {
			return err
		}
	}
	if *until != "" {
		if opt.Until, err = ts.Parse(*until); err != nil {
			return err
		}
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	st, runErr := bybit.NewCollector(store, bybit.NewClient(keys)).Run(ctx, opt)
	if err := printJSON(st); err != nil {
		return err
	}
	return runErr
}

func bybitBalance(ctx context.Context, args []string) error {
	fs := Flags("bybit balance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys, err := bybitKeys()
	if err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	b, err := bybit.CollectBalance(ctx, store, bybit.NewClient(keys), time.Now())
	if err != nil {
		return err
	}
	return printJSON(b)
}

func bybitCheckKey(ctx context.Context, args []string) error {
	keys, err := bybitKeys()
	if err != nil {
		return err
	}
	info, err := bybit.NewClient(keys).CheckKey(ctx)
	if err != nil {
		return err
	}
	if err := printJSON(info); err != nil {
		return err
	}
	if !info.ReadOnly {
		return Exit{Code: 3, Msg: "ОПАСНО: ключ не только на чтение, пересоздать"}
	}
	return nil
}

func bybitStatus(ctx context.Context, args []string) error {
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	st, err := bybit.ReadStatus(ctx, store.RO)
	if err != nil {
		return err
	}
	return printJSON(st)
}

func bybitUnlinked(ctx context.Context, args []string) error {
	fs := Flags("bybit unlinked")
	since := fs.String("since", "2026-08-01", "с какой даты (МСК)")
	asJSON := fs.Bool("json", false, "JSON вместо строк")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := bybit.Unlinked(ctx, store.RO, *since)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(map[string]any{"since": *since, "count": len(rows), "rows": rows})
	}
	for _, r := range rows {
		line := fmt.Sprintf("%s  %-32s %-8s %s %s", r.OccurredAt, r.Key, r.Direction, r.Amount, r.Coin)
		if r.Links != "" {
			line += "  связи: " + r.Links
		}
		if r.OpenProposal != 0 {
			line += fmt.Sprintf("  предложение #%d", r.OpenProposal)
		}
		if len(r.Candidates) > 0 {
			line += fmt.Sprintf("  похоже на строки %v", r.Candidates)
		}
		fmt.Println(line)
	}
	fmt.Printf("без строки журнала и без решения personal/ignore с %s: %d\n", *since, len(rows))
	return nil
}

func bybitLinkNotes(ctx context.Context, args []string) error {
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
	rep, err := bybit.LinkNotesAndClose(ctx, tx, "bybit link-notes", time.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(rep)
}

func bybitLink(ctx context.Context, args []string) error {
	fs := Flags("bybit link")
	docs := fs.String("doc", "", "номера строк журнала через запятую")
	decision := fs.String("decision", "", "personal, ignore; со строками журнала по умолчанию recorded")
	note := fs.String("note", "", "пояснение")
	author := fs.String("author", "manual", "кто связал")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	key := pos[0]
	var nums []int
	if *docs != "" {
		for _, p := range strings.Split(*docs, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n <= 0 {
				return fmt.Errorf("--doc: %q не номер строки", p)
			}
			nums = append(nums, n)
		}
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
	res, err := bybit.LinkManual(ctx, tx, key, nums, *decision, *note, *author, time.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(res)
}

func rulesImport(ctx context.Context, args []string) error {
	fs := Flags("rules import")
	dry := fs.Bool("dry-run", false, "проверить и откатить")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен каталог config ledger-sync")
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
	rep, err := bybit.ImportRules(ctx, tx, pos[0], time.Now())
	if err != nil {
		return err
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "lint:", w)
	}
	if !*dry {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return printJSON(map[string]any{"ok": true, "dryRun": *dry, "rules": rep.Rules, "notes": rep.Notes, "missing": rep.Missing,
		"warnings": len(rep.Warnings)})
}

func importBybitState(ctx context.Context, args []string) error {
	fs := Flags("import bybit-state")
	dry := fs.Bool("dry-run", false, "проверить и откатить")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен путь к bybit.sqlite")
	}
	source, err := bybit.OpenState(ctx, pos[0])
	if err != nil {
		return err
	}
	defer source.Close()
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
	rep, err := bybit.ImportState(ctx, tx, source, time.Now())
	if err == nil {
		_, err = bybit.PostConverts(ctx, tx, time.Now())
	}
	if err != nil {
		return err
	}
	if !*dry {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return printJSON(map[string]any{"ok": true, "dryRun": *dry, "report": rep})
}
