package payroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// MirrorMaxCells is how many cells one run may change without --yes-many: more is likelier a mapping mistake than a
// week of reports.
const MirrorMaxCells = 200

// colPaidMark is the «Выплачено» column of a week block (L): «да» on a page row marks the week paid.
const colPaidMark = 11

// SheetIO is what the mirror does with a spreadsheet: read values (UNFORMATTED_VALUE, as the reader), read formulas, and
// write one batch of cells.
type SheetIO interface {
	Values(ctx context.Context, sheet, rng string) ([][]Cell, error)
	Formulas(ctx context.Context, sheet, rng string) ([][]Cell, error)
	Write(ctx context.Context, sheet string, cells []RangeValue) error
}

// RangeValue is one cell to write: its range in A1 notation with the tab, and the number.
type RangeValue struct {
	Range string      `json:"range"`
	Value json.Number `json:"value"`
}

// MirrorCell is a cell the mirror plans to write, or one it reports and leaves.
type MirrorCell struct {
	Cell   string `json:"cell"` // A1, without the tab
	Date   string `json:"date"`
	Page   string `json:"page"`
	Row    string `json:"row,omitempty"` // the row's name in the tab
	Was    Cell   `json:"was"`
	Want   *int64 `json:"want_micros,omitempty"`
	Reason string `json:"reason,omitempty"`
	row    int    // 1-based
	col    int    // 0-based
}

// TabMirror is the plan of one tab and, after --apply, what happened to it.
type TabMirror struct {
	Model  string `json:"model"`
	Name   string `json:"name"`
	Person string `json:"person,omitempty"`
	Sheet  string `json:"-"`
	// Error is why the tab is left whole: not read, not parsed, a cell that holds text.
	Error   string       `json:"error,omitempty"`
	Writes  []MirrorCell `json:"writes"`
	Skips   []MirrorCell `json:"skips"`
	Correct int          `json:"correct"` // cells that already hold the posted amount
	// After --apply.
	Written   []string `json:"written,omitempty"`
	Unwritten []string `json:"unwritten,omitempty"`
	Failed    string   `json:"failed,omitempty"`
}

// MirrorPlan is the whole run.
type MirrorPlan struct {
	From        string      `json:"from"`
	ReportsFrom string      `json:"reports_from"`
	Tabs        []TabMirror `json:"tabs"`
	// Left are the tabs never read, with why.
	Left []TabMirror `json:"left"`
}

// Cells is how many cells the plan writes.
func (p MirrorPlan) Cells() int {
	n := 0
	for _, t := range p.Tabs {
		n += len(t.Writes)
	}
	return n
}

// MirrorFilter narrows a run.
type MirrorFilter struct{ Model, Person string }

// a1 is the A1 notation of a cell: column 0-based, row 1-based.
func a1(row, col int) string { return fmt.Sprintf("%c%d", 'A'+rune(col), row) }

// tabRange is a range of a tab, its name quoted as Sheets wants it.
func tabRange(tab, cells string) string {
	return "'" + strings.ReplaceAll(tab, "'", "''") + "'!" + cells
}

// paidMark is a week's «Выплачено» cell saying yes.
func paidMark(c Cell) bool {
	switch x := c.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(pyStrip(x))
		return s == "да" || s == "yes" || s == "true"
	}
	return false
}

func isFormula(c Cell) bool {
	s, ok := c.(string)
	return ok && strings.HasPrefix(s, "=")
}

func emptyCell(c Cell) bool {
	if c == nil {
		return true
	}
	s, ok := c.(string)
	return ok && pyStrip(s) == ""
}

// mirrorBlock is a week block as the writer walks it, the way ParseTab walks it.
type mirrorBlock struct {
	start time.Time
	paid  bool
	rows  map[string][]int // page label → 1-based rows
	names map[int]string   // 1-based row → its name
}

// mirrorBlocks finds the week blocks of a tab ParseTab accepted: a «Работяга» row with seven dates, the rows below it up to
// «Сумма». Only rows whose name the reader maps to a Hub page are page rows.
func mirrorBlocks(rows [][]Cell, resolve func(string) (string, bool)) []mirrorBlock {
	var out []mirrorBlock
	for h0, hdr := range rows {
		if len(hdr) == 0 || !cellEq(hdr[0], "Работяга") {
			continue
		}
		n, ok := asNum(at(hdr, colD0))
		if !ok {
			continue
		}
		d0, ok := serialDate(n.v)
		if !ok {
			continue
		}
		b := mirrorBlock{start: d0, rows: map[string][]int{}, names: map[int]string{}}
		for i := h0 + 1; i < len(rows) && !cellEq(at(rows[i], 1), "Сумма") && !cellEq(at(rows[i], 0), "Работяга"); i++ {
			if paidMark(at(rows[i], colPaidMark)) {
				b.paid = true
			}
			name := pyStr(at(rows[i], 1))
			if l, ok := resolve(name); ok {
				b.rows[l] = append(b.rows[l], i+1)
				b.names[i+1] = name
			}
		}
		out = append(out, b)
	}
	return out
}

// PlanMirrorTab is the plan of one tab: values and formulas are the tab from A1 (UNFORMATTED_VALUE and FORMULA), posts the
// person's posted pages of the tab's model from the start date on. It writes nothing.
func PlanMirrorTab(t TabMirror, values, formulas [][]Cell, posts []MirrorPost, resolve func(string) (string, bool), today time.Time) TabMirror {
	t.Writes, t.Skips = []MirrorCell{}, []MirrorCell{}
	where := t.Model + "/" + t.Name
	if _, err := ParseTab(values, where, today); err != nil {
		t.Error = "вкладка не разобрана, не трогаю: " + err.Error()
		return t
	}
	blocks := mirrorBlocks(values, resolve)
	want := map[[2]string]int64{} // date, page
	var keys [][2]string
	for _, p := range posts {
		k := [2]string{p.Date, p.Page}
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
		want[k] += p.Micros
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	skip := func(c MirrorCell, why string) {
		c.Reason = why
		t.Skips = append(t.Skips, c)
	}
	posted := map[string]map[string]bool{} // date → pages posted
	for _, k := range keys {
		date, page := k[0], k[1]
		if posted[date] == nil {
			posted[date] = map[string]bool{}
		}
		posted[date][page] = true
		v := want[k]
		c := MirrorCell{Date: date, Page: page, Want: &v}
		d, err := time.Parse(ts.DateLayout, date)
		if err != nil {
			skip(c, "дата отчёта не читается")
			continue
		}
		var at0 []struct {
			b   *mirrorBlock
			col int
		}
		for i := range blocks {
			if k := int(d.Sub(blocks[i].start).Hours() / 24); k >= 0 && k < 7 {
				at0 = append(at0, struct {
					b   *mirrorBlock
					col int
				}{&blocks[i], colD0 + k})
			}
		}
		if len(at0) == 0 {
			skip(c, "у даты нет колонки во вкладке: блок недели не создаю")
			continue
		}
		if len(at0) > 1 {
			skip(c, "дата стоит в двух блоках недели: не знаю, куда писать")
			continue
		}
		b, col := at0[0].b, at0[0].col
		rs := b.rows[page]
		if len(rs) == 0 {
			skip(c, "страницы нет строкой в блоке недели (или имя строки не сопоставлено: settings.sheet_pages): строку не создаю")
			continue
		}
		if len(rs) > 1 {
			skip(c, "страница стоит в блоке двумя строками: не знаю, какую писать")
			continue
		}
		c.row, c.col, c.Cell, c.Row = rs[0], col, a1(rs[0], col), b.names[rs[0]]
		c.Was = at(values[rs[0]-1], col)
		if rs[0]-1 < len(formulas) && isFormula(at(formulas[rs[0]-1], col)) {
			c.Was = at(formulas[rs[0]-1], col)
			skip(c, "в ячейке формула: не трогаю")
			continue
		}
		n, isNum := asNum(c.Was)
		if !isNum && !emptyCell(c.Was) {
			t.Error = fmt.Sprintf("%s: в ячейке %s не число, а %q — вкладку не трогаю", where, c.Cell, pyStr(c.Was))
			t.Writes = []MirrorCell{}
			return t
		}
		if isNum && money.FromFloat(n.v) == money.Micros(v) {
			t.Correct++
			continue
		}
		if b.paid {
			skip(c, "неделя выплачена («Выплачено: да»): только сообщаю")
			continue
		}
		t.Writes = append(t.Writes, c)
	}
	// A cell with money where the person's report of the day has no such page: left alone, said.
	for _, b := range blocks {
		for k := range 7 {
			date := b.start.AddDate(0, 0, k).Format(ts.DateLayout)
			if posted[date] == nil {
				continue
			}
			for label, rs := range b.rows {
				if posted[date][label] || len(rs) != 1 {
					continue
				}
				v := at(values[rs[0]-1], colD0+k)
				if truthy(v) {
					t.Skips = append(t.Skips, MirrorCell{Cell: a1(rs[0], colD0+k), Date: date, Page: label, Row: b.names[rs[0]], Was: v,
						Reason: "в отчёте дня этой страницы нет: не трогаю"})
				}
			}
		}
	}
	sort.SliceStable(t.Skips, func(i, j int) bool { return t.Skips[i].Date < t.Skips[j].Date })
	return t
}

// PlanMirror reads every tab the base lets the mirror write, through io, and plans it. A tab that cannot be read is an
// error of that tab; ctx stops it.
func PlanMirror(ctx context.Context, cfg Config, base MirrorBase, io SheetIO, f MirrorFilter, now time.Time) (MirrorPlan, error) {
	plan := MirrorPlan{From: base.From, ReportsFrom: base.ReportsFrom, Tabs: []TabMirror{}, Left: []TabMirror{}}
	sheets := map[string]string{}
	for _, t := range cfg.Tables {
		sheets[ledger.Slug(t.Model)] = t.Sheet
	}
	resolve := base.Resolver()
	byTab := map[string][]MirrorPost{}
	for _, p := range base.Posts {
		if base.From == "" || p.Date < base.From {
			continue // before the start date the sheet is the source: never touched
		}
		k := p.Person + "\x00" + ledger.Slug(p.Model)
		byTab[k] = append(byTab[k], p)
	}
	today := now.In(ts.MSK)
	for _, bt := range base.Tabs {
		if f.Model != "" && ledger.Slug(f.Model) != ledger.Slug(bt.Model) {
			continue
		}
		if f.Person != "" && f.Person != bt.Person {
			continue
		}
		t := TabMirror{Model: bt.Model, Name: bt.Name, Person: bt.Person, Writes: []MirrorCell{}, Skips: []MirrorCell{}}
		sheet, ok := sheets[ledger.Slug(bt.Model)]
		switch {
		case bt.Skip != "":
			t.Error = bt.Skip
		case !ok:
			t.Error = "таблицы модели нет в конфиге"
		}
		if t.Error != "" {
			plan.Left = append(plan.Left, t)
			continue
		}
		t.Sheet = sheet
		values, err := io.Values(ctx, sheet, tabRange(bt.Name, "A1:N"))
		var formulas [][]Cell
		if err == nil {
			formulas, err = io.Formulas(ctx, sheet, tabRange(bt.Name, "A1:N"))
		}
		if err != nil {
			if ctx.Err() != nil {
				return plan, ctx.Err()
			}
			t.Error = "вкладка не прочитана: " + err.Error()
			plan.Tabs = append(plan.Tabs, t)
			continue
		}
		plan.Tabs = append(plan.Tabs, PlanMirrorTab(t, values, formulas, byTab[bt.Person+"\x00"+ledger.Slug(bt.Model)], resolve, today))
	}
	return plan, nil
}

// JournalEntry is a line of ~/.config/ledger/sheets-mirror.jsonl: one applied cell.
type JournalEntry struct {
	At    string `json:"at"`
	Sheet string `json:"sheet"`
	Tab   string `json:"tab"`
	Cell  string `json:"cell"`
	Date  string `json:"date"`
	Page  string `json:"page"`
	Was   Cell   `json:"was"`
	Now   string `json:"now"`
}

// ErrMirrorTooMany is the guard against a mapping mistake.
var ErrMirrorTooMany = errors.New("too many cells")

// ApplyMirror writes the plan: one batch per tab, then a read-back of the tab that must hold every written number; each
// cell that reads back right goes to the journal. A failed batch is never retried: the tab says what was and was not
// written, and the run stops there. It refuses more than MirrorMaxCells cells unless many.
func ApplyMirror(ctx context.Context, plan *MirrorPlan, io SheetIO, journal string, many bool, now time.Time) error {
	if n := plan.Cells(); n > MirrorMaxCells && !many {
		return fmt.Errorf("%w: план меняет %d ячеек, больше %d за прогон — проверьте сопоставление по --dry-run и подтвердите --yes-many",
			ErrMirrorTooMany, n, MirrorMaxCells)
	}
	var problems []string
	for i := range plan.Tabs {
		t := &plan.Tabs[i]
		if t.Error != "" || len(t.Writes) == 0 {
			continue
		}
		batch := make([]RangeValue, len(t.Writes))
		for j, c := range t.Writes {
			batch[j] = RangeValue{Range: tabRange(t.Name, c.Cell), Value: json.Number(money.Micros(*c.Want).String())}
		}
		werr := io.Write(ctx, t.Sheet, batch)
		back, rerr := io.Values(ctx, t.Sheet, tabRange(t.Name, "A1:N"))
		var wrong []string
		var entries []JournalEntry
		for _, c := range t.Writes {
			var got Cell
			if rerr == nil && c.row-1 < len(back) {
				got = at(back[c.row-1], c.col)
			}
			if n, ok := asNum(got); rerr == nil && ok && money.FromFloat(n.v) == money.Micros(*c.Want) {
				t.Written = append(t.Written, c.Cell)
				entries = append(entries, JournalEntry{At: ts.Format(now), Sheet: t.Sheet, Tab: t.Name, Cell: c.Cell, Date: c.Date, Page: c.Page,
					Was: c.Was, Now: money.Micros(*c.Want).String()})
				continue
			}
			t.Unwritten = append(t.Unwritten, c.Cell)
			wrong = append(wrong, fmt.Sprintf("%s=%s", c.Cell, pyStr(got)))
		}
		if err := appendJournal(journal, entries); err != nil {
			problems = append(problems, t.Model+"/"+t.Name+": журнал не записан: "+err.Error())
		}
		switch {
		case werr != nil:
			t.Failed = "запись не прошла: " + werr.Error()
			if rerr != nil {
				t.Failed += "; перечитать не удалось (" + rerr.Error() + "): что записано — неизвестно, смотрите вкладку"
			}
			problems = append(problems, t.Model+"/"+t.Name+": "+t.Failed)
			return errors.New(strings.Join(problems, "\n"))
		case rerr != nil:
			t.Failed = "записано, но перечитать не удалось: " + rerr.Error()
		case len(wrong) > 0:
			t.Failed = "после записи ячейки не те (не число или другая сумма): " + strings.Join(wrong, ", ")
		default:
			if _, err := ParseTab(back, t.Model+"/"+t.Name, now.In(ts.MSK)); err != nil {
				t.Failed = "записано, но вкладка теперь не разбирается: " + err.Error()
			}
		}
		if t.Failed != "" {
			problems = append(problems, t.Model+"/"+t.Name+": "+t.Failed)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func appendJournal(path string, entries []JournalEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			f.Close()
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// GogSheets is the mirror's SheetIO through the gog CLI: reads exactly as the reader (Gog, read-only, with its retries),
// formulas the same way with --render=FORMULA, and one batch-update per tab — never retried.
type GogSheets struct{ Account string }

func (g GogSheets) Values(ctx context.Context, sheet, rng string) ([][]Cell, error) {
	return Gog(g.Account)(ctx, sheet, rng)
}

func (g GogSheets) Formulas(ctx context.Context, sheet, rng string) ([][]Cell, error) {
	return gogGet(g.Account, "FORMULA")(ctx, sheet, rng)
}

// Write sends the cells as JSON numbers with USER_ENTERED: a number is stored as a number whatever the sheet's locale
// (only a string would be parsed by the locale), and the read-back checks it.
func (g GogSheets) Write(ctx context.Context, sheet string, cells []RangeValue) error {
	type vr struct {
		Range  string          `json:"range"`
		Values [][]json.Number `json:"values"`
	}
	data := make([]vr, len(cells))
	for i, c := range cells {
		data[i] = vr{Range: c.Range, Values: [][]json.Number{{c.Value}}}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ledger-mirror-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "data.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, "gog", GogWriteArgs(sheet, file, g.Account)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("gog rc=%d: %s", exit.ExitCode(), tail(strings.TrimSpace(stderr.String()), 300))
		}
		return fmt.Errorf("gog не запускается или не ответил за 120 с (%v)", err)
	}
	return nil
}

// GogWriteArgs is the write command: `gog sheets batch-update <sheet> --data-json=@<file> --input=USER_ENTERED --no-input
// --json --account <account>`.
func GogWriteArgs(sheet, file, account string) []string {
	return []string{"sheets", "batch-update", sheet, "--data-json=@" + file, "--input=USER_ENTERED", "--no-input", "--json",
		"--account", account}
}
