// Package ledger is the write path of the books: events, journal documents, balanced entries,
// reversals and closed months. Every write goes through these functions inside one transaction.
package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Errors callers branch on.
var (
	ErrUnbalanced       = errors.New("entry does not balance")
	ErrEventConflict    = errors.New("event exists with another payload")
	ErrSourceKey        = errors.New("source_key exists with other lines")
	ErrAlreadyReversed  = errors.New("entry is already reversed")
	ErrPeriodLocked     = errors.New("month is already closed")
	ErrBadEffectiveTime = errors.New("effective_at must be ГГГГ-ММ-ДД or an MSK timestamp")
)

var effectiveRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}:\d{2}(\.\d{3})?\+03:00)?$`)

// Event is one raw fact.
type Event struct {
	Source     string
	SourceID   string
	Kind       string
	OccurredAt string // MSK timestamp or ""
	Payload    any
}

// AddEvent stores the event once per (source, source_id). A repeat with the same payload returns
// the stored id and inserted=false; a repeat with another payload is ErrEventConflict.
func AddEvent(ctx context.Context, q Querier, e Event, now time.Time) (id int64, inserted bool, err error) {
	if e.Source == "" || e.SourceID == "" || e.Kind == "" {
		return 0, false, fmt.Errorf("event needs source, source_id and kind")
	}
	payload, err := canonicalJSON(e.Payload)
	if err != nil {
		return 0, false, err
	}
	var occurred any
	if e.OccurredAt != "" {
		occurred = e.OccurredAt
	}
	res, err := q.ExecContext(ctx, `INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(source, source_id) DO NOTHING`,
		e.Source, e.SourceID, e.Kind, occurred, ts.Format(now), payload)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		id, err = res.LastInsertId()
		return id, true, err
	}
	var stored string
	if err := q.QueryRowContext(ctx, `SELECT id, payload FROM events WHERE source = ? AND source_id = ?`,
		e.Source, e.SourceID).Scan(&id, &stored); err != nil {
		return 0, false, err
	}
	if stored != payload {
		return id, false, fmt.Errorf("%w: %s/%s", ErrEventConflict, e.Source, e.SourceID)
	}
	return id, false, nil
}

func canonicalJSON(v any) (string, error) {
	var raw []byte
	switch p := v.(type) {
	case json.RawMessage:
		raw = p
	case []byte:
		raw = p
	case string:
		raw = []byte(p)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		raw = b
	}
	var x any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&x); err != nil {
		return "", fmt.Errorf("payload is not JSON: %w", err)
	}
	b, err := json.Marshal(x) // map keys sorted, numbers kept as written
	return string(b), err
}

// Line is one side of an entry.
type Line struct {
	Account  string
	Amount   money.Micros // + debit, − credit
	Currency string
	Orig     string
	Memo     string
}

// Entry is a balanced posting.
type Entry struct {
	EffectiveAt string
	Kind        string
	Memo        string
	DocumentID  int64
	ProposalID  int64
	DisputeID   int64
	ReversesID  int64
	Author      string
	SourceKey   string
	Lines       []Line
	EventIDs    []int64
}

// Post validates and writes the entry. With a SourceKey already stored, the same lines return the
// existing id (idempotent), other lines are ErrSourceKey.
func Post(ctx context.Context, q Querier, e Entry, now time.Time) (int64, error) {
	if !effectiveRe.MatchString(e.EffectiveAt) {
		return 0, fmt.Errorf("%w: %q", ErrBadEffectiveTime, e.EffectiveAt)
	}
	if e.Kind == "" || e.Author == "" {
		return 0, fmt.Errorf("entry needs kind and author")
	}
	if len(e.Lines) < 2 {
		return 0, fmt.Errorf("%w: an entry needs at least two lines", ErrUnbalanced)
	}
	var sum money.Micros
	for _, l := range e.Lines {
		if l.Amount == 0 {
			return 0, fmt.Errorf("zero line on %s", l.Account)
		}
		sum += l.Amount
	}
	if sum != 0 {
		return 0, fmt.Errorf("%w: lines sum to %s", ErrUnbalanced, sum)
	}
	if e.SourceKey != "" {
		var id int64
		err := q.QueryRowContext(ctx, `SELECT id FROM entries WHERE source_key = ?`, e.SourceKey).Scan(&id)
		if err == nil {
			same, err := sameLines(ctx, q, id, e.Lines)
			if err != nil {
				return 0, err
			}
			if !same {
				return id, fmt.Errorf("%w: %s", ErrSourceKey, e.SourceKey)
			}
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	date := e.EffectiveAt[:10]
	for _, l := range e.Lines {
		if err := EnsureAccount(ctx, q, l.Account, date); err != nil {
			return 0, err
		}
	}
	res, err := q.ExecContext(ctx, `INSERT INTO entries(effective_at, recorded_at, kind, memo, document_id, proposal_id,
		dispute_id, reverses_id, author, source_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.EffectiveAt, ts.Format(now), e.Kind, e.Memo, nullID(e.DocumentID), nullID(e.ProposalID),
		nullID(e.DisputeID), nullID(e.ReversesID), e.Author, nullStr(e.SourceKey))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: entries.reverses_id") {
			return 0, fmt.Errorf("%w: %d", ErrAlreadyReversed, e.ReversesID)
		}
		return 0, err
	}
	id, _ := res.LastInsertId()
	for _, l := range e.Lines {
		if _, err := q.ExecContext(ctx, `INSERT INTO lines(entry_id, account, amount, currency, orig_amount, memo)
			VALUES (?, ?, ?, ?, ?, ?)`, id, l.Account, int64(l.Amount), nullStr(l.Currency), nullStr(l.Orig), l.Memo); err != nil {
			return 0, err
		}
	}
	for _, ev := range e.EventIDs {
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO entry_events(entry_id, event_id) VALUES (?, ?)`, id, ev); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func sameLines(ctx context.Context, q Querier, id int64, want []Line) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT account, amount FROM lines WHERE entry_id = ?`, id)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var a string
		var v int64
		if err := rows.Scan(&a, &v); err != nil {
			return false, err
		}
		have = append(have, fmt.Sprintf("%s=%d", a, v))
	}
	var w []string
	for _, l := range want {
		w = append(w, fmt.Sprintf("%s=%d", l.Account, int64(l.Amount)))
	}
	sort.Strings(have)
	sort.Strings(w)
	return strings.Join(have, ",") == strings.Join(w, ","), rows.Err()
}

// Reversal describes why an entry is reversed.
type Reversal struct {
	Memo        string
	Author      string
	DisputeID   int64
	ProposalID  int64
	SourceKey   string
	EffectiveAt string // default: the reversed entry's own date, so the correction lands in its month
}

// Reverse posts the mirror image of entry id. The original stays untouched.
func Reverse(ctx context.Context, q Querier, id int64, r Reversal, now time.Time) (int64, error) {
	var effective, memo string
	var doc sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT effective_at, memo, document_id FROM entries WHERE id = ?`, id).
		Scan(&effective, &memo, &doc); err != nil {
		return 0, fmt.Errorf("entry %d: %w", id, err)
	}
	var already int64
	err := q.QueryRowContext(ctx, `SELECT id FROM entries WHERE reverses_id = ?`, id).Scan(&already)
	if err == nil {
		return 0, fmt.Errorf("%w: %d by %d", ErrAlreadyReversed, id, already)
	} else if err != sql.ErrNoRows {
		return 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT account, amount, currency, orig_amount, memo FROM lines WHERE entry_id = ? ORDER BY id`, id)
	if err != nil {
		return 0, err
	}
	var lines []Line
	for rows.Next() {
		var l Line
		var v int64
		var cur, orig sql.NullString
		if err := rows.Scan(&l.Account, &v, &cur, &orig, &l.Memo); err != nil {
			rows.Close()
			return 0, err
		}
		l.Amount, l.Currency, l.Orig = -money.Micros(v), cur.String, orig.String
		lines = append(lines, l)
	}
	rows.Close()
	if r.EffectiveAt == "" {
		r.EffectiveAt = effective
	}
	if r.Memo == "" {
		r.Memo = "сторно: " + memo
	}
	return Post(ctx, q, Entry{EffectiveAt: r.EffectiveAt, Kind: "reversal", Memo: r.Memo, DocumentID: doc.Int64,
		ProposalID: r.ProposalID, DisputeID: r.DisputeID, ReversesID: id, Author: r.Author, SourceKey: r.SourceKey,
		Lines: lines}, now)
}

func nullID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
