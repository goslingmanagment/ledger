package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "report save", Run: reportSave,
		Summary: "сохранить расклад отчёта смены (POST /v1/reports, право reports); ничего не проводит: <файл|->; " +
			"добавка (adds: page, amount_mills, what) называет свою продажу хаба в transaction_ref — ссылкой площадки или id хаба"})
	Command(Cmd{Name: "report list", Run: reportList,
		Summary: "отчёты смен (GET /v1/reports): [--from ГГГГ-ММ-ДД] [--to …] [--person код] [--json]; по умолчанию 14 дней"})
	Command(Cmd{Name: "report show", Run: reportShow,
		Summary: "отчёт смены со страницами и продажами (GET /v1/reports/{id}): <id> [--json]"})
}

func reportSave(ctx context.Context, args []string) error {
	fs := Flags("report save")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен файл отчёта или «-» для stdin: ledger report save <файл|->"}
	}
	var raw []byte
	if pos[0] == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(pos[0])
	}
	if err != nil {
		return err
	}
	if !json.Valid(raw) {
		return Exit{Code: 2, Msg: "не сохранено: в файле не JSON"}
	}
	var out reports.SaveResult
	if err := call(ctx, http.MethodPost, "/v1/reports", json.RawMessage(raw), &out); err != nil {
		return err
	}
	if out.Created {
		fmt.Printf("сохранено: отчёт %d, версия %d\n", out.ID, out.Version)
	} else {
		fmt.Printf("без изменений: отчёт %d, версия %d\n", out.ID, out.Version)
	}
	return nil
}

func reportList(ctx context.Context, args []string) error {
	fs := Flags("report list")
	today := ts.Now()
	from := fs.String("from", ts.Date(today.Add(-13*24*time.Hour)), "первый день, МСК")
	to := fs.String("to", ts.Date(today), "последний день, МСК")
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
	var ans reports.ListAnswer
	if err := call(ctx, http.MethodGet, "/v1/reports?"+q.Encode(), nil, &ans); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "отчёты смен %s — %s\n\n", ans.From, ans.To)
	rows := make([][]string, 0, len(ans.Reports))
	for _, r := range ans.Reports {
		var pages []string
		for _, p := range r.Pages {
			pages = append(pages, p.Page+" "+usd(p.AmountMicros))
		}
		rows = append(rows, []string{strconv.FormatInt(r.ID, 10), r.ReportDate, who(r), fmt.Sprintf("%d/%d", r.Version, r.Versions),
			r.Status, usd(r.TotalMicros), strings.Join(pages, ", ")})
	}
	writeTable(out, []string{"id", "дата", "человек", "версия", "состояние", "итог", "страницы"}, rows,
		[]bool{true, false, false, true, false, true, false})
	return out.Flush()
}

// who is the person of a report, or the Hub name while it is not matched to a card.
func who(r reports.Report) string {
	if r.Person != nil {
		return *r.Person
	}
	return r.HubUsername + " (хаб, без карточки)"
}

func reportShow(ctx context.Context, args []string) error {
	fs := Flags("report show")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен номер отчёта: ledger report show <id>"}
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return Exit{Code: 2, Msg: "номер отчёта — целое число: ledger report show <id>"}
	}
	var r reports.Report
	if err := call(ctx, http.MethodGet, "/v1/reports/"+strconv.FormatInt(id, 10), nil, &r); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}
	out := bufio.NewWriter(os.Stdout)
	printReport(out, r)
	return out.Flush()
}

func printReport(out *bufio.Writer, r reports.Report) {
	fmt.Fprintf(out, "отчёт %d: %s, %s, версия %d из %d, %s, итог %s\n", r.ID, r.ReportDate, who(r), r.Version, r.Versions,
		r.Status, usd(r.TotalMicros))
	fmt.Fprintf(out, "окно %s — %s, по хабу на %s; записано %s\n\n", r.WindowFrom, r.WindowTo, r.CutAt, r.CreatedAt)
	rows := make([][]string, 0, len(r.Pages))
	for _, p := range r.Pages {
		model := p.Model
		if model == "" {
			model = "—"
		}
		rows = append(rows, []string{p.Page, model, usd(p.AmountMicros)})
	}
	writeTable(out, []string{"страница", "модель", "сумма"}, rows, []bool{false, false, true})
	if len(r.Lines) == 0 {
		return
	}
	fmt.Fprintln(out)
	rows = make([][]string, 0, len(r.Lines))
	for _, l := range r.Lines {
		n, at, in := "+", "", "да"
		if l.N != nil {
			n = strconv.FormatInt(*l.N, 10)
		}
		if l.OccurredAt != nil {
			at = *l.OccurredAt
			if t, err := ts.Parse(at); err == nil {
				at = t.In(ts.MSK).Format("02.01 15:04")
			}
		}
		if !l.Counted {
			in = "нет"
		}
		what := strings.TrimSpace(l.Kind + " " + l.FanName)
		var why []string
		if l.Added {
			what = l.What
			if l.TransactionRef != nil {
				why = append(why, "продажа "+*l.TransactionRef)
			}
		} else if l.Decision != "" && l.Decision != "auto" {
			why = append(why, l.Decision)
		}
		why = append(why, l.Flags...)
		why = append(why, l.Notes...)
		rows = append(rows, []string{n, at, l.Page, what, usd(l.NetMicros), in, strings.Join(why, "; ")})
	}
	writeTable(out, []string{"№", "время", "страница", "что", "нетто", "в итоге", "почему"}, rows,
		[]bool{true, false, false, false, true, false, false})
}
