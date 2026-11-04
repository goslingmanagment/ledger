package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	events.Register(events.Source{Name: SourceSheets, Arrive: arrive,
		Settle: func(ctx context.Context, store *db.Store, now time.Time) error {
			_, err := Rebuild(ctx, store, now)
			return err
		}})
	server.Every("payroll", time.Hour, func(ctx context.Context, s *server.Server) error {
		rep, err := Rebuild(ctx, s.Store, ts.Now())
		if err != nil {
			return err
		}
		if rep.Changed() {
			s.Log.Info("payroll", "accruals", rep.Accruals, "clearings", rep.Clearings, "reversals", rep.Reversals)
		}
		return nil
	})
}

func refused(format string, args ...any) error {
	return &events.BadRequest{Msg: fmt.Sprintf(format, args...)}
}

// arrive applies one 'sheets' event to the latest state. A repeat is news too: a tab may have changed and
// changed back, and only the time says which content is the latest, so a fetch older than what is
// known stays in the history and touches nothing.
func arrive(ctx context.Context, tx *sql.Tx, a events.Arrival) (bool, error) {
	fetched := a.OccurredAt
	if fetched == "" {
		fetched = ts.Format(a.Now)
	}
	switch a.Kind {
	case "tab":
		var p TabPayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return false, refused("payload вкладки: %v", err)
		}
		if p.Model == "" || p.Name == "" || (p.Parsed == nil) == (p.Error == "") {
			return false, refused("payload вкладки: нужны model, name и ровно одно из parsed и error")
		}
		return arriveTab(ctx, tx, p, a.ID, fetched)
	case "table":
		var p TablePayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return false, refused("payload таблицы: %v", err)
		}
		if p.Model == "" {
			return false, refused("payload таблицы: нужна model")
		}
		return arriveTable(ctx, tx, p, a.ID, fetched)
	case "run":
		return false, nil
	}
	return false, refused("вид события «%s» источнику sheets неизвестен", a.Kind)
}

func arriveTable(ctx context.Context, tx *sql.Tx, p TablePayload, id int64, fetched string) (bool, error) {
	var last, curStatus string
	var curEvent int64
	err := tx.QueryRowContext(ctx, `SELECT fetched_at, status, event_id FROM payroll_tables WHERE model = ?`, p.Model).
		Scan(&last, &curStatus, &curEvent)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && fetched < last {
		return false, nil
	}
	status := "ok"
	var errText any
	if p.Error != "" {
		status, errText = "error", p.Error
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_tables(model, status, error, tabs, fetched_at, event_id) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET status = excluded.status, error = excluded.error, tabs = excluded.tabs,
		fetched_at = excluded.fetched_at, event_id = excluded.event_id`,
		p.Model, status, errText, jsonText(p.Tabs), fetched, id); err != nil {
		return false, err
	}
	if p.Error != "" {
		return curEvent != id, nil // the list is unknown: the tabs keep what they had
	}
	if _, err := tx.ExecContext(ctx, `UPDATE payroll_tabs SET listed = EXISTS (SELECT 1 FROM json_each(?) j WHERE j.value = payroll_tabs.name)
		WHERE model = ?`, jsonText(p.Tabs), p.Model); err != nil {
		return false, err
	}
	return curEvent != id, nil
}

func arriveTab(ctx context.Context, tx *sql.Tx, p TabPayload, id int64, fetched string) (bool, error) {
	var last, status string
	var curEvent int64
	var okEvent sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT fetched_at, status, event_id, ok_event_id FROM payroll_tabs WHERE model = ? AND name = ?`,
		p.Model, p.Name).Scan(&last, &status, &curEvent, &okEvent)
	exists := err == nil
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if exists && fetched < last {
		return false, nil
	}
	if p.Error != "" { // what the last good reading gave stays until the tab is good again
		if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_tabs(model, name, status, error, fetched_at, event_id, listed)
			VALUES (?, ?, 'error', ?, ?, ?, 1)
			ON CONFLICT(model, name) DO UPDATE SET status = 'error', error = excluded.error, fetched_at = excluded.fetched_at,
			event_id = excluded.event_id, listed = 1`, p.Model, p.Name, p.Error, fetched, id); err != nil {
			return false, err
		}
		return !exists || status != "error" || curEvent != id, nil
	}
	if okEvent.Valid && okEvent.Int64 == id { // the same content as the last good reading
		if _, err := tx.ExecContext(ctx, `UPDATE payroll_tabs SET status = 'ok', error = NULL, fetched_at = ?, event_id = ?, listed = 1
			WHERE model = ? AND name = ?`, fetched, id, p.Model, p.Name); err != nil {
			return false, err
		}
		return status != "ok", nil
	}
	t := p.Parsed
	var rate any
	if t.Rate != nil {
		rate = *t.Rate
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_tabs(model, name, status, error, fetched_at, kv, paid, total, rate, notes,
		event_id, ok_event_id, ok_at, listed) VALUES (?, ?, 'ok', NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(model, name) DO UPDATE SET status = 'ok', error = NULL, fetched_at = excluded.fetched_at, kv = excluded.kv,
		paid = excluded.paid, total = excluded.total, rate = excluded.rate, notes = excluded.notes, event_id = excluded.event_id,
		ok_event_id = excluded.ok_event_id, ok_at = excluded.ok_at, listed = 1`,
		p.Model, p.Name, fetched, t.KV, t.Paid, t.Total, rate, jsonText(t.Notes), id, id, fetched); err != nil {
		return false, err
	}
	if err := replaceDays(ctx, tx, p.Model, p.Name, t, id); err != nil {
		return false, err
	}
	return true, nil
}

// replaceDays writes the tab's days: the amount as the parser computed it and what each page of the
// block holds for that date.
func replaceDays(ctx context.Context, tx *sql.Tx, model, name string, t *Tab, event int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM payroll_days WHERE model = ? AND name = ?`, model, name); err != nil {
		return err
	}
	worked := map[string]bool{}
	for _, d := range t.Worked {
		worked[d] = true
	}
	week := map[string]string{}
	pages := map[string]map[string]*float64{}
	for _, b := range t.Blocks {
		start, err := time.Parse(ts.DateLayout, b.Start)
		if err != nil {
			return refused("блок %q: %v", b.Start, err)
		}
		for k := range 7 {
			date := start.AddDate(0, 0, k).Format(ts.DateLayout)
			week[date] = b.Start // weeks may overlap: the later block names the date
			day := pages[date]
			if day == nil {
				day = map[string]*float64{}
				pages[date] = day
			}
			for _, r := range b.Pages {
				v, had := day[r.Page], false
				if v != nil {
					had = true
				}
				switch {
				case r.Values[k] != nil && had:
					sum := *v + *r.Values[k]
					day[r.Page] = &sum
				case r.Values[k] != nil:
					day[r.Page] = r.Values[k]
				case !had:
					day[r.Page] = nil
				}
			}
		}
	}
	dates := make([]string, 0, len(t.Days))
	for d := range t.Days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	for _, d := range dates {
		w, ok := week[d]
		if !ok {
			return refused("день %s вне недельных блоков", d)
		}
		on := 0
		if worked[d] {
			on = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_days(model, name, date, amount, worked, week, pages, event_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, model, name, d, t.Days[d], on, w, jsonText(pages[d]), event); err != nil {
			return err
		}
	}
	return nil
}
