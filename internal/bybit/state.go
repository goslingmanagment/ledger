package bybit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"

	_ "modernc.org/sqlite" // the state file is opened here, on its own connection
)

// OpenState opens the ledger-sync database without writing to it. A WAL-mode file with no -shm next to
// it cannot be opened with mode=ro alone (SQLite would have to create the -shm); when the WAL holds
// nothing the main file is complete, so immutable=1 reads it. A non-empty WAL we cannot read is a refusal.
func OpenState(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	open := func(extra string) (*sql.DB, error) {
		q := url.Values{}
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
		dsn := "file:" + path + "?" + q.Encode() + extra
		st, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		var n int
		if err := st.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
			st.Close()
			return nil, err
		}
		return st, nil
	}
	st, err := open("")
	if err == nil {
		return st, nil
	}
	if info, serr := os.Stat(path + "-wal"); serr == nil && info.Size() > 0 {
		return nil, fmt.Errorf("%s: не открывается только на чтение, а в WAL есть данные: %w", path, err)
	}
	st, ierr := open("&immutable=1")
	if ierr != nil {
		return nil, fmt.Errorf("%s: %w", path, ierr)
	}
	return st, nil
}

// StateReport counts what ImportState did.
type StateReport struct {
	Watermark      string    `json:"ledger_sync_watermark,omitempty"`
	MovementsNew   int       `json:"movements_new"`
	MovementsKnown int       `json:"movements_known"`
	Mismatches     int       `json:"movement_mismatches"`
	FundingNew     int       `json:"funding_new"`
	FundingKnown   int       `json:"funding_known"`
	CardsNew       int       `json:"cards_new"`
	CardsKnown     int       `json:"cards_known"`
	PendingOpen    int       `json:"pending_open"`
	MarkRows       int       `json:"links_mark_rows"`      // a mark with a journal row
	MarkNoRow      int       `json:"links_mark_no_row"`    // mark «-»: no row by decision
	Closed         int       `json:"links_closed"`         // closed in ledger-sync without a mark
	CardsClosed    int       `json:"links_cards_closed"`   // card events closed without a mark
	CardsIgnored   int       `json:"links_cards_ignored"`  // service events of the bot: no row needed
	LinksExisting  int       `json:"links_existing"`       // already linked by an earlier import
	MissingEvents  int       `json:"marks_without_event"`  // a brief item whose event ledger does not have
	BadMarks       int       `json:"marks_bad"`            // ledger_rows that is not numbers or «-»
	Unresolved     int       `json:"unresolved"`           // deferred or undecided in ledger-sync: left for proposals
	Superseded     int       `json:"proposals_superseded"` // open proposals closed by these links
	NoteLinks      NoteLinks `json:"note_links"`
}

type stateItem struct {
	brief, num     int64
	key            string
	decision, text string
	rows           string
	hasRows        bool
}

// stateTx is the tx_json of a brief item: what history does not keep.
type stateTx struct {
	Key              string `json:"key"`
	Direction        string `json:"direction"`
	TimeMs           int64  `json:"time_ms"`
	Coin             string `json:"coin"`
	Amount           string `json:"amount"`
	Fee              string `json:"fee"`
	CounterpartyKind string `json:"counterparty_kind"`
	Counterparty     string `json:"counterparty"`
	TxID             string `json:"txid"`
	Method           string `json:"method"`
	Fiat             string `json:"fiat"`
	Price            string `json:"price"`
}

func kindOf(key string) (string, bool) {
	k, _, ok := strings.Cut(key, ":")
	if !ok {
		return "", false
	}
	for _, m := range movementKinds {
		if k == m {
			return k, true
		}
	}
	return "", false
}

// normalizeMethod maps ledger-sync's "USDT ETH" to the journal's "USDT ERC-20".
func normalizeMethod(m string) string {
	if i := strings.LastIndexByte(m, ' '); i > 0 {
		if n, ok := chains[m[i+1:]]; ok {
			return m[:i+1] + n
		}
	}
	return m
}

func stateMovement(tx stateTx, imported string) (Movement, error) {
	kind, ok := kindOf(tx.Key)
	if !ok {
		return Movement{}, fmt.Errorf("ключ %q: не движение Bybit", tx.Key)
	}
	t := time.UnixMilli(tx.TimeMs)
	meth := normalizeMethod(tx.Method)
	if meth == "" {
		switch {
		case kind == "p2p":
			meth = "P2P (Bybit)"
		case tx.CounterpartyKind == "uid":
			meth = "Internal Transfer (Bybit)"
		default:
			meth = tx.Coin
		}
	}
	return Movement{Key: tx.Key, Kind: kind, Direction: tx.Direction, TxID: tx.TxID, TimeMs: tx.TimeMs, At: ts.FormatMilli(t),
		Date: ts.Date(t), Coin: tx.Coin, Amount: tx.Amount, Fee: tx.Fee, CounterpartyKind: tx.CounterpartyKind,
		Counterparty: tx.Counterparty, Method: meth, Fiat: tx.Fiat, Price: tx.Price, Imported: imported}, nil
}

// ImportState loads the state of ledger-sync into q: history and Funding rows as Bybit events, bot
// messages as cincin_bot events, marks of the briefs and closed keys as event links. Old movements
// never surface in the brief again; what ledger-sync left undecided stays open. Repeatable.
func ImportState(ctx context.Context, q ledger.Querier, st *sql.DB, now time.Time) (StateReport, error) {
	var rep StateReport

	// Everything from the source first: the write transaction holds no reads of another file.
	var wmMs int64
	var wm sql.NullString
	if err := st.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_sync_ms'`).Scan(&wm); err != nil && err != sql.ErrNoRows {
		return rep, fmt.Errorf("meta: %w", err)
	}
	if wm.Valid {
		wmMs, _ = strconv.ParseInt(wm.String, 10, 64)
	}
	if wmMs > 0 {
		rep.Watermark = ts.FormatMilli(time.UnixMilli(wmMs))
	}

	items, txByKey, err := readStateItems(ctx, st)
	if err != nil {
		return rep, err
	}
	seen, err := readStringSet(ctx, st, `SELECT key FROM seen`)
	if err != nil {
		return rep, fmt.Errorf("seen: %w", err)
	}
	unresolved, err := readStringSet(ctx, st, `SELECT key FROM deferred`)
	if err != nil {
		return rep, fmt.Errorf("deferred: %w", err)
	}
	extraTx, err := readExtraTx(ctx, st)
	if err != nil {
		return rep, err
	}
	for key := range extraTx {
		unresolved[key] = true
	}
	marked := map[string]bool{}
	latest := map[string]stateItem{}
	for _, it := range items {
		if it.hasRows {
			if _, ok := parseMark(it.rows); ok { // a garbled mark links nothing: the key is not marked
				marked[it.key] = true
			}
		}
		latest[it.key] = it // items come ordered by brief, the last one is the latest
	}
	for key, it := range latest {
		if !marked[key] && !seen[key] && (it.decision == "" || it.decision == "defer") {
			unresolved[key] = true
		}
	}

	// History → movements.
	historyTimes := map[string]int64{}
	rows, err := st.QueryContext(ctx, `SELECT key, time_ms, COALESCE(direction, ''), COALESCE(coin, ''), COALESCE(amount, ''),
		COALESCE(counterparty_kind, ''), COALESCE(counterparty, ''), COALESCE(fee, '') FROM history ORDER BY time_ms, key`)
	if err != nil {
		return rep, fmt.Errorf("history: %w", err)
	}
	var history []stateTx
	for rows.Next() {
		var h stateTx
		var t sql.NullInt64
		if err := rows.Scan(&h.Key, &t, &h.Direction, &h.Coin, &h.Amount, &h.CounterpartyKind, &h.Counterparty, &h.Fee); err != nil {
			rows.Close()
			return rep, fmt.Errorf("history: %w", err)
		}
		h.TimeMs = t.Int64
		history = append(history, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("history: %w", err)
	}
	store := func(tx stateTx, imported string) error {
		m, err := stateMovement(tx, imported)
		if err != nil {
			return err
		}
		ins, mismatch, err := storeMovement(ctx, q, m, now)
		if err != nil {
			return err
		}
		switch {
		case ins:
			rep.MovementsNew++
		default:
			rep.MovementsKnown++
		}
		if mismatch != "" {
			rep.Mismatches++
		}
		return nil
	}
	for _, h := range history {
		if _, ok := kindOf(h.Key); !ok {
			continue
		}
		historyTimes[h.Key] = h.TimeMs
		if tx, ok := txByKey[h.Key]; ok {
			h.TxID, h.Method, h.Fiat, h.Price = tx.TxID, tx.Method, tx.Fiat, tx.Price
			if h.Fee == "" {
				h.Fee = tx.Fee
			}
		}
		if err := store(h, "history"); err != nil {
			return rep, err
		}
	}
	for key, tx := range extraTx { // deferred and succeeded withdrawals not yet in history
		if _, ok := historyTimes[key]; !ok && tx.TimeMs > 0 {
			if _, isMovement := kindOf(key); isMovement {
				if err := store(tx, "deferred"); err != nil {
					return rep, err
				}
			}
		}
	}

	if err := importFunding(ctx, q, st, &rep, now); err != nil {
		return rep, err
	}
	cards, err := importCards(ctx, q, st, &rep, now)
	if err != nil {
		return rep, err
	}
	if err := importPending(ctx, q, st, &rep, now); err != nil {
		return rep, err
	}

	// Marks → one link per journal row, «-» → a link with the decision and no row.
	for _, it := range items {
		if !it.hasRows {
			continue
		}
		ref, ok := ParseKey(it.key)
		if !ok {
			rep.MissingEvents++
			continue
		}
		id, err := eventID(ctx, q, ref)
		if err != nil {
			return rep, err
		}
		if id == 0 {
			rep.MissingEvents++
			continue
		}
		docs, ok := parseMark(it.rows)
		if !ok {
			rep.BadMarks++
			continue
		}
		decision := it.decision
		if decision == "" {
			decision = "closed"
		}
		brief := fmt.Sprintf("ledger-sync:%d/%d", it.brief, it.num)
		for _, doc := range docs {
			ins, err := AddLink(ctx, q, Link{EventID: id, Part: ref.Part, DocumentNum: doc, How: "mark", Decision: decision,
				BriefRef: brief, Note: it.text}, now)
			if err != nil {
				return rep, err
			}
			switch {
			case !ins:
				rep.LinksExisting++
			case doc == 0:
				rep.MarkNoRow++
			default:
				rep.MarkRows++
			}
		}
	}

	// Closed without a mark: keys in seen, and pruned keys older than the watermark that ledger-sync
	// does not hold open. `seen` keeps only 45 days, the watermark covers everything before it.
	closeKey := func(key, note string) error {
		ref, ok := ParseKey(key)
		if !ok {
			return nil
		}
		id, err := eventID(ctx, q, ref)
		if err != nil || id == 0 {
			return err
		}
		ins, err := AddLink(ctx, q, Link{EventID: id, How: "seen", Decision: "closed", BriefRef: "ledger-sync", Note: note}, now)
		if err != nil {
			return err
		}
		if ins {
			rep.Closed++
		} else {
			rep.LinksExisting++
		}
		return nil
	}
	for key := range seen {
		if !marked[key] {
			if err := closeKey(key, "ключ в seen ledger-sync, отметки нет"); err != nil {
				return rep, err
			}
		}
	}
	for key, t := range historyTimes {
		if !marked[key] && !seen[key] && !unresolved[key] && wmMs > 0 && t < wmMs {
			if err := closeKey(key, "старше водяного знака ledger-sync, отметки нет"); err != nil {
				return rep, err
			}
		}
	}
	rep.Unresolved = len(unresolved)

	// Card events closed without a mark; service messages of the bot need no journal row.
	for _, c := range cards {
		ref := KeyRef{Source: "cincin_bot", SourceID: strconv.FormatInt(c.msgID, 10)}
		id, err := eventID(ctx, q, ref)
		if err != nil {
			return rep, err
		}
		if id == 0 {
			continue
		}
		key := "card:" + ref.SourceID
		switch {
		case c.status == "closed" && !marked[key]:
			ins, err := AddLink(ctx, q, Link{EventID: id, How: "seen", Decision: "closed", BriefRef: "ledger-sync", Note: "событие закрыто в ledger-sync, отметки нет"}, now)
			if err != nil {
				return rep, err
			}
			if ins {
				rep.CardsClosed++
			} else {
				rep.LinksExisting++
			}
		case c.status == "skipped" && !marked[key]:
			ins, err := AddLink(ctx, q, Link{EventID: id, How: "seen", Decision: "ignore", BriefRef: "ledger-sync", Note: "служебное событие бота (ledger-sync: skipped)"}, now)
			if err != nil {
				return rep, err
			}
			if ins {
				rep.CardsIgnored++
			} else {
				rep.LinksExisting++
			}
		}
		if c.deltaStatus == "closed" && !marked[key+":delta"] {
			ins, err := AddLink(ctx, q, Link{EventID: id, Part: "delta", How: "seen", Decision: "closed", BriefRef: "ledger-sync", Note: "скрытое списание закрыто в ledger-sync, отметки нет"}, now)
			if err != nil {
				return rep, err
			}
			if ins {
				rep.CardsClosed++
			} else {
				rep.LinksExisting++
			}
		}
	}

	if rep.NoteLinks, err = LinkNotes(ctx, q, now); err != nil {
		return rep, err
	}
	rep.Superseded, err = supersedeLinked(ctx, q, "import:ledger-sync", now)
	return rep, err
}

func readStateItems(ctx context.Context, st *sql.DB) ([]stateItem, map[string]stateTx, error) {
	rows, err := st.QueryContext(ctx, `SELECT brief_id, num, key, COALESCE(decision, ''), COALESCE(decision_text, ''),
		ledger_rows, tx_json FROM brief_items ORDER BY brief_id, num`)
	if err != nil {
		return nil, nil, fmt.Errorf("brief_items: %w", err)
	}
	defer rows.Close()
	var items []stateItem
	txs := map[string]stateTx{}
	for rows.Next() {
		var it stateItem
		var marks sql.NullString
		var raw string
		if err := rows.Scan(&it.brief, &it.num, &it.key, &it.decision, &it.text, &marks, &raw); err != nil {
			return nil, nil, fmt.Errorf("brief_items: %w", err)
		}
		it.rows, it.hasRows = marks.String, marks.Valid
		items = append(items, it)
		var tx stateTx
		if err := json.Unmarshal([]byte(raw), &tx); err == nil && tx.Key != "" {
			txs[it.key] = tx
		}
	}
	return items, txs, rows.Err()
}

func readStringSet(ctx context.Context, st *sql.DB, query string) (map[string]bool, error) {
	rows, err := st.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// readExtraTx returns the full records of what ledger-sync still holds open: deferred keys and
// withdrawals that succeeded but were not reviewed.
func readExtraTx(ctx context.Context, st *sql.DB) (map[string]stateTx, error) {
	out := map[string]stateTx{}
	for _, query := range []string{`SELECT key, tx_json FROM deferred`,
		`SELECT key, tx_json FROM pending_withdrawals WHERE resolved_ms IS NULL AND last_status = 'success'`} {
		rows, err := st.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var tx stateTx
			if err := json.Unmarshal([]byte(raw), &tx); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: %v", key, err)
			}
			tx.Key = key
			out[key] = tx
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseMark reads ledger_rows: "387", "388,389" or "-" (no row by decision, returned as 0).
func parseMark(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "-" {
		return []int{0}, true
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "#")))
		if err != nil || n <= 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

func importFunding(ctx context.Context, q ledger.Querier, st *sql.DB, rep *StateReport, now time.Time) error {
	rows, err := st.QueryContext(ctx, `SELECT id, time_ms, COALESCE(coin, ''), COALESCE(signed, ''), COALESCE(after, ''),
		COALESCE(type, ''), COALESCE("desc", '') FROM funding_rows ORDER BY time_ms, id`)
	if err != nil {
		return fmt.Errorf("funding_rows: %w", err)
	}
	var list []FundingRow
	for rows.Next() {
		var r FundingRow
		if err := rows.Scan(&r.ID, &r.TimeMs, &r.Coin, &r.Signed, &r.After, &r.Type, &r.Desc); err != nil {
			rows.Close()
			return fmt.Errorf("funding_rows: %w", err)
		}
		r.At = ts.Format(time.UnixMilli(r.TimeMs))
		r.Imported = "funding_rows"
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("funding_rows: %w", err)
	}
	for _, r := range list {
		_, ins, err := addEventOnce(ctx, q, r.Event(), now)
		if err != nil {
			return err
		}
		if ins {
			rep.FundingNew++
		} else {
			rep.FundingKnown++
		}
	}
	return nil
}

type stateCard struct {
	msgID       int64
	status      string
	deltaStatus string
}

// importCards stores bot messages as events. The processing state of ledger-sync is not a fact about
// the message: it goes under `ledger_sync`, a snapshot of the import.
func importCards(ctx context.Context, q ledger.Querier, st *sql.DB, rep *StateReport, now time.Time) ([]stateCard, error) {
	rows, err := st.QueryContext(ctx, `SELECT * FROM card_events ORDER BY ts_ms, msg_id`)
	if err != nil {
		return nil, fmt.Errorf("card_events: %w", err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, err
	}
	type row struct {
		card    stateCard
		kind    string
		tsMs    int64
		payload map[string]any
	}
	var list []row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("card_events: %w", err)
		}
		r := row{payload: map[string]any{}}
		state := map[string]any{}
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			switch c {
			case "status", "delta", "delta_status", "bybit_key":
				state[c] = v
				s, _ := v.(string)
				if c == "status" {
					r.card.status = s
				} else if c == "delta_status" {
					r.card.deltaStatus = s
				}
			case "extra":
				if s, ok := v.(string); ok && json.Valid([]byte(s)) {
					v = json.RawMessage(s)
				}
				r.payload[c] = v
			default:
				r.payload[c] = v
				switch c {
				case "msg_id":
					r.card.msgID, _ = v.(int64)
				case "ts_ms":
					r.tsMs, _ = v.(int64)
				case "kind":
					r.kind, _ = v.(string)
				}
			}
		}
		r.payload["at"] = ts.Format(time.UnixMilli(r.tsMs))
		r.payload["imported"] = "card_events"
		r.payload["ledger_sync"] = state
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card_events: %w", err)
	}
	cards := make([]stateCard, 0, len(list))
	for _, r := range list {
		_, ins, err := addEventOnce(ctx, q, ledger.Event{Source: "cincin_bot", SourceID: strconv.FormatInt(r.card.msgID, 10), Kind: r.kind,
			OccurredAt: ts.Format(time.UnixMilli(r.tsMs)), Payload: r.payload}, now)
		if err != nil {
			return nil, fmt.Errorf("card %d: %w", r.card.msgID, err)
		}
		if ins {
			rep.CardsNew++
		} else {
			rep.CardsKnown++
		}
		cards = append(cards, r.card)
	}
	return cards, nil
}

// importPending hands withdrawals still hanging in ledger-sync over to the collector's table.
func importPending(ctx context.Context, q ledger.Querier, st *sql.DB, rep *StateReport, now time.Time) error {
	rows, err := st.QueryContext(ctx, `SELECT key, first_seen_ms, COALESCE(last_status, ''), tx_json FROM pending_withdrawals
		WHERE resolved_ms IS NULL AND COALESCE(last_status, '') <> 'success'`)
	if err != nil {
		return fmt.Errorf("pending_withdrawals: %w", err)
	}
	type row struct {
		key, status, raw string
		first            int64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.first, &r.status, &r.raw); err != nil {
			rows.Close()
			return fmt.Errorf("pending_withdrawals: %w", err)
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pending_withdrawals: %w", err)
	}
	for _, r := range list {
		var tx stateTx
		if err := json.Unmarshal([]byte(r.raw), &tx); err != nil {
			return fmt.Errorf("%s: %v", r.key, err)
		}
		tx.Key = r.key
		m, err := stateMovement(tx, "pending_withdrawals")
		if err != nil {
			return err
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		res, err := q.ExecContext(ctx, `INSERT INTO bybit_pending(key, first_seen_at, last_status, checked_at, movement)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(key) DO NOTHING`, r.key, ts.Format(time.UnixMilli(r.first)), r.status, ts.Format(now), string(raw))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			rep.PendingOpen++
		}
	}
	return nil
}
