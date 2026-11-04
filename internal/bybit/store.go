package bybit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Settings and cursors of the collector.
const (
	settingMode  = "mode"        // shadow (default): the journal canon lives elsewhere, ledger only proposes
	settingStart = "bybit_start" // first day the collector reads when it has no watermark yet
	defaultStart = "2026-08-01"

	cursorMovements = "bybit:movements" // watermarks: end of the last complete read of each stream
	cursorP2P       = "bybit:p2p"
	cursorFunding   = "bybit:funding"
	cursorStatus    = "bybit:status"  // JSON of the last run (Status)
	cursorBalance   = "bybit:balance" // JSON of the last balance read
)

func setting(ctx context.Context, q ledger.Querier, key, def string) (string, error) {
	var v string
	switch err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v); {
	case err == sql.ErrNoRows:
		return def, nil
	case err != nil:
		return "", err
	}
	if v = strings.TrimSpace(v); v == "" {
		return def, nil
	}
	return v, nil
}

// Mode is settings.mode: shadow unless the switchover sets live.
func Mode(ctx context.Context, q ledger.Querier) (string, error) {
	return setting(ctx, q, settingMode, "shadow")
}

// Start is the first day of collection (settings.bybit_start, MSK midnight).
func Start(ctx context.Context, q ledger.Querier) (time.Time, error) {
	v, err := setting(ctx, q, settingStart, defaultStart)
	if err != nil {
		return time.Time{}, err
	}
	t, err := ts.Parse(v)
	if err != nil {
		return t, fmt.Errorf("настройка %s: %w", settingStart, err)
	}
	return t, nil
}

func getCursor(ctx context.Context, q ledger.Querier, name string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, name).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func setCursor(ctx context.Context, q ledger.Querier, name, value string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, value, ts.Format(now))
	return err
}

func watermark(ctx context.Context, q ledger.Querier, name string) (time.Time, bool, error) {
	v, err := getCursor(ctx, q, name)
	if err != nil || v == "" {
		return time.Time{}, false, err
	}
	t, err := ts.Parse(v)
	if err != nil {
		return t, false, fmt.Errorf("cursor %s: %w", name, err)
	}
	return t, true, nil
}

// addEventOnce stores e unless (source, source_id) is already there. Events never change: the first
// observation stays (a refetch may differ in volatile raw fields such as confirmations).
func addEventOnce(ctx context.Context, q ledger.Querier, e ledger.Event, now time.Time) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM events WHERE source = ? AND source_id = ?`, e.Source, e.SourceID).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	return ledger.AddEvent(ctx, q, e, now)
}

// storeMovement adds the movement once; for a known one it reports a change of the money fields.
func storeMovement(ctx context.Context, q ledger.Querier, m Movement, now time.Time) (inserted bool, mismatch string, err error) {
	var payload string
	err = q.QueryRowContext(ctx, `SELECT payload FROM events WHERE source = 'bybit' AND source_id = ?`, m.Key).Scan(&payload)
	if err == sql.ErrNoRows {
		_, inserted, err = ledger.AddEvent(ctx, q, m.Event(), now)
		return inserted, "", err
	}
	if err != nil {
		return false, "", err
	}
	var old Movement
	if err := json.Unmarshal([]byte(payload), &old); err != nil {
		return false, "", fmt.Errorf("event %s: %v", m.Key, err)
	}
	if !sameAmount(old.Amount, m.Amount) || old.Coin != m.Coin || old.Direction != m.Direction || old.Counterparty != m.Counterparty {
		return false, fmt.Sprintf("%s: сохранено %s %s %s, Bybit теперь %s %s %s", m.Key, old.Direction, old.Amount, old.Coin,
			m.Direction, m.Amount, m.Coin), nil
	}
	return false, "", nil
}

// pendingRow is a tracked withdrawal in a non-terminal status.
type pendingRow struct {
	Key        string
	FirstSeen  string
	LastStatus string
	Movement   Movement
}

func openPending(ctx context.Context, q ledger.Querier) ([]pendingRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, first_seen_at, last_status, movement FROM bybit_pending
		WHERE resolved_at IS NULL ORDER BY first_seen_at, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var p pendingRow
		var raw string
		if err := rows.Scan(&p.Key, &p.FirstSeen, &p.LastStatus, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &p.Movement); err != nil {
			return nil, fmt.Errorf("bybit_pending %s: %v", p.Key, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func upsertPending(ctx context.Context, q ledger.Querier, m Movement, status, errText string, now time.Time) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	at := ts.Format(now)
	_, err = q.ExecContext(ctx, `INSERT INTO bybit_pending(key, first_seen_at, last_status, checked_at, movement, error)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(key) DO UPDATE SET last_status = excluded.last_status,
		checked_at = excluded.checked_at, movement = excluded.movement, error = excluded.error
		WHERE bybit_pending.resolved_at IS NULL`, m.Key, at, status, at, string(raw), errText)
	return err
}

func resolvePending(ctx context.Context, q ledger.Querier, key, resolution string, now time.Time) error {
	at := ts.Format(now)
	_, err := q.ExecContext(ctx, `UPDATE bybit_pending SET resolved_at = ?, resolution = ?, checked_at = ?, error = ''
		WHERE key = ? AND resolved_at IS NULL`, at, resolution, at, key)
	return err
}

// ReadStatus returns the cursors of the last pass and balance read as they are stored.
func ReadStatus(ctx context.Context, q ledger.Querier) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for name, cursor := range map[string]string{"run": cursorStatus, "balance": cursorBalance} {
		v, err := getCursor(ctx, q, cursor)
		if err != nil {
			return nil, err
		}
		if v != "" {
			out[name] = json.RawMessage(v)
		}
	}
	return out, nil
}
