package bybit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
)

func movement(kind, id, direction, cpKind, cp, amount, fee, when string) Movement {
	t := at(when)
	return Movement{Key: kind + ":" + id, Kind: kind, Direction: direction, TimeMs: t.UnixMilli(), At: when, Date: when[:10], Coin: "USDT",
		Amount: amount, Fee: fee, CounterpartyKind: cpKind, Counterparty: cp, Method: "USDT TRC-20"}
}

func addRule(t *testing.T, s *db.Store, kind, column, value, direction, name, category, model string) {
	t.Helper()
	if _, err := s.RW.Exec(`INSERT INTO rules(kind, `+column+`, direction, name, category, model, note, created_at, source)
		VALUES (?, ?, ?, ?, ?, ?, 'заметка', 'now', 'test')`, kind, value, direction, name, category, model); err != nil {
		t.Fatal(err)
	}
}

func addEvent(t *testing.T, s *db.Store, ctx context.Context, m Movement) int64 {
	t.Helper()
	id, _, err := ledger.AddEvent(ctx, s.RW, m.Event(), at(m.At))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func proposalOf(t *testing.T, s *db.Store, key string) (block, kind, summary string, c Content) {
	t.Helper()
	var raw string
	if err := s.RW.QueryRow(`SELECT block, kind, summary, content FROM proposals WHERE key = ? AND status = 'open'`, key).Scan(&block, &kind, &summary, &raw); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return
}

func classifyAll(t *testing.T, s *db.Store, ctx context.Context, mode string) ClassifyReport {
	t.Helper()
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rep, err := ClassifyEvents(ctx, tx, mode, at("2026-08-01"), at("2026-09-30"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return rep
}

func journalRow(t *testing.T, s *db.Store, ctx context.Context, num int, date, typ, category, counterparty, amount, note string) {
	t.Helper()
	d := ledger.Document{Num: num, Date: date, Type: typ, Category: category, Counterparty: counterparty, Model: "—", Amount: amount,
		Currency: "USDT", Method: "USDT TRC-20", Status: "paid", Note: note, Author: "test", Origin: "test"}
	if _, _, err := ledger.PutDocument(ctx, s.RW, d, at(date)); err != nil {
		t.Fatal(err)
	}
}

func TestExactRuleGivesAutoWithDocumentsAndFee(t *testing.T) {
	s, ctx := openDB(t)
	addRule(t, s, "counterparty", "match_address", "TtoKnown", "expense", "Подрядчик А", "Подрядчик", "Модель-1")
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "address", "TtoKnown", "100", "1.5", "2026-09-03T10:00:00.000+03:00"))
	internal := movement("withdrawal", "2", "expense", "uid", "2002", "48.75", "0", "2026-09-04T10:00:00.000+03:00")
	internal.Method = "Internal Transfer (Bybit)"
	addRule(t, s, "counterparty", "match_uid", "2002", "expense", "Чаттер Б", "Чаттер зарплата", "")
	addEvent(t, s, ctx, internal)
	addRule(t, s, "counterparty", "match_address", "Tcards", "expense", "Карты", "Внутренний перевод", "—")
	addEvent(t, s, ctx, movement("withdrawal", "3", "expense", "address", "Tcards", "200", "", "2026-09-05T10:00:00.000+03:00"))
	addRule(t, s, "counterparty", "match_prefix", "Trot", "income", "Плательщик", "Custom", "Модель-2")
	if _, err := s.RW.Exec(`UPDATE rules SET match_suffix = 'end' WHERE match_prefix = 'Trot'`); err != nil {
		t.Fatal(err)
	}
	addEvent(t, s, ctx, movement("deposit", "4", "income", "address", "Trot123end", "30", "", "2026-09-06T10:00:00.000+03:00"))

	if rep := classifyAll(t, s, ctx, "shadow"); rep.Auto != 4 || rep.Unknown != 0 {
		t.Fatalf("report %+v", rep)
	}
	block, kind, summary, c := proposalOf(t, s, "withdrawal:1")
	if block != "auto" || kind != KindMovement || c.Mode != "shadow" || c.Rule == nil || c.Rule.Name != "Подрядчик А" {
		t.Fatalf("proposal %s %s %+v", block, kind, c)
	}
	if summary != "заплатил 100 USDT — Подрядчик А (Подрядчик) (03.09 10:00)" {
		t.Fatalf("summary %q", summary)
	}
	if len(c.Documents) != 2 {
		t.Fatalf("documents %+v", c.Documents)
	}
	main, fee := c.Documents[0], c.Documents[1]
	if main != (SuggestedDocument{Date: "2026-09-03", Type: "expense", Category: "Подрядчик", Counterparty: "Подрядчик А", Model: "Модель-1",
		Amount: "100.00", Currency: "USDT", Method: "USDT TRC-20", Status: "paid", Note: "withdrawal:1"}) {
		t.Fatalf("main %+v", main)
	}
	if fee.Category != "Комиссии" || fee.Amount != "1.50" || fee.Counterparty != "Подрядчик А" || fee.Type != "expense" || fee.Model != "Модель-1" {
		t.Fatalf("fee %+v", fee)
	}
	if _, _, _, c = proposalOf(t, s, "withdrawal:2"); len(c.Documents) != 1 || c.Documents[0].Amount != "48.75" ||
		c.Documents[0].Method != "Internal Transfer (Bybit)" || c.Documents[0].Model != "—" {
		t.Fatalf("internal withdrawal, no fee row: %+v", c.Documents)
	}
	if _, _, _, c = proposalOf(t, s, "withdrawal:3"); c.Documents[0].Type != "transfer" {
		t.Fatalf("own transfer: %+v", c.Documents[0])
	}
	if _, _, _, c = proposalOf(t, s, "deposit:4"); c.Documents[0].Type != "income" || len(c.Documents) != 1 || c.Documents[0].Model != "Модель-2" {
		t.Fatalf("income: %+v", c.Documents)
	}
	// Shadow: the journal canon is elsewhere, ledger writes no documents.
	if n := count(t, s, `SELECT count(*) FROM documents`); n != 0 {
		t.Fatalf("%d documents written in shadow mode", n)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE status = 'auto'`); n != 0 {
		t.Fatalf("%d proposals marked as posted", n)
	}
}

func TestDirectionHintUnknownAndEnrichment(t *testing.T) {
	s, ctx := openDB(t)
	addRule(t, s, "counterparty", "match_uid", "3003", "expense", "Чаттер В", "Чаттер зарплата", "Модель-1")
	journalRow(t, s, ctx, 1, "2026-08-10", "expense", "Инструменты", "Сервис Г", "64.00", "старая оплата")
	journalRow(t, s, ctx, 2, "2026-08-11", "expense", "Инструменты", "Сервис Г", "64.00", "ещё одна")
	journalRow(t, s, ctx, 3, "2026-08-12", "expense", "Подрядчик", "Подрядчик Д", "64", "другой")
	journalRow(t, s, ctx, 4, "2026-08-13", "income", "Custom", "Фан", "64", "другое направление")

	addEvent(t, s, ctx, movement("internal_deposit", "1", "income", "uid", "3003", "10", "", "2026-09-03T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "2", "expense", "address", "Tunknown", "41", "1", "2026-08-14T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "3", "expense", "address", "Tunknown", "64", "1", "2026-09-04T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "4", "expense", "address", "Tother", "12.34", "1", "2026-09-05T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "5", "expense", "address", "Tunknown", "1", "1", "2026-07-30T10:00:00.000+03:00"))

	if rep := classifyAll(t, s, ctx, "shadow"); rep.Likely != 1 || rep.Unknown != 3 {
		t.Fatalf("report %+v", rep)
	}
	block, _, _, c := proposalOf(t, s, "internal_deposit:1")
	if block != "likely" || c.Hint != "идентификатор совпадает с «Чаттер В», но направление income — проверить вручную" || len(c.Documents) != 0 {
		t.Fatalf("hint: %s %+v", block, c)
	}
	block, kind, _, c := proposalOf(t, s, "withdrawal:3")
	if block != "unknown" || kind != KindMovement || c.AmountPattern != "круглая" {
		t.Fatalf("unknown: %s %+v", block, c)
	}
	if len(c.PriorSightings) != 2 || c.PriorSightings[0].Key != "withdrawal:2" || c.PriorSightings[0].Amount != "41" || c.PriorSightings[1].Key != "withdrawal:5" {
		t.Fatalf("sightings %+v", c.PriorSightings)
	}
	if len(c.SimilarDocuments) != 2 || c.SimilarDocuments[0] != (Similar{Name: "Сервис Г", N: 2}) || c.SimilarDocuments[1] != (Similar{Name: "Подрядчик Д", N: 1}) {
		t.Fatalf("similar %+v", c.SimilarDocuments)
	}
	if _, _, _, c = proposalOf(t, s, "withdrawal:4"); c.AmountPattern != "копейки" || len(c.PriorSightings) != 0 {
		t.Fatalf("tail: %+v", c)
	}
	// Before the start of collection: no proposal, it stays a plain event.
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE key = 'withdrawal:5'`); n != 0 {
		t.Fatal("a movement before the start date was proposed")
	}
	// A repeat finds nothing to do.
	if rep := classifyAll(t, s, ctx, "shadow"); rep != (ClassifyReport{}) {
		t.Fatalf("repeat %+v", rep)
	}
}

func TestP2PAndNonUSDTHaveTheirOwnKinds(t *testing.T) {
	s, ctx := openDB(t)
	journalRow(t, s, ctx, 10, "2026-09-02", "expense", "Owner зарплата", "Владелец", "1000.00", "обычный транш")
	journalRow(t, s, ctx, 11, "2026-09-20", "expense", "Owner зарплата", "Владелец", "500.00", "часть вперёд {за 2026-10: 200}")
	journalRow(t, s, ctx, 12, "2026-08-28", "expense", "Owner зарплата", "Владелец", "300.00", "аванс {за 2026-09: 300}")
	journalRow(t, s, ctx, 13, "2026-09-03", "expense", "Owner draw", "Владелец", "999.00", "не зарплата")

	p := movement("p2p", "1", "expense", "p2p", "buyer", "123.4561", "", "2026-09-05T10:00:00.000+03:00")
	p.Method, p.Fiat, p.Price = "P2P (Bybit)", "12300 RUB", "99.63"
	addEvent(t, s, ctx, p)
	btc := movement("deposit", "2", "income", "address", "Tbtc", "0.01", "", "2026-09-06T10:00:00.000+03:00")
	btc.Coin, btc.Method = "BTC", "BTC BTC"
	addEvent(t, s, ctx, btc)
	// A rule on this identity does not make another coin automatic.
	addRule(t, s, "counterparty", "match_address", "Tbtc", "income", "Плательщик", "Custom", "—")

	classifyAll(t, s, ctx, "shadow")
	block, kind, summary, c := proposalOf(t, s, "p2p:1")
	if kind != KindP2P || block != "unknown" || c.OwnerSalaryRecorded == nil || c.Rule != nil {
		t.Fatalf("p2p %s %s %+v", block, kind, c)
	}
	// 1000 + (500 − 200) + 300 moved here from August.
	if *c.OwnerSalaryRecorded != (SalaryContext{Month: "2026-09", Amount: "1600"}) {
		t.Fatalf("salary context %+v", *c.OwnerSalaryRecorded)
	}
	if summary != "продал 123.4561 USDT за 12300 RUB по 99.63 (05.09 10:00)" {
		t.Fatalf("summary %q", summary)
	}
	block, kind, _, c = proposalOf(t, s, "deposit:2")
	if kind != KindNonUSDT || block != "unknown" || c.Rule != nil || len(c.Documents) != 0 {
		t.Fatalf("non-USDT %s %s %+v", block, kind, c)
	}
}

func TestIgnoreRuleLinksWithoutProposal(t *testing.T) {
	s, ctx := openDB(t)
	addRule(t, s, "ignore", "match_uid", "9009", "any", "личное", "", "")
	id := addEvent(t, s, ctx, movement("internal_deposit", "1", "income", "uid", "9009", "10", "", "2026-09-03T10:00:00.000+03:00"))
	if rep := classifyAll(t, s, ctx, "shadow"); rep.Ignored != 1 || rep.Unknown != 0 {
		t.Fatalf("report %+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals`); n != 0 {
		t.Fatal("an ignored movement was proposed")
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND how = 'rule' AND decision = 'ignore' AND document_num IS NULL`, id); n != 1 {
		t.Fatal("no ignore link")
	}
	if n := count(t, s, `SELECT count(*) FROM v_bybit_unlinked`); n != 0 {
		t.Fatal("ignored movement listed as unlinked")
	}
	if rep := classifyAll(t, s, ctx, "shadow"); rep != (ClassifyReport{}) {
		t.Fatalf("repeat %+v", rep)
	}
}

func TestRuleValidityAndOrder(t *testing.T) {
	s, ctx := openDB(t)
	addRule(t, s, "counterparty", "match_uid", "4004", "expense", "Первый", "Подрядчик", "—")
	addRule(t, s, "counterparty", "match_uid", "4004", "any", "Второй", "Инструменты", "—")
	addRule(t, s, "counterparty", "match_uid", "5005", "expense", "Просрочен", "Подрядчик", "—")
	if _, err := s.RW.Exec(`UPDATE rules SET valid_to = '2026-09-01' WHERE match_uid = '5005'`); err != nil {
		t.Fatal(err)
	}
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "uid", "4004", "1", "", "2026-09-03T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("internal_deposit", "2", "income", "uid", "4004", "2", "", "2026-09-03T11:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "3", "expense", "uid", "5005", "3", "", "2026-09-03T12:00:00.000+03:00"))
	classifyAll(t, s, ctx, "shadow")
	if _, _, _, c := proposalOf(t, s, "withdrawal:1"); c.Rule.Name != "Первый" {
		t.Fatalf("first rule must win: %+v", c.Rule)
	}
	if _, _, _, c := proposalOf(t, s, "internal_deposit:2"); c.Rule.Name != "Второй" {
		t.Fatalf("direction any: %+v", c.Rule)
	}
	if block, _, _, _ := proposalOf(t, s, "withdrawal:3"); block != "unknown" {
		t.Fatalf("an expired rule still works: %s", block)
	}
}

func TestLiveHookIsCalledOnlyInLiveMode(t *testing.T) {
	s, ctx := openDB(t)
	addRule(t, s, "counterparty", "match_uid", "6006", "expense", "Известный", "Подрядчик", "—")
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "uid", "6006", "1", "", "2026-09-03T10:00:00.000+03:00"))
	calls := 0
	LiveHook = func(context.Context, ledger.Querier, int64, Content, time.Time) error {
		calls++
		return nil
	}
	defer func() { LiveHook = nil }()
	classifyAll(t, s, ctx, "shadow")
	if calls != 0 {
		t.Fatal("hook called in shadow")
	}
	addEvent(t, s, ctx, movement("withdrawal", "2", "expense", "uid", "6006", "2", "", "2026-09-03T11:00:00.000+03:00"))
	classifyAll(t, s, ctx, "live")
	if calls != 1 {
		t.Fatalf("hook calls %d", calls)
	}
}

func TestAmountPattern(t *testing.T) {
	for amount, want := range map[string]string{"5": "круглая", "140": "круглая", "72.50": "круглая", "72.5": "круглая",
		"12.34": "копейки", "0.57": "копейки", "1.05": "копейки", "100.00": "круглая"} {
		if got := amountPattern(amount); got != want {
			t.Fatalf("%s: %s", amount, got)
		}
	}
}
