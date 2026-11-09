package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/goslingmanagment/ledger/internal/money"
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

func usd(v int64) string { return money.Micros(v).Cents() }

// printPayouts renders the window's pay per person; with one person, also the days and what is unpaid.
func printPayouts(out *bufio.Writer, ans payroll.PayoutsAnswer, detail bool) {
	fmt.Fprintf(out, "окно %s — %s, начисления по дням с %s\n\n", ans.From, ans.To, ans.AccrualFrom)
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
				line := fmt.Sprintf("  %s %s %s%s", d.Date, d.Model, usd(d.AmountMicros), mark)
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
