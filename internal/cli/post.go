package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "post", Summary: "ручная проводка из JSON (файл или -): проверка, что сходится в ноль", Run: post})
	Command(Cmd{Name: "period lock", Summary: "закрыть месяц снимком: --month --snapshot --locked-at --as-of --source", Run: periodLock})
}

type postInput struct {
	EffectiveAt string `json:"effective_at"`
	Kind        string `json:"kind"`
	Memo        string `json:"memo"`
	Author      string `json:"author"`
	SourceKey   string `json:"source_key"`
	DocumentNum int    `json:"document_num"`
	Lines       []struct {
		Account  string `json:"account"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
		Orig     string `json:"orig_amount"`
		Memo     string `json:"memo"`
	} `json:"lines"`
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func post(ctx context.Context, args []string) error {
	fs := Flags("post")
	dry := fs.Bool("dry-run", false, "проверить и откатить")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен JSON-файл или -")
	}
	raw, err := readInput(pos[0])
	if err != nil {
		return err
	}
	var in postInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("JSON: %w", err)
	}
	e := ledger.Entry{EffectiveAt: in.EffectiveAt, Kind: in.Kind, Memo: in.Memo, Author: in.Author, SourceKey: in.SourceKey}
	if e.Kind == "" {
		e.Kind = "manual"
	}
	if e.Author == "" {
		e.Author = "manual"
	}
	for _, l := range in.Lines {
		v, err := money.Parse(l.Amount)
		if err != nil {
			return fmt.Errorf("строка %s: %w", l.Account, err)
		}
		e.Lines = append(e.Lines, ledger.Line{Account: l.Account, Amount: v, Currency: l.Currency, Orig: l.Orig, Memo: l.Memo})
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
	if in.DocumentNum != 0 {
		d, err := ledger.CurrentDocument(ctx, tx, in.DocumentNum)
		if err != nil {
			return fmt.Errorf("строка журнала #%d: %w", in.DocumentNum, err)
		}
		e.DocumentID = d.ID
	}
	id, err := ledger.Post(ctx, tx, e, time.Now())
	if err != nil {
		return err
	}
	if *dry {
		return printJSON(map[string]any{"ok": true, "dryRun": true, "balanced": true})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "entry": id})
}

func periodLock(ctx context.Context, args []string) error {
	fs := Flags("period lock")
	month := fs.String("month", "", "ГГГГ-ММ")
	snapshot := fs.String("snapshot", "", "JSON-файл снимка «на закрытии»")
	lockedAt := fs.String("locked-at", "", "время закрытия (МСК или RFC 3339)")
	asOf := fs.String("as-of", "", "срез, на который посчитан снимок")
	source := fs.String("source", "", "кто посчитал снимок (month.py close, ledger month close)")
	decision := fs.String("decision", "", "цифра решения, если есть")
	verdict := fs.String("verdict", "", "вердикт предохранителя, если есть")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *month == "" || *snapshot == "" || *lockedAt == "" || *asOf == "" || *source == "" {
		fs.Usage()
		return flag.ErrHelp
	}
	raw, err := readInput(*snapshot)
	if err != nil {
		return err
	}
	la, err := ts.Parse(*lockedAt)
	if err != nil {
		return err
	}
	ao, err := ts.Parse(*asOf)
	if err != nil {
		return err
	}
	p := ledger.Period{Month: *month, LockedAt: ts.Format(la), AsOf: ts.Format(ao), Snapshot: raw, Source: *source, Verdict: *verdict}
	if *decision != "" {
		d, err := money.Parse(*decision)
		if err != nil {
			return err
		}
		p.Decision = &d
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := ledger.Lock(ctx, store.RW, p); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "month": p.Month, "lockedAt": p.LockedAt})
}
