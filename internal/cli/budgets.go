package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/goslingmanagment/ledger/internal/budgets"
)

func init() {
	Command(Cmd{Name: "budgets", Run: budgetsShow,
		Summary: "бюджеты по месяцам, как их читает консоль хаба (GET /v1/budgets): [--from ГГГГ-ММ] [--to ГГГГ-ММ] [--json]"})
	Command(Cmd{Name: "budget plan", Run: budgetPlan,
		Summary: `план статьи на месяц (право decide или budgets): <ГГГГ-ММ> <статья> <сумма|-> [--note "…"]; «-» снимает план`})
	Command(Cmd{Name: "budget import", Run: budgetImport,
		Summary: `планы списком (право decide или budgets): <файл|-|JSON> — {"source": "closes.md", "plans": [{"month", "item", "amount", "note"}]}`})
}

func budgetsShow(ctx context.Context, args []string) error {
	fs := Flags("budgets")
	from := fs.String("from", "", "первый месяц, ГГГГ-ММ (по умолчанию — первый месяц журнала)")
	to := fs.String("to", "", "последний месяц, ГГГГ-ММ (по умолчанию — текущий)")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	q := url.Values{}
	if *from != "" {
		q.Set("from", *from)
	}
	if *to != "" {
		q.Set("to", *to)
	}
	var ans budgets.Answer
	if err := c.Do(ctx, http.MethodGet, "/v1/budgets?"+q.Encode(), nil, &ans); err != nil {
		return apiError(err)
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	printBudgets(out, ans)
	return out.Flush()
}

// printBudgets renders each month: in and out by category against the plan.
func printBudgets(out *bufio.Writer, ans budgets.Answer) {
	for i, m := range ans.Months {
		if i > 0 {
			fmt.Fprintln(out)
		}
		state := "открыт"
		if m.Closed {
			state = "закрыт"
		}
		if !m.Complete {
			state += ", учёт неполный"
		}
		fact := map[string]int64{}
		var in, out0, refunds, ownerIn, ownerOut, traffic int64
		for _, r := range m.Rows {
			switch r.Kind {
			case budgets.KindIncome:
				in += r.AmountMicros
			case budgets.KindExpense:
				out0 += r.AmountMicros
				fact[r.Category] += r.AmountMicros
				if r.Channel != "" {
					traffic += r.AmountMicros
				}
			case budgets.KindRefund:
				refunds += r.AmountMicros
			case budgets.KindOwnerIn:
				ownerIn += r.AmountMicros
			case budgets.KindOwnerOut:
				ownerOut += r.AmountMicros
			}
		}
		fmt.Fprintf(out, "%s (%s): пришло %s, ушло %s (возвраты −%s), осталось %s; трафик %s; владелец вынул %s, вложил %s\n",
			m.Month, state, usd(in), usd(out0-refunds), usd(refunds), usd(in-out0+refunds), usd(traffic), usd(ownerOut), usd(ownerIn))
		plan := map[string]int64{}
		items := map[string]bool{}
		for _, p := range m.Plans {
			plan[p.Item] = p.PlanMicros
			items[p.Item] = true
		}
		for c := range fact {
			items[c] = true
		}
		names := make([]string, 0, len(items))
		for n := range items {
			names = append(names, n)
		}
		sort.Strings(names)
		rows := make([][]string, 0, len(names))
		for _, n := range names {
			p := "—"
			if v, ok := plan[n]; ok {
				p = usd(v)
			}
			f := usd(fact[n])
			if n == budgets.ItemIncome {
				f = usd(in)
			}
			rows = append(rows, []string{n, p, f})
		}
		writeTable(out, []string{"статья", "план", "факт"}, rows, []bool{false, true, true})
		if len(m.PagePlans) > 0 {
			var parts []string
			var day int64
			for _, p := range m.PagePlans {
				parts = append(parts, p.Page+" "+usd(p.PerDayMicros))
				day += p.PerDayMicros
			}
			fmt.Fprintf(out, "план оборота: %s в день (%s)\n", usd(day), strings.Join(parts, ", "))
		}
	}
}

func budgetPlan(ctx context.Context, args []string) error {
	fs := Flags("budget plan")
	note := fs.String("note", "", "заметка к плану")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		return Exit{Code: 2, Msg: `нужно: ledger budget plan <ГГГГ-ММ> <статья> <сумма|-> [--note "…"]`}
	}
	return budgetWrite(ctx, budgets.SetInput{Source: "cli", Plans: []budgets.PlanInput{{Month: pos[0], Item: pos[1], Amount: pos[2], Note: *note}}})
}

func budgetImport(ctx context.Context, args []string) error {
	fs := Flags("budget import")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен файл: ledger budget import <файл|-|JSON>"}
	}
	raw, err := jsonArg(pos[0])
	if err != nil {
		return err
	}
	var in budgets.SetInput
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Exit{Code: 2, Msg: "не разобрано: " + err.Error()}
	}
	if in.Source == "" {
		in.Source = "import"
	}
	return budgetWrite(ctx, in)
}

func budgetWrite(ctx context.Context, in budgets.SetInput) error {
	c, err := apiClient()
	if err != nil {
		return err
	}
	var ans budgets.SetAnswer
	if err := c.Do(ctx, http.MethodPost, "/v1/budgets/plans", in, &ans); err != nil {
		return apiError(err)
	}
	fmt.Printf("записано планов: %d, уже такие: %d\n", ans.Written, ans.Unchanged)
	return nil
}
