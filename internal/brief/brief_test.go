package brief

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// All data in these tests is synthetic.
var day = at("2026-09-10T12:00:00+03:00")

func at(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

const journalHead = "---\ntitle: Журнал\n---\n\n## Транзакции\n\n" +
	"| # | Дата | Тип | Категория | Кому/Откуда | Модель | Сумма | Валюта | Метод | Статус | Примечание |\n" +
	"|---|------|-----|-----------|-------------|--------|-------|--------|-------|--------|------------|\n"

func journalRow(cells ...string) string { return "| " + strings.Join(cells, " | ") + " |" }

// open makes a database with a small journal in the given mode.
func open(t *testing.T, mode string) (*db.Store, context.Context) {
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
	for k, v := range map[string]string{"accrual_from": "2026-09-01", "models": "Alfa,Beta", "mode": mode} {
		if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES (?, ?, 'now')`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	text := journalHead + strings.Join([]string{
		journalRow("1", "2026-08-01", "income", "Owner вливание", "Владелец", "—", "1000.00", "USDT", "Internal Transfer (Bybit)", "paid", "старт"),
		journalRow("2", "2026-08-15", "expense", "Owner draw", "Владелец", "—", "100.00", "USDT", "P2P (Bybit)", "paid", "изъятие"),
		journalRow("3", "2026-09-02", "expense", "Инструменты", "Сервис А", "—", "30.00", "USDT", "USDT TRC-20", "paid", "подписка"),
	}, "\n") + "\n"
	tx := begin(t, s)
	if _, err := journal.Import(ctx, tx, journal.FromFile(text, "file", at("2026-09-03T10:00:00+03:00")),
		journal.Options{AccrualFrom: "2026-09-01", Models: []string{"Alfa", "Beta"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func begin(t *testing.T, s *db.Store) *sql.Tx {
	t.Helper()
	tx, err := s.RW.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

func count(t *testing.T, s *db.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.RW.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(query, err)
	}
	return n
}

func addRule(t *testing.T, s *db.Store, address, direction, name, category string) {
	t.Helper()
	if _, err := s.RW.Exec(`INSERT INTO rules(kind, match_address, direction, name, category, model, note, created_at, source)
		VALUES ('counterparty', ?, ?, ?, ?, '—', '', 'now', 'test')`, address, direction, name, category); err != nil {
		t.Fatal(err)
	}
}

// movement stores a Bybit movement and classifies it, in the mode of the database.
func movement(t *testing.T, s *db.Store, ctx context.Context, kind, id, direction, address, amount, fee, when string) int64 {
	t.Helper()
	w := at(when)
	m := bybit.Movement{Key: kind + ":" + id, Kind: kind, Direction: direction, TimeMs: w.UnixMilli(), At: ts.FormatMilli(w), Date: ts.Date(w), Coin: "USDT",
		Amount: amount, Fee: fee, CounterpartyKind: "address", Counterparty: address, Method: "USDT TRC-20"}
	eid, _, err := ledger.AddEvent(ctx, s.RW, m.Event(), w)
	if err != nil {
		t.Fatal(err)
	}
	classify(t, s, ctx)
	return eid
}

func classify(t *testing.T, s *db.Store, ctx context.Context) {
	t.Helper()
	tx := begin(t, s)
	mode, err := bybit.Mode(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bybit.ClassifyEvents(ctx, tx, mode, at("2026-08-01"), day); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func makeBrief(t *testing.T, s *db.Store, ctx context.Context, opt Options, now time.Time) *Brief {
	t.Helper()
	var b *Brief
	if err := InTx(ctx, s, func(tx *sql.Tx) error {
		var err error
		b, err = Make(ctx, tx, opt, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return b
}

func decide(ctx context.Context, s *db.Store, brief int64, now time.Time, specs ...string) (*Result, error) {
	set := make([]Decision, 0, len(specs))
	for _, spec := range specs {
		d, err := ParseSet(spec)
		if err != nil {
			return nil, err
		}
		set = append(set, d)
	}
	var res *Result
	err := InTx(ctx, s, func(tx *sql.Tx) error {
		var err error
		res, err = Decide(ctx, tx, brief, set, "owner-key", now)
		return err
	})
	return res, err
}

func nums(b *Brief) string {
	parts := make([]string, len(b.Items))
	for i, it := range b.Items {
		parts[i] = fmt.Sprintf("%d=%s", it.Num, it.Key)
	}
	return strings.Join(parts, " ")
}

func TestEmptyBriefSaysSo(t *testing.T) {
	s, ctx := open(t, "shadow")
	if b := makeBrief(t, s, ctx, Options{}, day); b != nil {
		t.Fatalf("nothing is open, got %+v", b)
	}
	if a := Respond(nil); !a.Empty || a.Text != EmptyText {
		t.Fatalf("%+v", a)
	}
}

func TestNumbersAreFrozenAcrossNewProposals(t *testing.T) {
	s, ctx := open(t, "shadow")
	addRule(t, s, "TaddrA", "expense", "Подрядчик А", "Подрядчик")
	movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "11", "1", "2026-09-05T10:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "2", "expense", "TaddrA", "22", "1", "2026-09-06T10:00:00+03:00")

	first := makeBrief(t, s, ctx, Options{Author: "agent"}, day)
	// «Записал сам» comes before «не разобрался», whatever arrived first.
	if got := nums(first); got != "1=withdrawal:2 2=withdrawal:1" {
		t.Fatal(got)
	}
	movement(t, s, ctx, "withdrawal", "3", "expense", "TaddrU", "33", "1", "2026-09-07T10:00:00+03:00")

	again := makeBrief(t, s, ctx, Options{}, day.Add(time.Hour))
	if again.ID != first.ID || nums(again) != nums(first) {
		t.Fatalf("an open unsent brief keeps its numbers: %d %s", again.ID, nums(again))
	}
	fresh := makeBrief(t, s, ctx, Options{New: true}, day.Add(2*time.Hour))
	if fresh.ID == first.ID || len(fresh.Items) != 3 {
		t.Fatalf("--new: %+v", fresh)
	}
	if got := nums(fresh); got != "1=withdrawal:2 2=withdrawal:1 3=withdrawal:3" {
		t.Fatal(got)
	}
	b, err := Get(ctx, s.RO, first.ID)
	if err != nil || nums(b) != "1=withdrawal:2 2=withdrawal:1" {
		t.Fatal(nums(b), err)
	}
	for _, q := range []string{`UPDATE brief_items SET num = 7 WHERE brief_id = ? AND num = 1`,
		`UPDATE brief_items SET proposal_version = 9 WHERE brief_id = ? AND num = 1`,
		`DELETE FROM brief_items WHERE brief_id = ?`,
		`INSERT INTO brief_items(brief_id, num, proposal_id, proposal_version, key, block, summary, asked_count, first_asked_at) VALUES (?, 9, 1, 1, 'x', 'auto', 'x', 1, 'now')`} {
		if _, err := s.RW.Exec(q, first.ID); err == nil || !strings.Contains(err.Error(), "frozen") {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestBlocksInOrderAndHangingInTheTail(t *testing.T) {
	s, ctx := open(t, "shadow")
	addRule(t, s, "TaddrA", "expense", "Подрядчик А", "Подрядчик")
	for i, in := range []Input{
		{Key: "card:1", Kind: "card_purchase", Block: "unknown", Summary: "покупка"},
		{Key: "withdrawal:9", Kind: bybit.KindNonUSDT, Block: "unknown", Summary: "не USDT"},
		{Key: "p2p:9", Kind: bybit.KindP2P, Block: "unknown", Summary: "P2P"},
		{Key: "withdrawal:8", Kind: bybit.KindMovement, Block: "unknown", Summary: "неясно"},
		{Key: "withdrawal:7", Kind: bybit.KindMovement, Block: "likely", Summary: "похоже"},
		{Key: "withdrawal:6", Kind: bybit.KindMovement, Block: "auto", Summary: "точно"},
	} {
		in.Author, in.Content = "test", map[string]any{"key": in.Key, "n": i}
		if _, _, err := Upsert(ctx, s.RW, in, day); err != nil {
			t.Fatal(err)
		}
	}
	first := makeBrief(t, s, ctx, Options{}, day)
	var blocks []string
	for _, it := range first.Items {
		blocks = append(blocks, it.Block)
	}
	if got := strings.Join(blocks, " "); got != "auto likely unknown p2p non_usdt cards" {
		t.Fatal(got)
	}
	text := first.Text()
	for _, want := range []string{"[Записал сам (тень", "[Похоже, проверь]", "[Не разобрался]", "[P2P]", "[Не-USDT]", "[Карты]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
	// Drafts are not questions: only sent briefs count, one per MSK day.
	if a := makeBrief(t, s, ctx, Options{New: true}, day.AddDate(0, 0, 1)); a.Items[0].AskedCount != 1 || a.Items[0].Hanging {
		t.Fatalf("%+v", a.Items[0])
	}
	send := func(id int64, when time.Time) {
		t.Helper()
		if _, err := MarkSent(ctx, s.RW, id, when); err != nil {
			t.Fatal(err)
		}
	}
	d1 := at("2026-09-11T00:30:00+03:00")
	send(first.ID, d1)
	second := makeBrief(t, s, ctx, Options{New: true}, d1.Add(time.Hour))
	send(second.ID, d1.Add(2*time.Hour)) // the same MSK day
	third := makeBrief(t, s, ctx, Options{New: true}, d1.Add(24*time.Hour))
	if third.Items[0].AskedCount != 2 || third.Items[0].Hanging {
		t.Fatalf("two days: %+v", third.Items[0])
	}
	send(third.ID, d1.Add(25*time.Hour))
	if _, err := s.RW.Exec(`UPDATE proposals SET status = 'superseded' WHERE key = 'card:1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Upsert(ctx, s.RW, Input{Key: "withdrawal:5", Kind: bybit.KindMovement, Block: "unknown", Summary: "новое", Author: "test", Content: map[string]any{"key": "withdrawal:5"}}, day); err != nil {
		t.Fatal(err)
	}
	fourth := makeBrief(t, s, ctx, Options{New: true}, d1.Add(48*time.Hour))
	if got := fourth.Items[0].Key; got != "withdrawal:5" || fourth.Items[0].Hanging {
		t.Fatalf("what is new goes first: %+v", fourth.Items[0])
	}
	if last := fourth.Items[len(fourth.Items)-1]; !last.Hanging || last.AskedCount != 3 || last.HangingNights != 2 {
		t.Fatalf("asked on three days: %+v", last)
	}
	if text := fourth.Text(); !strings.Contains(text, "[Висит]") || !strings.Contains(text, "висит 2 ночей") {
		t.Fatal(text)
	}
	if at, err := MarkSent(ctx, s.RW, first.ID, d1.Add(99*time.Hour)); err != nil || at != ts.Format(d1) {
		t.Fatalf("the first send stays: %s %v", at, err)
	}
	var r *Refusal
	if _, err := MarkSent(ctx, s.RW, 999, d1); !errors.As(err, &r) {
		t.Fatal(err)
	}
}

func TestDecideRefusesTheWholeSet(t *testing.T) {
	for _, mode := range []string{"shadow", "live"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := open(t, mode)
			movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "10", "1", "2026-09-05T10:00:00+03:00")
			movement(t, s, ctx, "withdrawal", "2", "expense", "TaddrV", "20", "", "2026-09-06T10:00:00+03:00")
			b := makeBrief(t, s, ctx, Options{}, day)
			docs := count(t, s, `SELECT count(*) FROM documents`)
			links := count(t, s, `SELECT count(*) FROM event_links`)
			custom := `1=custom:{"category":"Инструменты","counterparty":"Сервис Б"}`
			for name, set := range map[string][]string{
				"no such item":  {custom, "99=defer"},
				"twice":         {custom, "1=defer"},
				"unknown brief": nil,
			} {
				if set == nil {
					if _, err := decide(ctx, s, 999, day, custom); err == nil || !strings.Contains(err.Error(), "не найден") {
						t.Fatal(name, err)
					}
					continue
				}
				_, err := decide(ctx, s, b.ID, day, set...)
				var r *Refusal
				if !errors.As(err, &r) {
					t.Fatal(name, err)
				}
			}
			if n := count(t, s, `SELECT count(*) FROM documents`); n != docs {
				t.Fatalf("documents %d, was %d", n, docs)
			}
			if n := count(t, s, `SELECT count(*) FROM event_links`); n != links {
				t.Fatalf("links %d, was %d", n, links)
			}
			if n := count(t, s, `SELECT count(*) FROM brief_items WHERE decision IS NOT NULL`); n != 0 {
				t.Fatalf("recorded decisions %d", n)
			}
			if n := count(t, s, `SELECT count(*) FROM proposals WHERE status = 'open' AND decision IS NULL`); n != 2 {
				t.Fatalf("open proposals %d", n)
			}
		})
	}
}

func TestDecisionIsBoundToTheVersionShown(t *testing.T) {
	s, ctx := open(t, "shadow")
	in := Input{Key: "agent:x", Kind: KindAgent, Block: "likely", Summary: "было", Author: "agent", Content: map[string]any{"key": "agent:x"}}
	if _, change, err := Upsert(ctx, s.RW, in, day); err != nil || change != ChangeNew {
		t.Fatal(change, err)
	}
	if _, change, err := Upsert(ctx, s.RW, in, day); err != nil || change != ChangeSame {
		t.Fatal(change, err)
	}
	b := makeBrief(t, s, ctx, Options{}, day)
	in.Summary = "стало"
	p, change, err := Upsert(ctx, s.RW, in, day)
	if err != nil || change != ChangeVersion || p.Version != 2 {
		t.Fatal(p, change, err)
	}
	_, err = decide(ctx, s, b.ID, day, "1=confirm")
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "изменилось") {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM brief_items WHERE decision IS NOT NULL`); n != 0 {
		t.Fatal("recorded", n)
	}
	// The old version is closed, the new one reaches a new brief with its version.
	again := makeBrief(t, s, ctx, Options{}, day)
	if again.ID == b.ID || again.Items[0].ProposalVersion != 2 || again.Items[0].Summary != "стало" {
		t.Fatalf("%+v", again.Items)
	}
	if _, err := decide(ctx, s, again.ID, day, "1=defer"); err != nil {
		t.Fatal(err)
	}
}

func TestShadowOnlyRecords(t *testing.T) {
	s, ctx := open(t, "shadow")
	addRule(t, s, "TaddrA", "expense", "Подрядчик А", "Подрядчик")
	event := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrA", "10", "1", "2026-09-05T10:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "2", "expense", "TaddrU", "20", "", "2026-09-05T11:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "3", "expense", "TaddrV", "30", "", "2026-09-05T12:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "4", "expense", "TaddrW", "40", "", "2026-09-05T13:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	docs := count(t, s, `SELECT count(*) FROM documents`)
	entries := count(t, s, `SELECT count(*) FROM entries`)
	res, err := decide(ctx, s, b.ID, day, "1=confirm", "2=custom:просто текст владельца", "3=personal", "4=recorded:388,389")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "shadow" || len(res.Written) != 4 {
		t.Fatalf("%+v", res)
	}
	if n := count(t, s, `SELECT count(*) FROM documents`); n != docs {
		t.Fatalf("shadow wrote documents: %d", n-docs)
	}
	if n := count(t, s, `SELECT count(*) FROM entries`); n != entries {
		t.Fatalf("shadow wrote entries: %d", n-entries)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ?`, event); n != 0 {
		t.Fatalf("shadow wrote links: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE status = 'open' AND decision IS NOT NULL AND brief_id = ?`, b.ID); n != 4 {
		t.Fatalf("proposals keep the decision and stay open: %d", n)
	}
	// Until the journal answers, the decision may change, like in the old flow.
	if _, err := decide(ctx, s, b.ID, day, "1=defer"); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err := s.RW.QueryRow(`SELECT decision FROM brief_items WHERE brief_id = ? AND num = 1`, b.ID).Scan(&decision); err != nil || decision != "defer" {
		t.Fatal(decision, err)
	}
	if !strings.Contains(res.Text, "решение: custom «просто текст владельца»") {
		t.Fatal(res.Text)
	}
}

func TestJournalRowLinkedToMovementClosesTheProposal(t *testing.T) {
	s, ctx := open(t, "shadow")
	movement(t, s, ctx, "withdrawal", "77", "expense", "TaddrU", "30", "", "2026-09-05T10:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "78", "expense", "TaddrU", "40", "", "2026-09-05T11:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	if b == nil || len(b.Items) != 2 {
		t.Fatalf("%+v", b)
	}
	text := journalHead + strings.Join([]string{
		journalRow("1", "2026-08-01", "income", "Owner вливание", "Владелец", "—", "1000.00", "USDT", "Internal Transfer (Bybit)", "paid", "старт"),
		journalRow("2", "2026-08-15", "expense", "Owner draw", "Владелец", "—", "100.00", "USDT", "P2P (Bybit)", "paid", "изъятие"),
		journalRow("3", "2026-09-02", "expense", "Инструменты", "Сервис А", "—", "30.00", "USDT", "USDT TRC-20", "paid", "подписка"),
		journalRow("4", "2026-09-05", "expense", "Инструменты", "Сервис Б", "—", "30.00", "USDT", "USDT TRC-20", "paid", "оплата withdrawal:77"),
	}, "\n") + "\n"
	tx := begin(t, s)
	if _, err := journal.Import(ctx, tx, journal.FromFile(text, "file", day), journal.Options{AccrualFrom: "2026-09-01", Models: []string{"Alfa", "Beta"}}); err != nil {
		t.Fatal(err)
	}
	rep, err := Sweep(ctx, tx, "import:ledger-md", day)
	if err != nil || rep.Superseded != 1 {
		t.Fatal(rep, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status, decision, text2 string
	if err := s.RW.QueryRow(`SELECT status, decision, decision_text FROM proposals WHERE key = 'withdrawal:77'`).Scan(&status, &decision, &text2); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || decision != "recorded" || !strings.Contains(text2, "#4") {
		t.Fatal(status, decision, text2)
	}
	// It does not surface again, and an old number of it is refused; the other keeps its number.
	if _, err := decide(ctx, s, b.ID, day, "1=confirm"); err == nil || !strings.Contains(err.Error(), "уже закрыт") {
		t.Fatal(err)
	}
	if _, err := MarkSent(ctx, s.RW, b.ID, day); err != nil {
		t.Fatal(err)
	}
	next := makeBrief(t, s, ctx, Options{}, day)
	if got := nums(next); got != "1=withdrawal:78" {
		t.Fatal(got)
	}
	got, err := Get(ctx, s.RO, b.ID)
	if err != nil || got.Items[0].Key != "withdrawal:77" || got.Items[0].ClosedBy != "superseded:recorded" || !strings.Contains(got.Text(), "закрыт: superseded:recorded") {
		t.Fatalf("%+v %v", got, err)
	}
	// With the last open item closed too, the brief closes and pending shows nothing.
	if _, err := s.RW.Exec(`UPDATE proposals SET status = 'superseded' WHERE key = 'withdrawal:78'`); err != nil {
		t.Fatal(err)
	}
	pend, err := Pending(ctx, s.RO)
	if err != nil || len(pend) != 0 {
		t.Fatal(pend, err)
	}
}

// liveProposals classifies in shadow and goes live: what the switchover meets.
func goLive(t *testing.T, s *db.Store) {
	t.Helper()
	if _, err := s.RW.Exec(`UPDATE settings SET value = 'live' WHERE key = 'mode'`); err != nil {
		t.Fatal(err)
	}
}

func TestLiveConfirmWritesTheNextRowsAndTheMirrorShowsThem(t *testing.T) {
	s, ctx := open(t, "shadow")
	addRule(t, s, "TaddrA", "expense", "Подрядчик А", "Подрядчик")
	event := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrA", "100", "1.5", "2026-09-05T10:00:00+03:00")
	goLive(t, s)
	b := makeBrief(t, s, ctx, Options{}, day)
	before := count(t, s, `SELECT count(*) FROM entries`)
	res, err := decide(ctx, s, b.ID, day, "1=confirm")
	if err != nil {
		t.Fatal(err)
	}
	if w := res.Written[0]; w.Status != "approved" || fmt.Sprint(w.Documents) != "[4 5]" {
		t.Fatalf("%+v", w)
	}
	if n := count(t, s, `SELECT count(*) FROM entries`) - before; n != 2 {
		t.Fatalf("entries %d", n)
	}
	// The money left Bybit through the two rows, tied to the event and the proposal.
	var doc ledger.Document
	doc, err = ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || doc.Amount != "100.00" || doc.Category != "Подрядчик" || doc.Origin == "" || doc.ProposalID == 0 {
		t.Fatalf("%+v %v", doc, err)
	}
	fee, err := ledger.CurrentDocument(ctx, s.RW, 5)
	if err != nil || fee.Category != "Комиссии" || fee.Amount != "1.50" || fee.Counterparty != "Подрядчик А" {
		t.Fatalf("%+v %v", fee, err)
	}
	bal, err := ledger.Balance(ctx, s.RW, "bybit", "", "")
	if err != nil || bal != money.MustParse("768.50") {
		t.Fatal(bal, err)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND how = 'mark' AND decision = 'confirm' AND document_num IN (4, 5) AND brief_ref = ?`,
		event, fmt.Sprintf("ledger:%d/1", b.ID)); n != 2 {
		t.Fatalf("links %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM entry_events`); n != 2 {
		t.Fatalf("entry events %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM v_bybit_unlinked`); n != 0 {
		t.Fatalf("unlinked %d", n)
	}
	mirror, err := journal.Export(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mirror, "| 4 | 2026-09-05 | expense | Подрядчик | Подрядчик А | — | 100.00 | USDT | USDT TRC-20 | paid | withdrawal:1 |") ||
		!strings.Contains(mirror, "| 5 | 2026-09-05 | expense | Комиссии | Подрядчик А | — | 1.50 | USDT | USDT TRC-20 | paid | комиссия сети withdrawal:1 |") {
		t.Fatal(mirror)
	}
	f, errs := journal.Parse(mirror)
	if len(errs) > 0 || len(f.Rows) != 5 {
		t.Fatal(errs, len(f.Rows))
	}
	// A second decision on the closed item is refused, whatever it says.
	for _, again := range []string{"1=confirm", "1=personal", "1=defer"} {
		if _, err := decide(ctx, s, b.ID, day, again); err == nil || !strings.Contains(err.Error(), "уже закрыт") {
			t.Fatal(again, err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM documents`); n != 5 {
		t.Fatalf("documents %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM briefs WHERE id = ? AND status = 'closed'`, b.ID); n != 1 {
		t.Fatal("a brief with nothing left closes")
	}
}

func TestAutoIsWrittenInLiveAndShownOnce(t *testing.T) {
	s, ctx := open(t, "live")
	addRule(t, s, "TaddrA", "expense", "Подрядчик А", "Подрядчик")
	event := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrA", "100", "", "2026-09-05T10:00:00+03:00")
	var status, by string
	if err := s.RW.QueryRow(`SELECT status, decided_by FROM proposals WHERE key = 'withdrawal:1'`).Scan(&status, &by); err != nil || status != "auto" || by != "rule" {
		t.Fatal(status, by, err)
	}
	if _, err := ledger.CurrentDocument(ctx, s.RW, 4); err != nil {
		t.Fatal("no row", err)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND how = 'rule' AND decision = 'auto' AND document_num = 4`, event); n != 1 {
		t.Fatalf("links %d", n)
	}
	b := makeBrief(t, s, ctx, Options{}, day)
	if b == nil || b.Items[0].Block != "auto" || !strings.Contains(b.Text(), "[Записал сам]") {
		t.Fatalf("%+v", b)
	}
	// A blanket «да» is harmless; a correction goes through a dispute; nothing is written twice.
	if _, err := decide(ctx, s, b.ID, day, "1=personal"); err == nil || !strings.Contains(err.Error(), "dispute") {
		t.Fatal(err)
	}
	if _, err := decide(ctx, s, b.ID, day, "1=confirm"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num >= 4`); n != 1 {
		t.Fatalf("rows %d", n)
	}
	if _, err := MarkSent(ctx, s.RW, b.ID, day); err != nil {
		t.Fatal(err)
	}
	if next := makeBrief(t, s, ctx, Options{}, day.Add(time.Hour)); next != nil {
		t.Fatalf("shown once: %+v", next)
	}
}

func TestAutoTheJournalWouldRefuseStaysAQuestion(t *testing.T) {
	s, ctx := open(t, "live")
	// A transfer from a model needs its {перевод} tag: the rule cannot know it.
	if _, err := s.RW.Exec(`INSERT INTO rules(kind, match_address, direction, name, category, model, note, created_at, source)
		VALUES ('counterparty', 'TaddrM', 'income', 'Модель А', 'Платформы', 'Alfa', '', 'now', 'test')`); err != nil {
		t.Fatal(err)
	}
	movement(t, s, ctx, "deposit", "1", "income", "TaddrM", "500", "", "2026-09-05T10:00:00+03:00")
	var status, block, hint string
	if err := s.RW.QueryRow(`SELECT status, block, json_extract(content, '$.hint') FROM proposals WHERE key = 'deposit:1'`).Scan(&status, &block, &hint); err != nil {
		t.Fatal(err)
	}
	if status != "open" || block != "likely" || !strings.Contains(hint, "перевод") {
		t.Fatal(status, block, hint)
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num >= 4`); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

func TestLiveCustomPatch(t *testing.T) {
	s, ctx := open(t, "live")
	event := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "100", "1.5", "2026-09-05T10:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	refusals := map[string]string{
		"1=confirm":                   "предложенной строки нет",
		"1=custom:просто текст":       "JSON",
		`1=custom:{"category":"X"}`:   "контрагента",
		`1=custom:{"unknown":"поле"}`: "unknown",
		`1=custom:{"category":"Трафик","counterparty":"К","amount":"99"}`:                             "двигают",
		`1=custom:{"category":"Трафик","counterparty":"К","split":[{"amount":"60"},{"amount":"30"}]}`: "двигают",
	}
	for spec, want := range refusals {
		if _, err := decide(ctx, s, b.ID, day, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", spec, err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num >= 4`); n != 0 {
		t.Fatalf("rows %d", n)
	}
	res, err := decide(ctx, s, b.ID, day, `1=custom:{"counterparty":"Подрядчик Б","note":"разбор | с чертой","split":[{"amount":"60","category":"Трафик"},{"amount":40,"category":"Подрядчик"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(res.Written[0].Documents) != "[4 5 6]" {
		t.Fatalf("%+v", res.Written[0])
	}
	first, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || first.Category != "Трафик" || first.Amount != "60.00" || first.Counterparty != "Подрядчик Б" || first.Note != "разбор / с чертой; withdrawal:1" {
		t.Fatalf("%+v %v", first, err)
	}
	second, _ := ledger.CurrentDocument(ctx, s.RW, 5)
	feeRow, _ := ledger.CurrentDocument(ctx, s.RW, 6)
	if second.Category != "Подрядчик" || second.Amount != "40.00" || feeRow.Category != "Комиссии" || feeRow.Counterparty != "Подрядчик Б" {
		t.Fatalf("%+v %+v", second, feeRow)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND decision = 'custom'`, event); n != 3 {
		t.Fatalf("links %d", n)
	}
	mirror, err := journal.Export(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if _, errs := journal.Parse(mirror); len(errs) > 0 {
		t.Fatal(errs)
	}
}

func TestLiveCustomKeepsTheRulesOfTheJournal(t *testing.T) {
	s, ctx := open(t, "live")
	movement(t, s, ctx, "deposit", "1", "income", "TaddrU", "100", "", "2026-09-05T10:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	for spec, want := range map[string]string{
		`1=custom:{"category":"Custom","counterparty":"Фан","model":"Alfa","note":"без пометки"}`:                "сделка",
		`1=custom:{"category":"Custom","counterparty":"Фан","model":"Alfa","note":"битая {пометка}"}`:            "битый тег",
		`1=custom:{"category":"Нет такой","counterparty":"Фан","note":"{доля 0}"}`:                               "категорией",
		`1=custom:{"category":"Custom","counterparty":"Фан","model":"Чужая","note":"{доля 0}","date":"сегодня"}`: "дата",
	} {
		if _, err := decide(ctx, s, b.ID, day, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", spec, err)
		}
	}
	if _, err := decide(ctx, s, b.ID, day, `1=custom:{"category":"Custom","counterparty":"Фан","model":"Alfa","note":"{доля 0}"}`); err != nil {
		t.Fatal(err)
	}
	d, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || d.Note != "{доля 0}; deposit:1" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestLivePersonal(t *testing.T) {
	s, ctx := open(t, "live")
	spend := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "100", "1", "2026-09-05T10:00:00+03:00")
	came := movement(t, s, ctx, "deposit", "2", "income", "TaddrV", "50", "", "2026-09-05T11:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	if _, err := decide(ctx, s, b.ID, day, "1=personal", "2=personal"); err != nil {
		t.Fatal(err)
	}
	// A personal spend from the business wallet is an Owner draw row in the journal's name for the owner.
	draw, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || draw.Category != "Owner draw" || draw.Counterparty != "Владелец" || draw.Amount != "100.00" {
		t.Fatalf("%+v %v", draw, err)
	}
	feeRow, err := ledger.CurrentDocument(ctx, s.RW, 5)
	if err != nil || feeRow.Category != "Комиссии" || feeRow.Amount != "1.00" {
		t.Fatalf("%+v %v", feeRow, err)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND decision = 'personal' AND document_num IS NOT NULL`, spend); n != 2 {
		t.Fatalf("links %d", n)
	}
	// Money that came in closes with the decision alone.
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND decision = 'personal' AND document_num IS NULL`, came); n != 1 {
		t.Fatalf("links %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num > 5`); n != 0 {
		t.Fatalf("rows %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM v_bybit_unlinked`); n != 0 {
		t.Fatalf("unlinked %d", n)
	}
}

func TestLiveRecordedAndDefer(t *testing.T) {
	s, ctx := open(t, "live")
	event := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "30", "", "2026-09-03T10:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "2", "expense", "TaddrV", "40", "", "2026-09-03T11:00:00+03:00")
	movement(t, s, ctx, "withdrawal", "3", "expense", "TaddrW", "50", "", "2026-09-03T12:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	if b.Items[0].LedgerMatch != "#3" || !strings.Contains(b.Text(), "уже записано тобой? #3 — закрыть?") {
		t.Fatalf("%+v", b.Items[0])
	}
	for spec, want := range map[string]string{"1=recorded:999": "строки #999 нет", "2=recorded": "неоднозначна", "1=recorded:abc": "не номер"} {
		if _, err := decide(ctx, s, b.ID, day, spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", spec, err)
		}
	}
	res, err := decide(ctx, s, b.ID, day, "1=recorded", "2=recorded:1,2", "3=defer")
	if err != nil {
		t.Fatal(err)
	}
	if res.Written[2].Status != "open" {
		t.Fatalf("%+v", res.Written[2])
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND document_num = 3 AND decision = 'recorded'`, event); n != 1 {
		t.Fatalf("links %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM documents`); n != 3 {
		t.Fatalf("recorded writes nothing: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE key = 'withdrawal:3' AND status = 'open' AND decision = 'defer'`); n != 1 {
		t.Fatal("defer stays open")
	}
	// What is deferred comes back in the next brief.
	if _, err := MarkSent(ctx, s.RW, b.ID, day); err != nil {
		t.Fatal(err)
	}
	if next := makeBrief(t, s, ctx, Options{}, day.AddDate(0, 0, 1)); next == nil || nums(next) != "1=withdrawal:3" {
		t.Fatalf("%+v", next)
	}
}

func agentProposal() AgentProposal {
	return AgentProposal{Key: "cash-1", Documents: []bybit.SuggestedDocument{{Date: "2026-09-05", Type: "expense", Category: "Прочее", Counterparty: "Курьер",
		Amount: "12.50", Currency: "USD", Method: "Bybit", Note: "наличные за доставку"}}, Evidence: "чек в чате"}
}

func TestAgentProposalGoesThroughTheSameBrief(t *testing.T) {
	s, ctx := open(t, "live")
	bad := agentProposal()
	bad.Documents[0].Category = "Нет такой"
	var r *Refusal
	if _, _, err := Propose(ctx, s.RW, bad, "agent", day); !errors.As(err, &r) {
		t.Fatal(err)
	}
	collector := agentProposal()
	collector.Key = "withdrawal:1"
	if _, _, err := Propose(ctx, s.RW, collector, "agent", day); !errors.As(err, &r) {
		t.Fatal(err)
	}
	auto := agentProposal()
	auto.Block = "auto"
	if _, _, err := Propose(ctx, s.RW, auto, "agent", day); !errors.As(err, &r) {
		t.Fatal(err)
	}
	p, change, err := Propose(ctx, s.RW, agentProposal(), "agent", day)
	if err != nil || change != ChangeNew || p.Key != "agent:cash-1" || p.Block != "likely" || p.Author != "agent" {
		t.Fatalf("%+v %s %v", p, change, err)
	}
	if _, change, err = Propose(ctx, s.RW, agentProposal(), "agent", day); err != nil || change != ChangeSame {
		t.Fatal(change, err)
	}
	changed := agentProposal()
	changed.Documents[0].Amount = "13.00"
	if p, change, err = Propose(ctx, s.RW, changed, "agent", day); err != nil || change != ChangeVersion || p.Version != 2 {
		t.Fatal(p, change, err)
	}
	b := makeBrief(t, s, ctx, Options{}, day)
	if b == nil || len(b.Items) != 1 || b.Items[0].Block != "likely" || b.Items[0].ProposalVersion != 2 {
		t.Fatalf("%+v", b)
	}
	if _, err := decide(ctx, s, b.ID, day, "1=confirm"); err != nil {
		t.Fatal(err)
	}
	d, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || d.Amount != "13.00" || d.Status != "paid" || d.Model != "—" {
		t.Fatalf("%+v %v", d, err)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE source = 'manual' AND kind = 'proposal'`); n != 2 {
		t.Fatalf("events %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links l JOIN events e ON e.id = l.event_id WHERE e.source = 'manual' AND l.document_num = 4`); n != 1 {
		t.Fatalf("links %d", n)
	}
}

func TestEvidenceIsShownOnItems(t *testing.T) {
	s, ctx := open(t, "shadow")
	if _, err := AddEvidence(ctx, s.RW, Evidence{Key: "withdrawal:1", Text: ""}, day); err == nil {
		t.Fatal("empty evidence")
	}
	if added, err := AddEvidence(ctx, s.RW, Evidence{Key: "withdrawal:1", Source: "tg", Text: "тот же кошелёк, что раньше", Author: "agent"}, day); err != nil || !added {
		t.Fatal(added, err)
	}
	if added, err := AddEvidence(ctx, s.RW, Evidence{Key: "withdrawal:1", Source: "tg", Text: "тот же кошелёк, что раньше", Author: "agent"}, day); err != nil || added {
		t.Fatal(added, err)
	}
	st, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rep, err := ImportEvidence(ctx, s.RW, st, day); err != nil || rep.New != 0 {
		t.Fatal("a state file without the table", rep, err)
	}
	if _, err := st.Exec(`CREATE TABLE evidence (id INTEGER PRIMARY KEY, key TEXT, source TEXT, text TEXT, created_ms INTEGER);
		INSERT INTO evidence(key, source, text, created_ms) VALUES ('withdrawal:1', 'gmail', 'письмо с той же суммой', 1788000000000), ('withdrawal:1', 'tg', 'тот же кошелёк, что раньше', 1788000000000)`); err != nil {
		t.Fatal(err)
	}
	if rep, err := ImportEvidence(ctx, s.RW, st, day); err != nil || rep.New != 1 || rep.Known != 1 {
		t.Fatal(rep, err)
	}
	if rep, err := ImportEvidence(ctx, s.RW, st, day); err != nil || rep.New != 0 || rep.Known != 2 {
		t.Fatal(rep, err)
	}
	movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "10", "", "2026-09-05T10:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	if len(b.Items[0].PriorEvidence) != 2 || b.Items[0].PriorEvidence[0].Source != "gmail" {
		t.Fatalf("%+v", b.Items[0].PriorEvidence)
	}
}

func TestDisputeCorrectsARowAndLeavesTheClosedMonth(t *testing.T) {
	s, ctx := open(t, "live")
	addRule(t, s, "TaddrA", "expense", "Подрядчик А", "Подрядчик")
	movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrA", "100", "", "2026-09-05T10:00:00+03:00")
	if err := ledger.Lock(ctx, s.RW, ledger.Period{Month: "2026-09", LockedAt: ts.Format(at("2026-10-08T12:00:00+03:00")), AsOf: ts.Format(at("2026-10-08T12:00:00+03:00")),
		Snapshot: json.RawMessage(`{}`), Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Propose(ctx, s.RW, agentProposal(), "agent", day); err != nil {
		t.Fatal(err)
	}
	_, closedBefore, err := ledger.MonthTotals(ctx, s.RW, "2026-09")
	if err != nil || closedBefore["expense:contractor"] != money.MustParse("100") {
		t.Fatal(closedBefore, err)
	}
	var r *Refusal
	if _, err := OpenDispute(ctx, s.RW, OpenInput{Account: "bybit"}, "agent", day); !errors.As(err, &r) {
		t.Fatal(err)
	}
	if _, err := OpenDispute(ctx, s.RW, OpenInput{Subject: "x", Account: "нет такого"}, "agent", day); !errors.As(err, &r) {
		t.Fatal(err)
	}
	d, err := OpenDispute(ctx, s.RW, OpenInput{Subject: "это был трафик", Account: "expense:contractor", Amount: "100", Evidence: "переписка"}, "agent", day)
	if err != nil || d.Status != "open" || d.Amount != "100.00" {
		t.Fatalf("%+v %v", d, err)
	}
	list, err := ListDisputes(ctx, s.RO, "open")
	if err != nil || len(list) != 1 || list[0].Subject != "это был трафик" {
		t.Fatal(list, err)
	}
	num := 4
	resolve := func(in ResolveInput) (Dispute, error) {
		var out Dispute
		err := InTx(ctx, s, func(tx *sql.Tx) error {
			var err error
			out, err = ResolveDispute(ctx, tx, d.ID, in, "owner-key", at("2026-11-01T12:00:00+03:00"))
			return err
		})
		return out, err
	}
	category := "Трафик"
	wrong := "Нет такой"
	same := "Подрядчик"
	for _, bad := range []ResolveInput{{}, {Reject: true, Documents: []RowPatch{{Num: &num}}},
		{Documents: []RowPatch{{Num: &num, Patch: Patch{Category: &wrong}}}},
		{Documents: []RowPatch{{Num: &num, Patch: Patch{Category: &same}}}}} {
		if _, err := resolve(bad); !errors.As(err, &r) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	entriesBefore := count(t, s, `SELECT count(*) FROM entries`)
	out, err := resolve(ResolveInput{Note: "по переписке", Documents: []RowPatch{{Num: &num, Patch: Patch{Category: &category}}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "resolved" || out.ResolvedBy != "owner-key" {
		t.Fatalf("%+v", out)
	}
	// A new version, a reversal and a new posting, all with the dispute; the old stays.
	cur, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || cur.Version != 2 || cur.Category != "Трафик" || cur.DisputeID != d.ID {
		t.Fatalf("%+v %v", cur, err)
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num = 4`); n != 2 {
		t.Fatalf("versions %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM entries`) - entriesBefore; n != 2 {
		t.Fatalf("new entries %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM entries WHERE dispute_id = ?`, d.ID); n != 2 {
		t.Fatalf("with the dispute %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM entries WHERE dispute_id = ? AND reverses_id IS NOT NULL`, d.ID); n != 1 {
		t.Fatalf("reversals %d", n)
	}
	// «На закрытии» is what it was, «сейчас» shows the correction.
	nowTotals, closedAfter, err := ledger.MonthTotals(ctx, s.RW, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if closedAfter["expense:contractor"] != money.MustParse("100") || closedAfter["expense:traffic"] != 0 {
		t.Fatalf("at close changed: %+v", closedAfter)
	}
	if nowTotals["expense:contractor"] != 0 || nowTotals["expense:traffic"] != money.MustParse("100") {
		t.Fatalf("now: %+v", nowTotals)
	}
	if n := count(t, s, `SELECT count(*) FROM v_unbalanced`); n != 0 {
		t.Fatalf("unbalanced %d", n)
	}
	mirror, err := journal.Export(ctx, s.RO)
	if err != nil || !strings.Contains(mirror, "| 4 | 2026-09-05 | expense | Трафик |") {
		t.Fatal(mirror, err)
	}
	if _, err := resolve(ResolveInput{Reject: true}); !errors.As(err, &r) {
		t.Fatal("closed dispute", err)
	}
}

func TestDisputeInShadowRefusesCorrectionsAndCanBeRejected(t *testing.T) {
	s, ctx := open(t, "shadow")
	d, err := OpenDispute(ctx, s.RW, OpenInput{Subject: "сумма"}, "agent", day)
	if err != nil {
		t.Fatal(err)
	}
	num, note := 3, "другое"
	entries := count(t, s, `SELECT count(*) FROM entries`)
	_, err = ResolveDispute(ctx, s.RW, d.ID, ResolveInput{Documents: []RowPatch{{Num: &num, Patch: Patch{Note: &note}}}}, "owner-key", day)
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "/OS") {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM entries`); n != entries {
		t.Fatal("shadow posted")
	}
	got, err := GetDispute(ctx, s.RO, d.ID)
	if err != nil || got.Status != "open" {
		t.Fatalf("%+v %v", got, err)
	}
	out, err := ResolveDispute(ctx, s.RW, d.ID, ResolveInput{Reject: true, Note: "правится в /OS"}, "owner-key", day)
	if err != nil || out.Status != "rejected" || string(out.Resolution) != `"правится в /OS"` {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestParseRowPatches(t *testing.T) {
	one, err := ParseRowPatches(`{"num": 5, "amount": 12.5, "category": "Трафик"}`)
	if err != nil || len(one) != 1 || *one[0].Num != 5 || string(*one[0].Amount) != "12.5" {
		t.Fatalf("%+v %v", one, err)
	}
	two, err := ParseRowPatches(`[{"num": 5, "status": "cancelled"}, {"date": "2026-09-05", "type": "expense"}]`)
	if err != nil || len(two) != 2 || two[1].Num != nil {
		t.Fatalf("%+v %v", two, err)
	}
	if _, err := ParseRowPatches(`{"num": 5, "mystery": 1}`); err == nil {
		t.Fatal("unknown field")
	}
}

func TestAPIScopes(t *testing.T) {
	s, ctx := open(t, "shadow")
	movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "10", "", "2026-09-05T10:00:00+03:00")
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) string {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	reader, proposer, decider := key("reader", auth.ScopeRead), key("proposer", auth.ScopePropose), key("decider", auth.ScopeDecide)
	call := func(method, path, token, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := call("POST", "/v1/brief", reader, `{}`); code != 403 {
		t.Fatal(code)
	}
	code, out := call("POST", "/v1/brief", proposer, `{}`)
	if code != 200 || out["brief"] == nil {
		t.Fatal(code, out)
	}
	id := int(out["brief"].(map[string]any)["id"].(float64))
	if code, _ := call("POST", fmt.Sprintf("/v1/brief/%d/decide", id), proposer, `{"set":[{"num":1,"kind":"defer"}]}`); code != 403 {
		t.Fatal(code)
	}
	if code, out := call("POST", fmt.Sprintf("/v1/brief/%d/decide", id), decider, `{"set":[{"num":9,"kind":"defer"}]}`); code != 422 || !strings.Contains(fmt.Sprint(out["error"]), "нет пункта 9") {
		t.Fatal(code, out)
	}
	if code, out := call("POST", fmt.Sprintf("/v1/brief/%d/decide", id), decider, `{"set":[{"num":1,"kind":"defer"}]}`); code != 200 {
		t.Fatal(code, out)
	}
	var by string
	if err := s.RW.QueryRow(`SELECT decided_by FROM brief_items WHERE brief_id = ? AND num = 1`, id).Scan(&by); err != nil || by != "decider" {
		t.Fatal(by, err)
	}
	if code, _ := call("POST", "/v1/proposals/1/decide", proposer, `{"decision":"confirm"}`); code != 403 {
		t.Fatal(code)
	}
	if code, out := call("POST", "/v1/proposals/1/decide", decider, `{"decision":"defer"}`); code != 422 || !strings.Contains(fmt.Sprint(out["error"]), "без брифа") {
		t.Fatal(code, out)
	}
	if code, out := call("POST", fmt.Sprintf("/v1/brief/%d/sent", id), proposer, ""); code != 200 || out["sent_at"] == nil {
		t.Fatal(code, out)
	}
	if code, _ := call("GET", "/v1/pending", reader, ""); code == 403 {
		t.Fatal(code)
	}
	if code, out := call("POST", "/v1/disputes", proposer, `{"subject":"сумма"}`); code != 200 || out["opened_by"] != "proposer" {
		t.Fatal(code, out)
	}
	if code, _ := call("POST", "/v1/disputes/1/resolve", proposer, `{"reject":true}`); code != 403 {
		t.Fatal(code)
	}
	if code, out := call("POST", "/v1/disputes/1/resolve", decider, `{"reject":true,"note":"нет"}`); code != 200 || out["status"] != "rejected" {
		t.Fatal(code, out)
	}
}

func TestAgentProposalInShadowClosesOnTheAnswer(t *testing.T) {
	s, ctx := open(t, "shadow")
	in := agentProposal()
	p, _, err := Propose(ctx, s.RW, in, "agent", day)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := EvidenceFor(ctx, s.RO, p.Key); err != nil || len(got) != 1 || got[0].Text != "чек в чате" || got[0].Author != "agent" {
		t.Fatalf("%+v %v", got, err)
	}
	b := makeBrief(t, s, ctx, Options{}, day)
	if _, err := decide(ctx, s, b.ID, day, "1=confirm"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num >= 4`); n != 0 {
		t.Fatal("the row is for /OS")
	}
	if got := makeBrief(t, s, ctx, Options{}, day); got != nil {
		t.Fatalf("no movement will ever close it, the answer did: %+v", got)
	}
}

func TestLiveCustomOfACoinTheOwnerPutsInDollars(t *testing.T) {
	s, ctx := open(t, "live")
	w := at("2026-09-05T10:00:00+03:00")
	m := bybit.Movement{Key: "withdrawal:1", Kind: "withdrawal", Direction: "expense", TimeMs: w.UnixMilli(), At: ts.FormatMilli(w), Date: ts.Date(w), Coin: "ETH",
		Amount: "0.5", Fee: "0.001", CounterpartyKind: "address", Counterparty: "0xsynthetic", Method: "ETH"}
	if _, _, err := ledger.AddEvent(ctx, s.RW, m.Event(), w); err != nil {
		t.Fatal(err)
	}
	classify(t, s, ctx)
	b := makeBrief(t, s, ctx, Options{}, day)
	if b.Items[0].Block != "non_usdt" {
		t.Fatalf("%+v", b.Items[0])
	}
	// No suggestion, and half an ether is not half a dollar: the whole row or nothing.
	if _, err := decide(ctx, s, b.ID, day, `1=custom:{"category":"Трафик","counterparty":"К"}`); err == nil {
		t.Fatal("no amount, no method")
	}
	if _, err := decide(ctx, s, b.ID, day, `1=custom:{"type":"expense","category":"Трафик","counterparty":"К","amount":"1200","currency":"USD","method":"USDT TRC-20","note":"по курсу на день"}`); err != nil {
		t.Fatal(err)
	}
	d, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || d.Amount != "1200.00" || d.Date != "2026-09-05" || d.Note != "по курсу на день; withdrawal:1" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestDecideProposalWithoutABrief(t *testing.T) {
	s, ctx := open(t, "live")
	event := movement(t, s, ctx, "withdrawal", "1", "expense", "TaddrU", "100", "1.5", "2026-09-05T10:00:00+03:00")
	b := makeBrief(t, s, ctx, Options{}, day)
	if _, err := MarkSent(ctx, s.RW, b.ID, day); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.RW.QueryRow(`SELECT id FROM proposals WHERE key = 'withdrawal:1' AND status = 'open'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	direct := func(spec string) (*Result, error) {
		d, err := ParseDecision(spec)
		if err != nil {
			return nil, err
		}
		var res *Result
		err = InTx(ctx, s, func(tx *sql.Tx) error {
			var err error
			res, err = DecideProposal(ctx, tx, id, d, "session-key", day)
			return err
		})
		return res, err
	}
	for spec, want := range map[string]string{
		"defer":                     "без брифа",
		"confirm":                   "предложенной строки нет",
		`custom:{"category":"X"}`:   "контрагента",
		`custom:{"unknown":"поле"}`: "unknown",
	} {
		if _, err := direct(spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", spec, err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE num >= 4`); n != 0 {
		t.Fatalf("rows %d", n)
	}
	res, err := direct(`custom:{"category":"Трафик","counterparty":"Подрядчик Б"}`)
	if err != nil {
		t.Fatal(err)
	}
	if w := res.Written[0]; fmt.Sprint(w.Documents) != "[4 5]" || w.Proposal != id || w.Status != "approved" {
		t.Fatalf("%+v", w)
	}
	row, err := ledger.CurrentDocument(ctx, s.RW, 4)
	if err != nil || row.Category != "Трафик" || row.Amount != "100.00" || row.Author != "session-key" {
		t.Fatalf("%+v %v", row, err)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE id = ? AND status = 'approved' AND decided_by = 'session-key' AND brief_id IS NULL`, id); n != 1 {
		t.Fatal("proposal not closed by the session")
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND brief_ref = ?`, event, fmt.Sprintf("ledger:proposal/%d", id)); n != 2 {
		t.Fatalf("links %d", n)
	}
	// the question in the sent brief is answered with it, and the brief does not ask again
	if n := count(t, s, `SELECT count(*) FROM brief_items WHERE brief_id = ? AND decision = 'custom' AND decided_by = 'session-key'`, b.ID); n != 1 {
		t.Fatal("brief item not answered")
	}
	if next := makeBrief(t, s, ctx, Options{New: true}, day); next != nil {
		t.Fatalf("asked again: %s", nums(next))
	}
	if _, err := direct("confirm"); err == nil || !strings.Contains(err.Error(), "уже закрыт") {
		t.Fatal(err)
	}
}
