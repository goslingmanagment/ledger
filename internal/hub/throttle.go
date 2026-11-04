package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// throttleCursor keeps the send times of the last dataset requests (unix seconds), so a run that
// starts within a minute of the previous one, or a CLI run beside the service, counts them too.
const throttleCursor = "hub:sent"

// throttleWindow is month.py's minute with its half-second margin.
const throttleWindow = 60500 * time.Millisecond

// Throttle allows at most RPM dataset requests in any rolling minute (month.py Hub.throttle). It
// waits only when the minute's quota is already spent.
type Throttle struct {
	DB    *sql.DB // write pool: the state is read and written in one immediate transaction
	RPM   int
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// Wait blocks until one more request fits in the minute and records it.
func (t *Throttle) Wait(ctx context.Context) error {
	if t.RPM < 1 {
		return fmt.Errorf("hub_rpm должен быть ≥ 1")
	}
	now, sleep := t.Now, t.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepCtx
	}
	for {
		wait, err := t.reserve(ctx, now())
		if err != nil || wait <= 0 {
			return err
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (t *Throttle) reserve(ctx context.Context, now time.Time) (time.Duration, error) {
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var raw string
	var sent []float64
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, throttleCursor).Scan(&raw); err {
	case nil:
		if json.Unmarshal([]byte(raw), &sent) != nil {
			sent = nil // a broken state only loses the memory of the last minute
		}
	case sql.ErrNoRows:
	default:
		return 0, err
	}
	sort.Float64s(sent)
	at := float64(now.UnixNano()) / 1e9
	win := throttleWindow.Seconds()
	for len(sent) > 0 && at-sent[0] >= win {
		sent = sent[1:]
	}
	if len(sent) >= t.RPM {
		oldest := sent[len(sent)-t.RPM]
		wait := time.Duration(math.Ceil((oldest + win - at) * 1e9))
		if wait <= 0 {
			wait = time.Millisecond
		}
		return wait, tx.Commit()
	}
	sent = append(sent, at)
	b, _ := json.Marshal(sent)
	if _, err := tx.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		throttleCursor, string(b), ts.Format(now)); err != nil {
		return 0, err
	}
	return 0, tx.Commit()
}
