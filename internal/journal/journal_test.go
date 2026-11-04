package journal

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const head = "---\ntitle: Журнал\nupdated: 2026-09-30\n---\n\n# Журнал\n\n## Транзакции\n\n" +
	"| # | Дата | Тип | Категория | Кому/Откуда | Модель | Сумма | Валюта | Метод | Статус | Примечание |\n" +
	"|---|------|-----|-----------|-------------|--------|-------|--------|-------|--------|------------|\n"

func row(cells ...string) string { return "| " + strings.Join(cells, " | ") + " |" }

// A synthetic journal: start balance, cash-era transfer, a deal in two payments, a card purchase,
// a prepayment, a fee for another month, an internal transfer to a card, a model transfer and a
// chatter payout after the accrual date, a withheld purchase and a cancelled row; text blocks and a
// blank line inside the table, like the real file has.
func sample() string {
	return head + strings.Join([]string{
		row("1", "2026-07-30", "income", "Owner вливание", "Владелец", "—", "1000.00", "USDT", "Internal Transfer (Bybit)", "paid", "старт"),
		row("2", "2026-08-01", "transfer", "Остаток на старте", "CinCin •1111", "—", "20.00", "USD", "—", "paid", "остаток карты"),
		row("3", "2026-08-10", "income", "Платформы", "Модель А", "Alfa", "700.00", "USDT", "Internal Transfer (Bybit)", "paid", "перевод {перевод: срез 2026-08-10 12:00; депозиты: 1000}"),
		row("4", "2026-08-12", "income", "Custom", "Фан", "Alfa", "250.00", "USDT", "USDT TRC-20", "paid", "аванс {сделка: d1; цена 450; доля 135}"),
		"",
		row("5", "2026-08-20", "income", "Custom", "Фан", "Alfa", "200.00", "USDT", "USDT TRC-20", "paid", "остаток {сделка: d1; цена 450; доля 135}"),
		row("6", "2026-08-21", "expense", "Инструменты", "Сервис", "—", "12.00", "USD", "Карта CinCin 1111", "paid", "покупка"),
		row("7", "2026-07-23", "expense", "Инструменты", "Прокси", "—", "90.00", "USDT", "USDT TRC-20", "paid", "на 3 месяца {предоплата 2026-07..2026-09}"),
		row("8", "2026-09-05", "expense", "Комиссии", "Bybit", "—", "3.00", "USDT", "USDT TRC-20", "paid", "сеть за август {за 2026-08: 3}"),
		row("9", "2026-09-06", "transfer", "Внутренний перевод", "Bybit → CinCin (кошелёк)", "—", "97.00", "USDT", "USDT TRC-20", "paid", "пополнение"),
		row("10", "2026-09-08", "income", "Платформы", "Модель А", "Alfa", "1400.00", "USDT", "Internal Transfer (Bybit)", "paid", "перевод {перевод: срез 2026-09-08 10:00; депозиты: 2000}"),
		row("11", "2026-09-09", "expense", "Чаттер зарплата", "Чаттер", "Alfa", "105.00", "USDT", "Internal Transfer (Bybit)", "paid", "по таблице 100 {сверх таблицы: 5}"),
		row("12", "2026-09-09", "expense", "Прочее", "Модель А", "Alfa", "40.00", "USDT", "Удержано из вывода", "paid", "покупка из вывода #10"),
		row("13", "2026-09-10", "expense", "Комиссии", "Карта", "—", "1.00", "USD", "Карта CinCin 1111", "cancelled", "аннулирована"),
	}, "\n") + "\n\n## Блок\n\nтекст после таблицы\n"
}

func openDB(t *testing.T) (*db.Store, context.Context) {
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
	return s, ctx
}

func runImport(t *testing.T, s *db.Store, ctx context.Context, b Bundle, o Options) (Report, error) {
	t.Helper()
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Import(ctx, tx, b, o)
	if err != nil {
		tx.Rollback()
		return rep, err
	}
	return rep, tx.Commit()
}

func bal(t *testing.T, s *db.Store, acct, before string) money.Micros {
	t.Helper()
	v, err := ledger.Balance(context.Background(), s.RO, acct, before, "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRoundTripAndBalances(t *testing.T) {
	s, ctx := openDB(t)
	text := sample()
	at, _ := ts.Parse("2026-09-30T12:00:00+03:00")
	opts := Options{AccrualFrom: "2026-09-01", Align: []Align{{Account: "bybit", Date: "2026-08-10", UpTo: 3, Amount: money.MustParse("1700.00")}}}
	if _, err := runImport(t, s, ctx, FromFile(text, "file", at), opts); err != nil {
		t.Fatal(err)
	}
	out, err := Export(ctx, s.RO)
	if err != nil || out != text {
		t.Fatalf("export differs (%v):\n%s", err, out)
	}
	// Bybit is aligned to 1700 right after #3 (the July prepayment #7 is inside the snapshot), then
	// + 250 + 200 − 3 − 97 + 1400 − 105 − 40 = 3305.
	if got := bal(t, s, "bybit", ""); got != money.MustParse("3305.00") {
		t.Fatalf("bybit %s", got)
	}
	if got := bal(t, s, "card:1111", ""); got != money.MustParse("8.00") {
		t.Fatalf("card %s", got)
	}
	if got := bal(t, s, "cincin_wallet", ""); got != money.MustParse("97.00") {
		t.Fatalf("wallet %s", got)
	}
	// The deal's share goes to the model once, in its first payment by date.
	if got := bal(t, s, "payable:alfa", ""); got != money.MustParse("-135.00") {
		t.Fatalf("model share %s", got)
	}
	if got := bal(t, s, "income:custom", ""); got != money.MustParse("-315.00") {
		t.Fatalf("custom income %s", got)
	}
	// After the accrual date, transfers and table payouts wait for accruals; the withheld purchase
	// leaves Bybit and is an expense; the part above the table is a direct expense.
	if got := bal(t, s, "suspense:accrual", ""); got != money.MustParse("-1300.00") {
		t.Fatalf("suspense %s (want −1400 transfer + 100 payout)", got)
	}
	if got := bal(t, s, "expense:chatter_salary", ""); got != money.MustParse("5.00") {
		t.Fatalf("above the table %s", got)
	}
	// Prepayment spread evenly over three months; a fee moved into the month it belongs to.
	m := map[string]string{"2026-07": "30.00", "2026-08": "30.00", "2026-09": "30.00"}
	for month, want := range m {
		m0, m1, _ := ts.MonthStart(month)
		tot, _ := ledger.Totals(ctx, s.RO, ledger.Cut{From: ts.Date(m0), To: ts.Date(m1)})
		if tot["expense:tools"] != money.MustParse(want)+map[string]money.Micros{"2026-08": money.MustParse("12.00")}[month] {
			t.Fatalf("%s tools %s", month, tot["expense:tools"])
		}
	}
	aug, _ := ledger.Totals(ctx, s.RO, ledger.Cut{From: "2026-08-01", To: "2026-09-01"})
	if aug["expense:fees"] != money.MustParse("3.00") || aug["clearing:period"] != money.MustParse("-3.00") {
		t.Fatalf("fee for August: %v", aug)
	}
	var unbalanced int
	s.RO.QueryRow(`SELECT count(*) FROM v_unbalanced`).Scan(&unbalanced)
	if unbalanced != 0 {
		t.Fatal("unbalanced entries")
	}
}

func TestHistoryVersionsAndAtClose(t *testing.T) {
	s, ctx := openDB(t)
	v1 := strings.Replace(sample(), "| 6 | 2026-08-21 | expense | Инструменты | Сервис | — | 12.00 |", "| 6 | 2026-08-21 | expense | Инструменты | Сервис | — | 15.00 |", 1)
	b := Bundle{Versions: []Version{
		delta(v1, map[int]string{}, "c1", "2026-09-01T10:00:00+03:00"),
	}}
	prev := map[int]string{}
	delta(v1, prev, "", "")
	b.Versions = append(b.Versions, delta(sample(), prev, "c2", "2026-10-20T10:00:00+03:00"))
	b.Final, b.FinalRef, b.FinalAt = sample(), "c2", "2026-10-20T10:00:00+03:00"
	opts := Options{AccrualFrom: "2026-09-01"}
	rep, err := runImport(t, s, ctx, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NewVersions != 1 || rep.Reposted != 1 {
		t.Fatalf("report %+v", rep)
	}
	if err := ledger.Lock(ctx, s.RW, ledger.Period{Month: "2026-08", LockedAt: "2026-10-14T19:00:00+03:00",
		AsOf: "2026-10-14T19:00:00+03:00", Snapshot: []byte(`{}`), Source: "test"}); err != nil {
		t.Fatal(err)
	}
	now, atClose, err := ledger.MonthTotals(ctx, s.RO, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if now["expense:tools"] != money.MustParse("42.00") || atClose["expense:tools"] != money.MustParse("45.00") {
		t.Fatalf("now %s at close %s", now["expense:tools"], atClose["expense:tools"])
	}
	old, _ := ledger.DocumentsAsOf(ctx, s.RO, "2026-10-14T19:00:00+03:00")
	for _, d := range old {
		if d.Num == 6 && d.Amount != "15.00" {
			t.Fatalf("row as of close %s", d.Amount)
		}
	}
	again, err := runImport(t, s, ctx, b, opts)
	if err != nil || again.NewVersions != 0 || again.Entries != 0 || again.Skipped != 2 {
		t.Fatalf("re-import %+v %v", again, err)
	}
}

func TestUnknownValuesStopImport(t *testing.T) {
	s, ctx := openDB(t)
	bad := strings.Replace(sample(), "Карта CinCin 1111 | paid | покупка", "Карта CinCin 1111/2222 | paid | покупка", 1)
	bad = strings.Replace(bad, "| Прочее | Модель А |", "| Неведомое | Модель А |", 1)
	_, err := runImport(t, s, ctx, FromFile(bad, "file", time.Now()), Options{AccrualFrom: "2026-09-01"})
	pe, ok := err.(ProblemsError)
	if !ok || len(pe.Problems) != 2 {
		t.Fatalf("want two problems, got %v", err)
	}
	var n int
	if err := s.RO.QueryRow(`SELECT count(*) FROM documents`).Scan(&n); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
}

func TestTagsAreStrict(t *testing.T) {
	ok := []string{"{перевод: срез 2026-10-12 14:44; депозиты: 1+2.5 + 3; удержано: 40}", "{сделка: x1; цена 500; доля 150}",
		"{сверх таблицы: 0.15; период 2026-09}", "{предоплата 2026-09..2026-11}", "{за 2026-08: 3} {за 2026-07: 1}", "{доля 0}"}
	for _, n := range ok {
		if _, err := ParseTags(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	bad := []string{"{перевод: депозиты: 1}", "{сделка: x; цена 100; доля 150}", "{предоплата 2026-09..2026-09}",
		"{доля 0} {сделка: x; цена 1; доля 0}", "{внутри доли} {сверх доли}", "{незнакомый}", "{за 2026-08: 1} {за 2026-08: 2}",
		"текст {не закрыт", "{перевод: срез 2026-10-12 14:44; срез 2026-10-12 14:45}"}
	for _, n := range bad {
		if _, err := ParseTags(n); err == nil {
			t.Errorf("%s accepted", n)
		}
	}
	r := Row{Num: 9, Type: "expense", Category: "Чаттер зарплата", Amount: "10.00", Note: "бонус сверх таблицы"}
	if _, err := RowTags(r); err == nil {
		t.Error("plain-text «сверх таблицы» without the tag accepted")
	}
	r.Note = "{перевод: срез 2026-10-12 14:44}"
	if _, err := RowTags(r); err == nil {
		t.Error("transfer tag on an expense accepted")
	}
}

// Alignments of one account: a repeat changes nothing, a new target moves the balance to it, and a later
// alignment starts from the earlier one.
func TestAlignmentsAddUpToTheirTargets(t *testing.T) {
	s, ctx := openDB(t)
	at, _ := ts.Parse("2026-09-30T12:00:00+03:00")
	b := FromFile(sample(), "file", at)
	base := Options{AccrualFrom: "2026-09-01", Models: []string{"Alfa"}}
	if _, err := runImport(t, s, ctx, b, base); err != nil {
		t.Fatal(err)
	}
	day := "2026-08-10"
	target := func(amount string) Align { return Align{Account: "bybit", Date: day, Amount: money.MustParse(amount)} }
	through := func() money.Micros { return bal(t, s, "bybit", "2026-08-11") }
	for _, amount := range []string{"1000.00", "1100.00", "1100.00", "1100.00", "900.00"} {
		o := base
		o.Align = []Align{target(amount)}
		if _, err := runImport(t, s, ctx, b, o); err != nil {
			t.Fatal(err)
		}
		if got := through(); got != money.MustParse(amount) {
			t.Fatalf("after aligning to %s the balance is %s", amount, got)
		}
	}
	later := base
	later.Align = []Align{{Account: "bybit", Date: "2026-12-31", Amount: money.MustParse("777.00")}}
	if _, err := runImport(t, s, ctx, b, later); err != nil {
		t.Fatal(err)
	}
	if got := bal(t, s, "bybit", "2027-01-01"); got != money.MustParse("777.00") {
		t.Fatalf("a later alignment left %s", got)
	}
}

// The model's share sits on the first paid row of a deal: a row dated earlier moves it, without doubling.
func TestDealShareMovesToTheFirstPaidRow(t *testing.T) {
	s, ctx := openDB(t)
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('accrual_from', '2026-09-01', 'x'), ('models', 'Alfa', 'x')`); err != nil {
		t.Fatal(err)
	}
	w := Write{Author: "t", Origin: "test", Now: time.Date(2026, 10, 1, 12, 0, 0, 0, ts.MSK)}
	deal := func(date, amount, status string) Row {
		return Row{Date: date, Type: "income", Category: "Custom", Counterparty: "Фан", Model: "Alfa", Amount: amount, Currency: "USDT",
			Method: "USDT TRC-20", Status: status, Note: "{сделка: d1; цена 100; доля 60}"}
	}
	due := func() money.Micros { return -bal(t, s, "payable:alfa", "2030-01-01") }
	if _, err := Append(ctx, s.RW, []Row{deal("2026-09-20", "100.00", "paid")}, w); err != nil {
		t.Fatal(err)
	}
	if got := due(); got != money.MustParse("60.00") {
		t.Fatalf("share after the first row: %s", got)
	}
	if _, err := Append(ctx, s.RW, []Row{deal("2026-09-10", "100.00", "paid")}, w); err != nil {
		t.Fatal(err)
	}
	if got := due(); got != money.MustParse("60.00") {
		t.Fatalf("share after an earlier row: %s", got)
	}
	var first string
	if err := s.RW.QueryRow(`SELECT e.effective_at FROM entries e JOIN lines l ON l.entry_id = e.id WHERE l.account = 'payable:alfa'
		AND e.reverses_id IS NULL AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first != "2026-09-10" {
		t.Fatalf("the share stayed on %s", first)
	}
	// A row that is not paid is no first row.
	if _, err := Append(ctx, s.RW, []Row{deal("2026-09-01", "10.00", "pending")}, w); err != nil {
		t.Fatal(err)
	}
	if got := due(); got != money.MustParse("60.00") {
		t.Fatalf("share after a pending row: %s", got)
	}
}
