package bybit

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
)

func TestKeysIn(t *testing.T) {
	note := "перевод (internal_deposit:11, withdrawal:22), депозит deposit:33; p2p:1234567890123456789 card:44 и card:45:delta, ещё withdrawal:22. " +
		"не ключи: mywithdrawal:1 deposit:abc withdrawal:9x withdrawal: card:"
	var got []string
	for _, k := range KeysIn(note) {
		got = append(got, k.Key)
	}
	want := "internal_deposit:11,withdrawal:22,deposit:33,p2p:1234567890123456789,card:44,card:45:delta"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
	if k, ok := ParseKey("card:45:delta"); !ok || k.Source != "cincin_bot" || k.SourceID != "45" || k.Part != "delta" {
		t.Fatal(k, ok)
	}
	if k, ok := ParseKey("internal_deposit:7"); !ok || k.Source != "bybit" || k.SourceID != "internal_deposit:7" {
		t.Fatal(k, ok)
	}
	for _, bad := range []string{"deposit", "x deposit:1", "deposit:1 x", "withdrawal:1:delta", ""} {
		if _, ok := ParseKey(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestLinkNotesLinksAndClosesProposals(t *testing.T) {
	s, ctx := openDB(t)
	now := at("2026-10-01T10:00:00+03:00")
	w := addEvent(t, s, ctx, movement("withdrawal", "22", "expense", "address", "Tto", "100", "1", "2026-09-03T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("deposit", "33", "income", "address", "Tfrom", "50", "", "2026-09-04T10:00:00.000+03:00"))
	card, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: "cincin_bot", SourceID: "44", Kind: "purchase", OccurredAt: "2026-09-05T10:00:00+03:00",
		Payload: map[string]any{"amount": "5"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	classifyAll(t, s, ctx, "shadow")
	if count(t, s, `SELECT count(*) FROM proposals WHERE status = 'open'`) != 2 {
		t.Fatal("two open proposals expected")
	}
	journalRow(t, s, ctx, 100, "2026-09-03", "expense", "Подрядчик", "Подрядчик", "100", "оплата, withdrawal:22")
	journalRow(t, s, ctx, 101, "2026-09-03", "expense", "Комиссии", "Подрядчик", "1", "комиссия сети withdrawal:22")
	journalRow(t, s, ctx, 102, "2026-09-05", "expense", "Инструменты", "Сервис", "5", "card:44 и card:44:delta")
	journalRow(t, s, ctx, 103, "2026-09-06", "expense", "Инструменты", "Сервис", "6", "ключа нет: card:999")

	tx, _ := s.RW.BeginTx(ctx, nil)
	defer tx.Rollback()
	rep, err := LinkNotes(ctx, tx, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Documents != 4 || rep.Keys != 5 || rep.Linked != 4 || rep.Missing != 1 {
		t.Fatalf("report %+v", rep)
	}
	n, err := supersedeLinked(ctx, tx, "test", now)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	// Repeat: the links are there already.
	if rep, err = LinkNotes(ctx, tx, now); err != nil || rep.Linked != 0 {
		t.Fatal(rep, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND how = 'note_key' AND part = ''`, w); got != 2 {
		t.Fatalf("withdrawal links %d", got)
	}
	if got := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND part = 'delta' AND document_num = 102`, card); got != 1 {
		t.Fatalf("card delta links %d", got)
	}
	var status, decision, text string
	if err := s.RW.QueryRow(`SELECT status, decision, decision_text FROM proposals WHERE key = 'withdrawal:22'`).Scan(&status, &decision, &text); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || decision != "linked" || text != "note_key:recorded #100, note_key:recorded #101" {
		t.Fatalf("proposal %s %s %s", status, decision, text)
	}
	// The other one stays open and is listed.
	rows, err := Unlinked(ctx, s.RW, "2026-08-01")
	if err != nil || len(rows) != 1 || rows[0].Key != "deposit:33" || rows[0].OpenProposal == 0 || rows[0].Amount != "50" {
		t.Fatal(rows, err)
	}
}

func TestUnlinkedShowsCandidates(t *testing.T) {
	s, ctx := openDB(t)
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "address", "Tto", "100", "1", "2026-09-03T10:00:00.000+03:00"))
	journalRow(t, s, ctx, 1, "2026-09-04", "expense", "Подрядчик", "Подрядчик", "100.00", "та же сумма, день спустя")
	journalRow(t, s, ctx, 2, "2026-09-02", "transfer", "Внутренний перевод", "Карты", "100", "перевод тоже подходит")
	journalRow(t, s, ctx, 3, "2026-09-06", "expense", "Подрядчик", "Подрядчик", "100", "другой день")
	journalRow(t, s, ctx, 4, "2026-09-03", "expense", "Подрядчик", "Подрядчик", "100.01", "другая сумма")
	journalRow(t, s, ctx, 5, "2026-09-03", "income", "Custom", "Фан", "100", "другое направление")
	rows, err := Unlinked(ctx, s.RO, "2026-08-01")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if fmt.Sprint(rows[0].Candidates) != "[1 2]" {
		t.Fatalf("candidates %v", rows[0].Candidates)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links`); n != 0 {
		t.Fatal("a candidate must not become a link")
	}
}

func TestManualLinks(t *testing.T) {
	s, ctx := openDB(t)
	now := at("2026-10-01T10:00:00+03:00")
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "address", "Tto", "100", "1", "2026-09-03T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("deposit", "2", "income", "address", "Tfrom", "50", "", "2026-09-04T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("deposit", "3", "income", "address", "Tfrom", "51", "", "2026-07-04T10:00:00.000+03:00"))
	classifyAll(t, s, ctx, "shadow")
	journalRow(t, s, ctx, 7, "2026-09-03", "expense", "Подрядчик", "Подрядчик", "100", "без ключа")

	res, err := LinkManual(ctx, s.RW, "withdrawal:1", []int{7, 8}, "", "сверено вручную", "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked != 2 || res.Superseded != 1 || len(res.UnknownDoc) != 1 || res.UnknownDoc[0] != 8 {
		t.Fatalf("result %+v", res)
	}
	if res, err = LinkManual(ctx, s.RW, "deposit:2", nil, "personal", "личное", "agent", now); err != nil || res.Linked != 1 || res.Superseded != 1 {
		t.Fatal(res, err)
	}
	if rows, err := Unlinked(ctx, s.RW, "2026-08-01"); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if rows, err := Unlinked(ctx, s.RW, "2026-07-01"); err != nil || len(rows) != 1 || rows[0].Key != "deposit:3" {
		t.Fatal(rows, err)
	}
	for _, c := range []struct {
		key      string
		docs     []int
		decision string
	}{{"deposit:999", []int{1}, ""}, {"nonsense", []int{1}, ""}, {"deposit:3", nil, ""}, {"deposit:3", nil, "confirm"}, {"deposit:3", []int{1}, "maybe"}} {
		if _, err := LinkManual(ctx, s.RW, c.key, c.docs, c.decision, "", "agent", now); err == nil {
			t.Fatalf("%+v accepted", c)
		}
	}
	// A second identical call changes nothing.
	if res, err = LinkManual(ctx, s.RW, "withdrawal:1", []int{7}, "", "", "agent", now.Add(time.Hour)); err != nil || res.Linked != 0 {
		t.Fatal(res, err)
	}
}
