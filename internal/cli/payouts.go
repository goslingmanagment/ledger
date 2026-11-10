package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "payouts", Run: payouts,
		Summary: "выплаты чаттерам, как их читает хаб (GET /v1/payouts): [--from ГГГГ-ММ-ДД] [--to …] [--person код] [--json]"})
}

func payouts(ctx context.Context, args []string) error {
	fs := Flags("payouts")
	today := ts.Now()
	from := fs.String("from", ts.Date(ts.WeekStart(today)), "первый день окна, МСК")
	to := fs.String("to", ts.Date(today), "последний день окна, МСК")
	person := fs.String("person", "", "только этот человек: код (slug)")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, _, err := payroll.PayoutWindow(*from, *to); err != nil {
		return err
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	var ans payroll.PayoutsAnswer
	if err := c.Do(ctx, http.MethodGet, "/v1/payouts?from="+url.QueryEscape(*from)+"&to="+url.QueryEscape(*to), nil, &ans); err != nil {
		return apiError(err)
	}
	if *person != "" {
		var only []payroll.PayoutPerson
		for _, p := range ans.People {
			if p.Slug == *person {
				only = append(only, p)
			}
		}
		if len(only) == 0 {
			return Exit{Code: 2, Msg: fmt.Sprintf("человека %q в выплатах нет: нужна вкладка выплат", *person)}
		}
		ans.People = only
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	printPayouts(out, ans, *person != "")
	return out.Flush()
}

// printPayouts renders the window's pay per person; with one person, also the days and what is unpaid.
func printPayouts(out *bufio.Writer, ans payroll.PayoutsAnswer, detail bool) {
	fmt.Fprintf(out, "окно %s — %s, начисления по дням с %s\n", ans.From, ans.To, ans.AccrualFrom)
	switch {
	case ans.ReportsFrom != "":
		fmt.Fprintf(out, "с %s дни начисляют отчёты смен, таблицы — зеркало\n", ans.ReportsFrom)
	case ans.FactsFrom != "":
		fmt.Fprintf(out, "с %s дни начисляют факты хаба, таблицы — зеркало\n", ans.FactsFrom)
	}
	fmt.Fprintln(out)
	rows := make([][]string, 0, len(ans.People))
	for _, p := range ans.People {
		toPay, oldest := "—", "—"
		if p.ToPayMicros != nil {
			toPay = usd(*p.ToPayMicros)
		}
		if p.OldestUnpaidDate != nil {
			oldest = *p.OldestUnpaidDate
		}
		rows = append(rows, []string{p.Name + " (" + p.Slug + ")", fmt.Sprint(len(p.Days)), usd(p.AccruedMicros), usd(p.PaidMicros),
			usd(p.BalanceMicros), toPay, oldest})
	}
	writeTable(out, []string{"человек", "дней", "начислено", "выплачено", "долг сейчас", "к выплате", "старый день"}, rows,
		[]bool{false, true, true, true, true, true, false})
	if detail {
		for _, p := range ans.People {
			fmt.Fprintf(out, "\n%s, дни окна:\n", p.Name)
			for _, d := range p.Days {
				var pages []string
				for _, pg := range d.Pages {
					pages = append(pages, pg.Page+" "+usd(pg.RevenueMicros))
				}
				mark := ""
				if !d.Worked {
					mark = " (ячейки пустые)"
				}
				line := fmt.Sprintf("  %s %s %s%s%s", d.Date, d.Model, usd(d.AmountMicros), mark, sourceMark(d.Source))
				if len(pages) > 0 {
					line += ", выручка: " + strings.Join(pages, ", ")
				}
				fmt.Fprintln(out, line)
			}
			if len(p.UnpaidDays) > 0 {
				var parts []string
				for _, u := range p.UnpaidDays {
					what := u.Date
					if u.Kind == "opening" {
						what = "старт"
					}
					parts = append(parts, what+" "+usd(u.UnpaidMicros))
				}
				fmt.Fprintf(out, "  не оплачено: %s\n", strings.Join(parts, ", "))
			}
		}
	}
	for _, p := range ans.People {
		for _, c := range p.PendingChanges {
			fmt.Fprintf(out, "\n! %s: изменение %d за выплаченный день %s (%s) ждёт решения: проведено %s, по отчёту %s — %s\n", p.Name, c.ID, c.Date,
				c.Model, usd(c.PostedMicros), usd(c.NewMicros), c.ReasonText)
		}
	}
	dash := func(s *string) string {
		if s == nil {
			return "никогда"
		}
		return *s
	}
	fmt.Fprintf(out, "\nтаблицы прочитаны: %s, долги сведены: %s\n", dash(ans.Checks.LastSheetsCollectAt), dash(ans.Checks.LastRebuildAt))
	if ans.Checks.Stale {
		fmt.Fprintln(out, "! данные несвежие: сборщик таблиц или свод долгов давно не запускались")
	}
}

func init() {
	Command(Cmd{Name: "payout prepare", Run: payoutPrepare,
		Summary: "выплата человеку, шаг 1 (GET /v1/payouts/prepare): сумма, дни, блоки, флаги и текст сообщения; ничего не пишет: <код> [--json]"})
}

func payoutPrepare(ctx context.Context, args []string) error {
	fs := Flags("payout prepare")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен один код человека (slug): ledger payout prepare <код>"}
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	var b payroll.PayoutBrief
	if err := c.Do(ctx, http.MethodGet, "/v1/payouts/prepare?person="+url.QueryEscape(pos[0]), nil, &b); err != nil {
		return apiError(err)
	}
	if *asJSON {
		return printJSON(b)
	}
	out := bufio.NewWriter(os.Stdout)
	printBrief(out, b)
	return out.Flush()
}

// sourceMark says where a day came from when it is not the sheet.
func sourceMark(source string) string {
	switch source {
	case payroll.SourceFacts:
		return " (факты)"
	case payroll.SourceReports:
		return " (отчёт)"
	}
	return ""
}

// printBrief renders the brief for the owner: the amount and the hint, the lines, the blocks to mark, the
// flags, then the message as it would go.
func printBrief(out *bufio.Writer, b payroll.PayoutBrief) {
	toPay := "—"
	if b.ToPayMicros != nil {
		toPay = usd(*b.ToPayMicros)
	}
	if b.Nothing != "" {
		fmt.Fprintf(out, "%s (%s): %s; «К выплате» вкладок %s\n", b.Name, b.Slug, b.Nothing, toPay)
	} else {
		period := "—"
		if b.Period != nil {
			period = b.Period.From + " — " + b.Period.To
		}
		fmt.Fprintf(out, "%s (%s): к выплате %s, «К выплате» вкладок %s, период %s\n", b.Name, b.Slug, usd(b.AmountMicros), toPay, period)
		if b.Suggestion != nil {
			fmt.Fprintf(out, "ориентир перевода %s (%s)\n", usd(b.Suggestion.TransferMicros), b.Suggestion.Note)
		}
		fmt.Fprintln(out)
		rows := make([][]string, 0, len(b.Lines))
		for _, l := range b.Lines {
			rows = append(rows, []string{l.Model + sourceMark(l.Source), l.Text, usd(l.ShownMicros)})
		}
		writeTable(out, []string{"вкладка", "строка", "сумма"}, rows, []bool{false, false, true})
		if len(b.Blocks) > 0 {
			var parts []string
			for _, bl := range b.Blocks {
				s := fmt.Sprintf("%s неделя %s (%s — %s)", bl.Model, bl.Week, bl.From, bl.To)
				if !bl.WeekOver {
					s += ": неделя не кончилась, «оплатил по " + bl.To + " включительно»"
				}
				parts = append(parts, s)
			}
			fmt.Fprintf(out, "\nзакрывает блоки: %s\n", strings.Join(parts, "; "))
		}
	}
	if len(b.Flags) > 0 {
		fmt.Fprintf(out, "\nфлаги (блокеров %d):\n", b.Blockers)
		for _, f := range b.Flags {
			mark := "-"
			if f.Blocker {
				mark = "!"
			}
			fmt.Fprintf(out, "  %s %s\n", mark, f.Text)
		}
	}
	if b.Message != "" {
		fmt.Fprintf(out, "\nсообщение (%s — сколько ушло на самом деле, вписать после перевода):\n\n%s\n", b.Placeholder, b.Message)
	}
}
