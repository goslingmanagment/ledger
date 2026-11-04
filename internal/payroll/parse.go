// Package payroll keeps the debts to people: the strict reading of the payout sheets (a port of /OS
// scripts/payroll.py), the accrual per work day, the clearing of payouts from the journal, the FIFO of
// «за какие дни», and what the hub shows that nobody entered.
package payroll

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
)

// Columns of a week block, 0-based: first day (C), «З/П» (K), «Фикс» (M).
const (
	colD0  = 2
	colZP  = 10
	colFix = 12
)

// epoch is day zero of Google Sheets' serial dates.
var epoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// Fail is a refusal of the strict reading; the message says which cells to look at.
type Fail struct{ Msg string }

func (e *Fail) Error() string { return e.Msg }

func failf(format string, args ...any) *Fail { return &Fail{Msg: fmt.Sprintf(format, args...)} }

// PageRow is one row of a week block: the page and its seven cells (nil: no number).
type PageRow struct {
	Page   string      `json:"page"`
	Values [7]*float64 `json:"values"`
}

// Block is a parsed week.
type Block struct {
	Start string    `json:"start"`
	Pages []PageRow `json:"pages"`
}

// Tab is a parsed sheet tab. Days holds every date of every block, zero included: the accrual per day
// is the percent by the day's revenue plus the fix split over the days with records.
type Tab struct {
	Days   map[string]float64 `json:"days"`
	Worked []string           `json:"worked"`
	KV     float64            `json:"kv"`
	Paid   float64            `json:"paid"`
	Total  float64            `json:"total"`
	Rate   *float64           `json:"rate,omitempty"`
	Notes  []string           `json:"notes"`
	Blocks []Block            `json:"blocks"`
}

func at(row []Cell, i int) Cell {
	if i < len(row) {
		return row[i]
	}
	return ""
}

func indexOf(row []Cell, text string) int {
	for i, c := range row {
		if cellEq(c, text) {
			return i
		}
	}
	return -1
}

func serialDate(serial float64) (time.Time, bool) {
	if math.Abs(serial) > 3_000_000 { // past year 9999: Python would crash on it, here it is a refusal
		return time.Time{}, false
	}
	return epoch.AddDate(0, 0, int(serial)), true
}

// cellRange prints [(row, column)] as «C351:E351, G12»: adjacent columns of a row as a range.
func cellRange(points [][2]int) string {
	sort.Slice(points, func(i, j int) bool {
		if points[i][0] != points[j][0] {
			return points[i][0] < points[j][0]
		}
		return points[i][1] < points[j][1]
	})
	type run struct{ r, a, b int }
	var runs []run
	for _, p := range points {
		if n := len(runs); n > 0 && runs[n-1].r == p[0] && runs[n-1].b == p[1]-1 {
			runs[n-1].b = p[1]
		} else {
			runs = append(runs, run{p[0], p[1], p[1]})
		}
	}
	parts := make([]string, len(runs))
	for i, r := range runs {
		parts[i] = fmt.Sprintf("%c%d", 'A'+rune(r.a), r.r)
		if r.b > r.a {
			parts[i] += fmt.Sprintf(":%c%d", 'A'+rune(r.b), r.r)
		}
	}
	return strings.Join(parts, ", ")
}

// sigOf is what makes two weeks copies: the page cell and seven numbers, compared the way Python's
// tuple equality compares them (1 == 1.0 == True, 0.0 == -0.0).
func sigOf(body []bodyRow) string {
	var b strings.Builder
	cell := func(c Cell) {
		switch x := c.(type) {
		case nil:
			b.WriteString("N")
		case string:
			b.WriteString("s" + x)
		case bool:
			if x {
				b.WriteString("n1")
			} else {
				b.WriteString("n0")
			}
		default:
			if n, ok := asNum(c); ok {
				fmt.Fprintf(&b, "n%x", n.v+0)
			}
		}
		b.WriteByte(0)
	}
	for _, r := range body {
		cell(at(r.cells, 1))
		for k := range 7 {
			if v := at(r.cells, colD0+k); isNumber(v) {
				cell(v)
			} else {
				cell(nil)
			}
		}
		b.WriteByte(1)
	}
	return b.String()
}

func isNumber(c Cell) bool {
	_, ok := asNum(c)
	return ok
}

type bodyRow struct {
	num   int // 1-based sheet row
	cells []Cell
}

// truthy is bool(cell) of a cell that passed the number check: a non-zero number.
func truthy(c Cell) bool {
	n, ok := asNum(c)
	return ok && n.v != 0
}

// ParseTab is payroll.py parse_tab: the tab from A1 with UNFORMATTED_VALUE cells; today is the calendar
// date of the given time in its own location. It refuses (*Fail)
// on a date that is not a serial number, weeks out of order, days after today, a copied week, a «З/П»
// that does not add up or does not match the rate (±$1), a header that does not equal the blocks
// (±$1) and data outside the blocks. All the problems of the tab come in one refusal.
func ParseTab(rows [][]Cell, where string, today time.Time) (*Tab, error) {
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	first, second := []Cell(nil), []Cell(nil)
	if len(rows) > 0 {
		first = rows[0]
	}
	if len(rows) > 1 {
		second = rows[1]
	}
	kv, kvOK := asNum(at(second, 0))
	if !cellEq(at(first, 0), "К выплате:") || !kvOK {
		return nil, failf("%s: A1 «К выплате:» и число в A2 не на месте — формат вкладки сменился", where)
	}
	paidAt := indexOf(first, "Выплачено:")
	if paidAt < 0 {
		return nil, failf("%s: в шапке нет числа «Выплачено:» — формат вкладки сменился", where)
	}
	paid, paidOK := asNum(at(second, paidAt))
	if !paidOK {
		return nil, failf("%s: в шапке нет числа «Выплачено:» — формат вкладки сменился", where)
	}
	var rate *float64
	if r, ok := asNum(at(second, 12)); ok && cellEq(at(first, 12), "Ставка %") {
		rate = &r.v
	}
	shift := 0.0
	if s, ok := asNum(at(second, 13)); ok && strings.HasPrefix(pyStr(at(first, 13)), "Фикс/см") {
		shift = s.v
	}

	tab := &Tab{Days: map[string]float64{}, Worked: []string{}, KV: kv.v, Paid: paid.v, Notes: []string{}, Blocks: []Block{}, Rate: rate}
	var errs []string
	var future [][2]int
	seen := map[string]string{}
	inside := map[int]bool{0: true, 1: true}
	type start struct {
		serial int
		text   string
	}
	var starts []start
	worked := map[string]bool{}
	if rate == nil {
		tab.Notes = append(tab.Notes, where+": в шапке нет ставки % — «З/П» по выручке не сверена")
	}
	for h0, hdr := range rows {
		if len(hdr) == 0 || !cellEq(hdr[0], "Работяга") {
			continue
		}
		h := h0 + 1
		span := fmt.Sprintf("C%d:I%d", h, h)
		inside[h0] = true
		var dates [7]float64
		numeric := true
		for k := range 7 {
			n, ok := asNum(at(hdr, colD0+k))
			if !ok {
				numeric = false
				break
			}
			dates[k] = n.v
		}
		var d0 time.Time
		if numeric {
			d0, numeric = serialDate(dates[0])
		}
		if !numeric {
			errs = append(errs, span+" — дата недели не числом (нужен серийный номер даты с годом)")
			continue
		}
		consecutive := true
		for k := range 7 {
			if dates[k] != dates[0]+float64(k) {
				consecutive = false
			}
		}
		if !consecutive {
			errs = append(errs, span+" — даты недели не идут подряд")
			continue
		}
		if n := len(starts); n > 0 {
			step := int(dates[0]) - starts[n-1].serial
			if step < 1 || step > 7 { // overlap is allowed, a step back or a gap is not
				errs = append(errs, fmt.Sprintf("%s (%s) — после блока %s недели идут не подряд (шаг %d дн.)",
					span, d0.Format("02.01.2006"), starts[n-1].text, step))
			}
		}
		label := fmt.Sprintf("%s (%s)", span, d0.Format("02.01.2006"))
		starts = append(starts, start{int(dates[0]), label})
		var body []bodyRow
		i := h // 0-based index of the next row
		for i < len(rows) && !cellEq(at(rows[i], 1), "Сумма") && !cellEq(at(rows[i], 0), "Работяга") {
			inside[i] = true
			blank := true
			for _, x := range rows[i] {
				if pyStrip(pyStr(x)) != "" {
					blank = false
					break
				}
			}
			if !blank {
				body = append(body, bodyRow{i + 1, rows[i]})
			}
			i++
		}
		if i >= len(rows) || !cellEq(at(rows[i], 1), "Сумма") {
			errs = append(errs, "блок "+span+": нет строки «Сумма»")
			continue
		}
		inside[i] = true
		var bad [][2]int
		for _, r := range body {
			for _, c := range []int{colD0, colD0 + 1, colD0 + 2, colD0 + 3, colD0 + 4, colD0 + 5, colD0 + 6, colZP, colFix} {
				if v := at(r.cells, c); !cellEq(v, "") && !isNumber(v) {
					bad = append(bad, [2]int{r.num, c})
				}
			}
		}
		if len(bad) > 0 {
			errs = append(errs, cellRange(bad)+" — не число")
			continue
		}
		anyValue := false
		for _, r := range body {
			for k := range 7 {
				if truthy(at(r.cells, colD0+k)) {
					anyValue = true
					if d0.AddDate(0, 0, k).After(today) {
						future = append(future, [2]int{r.num, colD0 + k})
					}
				}
			}
		}
		if anyValue {
			sig := sigOf(body)
			if prev, ok := seen[sig]; ok {
				errs = append(errs, fmt.Sprintf("блок %s (%s) — копия недели из блока %s", span, d0.Format("02.01.2006"), prev))
			} else {
				seen[sig] = label
			}
		}
		var sums [7]num
		var entered [7]bool
		for k := range 7 {
			var col []num
			for _, r := range body {
				if n, ok := asNum(at(r.cells, colD0+k)); ok {
					col = append(col, n)
				}
			}
			sums[k], entered[k] = pySum(col), len(col) > 0
		}
		sumOfSums := pySum(sums[:]).v
		column := func(c int) num {
			var col []num
			for _, r := range body {
				if n, ok := asNum(at(r.cells, c)); ok {
					col = append(col, n)
				}
			}
			return pySum(col)
		}
		fix := column(colFix).v
		payCell := at(rows[i], colZP)
		pay, payOK := asNum(payCell)
		if !payOK || math.Abs(column(colZP).v+fix-pay.v) > 0.01 {
			errs = append(errs, fmt.Sprintf("K%d: «З/П» недели %s не равна сумме строк + фикс %s", i+1, pyStr(payCell), fmtG(fix)))
			continue
		}
		if rate != nil {
			revenuePart := float64(sumOfSums * *rate)
			if math.Abs(revenuePart+fix-pay.v) > 1 {
				errs = append(errs, fmt.Sprintf("K%d: «З/П» недели %s ≠ выручка %s × %s + фикс %s", i+1, fmtG(pay.v),
					fmtG(sumOfSums), fmtG(*rate), fmtG(fix)))
				continue
			}
		}
		tab.Total += pay.v
		pct := money.RoundN(pay.v-fix, 6)
		n := 0
		for _, e := range entered {
			if e {
				n++
			}
		}
		if (pct != 0 && sumOfSums == 0) || (fix != 0 && n == 0) {
			errs = append(errs, fmt.Sprintf("блок %s: начислено %s, а записей по дням нет — день работы не определить", span, fmtG(pay.v)))
			continue
		}
		if shift != 0 && fix != 0 && math.Abs(fix/shift-float64(n)) > 0.01 {
			tab.Notes = append(tab.Notes, fmt.Sprintf("%s неделя %s: фикс %s = %s смен, дней с записями %d — фикс поровну на дни с записями",
				where, d0.Format("02.01"), fmtG(fix), fmtG(fix/shift), n))
		}
		block := Block{Start: d0.Format("2006-01-02"), Pages: make([]PageRow, 0, len(body))}
		for _, r := range body {
			row := PageRow{Page: pyStr(at(r.cells, 1))}
			for k := range 7 {
				if v, ok := asNum(at(r.cells, colD0+k)); ok {
					x := v.v
					row.Values[k] = &x
				}
			}
			block.Pages = append(block.Pages, row)
		}
		tab.Blocks = append(tab.Blocks, block)
		for k := range 7 {
			date := d0.AddDate(0, 0, k).Format("2006-01-02")
			part := 0.0
			if pct != 0 {
				part = float64(pct*sums[k].v) / sumOfSums
			}
			if entered[k] && fix != 0 {
				part += fix / float64(n)
			}
			tab.Days[date] += part
			if entered[k] {
				worked[date] = true
			}
		}
	}
	var stray []string
	for i, r := range rows {
		if inside[i] {
			continue
		}
		for _, x := range r {
			if s, ok := x.(string); (ok && pyStrip(s) != "") || truthy(x) {
				stray = append(stray, fmt.Sprint(i+1))
				break
			}
		}
	}
	if len(stray) > 0 {
		errs = append(errs, "строки "+strings.Join(stray[:min(5, len(stray))], ", ")+" — данные вне недельных блоков (не попали бы в расчёт)")
	}
	if len(future) > 0 {
		errs = append(errs, cellRange(future)+" — заполнены дни позже "+today.Format("02.01.2006"))
	}
	if sum := paid.v + kv.v; math.Abs(sum-tab.Total) > 1 {
		errs = append(errs, fmt.Sprintf("шапка «Выплачено» %.2f + «К выплате» %.2f = %.2f, а сумма блоков %.2f — блок не разобран",
			paid.v, kv.v, sum, tab.Total))
	}
	if len(errs) > 0 {
		return nil, &Fail{Msg: where + ": " + strings.Join(errs, "; ")}
	}
	for d := range worked {
		tab.Worked = append(tab.Worked, d)
	}
	sort.Strings(tab.Worked)
	return tab, nil
}

// Names is payroll.py names: the chatters between the rows «Чаттер» and «ИТОГО» of a list tab.
func Names(rows [][]Cell, where string) ([]string, error) {
	col := make([]string, len(rows))
	from, to := -1, -1
	for i, r := range rows {
		col[i] = pyStr(at(r, 0))
		if col[i] == "Чаттер" && from < 0 {
			from = i
		}
		if col[i] == "ИТОГО" && to < 0 {
			to = i
		}
	}
	if from < 0 || to < 0 {
		return nil, failf("%s: нет блока «Чаттер … ИТОГО»", where)
	}
	out := []string{}
	for i := from + 1; i < to; i++ {
		if col[i] != "" {
			out = append(out, col[i])
		}
	}
	return out, nil
}
