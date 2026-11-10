package payroll

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
)

// All made up: person alfa, models alpha and beta, pages page-a, page-b, page-c, page-x.

// fakeGrid is a spreadsheet the mirror reads and writes, the way gog would.
type fakeGrid struct {
	tabs     map[string][][]Cell          // "<sheet>!<tab>"
	formulas map[string]map[[2]int]string // "<sheet>!<tab>" → (1-based row, 0-based col) → formula
	failOn   map[string]error             // sheet → error of a write
	partial  bool                         // a failed write still lands its first cell
	asText   bool                         // a write lands as text (a locale that ate the number)
	writes   int
	cells    int
}

func newGrid() *fakeGrid {
	return &fakeGrid{tabs: map[string][][]Cell{}, formulas: map[string]map[[2]int]string{}, failOn: map[string]error{}}
}

func splitRange(rng string) (tab, cell string) {
	i := strings.LastIndex(rng, "!")
	return strings.ReplaceAll(strings.Trim(rng[:i], "'"), "''", "'"), rng[i+1:]
}

func copyGrid(g [][]Cell) [][]Cell {
	out := make([][]Cell, len(g))
	for i, r := range g {
		out[i] = append([]Cell(nil), r...)
	}
	return out
}

func (f *fakeGrid) Values(_ context.Context, sheet, rng string) ([][]Cell, error) {
	tab, _ := splitRange(rng)
	return copyGrid(f.tabs[sheet+"!"+tab]), nil
}

func (f *fakeGrid) Formulas(_ context.Context, sheet, rng string) ([][]Cell, error) {
	tab, _ := splitRange(rng)
	g := copyGrid(f.tabs[sheet+"!"+tab])
	for p, v := range f.formulas[sheet+"!"+tab] {
		g[p[0]-1][p[1]] = v
	}
	return g, nil
}

func (f *fakeGrid) set(key, cell string, v Cell) {
	col := int(cell[0] - 'A')
	row, _ := strconv.Atoi(cell[1:])
	g := f.tabs[key]
	for len(g[row-1]) <= col {
		g[row-1] = append(g[row-1], "")
	}
	g[row-1][col] = v
}

func (f *fakeGrid) Write(_ context.Context, sheet string, cells []RangeValue) error {
	f.writes++
	if err := f.failOn[sheet]; err != nil {
		if f.partial && len(cells) > 0 {
			tab, cell := splitRange(cells[0].Range)
			f.set(sheet+"!"+tab, cell, cells[0].Value)
			f.cells++
		}
		return err
	}
	for _, c := range cells {
		tab, cell := splitRange(c.Range)
		var v Cell = c.Value
		if f.asText {
			v = strings.ReplaceAll(c.Value.String(), ".", ",")
		}
		f.set(sheet+"!"+tab, cell, v)
		f.cells++
		recalc(f.tabs[sheet+"!"+tab])
	}
	return nil
}

// recalc is what the sheet's formulas do after a write: a row's week (J) and pay (K) at 25%, the week's «З/П» and «К выплате».
func recalc(g [][]Cell) {
	total := 0.0
	for i := range g {
		if !cellEq(at(g[i], 0), "Работяга") {
			continue
		}
		pay := 0.0
		j := i + 1
		for ; j < len(g) && !cellEq(at(g[j], 1), "Сумма"); j++ {
			if len(g[j]) <= colZP {
				continue
			}
			rev := 0.0
			for k := range 7 {
				if v, ok := asNum(at(g[j], colD0+k)); ok {
					rev += v.v
				}
			}
			g[j][9], g[j][colZP] = n(rev), n(rev*0.25)
			pay += rev * 0.25
		}
		g[j][colZP] = n(pay)
		total += pay
	}
	paid, _ := asNum(g[1][4])
	g[1][0] = n(total - paid.v)
}

// mirrorTab is a synthetic tab of alfa on alpha: the week of 26.10 (before the start), of 02.11 and of 09.11 (paid).
func mirrorTab() [][]Cell {
	w1 := week("2026-10-26", map[string][7]any{"Page A": {nil, 40, nil, nil, nil, nil, nil}})
	w2 := week("2026-11-02", map[string][7]any{
		"Page A":       {100, 30, nil, 15, nil, nil, nil}, // 15 is a formula (=10+5)
		"Page B":       {50, nil, nil, nil, nil, nil, nil},
		"Общак":        {5, nil, nil, nil, nil, nil, nil},
		"Страница Икс": {nil, nil, nil, nil, nil, nil, nil},
	})
	w3 := week("2026-11-09", map[string][7]any{"Page A": {10, nil, nil, nil, nil, nil, nil}})
	w3[1] = append(w3[1], "да") // L: «Выплачено»
	total := 0.0
	for _, w := range [][][]Cell{w1, w2, w3} {
		v, _ := asNum(w[len(w)-2][colZP])
		total += v.v
	}
	return sheet(total, 0, w1, w2, w3)
}

// rowOf is the 1-based row of a page in the block of a week.
func rowOf(t *testing.T, g [][]Cell, weekStart, page string) int {
	t.Helper()
	s := serialOf(weekStart)
	for i := 0; i < len(g); i++ {
		if cellEq(at(g[i], 0), "Работяга") && at(g[i], colD0) == Cell(n(s)) {
			for j := i + 1; j < len(g) && !cellEq(at(g[j], 1), "Сумма"); j++ {
				if cellEq(at(g[j], 1), page) {
					return j + 1
				}
			}
		}
	}
	t.Fatalf("no row %s in week %s", page, weekStart)
	return 0
}

func mirrorFixture(t *testing.T) (*fakeGrid, Config, MirrorBase) {
	t.Helper()
	g := newGrid()
	g.tabs["id-alpha!Альфа"] = mirrorTab()
	g.formulas["id-alpha!Альфа"] = map[[2]int]string{{rowOf(t, g.tabs["id-alpha!Альфа"], "2026-11-02", "Page A"), colD0 + 3}: "=10+5"}
	cfg := Config{Account: "a@example.com", Tables: []Table{{Model: "alpha", Sheet: "id-alpha"}}}
	m := func(s string) int64 { return int64(money.MustParse(s)) }
	base := MirrorBase{
		ReportsFrom: "2026-11-02", From: "2026-11-02",
		Tabs: []MirrorTab{
			{Model: "alpha", Name: "Альфа", Person: "alfa"},
			{Model: "alpha", Name: "Новенький", Skip: "вкладка не сопоставлена с человеком"},
			{Model: "gamma", Name: "Гамма", Person: "alfa"},
		},
		SheetPages: map[string]string{"Страница Икс": "page-x", "Общак": ""},
		Labels:     []string{"page-a", "page-b", "page-c"},
		Posts: []MirrorPost{
			{Person: "alfa", Model: "alpha", Date: "2026-10-27", Page: "page-a", Micros: m("41")},     // before the start
			{Person: "alfa", Model: "alpha", Date: "2026-11-02", Page: "page-a", Micros: m("100.40")}, // whole dollars → exact
			{Person: "alfa", Model: "alpha", Date: "2026-11-03", Page: "page-a", Micros: m("30")},     // already right
			{Person: "alfa", Model: "alpha", Date: "2026-11-03", Page: "page-b", Micros: 0},           // a zero page
			{Person: "alfa", Model: "alpha", Date: "2026-11-03", Page: "page-c", Micros: m("7")},      // no row
			{Person: "alfa", Model: "alpha", Date: "2026-11-04", Page: "page-x", Micros: m("20.5")},   // through sheet_pages
			{Person: "alfa", Model: "alpha", Date: "2026-11-05", Page: "page-a", Micros: m("20")},     // a formula
			{Person: "alfa", Model: "alpha", Date: "2026-11-09", Page: "page-a", Micros: m("12")},     // a paid week
			{Person: "alfa", Model: "alpha", Date: "2026-11-17", Page: "page-a", Micros: m("9")},      // no column
			{Person: "alfa", Model: "beta", Date: "2026-11-02", Page: "page-b", Micros: m("3")},       // another model
		},
	}
	return g, cfg, base
}

var mirrorNow = t0("2026-11-20T12:00:00+03:00")

func reasons(t TabMirror) map[string]string {
	out := map[string]string{}
	for _, c := range t.Skips {
		out[c.Date+" "+c.Page] = c.Reason
	}
	return out
}

func TestMirrorPlan(t *testing.T) {
	g, cfg, base := mirrorFixture(t)
	plan, err := PlanMirror(context.Background(), cfg, base, g, MirrorFilter{}, mirrorNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tabs) != 1 || plan.Tabs[0].Error != "" {
		t.Fatalf("tabs: %+v", plan.Tabs)
	}
	if len(plan.Left) != 2 || plan.Left[0].Error != "вкладка не сопоставлена с человеком" || plan.Left[1].Error != "таблицы модели нет в конфиге" {
		t.Fatalf("left: %+v", plan.Left)
	}
	tab := plan.Tabs[0]
	grid := g.tabs["id-alpha!Альфа"]
	rA, rB, rX := rowOf(t, grid, "2026-11-02", "Page A"), rowOf(t, grid, "2026-11-02", "Page B"), rowOf(t, grid, "2026-11-02", "Страница Икс")
	want := map[string]string{a1(rA, colD0): "100.40", a1(rB, colD0+1): "0.00", a1(rX, colD0+2): "20.50"}
	if len(tab.Writes) != len(want) {
		t.Fatalf("writes: %+v", tab.Writes)
	}
	for _, c := range tab.Writes {
		if want[c.Cell] != money.Micros(*c.Want).String() {
			t.Errorf("write %s %s %s = %s", c.Cell, c.Date, c.Page, money.Micros(*c.Want).String())
		}
	}
	if tab.Writes[0].Was != Cell(n(100)) || tab.Writes[2].Row != "Страница Икс" {
		t.Errorf("was / row: %+v", tab.Writes)
	}
	if tab.Correct != 1 {
		t.Errorf("correct %d", tab.Correct)
	}
	r := reasons(tab)
	for k, sub := range map[string]string{
		"2026-11-03 page-c": "страницы нет строкой",
		"2026-11-05 page-a": "формула",
		"2026-11-09 page-a": "неделя выплачена",
		"2026-11-17 page-a": "нет колонки",
		"2026-11-02 page-b": "в отчёте дня этой страницы нет", // 50 on Page B, no post of page-b that day: left alone
	} {
		if !strings.Contains(r[k], sub) {
			t.Errorf("%s: %q, want %q", k, r[k], sub)
		}
	}
	if len(tab.Skips) != 5 {
		t.Errorf("skips: %+v", tab.Skips)
	}
	for _, c := range tab.Writes {
		if c.Date < "2026-11-02" || c.Row == "Общак" {
			t.Errorf("touches %+v", c)
		}
	}

	// Filters.
	if p, _ := PlanMirror(context.Background(), cfg, base, g, MirrorFilter{Person: "bravo"}, mirrorNow); len(p.Tabs)+len(p.Left) != 0 {
		t.Errorf("person filter: %+v", p)
	}
	if p, _ := PlanMirror(context.Background(), cfg, base, g, MirrorFilter{Model: "Alpha"}, mirrorNow); len(p.Tabs) != 1 || len(p.Left) != 1 {
		t.Errorf("model filter: %+v", p)
	}
}

func TestMirrorRefusesABrokenTab(t *testing.T) {
	g, cfg, base := mirrorFixture(t)
	key := "id-alpha!Альфа"
	g.set(key, "A2", "сломано") // «К выплате» no longer a number: the grid does not parse
	plan, _ := PlanMirror(context.Background(), cfg, base, g, MirrorFilter{}, mirrorNow)
	if tab := plan.Tabs[0]; !strings.Contains(tab.Error, "не разобрана") || len(tab.Writes) != 0 {
		t.Fatalf("%+v", tab)
	}
	// Applying writes nothing for it, and says so.
	if err := ApplyMirror(context.Background(), &plan, g, filepath.Join(t.TempDir(), "j.jsonl"), false, mirrorNow); err != nil || g.writes != 0 {
		t.Fatalf("%v, writes %d", err, g.writes)
	}
}

func readJournal(t *testing.T, path string) []JournalEntry {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e JournalEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// The writer, then the reader over the same sheet: what the reader takes for those cells is what the reports posted.
func TestMirrorApplyThenRead(t *testing.T) {
	g, cfg, base := mirrorFixture(t)
	ctx := context.Background()
	journal := filepath.Join(t.TempDir(), "sheets-mirror.jsonl")
	plan, _ := PlanMirror(ctx, cfg, base, g, MirrorFilter{}, mirrorNow)
	before := copyGrid(g.tabs["id-alpha!Альфа"])
	if err := ApplyMirror(ctx, &plan, g, journal, false, mirrorNow); err != nil {
		t.Fatal(err)
	}
	if g.writes != 1 || g.cells != 3 {
		t.Fatalf("one batch of 3 cells, got %d batches %d cells", g.writes, g.cells)
	}
	tab := plan.Tabs[0]
	if len(tab.Written) != 3 || len(tab.Unwritten) != 0 || tab.Failed != "" {
		t.Fatalf("%+v", tab)
	}
	after := g.tabs["id-alpha!Альфа"]
	written := map[string]bool{}
	for _, c := range tab.Writes {
		written[c.Cell] = true
	}
	for i := range after {
		for j := range max(len(after[i]), len(before[i])) {
			formula := j == 9 || j == colZP || (i == 1 && j == 0) // the week's sums and «К выплате» follow the cells
			if !written[a1(i+1, j)] && !formula && at(after[i], j) != at(before[i], j) {
				t.Errorf("%s changed: %v → %v", a1(i+1, j), at(before[i], j), at(after[i], j))
			}
		}
	}
	if j := readJournal(t, journal); len(j) != 3 || j[0].Tab != "Альфа" || j[0].Sheet != "id-alpha" || j[0].Now != "100.40" || fmt.Sprint(j[0].Was) != "100" {
		t.Fatalf("journal: %+v", j)
	}

	// The reader: collect sheets over the same sheet.
	g.tabs["id-alpha!Итого"] = names("Альфа")
	g.tabs["id-alpha!Неактивные"] = names()
	got, err := Collect(ctx, cfg, func(ctx context.Context, sheet, rng string) ([][]Cell, error) { return g.Values(ctx, sheet, rng) }, mirrorNow)
	if err != nil || len(got.Errors) > 0 {
		t.Fatalf("%v %v", err, got.Errors)
	}
	var p TabPayload
	for _, e := range got.Events {
		if e.Kind == "tab" {
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolve := base.Resolver()
	read := map[string]money.Micros{}
	for _, b := range p.Parsed.Blocks {
		d0, _ := time.Parse("2006-01-02", b.Start)
		for _, r := range b.Pages {
			l, ok := resolve(r.Page)
			for k, v := range r.Values {
				if ok && v != nil {
					read[d0.AddDate(0, 0, k).Format("2006-01-02")+" "+l] = money.FromFloat(*v)
				}
			}
		}
	}
	for _, post := range base.Posts {
		k := post.Date + " " + post.Page
		if post.Model != "alpha" || post.Date < base.From || reasons(tab)[k] != "" {
			continue
		}
		if read[k] != money.Micros(post.Micros) {
			t.Errorf("%s: the reader takes %s, the report posted %s", k, read[k], money.Micros(post.Micros))
		}
	}

	// A second run has nothing to write.
	again, _ := PlanMirror(ctx, cfg, base, g, MirrorFilter{}, mirrorNow)
	if again.Cells() != 0 || again.Tabs[0].Correct != 4 {
		t.Fatalf("again: %d cells, correct %d", again.Cells(), again.Tabs[0].Correct)
	}
}

func TestMirrorGuardsManyCells(t *testing.T) {
	g := newGrid()
	plan := MirrorPlan{Tabs: []TabMirror{{Model: "alpha", Name: "Альфа", Sheet: "id-alpha"}}}
	v := int64(1)
	for i := range MirrorMaxCells + 1 {
		plan.Tabs[0].Writes = append(plan.Tabs[0].Writes, MirrorCell{Cell: a1(10+i, colD0), Want: &v, row: 10 + i, col: colD0})
	}
	err := ApplyMirror(context.Background(), &plan, g, filepath.Join(t.TempDir(), "j"), false, mirrorNow)
	if !errors.Is(err, ErrMirrorTooMany) || g.writes != 0 {
		t.Fatalf("%v, writes %d", err, g.writes)
	}
}

func TestMirrorFailedBatchStops(t *testing.T) {
	g, cfg, base := mirrorFixture(t)
	ctx := context.Background()
	// A second tab of another model, to see that the run stops after the failed one.
	g.tabs["id-beta!Альфа"] = mirrorTab()
	cfg.Tables = append(cfg.Tables, Table{Model: "beta", Sheet: "id-beta"})
	base.Tabs = append(base.Tabs, MirrorTab{Model: "beta", Name: "Альфа", Person: "alfa"})
	g.failOn["id-alpha"] = errors.New("gog rc=1: quota")
	g.partial = true
	journal := filepath.Join(t.TempDir(), "j.jsonl")
	plan, _ := PlanMirror(ctx, cfg, base, g, MirrorFilter{}, mirrorNow)
	err := ApplyMirror(ctx, &plan, g, journal, false, mirrorNow)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("%v", err)
	}
	tab := plan.Tabs[0]
	if len(tab.Written) != 1 || len(tab.Unwritten) != 2 || g.writes != 1 {
		t.Fatalf("written %v unwritten %v, batches %d", tab.Written, tab.Unwritten, g.writes)
	}
	if plan.Tabs[1].Written != nil {
		t.Errorf("the next tab was written after a failure")
	}
	if j := readJournal(t, journal); len(j) != 1 {
		t.Errorf("journal: %+v", j)
	}
}

// A number that comes back as text (a locale that parsed it) is an error naming the cells.
func TestMirrorReadBackCatchesText(t *testing.T) {
	g, cfg, base := mirrorFixture(t)
	g.asText = true
	plan, _ := PlanMirror(context.Background(), cfg, base, g, MirrorFilter{}, mirrorNow)
	err := ApplyMirror(context.Background(), &plan, g, filepath.Join(t.TempDir(), "j"), false, mirrorNow)
	if err == nil || !strings.Contains(err.Error(), plan.Tabs[0].Writes[0].Cell+"=100,40") {
		t.Fatalf("%v", err)
	}
}

// A cell of a page row holding text refuses the tab: ParseTab already does, so nothing of it is written.
func TestMirrorTextCellRefusesTheTab(t *testing.T) {
	g, cfg, base := mirrorFixture(t)
	key := "id-alpha!Альфа"
	r := rowOf(t, g.tabs[key], "2026-11-02", "Page B")
	g.set(key, a1(r, colD0+1), "н/д")
	plan, _ := PlanMirror(context.Background(), cfg, base, g, MirrorFilter{}, mirrorNow)
	if plan.Tabs[0].Error == "" || plan.Cells() != 0 {
		t.Fatalf("%+v", plan.Tabs[0])
	}
}

// The base from the database: the person's tab, the unmatched one, and the switch.
func TestMirrorBaseFromTheDatabase(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("alpha")
	f.set("alpha", []string{"Альфа", "Никто"}, map[string][][]Cell{
		"Альфа": sheet(0, 0),
		"Никто": sheet(0, 0),
	})
	f.run(t, s, ctx, "2026-11-10T12:00:00+03:00")
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES ('sheet_pages', '{"Страница Икс": "page-x"}', '2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	b, err := LoadMirrorBase(ctx, s.RO, "")
	if err != nil {
		t.Fatal(err)
	}
	if b.ReportsFrom != "" || b.From != "" || len(b.Posts) != 0 || b.SheetPages["Страница Икс"] != "page-x" {
		t.Fatalf("%+v", b)
	}
	skips := map[string]string{}
	for _, tb := range b.Tabs {
		skips[tb.Name] = tb.Skip
	}
	if skips["Альфа"] != "" || skips["Никто"] == "" {
		t.Fatalf("tabs: %+v", b.Tabs)
	}
	if b, err = LoadMirrorBase(ctx, s.RO, "2026-11-02"); err != nil || b.From != "2026-11-02" {
		t.Fatalf("%v %+v", err, b)
	}
	if n, err := mirrorLagCells(ctx, s.RO, "2026-11-02"); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}
