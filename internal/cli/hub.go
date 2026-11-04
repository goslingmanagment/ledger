package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "import hub-exceptions", Summary: "решения по строкам хаба из hub-exceptions.md: <файл> [--source]", Run: importHubExceptions})
	Command(Cmd{Name: "import business", Summary: "маршруты, ставки маршрутов и параметры учёта из JSON: <файл>", Run: importBusiness})
	Command(Cmd{Name: "hub sync", Summary: "выгрузить хаб сейчас: [--page страница]…", Run: hubSync})
	Command(Cmd{Name: "hub status", Summary: "что хаб доказал по каждой странице и датасету", Run: hubStatus})
	Command(Cmd{Name: "hub net", Summary: "оборот страниц месяца по зеркалу хаба: --month ГГГГ-ММ [--as-of]", Run: hubNet})
}

func importHubExceptions(ctx context.Context, args []string) error {
	fs := Flags("import hub-exceptions")
	source := fs.String("source", "", "источник строк (по умолчанию имя файла); повторный импорт его заменяет")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("нужен путь к hub-exceptions.md")
	}
	ex, err := hub.ReadExceptions(fs.Arg(0))
	if err != nil {
		return err
	}
	if *source == "" {
		*source = filepath.Base(fs.Arg(0))
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
	n, err := hub.ImportExceptions(ctx, tx, ex, *source, time.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "source": *source, "rows": len(ex.Rows), "starts": len(ex.Starts),
		"floors": len(ex.Floors), "written": n})
}

func importBusiness(ctx context.Context, args []string) error {
	fs := Flags("import business")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("нужен JSON-файл")
	}
	raw, err := readInput(fs.Arg(0))
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
	rep, err := hub.ImportBusiness(ctx, tx, raw, time.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "routes": rep.Routes, "routeRates": rep.RouteRates, "settings": rep.Settings})
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func hubSync(ctx context.Context, args []string) error {
	fs := Flags("hub sync")
	var pages listFlag
	fs.Var(&pages, "page", "страница гранта (можно несколько); без флага — весь грант")
	quiet := fs.Bool("quiet", false, "без строк хода в stderr")
	if err := fs.Parse(args); err != nil {
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
	sy := hub.NewSyncer(store, cfg)
	if !*quiet {
		sy.Log = func(msg string) { fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().In(ts.MSK).Format("15:04:05"), msg) }
	}
	rep, err := sy.Run(ctx, pages)
	if err != nil {
		return err
	}
	if err := printJSON(rep); err != nil {
		return err
	}
	if n := rep.Unknown(); n > 0 {
		return Exit{Code: 3, Msg: fmt.Sprintf("хаб не доказал %d выгрузок из %d — «неизвестно», см. reasons", n, len(rep.Results))}
	}
	return nil
}

func hubStatus(ctx context.Context, args []string) error {
	fs := Flags("hub status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	sts, err := hub.Statuses(ctx, store.RO)
	if err != nil {
		return err
	}
	unknown := 0
	for _, s := range sts {
		if s.Status != "ok" {
			unknown++
		}
	}
	if sts == nil {
		sts = []hub.SyncStatus{}
	}
	return printJSON(map[string]any{"ok": unknown == 0 && len(sts) > 0, "unknown": unknown, "datasets": sts})
}

func hubNet(ctx context.Context, args []string) error {
	fs := Flags("hub net")
	month := fs.String("month", "", "ГГГГ-ММ (МСК)")
	asOf := fs.String("as-of", "", "срез (МСК или RFC 3339); по умолчанию — до куда хаб доказал транзакции всех страниц")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *month == "" {
		fs.Usage()
		return flag.ErrHelp
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	var cut time.Time
	if *asOf != "" {
		if cut, err = ts.Parse(*asOf); err != nil {
			return err
		}
	} else if cut, err = hub.DefaultAsOf(ctx, store.RO); err != nil {
		return err
	}
	rep, err := hub.MonthNet(ctx, store.RO, *month, cut)
	if err != nil {
		return err
	}
	if err := printJSON(rep); err != nil {
		return err
	}
	if rep.Unknown > 0 {
		return Exit{Code: 3, Msg: fmt.Sprintf("%d страниц «неизвестно»: хаб не доказал их транзакции на срез", rep.Unknown)}
	}
	return nil
}
