// Package journal reads and writes the ledger.md journal: the eleven-column table, the text blocks
// between tables, the {…} tags of notes and the mapping of a row to balanced entries.
package journal

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Row is one journal line: the eleven columns as written.
type Row struct {
	Num          int
	Date         string
	Type         string
	Category     string
	Counterparty string
	Model        string
	Amount       string
	Currency     string
	Method       string
	Status       string
	Note         string
	Line         int // 1-based line in the file
}

// Cells returns the columns in file order.
func (r Row) Cells() []string {
	return []string{strconv.Itoa(r.Num), r.Date, r.Type, r.Category, r.Counterparty, r.Model, r.Amount, r.Currency,
		r.Method, r.Status, r.Note}
}

// Format renders the row exactly as the journal writes it.
func (r Row) Format() string { return "| " + strings.Join(r.Cells(), " | ") + " |" }

// File is a parsed ledger.md: every line, rows replaced by placeholders, and the rows.
type File struct {
	Lines []string // text lines; a row line is "\x00<num>"
	Rows  []Row
}

var (
	rowRe    = regexp.MustCompile(`^\|\s*(\d+)\s*\|`)
	dateRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	amountRe = regexp.MustCompile(`^\d+(\.\d+)?$`)
)

// Placeholder marks the place of row num in File.Lines.
func Placeholder(num int) string { return "\x00" + strconv.Itoa(num) }

// PlaceholderNum returns the row number of a placeholder line, or 0.
func PlaceholderNum(line string) int {
	if !strings.HasPrefix(line, "\x00") {
		return 0
	}
	n, _ := strconv.Atoi(line[1:])
	return n
}

// Parse reads the file strictly: a row must have eleven cells with a date, a known type and status,
// a plain amount and a number not seen before. All problems are returned together.
func Parse(text string) (File, []error) {
	var f File
	var errs []error
	seen := map[int]int{}
	for i, line := range strings.Split(text, "\n") {
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			f.Lines = append(f.Lines, line)
			continue
		}
		cells := strings.Split(strings.TrimSpace(line), "|")
		cells = cells[1 : len(cells)-1]
		if !strings.HasSuffix(strings.TrimSpace(line), "|") || len(cells) != 11 {
			errs = append(errs, fmt.Errorf("строка %d: %d ячеек вместо 11", i+1, len(cells)))
			f.Lines = append(f.Lines, line)
			continue
		}
		for k := range cells {
			cells[k] = strings.TrimSpace(cells[k])
		}
		num, _ := strconv.Atoi(cells[0])
		r := Row{Num: num, Date: cells[1], Type: cells[2], Category: cells[3], Counterparty: cells[4], Model: cells[5],
			Amount: cells[6], Currency: cells[7], Method: cells[8], Status: cells[9], Note: cells[10], Line: i + 1}
		if prev, dup := seen[num]; dup {
			errs = append(errs, fmt.Errorf("строка %d: #%d уже был в строке %d", i+1, num, prev))
		}
		seen[num] = i + 1
		if !dateRe.MatchString(r.Date) {
			errs = append(errs, fmt.Errorf("#%d: дата «%s»", num, r.Date))
		}
		if r.Type != "income" && r.Type != "expense" && r.Type != "transfer" {
			errs = append(errs, fmt.Errorf("#%d: тип «%s»", num, r.Type))
		}
		if r.Status != "paid" && r.Status != "pending" && r.Status != "cancelled" {
			errs = append(errs, fmt.Errorf("#%d: статус «%s»", num, r.Status))
		}
		if !amountRe.MatchString(r.Amount) {
			errs = append(errs, fmt.Errorf("#%d: сумма «%s»", num, r.Amount))
		}
		if r.Currency != "USD" && r.Currency != "USDT" {
			errs = append(errs, fmt.Errorf("#%d: валюта «%s»", num, r.Currency))
		}
		if r.Format() != line {
			errs = append(errs, fmt.Errorf("#%d: строка не в каноническом виде (лишние пробелы)", num))
		}
		f.Rows = append(f.Rows, r)
		f.Lines = append(f.Lines, Placeholder(num))
	}
	return f, errs
}

// Render prints the file back from its lines and the rows by number.
func Render(lines []string, rows map[int]Row, extra []Row) string {
	var b strings.Builder
	last := -1
	for i, l := range lines {
		if PlaceholderNum(l) > 0 {
			last = i
		}
	}
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if n := PlaceholderNum(l); n > 0 {
			b.WriteString(rows[n].Format())
		} else {
			b.WriteString(l)
		}
		if i == last {
			for _, r := range extra {
				b.WriteByte('\n')
				b.WriteString(r.Format())
			}
		}
	}
	return b.String()
}
