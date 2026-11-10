package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// All names below are made up: people Alfa and Bravo, models Alpha and Beta, pages page-1 and page-2.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func t0(s string) time.Time {
	v, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func openStore(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"accrual_from": "2026-09-01", "hold_days": "5", "hold_types": "tip,subscription",
		"tab_aliases": `{"Чарли": "Чарльз"}`} {
		if _, err := s.RW.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES (?, ?, '2026-01-01')`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	err = PutPeople(ctx, s.RW, []Person{
		{Slug: "alfa", Name: "Альфа", Status: "active", Schedule: "16:00-00:00", Workdays: []string{}, Models: []string{"alpha", "beta"}},
		{Slug: "bravo", Name: "Браво", Status: "active", Schedule: "00:00-08:00", Workdays: []string{"mon", "tue"}, Models: []string{"alpha"}},
		{Slug: "charlie", Name: "Чарли", Status: "departed", Models: []string{"alpha"}},
	}, "test", t0("2026-09-01"))
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func n(v any) json.Number { return json.Number(fmt.Sprint(v)) }

func serialOf(date string) int {
	d, err := time.Parse(ts.DateLayout, date)
	if err != nil {
		panic(err)
	}
	return int(d.Sub(epoch).Hours() / 24)
}

// week is a synthetic week block at 25% on one or more pages; a nil cell is left empty.
func week(start string, pages map[string][7]any) [][]Cell {
	s := serialOf(start)
	hdr := []Cell{"Работяга", "Страница"}
	for k := range 7 {
		hdr = append(hdr, n(s+k))
	}
	rows := [][]Cell{append(hdr, "Итого за неделю", "З/П", "Выплачено", "Фикс")}
	total := 0.0
	names := make([]string, 0, len(pages))
	for p := range pages {
		names = append(names, p)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, p := range names {
		row := []Cell{"Кто-то", p}
		rev := 0.0
		for _, c := range pages[p] {
			if c == nil {
				row = append(row, "")
				continue
			}
			row = append(row, n(c))
			var f float64
			fmt.Sscan(fmt.Sprint(c), &f)
			rev += f
		}
		row = append(row, n(rev), n(rev*0.25))
		total += rev * 0.25
		rows = append(rows, row)
	}
	sum := []Cell{"", "Сумма", n(0), n(0), n(0), n(0), n(0), n(0), n(0), n(0), n(total)}
	return append(rows, sum, []Cell{})
}

func sheet(kv, paid float64, weeks ...[][]Cell) [][]Cell {
	h := []Cell{"К выплате:", "", "Заработано:", "", "Выплачено:", "", "Отраб. дней:", "", "Средн./смена:", "", "Лучший день:", "", "Ставка %"}
	v := []Cell{n(kv), "", n(0), "", n(paid), "", n(0), "", n(0), "", n(0), "", n(0.25)}
	rows := [][]Cell{h, v, {}}
	for _, w := range weeks {
		rows = append(rows, w...)
	}
	return rows
}

func names(list ...string) [][]Cell {
	rows := [][]Cell{{"Чаттер"}}
	for _, x := range list {
		rows = append(rows, []Cell{x})
	}
	return append(rows, []Cell{"ИТОГО"})
}

// fakeSheets is what gog would read.
type fakeSheets struct {
	tabs map[string][][]Cell // "<sheet>!<tab>"
	fail map[string]error
	cfg  Config
}

func newSheets(models ...string) *fakeSheets {
	f := &fakeSheets{tabs: map[string][][]Cell{}, fail: map[string]error{}, cfg: Config{Account: "a@example.com"}}
	for _, m := range models {
		f.cfg.Tables = append(f.cfg.Tables, Table{Model: m, Sheet: "id-" + m})
		f.tabs["id-"+m+"!Неактивные"] = names()
	}
	return f
}

func (f *fakeSheets) set(model string, list []string, tabs map[string][][]Cell) {
	f.tabs["id-"+model+"!Итого"] = names(list...)
	for k, v := range tabs {
		f.tabs["id-"+model+"!"+k] = v
	}
}

func (f *fakeSheets) read(_ context.Context, sheet, rng string) ([][]Cell, error) {
	tab := strings.Trim(rng[:strings.Index(rng, "!")], "'")
	if err := f.fail[sheet+"!"+tab]; err != nil {
		return nil, err
	}
	return f.tabs[sheet+"!"+tab], nil
}

// run collects at the time and hands the events to the database, like the collector with --local-db.
func (f *fakeSheets) run(t *testing.T, s *db.Store, ctx context.Context, at string) events.Answer {
	t.Helper()
	got, err := Collect(ctx, f.cfg, f.read, t0(at))
	if err != nil {
		t.Fatal(err)
	}
	ans, err := events.Accept(ctx, s, got.Events, t0(at))
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Settle) > 0 {
		t.Fatalf("settle: %v", ans.Settle)
	}
	return ans
}

func bal(t *testing.T, s *db.Store, account string) money.Micros {
	t.Helper()
	v, err := ledger.Balance(context.Background(), s.RO, account, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func count(t *testing.T, s *db.Store, query string, args ...any) int {
	t.Helper()
	var v int
	if err := s.RO.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func journalHead() string {
	return "---\ntitle: Журнал\nupdated: 2026-09-30\n---\n\n# Журнал\n\n## Транзакции\n\n" +
		"| # | Дата | Тип | Категория | Кому/Откуда | Модель | Сумма | Валюта | Метод | Статус | Примечание |\n" +
		"|---|------|-----|-----------|-------------|--------|-------|--------|-------|--------|------------|\n"
}

func payout(num int, date, name, model, amount, note string) string {
	return fmt.Sprintf("| %d | %s | expense | Чаттер зарплата | %s | %s | %s | USDT | Internal Transfer (Bybit) | paid | %s |", num, date, name, model, amount, note)
}

func importJournal(t *testing.T, s *db.Store, ctx context.Context, at string, rows ...string) {
	t.Helper()
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	text := journalHead() + strings.Join(rows, "\n") + "\n"
	if _, err := journal.Import(ctx, tx, journal.FromFile(text, "file", t0(at)), journal.Options{AccrualFrom: "2026-09-01"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func rebuild(t *testing.T, s *db.Store, ctx context.Context, at string) Report {
	t.Helper()
	rep, err := Rebuild(ctx, s, t0(at))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestAccrualFollowsTheCell(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	w := func(second any) [][]Cell {
		return week("2026-09-07", map[string][7]any{"page-1": {100, second, nil, nil, nil, nil, nil}})
	}
	put := func(second any) {
		var rev float64
		fmt.Sscan(fmt.Sprint(second), &rev)
		f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet((100+rev)*0.25, 0, w(second))})
	}
	put(200)
	ans := f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	if ans.Inserted != 3 { // the table, the tab, the run
		t.Fatalf("inserted %d", ans.Inserted)
	}
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-75") {
		t.Fatalf("debt %s", got)
	}
	if got := bal(t, s, "expense:chatter_salary"); got != money.MustParse("75") {
		t.Fatalf("expense %s", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entries WHERE kind = ? AND recorded_at = '2026-09-14T10:00:00+03:00' AND effective_at IN ('2026-09-07','2026-09-08')`, KindAccrual); got != 2 {
		t.Fatalf("accruals at the event's time: %d", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entry_events`); got != 2 {
		t.Fatalf("entries not tied to their event: %d", got)
	}

	// A repeat changes nothing but the freshness: the tab's time, and the source's newest event, which the
	// checks of the collector's health read.
	ans = f.run(t, s, ctx, "2026-09-14T11:00:00+03:00")
	if ans.Inserted != 1 || ans.Repeated != 2 || count(t, s, `SELECT count(*) FROM entries`) != 2 {
		t.Fatalf("repeat: %+v, entries %d", ans, count(t, s, `SELECT count(*) FROM entries`))
	}
	if got := count(t, s, `SELECT count(*) FROM payroll_tabs WHERE fetched_at = '2026-09-14T11:00:00+03:00'`); got != 1 {
		t.Fatal("fetched_at not refreshed by the repeat")
	}
	if got := count(t, s, `SELECT count(*) FROM events WHERE source = 'sheets' AND received_at = '2026-09-14T11:00:00+03:00'`); got != 1 {
		t.Fatal("nothing shows that the collector ran")
	}

	// The cell changes: the day is reversed and posted anew, the other day stays, nothing is edited.
	var firstMemo string
	if err := s.RO.QueryRow(`SELECT memo FROM entries WHERE effective_at = '2026-09-08'`).Scan(&firstMemo); err != nil {
		t.Fatal(err)
	}
	put(240)
	f.run(t, s, ctx, "2026-09-15T10:00:00+03:00")
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-85") {
		t.Fatalf("debt after the change %s", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entries`); got != 4 {
		t.Fatalf("entries %d, want 2 + reversal + new", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entries WHERE reverses_id IS NOT NULL AND effective_at = '2026-09-08' AND recorded_at = '2026-09-15T10:00:00+03:00'`); got != 1 {
		t.Fatalf("reversal %d", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entries e JOIN lines l ON l.entry_id = e.id WHERE e.effective_at = '2026-09-08' AND e.memo = ? AND l.account = 'payable:alfa' AND l.amount = -50000000`, firstMemo); got != 1 {
		t.Fatal("the original entry is gone or edited")
	}

	// The cell changes back: the old event is the newest content again, and the entry needs a new key.
	put(200)
	f.run(t, s, ctx, "2026-09-16T10:00:00+03:00")
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-75") {
		t.Fatalf("debt after changing back %s", got)
	}
	if got := count(t, s, `SELECT count(*) FROM events WHERE kind = 'tab'`); got != 2 {
		t.Fatalf("tab events %d: the first content must not be stored twice", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entries WHERE recorded_at = '2026-09-16T10:00:00+03:00'`); got != 2 {
		t.Fatalf("second change posted %d entries", got)
	}

	// An old fetch that was stuck in a spool arrives late: history, not the latest state.
	stale, err := Collect(ctx, f.cfg, func(ctx context.Context, sheet, rng string) ([][]Cell, error) {
		if strings.Contains(rng, "Альфа") {
			return sheet2(100, 300), nil
		}
		return f.read(ctx, sheet, rng)
	}, t0("2026-09-15T12:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Accept(ctx, s, stale.Events, t0("2026-09-16T12:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-75") {
		t.Fatalf("a late old event moved the debt: %s", got)
	}
	if got := count(t, s, `SELECT count(*) FROM events WHERE kind = 'tab'`); got != 3 {
		t.Fatalf("the late event must stay in the history: %d", got)
	}
	var unbalanced int
	if err := s.RO.QueryRow(`SELECT count(*) FROM v_unbalanced`).Scan(&unbalanced); err != nil || unbalanced != 0 {
		t.Fatalf("unbalanced %d %v", unbalanced, err)
	}
}

func sheet2(first, second float64) [][]Cell {
	return sheet((first+second)*0.25, 0, week("2026-09-07", map[string][7]any{"page-1": {first, second, nil, nil, nil, nil, nil}}))
}

func TestRefusedTabKeepsLastGoodDays(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа", "Браво"}, map[string][][]Cell{"Альфа": sheet2(100, 200), "Браво": sheet2(40, 0)})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-75") {
		t.Fatalf("debt %s", got)
	}
	broken := sheet2(100, 200)
	broken[1][0] = "текст"
	f.tabs["id-Alpha!Альфа"] = broken
	f.fail["id-Alpha!Браво"] = failf("таблица id-Alpha 'Браво'!A1:N: gog rc=1: boom")
	got, err := Collect(ctx, f.cfg, f.read, t0("2026-09-15T10:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Summary(), "таблицы выплат:\n  Alpha/Альфа: A1") || !strings.Contains(got.Summary(), "\n  таблица id-Alpha 'Браво'") {
		t.Fatalf("errors of all tabs together: %q", got.Summary())
	}
	if _, err := events.Accept(ctx, s, got.Events, t0("2026-09-15T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-75") {
		t.Fatalf("a refused tab must leave the debt: %s", got)
	}
	var status, errText string
	var kv float64
	if err := s.RO.QueryRow(`SELECT status, error, kv FROM payroll_tabs WHERE name = 'Альфа'`).Scan(&status, &errText, &kv); err != nil {
		t.Fatal(err)
	}
	if status != "error" || !strings.Contains(errText, "A1 «К выплате:»") || kv != 75 {
		t.Fatalf("%s %s %v", status, errText, kv)
	}
	c, err := Check(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if c.OK || len(c.Tabs) != 2 {
		t.Fatalf("check %+v", c)
	}
	// The table itself cannot be read: everything stays, and the check says so.
	f.fail["id-Alpha!Итого"] = failf("таблица id-Alpha 'Итого'!A1:A40: gog rc=1: boom")
	f.run(t, s, ctx, "2026-09-16T10:00:00+03:00")
	var tableStatus string
	if err := s.RO.QueryRow(`SELECT status FROM payroll_tables WHERE model = 'Alpha'`).Scan(&tableStatus); err != nil || tableStatus != "error" {
		t.Fatalf("table %s %v", tableStatus, err)
	}
	if got := count(t, s, `SELECT count(*) FROM payroll_tabs WHERE listed = 0`); got != 0 {
		t.Fatal("an unread list must not unlist tabs")
	}
	// A tab dropped from the list is reported and keeps its accruals.
	delete(f.fail, "id-Alpha!Итого")
	f.set("Alpha", []string{"Браво"}, nil)
	delete(f.fail, "id-Alpha!Браво")
	f.run(t, s, ctx, "2026-09-17T10:00:00+03:00")
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-75") {
		t.Fatalf("an unlisted tab must keep its accruals: %s", got)
	}
	c, err = Check(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range c.Tabs {
		found = found || (x.Name == "Альфа" && strings.Contains(x.Reason, "нет в списках"))
	}
	if !found {
		t.Fatalf("unlisted tab not reported: %+v", c.Tabs)
	}
}

func TestPayoutWithAboveTableAndFIFOAcrossTabs(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha", "Beta")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(30, 75, week("2026-09-07", map[string][7]any{"page-1": {100, 200, nil, nil, 120, nil, nil}}))})
	f.set("Beta", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(30, 0, week("2026-09-07", map[string][7]any{"page-2": {nil, 40, nil, 80, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	// 2026-09-07: 25; 09-08: 50 + 10; 09-10: 20; 09-11: 30.
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-135") {
		t.Fatalf("accrued %s", got)
	}
	// A payout of 90 of the table plus 5 above it; the row after the accrual date is in suspense.
	importJournal(t, s, ctx, "2026-09-15T09:00:00+03:00",
		payout(1, "2026-09-15", "Альфа", "Alpha", "95.00", "по таблице {сверх таблицы: 5}"))
	rep := rebuild(t, s, ctx, "2026-09-15T10:00:00+03:00")
	if rep.Clearings != 1 {
		t.Fatalf("%+v", rep)
	}
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-45") {
		t.Fatalf("debt %s", got)
	}
	if got := bal(t, s, "suspense:accrual"); got != 0 {
		t.Fatalf("suspense %s", got)
	}
	if got := bal(t, s, "expense:chatter_salary"); got != money.MustParse("140") {
		t.Fatalf("expense %s: 135 accrued + 5 above the table", got)
	}
	if got := bal(t, s, "bybit"); got != money.MustParse("-95") {
		t.Fatalf("bybit %s", got)
	}
	var doc sql.NullInt64
	var recorded string
	if err := s.RO.QueryRow(`SELECT document_id, recorded_at FROM entries WHERE kind = ?`, KindClearing).Scan(&doc, &recorded); err != nil || !doc.Valid {
		t.Fatalf("clearing without a document: %v", err)
	}
	if recorded != "2026-09-15T09:00:00+03:00" {
		t.Fatalf("clearing recorded at %s, want the row's time", recorded)
	}
	// FIFO: 90 covers 09-07 (25), Alpha's 09-08 (50), Beta's 09-08 (10) and 5 of 09-10.
	type row struct {
		date, model          string
		amount, paid, unpaid int64
		by                   string
	}
	var got []row
	rows, err := s.RO.Query(`SELECT date, model, amount, paid, unpaid, paid_by FROM payable_days WHERE person = 'alfa' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.date, &r.model, &r.amount, &r.paid, &r.unpaid, &r.by); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	want := []row{
		{"2026-09-07", "Alpha", 25_000_000, 25_000_000, 0, "[1]"},
		{"2026-09-08", "Alpha", 50_000_000, 50_000_000, 0, "[1]"},
		{"2026-09-08", "Beta", 10_000_000, 10_000_000, 0, "[1]"},
		{"2026-09-10", "Beta", 20_000_000, 5_000_000, 15_000_000, "[1]"},
		{"2026-09-11", "Alpha", 30_000_000, 0, 30_000_000, "[]"},
	}
	if len(got) != len(want) {
		t.Fatalf("days %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("day %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	list, err := Payables(ctx, s.RO, "alfa")
	if err != nil || len(list) != 1 {
		t.Fatalf("payables %v %v", list, err)
	}
	p := list[0]
	if p.Balance != 45 || p.ToPay == nil || *p.ToPay != 60 || *p.Difference != -15 || p.OldestUnpaid != "2026-09-10" ||
		p.UnpaidDays != "2026-09-10 15.00, 2026-09-11 30.00" || p.TabsStatus != "ok" || p.Tabs != 2 {
		t.Fatalf("view %+v", p)
	}
	// An overpayment shows as an advance, and the row's new amount reposts the clearing only.
	importJournal(t, s, ctx, "2026-09-16T09:00:00+03:00",
		payout(1, "2026-09-15", "Альфа", "Alpha", "200.00", "по таблице {сверх таблицы: 5}"))
	rep = rebuild(t, s, ctx, "2026-09-16T10:00:00+03:00")
	if rep.Reversals != 1 || rep.Clearings != 1 || rep.Accruals != 0 {
		t.Fatalf("%+v", rep)
	}
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("60") {
		t.Fatalf("debt %s", got)
	}
	if got := count(t, s, `SELECT count(*) FROM payable_days WHERE person = 'alfa' AND kind = 'advance' AND unpaid = -60000000`); got != 1 {
		t.Fatal("no advance row")
	}
	var fifoSum, lineSum int64
	if err := s.RO.QueryRow(`SELECT SUM(unpaid) FROM payable_days WHERE person = 'alfa'`).Scan(&fifoSum); err != nil {
		t.Fatal(err)
	}
	if err := s.RO.QueryRow(`SELECT -SUM(amount) FROM lines WHERE account = 'payable:alfa'`).Scan(&lineSum); err != nil {
		t.Fatal(err)
	}
	if fifoSum != lineSum {
		t.Fatalf("FIFO %d, lines %d", fifoSum, lineSum)
	}
}

func TestUnresolvedPersonStaysInSuspense(t *testing.T) {
	s, ctx := openStore(t)
	importJournal(t, s, ctx, "2026-09-15T09:00:00+03:00",
		payout(1, "2026-09-15", "Незнакомец", "Alpha", "40.00", "выплата"),
		payout(2, "2026-08-20", "Незнакомец", "Alpha", "7.00", "до даты начисления: кассовый учёт"),
		payout(3, "2026-09-16", "Чарли", "—", "12.00", "через tab_aliases и без модели"))
	rep := rebuild(t, s, ctx, "2026-09-16T10:00:00+03:00")
	if rep.Clearings != 1 {
		t.Fatalf("%+v", rep)
	}
	if got := bal(t, s, "suspense:accrual"); got != money.MustParse("40") {
		t.Fatalf("suspense %s", got)
	}
	if got := bal(t, s, "payable:charlie"); got != money.MustParse("12") {
		t.Fatalf("charlie %s", got)
	}
	c, err := Check(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if c.OK || c.TabsRead != 0 || len(c.Rows) != 1 || c.Rows[0].Num != 1 || !strings.Contains(c.Rows[0].Reason, "Незнакомец") {
		t.Fatalf("check %+v", c)
	}
	// The person's card arrives: the row clears, as a correction of today.
	if err := PutPeople(ctx, s.RW, []Person{{Slug: "stranger", Name: "Незнакомец", Status: "active"}}, "test", t0("2026-09-20")); err != nil {
		t.Fatal(err)
	}
	rebuild(t, s, ctx, "2026-09-20T10:00:00+03:00")
	if got := bal(t, s, "suspense:accrual"); got != 0 {
		t.Fatalf("suspense %s", got)
	}
	if got := count(t, s, `SELECT count(*) FROM entries WHERE kind = ? AND effective_at = '2026-09-15' AND recorded_at = '2026-09-20T10:00:00+03:00'`, KindClearing); got != 1 {
		t.Fatal("a late mapping must be recorded now, not at the row's time")
	}
}

func TestOpeningIsOnceAndFrozen(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	// Alfa: К выплате 500; accrued 75 since the date, 40 paid since it; so 465 stayed from before the date.
	before := week("2026-08-24", map[string][7]any{"page-1": {4000, nil, nil, nil, nil, nil, nil}})
	f.set("Alpha", []string{"Альфа", "Браво", "Чарльз"}, map[string][][]Cell{
		"Альфа":  sheet(500, 575, before, week("2026-08-31", map[string][7]any{"page-1": {nil, 100, 200, nil, nil, nil, nil}})),
		"Браво":  sheet(0, 75, week("2026-09-07", map[string][7]any{"page-1": {100, 200, nil, nil, nil, nil, nil}})),
		"Чарльз": sheet(9000, -8975, week("2026-09-07", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	importJournal(t, s, ctx, "2026-09-15T09:00:00+03:00",
		payout(1, "2026-09-15", "Альфа", "Alpha", "40.00", "часть"),
		payout(2, "2026-09-16", "Браво", "Alpha", "75.00", "всё"),
		payout(3, "2026-09-16", "Браво", "Alpha", "10.00", "лишнее"))
	rebuild(t, s, ctx, "2026-09-16T10:00:00+03:00")
	plan, from, err := PlanOpenings(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if from != "2026-09-01" {
		t.Fatal(from)
	}
	by := map[string]Opening{}
	for _, o := range plan {
		by[o.Person] = o
	}
	if a := by["alfa"]; a.Amount != money.MustParse("465") || a.Accrued != money.MustParse("75") || a.Paid != money.MustParse("40") || len(a.Flags) != 0 {
		t.Fatalf("alfa %+v", a)
	}
	if b := by["bravo"]; b.Amount != money.MustParse("10") || len(b.Flags) != 1 || !strings.Contains(b.Flags[0], "больше всех дней") {
		t.Fatalf("bravo: paid more than earned since the date and nothing before it: %+v", b)
	}
	if c := by["charlie"]; c.Amount != money.MustParse("8975") || len(c.Flags) != 1 || !strings.Contains(c.Flags[0], "больше всех дней") {
		t.Fatalf("charlie: implausibly large: %+v", c)
	}
	post := func(at string) int {
		plan, from, err := PlanOpenings(ctx, s.RO)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.RW.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		n, err := PostOpenings(ctx, tx, plan, from, t0(at))
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := post("2026-09-16T11:00:00+03:00"); got != 3 {
		t.Fatalf("frozen %d", got)
	}
	var effective, key string
	if err := s.RO.QueryRow(`SELECT effective_at, source_key FROM entries WHERE kind = ? AND source_key LIKE '%alfa'`, KindOpening).Scan(&effective, &key); err != nil {
		t.Fatal(err)
	}
	if effective != "2026-08-31" || key != "opening:payroll:alfa" {
		t.Fatalf("%s %s", effective, key)
	}
	if got := bal(t, s, "opening"); got != money.MustParse("9450") {
		t.Fatalf("opening %s", got)
	}
	rebuild(t, s, ctx, "2026-09-16T12:00:00+03:00")
	if got := bal(t, s, "payable:alfa"); got != money.MustParse("-500") {
		t.Fatalf("debt %s", got)
	}
	// FIFO: the payout covered the opening first.
	var kind string
	var unpaid int64
	if err := s.RO.QueryRow(`SELECT kind, unpaid FROM payable_days WHERE person = 'alfa' ORDER BY seq LIMIT 1`).Scan(&kind, &unpaid); err != nil {
		t.Fatal(err)
	}
	if kind != "opening" || unpaid != 425_000_000 {
		t.Fatalf("%s %d", kind, unpaid)
	}
	// Again, with К выплате changed: nothing moves.
	f.tabs["id-Alpha!Альфа"][1][0] = n(700)
	f.run(t, s, ctx, "2026-09-17T10:00:00+03:00")
	entries := count(t, s, `SELECT count(*) FROM entries WHERE kind = ?`, KindOpening)
	if got := post("2026-09-17T11:00:00+03:00"); got != 0 {
		t.Fatalf("second run froze %d", got)
	}
	if count(t, s, `SELECT count(*) FROM entries WHERE kind = ?`, KindOpening) != entries {
		t.Fatal("second run posted")
	}
	if got := bal(t, s, "opening"); got != money.MustParse("9450") {
		t.Fatalf("opening moved: %s", got)
	}
	plan, _, err = PlanOpenings(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range plan {
		if o.Person == "alfa" && (!o.Frozen || o.Amount != money.MustParse("465")) {
			t.Fatalf("%+v", o)
		}
	}
	if _, err := s.RW.ExecContext(ctx, `UPDATE payroll_openings SET amount = 0`); err == nil {
		t.Fatal("frozen rows must not change")
	}
}

func TestOpeningFreezesZeroAndSkipsBrokenTabs(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	broken := sheet2(100, 200)
	broken[1][0] = "текст"
	f.set("Alpha", []string{"Альфа", "Браво"}, map[string][][]Cell{"Альфа": sheet2(100, 200), "Браво": broken})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	plan, from, err := PlanOpenings(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := PostOpenings(ctx, tx, plan, from, t0("2026-09-14T11:00:00+03:00"))
	if err != nil || got != 1 {
		t.Fatalf("%d %v", got, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM payroll_openings WHERE person = 'alfa' AND amount = 0 AND entry_id IS NULL`) != 1 {
		t.Fatal("a zero opening must be frozen too")
	}
	if count(t, s, `SELECT count(*) FROM payroll_openings WHERE person = 'bravo'`) != 0 {
		t.Fatal("a person whose tab is broken must not be frozen")
	}
}

// hubDay stores what the hub proved: transactions of a page and the window.
func hubDay(t *testing.T, s *db.Store, ctx context.Context, page, from, to string, tx ...[2]any) {
	t.Helper()
	for i, x := range tx {
		at := t0(x[0].(string))
		key := fmt.Sprintf("%s-%d", page, i)
		fields := fmt.Sprintf(`{"occurredAt":%q,"netMills":%d,"currency":"USD","transactionType":"tip","transactionRef":%q}`,
			at.UTC().Format(time.RFC3339), x[1], key)
		id, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "hub", SourceID: "t:" + key, Kind: "transactions", Payload: fields}, at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash, event_id, first_seen_at, updated_at)
			VALUES (?, ?, ?, ?, 'USD', 'tip', ?, ?, 'h', ?, ?, ?)`, page, key, ts.FormatMilli(at), x[1], key, fields, id, ts.Format(at), ts.Format(at)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_sync(page_label, dataset, status, window_from, window_to, checked_at, ok_at)
		VALUES (?, 'transactions', 'ok', ?, ?, ?, ?)`, page, from, to, to, to); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT OR IGNORE INTO routes(page_label, model, channel, rate, payee) VALUES (?, 'Alpha', 'Fansly', '0.95', 'model')`, page); err != nil {
		t.Fatal(err)
	}
}

func TestUnenteredShiftWindows(t *testing.T) {
	s, ctx := openStore(t)
	now := "2026-09-20T10:00:00+03:00"
	// Bravo works 00:00–08:00, mon and tue by the card: the cells of Tue 09-08 and Wed 09-09 (a night is dated
	// by the evening of entry); Thursday 09-10 is not his. The tab starts with an empty week: no shifts there.
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Браво", "Альфа"}, map[string][][]Cell{
		"Браво": sheet(25, 0, week("2026-08-31", map[string][7]any{"page-1": {nil, nil, nil, nil, nil, nil, nil}}),
			week("2026-09-07", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}, "page-2": {nil, nil, nil, nil, nil, nil, nil}})),
		"Альфа": sheet(25, 0, week("2026-09-14", map[string][7]any{"page-2": {100, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, now)
	hubDay(t, s, ctx, "page-1", "2026-08-01T00:00:00+03:00", now,
		[2]any{"2026-09-08T03:00:00+03:00", 80000}, // inside the night shift of the cell 09-08
		[2]any{"2026-09-08T09:00:00+03:00", 5000},  // after that shift ended: somebody else's
		[2]any{"2026-09-01T03:00:00+03:00", 9000},  // cell 09-01: before the first value of the tab
		[2]any{"2026-09-10T03:00:00+03:00", 7000})  // cell 09-10: no shift by the card
	rebuild(t, s, ctx, now)
	type row struct {
		person, date, page, status string
		net                        sql.NullInt64
		share                      sql.NullFloat64
	}
	read := func() map[string]row {
		rows, err := s.RO.Query(`SELECT person, date, page, status, net_mills, share FROM payroll_unentered`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.person, &r.date, &r.page, &r.status, &r.net, &r.share); err != nil {
				t.Fatal(err)
			}
			out[r.person+" "+r.date+" "+r.page] = r
		}
		return out
	}
	got := read()
	r, ok := got["bravo 2026-09-08 page-1"]
	if !ok || r.status != "ok" || r.net.Int64 != 80000 || r.share.Float64 != 20 {
		t.Fatalf("night cell: %+v of %+v", r, got)
	}
	for k := range got {
		if strings.HasPrefix(k, "bravo 2026-09-01") || strings.HasPrefix(k, "bravo 2026-09-10") {
			t.Fatalf("a day that is no shift counted: %s", k)
		}
	}
	if _, ok := got["bravo 2026-09-08 page-2"]; ok {
		t.Fatal("a row without money that week is not the person's page")
	}
	// Alfa's week names page-2 only, and the hub never proved it: «неизвестно», not zero.
	u, ok := got["alfa 2026-09-15 page-2"]
	if !ok || u.status != "unknown" || u.net.Valid {
		t.Fatalf("hub refused: %+v of %+v", u, got)
	}
	list, err := Payables(ctx, s.RO, "bravo")
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Balance != 25 || list[0].UnenteredDays != 1 || list[0].UnenteredNet != 80 || list[0].UnenteredShare != 20 {
		t.Fatalf("information only: %+v", list[0])
	}
	if a, err := Payables(ctx, s.RO, "Альфа"); err != nil || a[0].UnenteredUnknown == 0 || a[0].UnenteredDays != 0 || a[0].UnenteredShare != 0 {
		t.Fatalf("unknown must not read as zero: %+v %v", a, err)
	}
	if got := bal(t, s, "payable:bravo"); got != money.MustParse("-25") {
		t.Fatalf("the debt must not take the unentered: %s", got)
	}
	// The page changed hands: Delta on the same slot entered it, so Bravo's empty cell is no gap.
	if err := PutPeople(ctx, s.RW, []Person{{Slug: "delta", Name: "Дельта", Status: "active", Schedule: "00:00-08:00"}}, "test", t0("2026-09-01")); err != nil {
		t.Fatal(err)
	}
	f.set("Alpha", []string{"Браво", "Альфа", "Дельта"}, map[string][][]Cell{
		"Дельта": sheet(25, 0, week("2026-09-07", map[string][7]any{"page-1": {nil, 100, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, now)
	if _, ok := read()["bravo 2026-09-08 page-1"]; ok {
		t.Fatal("money on a colleague's tab must not read as unentered")
	}
}

func TestEventsThroughTheAPI(t *testing.T) {
	s, ctx := openStore(t)
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	key := func(name, scope string) string {
		token, _, err := auth.Create(ctx, s.RW, name, []string{scope}, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	collector := client.New(config.Client{URL: srv.URL, Key: key("mac", auth.ScopeCollect)})
	reader := client.New(config.Client{URL: srv.URL, Key: key("agent", auth.ScopeRead)})

	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet2(100, 200)})
	got, err := Collect(ctx, f.cfg, f.read, t0("2026-09-14T10:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	post := func(batch []events.Event) (events.Answer, error) {
		var ans events.Answer
		err := collector.Do(ctx, http.MethodPost, "/v1/events", map[string]any{"events": batch}, &ans)
		return ans, err
	}
	ans, err := post(got.Events)
	if err != nil || ans.Inserted != 3 || ans.Repeated != 0 || !ans.OK {
		t.Fatalf("%+v %v", ans, err)
	}
	if b := bal(t, s, "payable:alfa"); b != money.MustParse("-75") {
		t.Fatalf("arrival did not settle: %s", b)
	}
	ans, err = post(got.Events)
	if err != nil || ans.Inserted != 0 || ans.Repeated != 3 || !ans.OK || ans.Results[0].ID == 0 {
		t.Fatalf("repeat: %+v %v", ans, err)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE source = 'sheets'`); n != 3 {
		t.Fatalf("events %d", n)
	}
	// The same id with another payload is a conflict, reported, nothing written, the others go on.
	other := got.Events[0]
	other.Payload = json.RawMessage(`{"model":"Alpha","tabs":["Другой"]}`)
	var ans2 events.Answer
	if err := collector.Do(ctx, http.MethodPost, "/v1/events", map[string]any{"events": []events.Event{other}}, &ans2); err != nil {
		t.Fatal(err)
	}
	if ans2.OK || ans2.Results[0].Conflict == "" {
		t.Fatalf("%+v", ans2)
	}
	// One event as the whole body works too.
	single := events.Event{Source: SourceSheets, SourceID: "table:Beta:x", Kind: "table", OccurredAt: "2026-09-14T10:00:00+03:00", Payload: json.RawMessage(`{"model":"Beta","tabs":[]}`)}
	var ans3 events.Answer
	if err := collector.Do(ctx, http.MethodPost, "/v1/events", single, &ans3); err != nil || ans3.Inserted != 1 {
		t.Fatalf("%+v %v", ans3, err)
	}
	expect := func(what string, status int, err error) {
		t.Helper()
		e, ok := err.(*client.Error)
		if !ok || e.Status != status {
			t.Fatalf("%s: %v, want HTTP %d", what, err, status)
		}
	}
	_, err = post([]events.Event{{Source: "bybit", SourceID: "x", Kind: "deposit", Payload: json.RawMessage(`{}`)}})
	expect("unknown source", http.StatusBadRequest, err)
	if !strings.Contains(err.Error(), "неизвестный источник") {
		t.Fatal(err)
	}
	_, err = post([]events.Event{{Source: SourceSheets, SourceID: "x", Kind: "tab", OccurredAt: "вчера", Payload: json.RawMessage(`{}`)}})
	expect("bad time", http.StatusBadRequest, err)
	_, err = post([]events.Event{{Source: SourceSheets, SourceID: "x", Kind: "tab", Payload: json.RawMessage(`{"model":"Alpha"}`)}})
	expect("payload the source cannot read", http.StatusBadRequest, err)
	if n := count(t, s, `SELECT count(*) FROM events WHERE source_id = 'x'`); n != 0 {
		t.Fatal("a refused batch must leave nothing")
	}
	expect("read key cannot write", http.StatusForbidden, reader.Do(ctx, http.MethodPost, "/v1/events", map[string]any{"events": got.Events}, nil))
	nobody := client.New(config.Client{URL: srv.URL, Key: "ldg_nobody_x"})
	expect("no key", http.StatusUnauthorized, nobody.Do(ctx, http.MethodPost, "/v1/events", single, nil))

	var pay Answer
	if err := reader.Do(ctx, http.MethodGet, "/v1/payables?person=alfa", nil, &pay); err != nil {
		t.Fatal(err)
	}
	if len(pay.Payables) != 1 || pay.Payables[0].Balance != 75 || pay.Payables[0].Difference == nil || *pay.Payables[0].Difference != 0 {
		t.Fatalf("%+v", pay.Payables)
	}
	if len(pay.Check.People) == 0 {
		t.Fatalf("%+v", pay.Check)
	}
	expect("collector cannot read", http.StatusForbidden, collector.Do(ctx, http.MethodGet, "/v1/payables", nil, nil))
}

func TestSpoolResendsWithoutDuplicates(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet2(100, 200)})
	spool := Spool{Dir: filepath.Join(t.TempDir(), "spool")}
	collect := func(at string) {
		got, err := Collect(ctx, f.cfg, f.read, t0(at))
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.Put(got.Events); err != nil {
			t.Fatal(err)
		}
	}
	collect("2026-09-14T10:00:00+03:00")
	down := fmt.Errorf("API недоступен")
	rep, err := spool.Flush(ctx, func(context.Context, []events.Event) (events.Answer, error) { return events.Answer{}, down })
	if err != down || rep.Pending != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	// The next run reads the same content: the same files for the table and the tab, one more for the run.
	collect("2026-09-14T11:00:00+03:00")
	pending, err := spool.Pending()
	if err != nil || len(pending) != 4 || pending[0].Kind != "run" || pending[1].Kind != "table" || pending[2].Kind != "tab" {
		t.Fatalf("%d %v: the older run, then the newest in reading order", len(pending), err)
	}
	info, err := os.Stat(spool.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("spool dir %v %v", info.Mode(), err)
	}
	send := func(ctx context.Context, batch []events.Event) (events.Answer, error) {
		return events.Accept(ctx, s, batch, ts.Now())
	}
	rep, err = spool.Flush(ctx, send)
	if err != nil || rep.Inserted != 4 || rep.Pending != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if left, _ := spool.Pending(); len(left) != 0 {
		t.Fatalf("left %d", len(left))
	}
	// The API said yes but the answer was lost: the next run resends, and nothing doubles.
	collect("2026-09-14T12:00:00+03:00")
	rep, err = spool.Flush(ctx, send)
	if err != nil || rep.Inserted != 1 || rep.Repeated != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE source = 'sheets' AND kind <> 'run'`); n != 2 {
		t.Fatalf("events %d", n)
	}
	if b := bal(t, s, "payable:alfa"); b != money.MustParse("-75") {
		t.Fatalf("debt %s", b)
	}
	var fetched string
	if err := s.RO.QueryRow(`SELECT fetched_at FROM payroll_tabs`).Scan(&fetched); err != nil || fetched != "2026-09-14T12:00:00+03:00" {
		t.Fatalf("%s %v", fetched, err)
	}
}

func TestConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sheets.json")
	raw, err := os.ReadFile("testdata/sheets.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("readable by others: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Account != "someone@example.com" || len(cfg.Tables) != 2 || cfg.Tables[0] != (Table{"Alpha", "sheet-id-of-alpha"}) || cfg.Tables[1].Model != "Beta" {
		t.Fatalf("%+v", cfg)
	}
}

func TestDecodeGogOutput(t *testing.T) {
	rows, err := decodeValues("s", "r", []byte(`{"range":"x","values":[["К выплате:",""],[552.3560000000001,12,true,null]]}`))
	if err != nil {
		t.Fatal(err)
	}
	if rows[1][0].(json.Number).String() != "552.3560000000001" || rows[1][2] != true || rows[1][3] != nil {
		t.Fatalf("%v", rows)
	}
	if rows, err := decodeValues("s", "r", []byte(`{"range":"x"}`)); err != nil || len(rows) != 0 {
		t.Fatalf("an empty range: %v %v", rows, err)
	}
	if _, err := decodeValues("s", "r", []byte(`{"values":"нет"}`)); err == nil || !strings.Contains(err.Error(), "gog вернул не таблицу") {
		t.Fatal(err)
	}
}

func TestFrontmatterAndResolver(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("petr.md", "---\ntitle: Пётр\ntype: person\nposition: Чаттер\nstatus: departed\naliases: []\nschedule: null\nworkdays: [mon, tue]\nrate: \"25%, потом 30%\"\nmodels: [alpha]\naccount: page-1, page-2 (Alpha)\nupdated: 2026-08-05\n---\n\nтекст")
	write("petr-2.md", "---\ntitle: Пётр (petr-2)\ntype: person\nstatus: active\naliases: [petr-2, \"@petr, второй\"]\nschedule: \"08:00-16:00\"\nmodels:\n  - beta\n# заметка\nnested:\n  key: value\naccount: \"page-3; с 2026-10-27\" # комментарий\n---\n")
	write("petya.md", "---\ntitle: Петя\nstatus: active\nmodels: [beta]\n---\n")
	write("plan.md", "---\ntitle: Заметка\ntype: context\n---\n")
	write("no-front.md", "просто текст\n")
	people, skipped, err := ReadPeople(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(skipped, ",") != "no-front,plan" || len(people) != 3 {
		t.Fatalf("%v %v", people, skipped)
	}
	by := map[string]Person{}
	for _, p := range people {
		by[p.Slug] = p
	}
	i2 := by["petr-2"]
	if i2.Schedule != "08:00-16:00" || strings.Join(i2.Aliases, "|") != "petr-2|@petr, второй" || strings.Join(i2.Models, ",") != "beta" || i2.Pages != "page-3; с 2026-10-27" {
		t.Fatalf("%+v", i2)
	}
	if v := by["petr"]; v.Schedule != "" || v.Rate != "25%, потом 30%" || strings.Join(v.Workdays, ",") != "mon,tue" || v.Pages != "page-1, page-2 (Alpha)" {
		t.Fatalf("%+v", v)
	}
	if _, _, err := ReadPeople(t.TempDir()); err == nil {
		t.Fatal("an empty folder is an error")
	}
	write("Bad Slug.md", "---\ntitle: X\n---\n")
	if _, _, err := ReadPeople(dir); err == nil {
		t.Fatal("a bad slug must stop the import")
	}
	os.Remove(filepath.Join(dir, "Bad Slug.md"))

	s, ctx := openStore(t)
	if err := PutPeople(ctx, s.RW, people, "test", t0("2026-09-01")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `UPDATE settings SET value = '{"Петя": "Пётр Иванов"}' WHERE key = 'tab_aliases'`); err != nil {
		t.Fatal(err)
	}
	res, err := NewResolver(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ model, name, want string }{
		{"Alpha", "Пётр", "petr"},
		{"Beta", "Пётр", "petr-2"}, // one first name, two people: the model decides
		{"Alpha", "petr-2", "petr-2"},
		{"Beta", "Пётр Иванов", "petya"}, // the tab of tab_aliases
		{"Beta", "Петя", "petya"},
		{"—", "Петя", "petya"},
		{"Gamma", "Петя", "petya"}, // one candidate: the model does not matter
	} {
		if got, err := res.Resolve(c.model, c.name); err != nil || got != c.want {
			t.Errorf("%s/%s = %q %v, want %s", c.model, c.name, got, err, c.want)
		}
	}
	for _, c := range []struct{ model, name, msg string }{
		{"—", "Пётр", "подходит нескольким: petr, petr-2"}, // no model, no way to tell
		{"Gamma", "Пётр", "подходит нескольким"},
		{"Alpha", "Сидор", "такого человека нет"},
	} {
		if got, err := res.Resolve(c.model, c.name); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s/%s = %q %v, want %s", c.model, c.name, got, err, c.msg)
		}
	}
}

func TestSlotWindows(t *testing.T) {
	d, _ := time.ParseInLocation(ts.DateLayout, "2026-08-20", ts.MSK)
	night, _ := parseSlot("00:00-08:00")
	from, to := night.window(d)
	if from.UTC().Format(time.RFC3339) != "2026-08-19T21:00:00Z" || to.UTC().Format(time.RFC3339) != "2026-08-20T05:00:00Z" {
		t.Fatalf("%v %v", from, to)
	}
	late, _ := parseSlot("18:00-02:00")
	from, to = late.window(d)
	if from.UTC().Format(time.RFC3339) != "2026-08-20T15:00:00Z" || to.UTC().Format(time.RFC3339) != "2026-08-20T23:00:00Z" {
		t.Fatalf("%v %v", from, to)
	}
	evening, _ := parseSlot("16:00-00:00")
	from, to = evening.window(d)
	if from.UTC().Format(time.RFC3339) != "2026-08-20T13:00:00Z" || to.UTC().Format(time.RFC3339) != "2026-08-20T21:00:00Z" {
		t.Fatalf("%v %v", from, to)
	}
	if _, ok := parseSlot("по договорённости"); ok {
		t.Fatal("a free-form slot must not parse")
	}
	// 2026-08-20 is a Thursday. A Wednesday night shift is the Thursday cell; a late evening shift is its own date.
	if !night.scheduled([]string{"wed"}, d) || night.scheduled([]string{"thu"}, d) {
		t.Fatal("night slot dating")
	}
	if !late.scheduled([]string{"thu"}, d) || late.scheduled([]string{"wed"}, d) {
		t.Fatal("evening slot dating")
	}
	if !late.scheduled(nil, d) {
		t.Fatal("no workdays means every day")
	}
}

// Two cards on the same slot of a page, nobody entered it: the money is not counted for both. Without Hub's team
// facts each cell is «неизвестно»; with a proven week it is theirs whose shift Hub saw on the page.
func TestUnenteredSharedSlot(t *testing.T) {
	s, ctx := openStore(t)
	now := "2026-09-20T10:00:00+03:00"
	if err := PutPeople(ctx, s.RW, []Person{
		{Slug: "echo", Name: "Эхо", Status: "active", Schedule: "00:00-08:00", Workdays: []string{"mon", "tue"}, Models: []string{"alpha"}},
	}, "test", t0("2026-09-01")); err != nil {
		t.Fatal(err)
	}
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Браво", "Эхо"}, map[string][][]Cell{
		"Браво": sheet(25, 0, week("2026-09-07", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}})),
		"Эхо":   sheet(25, 0, week("2026-09-07", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, now)
	hubDay(t, s, ctx, "page-1", "2026-08-01T00:00:00+03:00", now, [2]any{"2026-09-08T03:00:00+03:00", 80000})
	rebuild(t, s, ctx, now)
	read := func() map[string][2]any {
		rows, err := s.RO.Query(`SELECT person, status, share FROM payroll_unentered WHERE date = '2026-09-08' AND page = 'page-1'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][2]any{}
		for rows.Next() {
			var person, status string
			var share sql.NullFloat64
			if err := rows.Scan(&person, &status, &share); err != nil {
				t.Fatal(err)
			}
			out[person] = [2]any{status, share}
		}
		return out
	}
	got := read()
	if len(got) != 2 || got["bravo"][0] != "unknown" || got["echo"][0] != "unknown" || got["bravo"][1].(sql.NullFloat64).Valid {
		t.Fatalf("without Hub's facts both cells are unknown, no money: %+v", got)
	}
	// Hub's team facts prove the week; only Эхо (Hub user 7, a login equal to the card's code) worked the page.
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_team_sync(window_from, window_to, status, reasons, works, sales, net_mills, read_at, ok_at)
		VALUES ('2026-09-07T00:00:00+03:00', '2026-09-14T00:00:00+03:00', 'ok', '[]', 1, 0, 0, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	payload := `{"ref":"7:2026-09-07T21:00:00.000Z","userId":7,"username":"Echo","role":"chatter","kind":"shift",` +
		`"startedAt":"2026-09-08T00:02:00+03:00","endedAt":"2026-09-08T07:58:00+03:00","ongoing":false,"workedMinutes":476,` +
		`"pages":[{"pageLabel":"page-1","messages":10,"salesFrom":null}]}`
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_works(window_from, ref, user_id, username, role, kind, started_at, ended_at, ongoing,
		worked_minutes, day, payload, read_at) VALUES ('2026-09-07T00:00:00+03:00', '7:2026-09-07T21:00:00.000Z', 7, 'Echo', 'chatter', 'shift',
		'2026-09-08T00:02:00.000+03:00', '2026-09-08T07:58:00.000+03:00', 0, 476, '2026-09-08', ?, ?)`, payload, now); err != nil {
		t.Fatal(err)
	}
	rebuild(t, s, ctx, now)
	got = read()
	if _, ok := got["bravo"]; ok || got["echo"][0] != "ok" || got["echo"][1].(sql.NullFloat64).Float64 != 20 {
		t.Fatalf("with a proven week the money is the worker's only: %+v", got)
	}
}

// teamWork stores a work of Hub's team facts on page-1 in the week window it began in.
func teamWork(t *testing.T, s *db.Store, ctx context.Context, user int64, username, kind, from, to string) {
	t.Helper()
	start, end := t0(from), t0(to)
	win := ts.Format(ts.WeekStart(start))
	ref := fmt.Sprintf("%d:%s", user, start.UTC().Format("2006-01-02T15:04:05.000Z"))
	payload := fmt.Sprintf(`{"ref":%q,"userId":%d,"username":%q,"role":"chatter","kind":%q,"startedAt":%q,"endedAt":%q,`+
		`"ongoing":false,"workedMinutes":%d,"pages":[{"pageLabel":"page-1","messages":5,"salesFrom":%q}]}`,
		ref, user, username, kind, from, to, int(end.Sub(start).Minutes()), from)
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_works(window_from, ref, user_id, username, role, kind, started_at, ended_at, ongoing,
		worked_minutes, day, payload, read_at) VALUES (?, ?, ?, ?, 'chatter', ?, ?, ?, 0, ?, ?, ?, ?)`,
		win, ref, user, username, kind, ts.FormatMilli(start), ts.FormatMilli(end), int(end.Sub(start).Minutes()),
		ts.Date(start), payload, ts.Format(end)); err != nil {
		t.Fatal(err)
	}
}

// In a week Hub's team facts proved, a mapped person's empty day is their own accrued sales of the day: a day off is
// no gap, a neighbour's sale in the slot is not theirs. An unproven week and an unmapped person go by the slot.
func TestUnenteredFromFacts(t *testing.T) {
	s, ctx := openStore(t)
	now := "2026-11-15T10:00:00+03:00"
	// Bravo (00:00–08:00, mon and tue: the cells of Tue and Wed) and Alfa (16:00–00:00, every day) on page-1.
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Браво", "Альфа"}, map[string][][]Cell{
		"Браво": sheet(45, 0, week("2026-11-02", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}}),
			week("2026-11-09", map[string][7]any{"page-1": {80, nil, nil, nil, nil, nil, nil}})),
		"Альфа": sheet(25, 0, week("2026-11-09", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, now)
	hubDay(t, s, ctx, "page-1", "2026-10-01T00:00:00+03:00", now,
		[2]any{"2026-11-03T03:00:00+03:00", 60000}, // Bravo's slot, unproven week
		[2]any{"2026-11-10T02:00:00+03:00", 30000}, // Bravo's slot: his own sale
		[2]any{"2026-11-10T05:00:00+03:00", 50000}, // Bravo's slot: a neighbour's sale
		[2]any{"2026-11-11T03:00:00+03:00", 40000}, // Bravo's slot on his day off
		[2]any{"2026-11-10T20:00:00+03:00", 20000}) // Alfa's slot
	for _, q := range []string{
		`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-1', 'fansly', 'Alpha', 'Alpha', '2026-11-15')`,
		`INSERT INTO hub_team_sync(window_from, window_to, status, reasons, read_at, ok_at) VALUES
			('2026-11-02T00:00:00+03:00', '2026-11-09T00:00:00+03:00', 'unknown', '["отказ"]', '2026-11-15', NULL),
			('2026-11-09T00:00:00+03:00', '2026-11-15T10:00:00+03:00', 'ok', '[]', '2026-11-15', '2026-11-15')`,
		`INSERT INTO fact_weeks(week, status, computed_at) VALUES ('2026-11-02', 'unknown', '2026-11-15'), ('2026-11-09', 'ok', '2026-11-15')`,
		// Hub user 5 «Bravo» is the card bravo by its code; user 9 «Delta» has no card.
		`INSERT INTO fact_accruals(hub_user_id, username, person, model, page_label, day, week, kind, sales, net_micros) VALUES
			(5, 'Bravo', 'bravo', 'Alpha', 'page-1', '2026-11-03', '2026-11-02', 'sales', 1, 10000000),
			(5, 'Bravo', 'bravo', 'Alpha', 'page-1', '2026-11-10', '2026-11-09', 'sales', 1, 30000000),
			(9, 'Delta', NULL, 'Alpha', 'page-1', '2026-11-10', '2026-11-09', 'sales', 1, 50000000),
			(5, 'Bravo', 'bravo', 'Alpha', 'page-1', '2026-11-12', '2026-11-09', 'sales', 1, 15000000)`,
	} {
		if _, err := s.RW.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	teamWork(t, s, ctx, 5, "Bravo", "shift", "2026-11-10T00:01:00+03:00", "2026-11-10T07:59:00+03:00")
	teamWork(t, s, ctx, 9, "Delta", "visit", "2026-11-10T04:30:00+03:00", "2026-11-10T05:30:00+03:00")
	teamWork(t, s, ctx, 5, "Bravo", "shift", "2026-11-12T00:00:00+03:00", "2026-11-12T08:00:00+03:00") // off the card's days
	rebuild(t, s, ctx, now)

	type row struct {
		status, reason string
		net            int64
		share          float64
	}
	got := map[string]row{}
	rows, err := s.RO.Query(`SELECT person, date, page, status, COALESCE(reason, ''), COALESCE(net_mills, 0), COALESCE(share, 0)
		FROM payroll_unentered`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var person, date, page string
		var r row
		if err := rows.Scan(&person, &date, &page, &r.status, &r.reason, &r.net, &r.share); err != nil {
			t.Fatal(err)
		}
		got[person+" "+date+" "+page] = r
	}
	rows.Close()
	want := map[string]row{
		"bravo 2026-11-03 page-1": {"ok", reasonSlot, 60000, 15},   // unproven week: the slot, not the facts' 10.00
		"bravo 2026-11-10 page-1": {"ok", reasonFacts, 30000, 7.5}, // his own sale; the neighbour's 50.00 is not his
		"bravo 2026-11-12 page-1": {"ok", reasonFacts, 15000, 3.75},
		"alfa 2026-11-10 page-1":  {"ok", reasonSlot, 20000, 5}, // no Hub user is Alfa: the slot
	}
	if len(got) != len(want) {
		t.Fatalf("rows %+v, want %+v (the day off 11.11 is no gap)", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	list, err := Payables(ctx, s.RO, "bravo")
	if err != nil {
		t.Fatal(err)
	}
	if list[0].UnenteredDays != 3 || list[0].UnenteredShare != 26.25 || list[0].UnenteredUnknown != 0 {
		t.Fatalf("v_payables %+v", list[0])
	}
}
