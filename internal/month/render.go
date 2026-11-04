package month

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func thousands(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	out := b.String()
	if hasFrac {
		out += "." + frac
	}
	if neg {
		out = "−" + out
	}
	return out
}

// Money prints like the card: "1 234.50", "−3.00", "—" for zero.
func Money(x float64) string {
	if math.Abs(x) < 5e-3 {
		return "—"
	}
	return thousands(strconv.FormatFloat(x, 'f', 2, 64))
}

// USD prints whole dollars for the owner's messages.
func USD(x float64, sign bool) string {
	pre := ""
	switch {
	case x <= -.5:
		pre = "−"
	case sign && x >= .5:
		pre = "+"
	}
	return pre + "$" + thousands(strconv.FormatFloat(math.Abs(x), 'f', 0, 64))
}

var ruMonths = strings.Fields("Январь Февраль Март Апрель Май Июнь Июль Август Сентябрь Октябрь Ноябрь Декабрь")

func monthName(mo string) string {
	n, _ := strconv.Atoi(mo[5:])
	return ruMonths[n-1]
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Render prints the card for people. ceoRate nil: the safeguard is not judged.
func (c *Card) Render(ceoRate *float64, res []Check, extraNotes []string) string {
	t := c.Totals()
	cfg := c.Cover.Cfg
	var out []string
	var failed []string
	for _, x := range res {
		if !x.OK {
			failed = append(failed, fmt.Sprintf("- %d — %s", x.N, x.Msg))
		}
	}
	if len(failed) > 0 {
		out = append(out, "**ПРОВЕРКИ НЕ ПРОЙДЕНЫ** — цифры ниже не для решений:")
		out = append(out, failed...)
		out = append(out, "")
	}
	name := func(m string) string {
		if n, ok := cfg.Names[m]; ok {
			return n
		}
		return m
	}
	cols := []struct{ key, title string }{{"net", "Оборот"}, {"earned", "Заработано"}, {"arrived", "Пришло"}, {"hold", "Холд"},
		{"page", "На странице"}, {"riding", "Едет"}, {"stuck", "Застряло"}}
	head := "| Строка |"
	sep := "|---|"
	for _, col := range cols {
		head += " " + col.title + " |"
		sep += "---:|"
	}
	out = append(out, fmt.Sprintf("# Месяц %s · на %s МСК", c.Month, c.AsOf.In(ts.MSK).Format("2006-01-02 15:04")),
		fmt.Sprintf("ledger по #%d; хаб и заявки на срез; таблицы выплат — текущее состояние", c.LedgerN), "", head, sep)
	order := map[string]int{}
	for i, m := range cfg.Models {
		order[m] = i
	}
	lines := append([]*Line(nil), c.Lines...)
	sort.SliceStable(lines, func(i, j int) bool {
		a, aok := order[lines[i].Model]
		b, bok := order[lines[j].Model]
		if !aok {
			a = len(cfg.Models)
		}
		if !bok {
			b = len(cfg.Models)
		}
		if a != b {
			return a < b
		}
		return lines[i].Channel < lines[j].Channel
	})
	for _, l := range lines {
		row := "| " + name(l.Model) + " · " + l.Channel + " |"
		for _, col := range cols {
			row += " " + Money(l.Vals.Vals[col.key]) + " |"
		}
		out = append(out, row)
	}
	totals := map[string]float64{"net": t.Net, "earned": t.Earned, "arrived": t.Arrived, "hold": t.Hold, "page": t.Page, "riding": t.Riding, "stuck": t.Stuck}
	row := "| **Итого** |"
	for _, col := range cols {
		row += " **" + Money(totals[col.key]) + "** |"
	}
	out = append(out, row, "", "- OF: заявки — из хаба (ofapi_payout_requests), в перевод — по срезу, как Fansly.")
	for _, m := range c.OffOrder {
		out = append(out, fmt.Sprintf("- вне платформ %s: %s", name(m), strings.Join(c.Off[m].Rows, ", ")))
	}
	out = append(out, "", "## Переводы", "", "| Перевод | Дата | Срез | Оценка покрытого | Пришло | k | Из них за месяц |", "|---|---|---|---:|---:|---:|---:|")
	for _, m := range sortedTransferModels(c.Cover) {
		for _, tr := range c.Cover.Transfers[m] {
			if !tr.HasK {
				continue
			}
			arrived := tr.Amount
			if cfg.PayeeOf(m) != "model" {
				arrived = float64(cfg.Share * (tr.Amount - tr.Fee))
			}
			bad, inMonth := "", float64(tr.ByMonth[c.Month]*tr.K)
			if len(tr.Bad) > 0 {
				bad, inMonth = " НЕ ПРИНЯТ", 0
			}
			out = append(out, fmt.Sprintf("| #%d %s | %s | %s | %s | %s | %.4f%s | %s |", tr.N, name(m), tr.Date, tr.Cut.In(ts.MSK).Format("02.01 15:04"),
				Money(tr.Est), Money(arrived), tr.K, bad, Money(inMonth)))
		}
	}
	e := c.Exp
	if e == nil {
		return strings.Join(append(out, "", "Расходы не посчитаны: таблицы выплат не загружены."), "\n")
	}
	var chat []string
	for _, m := range e.Chat.Keys {
		chat = append(chat, name(m)+" "+Money(e.Chat.Vals[m]))
	}
	var extras []string
	extraSum := 0.0
	for _, x := range e.Extra {
		extras = append(extras, fmt.Sprintf("#%d %.2f", x.N, x.Amount))
		extraSum += x.Amount
	}
	ex := strings.Join(extras, ", ")
	if ex == "" {
		ex = "нет"
	}
	out = append(out, "", "## Расходы", "", "| Статья | Сумма |", "|---|---:|",
		fmt.Sprintf("| Чаттеры по таблицам (%s) | %s |", strings.Join(chat, ", "), Money(e.Chat.Sum())),
		fmt.Sprintf("| Чаттеры сверх таблиц (%s) | %s |", ex, Money(extraSum)))
	cats := append([]string(nil), e.Cats.Keys...)
	sort.SliceStable(cats, func(i, j int) bool { return e.Cats.Vals[cats[i]] > e.Cats.Vals[cats[j]] })
	for _, k := range cats {
		out = append(out, fmt.Sprintf("| %s | %s |", k, Money(e.Cats.Vals[k])))
	}
	dec := *t.Decision
	out = append(out, fmt.Sprintf("| **Расходы** | **%s** |", Money(e.Total)), "",
		fmt.Sprintf("**Цифра решения** = заработано %s − застряло %s − расходы %s = **%s** (до зарплаты CEO)", Money(t.Earned), Money(t.Stuck), Money(e.Total), Money(dec)))
	if ceoRate == nil {
		out = append(out, "Предохранитель: без --ceo-rate вердикт не считаю")
	} else {
		cand := *ceoRate + 500
		need := cand / 0.6
		out = append(out, fmt.Sprintf("Предохранитель %s (ставка %s + 500; нужно ≥ %s): %s, запас %s; по оценке ещё в пути %s — до закрытия цифра предварительная",
			Money(cand), Money(*ceoRate), Money(need), Verdict(dec, cand, nil), Money(dec-need), Money(t.Transit)))
	}
	notes := dedupe(append(append(append([]string(nil), e.Notes...), c.Notes...), extraNotes...))
	if len(notes) > 0 {
		out = append(out, "", "## Пометки", "")
		for _, n := range notes {
			out = append(out, "- "+n)
		}
	}
	return strings.Join(out, "\n")
}

// tailMonday is the withdrawal for the previous month: the first Monday from the 8th of this month.
func tailMonday(month string) time.Time {
	m0, _, _ := ts.MonthStart(month)
	d := m0.AddDate(0, 0, 7)
	for d.Weekday() != time.Monday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

func planLines(ctx context.Context, q ledger.Querier, cfg Config, mo string, start, end time.Time) ([]string, error) {
	n := int(end.Sub(start).Hours() / 24)
	rows, err := q.QueryContext(ctx, `SELECT page_label, per_day FROM month_plans WHERE month = ? ORDER BY rowid`, mo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pl struct {
		page string
		v    float64
	}
	var plan []pl
	for rows.Next() {
		var p pl
		if err := rows.Scan(&p.page, &p.v); err != nil {
			return nil, err
		}
		plan = append(plan, p)
	}
	if len(plan) == 0 || n < 3 {
		return nil, nil
	}
	var bad []string
	for _, p := range plan {
		if _, ok := cfg.Routes[p.page]; !ok {
			bad = append(bad, p.page)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, failf("план оборота %s: страниц %s нет в таблице маршрутов", mo, strings.Join(bad, ", "))
	}
	fact := map[string]float64{}
	total := 0.0
	for _, p := range cfg.Pages {
		net, err := hub.NetExcluded(ctx, q, p, start, end)
		if err != nil {
			return nil, err
		}
		fact[p] = net / float64(n)
		total += fact[p]
	}
	goal := 0.0
	var cells []string
	for _, p := range plan {
		goal += p.v
		cell := fmt.Sprintf("%s %s из %s", p.page, USD(fact[p.page], false), USD(p.v, false))
		if fact[p.page] < p.v-.5 {
			cell += " ▼"
		}
		cells = append(cells, cell)
	}
	diff := "ровно"
	if math.Abs(total-goal) >= .5 {
		diff = USD(total-goal, true)
	}
	out := []string{"", fmt.Sprintf("Темп по %s (%d дн.): оборот %s в день, план %s (%s)", end.Add(-24*time.Hour).In(ts.MSK).Format("02.01"), n, USD(total, false), USD(goal, false), diff)}
	for k := 0; k < len(cells); k += 3 {
		e := k + 3
		if e > len(cells) {
			e = len(cells)
		}
		out = append(out, strings.Join(cells[k:e], " · "))
	}
	return out, nil
}

// Brief is the owner's Monday message on the coverage cv (cut: the end of yesterday).
func Brief(ctx context.Context, q ledger.Querier, cv *Cover, tabs []Tab, ceoRate *float64) ([]string, []DriftRow, error) {
	cfg := cv.Cfg
	asOf := cv.AsOf
	today := asOf.Add(time.Second)
	cur := ts.Month(asOf)
	m0, _, _ := ts.MonthStart(cur)
	prev := ts.Month(m0.AddDate(0, 0, -1))
	p0, _, _ := ts.MonthStart(prev)
	pp := ts.Month(p0.AddDate(0, 0, -1))
	name := func(m string) string {
		if n, ok := cfg.Names[m]; ok {
			return n
		}
		return m
	}
	var snap map[string]any
	per, err := ledger.GetPeriod(ctx, q, prev)
	closed := err == nil
	if closed {
		if err := jsonUnmarshal(per.Snapshot, &snap); err != nil {
			return nil, nil, err
		}
	}
	var card *Card
	if !closed {
		if card, err = BuildCard(cv, prev, tabs); err != nil {
			return nil, nil, err
		}
	}
	var dec, exp, earned, stuck, arrived, transit float64
	haveTotals := true
	var who []string
	if closed {
		dec, _ = snap["decision"].(float64)
		if e, ok := snap["expenses"].(map[string]any); ok {
			exp, _ = e["total"].(float64)
		}
		tot, ok := snap["totals"].(map[string]any)
		haveTotals = ok
		if ok {
			earned, _ = tot["earned"].(float64)
			stuck, _ = tot["stuck"].(float64)
			arrived, _ = tot["arrived"].(float64)
			transit, _ = tot["transit"].(float64)
		}
		if ls, ok := snap["lines"].([]any); ok {
			for _, l := range ls {
				m, _ := l.(map[string]any)
				if v, _ := m["stuck"].(float64); v >= .5 {
					mod, _ := m["model"].(string)
					who = append(who, name(mod))
				}
			}
		}
	} else {
		t := card.Totals()
		dec, exp, earned, stuck, arrived, transit = *t.Decision, card.Exp.Total, t.Earned, t.Stuck, t.Arrived, t.Transit
		for _, l := range card.Lines {
			if l.Vals.Vals["stuck"] >= .5 {
				who = append(who, name(l.Model))
			}
		}
	}
	who = dedupe(who)
	cmp := ""
	if pp >= ts.Month(cfg.FullLedger) {
		c2, err := BuildCard(cv, pp, tabs)
		if err != nil {
			return nil, nil, err
		}
		cmp = fmt.Sprintf(" (%s — %s)", strings.ToLower(monthName(pp)), USD(*c2.Totals().Decision, false))
	}
	out := []string{"📊 Месяц · цифры на " + today.In(ts.MSK).Format("02.01"), ""}
	if closed {
		out = append(out, fmt.Sprintf("%s закрыт: прибыль до твоей зарплаты %s%s", monthName(prev), USD(dec, false), cmp))
	} else {
		out = append(out, fmt.Sprintf("%s (предварительно, закрытие после вывода %s)", monthName(prev), tailMonday(cur).Format("02.01")),
			"Прибыль до твоей зарплаты: "+USD(dec, false)+cmp)
	}
	if haveTotals {
		line := fmt.Sprintf("= заработали %s − расходы %s", USD(earned, false), USD(exp, false))
		if stuck >= .5 {
			line += fmt.Sprintf(" − застряло %s (%s, ниже минимума вывода)", USD(stuck, false), strings.Join(who, ", "))
		}
		out = append(out, line)
	}
	var cand *float64
	if closed {
		if v, ok := snap["candidate"].(float64); ok {
			cand = &v
		}
	} else if ceoRate != nil && *ceoRate != 0 {
		v := *ceoRate + 500
		cand = &v
	}
	if cand != nil {
		need := *cand / 0.6
		if closed {
			if v, ok := snap["need"].(float64); ok && v != 0 {
				need = v
			}
		}
		var tr *float64
		if closed && haveTotals {
			tr = &transit
		}
		v := Verdict(dec, need*0.6, tr)
		line := fmt.Sprintf("Повышение до %s %s: нужно от %s, ", USD(*cand, false), v, USD(need, false))
		if dec < need {
			line += "не хватает " + USD(need-dec, false)
		} else {
			line += "запас " + USD(dec-need, false)
		}
		if v == "на грани" {
			line += ", решаешь ты"
		}
		out = append(out, line)
	}
	if haveTotals {
		line := "Из заработанного пришло " + USD(arrived, false)
		if transit >= .5 {
			line += fmt.Sprintf(", ещё %s в пути — придёт с выводом в понедельник %s", USD(transit, false), tailMonday(cur).Format("02.01"))
		}
		out = append(out, "", line)
	}
	if today.In(ts.MSK).Day() >= 8 {
		cc, err := BuildCard(cv, cur, tabs)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, "", fmt.Sprintf("%s по %s: заработали %s, потратили %s", monthName(cur), asOf.In(ts.MSK).Format("02.01"), USD(cc.Totals().Earned, false), USD(cc.Exp.Total, false)))
	}
	pl, err := planLines(ctx, q, cfg, cur, m0, today)
	if err != nil {
		return nil, nil, err
	}
	out = append(out, pl...)
	rows, err := Drift(ctx, q, cv, tabs)
	if err != nil {
		return nil, nil, err
	}
	var flagged []DriftRow
	for _, x := range rows {
		if !x.Flag {
			continue
		}
		flagged = append(flagged, x)
		line := fmt.Sprintf("%s пересчитан: %s → %s (%s), ", monthName(x.Month), USD(x.Reported, false), USD(x.Now, false), USD(x.Delta, true))
		if x.Same {
			line += "вердикт тот же."
		} else {
			line += fmt.Sprintf("вердикт сменился: %s → %s.", x.VerdictTold, x.VerdictNow)
		}
		if len(x.Failed) > 0 {
			line += " Не пройдены: " + strings.Join(x.Failed, ", ") + "."
		}
		out = append(out, "", line, "Причина: "+x.Reason)
	}
	if len(flagged) > 0 {
		out = append(out, "«прими» — запомню новую цифру и больше не напомню")
	}
	return out, flagged, nil
}
