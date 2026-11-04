package bybit

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Link ties an event (or its part) to a journal row, or records a decision that needs no row.
type Link struct {
	EventID     int64
	Part        string // "" — the movement itself; "delta" — the hidden charge of a card event
	DocumentNum int    // 0 — no journal row
	How         string // mark | note_key | manual | seen | rule
	Decision    string // confirm | recorded | custom | personal | ignore | closed
	BriefRef    string
	Note        string
}

// AddLink stores l once (same event, part, row, how and brief); inserted is false for a repeat.
func AddLink(ctx context.Context, q ledger.Querier, l Link, now time.Time) (bool, error) {
	var doc any
	if l.DocumentNum > 0 {
		doc = l.DocumentNum
	}
	res, err := q.ExecContext(ctx, `INSERT INTO event_links(event_id, part, document_num, how, decision, brief_ref, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		l.EventID, l.Part, doc, l.How, l.Decision, l.BriefRef, l.Note, ts.Format(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// KeyRef is a movement key as ledger-sync and journal notes write it.
type KeyRef struct {
	Key      string // withdrawal:123, card:5:delta
	Source   string // bybit | cincin_bot
	SourceID string // withdrawal:123 | 5
	Part     string // "" | delta
}

var keyRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])((internal_deposit|withdrawal|deposit|p2p|card):(\d+)(:delta)?)\b`)

func keyRef(prefix, id, delta string) KeyRef {
	if prefix == "card" {
		r := KeyRef{Key: "card:" + id, Source: "cincin_bot", SourceID: id}
		if delta != "" {
			r.Key, r.Part = r.Key+":delta", "delta"
		}
		return r
	}
	return KeyRef{Key: prefix + ":" + id, Source: "bybit", SourceID: prefix + ":" + id}
}

// ParseKey reads one key exactly (deposit:1, internal_deposit:1, withdrawal:1, p2p:1, card:1, card:1:delta).
func ParseKey(s string) (KeyRef, bool) {
	s = strings.TrimSpace(s)
	m := keyRe.FindStringSubmatch(s)
	if m == nil || m[1] != s || (m[4] != "" && m[2] != "card") {
		return KeyRef{}, false
	}
	return keyRef(m[2], m[3], m[4]), true
}

// KeysIn finds every movement key in a journal note.
func KeysIn(note string) []KeyRef {
	var out []KeyRef
	seen := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(note, -1) {
		delta := m[4]
		if m[2] != "card" {
			delta = ""
		}
		r := keyRef(m[2], m[3], delta)
		if !seen[r.Key] {
			seen[r.Key] = true
			out = append(out, r)
		}
	}
	return out
}

func eventID(ctx context.Context, q ledger.Querier, r KeyRef) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM events WHERE source = ? AND source_id = ?`, r.Source, r.SourceID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// NoteLinks is what LinkNotes did.
type NoteLinks struct {
	Documents int `json:"documents"` // journal rows with a key in the note
	Keys      int `json:"keys"`
	Linked    int `json:"linked"`  // new links
	Missing   int `json:"missing"` // keys without an event (not collected or not imported yet)
}

// LinkNotes links events to journal rows whose note names the movement key ("withdrawal:259…").
func LinkNotes(ctx context.Context, q ledger.Querier, now time.Time) (NoteLinks, error) {
	var rep NoteLinks
	rows, err := q.QueryContext(ctx, `SELECT num, note FROM v_documents WHERE note LIKE '%:%' ORDER BY num`)
	if err != nil {
		return rep, err
	}
	type docKeys struct {
		num  int
		keys []KeyRef
	}
	var docs []docKeys
	for rows.Next() {
		var d docKeys
		var note string
		if err := rows.Scan(&d.num, &note); err != nil {
			rows.Close()
			return rep, err
		}
		if d.keys = KeysIn(note); len(d.keys) > 0 {
			docs = append(docs, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for _, d := range docs {
		rep.Documents++
		for _, k := range d.keys {
			rep.Keys++
			id, err := eventID(ctx, q, k)
			if err != nil {
				return rep, err
			}
			if id == 0 {
				rep.Missing++
				continue
			}
			ins, err := AddLink(ctx, q, Link{EventID: id, Part: k.Part, DocumentNum: d.num, How: "note_key", Decision: "recorded"}, now)
			if err != nil {
				return rep, err
			}
			if ins {
				rep.Linked++
			}
		}
	}
	return rep, nil
}

// supersedeLinked closes the open proposals of Bybit movements and bot events whose event (or its
// delta part) got a link: decided elsewhere (ledger-sync, a journal row, a manual link) and must not
// come back in the brief. A link to a journal row reads as recorded, one without a row keeps its decision.
func supersedeLinked(ctx context.Context, q ledger.Querier, by string, now time.Time) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, key FROM proposals WHERE status = 'open'
		AND (kind LIKE 'bybit%' OR kind LIKE 'card%') ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type open struct {
		id  int64
		key string
	}
	var list []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.key); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	closed := 0
	for _, o := range list {
		ref, ok := ParseKey(o.key)
		if !ok {
			continue
		}
		event, err := eventID(ctx, q, ref)
		if err != nil {
			return closed, err
		}
		if event == 0 {
			continue
		}
		decision, text, err := linksOf(ctx, q, event, ref.Part)
		if err != nil {
			return closed, err
		}
		if decision == "" {
			continue
		}
		if _, err := q.ExecContext(ctx, `UPDATE proposals SET status = 'superseded', decided_at = ?, decided_by = ?,
			decision = ?, decision_text = ? WHERE id = ? AND status = 'open'`,
			ts.Format(now), by, decision, text, o.id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// linksOf is the decision the links of an event part amount to and their text; "" without links.
func linksOf(ctx context.Context, q ledger.Querier, event int64, part string) (decision, text string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT how, decision, COALESCE(document_num, 0) FROM event_links
		WHERE event_id = ? AND part = ? ORDER BY id`, event, part)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var how, d string
		var doc int
		if err := rows.Scan(&how, &d, &doc); err != nil {
			return "", "", err
		}
		t := how + ":" + d
		if doc > 0 {
			t += fmt.Sprintf(" #%d", doc)
			decision = "recorded"
		} else if decision == "" {
			decision = d
		}
		texts = append(texts, t)
	}
	return decision, strings.Join(texts, ", "), rows.Err()
}

// SupersedeLinked is what a journal or state import runs after its rows are linked.
func SupersedeLinked(ctx context.Context, q ledger.Querier, by string, now time.Time) (int, error) {
	return supersedeLinked(ctx, q, by, now)
}

// UnlinkedRow is a Bybit movement without a journal row and without a personal/ignore decision.
type UnlinkedRow struct {
	EventID          int64  `json:"event_id"`
	Key              string `json:"key"`
	OccurredAt       string `json:"occurred_at"`
	Direction        string `json:"direction"`
	Amount           string `json:"amount"`
	Coin             string `json:"coin"`
	CounterpartyKind string `json:"counterparty_kind"`
	Links            string `json:"links,omitempty"`
	OpenProposal     int64  `json:"open_proposal,omitempty"`
	Candidates       []int  `json:"candidates,omitempty"` // journal rows that may be this movement: a hint, never a link
}

// Unlinked lists v_bybit_unlinked from since (a date or an MSK timestamp).
func Unlinked(ctx context.Context, q ledger.Querier, since string) ([]UnlinkedRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT event_id, key, occurred_at, COALESCE(direction, ''), COALESCE(amount, ''),
		COALESCE(coin, ''), COALESCE(counterparty_kind, ''), COALESCE(links, ''), COALESCE(open_proposal, 0)
		FROM v_bybit_unlinked WHERE occurred_at >= ? ORDER BY occurred_at, key`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnlinkedRow{}
	for rows.Next() {
		var r UnlinkedRow
		if err := rows.Scan(&r.EventID, &r.Key, &r.OccurredAt, &r.Direction, &r.Amount, &r.Coin, &r.CounterpartyKind,
			&r.Links, &r.OpenProposal); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Candidates, err = journalCandidates(ctx, q, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// journalCandidates finds rows someone may have written for the movement: the same amount, a type
// compatible with the direction, the date within a day. A question for a person, not an answer.
func journalCandidates(ctx context.Context, q ledger.Querier, r UnlinkedRow) ([]int, error) {
	amount, err := toMicros(r.Amount)
	if err != nil {
		return nil, nil
	}
	day, err := ts.Parse(r.OccurredAt)
	if err != nil {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT num FROM v_documents WHERE amount_micros = ? AND type IN (?, 'transfer')
		AND date BETWEEN ? AND ? ORDER BY num`, int64(amount), r.Direction, ts.Date(day.AddDate(0, 0, -1)), ts.Date(day.AddDate(0, 0, 1)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ManualLink is the result of LinkManual.
type ManualLink struct {
	Key        string `json:"key"`
	Linked     int    `json:"linked"`
	UnknownDoc []int  `json:"unknown_documents,omitempty"` // row numbers not (yet) in v_documents
	Superseded int    `json:"superseded"`
}

// LinkManual links the movement key to journal rows (decision recorded by default) or records a
// personal / ignore decision without a row, and closes its open proposal.
func LinkManual(ctx context.Context, q ledger.Querier, key string, docs []int, decision, note, author string, now time.Time) (ManualLink, error) {
	res := ManualLink{Key: key}
	r, ok := ParseKey(key)
	if !ok {
		return res, fmt.Errorf("не ключ движения: %q (deposit:N, internal_deposit:N, withdrawal:N, p2p:N, card:N, card:N:delta)", key)
	}
	id, err := eventID(ctx, q, r)
	if err != nil {
		return res, err
	}
	if id == 0 {
		return res, fmt.Errorf("события %s нет в базе", key)
	}
	if decision == "" {
		if len(docs) == 0 {
			return res, fmt.Errorf("нужна строка журнала (--doc) или решение --decision personal|ignore")
		}
		decision = "recorded"
	}
	if !knownDecision[decision] {
		return res, fmt.Errorf("решение %q: допустимо %s", decision, strings.Join(sortedKeys(knownDecision), ", "))
	}
	if len(docs) == 0 {
		if decision != "personal" && decision != "ignore" {
			return res, fmt.Errorf("без строки журнала допустимо только personal или ignore")
		}
		docs = []int{0}
	}
	for _, n := range docs {
		if n > 0 {
			var one int
			if err := q.QueryRowContext(ctx, `SELECT 1 FROM v_documents WHERE num = ?`, n).Scan(&one); err == sql.ErrNoRows {
				res.UnknownDoc = append(res.UnknownDoc, n)
			} else if err != nil {
				return res, err
			}
		}
		ins, err := AddLink(ctx, q, Link{EventID: id, Part: r.Part, DocumentNum: n, How: "manual", Decision: decision, Note: note}, now)
		if err != nil {
			return res, err
		}
		if ins {
			res.Linked++
		}
	}
	res.Superseded, err = supersedeLinked(ctx, q, author, now)
	return res, err
}

var knownDecision = map[string]bool{"confirm": true, "recorded": true, "custom": true, "personal": true, "ignore": true, "closed": true}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LinkNotesAndClose runs LinkNotes and closes the open proposals of the movements it linked.
func LinkNotesAndClose(ctx context.Context, q ledger.Querier, by string, now time.Time) (map[string]any, error) {
	rep, err := LinkNotes(ctx, q, now)
	if err != nil {
		return nil, err
	}
	n, err := supersedeLinked(ctx, q, by, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{"documents": rep.Documents, "keys": rep.Keys, "linked": rep.Linked, "missing": rep.Missing, "proposals_superseded": n}, nil
}

// PendingUSD sums the dollar movements the books do not know yet: they already changed the balance of the
// API, and have their own check of being closed within three days.
func PendingUSD(ctx context.Context, q ledger.Querier) (float64, error) {
	start := ""
	if err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'full_ledger_from'`).Scan(&start); err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if start == "" {
		start = "2026-08-01"
	}
	rows, err := q.QueryContext(ctx, `SELECT u.direction, u.amount, COALESCE(json_extract(e.payload, '$.fee'), '') FROM v_bybit_unlinked u
		JOIN events e ON e.id = u.event_id WHERE u.occurred_at >= ? AND u.coin IN ('USDT', 'USDC')`, start)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	sum := 0.0
	for rows.Next() {
		var dir, amount, fee sql.NullString
		if err := rows.Scan(&dir, &amount, &fee); err != nil {
			return 0, err
		}
		a, _ := strconv.ParseFloat(amount.String, 64)
		f, _ := strconv.ParseFloat(fee.String, 64)
		if dir.String == "income" {
			sum += a
		} else {
			sum -= a + f
		}
	}
	return sum, rows.Err()
}
