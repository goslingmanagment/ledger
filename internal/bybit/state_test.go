package bybit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The tables of ledger-sync's store.py that the import reads.
const stateSchema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE seen (key TEXT PRIMARY KEY, time_ms INTEGER NOT NULL);
CREATE TABLE deferred (key TEXT PRIMARY KEY, tx_json TEXT NOT NULL, deferred_ms INTEGER NOT NULL);
CREATE TABLE history (key TEXT PRIMARY KEY, time_ms INTEGER, date TEXT, direction TEXT, coin TEXT, amount TEXT,
  counterparty_kind TEXT, counterparty TEXT, first_seen_ms INTEGER, fee TEXT);
CREATE TABLE brief_items (brief_id INTEGER NOT NULL, num INTEGER NOT NULL, key TEXT NOT NULL, block TEXT NOT NULL, summary TEXT NOT NULL,
  tx_json TEXT NOT NULL, asked_count INTEGER NOT NULL, first_asked_ms INTEGER, ledger_match TEXT, decision TEXT, decision_text TEXT,
  decided_ms INTEGER, ledger_rows TEXT, applied_ms INTEGER, PRIMARY KEY (brief_id, num));
CREATE TABLE funding_rows (id TEXT PRIMARY KEY, time_ms INTEGER NOT NULL, coin TEXT, signed TEXT, after TEXT, type TEXT, desc TEXT, first_seen_ms INTEGER);
CREATE TABLE pending_withdrawals (key TEXT PRIMARY KEY, first_seen_ms INTEGER NOT NULL, last_status TEXT, tx_json TEXT NOT NULL, resolved_ms INTEGER, resolution TEXT);
CREATE TABLE card_events (msg_id INTEGER PRIMARY KEY, ts_ms INTEGER NOT NULL, kind TEXT NOT NULL, card TEXT, amount TEXT, merchant TEXT, balance_after TEXT,
  raw TEXT NOT NULL, delta TEXT, bybit_key TEXT, status TEXT NOT NULL DEFAULT 'new', delta_status TEXT);
`

type stateBuilder struct {
	t  *testing.T
	db *sql.DB
}

func newState(t *testing.T) (*stateBuilder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bybit.sqlite")
	st, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Exec(stateSchema); err != nil {
		t.Fatal(err)
	}
	return &stateBuilder{t: t, db: st}, path
}

func (b *stateBuilder) exec(q string, args ...any) {
	b.t.Helper()
	if _, err := b.db.Exec(q, args...); err != nil {
		b.t.Fatal(q, err)
	}
}

func msi(s string) int64 { n, _ := strconv.ParseInt(ms(s), 10, 64); return n }

func (b *stateBuilder) history(key, when, direction, amount, kind, cp, fee string) {
	b.t.Helper()
	b.exec(`INSERT INTO history(key, time_ms, date, direction, coin, amount, counterparty_kind, counterparty, first_seen_ms, fee)
		VALUES (?, ?, ?, ?, 'USDT', ?, ?, ?, ?, ?)`, key, msi(when), when[:10], direction, amount, kind, cp, msi(when), fee)
}

func (b *stateBuilder) item(brief, num int, key, block, decision, text, rows string, tx map[string]any) {
	b.t.Helper()
	tx["key"] = key
	raw, _ := json.Marshal(tx)
	var d, dt, r any
	if decision != "" {
		d = decision
	}
	if text != "" {
		dt = text
	}
	if rows != "" {
		r = rows
	}
	b.exec(`INSERT INTO brief_items(brief_id, num, key, block, summary, tx_json, asked_count, decision, decision_text, ledger_rows)
		VALUES (?, ?, ?, ?, 'сводка', ?, 1, ?, ?, ?)`, brief, num, key, block, string(raw), d, dt, r)
}

func TestImportState(t *testing.T) {
	s, ctx := openDB(t)
	now := at("2026-11-04T12:00:00+03:00")
	b, path := newState(t)

	wm := msi("2026-11-03T09:00:00+03:00")
	b.exec(`INSERT INTO meta VALUES ('last_sync_ms', ?)`, strconv.FormatInt(wm, 10))
	// A mark with one row, a mark with two rows (with fee) and «personal» with a row.
	b.history("withdrawal:1", "2026-09-01T10:00:00+03:00", "expense", "100", "address", "Taddr1", "1")
	b.item(5, 1, "withdrawal:1", "agency", "confirm", "", "387", map[string]any{"method": "USDT TRC-20", "txid": "tx1", "fee": "1"})
	b.history("withdrawal:2", "2026-09-02T10:00:00+03:00", "expense", "200", "address", "Taddr2", "")
	b.item(5, 2, "withdrawal:2", "agency", "recorded", "", "388, 389", map[string]any{"method": "USDT ETH", "fee": "2"})
	b.history("p2p:3", "2026-09-03T10:00:00+03:00", "expense", "300", "p2p", "buyer", "")
	b.item(5, 3, "p2p:3", "p2p", "custom", "текст решения", "390", map[string]any{"method": "P2P (Bybit)", "fiat": "30000 RUB", "price": "100"})
	b.history("internal_deposit:4", "2026-09-04T10:00:00+03:00", "income", "400", "uid", "1001", "")
	b.item(5, 4, "internal_deposit:4", "unknown", "personal", "", "391", map[string]any{"method": "Internal Transfer (Bybit)"})
	// Deferred once, then decided in a later brief.
	b.history("deposit:5", "2026-09-05T10:00:00+03:00", "income", "500", "address", "Tfrom5", "")
	b.item(5, 5, "deposit:5", "unknown", "defer", "", "", map[string]any{"method": "USDT TRC-20"})
	b.item(6, 1, "deposit:5", "unknown", "confirm", "", "392", map[string]any{"method": "USDT TRC-20"})
	// Personal with «-» is decided; a key in seen with no mark is closed.
	b.history("deposit:6", "2026-09-06T10:00:00+03:00", "income", "600", "address", "Tfrom6", "")
	b.item(6, 2, "deposit:6", "unknown", "personal", "", "-", map[string]any{"method": "USDT TRC-20"})
	b.history("withdrawal:7", "2026-09-07T10:00:00+03:00", "expense", "700", "uid", "2002", "")
	b.exec(`INSERT INTO seen VALUES ('withdrawal:7', ?)`, msi("2026-09-07T10:00:00+03:00"))
	// Pruned from seen long ago: only the watermark says it was reviewed.
	b.history("withdrawal:8", "2026-08-20T10:00:00+03:00", "expense", "800", "address", "Taddr8", "")
	// Still open in ledger-sync: deferred, undecided in the last brief, succeeded but not reviewed.
	b.history("withdrawal:9", "2026-10-30T10:00:00+03:00", "expense", "900", "address", "Taddr9", "1")
	b.exec(`INSERT INTO deferred VALUES ('withdrawal:9', '{}', 1)`)
	b.history("deposit:10", "2026-10-31T10:00:00+03:00", "income", "1000", "address", "Tfrom10", "")
	b.item(7, 1, "deposit:10", "unknown", "", "", "", map[string]any{"method": "USDT TRC-20"})
	b.exec(`INSERT INTO pending_withdrawals VALUES ('withdrawal:11', ?, 'success', ?, NULL, NULL)`, wm,
		`{"key":"withdrawal:11","direction":"expense","time_ms":`+strconv.FormatInt(msi("2026-11-01T10:00:00+03:00"), 10)+`,"coin":"USDT","amount":"11","fee":"1","counterparty_kind":"address","counterparty":"Taddr11","method":"USDT TRC-20"}`)
	// After the watermark: ledger-sync has not reviewed it yet.
	b.history("deposit:12", "2026-11-03T10:00:00+03:00", "income", "1200", "address", "Tfrom12", "")
	// Hanging withdrawal, handed over to the collector; a resolved one stays behind.
	b.exec(`INSERT INTO pending_withdrawals VALUES ('withdrawal:13', ?, 'SecurityCheck', ?, NULL, NULL)`, wm,
		`{"key":"withdrawal:13","direction":"expense","time_ms":`+strconv.FormatInt(msi("2026-11-02T10:00:00+03:00"), 10)+`,"coin":"USDT","amount":"13","fee":"1","counterparty_kind":"address","counterparty":"Taddr13","method":"USDT TRC-20"}`)
	b.exec(`INSERT INTO pending_withdrawals VALUES ('withdrawal:14', ?, 'success', '{"key":"withdrawal:14"}', 5, 'closed')`, wm)
	// Before the collection start: event yes, proposal no.
	b.history("deposit:15", "2026-07-10T10:00:00+03:00", "income", "1500", "address", "Tfrom15", "")
	// Funding rows.
	b.exec(`INSERT INTO funding_rows VALUES ('f1', ?, 'USDT', '-101', '899', 'Withdraw', 'synthetic', 1)`, msi("2026-09-01T10:00:01+03:00"))
	b.exec(`INSERT INTO funding_rows VALUES ('f2', ?, 'USDT', '500', '1399', 'Deposit', 'synthetic', 1)`, msi("2026-09-05T10:00:01+03:00"))
	// Cards: marks with a row, «-», the delta part, closed without a mark, service events, not closed.
	card := func(id int, kind, status, deltaStatus string) {
		b.exec(`INSERT INTO card_events(msg_id, ts_ms, kind, card, amount, merchant, balance_after, raw, delta, bybit_key, status, delta_status)
			VALUES (?, ?, ?, '1234', '10', 'Магазин', '90', 'текст бота', NULL, NULL, ?, ?)`, id, msi("2026-09-10T10:00:00+03:00")+int64(id), kind, status,
			map[bool]any{true: deltaStatus, false: nil}[deltaStatus != ""])
	}
	card(100, "purchase", "closed", "closed")
	b.item(5, 6, "card:100", "cards", "confirm", "", "400", map[string]any{})
	b.item(5, 7, "card:100:delta", "cards", "confirm", "", "401", map[string]any{})
	card(101, "load", "closed", "")
	b.item(5, 8, "card:101", "cards", "custom", "только комиссия", "-", map[string]any{})
	card(102, "purchase", "closed", "closed") // closed, no marks at all
	card(103, "wallet_anchor", "skipped", "")
	card(104, "purchase", "new", "")
	// A mark whose event does not exist and a garbled mark are counted, not fatal.
	b.item(5, 9, "card:999", "cards", "confirm", "", "402", map[string]any{})
	b.history("withdrawal:16", "2026-09-08T10:00:00+03:00", "expense", "16", "address", "Taddr16", "")
	b.item(5, 10, "withdrawal:16", "agency", "confirm", "", "#403 и ещё", map[string]any{})
	b.exec(`INSERT INTO seen VALUES ('withdrawal:16', ?)`, msi("2026-09-08T10:00:00+03:00"))

	// The collector ran first: an open proposal for a key the import closes, and one it must keep.
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "address", "Taddr1", "100", "1", "2026-09-01T10:00:00.000+03:00"))
	classifyAll(t, s, ctx, "shadow")

	source, err := OpenState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	run := func() StateReport {
		t.Helper()
		tx, err := s.RW.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		rep, err := ImportState(ctx, tx, source, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	rep := run()

	want := StateReport{Watermark: "2026-11-03T09:00:00.000+03:00", MovementsNew: 13, MovementsKnown: 1, FundingNew: 2, CardsNew: 5, PendingOpen: 1,
		MarkRows: 8, MarkNoRow: 2, Closed: 4, CardsClosed: 2, CardsIgnored: 1, MissingEvents: 1, BadMarks: 1, Unresolved: 3, Superseded: 1}
	want.NoteLinks = rep.NoteLinks
	if rep != want {
		t.Fatalf("report\n got %+v\nwant %+v", rep, want)
	}

	// Enrichment from the brief item: txid, method (normalized to the journal name), fiat.
	var payload string
	if err := s.RW.QueryRow(`SELECT payload FROM events WHERE source = 'bybit' AND source_id = 'withdrawal:2'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var m Movement
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatal(err)
	}
	if m.Method != "USDT ERC-20" || m.Fee != "2" || m.Imported != "history" || m.Date != "2026-09-02" {
		t.Fatalf("movement %+v", m)
	}
	if err := s.RW.QueryRow(`SELECT payload FROM events WHERE source = 'bybit' AND source_id = 'p2p:3'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(payload), &m)
	if m.Fiat != "30000 RUB" || m.Price != "100" || m.Kind != "p2p" {
		t.Fatalf("p2p %+v", m)
	}
	if count(t, s, `SELECT count(*) FROM events WHERE source = 'bybit' AND kind = 'funding' AND source_id = 'funding:f1' AND occurred_at = '2026-09-01T10:00:01+03:00'`) != 1 {
		t.Fatal("funding event")
	}
	if err := s.RW.QueryRow(`SELECT payload FROM events WHERE source = 'cincin_bot' AND source_id = '100' AND kind = 'purchase'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"raw":"текст бота"`) || !strings.Contains(payload, `"merchant":"Магазин"`) || !strings.Contains(payload, `"ledger_sync":{`) {
		t.Fatalf("card payload %s", payload)
	}

	// Links: one per row, «-» with the decision, brief reference and the owner's text kept.
	links := func(key string) string {
		t.Helper()
		src, id := "bybit", key
		if strings.HasPrefix(key, "card:") {
			src, id = "cincin_bot", strings.Split(key, ":")[1]
		}
		rows, err := s.RW.Query(`SELECT l.part, COALESCE(l.document_num, 0), l.how, l.decision, l.brief_ref FROM event_links l JOIN events e ON e.id = l.event_id
			WHERE e.source = ? AND e.source_id = ? ORDER BY l.part, l.document_num`, src, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var part, how, decision, brief string
			var doc int
			if err := rows.Scan(&part, &doc, &how, &decision, &brief); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s|%d|%s|%s|%s", part, doc, how, decision, brief))
		}
		return strings.Join(out, " ")
	}
	for key, want := range map[string]string{
		"withdrawal:1":  "|387|mark|confirm|ledger-sync:5/1",
		"withdrawal:2":  "|388|mark|recorded|ledger-sync:5/2 |389|mark|recorded|ledger-sync:5/2",
		"deposit:5":     "|392|mark|confirm|ledger-sync:6/1",
		"deposit:6":     "|0|mark|personal|ledger-sync:6/2",
		"withdrawal:7":  "|0|seen|closed|ledger-sync",
		"withdrawal:8":  "|0|seen|closed|ledger-sync",
		"withdrawal:9":  "",
		"deposit:10":    "",
		"withdrawal:11": "",
		"deposit:12":    "",
		"card:100":      "|400|mark|confirm|ledger-sync:5/6 delta|401|mark|confirm|ledger-sync:5/7",
		"card:101":      "|0|mark|custom|ledger-sync:5/8",
		"card:102":      "|0|seen|closed|ledger-sync delta|0|seen|closed|ledger-sync",
		"card:103":      "|0|seen|ignore|ledger-sync",
		"card:104":      "",
	} {
		if got := links(key); got != want {
			t.Fatalf("%s: got %q want %q", key, got, want)
		}
	}
	var note string
	if err := s.RW.QueryRow(`SELECT l.note FROM event_links l JOIN events e ON e.id = l.event_id WHERE e.source_id = 'p2p:3'`).Scan(&note); err != nil || note != "текст решения" {
		t.Fatal(note, err)
	}

	// The proposal of the key closed in ledger-sync is gone from the brief.
	var status, decided string
	if err := s.RW.QueryRow(`SELECT status, decided_by FROM proposals WHERE key = 'withdrawal:1'`).Scan(&status, &decided); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || decided != "import:ledger-sync" {
		t.Fatal(status, decided)
	}
	// Hanging withdrawal goes on with the collector.
	if count(t, s, `SELECT count(*) FROM bybit_pending WHERE key = 'withdrawal:13' AND last_status = 'SecurityCheck' AND resolved_at IS NULL`) != 1 {
		t.Fatal("hanging withdrawal not handed over")
	}
	if count(t, s, `SELECT count(*) FROM bybit_pending WHERE key = 'withdrawal:14'`) != 0 {
		t.Fatal("a resolved withdrawal came over")
	}

	// Only what ledger-sync left open gets a proposal; everything older stays silent.
	cr := classifyAll(t, s, ctx, "shadow")
	if cr.Unknown != 4 {
		t.Fatalf("classification after import %+v", cr)
	}
	for _, key := range []string{"withdrawal:9", "deposit:10", "withdrawal:11", "deposit:12"} {
		if count(t, s, `SELECT count(*) FROM proposals WHERE key = ? AND status = 'open'`, key) != 1 {
			t.Fatalf("%s must be proposed", key)
		}
	}
	for _, key := range []string{"withdrawal:7", "withdrawal:8", "deposit:15", "withdrawal:2"} {
		if count(t, s, `SELECT count(*) FROM proposals WHERE key = ?`, key) != 0 {
			t.Fatalf("%s must not come back", key)
		}
	}

	// Exceptions of the check: closed without a known row, and what is open. «personal» is decided,
	// marks with rows are linked.
	rows, err := Unlinked(ctx, s.RW, "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	if got := strings.Join(keys, ","); got != "withdrawal:8,withdrawal:7,withdrawal:16,withdrawal:9,deposit:10,withdrawal:11,deposit:12" {
		t.Fatalf("unlinked %s", got)
	}
	if rows[0].Links != "seen:closed" || rows[3].OpenProposal == 0 {
		t.Fatalf("rows %+v", rows[:4])
	}

	// Repeat: nothing new.
	rep = run()
	if rep.MovementsNew != 0 || rep.MovementsKnown != 14 || rep.FundingNew != 0 || rep.FundingKnown != 2 || rep.CardsNew != 0 || rep.CardsKnown != 5 ||
		rep.MarkRows != 0 || rep.MarkNoRow != 0 || rep.Closed != 0 || rep.CardsClosed != 0 || rep.CardsIgnored != 0 || rep.PendingOpen != 0 || rep.Superseded != 0 {
		t.Fatalf("second import %+v", rep)
	}

	// A journal row with the key in its note closes the exception; the collector does it each pass.
	journalRow(t, s, ctx, 500, "2026-08-20", "expense", "Подрядчик", "Подрядчик", "800", "оплата withdrawal:8")
	rep = run()
	if rep.NoteLinks.Linked != 1 {
		t.Fatalf("note links %+v", rep.NoteLinks)
	}
	if rows, err = Unlinked(ctx, s.RW, "2026-08-01"); err != nil || len(rows) != 6 {
		t.Fatal(rows, err)
	}
}

func TestOpenStateReadsWALFileWithoutShm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "bybit.sqlite")
	st, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Exec(stateSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Exec(`INSERT INTO meta VALUES ('last_sync_ms', '1')`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	before, _ := os.ReadFile(path)
	// A read-only directory: nowhere to put the -shm, so only immutable can read the file.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	source, err := OpenState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := source.QueryRowContext(ctx, `SELECT value FROM meta`).Scan(&v); err != nil || v != "1" {
		t.Fatal(v, err)
	}
	if _, err := source.ExecContext(ctx, `INSERT INTO meta VALUES ('x', 'y')`); err == nil {
		t.Fatal("the state file accepted a write")
	}
	source.Close()
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("the state file changed")
	}
	if _, err := OpenState(ctx, filepath.Join(dir, "absent.sqlite")); err == nil {
		t.Fatal("absent file opened")
	}
}
