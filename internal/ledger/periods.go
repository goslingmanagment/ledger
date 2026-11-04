package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Period is a closed month.
type Period struct {
	Month    string          `json:"month"`
	LockedAt string          `json:"locked_at"`
	AsOf     string          `json:"as_of"`
	Snapshot json.RawMessage `json:"snapshot"`
	Decision *money.Micros   `json:"decision,omitempty"`
	Verdict  string          `json:"verdict,omitempty"`
	Source   string          `json:"source"`
}

// Lock closes a month once. The snapshot never changes afterwards.
func Lock(ctx context.Context, q Querier, p Period) error {
	if _, _, err := ts.MonthStart(p.Month); err != nil {
		return err
	}
	if !json.Valid(p.Snapshot) {
		return fmt.Errorf("snapshot of %s is not JSON", p.Month)
	}
	var dec any
	if p.Decision != nil {
		dec = int64(*p.Decision)
	}
	_, err := q.ExecContext(ctx, `INSERT INTO periods(month, locked_at, as_of, snapshot, decision, verdict, source)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, p.Month, p.LockedAt, p.AsOf, string(p.Snapshot), dec, nullStr(p.Verdict), p.Source)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: periods.month") {
		return fmt.Errorf("%w: %s", ErrPeriodLocked, p.Month)
	}
	return err
}

// GetPeriod returns the closed month or sql.ErrNoRows.
func GetPeriod(ctx context.Context, q Querier, month string) (Period, error) {
	var p Period
	var snap string
	var dec sql.NullInt64
	var verdict sql.NullString
	err := q.QueryRowContext(ctx, `SELECT month, locked_at, as_of, snapshot, decision, verdict, source FROM periods
		WHERE month = ?`, month).Scan(&p.Month, &p.LockedAt, &p.AsOf, &snap, &dec, &verdict, &p.Source)
	if err != nil {
		return p, err
	}
	p.Snapshot, p.Verdict = json.RawMessage(snap), verdict.String
	if dec.Valid {
		m := money.Micros(dec.Int64)
		p.Decision = &m
	}
	return p, nil
}

// Cut selects lines: effective in [From, To) (MSK dates or timestamps, "" = unbounded) and
// recorded at or before RecordedBy ("" = everything recorded so far).
type Cut struct {
	From, To   string
	RecordedBy string
}

// Totals sums lines per account over the cut.
func Totals(ctx context.Context, q Querier, c Cut) (map[string]money.Micros, error) {
	var where []string
	var args []any
	if c.From != "" {
		where, args = append(where, "e.effective_at >= ?"), append(args, c.From)
	}
	if c.To != "" {
		where, args = append(where, "e.effective_at < ?"), append(args, c.To)
	}
	if c.RecordedBy != "" {
		where, args = append(where, "e.recorded_at <= ?"), append(args, c.RecordedBy)
	}
	sqlText := `SELECT l.account, SUM(l.amount) FROM lines l JOIN entries e ON e.id = l.entry_id`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := q.QueryContext(ctx, sqlText+" GROUP BY l.account", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]money.Micros{}
	for rows.Next() {
		var a string
		var v int64
		if err := rows.Scan(&a, &v); err != nil {
			return nil, err
		}
		out[a] = money.Micros(v)
	}
	return out, rows.Err()
}

// MonthTotals returns per-account sums of a month «сейчас» and, for a closed month, «на закрытии»
// (only entries recorded by locked_at). atClose is nil for an open month.
func MonthTotals(ctx context.Context, q Querier, month string) (now, atClose map[string]money.Micros, err error) {
	m0, m1, err := ts.MonthStart(month)
	if err != nil {
		return nil, nil, err
	}
	c := Cut{From: ts.Date(m0), To: ts.Date(m1)}
	if now, err = Totals(ctx, q, c); err != nil {
		return nil, nil, err
	}
	p, err := GetPeriod(ctx, q, month)
	if err == sql.ErrNoRows {
		return now, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	c.RecordedBy = p.LockedAt
	atClose, err = Totals(ctx, q, c)
	return now, atClose, err
}

// Balance of one account: effective before `before` ("" = all) and recorded by `recordedBy`.
func Balance(ctx context.Context, q Querier, account, before, recordedBy string) (money.Micros, error) {
	t, err := Totals(ctx, q, Cut{To: before, RecordedBy: recordedBy})
	return t[account], err
}
