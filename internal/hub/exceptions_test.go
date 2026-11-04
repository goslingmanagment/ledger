package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
)

func TestParseExceptionsFile(t *testing.T) {
	ex, err := ReadExceptions("testdata/hub-exceptions.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Rows) != 3 || len(ex.Starts) != 1 || len(ex.Floors) != 1 {
		t.Fatalf("%d rows, %d starts, %d floors", len(ex.Rows), len(ex.Starts), len(ex.Floors))
	}
	r := ex.Rows[RowRef{"alfa-1", "5002"}]
	if r.Decision != "возврат" || r.Net != -5 || !r.At.Equal(time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", r)
	}
	if !ex.Starts["gamma-1"].Equal(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)) || ex.Floors["gamma-1"] != "2026-08-25" {
		t.Fatalf("%v %v", ex.Starts, ex.Floors)
	}
}

func TestParseExceptionsRefusesWithTheLine(t *testing.T) {
	const rows = "| Страница | transactionRef | Дата UTC | Net | Решение | Основание |\n|---|---|---|---|---|---|\n"
	const starts = "| Страница | Начало UTC | Основание |\n|---|---|---|\n"
	const floors = "| Страница | Заявки с первой | Основание |\n|---|---|---|\n"
	cases := []struct{ text, want string }{
		{"| Страница | Что-то |\n", "x.md:1: неизвестная таблица ['Страница', 'Что-то']"},
		{"| Страница | Начало UTC | Основание |\n| a-1 | 2026-09-03 10:00:00 | b |\n", "x.md:2: у шапки нет строки-разделителя"},
		{rows + "| alfa-1 | 1 | 2026-09-03 10:00:00 | 1.00 | приз |\n", "x.md:3: 5 ячеек вместо 6"},
		{rows + "| Alfa-1 | 1 | 2026-09-03 10:00:00 | 1.00 | приз | x |\n", "x.md:3: страница «Alfa-1»"},
		{rows + "| alfa | 1 | 2026-09-03 10:00:00 | 1.00 | приз | x |\n", "x.md:3: страница «alfa»"},
		{rows + "| alfa-1 | 5a | 2026-09-03 10:00:00 | 1.00 | приз | x |\n", "x.md:3: ref «5a» (цифры), net «1.00» (0.00), решение «приз» (из ['приз', 'исключить', 'возврат'])"},
		{rows + "| alfa-1 | 5 | 2026-09-03 10:00:00 | 1.0 | приз | x |\n", "net «1.0» (0.00)"},
		{rows + "| alfa-1 | 5 | 2026-09-03 10:00:00 | 1.00 | бонус | x |\n", "решение «бонус»"},
		{rows + "| alfa-1 | 5 | 2026-09-03T10:00:00 | 1.00 | приз | x |\n", "x.md:3: «2026-09-03T10:00:00» — нужно ГГГГ-ММ-ДД ЧЧ:ММ:СС (UTC)"},
		{rows + "| alfa-1 | 5 | 2026-02-30 10:00:00 | 1.00 | приз | x |\n", "«2026-02-30 10:00:00» — нужно"},
		{rows + "| alfa-1 | 5 | 2026-09-03 10:00:60 | 1.00 | приз | x |\n", "«2026-09-03 10:00:60» — нужно"},
		{rows + "| alfa-1 | 5 | 2026-09-03 10:00:00 | 1.00 | приз | x |\n| alfa-1 | 5 | 2026-09-04 10:00:00 | 2.00 | исключить | y |\n",
			"x.md:4: дубль ('alfa-1', '5')"},
		{starts + "| alfa-1 | 2026-09-03 10:00:00 | x |\n| alfa-1 | 2026-09-04 10:00:00 | y |\n", "x.md:4: дубль alfa-1"},
		{floors + "| alfa-1 | 2026-6-15 | x |\n", "x.md:3: дата «2026-6-15»"},
		{starts + "  | alfa-1 | 2026-09-03 10:00:00 | x |\n", "x.md:3: строка таблицы должна начинаться и кончаться «|» без отступа"},
		{starts + "| alfa-1 | 2026-09-03 10:00:00 | x |\nтекст без пустой строки\n", "x.md:4: строка таблицы должна"},
		{"\xff\n", "x.md:0: не читается"},
	}
	for _, c := range cases {
		_, err := ParseExceptions("x.md", []byte(c.text))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.text, err, c.want)
		}
	}
	// What Python's strptime accepts is accepted: one-digit fields, a run of spaces, CRLF lines.
	ok := starts + "| alfa-1 | 2026-9-3 1:02:3 | x |\r\n| alfa-2 | 2026-09-03  10:00:00 | y |\n\nтекст\n" + floors +
		"| alfa-1 | 2026-13-45 | дата не проверяется, как в month_files.py |\n"
	ex, err := ParseExceptions("x.md", []byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Starts["alfa-1"].Equal(time.Date(2026, 9, 3, 1, 2, 3, 0, time.UTC)) || len(ex.Starts) != 2 || ex.Floors["alfa-1"] != "2026-13-45" {
		t.Fatalf("%v %v", ex.Starts, ex.Floors)
	}
	if _, err := ReadExceptions(filepath.Join(t.TempDir(), "missing.md")); err == nil || !strings.Contains(err.Error(), "missing.md:0: не читается") {
		t.Fatalf("missing file: %v", err)
	}
}

func TestImportExceptionsReplacesItsSource(t *testing.T) {
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ex, err := ReadExceptions("testdata/hub-exceptions.md")
	if err != nil {
		t.Fatal(err)
	}
	now := mustTime("2026-11-04T06:00:00+03:00")
	if n, err := ImportExceptions(ctx, s.RW, ex, "hub-exceptions.md", now); err != nil || n != 5 {
		t.Fatalf("%d %v", n, err)
	}
	other, _ := ParseExceptions("b.md", []byte("| Страница | Начало UTC | Основание |\n|---|---|---|\n| delta-1 | 2026-09-01 00:00:00 | x |\n"))
	if _, err := ImportExceptions(ctx, s.RW, other, "b.md", now); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("testdata/hub-exceptions.md")
	fewer := strings.Replace(string(raw), "| alfa-1 | 5003 | 2026-09-05 10:00:00 | 1.00 | исключить | синтетическая ошибка платформы |\n", "", 1)
	ex2, err := ParseExceptions("hub-exceptions.md", []byte(fewer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportExceptions(ctx, s.RW, ex2, "hub-exceptions.md", now); err != nil {
		t.Fatal(err)
	}
	got, err := LoadExceptions(ctx, s.RW)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || len(got.Starts) != 2 || got.Rows[RowRef{"alfa-1", "5001"}].NetText != "50.00" {
		t.Fatalf("%d rows %d starts", len(got.Rows), len(got.Starts))
	}
	var at string
	s.RW.QueryRow(`SELECT at FROM hub_exceptions WHERE transaction_ref = '5001'`).Scan(&at)
	if at != "2026-09-03T13:00:00+03:00" {
		t.Fatalf("stored at %s", at)
	}
	// The same start from another file is a conflict, not a silent choice.
	dup, _ := ParseExceptions("c.md", []byte("| Страница | Начало UTC | Основание |\n|---|---|---|\n| gamma-1 | 2026-08-21 00:00:00 | x |\n"))
	if _, err := ImportExceptions(ctx, s.RW, dup, "c.md", now); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExceptions(ctx, s.RW); err == nil || !strings.Contains(err.Error(), "gamma-1") {
		t.Fatalf("two starts of a page: %v", err)
	}
}
