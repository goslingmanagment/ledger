package cli

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "collect reports", Run: collectReports,
		Summary: "посты «Отчётности» через tg history → события в API: [--dry-run] [--local-db] [--since] [--config файл] [--spool папка]"})
	Command(Cmd{Name: "report days", Run: reportDays,
		Summary: "база оплаты по отчётам: пост и черновик по страницам (GET /v1/report-days): [--from] [--to] [--person код] [--json]; ничего не начисляет"})
	Command(Cmd{Name: "report posts", Run: reportPosts,
		Summary: "разобранные посты «Отчётности» с пометками (GET /v1/report-posts): [--from] [--to] [--flagged] [--json]"})
	Command(Cmd{Name: "report parse", Run: reportParse,
		Summary: "разобрать посты «Отчётности» сейчас, как джоб: рядом с базой или через API (ключ collect)"})
}

func reportParse(ctx context.Context, args []string) error {
	if _, err := ParseArgs(Flags("report parse"), args); err != nil {
		return err
	}
	var rep reports.ParseReport
	if local() {
		if err := readLocal(ctx, func(store *db.Store) error {
			if err := brief.InTx(ctx, store, func(tx *sql.Tx) error {
				var err error
				rep, err = reports.ParsePosts(ctx, tx, time.Now())
				return err
			}); err != nil {
				return err
			}
			return reports.Changed(ctx, store, time.Now())
		}); err != nil {
			return err
		}
	} else if err := call(ctx, http.MethodPost, "/v1/report-posts/parse", map[string]any{}, &rep); err != nil {
		return err
	}
	return printJSON(rep)
}

func windowFlags(name string) (*flag.FlagSet, *string, *string) {
	fs := Flags(name)
	today := ts.Now()
	from := fs.String("from", ts.Date(today.Add(-13*24*time.Hour)), "первый день, МСК")
	to := fs.String("to", ts.Date(today), "последний день, МСК")
	return fs, from, to
}

func reportDays(ctx context.Context, args []string) error {
	fs, from, to := windowFlags("report days")
	person := fs.String("person", "", "только этот человек: код (slug)")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	if _, _, err := payroll.PayoutWindow(*from, *to); err != nil {
		return Exit{Code: 2, Msg: err.Error()}
	}
	q := url.Values{"from": {*from}, "to": {*to}}
	if *person != "" {
		q.Set("person", *person)
	}
	var ans reports.DaysAnswer
	if err := call(ctx, http.MethodGet, "/v1/report-days?"+q.Encode(), nil, &ans); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "база оплаты по отчётам %s — %s (ничего не начислено)\n\n", ans.From, ans.To)
	opt := func(v *int64) string {
		if v == nil {
			return "—"
		}
		return usd(*v)
	}
	states := map[string]string{reports.StatePostedEqual: "пост = черновик", reports.StatePostedChanged: "пост ≠ черновик",
		reports.StatePostOnly: "только пост", reports.StateDraftOnly: "только черновик"}
	rows := make([][]string, 0, len(ans.Days))
	for _, d := range ans.Days {
		state := states[d.State]
		if d.DiffMicros != nil && *d.DiffMicros != 0 {
			state += " (" + signedUSD(*d.DiffMicros) + ")"
		}
		rows = append(rows, []string{d.ReportDate, d.Person, d.Page, opt(d.PostedMicros), opt(d.DraftMicros), state})
	}
	writeTable(out, []string{"дата", "человек", "страница", "пост", "черновик", "состояние"}, rows,
		[]bool{false, false, false, true, true, false})
	return out.Flush()
}

func signedUSD(v int64) string {
	if v > 0 {
		return "+" + usd(v)
	}
	return usd(v)
}

func reportPosts(ctx context.Context, args []string) error {
	fs, from, to := windowFlags("report posts")
	flagged := fs.Bool("flagged", false, "только посты с пометками")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	if _, _, err := payroll.PayoutWindow(*from, *to); err != nil {
		return Exit{Code: 2, Msg: err.Error()}
	}
	q := url.Values{"from": {*from}, "to": {*to}}
	if *flagged {
		q.Set("flagged", "1")
	}
	var ans reports.PostsAnswer
	if err := call(ctx, http.MethodGet, "/v1/report-posts?"+q.Encode(), nil, &ans); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "посты «Отчётности» %s — %s\n\n", ans.From, ans.To)
	rows := make([][]string, 0, len(ans.Posts))
	for _, p := range ans.Posts {
		date, who := "—", p.Sender+" "+p.Tag+" (не узнан)"
		if p.ReportDate != nil {
			date = *p.ReportDate
		}
		if p.Person != nil {
			who = *p.Person
		}
		var pages []string
		for _, pg := range p.Pages {
			name := pg.Name
			if pg.Page == nil {
				name += " (?)"
			} else if *pg.Page != pg.Name {
				name += "→" + *pg.Page
			}
			if pg.InShift {
				pages = append(pages, name+" "+usd(pg.AmountMicros))
			} else {
				pages = append(pages, name+" не в смене")
			}
		}
		rows = append(rows, []string{strconv.FormatInt(p.ID, 10), date, who, usd(p.TotalMicros), strings.Join(pages, ", "),
			strings.Join(p.FlagTexts, "; ")})
	}
	writeTable(out, []string{"id", "дата", "человек", "итог", "страницы", "пометки"}, rows,
		[]bool{true, false, false, true, false, false})
	return out.Flush()
}

// collectReports reads the «Отчётность» topics through `tg history`, read-only, and sends the posts to the API through
// a local spool: a post read twice is one event, an edited post a new one.
func collectReports(ctx context.Context, args []string) error {
	fs := Flags("collect reports")
	dry := fs.Bool("dry-run", false, "напечатать, что ушло бы, ничего не отправляя и не трогая очередь")
	localDB := fs.Bool("local-db", false, "писать прямо в LEDGER_DB, без API")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "ledger")
	configPath := fs.String("config", filepath.Join(dir, "reports.json"), `конфиг: {"topics": [{"peer", "topic"}], "since_days", "tg_dir"}`)
	spoolDir := fs.String("spool", filepath.Join(dir, "spool-reports"), "очередь событий")
	sentPath := fs.String("state", filepath.Join(dir, "reports-sent.json"), "что сервер уже принял")
	since := fs.String("since", "", "читать с даты ГГГГ-ММ-ДД; по умолчанию since_days дней назад")
	limit := fs.Int("limit", reports.DefaultLimit, "сообщений за одно чтение топика")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := reports.LoadCollectConfig(*configPath)
	if err != nil {
		return err
	}
	from := *since
	if from == "" {
		from = ts.Date(ts.Now().Add(-time.Duration(cfg.SinceDays) * 24 * time.Hour))
	}
	got, readErr := reports.CollectPosts(ctx, cfg, tgTopicReader(cfg.TgDir), from, *limit)
	if readErr != nil && *dry {
		return readErr
	}
	sent := reports.LoadSent(*sentPath)
	list := sent.Fresh(got.Events)
	if *dry {
		for _, e := range list {
			fmt.Printf("%s  %s\n", e.SourceID, e.OccurredAt)
		}
		return printJSON(map[string]any{"since": from, "messages": got.Messages, "posts": len(got.Events), "new": len(list)})
	}
	spool := events.Spool{Dir: *spoolDir}
	if err := spool.Put(list); err != nil {
		return fmt.Errorf("очередь: %w", err)
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
	if err == nil {
		// What is still in the spool (a conflict) goes again from there, not from the read.
		if serr := sent.Save(*sentPath, list, from); serr != nil {
			return serr
		}
	}
	if perr := printJSON(map[string]any{"ok": err == nil && readErr == nil && len(rep.Conflicts) == 0, "since": from,
		"messages": got.Messages, "posts": len(got.Events), "new": len(list), "flush": rep}); perr != nil {
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

// tgTopicReader is the reader `collect reports` uses; tests put a fake tg in its place.
var tgTopicReader = reports.TgTopicReader
