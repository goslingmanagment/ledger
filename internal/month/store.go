package month

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// LoadTabs reads the parsed payout tabs of the sheets collector. tabsErr lists the tabs the strict
// parser refused: the card can still be shown, the close is blocked.
func LoadTabs(ctx context.Context, q ledger.Querier) (tabs []Tab, tabsErr string, err error) {
	var errs []string
	var read int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM payroll_tables`).Scan(&read); err != nil {
		return nil, "", err
	}
	if read == 0 {
		return nil, "таблицы выплат ещё не собраны", nil
	}
	failed, err := q.QueryContext(ctx, `SELECT model, COALESCE(error, '') FROM payroll_tables WHERE status <> 'ok' ORDER BY model`)
	if err != nil {
		return nil, "", err
	}
	for failed.Next() {
		var model, tableErr string
		if err := failed.Scan(&model, &tableErr); err != nil {
			failed.Close()
			return nil, "", err
		}
		errs = append(errs, "таблица "+model+": "+tableErr)
	}
	failed.Close()
	if err := failed.Err(); err != nil {
		return nil, "", err
	}
	// A tab gone from the sheet's lists is not read by the card, as in the script; its debts stay in payroll.
	rows, err := q.QueryContext(ctx, `SELECT model, name, status, COALESCE(error, ''), COALESCE(notes, '[]') FROM payroll_tabs WHERE listed = 1 ORDER BY rowid`)
	if err != nil {
		return nil, "", err
	}
	byKey := map[string]*Tab{}
	for rows.Next() {
		var model, name, status, tabErr, notes string
		if err := rows.Scan(&model, &name, &status, &tabErr, &notes); err != nil {
			rows.Close()
			return nil, "", err
		}
		if status != "ok" {
			errs = append(errs, model+"/"+name+": "+tabErr)
			continue
		}
		t := Tab{Model: model, Name: name, Days: map[string]float64{}}
		_ = json.Unmarshal([]byte(notes), &t.Notes)
		tabs = append(tabs, t)
		byKey[model+"\x00"+name] = &tabs[len(tabs)-1]
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	// Pointers above may be stale after appends: rebuild the index on the final slice.
	for i := range tabs {
		byKey[tabs[i].Model+"\x00"+tabs[i].Name] = &tabs[i]
	}
	drows, err := q.QueryContext(ctx, `SELECT model, name, date, amount, worked FROM payroll_days ORDER BY date`)
	if err != nil {
		return nil, "", err
	}
	defer drows.Close()
	for drows.Next() {
		var model, name, date string
		var amount float64
		var worked int
		if err := drows.Scan(&model, &name, &date, &amount, &worked); err != nil {
			return nil, "", err
		}
		t := byKey[model+"\x00"+name]
		if t == nil {
			continue
		}
		if amount != 0 {
			t.Days[date] = amount
		}
		if worked != 0 {
			t.Worked = append(t.Worked, date)
		}
	}
	return tabs, strings.Join(errs, "; "), drows.Err()
}

// RefreshCards recomputes the cards of every month from the first full one to the current one.
func RefreshCards(ctx context.Context, tx *sql.Tx, asOf, now time.Time) error {
	cfg, err := LoadConfig(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := JournalRows(ctx, tx, "")
	if err != nil {
		return err
	}
	tabs, tabsErr, err := LoadTabs(ctx, tx)
	if err != nil {
		return err
	}
	save := func(month, status, reason string, card any) error {
		raw, err := json.Marshal(card)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO month_cards(month, as_of, computed_at, status, reason, card) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(month) DO UPDATE SET as_of = excluded.as_of, computed_at = excluded.computed_at, status = excluded.status,
			reason = excluded.reason, card = excluded.card`, month, ts.Format(asOf), ts.Format(now), status, reason, string(raw))
		return err
	}
	var months []string
	for m := cfg.FullLedger; !m.After(asOf); m = m.AddDate(0, 1, 0) {
		months = append(months, ts.Month(m))
	}
	src := DBSource{Ctx: ctx, Q: tx}
	cv, err := BuildCover(cfg, src, rows, asOf)
	if err != nil {
		var f Fail
		if !hub.IsRefusal(err) && !errors.As(err, &f) {
			return err
		}
		for _, m := range months {
			if serr := save(m, "unknown", err.Error(), map[string]any{}); serr != nil {
				return serr
			}
		}
		return nil
	}
	if tabs == nil {
		tabs = []Tab{}
	}
	for _, m := range months {
		card, err := BuildCard(cv, m, tabs)
		if err != nil {
			var f Fail
			if !hub.IsRefusal(err) && !errors.As(err, &f) {
				return err
			}
			if serr := save(m, "unknown", err.Error(), map[string]any{}); serr != nil {
				return serr
			}
			continue
		}
		snap := card.Snapshot()
		snap["checks"] = card.Checks(tabsErr)
		snap["notes"] = card.Notes
		reason := ""
		if tabsErr != "" {
			reason = "таблицы выплат: " + tabsErr
		}
		if err := save(m, "ok", reason, snap); err != nil {
			return err
		}
	}
	return nil
}

// PayrollOf renders the tabs' days of a month the way the closing snapshot keeps them.
func PayrollOf(tabs []Tab, month string) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	for _, t := range tabs {
		days := map[string]float64{}
		keys := make([]string, 0, len(t.Days))
		for d := range t.Days {
			keys = append(keys, d)
		}
		sort.Strings(keys)
		for _, d := range keys {
			if d[:7] == month {
				days[d] = t.Days[d]
			}
		}
		out[t.Model+"/"+t.Name] = days
	}
	return out
}
