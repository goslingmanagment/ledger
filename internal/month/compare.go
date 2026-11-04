package month

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OverlayPayroll returns tabs with a month's days replaced by the closing snapshot's payroll where they
// differ by more than a cent: the sheets as they stood at the close. differences lists what was replaced.
func OverlayPayroll(tabs []Tab, payroll map[string]map[string]float64, month string) ([]Tab, []string) {
	var diffs []string
	out := make([]Tab, 0, len(tabs))
	for _, t := range tabs {
		key := t.Model + "/" + t.Name
		snap, known := payroll[key]
		days := map[string]float64{}
		for d, v := range t.Days {
			days[d] = v
		}
		if !known {
			for d, v := range t.Days {
				if d[:7] == month && v != 0 {
					diffs = append(diffs, fmt.Sprintf("%s %s: %.2f — вкладки не было на закрытии", key, d, v))
					delete(days, d)
				}
			}
		} else {
			for d, v := range t.Days {
				if d[:7] != month {
					continue
				}
				if s, ok := snap[d]; !ok && math.Abs(v) > 0.006 {
					diffs = append(diffs, fmt.Sprintf("%s %s: сейчас %.2f, на закрытии дня не было", key, d, v))
					delete(days, d)
				} else if ok && math.Abs(v-s) > 0.006 {
					diffs = append(diffs, fmt.Sprintf("%s %s: сейчас %.2f, на закрытии %.2f", key, d, v, s))
					days[d] = s
				}
			}
			for d, s := range snap {
				if _, ok := t.Days[d]; !ok && math.Abs(s) > 0.006 {
					diffs = append(diffs, fmt.Sprintf("%s %s: сейчас нет, на закрытии %.2f", key, d, s))
					days[d] = s
				}
			}
		}
		t.Days = days
		out = append(out, t)
	}
	sort.Strings(diffs)
	return out, diffs
}

// CompareSnapshot compares a computed card with a closing snapshot of the script to the cent and lists
// every difference. An empty list means «совпадает до цента».
func CompareSnapshot(card *Card, raw []byte) ([]string, error) {
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	got := map[string]any{}
	b, _ := json.Marshal(card.Snapshot())
	if err := json.Unmarshal(b, &got); err != nil {
		return nil, err
	}
	var diffs []string
	num := func(v any) float64 { f, _ := v.(float64); return f }
	cmp := func(name string, a, b any) {
		if math.Abs(num(a)-num(b)) > 1e-9 {
			diffs = append(diffs, fmt.Sprintf("%s: %v, в снимке %v", name, a, b))
		}
	}
	gt, _ := got["totals"].(map[string]any)
	st, _ := s["totals"].(map[string]any)
	for k, v := range st {
		cmp("итог "+k, gt[k], v)
	}
	cmp("цифра решения", got["decision"], s["decision"])
	cmp("последняя строка журнала", got["ledger_n"], s["ledger_n"])
	type lk struct{ m, c string }
	index := func(v any) map[lk]map[string]any {
		out := map[lk]map[string]any{}
		ls, _ := v.([]any)
		for _, l := range ls {
			m, _ := l.(map[string]any)
			mm, _ := m["model"].(string)
			cc, _ := m["channel"].(string)
			out[lk{mm, cc}] = m
		}
		return out
	}
	gl, sl := index(got["lines"]), index(s["lines"])
	for k, v := range sl {
		g, ok := gl[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("строка %s/%s: нет", k.m, k.c))
			continue
		}
		for f, x := range v {
			if _, isNum := x.(float64); isNum {
				cmp(fmt.Sprintf("строка %s/%s %s", k.m, k.c, f), g[f], x)
			}
		}
	}
	for k := range gl {
		if _, ok := sl[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("строка %s/%s: лишняя", k.m, k.c))
		}
	}
	ge, _ := got["expenses"].(map[string]any)
	se, _ := s["expenses"].(map[string]any)
	cmp("расходы", ge["total"], se["total"])
	for _, sec := range []string{"chat", "cats"} {
		a, _ := ge[sec].(map[string]any)
		b, _ := se[sec].(map[string]any)
		seen := map[string]bool{}
		for k, v := range b {
			seen[k] = true
			if math.Abs(num(a[k])-num(v)) > 0.005 {
				diffs = append(diffs, fmt.Sprintf("расходы.%s %s: %v, в снимке %v", sec, k, a[k], v))
			}
		}
		for k, v := range a {
			if !seen[k] && math.Abs(num(v)) > 0.005 {
				diffs = append(diffs, fmt.Sprintf("расходы.%s %s: %v, в снимке нет", sec, k, v))
			}
		}
	}
	ax, _ := ge["extra"].([]any)
	bx, _ := se["extra"].([]any)
	if len(ax) != len(bx) {
		diffs = append(diffs, fmt.Sprintf("выплаты сверх таблиц: %d, в снимке %d", len(ax), len(bx)))
	} else {
		for i := range ax {
			a, _ := ax[i].([]any)
			b, _ := bx[i].([]any)
			if len(a) > 1 && len(b) > 1 && (num(a[0]) != num(b[0]) || math.Abs(num(a[1])-num(b[1])) > 1e-9) {
				diffs = append(diffs, fmt.Sprintf("сверх таблиц %v, в снимке %v", a, b))
			}
		}
	}
	gtr := map[float64]map[string]any{}
	if ls, ok := got["transfers"].([]any); ok {
		for _, x := range ls {
			m, _ := x.(map[string]any)
			gtr[num(m["n"])] = m
		}
	}
	if ls, ok := s["transfers"].([]any); ok {
		for _, x := range ls {
			m, _ := x.(map[string]any)
			g, ok := gtr[num(m["n"])]
			if !ok {
				diffs = append(diffs, fmt.Sprintf("перевод #%v: нет", m["n"]))
				continue
			}
			for _, f := range []string{"amount", "est", "k", "month"} {
				cmp(fmt.Sprintf("перевод #%v %s", m["n"], f), g[f], m[f])
			}
			delete(gtr, num(m["n"]))
		}
	}
	for n := range gtr {
		diffs = append(diffs, fmt.Sprintf("перевод #%v: лишний", n))
	}
	greq := map[string]map[string]any{}
	if ls, ok := got["requests"].([]any); ok {
		for _, x := range ls {
			m, _ := x.(map[string]any)
			greq[m["page"].(string)+" "+m["t"].(string)] = m
		}
	}
	if ls, ok := s["requests"].([]any); ok {
		for _, x := range ls {
			m, _ := x.(map[string]any)
			k := m["page"].(string) + " " + m["t"].(string)
			g, ok := greq[k]
			if !ok {
				diffs = append(diffs, "заявка "+k+": нет")
				continue
			}
			cmp("заявка "+k+" amount", g["amount"], m["amount"])
			cmp("заявка "+k+" avail", g["avail"], m["avail"])
			if fmt.Sprint(g["transfer"]) != fmt.Sprint(m["transfer"]) {
				diffs = append(diffs, fmt.Sprintf("заявка %s перевод: %v, в снимке %v", k, g["transfer"], m["transfer"]))
			}
			delete(greq, k)
		}
	}
	for k := range greq {
		diffs = append(diffs, "заявка "+k+": лишняя")
	}
	gp, _ := got["pages"].(map[string]any)
	sp, _ := s["pages"].(map[string]any)
	for k, v := range sp {
		cmp("оборот "+k, gp[k], v)
		delete(gp, k)
	}
	for k := range gp {
		diffs = append(diffs, "оборот "+k+": лишний")
	}
	sort.Strings(diffs)
	return diffs, nil
}

var planHead = regexp.MustCompile(`^### План оборота (\d{4}-\d{2})\s*$`)

// PlanRow is one page of a month's turnover plan.
type PlanRow struct {
	Month  string
	Page   string
	PerDay float64
}

// ParsePlans reads the «План оборота ГГГГ-ММ» tables of the closing notes, strictly like the script.
func ParsePlans(text string) ([]PlanRow, error) {
	lines := strings.Split(text, "\n")
	var out []PlanRow
	pageRe := regexp.MustCompile(`^[a-z0-9-]+$`)
	numRe := regexp.MustCompile(`^\d+(\.\d+)?$`)
	for i := 0; i < len(lines); i++ {
		m := planHead.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		n := 0
		for j := i + 1; j < len(lines); j++ {
			line := lines[j]
			if strings.HasPrefix(line, "#") || (n > 0 && !strings.HasPrefix(line, "|")) {
				break
			}
			if !strings.HasPrefix(line, "|") {
				continue
			}
			cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
			for k := range cells {
				cells[k] = strings.TrimSpace(cells[k])
			}
			if cells[0] == "Страница" || strings.Trim(cells[0], "-: ") == "" {
				continue
			}
			if len(cells) < 2 || !pageRe.MatchString(cells[0]) || !numRe.MatchString(cells[1]) {
				return nil, fmt.Errorf("строка %d: план «%s» — нужно «| страница | $ в день | …»", j+1, line)
			}
			v, _ := strconv.ParseFloat(cells[1], 64)
			out = append(out, PlanRow{m[1], cells[0], v})
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("в «План оборота %s» нет строк таблицы", m[1])
		}
	}
	return out, nil
}
