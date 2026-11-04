package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func open(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func at(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func pay(amount string) []Line {
	v := money.MustParse(amount)
	return []Line{{Account: "expense:tools", Amount: v}, {Account: "bybit", Amount: -v}}
}

func TestUnbalancedEntryIsRejected(t *testing.T) {
	s, ctx := open(t)
	_, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-03", Kind: "manual", Author: "test", Lines: []Line{
		{Account: "expense:tools", Amount: money.MustParse("10.00")},
		{Account: "bybit", Amount: money.MustParse("-9.99")},
	}}, at("2026-09-03"))
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("want ErrUnbalanced, got %v", err)
	}
	var n int
	s.RW.QueryRow(`SELECT count(*) FROM entries`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d entries written", n)
	}
	if _, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-03", Kind: "manual", Author: "test",
		Lines: []Line{{Account: "nonsense", Amount: 1}, {Account: "bybit", Amount: -1}}}, at("2026-09-03")); err == nil {
		t.Fatal("unknown account accepted")
	}
}

func TestEventRepeatIsIdempotent(t *testing.T) {
	s, ctx := open(t)
	e := Event{Source: "bybit", SourceID: "deposit:1", Kind: "deposit", OccurredAt: "2026-09-03T10:00:00+03:00",
		Payload: map[string]any{"amount": "100", "coin": "USDT"}}
	id1, ins1, err := AddEvent(ctx, s.RW, e, at("2026-09-03"))
	if err != nil || !ins1 {
		t.Fatal(err, ins1)
	}
	e.Payload = json.RawMessage(`{"coin":"USDT","amount":"100"}`) // same fact, other key order
	id2, ins2, err := AddEvent(ctx, s.RW, e, at("2026-09-04"))
	if err != nil || ins2 || id2 != id1 {
		t.Fatalf("repeat: id %d/%d inserted %v err %v", id1, id2, ins2, err)
	}
	e.Payload = map[string]any{"amount": "101", "coin": "USDT"}
	if _, _, err := AddEvent(ctx, s.RW, e, at("2026-09-04")); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	if _, err := s.RW.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("events can be deleted")
	}
}

func TestReversalKeepsOriginal(t *testing.T) {
	s, ctx := open(t)
	id, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-03", Kind: "manual", Author: "test", Lines: pay("50.00")}, at("2026-09-03"))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := Reverse(ctx, s.RW, id, Reversal{Author: "test"}, at("2026-09-10"))
	if err != nil {
		t.Fatal(err)
	}
	var orig int64
	s.RW.QueryRow(`SELECT amount FROM lines WHERE entry_id = ? AND account = 'bybit'`, id).Scan(&orig)
	if orig != -50_000_000 {
		t.Fatalf("original line changed: %d", orig)
	}
	if b, _ := Balance(ctx, s.RW, "bybit", "", ""); b != 0 {
		t.Fatalf("balance after reversal %s", b)
	}
	if _, err := Reverse(ctx, s.RW, id, Reversal{Author: "test"}, at("2026-09-11")); !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("second reversal: %v", err)
	}
	var effective string
	s.RW.QueryRow(`SELECT effective_at FROM entries WHERE id = ?`, rev).Scan(&effective)
	if effective != "2026-09-03" {
		t.Fatalf("reversal effective %s, want the original date", effective)
	}
	for _, q := range []string{`UPDATE lines SET amount = 1`, `DELETE FROM entries`, `UPDATE entries SET memo = 'x'`} {
		if _, err := s.RW.Exec(q); err == nil {
			t.Fatalf("%s succeeded", q)
		}
	}
}

func TestSourceKeyIsIdempotent(t *testing.T) {
	s, ctx := open(t)
	e := Entry{EffectiveAt: "2026-09-03", Kind: "accrual", Author: "test", SourceKey: "k1", Lines: pay("5.00")}
	a, err := Post(ctx, s.RW, e, at("2026-09-03"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Post(ctx, s.RW, e, at("2026-09-04"))
	if err != nil || a != b {
		t.Fatalf("repeat gave %d/%d %v", a, b, err)
	}
	e.Lines = pay("6.00")
	if _, err := Post(ctx, s.RW, e, at("2026-09-04")); !errors.Is(err, ErrSourceKey) {
		t.Fatalf("other lines under the same key: %v", err)
	}
}

func TestAtCloseIgnoresLaterCorrections(t *testing.T) {
	s, ctx := open(t)
	id, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-20", Kind: "manual", Author: "test", Lines: pay("100.00")},
		at("2026-09-20T12:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	snap := json.RawMessage(`{"decision": 1}`)
	if err := Lock(ctx, s.RW, Period{Month: "2026-09", LockedAt: "2026-10-14T19:22:53+03:00",
		AsOf: "2026-10-14T19:22:38+03:00", Snapshot: snap, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := Lock(ctx, s.RW, Period{Month: "2026-09", LockedAt: "x", AsOf: "x", Snapshot: snap, Source: "t"}); !errors.Is(err, ErrPeriodLocked) {
		t.Fatalf("second lock: %v", err)
	}
	// A dispute after the close: the 100 was really 80.
	if _, err := Reverse(ctx, s.RW, id, Reversal{Author: "owner"}, at("2026-10-20T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if _, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-20", Kind: "correction", Author: "owner", Lines: pay("80.00")},
		at("2026-10-20T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	now, atClose, err := MonthTotals(ctx, s.RW, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if now["expense:tools"] != money.MustParse("80.00") || atClose["expense:tools"] != money.MustParse("100.00") {
		t.Fatalf("now %s, at close %s", now["expense:tools"], atClose["expense:tools"])
	}
	if _, err := s.RW.Exec(`UPDATE periods SET snapshot = '{}'`); err == nil {
		t.Fatal("closed month changed")
	}
	if _, err := GetPeriod(ctx, s.RW, "2026-10"); err != sql.ErrNoRows {
		t.Fatalf("open month: %v", err)
	}
}

func TestDocumentVersions(t *testing.T) {
	s, ctx := open(t)
	d := Document{Num: 7, Date: "2026-09-02", Type: "expense", Category: "Инструменты", Counterparty: "Сервис",
		Model: "—", Amount: "10.00", Currency: "USDT", Method: "USDT TRC-20", Status: "paid", Note: "первый текст",
		Author: "test", Origin: "test", RecordedAt: "2026-09-02T10:00:00+03:00"}
	if _, changed, err := PutDocument(ctx, s.RW, d, at("2026-09-02")); err != nil || !changed {
		t.Fatal(err)
	}
	if _, changed, _ := PutDocument(ctx, s.RW, d, at("2026-09-03")); changed {
		t.Fatal("identical row made a new version")
	}
	d.Note, d.RecordedAt = "текст с тегом {предоплата 2026-09..2026-11}", "2026-10-15T10:00:00+03:00"
	v2, changed, err := PutDocument(ctx, s.RW, d, at("2026-10-15"))
	if err != nil || !changed || v2.Version != 2 {
		t.Fatal(err, changed, v2.Version)
	}
	old, _ := DocumentsAsOf(ctx, s.RW, "2026-10-14T19:22:53+03:00")
	cur, _ := DocumentsAsOf(ctx, s.RW, "")
	if len(old) != 1 || old[0].Note != "первый текст" || len(cur) != 1 || cur[0].Version != 2 {
		t.Fatalf("as of close %+v, now %+v", old, cur)
	}
	if none, _ := DocumentsAsOf(ctx, s.RW, "2026-09-01T00:00:00+03:00"); len(none) != 0 {
		t.Fatalf("row visible before it was recorded: %+v", none)
	}
}

// What reaches the books after a close stays out of «на закрытии», whatever time its source claims.
func TestEntryAfterCloseIsRecordedAfterIt(t *testing.T) {
	s, ctx := open(t)
	locked := time.Date(2026, 10, 14, 12, 0, 0, 0, ts.MSK)
	line := func(v string) []Line {
		return []Line{{Account: "expense:tools", Amount: money.MustParse(v)}, {Account: "bybit", Amount: -money.MustParse(v)}}
	}
	if _, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-10", Kind: "document", Author: "t", Lines: line("100.00")}, locked.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Lock(ctx, s.RW, Period{Month: "2026-09", LockedAt: ts.Format(locked), AsOf: ts.Format(locked), Snapshot: []byte(`{}`), Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-11", Kind: "accrual", Author: "t", Lines: line("7.00")}, locked.Add(-time.Minute)); err != nil {
		t.Fatal(err) // fetched before the close, delivered after it
	}
	if _, err := Post(ctx, s.RW, Entry{EffectiveAt: "2026-09-12", Kind: "document", Author: "t", Lines: line("3.00")}, locked.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err) // the same second as the close
	}
	now, atClose, err := MonthTotals(ctx, s.RW, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if atClose["expense:tools"] != money.MustParse("100.00") || now["expense:tools"] != money.MustParse("110.00") {
		t.Fatalf("at close %s, now %s", atClose["expense:tools"], now["expense:tools"])
	}
}
