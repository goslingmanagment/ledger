package ledger

import (
	"context"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A journal row is dated by its day, so its entry tells when the money moved only to the day. A balance
// read at a moment (a bot's anchor) is compared with the books at that moment: a day-dated entry counts at
// the latest event of that day it stands on (its own or its document's), with none of that day at the day.

// TimedLine is a line of an account and when its money moved.
type TimedLine struct {
	Entry     int64
	Amount    money.Micros
	Day       string    // the MSK day of the entry
	At        time.Time // the moment; zero when only the day is known
	SourceKey string
}

// By says whether the line's money had moved by t.
func (l TimedLine) By(t time.Time) bool {
	if !l.At.IsZero() {
		return !l.At.After(t)
	}
	return l.Day <= ts.Date(t)
}

// AccountLines reads the lines of an account with their moments.
func AccountLines(ctx context.Context, q Querier, account string) ([]TimedLine, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.id, l.amount, e.effective_at, COALESCE(e.source_key, ''),
		COALESCE((SELECT group_concat(COALESCE(v.occurred_at, v.received_at), '|') FROM events v WHERE v.id IN (
			SELECT ee.event_id FROM entry_events ee WHERE ee.entry_id = e.id
			UNION SELECT el.event_id FROM event_links el JOIN documents d ON d.num = el.document_num WHERE d.id = e.document_id)), '')
		FROM lines l JOIN entries e ON e.id = l.entry_id WHERE l.account = ? ORDER BY e.effective_at, e.id, l.id`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimedLine
	for rows.Next() {
		var l TimedLine
		var v int64
		var effective, events string
		if err := rows.Scan(&l.Entry, &v, &effective, &l.SourceKey, &events); err != nil {
			return nil, err
		}
		l.Amount, l.Day = money.Micros(v), effective[:10]
		if len(effective) > len(ts.DateLayout) {
			if t, err := ts.Parse(effective); err == nil {
				l.At, l.Day = t, ts.Date(t)
			}
		} else {
			l.At = latestOfDay(events, l.Day)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func latestOfDay(events, day string) time.Time {
	var latest time.Time
	for _, s := range strings.Split(events, "|") {
		t, err := ts.Parse(s)
		if err != nil || ts.Date(t) != day {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

// BalanceAt is the account at moment t.
func BalanceAt(ctx context.Context, q Querier, account string, t time.Time) (money.Micros, error) {
	lines, err := AccountLines(ctx, q, account)
	if err != nil {
		return 0, err
	}
	var sum money.Micros
	for _, l := range lines {
		if l.By(t) {
			sum += l.Amount
		}
	}
	return sum, nil
}
