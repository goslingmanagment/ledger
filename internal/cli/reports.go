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
		Summary: "отчёты смен (GET /v1/reports): [--from ГГГГ-ММ-ДД] [--to …] [--person код] [--source agent|hub|all] [--json]; по умолчанию 14 дней, источник agent"})
	Command(Cmd{Name: "report show", Run: reportShow,
		Summary: "отчёт смены со страницами и продажами (GET /v1/reports/{id}, GET /v1/reports/hub/{id}): <id|hub:id> [--json]"})
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
	return runReportList(ctx, args, os.Stdout)
}

func runReportList(ctx context.Context, args []string, w io.Writer) error {
	fs := Flags("report list")
	today := ts.Now()
	from := fs.String("from", ts.Date(today.Add(-13*24*time.Hour)), "первый день, МСК")
	to := fs.String("to", ts.Date(today), "последний день, МСК")
	person := fs.String("person", "", "только этот человек: код (slug)")
	source := fs.String("source", "", "источник: agent, hub или all (по умолчанию agent)")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	if _, _, err := payroll.PayoutWindow(*from, *to); err != nil {
		return Exit{Code: 2, Msg: err.Error()}
	}
	src := strings.TrimSpace(*source)
	if !reports.ValidSource(src) {
		return Exit{Code: 2, Msg: fmt.Sprintf("source: бывает agent, hub или all, а не %q", src)}
	}
	q := url.Values{"from": {*from}, "to": {*to}}
	if *person != "" {
		q.Set("person", *person)
	}
	if src != "" {
		q.Set("source", src)
	}
	var ans reports.ListAnswer
	if err := call(ctx, http.MethodGet, "/v1/reports?"+q.Encode(), nil, &ans); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(w, ans)
	}
	out := bufio.NewWriter(w)
	printReportList(out, ans)
	return out.Flush()
}

func printReportList(out *bufio.Writer, ans reports.ListAnswer) {
	fmt.Fprintf(out, "отчёты смен %s — %s\n\n", ans.From, ans.To)
	rows := make([][]string, 0, len(ans.Reports))
	for _, r := range ans.Reports {
		var pages []string
		for _, p := range r.Pages {
			pages = append(pages, p.Page+" "+usd(p.AmountMicros))
		}
		detail := strings.Join(pages, ", ")
		if detail == "" && r.Problem != nil {
			detail = "! " + *r.Problem
		}
		src := r.Source
		if src == "" {
			src = reports.SourceAgent
		}
		rows = append(rows, []string{reportIDLabel(r), src, r.ReportDate, who(r), fmt.Sprintf("%d/%d", r.Version, r.Versions),
			r.Status, usd(r.TotalMicros), detail})
	}
	writeTable(out, []string{"id", "источник", "дата", "человек", "версия", "состояние", "итог", "страницы"}, rows,
		[]bool{false, false, false, false, true, false, true, false})
}

func reportIDLabel(r reports.Report) string {
	id := strconv.FormatInt(r.ID, 10)
	if r.Source == reports.SourceHub {
		return "hub:" + id
	}
	return id
}

// who is the person of a report, or the Hub name while it is not matched to a card.
func who(r reports.Report) string {
	if r.Person != nil {
		return *r.Person
	}
	if r.HubUsername != "" {
		return r.HubUsername + " (хаб, без карточки)"
	}
	if r.HubUserID != nil {
		return fmt.Sprintf("hub:%d (хаб, без карточки)", *r.HubUserID)
	}
	return "— (хаб, без карточки)"
}

func reportShow(ctx context.Context, args []string) error {
	return runReportShow(ctx, args, os.Stdout)
}

func runReportShow(ctx context.Context, args []string, w io.Writer) error {
	fs := Flags("report show")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен номер отчёта: ledger report show <id|hub:id>"}
	}
	path, err := reportShowPath(pos[0])
	if err != nil {
		return err
	}
	var r reports.Report
	if err := call(ctx, http.MethodGet, path, nil, &r); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(w, r)
	}
	out := bufio.NewWriter(w)
	printReport(out, r)
	return out.Flush()
}

func reportShowPath(arg string) (string, error) {
	if rest, isHub := strings.CutPrefix(arg, "hub:"); isHub {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 {
			return "", Exit{Code: 2, Msg: "номер отчёта — целое число или hub:<id>: ledger report show <id|hub:id>"}
		}
		return "/v1/reports/hub/" + strconv.FormatInt(id, 10), nil
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 {
		return "", Exit{Code: 2, Msg: "номер отчёта — целое число или hub:<id>: ledger report show <id|hub:id>"}
	}
	return "/v1/reports/" + strconv.FormatInt(id, 10), nil
}

func printReport(out *bufio.Writer, r reports.Report) {
	if r.Source == reports.SourceHub {
		fmt.Fprintf(out, "отчёт hub:%d (хаб): %s, %s, версия %d из %d, %s, итог %s\n", r.ID, r.ReportDate, who(r), r.Version, r.Versions,
			r.Status, usd(r.TotalMicros))
		var meta []string
		if r.ShiftID != nil {
			meta = append(meta, fmt.Sprintf("смена %d", *r.ShiftID))
		}
		if r.Door != "" {
			meta = append(meta, "дверь "+r.Door)
		}
		if r.CreatedAt != "" {
			meta = append(meta, "сдан "+r.CreatedAt)
		}
		if len(meta) > 0 {
			fmt.Fprintln(out, strings.Join(meta, ", "))
		}
		if r.Comment != "" {
			fmt.Fprintf(out, "комментарий: %s\n", r.Comment)
		}
		if r.Problem != nil {
			fmt.Fprintf(out, "проблема: %s\n", *r.Problem)
		}
		if len(r.Pages) > 0 || len(r.Lines) > 0 {
			fmt.Fprintln(out)
		}
	} else {
		fmt.Fprintf(out, "отчёт %d: %s, %s, версия %d из %d, %s, итог %s\n", r.ID, r.ReportDate, who(r), r.Version, r.Versions,
			r.Status, usd(r.TotalMicros))
		fmt.Fprintf(out, "окно %s — %s, по хабу на %s; записано %s\n\n", r.WindowFrom, r.WindowTo, r.CutAt, r.CreatedAt)
	}
	showPages := len(r.Pages) > 0 || r.Source != reports.SourceHub
	if showPages {
		rows := make([][]string, 0, len(r.Pages))
		for _, p := range r.Pages {
			model := p.Model
			if model == "" {
				model = "—"
			}
			rows = append(rows, []string{p.Page, model, usd(p.AmountMicros)})
		}
		writeTable(out, []string{"страница", "модель", "сумма"}, rows, []bool{false, false, true})
	}
	if len(r.Lines) == 0 {
		return
	}
	if showPages {
		fmt.Fprintln(out)
	}
	rows := make([][]string, 0, len(r.Lines))
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
		} else if l.AmountMicros != nil && *l.AmountMicros != l.NetMicros {
			in = fmt.Sprintf("да (%s)", usd(*l.AmountMicros))
		}
		page := l.Page
		if page == "" {
			page = "—"
		}
		what := strings.TrimSpace(l.Kind + " " + l.FanName)
		var why []string
		if l.Added && l.What != "" {
			what = l.What
			if l.TransactionRef != nil {
				why = append(why, "продажа "+*l.TransactionRef)
			}
		} else if r.Source == reports.SourceHub && l.TransactionRef != nil {
			if what == "" {
				what = "продажа " + *l.TransactionRef
			} else {
				what += " #" + *l.TransactionRef
			}
		}
		if l.Decision != "" && l.Decision != "auto" {
			why = append(why, l.Decision)
		}
		for _, fl := range l.Flags {
			if fl == "спорно" && l.ClaimedMicros != nil {
				why = append(why, fmt.Sprintf("спорно (заявлено %s)", usd(*l.ClaimedMicros)))
			} else {
				why = append(why, fl)
			}
		}
		why = append(why, l.Notes...)
		rows = append(rows, []string{n, at, page, what, usd(l.NetMicros), in, strings.Join(why, "; ")})
	}
	writeTable(out, []string{"№", "время", "страница", "что", "нетто", "в итоге", "почему"}, rows,
		[]bool{true, false, false, false, true, false, false})
}
