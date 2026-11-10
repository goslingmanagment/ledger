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
	"github.com/goslingmanagment/ledger/internal/split"
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
	named, known, err := pageNames(ctx, q)
	if err != nil {
		return nil, err
	}
	return func(page string) (string, bool) { return resolvePage(named, known, page) }, nil
}

// pageNames are the inputs of resolvePage: settings.sheet_pages and the labels Hub and the routes know.
func pageNames(ctx context.Context, q ledger.Querier) (map[string]string, map[string]bool, error) {
	named, err := optionalMap(ctx, q, "sheet_pages")
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT page_label FROM routes UNION SELECT page_label FROM hub_pages`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	known := map[string]bool{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, nil, err
		}
		known[l] = true
	}
	return named, known, rows.Err()
}

// resolvePage is the one rule that maps a tab's row name to Hub's label: the reader («не занесено», the mirror check) and
// the writer (ledger mirror sheets) both go through it, so the two can never disagree. A row named in sheet_pages with ""
// is not a page.
func resolvePage(named map[string]string, known map[string]bool, page string) (string, bool) {
	if l, ok := named[page]; ok {
		return l, l != ""
	}
	l := strings.ToLower(strings.Join(strings.Fields(page), "-"))
	return l, known[l]
}

// PageLabeler maps a sheet's page row to Hub's label the way «не занесено» does; a row it cannot map is spelled as a
// label ("Page 1" → "page-1") all the same, so that a comparison still lists it.
func PageLabeler(ctx context.Context, q ledger.Querier) (func(string) string, error) {
	label, err := pageResolver(ctx, q)
	if err != nil {
		return nil, err
	}
	return func(page string) string {
		l, _ := label(page)
		if l == "" {
			l = strings.ToLower(strings.Join(strings.Fields(page), "-"))
		}
		return l
	}, nil
}

// HubPageModels is the ledger model of each page of the hub grant: settings.hub_model_slugs of its modelSlug, or the
// slug itself.
func HubPageModels(ctx context.Context, q ledger.Querier) (map[string]string, error) {
	slugs, err := optionalMap(ctx, q, "hub_model_slugs")
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT page_label, model_slug FROM hub_pages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var label, slug string
		if err := rows.Scan(&label, &slug); err != nil {
			return nil, err
		}
		out[label] = slug
		if m, ok := slugs[slug]; ok {
			out[label] = m
		}
	}
	return out, rows.Err()
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

// Which way a row of «не занесено» was counted, in its reason.
const (
	reasonFacts = "по фактам хаба: его доли продаж за день (fact_accruals)"
	reasonSlot  = "по окну графика: все деньги страницы в окне смены из карточки"
	reasonRate  = "в шапке вкладки нет ставки: доля не посчитана"
)

type unenteredTab struct {
	model, name, person string
	rate                *float64
}

// computeUnentered walks the days without a value of the last 45 days. A day of a week Hub's team facts proved, of
// a person who is a Hub user, is their own accrued sales of the day (teamFacts.rows), and a day without their work is
// a day off; every other day is the money of the week's pages in the person's slot by the card — unless Hub's works
// prove the slot (settleSlots): then a slot nobody shares and the person had no shift in is a day off too, before the
// split rule's start as well.
//
// Two kinds of day are no gap at all and make no row: a day on or before the person's paid-through day (a payout is
// the check of those days, whichever way they would be counted), and, for the slot only, a day of a tab whose model
// the card no longer lists (the person left that model: their slot there is no guess, and no colleague shares it).
// Hub's facts still count such a tab: their own sale on the model is cover money nobody entered.
func computeUnentered(ctx context.Context, q ledger.Querier, now time.Time) ([]unenteredRow, error) {
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	label, err := pageResolver(ctx, q)
	if err != nil {
		return nil, err
	}
	facts, err := loadFacts(ctx, q, now)
	if err != nil {
		return nil, err
	}
	paid, err := paidThrough(ctx, q)
	if err != nil {
		return nil, err
	}
	// From the switch to a source after the sheets on, an empty cell is no gap: the source accrues the day. What it
	// could not is marked unknown (planFacts, planReports); under the reports so is a worked shift without a report.
	plan, err := planAfter(ctx, q, now)
	if err != nil {
		return nil, err
	}
	afterFrom := ""
	if plan != nil {
		afterFrom = plan.start()
	}
	nowMSK := now.In(ts.MSK)
	today := time.Date(nowMSK.Year(), nowMSK.Month(), nowMSK.Day(), 0, 0, 0, 0, ts.MSK)
	first := today.AddDate(0, 0, -unenteredDays)
	var tabs []unenteredTab
	rows, err := q.QueryContext(ctx, `SELECT model, name, person, rate FROM payroll_tabs WHERE person IS NOT NULL AND ok_event_id IS NOT NULL
		ORDER BY model, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t unenteredTab
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
	enteredDay := map[string]bool{}           // person, model, date → a value on a tab of theirs
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
			if d.hasValue() {
				enteredDay[personDay(t.person, t.model, d.date)] = true
			}
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
	var out, fromFacts []unenteredRow
	factDays := map[string]bool{} // model, name, date → counted from the facts
	counted := map[string]bool{}  // person, model, date → the facts of the day are on a tab already
	for _, t := range tabs {
		p := people[t.person]
		sl, haveSlot := parseSlot(p.Schedule)
		onCard := cardHasModel(p, t.model)
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
			if date.Before(first) || date.After(until) || d.date < firstValue || d.hasValue() || d.date <= paid[t.person] ||
				(afterFrom != "" && d.date >= afterFrom) {
				continue
			}
			if facts.cover(t.person, t.model, d.date) {
				factDays[t.model+"\x00"+t.name+"\x00"+d.date] = true
				k := personDay(t.person, t.model, d.date)
				if !counted[k] && !enteredDay[k] { // one tab of the model takes the day; a value on another is an entry
					counted[k] = true
					fromFacts = append(fromFacts, facts.rows(t, d.date)...)
				}
			}
			if !onCard {
				continue // the card left this model: no slot guess
			}
			if !haveSlot && p.Status == "departed" {
				continue // nobody expects a shift of a person who left and has no slot
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
				row.status, row.net, row.reason = "ok", &net, reasonSlot
				if t.rate != nil {
					share := float64(float64(net)/1000) * *t.rate
					row.share = &share
				} else {
					row.reason += "; " + reasonRate
				}
				out = append(out, row)
			}
		}
	}
	// The slot rows of every person go through settleSlots first, so that a colleague on the same slot who has no
	// facts is settled exactly as before. Then a day the facts cover drops its slot rows on Hub's pages (and the
	// «no slot» row) for the person's own sales; a page outside the grant keeps what the slot said.
	out, err = settleSlots(ctx, q, out)
	if err != nil {
		return nil, err
	}
	kept := out[:0:0]
	for _, r := range out {
		if factDays[r.model+"\x00"+r.name+"\x00"+r.date] && (r.page == "—" || facts.hubPage(r.page)) {
			continue
		}
		kept = append(kept, r)
	}
	kept = append(kept, fromFacts...)
	if plan != nil {
		kept = append(kept, plan.marks()...)
	}
	return kept, nil
}

func personDay(person, model, date string) string {
	return person + "\x00" + ledger.Slug(model) + "\x00" + date
}

// cardHasModel says whether the card lists the tab's model (card ari is tab Ari, as Resolve compares them). A card
// without models lists every model.
func cardHasModel(p Person, model string) bool {
	if len(p.Models) == 0 {
		return true
	}
	return slices.ContainsFunc(p.Models, func(m string) bool { return ledger.Slug(m) == ledger.Slug(model) })
}

// paidThrough is each person's last day the payouts reached in the FIFO of their debt (payable_days): the last debt
// row cleared in full with every row before it, or the eve of the first row a payout only began on. A person with
// nothing unpaid is paid through their last debt row. Days up to it are settled: «не занесено» chases none of them.
func paidThrough(ctx context.Context, q ledger.Querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT person, date, paid, unpaid FROM payable_days WHERE kind <> 'advance' ORDER BY person, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	stopped := map[string]bool{}
	for rows.Next() {
		var person, date string
		var paid, unpaid int64
		if err := rows.Scan(&person, &date, &paid, &unpaid); err != nil {
			return nil, err
		}
		if stopped[person] {
			continue
		}
		if unpaid > 0 {
			stopped[person] = true
			if paid == 0 {
				continue
			}
			d, err := time.Parse(ts.DateLayout, date)
			if err != nil {
				return nil, fmt.Errorf("payable_days %s %s: %w", person, date, err)
			}
			date = d.AddDate(0, 0, -1).Format(ts.DateLayout)
		}
		if date > out[person] {
			out[person] = date
		}
	}
	return out, rows.Err()
}

// teamFacts are Hub's proven team facts as «не занесено» reads them (ledger release 2): the weeks fact_weeks proved,
// each person's accrued sales by model, page and day (fact_accruals, the split rules' shares of their shifts and
// look-ins), and their works by day.
type teamFacts struct {
	since string            // the first day the split rule pays
	weeks map[string]bool   // Mondays of the proven weeks
	pages map[string]string // page of the grant → ledger model
	res   *Resolver
	users map[int64]string            // the Hub users of the proven windows → username
	isHub map[string]bool             // person, model → one of those users is the person (cache)
	net   map[string]map[string]int64 // person, model, date → page → net of their shares, mills
	works map[string]*dayWorks        // person, model, date
}

// dayWorks spans a person's works of a day on a model's pages.
type dayWorks struct {
	from, to time.Time
	going    bool
}

func loadFacts(ctx context.Context, q ledger.Querier, now time.Time) (*teamFacts, error) {
	f := &teamFacts{since: ts.Date(split.Default.Since), weeks: map[string]bool{}, users: map[int64]string{}, isHub: map[string]bool{},
		net: map[string]map[string]int64{}, works: map[string]*dayWorks{}}
	rows, err := q.QueryContext(ctx, `SELECT week FROM fact_weeks WHERE status = 'ok'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var wk string
		if err := rows.Scan(&wk); err != nil {
			rows.Close()
			return nil, err
		}
		f.weeks[wk] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(f.weeks) == 0 {
		return f, nil
	}
	if f.res, err = NewResolver(ctx, q); err != nil {
		return nil, err
	}
	if f.pages, err = HubPageModels(ctx, q); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT person, model, page_label, day, SUM(net_micros) FROM fact_accruals
		WHERE kind = 'sales' AND person IS NOT NULL GROUP BY person, model, page_label, day`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var person, model, page, day string
		var micros int64
		if err := rows.Scan(&person, &model, &page, &day, &micros); err != nil {
			rows.Close()
			return nil, err
		}
		k := personDay(person, model, day)
		if f.net[k] == nil {
			f.net[k] = map[string]int64{}
		}
		f.net[k][page] += micros / 1000 // the shares are whole mills
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A work is in every window its days touch: the whole version is the one of the proven window it began in, else
	// the longest one there is (it began before the facts). The Hub users the facts answer for are those of the rule's
	// weeks: the windows before them (hub_team_since) are read for the slot (settleSlots).
	ruleWeek := ts.WeekStart(split.Default.Since)
	rows, err = q.QueryContext(ctx, `SELECT s.window_from, s.window_to, w.ref, w.payload FROM hub_works w
		JOIN hub_team_sync s ON s.window_from = w.window_from WHERE s.status = 'ok' ORDER BY w.ref, s.window_from`)
	if err != nil {
		return nil, err
	}
	type version struct {
		w     hub.TeamWork
		whole bool
	}
	works := map[string]version{}
	var refs []string
	for rows.Next() {
		var wf, wt, ref, payload string
		if err := rows.Scan(&wf, &wt, &ref, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		w, err := hub.DecodeTeamWork([]byte(payload))
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("hub_works %s %s: %w", wf, ref, err)
		}
		from, err1 := ts.Parse(wf)
		to, err2 := ts.Parse(wt)
		if err1 != nil || err2 != nil {
			rows.Close()
			return nil, fmt.Errorf("hub_team_sync %s: bad window", wf)
		}
		if !from.Before(ruleWeek) {
			f.users[w.UserID] = w.Username
		}
		v := version{w: w, whole: !w.StartedAt.Before(from) && w.StartedAt.Before(to)}
		old, seen := works[ref]
		switch {
		case !seen:
			refs = append(refs, ref)
		case old.whole, !v.whole && !w.EndedAt.After(old.w.EndedAt):
			continue
		}
		works[ref] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		w := works[ref].w
		seen := map[string]bool{}
		for _, p := range w.Pages {
			model, ok := f.pages[p.PageLabel]
			if !ok || seen[ledger.Slug(model)] {
				continue
			}
			seen[ledger.Slug(model)] = true
			person, err := f.res.HubPerson(model, w.UserID, w.Username)
			if err != nil {
				continue
			}
			k := personDay(person, model, w.Day())
			d := f.works[k]
			if d == nil {
				d = &dayWorks{from: w.StartedAt, to: w.EndedAt}
				f.works[k] = d
			}
			if w.StartedAt.Before(d.from) {
				d.from = w.StartedAt
			}
			if w.EndedAt.After(d.to) {
				d.to = w.EndedAt
			}
			d.going = d.going || w.Ongoing || w.EndedAt.After(now)
		}
	}
	return f, nil
}

// cover says whether the facts answer for the person's day on the model: the week is proven (from the split rule's
// first day on) and the person is a Hub user of the facts. Otherwise the day goes by the slot.
func (f *teamFacts) cover(person, model, date string) bool {
	if date < f.since {
		return false
	}
	d, err := time.ParseInLocation(ts.DateLayout, date, ts.MSK)
	if err != nil || !f.weeks[ts.Date(ts.WeekStart(d))] {
		return false
	}
	k := person + "\x00" + ledger.Slug(model)
	is, ok := f.isHub[k]
	if !ok {
		for id, name := range f.users {
			if got, err := f.res.HubPerson(model, id, name); err == nil && got == person {
				is = true
				break
			}
		}
		f.isHub[k] = is
	}
	return is
}

// hubPage says whether the facts see the page (it is in the grant).
func (f *teamFacts) hubPage(page string) bool {
	_, ok := f.pages[page]
	return ok
}

// rows are the person's own accrued sales of the day on the tab's model, a row per page. No work that day is a day
// off, not a missing entry: no rows; a work still going is a shift not over yet.
func (f *teamFacts) rows(t unenteredTab, date string) []unenteredRow {
	k := personDay(t.person, t.model, date)
	w := f.works[k]
	if w != nil && w.going {
		return nil
	}
	pages := make([]string, 0, len(f.net[k]))
	for page := range f.net[k] {
		pages = append(pages, page)
	}
	sort.Strings(pages)
	var out []unenteredRow
	for _, page := range pages {
		net := f.net[k][page]
		if net == 0 {
			continue
		}
		row := unenteredRow{model: t.model, name: t.name, person: t.person, date: date, page: page, status: "ok", net: &net,
			reason: reasonFacts}
		if w != nil {
			row.from, row.to = w.from, w.to
		}
		if t.rate != nil {
			share := float64(float64(net)/1000) * *t.rate
			row.share = &share
		} else {
			row.reason += "; " + reasonRate
		}
		out = append(out, row)
	}
	return out
}

// settleSlots settles the page-window money of the slot rows by Hub's team facts.
//
// Two or more people's empty cells claim it at once: their cards put them on the same slot of the page, and nobody
// entered it. Where Hub's team facts prove the window, it is theirs whose work Hub saw on the page in the window
// (others drop out: they did not work it); otherwise it is «неизвестно» for each of them — never the same money
// counted twice.
//
// One person's claim stays as the slot says, unless Hub's facts prove them off it (teamWorks.dayOff): no shift of
// theirs on the page in the window, a look-in being no shift. Then the day is a day off, not a gap. Before the split
// rule's start this is all Hub can say: the payout sheets still pay those weeks, the accruals do not count them.
func settleSlots(ctx context.Context, q ledger.Querier, rows []unenteredRow) ([]unenteredRow, error) {
	type slotKey struct {
		model, page string
		from, to    int64
	}
	claims := map[slotKey][]int{}
	for i, r := range rows {
		if r.status == "ok" {
			k := slotKey{r.model, r.page, r.from.Unix(), r.to.Unix()}
			claims[k] = append(claims[k], i)
		}
	}
	if len(claims) == 0 {
		return rows, nil
	}
	worked, err := provenWorks(ctx, q)
	if err != nil {
		return nil, err
	}
	drop := map[int]bool{}
	for k, idx := range claims {
		from, to := time.Unix(k.from, 0), time.Unix(k.to, 0)
		people := map[string]bool{}
		for _, i := range idx {
			people[rows[i].person] = true
		}
		if len(people) == 1 {
			if worked.dayOff(rows[idx[0]].person, k.page, from, to) {
				for _, i := range idx {
					drop[i] = true
				}
			}
			continue
		}
		proven, who := worked.on(k.page, from, to)
		var keep []int
		if proven {
			for _, i := range claims[k] {
				if who[rows[i].person] {
					keep = append(keep, i)
				}
			}
		}
		for _, i := range claims[k] {
			switch {
			case proven && len(keep) > 0 && !slices.Contains(keep, i):
				drop[i] = true
			case proven && len(keep) > 0:
				// theirs: Hub saw their shift on the page
			case proven:
				rows[i].status, rows[i].net, rows[i].share = "unknown", nil, nil
				rows[i].reason = "на этом слоте страницы по графику несколько человек, а хаб не видел смены ни у кого из них"
			default:
				rows[i].status, rows[i].net, rows[i].share = "unknown", nil, nil
				rows[i].reason = "на этом слоте страницы по графику несколько человек; кто работал, хаб ещё не доказал"
			}
		}
	}
	out := rows[:0:0]
	for i, r := range rows {
		if !drop[i] {
			out = append(out, r)
		}
	}
	return out, nil
}

// teamWorks are the shifts and look-ins of the windows Hub's team facts proved (hub_team_sync ok, from hub_team_since
// on), by ledger person.
type teamWorks struct {
	windows []struct{ from, to time.Time }
	works   []provenWork
	// seen spans each person's works: from the start of the first to the end of the last.
	seen map[string]struct{ from, to time.Time }
}

type provenWork struct {
	person, kind string
	pages        []string
	from, to     time.Time
}

func provenWorks(ctx context.Context, q ledger.Querier) (teamWorks, error) {
	tw := teamWorks{seen: map[string]struct{ from, to time.Time }{}}
	rows, err := q.QueryContext(ctx, `SELECT window_from, window_to FROM hub_team_sync WHERE status = 'ok'`)
	if err != nil {
		return tw, err
	}
	for rows.Next() {
		var f, t string
		if err := rows.Scan(&f, &t); err != nil {
			rows.Close()
			return tw, err
		}
		from, err1 := ts.Parse(f)
		to, err2 := ts.Parse(t)
		if err1 != nil || err2 != nil {
			rows.Close()
			return tw, fmt.Errorf("hub_team_sync %s: bad window", f)
		}
		tw.windows = append(tw.windows, struct{ from, to time.Time }{from, to})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return tw, err
	}
	if len(tw.windows) == 0 {
		return tw, nil
	}
	res, err := NewResolver(ctx, q)
	if err != nil {
		return tw, err
	}
	wr, err := q.QueryContext(ctx, `SELECT w.user_id, w.username, w.payload FROM hub_works w
		JOIN hub_team_sync s ON s.window_from = w.window_from AND s.status = 'ok'`)
	if err != nil {
		return tw, err
	}
	defer wr.Close()
	for wr.Next() {
		var user int64
		var username, payload string
		if err := wr.Scan(&user, &username, &payload); err != nil {
			return tw, err
		}
		var w hub.TeamWork
		if err := json.Unmarshal([]byte(payload), &w); err != nil {
			return tw, fmt.Errorf("hub_works %d: %w", user, err)
		}
		person, err := res.HubPerson("", user, username)
		if err != nil {
			continue // a Hub user without a card claims no one's cell
		}
		pages := make([]string, 0, len(w.Pages))
		for _, p := range w.Pages {
			pages = append(pages, p.PageLabel)
		}
		tw.works = append(tw.works, provenWork{person: person, kind: w.Kind, pages: pages, from: w.StartedAt, to: w.EndedAt})
		seen, ok := tw.seen[person]
		if !ok || w.StartedAt.Before(seen.from) {
			seen.from = w.StartedAt
		}
		if !ok || w.EndedAt.After(seen.to) {
			seen.to = w.EndedAt
		}
		tw.seen[person] = seen
	}
	return tw, wr.Err()
}

// dayOff says whether Hub's facts prove the person did not work the page in [from, to): the window is proven, Hub saw
// them before it and after it (a work began by its start and one ended by its end or later: they were on Hub then, so
// no work is no shift, not missing data), and no shift of theirs on the page overlaps it. A look-in is no shift.
func (tw teamWorks) dayOff(person, page string, from, to time.Time) bool {
	if proven, _ := tw.on(page, from, to); !proven {
		return false
	}
	seen, ok := tw.seen[person]
	if !ok || seen.from.After(from) || seen.to.Before(to) {
		return false
	}
	for _, w := range tw.works {
		if w.person == person && w.kind == split.Shift && w.from.Before(to) && from.Before(w.to) && slices.Contains(w.pages, page) {
			return false
		}
	}
	return true
}

// on says whether Hub's facts prove the window [from, to), and whose work on the page overlaps it.
func (tw teamWorks) on(page string, from, to time.Time) (bool, map[string]bool) {
	proven := false
	for _, w := range tw.windows {
		if !from.Before(w.from) && !to.After(w.to) {
			proven = true
		}
	}
	if !proven {
		return false, nil
	}
	who := map[string]bool{}
	for _, w := range tw.works {
		if w.from.Before(to) && from.Before(w.to) && slices.Contains(w.pages, page) {
			who[w.person] = true
		}
	}
	return true, who
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
