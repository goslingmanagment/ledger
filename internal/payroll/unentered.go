package payroll

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const (
	// unenteredDays is how far back «не занесено» looks.
	unenteredDays = 45
	// unenteredTail is how long after the last value of a tab its empty days still count: the tail of
	// a table lags by days, while a tab silent for weeks is a page the person has left.
	unenteredTail = 14
)

// slot is a person's shift by MSK clock, in minutes from midnight.
type slot struct{ start, end int }

var slotRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})\s*[-–—]\s*(\d{1,2}):(\d{2})$`)

func parseSlot(s string) (slot, bool) {
	m := slotRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return slot{}, false
	}
	n := func(i int) int {
		v, _ := strconv.Atoi(m[i])
		return v
	}
	if n(1) > 23 || n(2) > 59 || n(3) > 24 || n(4) > 59 {
		return slot{}, false
	}
	return slot{start: n(1)*60 + n(2), end: n(3)*60 + n(4)}, true
}

// window is the hub window of a table cell: the cell's date is the calendar date the shift starts on,
// so the slot 00:00–08:00 of cell D is (D−1) 21:00Z … D 05:00Z, and a slot past midnight ends on D+1.
func (s slot) window(date time.Time) (time.Time, time.Time) {
	from := date.Add(time.Duration(s.start) * time.Minute)
	to := date.Add(time.Duration(s.end) * time.Minute)
	if !to.After(from) {
		to = to.Add(24 * time.Hour)
	}
	return from, to
}

// scheduled reads the card's workdays for a cell. The schedule dates a night shift by the evening of
// entry: «вышел в 00:00, это ещё понедельник», so mon with a slot starting after midnight is the cell of
// Tuesday. No workdays on the card means every day.
func (s slot) scheduled(workdays []string, date time.Time) bool {
	if len(workdays) == 0 {
		return true
	}
	if s.start < 6*60 {
		date = date.AddDate(0, 0, -1)
	}
	return slices.Contains(workdays, strings.ToLower(date.Weekday().String()[:3]))
}

// pageResolver maps a page row of a tab to the hub's label: settings.sheet_pages first, then the name
// spelled as a label ("Page 1" → "page-1") when that label exists. Nothing else is tried.
func pageResolver(ctx context.Context, q ledger.Querier) (func(string) (string, bool), error) {
	named, err := optionalMap(ctx, q, "sheet_pages")
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT page_label FROM routes UNION SELECT page_label FROM hub_pages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		known[l] = true
	}
	return func(page string) (string, bool) {
		if l, ok := named[page]; ok {
			return l, l != ""
		}
		l := strings.ToLower(strings.Join(strings.Fields(page), "-"))
		return l, known[l]
	}, rows.Err()
}

type unenteredRow struct {
	model, name, person, date, page string
	from, to                        time.Time
	status, reason                  string
	net                             *int64
	share                           *float64
}

type dayCells struct {
	date  string
	week  string
	pages map[string]*float64
}

func (d dayCells) hasValue() bool {
	for _, v := range d.pages {
		if v != nil && *v != 0 {
			return true
		}
	}
	return false
}

// weekPages decides which pages of the tab count for each week: the rows that hold money that week.
// An empty week (the tail of a table lags) keeps the pages of the last week that had any; the rows of a
// block also name pages the person does not work, and their money belongs to whoever is on them.
func weekPages(days []dayCells) map[string][]string {
	byWeek := map[string]map[string]bool{}
	var weeks []string
	for _, d := range days {
		w := byWeek[d.week]
		if w == nil {
			w = map[string]bool{}
			byWeek[d.week] = w
			weeks = append(weeks, d.week)
		}
		for page, v := range d.pages {
			if v != nil && *v != 0 {
				w[page] = true
			}
		}
	}
	sort.Strings(weeks)
	out := map[string][]string{}
	var last []string
	for _, week := range weeks {
		if len(byWeek[week]) > 0 {
			last = last[:0:0]
			for p := range byWeek[week] {
				last = append(last, p)
			}
			sort.Strings(last)
		}
		out[week] = last
	}
	return out
}

// computeUnentered walks the days without a value of the last 45 days.
func computeUnentered(ctx context.Context, q ledger.Querier, now time.Time) ([]unenteredRow, error) {
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	label, err := pageResolver(ctx, q)
	if err != nil {
		return nil, err
	}
	nowMSK := now.In(ts.MSK)
	today := time.Date(nowMSK.Year(), nowMSK.Month(), nowMSK.Day(), 0, 0, 0, 0, ts.MSK)
	first := today.AddDate(0, 0, -unenteredDays)
	type tab struct {
		model, name, person string
		rate                *float64
	}
	var tabs []tab
	rows, err := q.QueryContext(ctx, `SELECT model, name, person, rate FROM payroll_tabs WHERE person IS NOT NULL AND ok_event_id IS NOT NULL
		ORDER BY model, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t tab
		if err := rows.Scan(&t.model, &t.name, &t.person, &t.rate); err != nil {
			rows.Close()
			return nil, err
		}
		tabs = append(tabs, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// Everything is read first: a person's empty cell is no gap when someone on the same slot entered that
	// page that day (the page changed hands, and the money is already on their tab).
	daysOf := map[string][]dayCells{}
	enteredBy := map[string]map[string]bool{} // model, page, date → slugs with a value there
	for _, t := range tabs {
		// The whole tab: a week inherits the pages of the last filled one, and only the days from the first
		// value on are shifts at all (new tabs start with empty weeks).
		dr, err := q.QueryContext(ctx, `SELECT date, week, pages FROM payroll_days WHERE model = ? AND name = ? AND date <= ? ORDER BY date`,
			t.model, t.name, ts.Date(today))
		if err != nil {
			return nil, err
		}
		var days []dayCells
		for dr.Next() {
			var d dayCells
			var pages string
			if err := dr.Scan(&d.date, &d.week, &pages); err != nil {
				dr.Close()
				return nil, err
			}
			if err := json.Unmarshal([]byte(pages), &d.pages); err != nil {
				dr.Close()
				return nil, fmt.Errorf("payroll_days %s/%s %s: %w", t.model, t.name, d.date, err)
			}
			days = append(days, d)
			for page, v := range d.pages {
				if v != nil && *v != 0 {
					k := t.model + "\x00" + page + "\x00" + d.date
					if enteredBy[k] == nil {
						enteredBy[k] = map[string]bool{}
					}
					enteredBy[k][t.person] = true
				}
			}
		}
		if err := dr.Err(); err != nil {
			dr.Close()
			return nil, err
		}
		dr.Close()
		daysOf[t.model+"\x00"+t.name] = days
	}
	var out []unenteredRow
	for _, t := range tabs {
		p := people[t.person]
		sl, haveSlot := parseSlot(p.Schedule)
		if !haveSlot && p.Status == "departed" {
			continue // nobody expects a shift of a person who left and has no slot
		}
		days := daysOf[t.model+"\x00"+t.name]
		active := weekPages(days)
		firstValue, lastValue := "", ""
		for _, d := range days {
			if d.hasValue() {
				if firstValue == "" {
					firstValue = d.date
				}
				lastValue = d.date
			}
		}
		if firstValue == "" {
			continue // nothing was ever entered here
		}
		until, err := time.ParseInLocation(ts.DateLayout, lastValue, ts.MSK)
		if err != nil {
			return nil, err
		}
		until = until.AddDate(0, 0, unenteredTail)
		for _, d := range days {
			date, err := time.ParseInLocation(ts.DateLayout, d.date, ts.MSK)
			if err != nil {
				return nil, err
			}
			if date.Before(first) || date.After(until) || d.date < firstValue || d.hasValue() {
				continue
			}
			base := unenteredRow{model: t.model, name: t.name, person: t.person, date: d.date}
			if !haveSlot {
				base.page, base.status, base.reason = "—", "unknown", "в карточке нет разборчивого графика: окно смены не определить"
				out = append(out, base)
				continue
			}
			if !sl.scheduled(p.Workdays, date) {
				continue
			}
			from, to := sl.window(date)
			if to.After(now) {
				continue // the shift is not over
			}
			base.from, base.to = from, to
			for _, page := range active[d.week] {
				covered := false
				for other := range enteredBy[t.model+"\x00"+page+"\x00"+d.date] {
					if theirs, ok := parseSlot(people[other].Schedule); ok && theirs == sl {
						covered = true
					}
				}
				if covered {
					continue
				}
				row := base
				row.page = page
				l, ok := label(page)
				if !ok { // a page with money that the hub cannot be asked about
					row.status, row.reason = "unknown", "страница вкладки не сопоставлена со страницей хаба (settings.sheet_pages)"
					out = append(out, row)
					continue
				}
				row.page = l
				checked, err := hub.Rows(ctx, q, l, from, to)
				if err != nil {
					if !hub.IsRefusal(err) {
						return nil, err
					}
					row.status, row.reason = "unknown", err.Error()
					out = append(out, row)
					continue
				}
				var net int64
				for _, r := range checked.Rows {
					net += r.NetMills
				}
				if net == 0 {
					continue
				}
				row.status, row.net = "ok", &net
				if t.rate != nil {
					share := float64(float64(net)/1000) * *t.rate
					row.share = &share
				} else {
					row.reason = "в шапке вкладки нет ставки: доля не посчитана"
				}
				out = append(out, row)
			}
		}
	}
	return out, nil
}

func refreshUnentered(ctx context.Context, store *db.Store, now time.Time) (int, error) {
	rows, err := computeUnentered(ctx, store.RO, now)
	if err != nil {
		return 0, fmt.Errorf("не занесено: %w", err)
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM payroll_unentered`); err != nil {
		return 0, err
	}
	at := ts.Format(now)
	for _, r := range rows {
		var from, to, reason any
		if !r.from.IsZero() {
			from, to = ts.Format(r.from), ts.Format(r.to)
		}
		if r.reason != "" {
			reason = r.reason
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_unentered(model, name, person, date, page, window_from, window_to, status, reason,
			net_mills, share, checked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(model, name, date, page) DO NOTHING`,
			r.model, r.name, r.person, r.date, r.page, from, to, r.status, reason, r.net, r.share, at); err != nil {
			return 0, err
		}
	}
	return len(rows), tx.Commit()
}
