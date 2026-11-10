package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The switch of the accruals (ledger release 2, L4). From settings.payroll_facts_from on, a person's day is accrued
// from Hub's facts (fact_accruals: their shares of the sales by the split rules at the rate of their tab, the fix per
// shift), keyed to the person's tab of the model; every day before it stays the sheet's, as it was. The sheet becomes
// a mirror for the days from the switch: still read, compared, never posted. A day the facts cannot answer for — its
// week not proven, the person no Hub user, no rate to price it — is never a silent zero: it keeps what is posted (if
// anything) and stands in «не занесено» as unknown, with the reason. Clearing the setting and rebuilding gives back the
// sheet's books exactly.

// SettingFactsFrom is the first Moscow day accrued from Hub's facts; empty or absent — the sheets only.
const SettingFactsFrom = "payroll_facts_from"

// Sources of an accrued day.
const (
	SourceSheet = "sheet"
	SourceFacts = "facts"
)

// reasonPending opens the reason of a day the facts did not accrue («не занесено», status unknown).
const reasonPending = "факты хаба не начислили"

// FactsFrom is the switch day, "" when the sheets still accrue everything.
func FactsFrom(ctx context.Context, q ledger.Querier) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, SettingFactsFrom).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(ts.DateLayout, v); err != nil || t.Format(ts.DateLayout) != v {
		return "", fmt.Errorf("настройка %s: нужна дата ГГГГ-ММ-ДД, а не %q", SettingFactsFrom, v)
	}
	return v, nil
}

// SetFactsFrom sets the switch day ("" turns the facts off). It must not precede the split rule's first day nor the
// accrual date: before them there are no facts to accrue from.
func SetFactsFrom(ctx context.Context, q ledger.Querier, day string, now time.Time) error {
	if day != "" {
		t, err := time.Parse(ts.DateLayout, day)
		if err != nil || t.Format(ts.DateLayout) != day {
			return fmt.Errorf("дата переключения: нужна ГГГГ-ММ-ДД, а не %q", day)
		}
		if rule := ts.Date(split.Default.Since); day < rule {
			return fmt.Errorf("дата переключения %s раньше начала правил дележа %s: фактов за те дни нет", day, rule)
		}
		from, err := AccrualFrom(ctx, q)
		if err != nil {
			return err
		}
		if day < from {
			return fmt.Errorf("дата переключения %s раньше accrual_from %s", day, from)
		}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingFactsFrom, day, factsFromNote, ts.Format(now))
	return err
}

const factsFromNote = "С какого дня (МСК) начисления людям — из фактов хаба (fact_accruals), а не из таблиц выплат; пусто — только таблицы. " +
	"Ставит и снимает ledger payroll facts-from; откат — --off и пересборка"

// weekOfDate is the Monday of a Moscow date.
func weekOfDate(day string) string {
	t, err := time.ParseInLocation(ts.DateLayout, day, ts.MSK)
	if err != nil {
		return day
	}
	return ts.Date(ts.WeekStart(t))
}

// factDay is a tab's day as the facts accrue it.
type factDay struct {
	model, name, person, date string
	amount                    money.Micros
}

// Unaccrued are the facts from the switch on that no tab takes: a Hub user no card names, or a person without a tab of
// the model. They are nobody's debt until that is put right; the health check and `ledger payables` list them.
type Unaccrued struct {
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	Person    string `json:"person,omitempty"`
	Model     string `json:"model"`
	NetMicros int64  `json:"net_micros"`
	Days      int    `json:"days"`
	Reason    string `json:"reason"`
}

// factPlan is what Hub's facts say about every tab's day from the switch on.
type factPlan struct {
	from      string
	want      map[string]factDay // "<model>/<name>:<date>" → accrued from the facts
	proven    map[string]bool    // Mondays of the proven weeks
	reasons   map[string][]string
	unmapped  map[string]bool   // tabs "<model>/<name>" of people no Hub user of the facts is
	unpriced  map[string]string // "<model>/<name>:<date>" → why the facts give no amount
	markers   []unenteredRow    // the days the facts did not accrue, for «не занесено»
	unaccrued []Unaccrued
}

// holds says whether a tab's day from the switch on is the facts' to change: it is not while its week is unproven, the
// person is no Hub user, or the facts cannot price it. Such a day keeps what is posted.
func (p *factPlan) holds(model, name, date string) bool {
	if !p.proven[weekOfDate(date)] || p.unmapped[model+"/"+name] {
		return true
	}
	_, ok := p.unpriced[model+"/"+name+":"+date]
	return ok
}

type planTab struct {
	model, name, person string
	listed              bool
}

// planFacts reads the facts from the switch day on: fact_accruals of the proven weeks keyed to each person's tab of the
// model, and what cannot be accrued with why. nil when the sheets accrue everything.
func planFacts(ctx context.Context, q ledger.Querier, from string, now time.Time) (*factPlan, error) {
	if from == "" {
		return nil, nil
	}
	p := &factPlan{from: from, want: map[string]factDay{}, proven: map[string]bool{}, reasons: map[string][]string{},
		unmapped: map[string]bool{}, unpriced: map[string]string{}}
	rows, err := q.QueryContext(ctx, `SELECT week, status, reasons FROM fact_weeks`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var wk, status, reasons string
		if err := rows.Scan(&wk, &status, &reasons); err != nil {
			rows.Close()
			return nil, err
		}
		if status == "ok" {
			p.proven[wk] = true
			continue
		}
		var rs []string
		_ = json.Unmarshal([]byte(reasons), &rs)
		p.reasons[wk] = rs
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	res, err := NewResolver(ctx, q)
	if err != nil {
		return nil, err
	}

	// The tab of each person and model: a listed one first, then by name; several of one model share the rate (the
	// facts refuse to price them otherwise).
	var tabs []planTab
	rows, err = q.QueryContext(ctx, `SELECT model, name, person, listed FROM payroll_tabs WHERE person IS NOT NULL
		ORDER BY listed DESC, model, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t planTab
		if err := rows.Scan(&t.model, &t.name, &t.person, &t.listed); err != nil {
			rows.Close()
			return nil, err
		}
		tabs = append(tabs, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tabOf := map[string]planTab{}
	for _, t := range tabs {
		k := t.person + "\x00" + ledger.Slug(t.model)
		if _, ok := tabOf[k]; !ok {
			tabOf[k] = t
		}
	}

	// A person is a Hub user of the facts when one of the users of the rule's windows resolves to them on the model.
	users, err := factUsers(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, t := range tabs {
		found := false
		for id, name := range users {
			if got, err := res.HubPerson(t.model, id, name); err == nil && got == t.person {
				found = true
				break
			}
		}
		if !found {
			p.unmapped[t.model+"/"+t.name] = true
		}
	}

	rows, err = q.QueryContext(ctx, `SELECT hub_user_id, username, COALESCE(person, ''), model, day, net_micros, amount_micros,
		COALESCE(reason, '') FROM fact_accruals WHERE day >= ? ORDER BY day, hub_user_id, model, page_label`, from)
	if err != nil {
		return nil, err
	}
	type lostKey struct {
		user             int64
		username, person string
		model            string
	}
	lost := map[lostKey]*Unaccrued{}
	lostDays := map[lostKey]map[string]bool{}
	unpricedNet := map[string]int64{}
	for rows.Next() {
		var user, net int64
		var username, person, model, day, reason string
		var amount sql.NullInt64
		if err := rows.Scan(&user, &username, &person, &model, &day, &net, &amount, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if !p.proven[weekOfDate(day)] {
			continue // the week's markers say it
		}
		t, ok := tabOf[person+"\x00"+ledger.Slug(model)]
		if person == "" || !ok {
			k := lostKey{user, username, person, model}
			u := lost[k]
			if u == nil {
				u = &Unaccrued{UserID: user, Username: username, Person: person, Model: model, Reason: reason}
				if person != "" && reason == "" {
					u.Reason = "нет вкладки человека в таблице выплат модели «" + model + "»"
				}
				lost[k], lostDays[k] = u, map[string]bool{}
			}
			u.NetMicros += net
			lostDays[k][day] = true
			continue
		}
		key := t.model + "/" + t.name + ":" + day
		if !amount.Valid {
			p.unpriced[key] = reason
			unpricedNet[key] += net
			continue
		}
		d := p.want[key]
		d.model, d.name, d.person, d.date = t.model, t.name, t.person, day
		d.amount += money.Micros(amount.Int64)
		p.want[key] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for key := range p.unpriced {
		delete(p.want, key) // a day priced in part is not the facts' answer
	}
	for k, u := range lost {
		u.Days = len(lostDays[k])
		p.unaccrued = append(p.unaccrued, *u)
	}
	sort.Slice(p.unaccrued, func(i, j int) bool {
		a, b := p.unaccrued[i], p.unaccrued[j]
		if a.Username != b.Username {
			return a.Username < b.Username
		}
		return a.Model < b.Model
	})

	if err := p.mark(ctx, q, tabs, people, unpricedNet, now); err != nil {
		return nil, err
	}
	return p, nil
}

// factUsers are the Hub users of the proven windows from the split rule's week on.
func factUsers(ctx context.Context, q ledger.Querier) (map[int64]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT s.window_from, w.user_id, w.username FROM hub_works w
		JOIN hub_team_sync s ON s.window_from = w.window_from WHERE s.status = 'ok'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ruleWeek := ts.WeekStart(split.Default.Since)
	out := map[int64]string{}
	for rows.Next() {
		var win, name string
		var id int64
		if err := rows.Scan(&win, &id, &name); err != nil {
			return nil, err
		}
		if t, err := ts.Parse(win); err == nil && !t.Before(ruleWeek) {
			out[id] = name
		}
	}
	return out, rows.Err()
}

// mark lists the days from the switch on that the facts did not accrue, as «не занесено» unknown: a tab a week whose
// facts are not proven, or whose person is no Hub user; a day the facts could not price. A week is marked for a tab its
// person works (listed, the card active and naming the model) or that holds money of the week (posted or in the sheet).
func (p *factPlan) mark(ctx context.Context, q ledger.Querier, tabs []planTab, people map[string]Person, unpricedNet map[string]int64,
	now time.Time) error {
	nowMSK := now.In(ts.MSK)
	today := time.Date(nowMSK.Year(), nowMSK.Month(), nowMSK.Day(), 0, 0, 0, 0, ts.MSK)
	first := max(p.from, ts.Date(today.AddDate(0, 0, -unenteredDays)))
	last := ts.Date(today)
	if first > last {
		return nil
	}
	busy := map[string]bool{} // "<model>/<name>\x00<week>": money of the week, posted or in the sheet
	rows, err := q.QueryContext(ctx, `SELECT model, name, date FROM payroll_days WHERE person IS NOT NULL AND amount <> 0 AND date BETWEEN ? AND ?`,
		first, last)
	if err != nil {
		return err
	}
	for rows.Next() {
		var model, name, date string
		if err := rows.Scan(&model, &name, &date); err != nil {
			rows.Close()
			return err
		}
		busy[model+"/"+name+"\x00"+weekOfDate(date)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	posted, err := postedDays(ctx, q, first)
	if err != nil {
		return err
	}
	for day := range posted {
		if model, name, date, ok := splitDayKey(day); ok && date <= last {
			busy[model+"/"+name+"\x00"+weekOfDate(date)] = true
		}
	}
	start, err := time.ParseInLocation(ts.DateLayout, first, ts.MSK)
	if err != nil {
		return err
	}
	for wk := ts.WeekStart(start); !wk.After(today); wk = wk.AddDate(0, 0, 7) {
		week := ts.Date(wk)
		day := max(week, first)
		var reason string
		for _, t := range tabs {
			card := people[t.person]
			works := t.listed && card.Status != "departed" && cardHasModel(card, t.model)
			if !works && !busy[t.model+"/"+t.name+"\x00"+week] {
				continue
			}
			switch {
			case !p.proven[week]:
				why := "факты недели не читались: ledger hub team-sync"
				if rs := p.reasons[week]; len(rs) > 0 {
					why = strings.Join(rs, "; ")
				}
				reason = fmt.Sprintf("%s: неделя %s не доказана — %s; дни с %s ждут фактов", reasonPending, week, why, day)
			case p.unmapped[t.model+"/"+t.name]:
				reason = fmt.Sprintf("%s: человек не сопоставлен с пользователем хаба — добавьте в aliases карточки имя хаба или hub:<id>; дни недели с %s ждут", reasonPending, day)
			default:
				continue
			}
			p.markers = append(p.markers, unenteredRow{model: t.model, name: t.name, person: t.person, date: day, page: "—",
				status: "unknown", reason: reason})
		}
	}
	keys := make([]string, 0, len(p.unpriced))
	for k := range p.unpriced {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		model, name, date, ok := splitDayKey(k)
		if !ok || date < first || date > last {
			continue
		}
		person := ""
		for _, t := range tabs {
			if t.model == model && t.name == name {
				person = t.person
			}
		}
		p.markers = append(p.markers, unenteredRow{model: model, name: name, person: person, date: date, page: "—", status: "unknown",
			reason: fmt.Sprintf("%s: доли продаж на %s не посчитаны — %s", reasonPending, money.Micros(unpricedNet[k]).Cents(), p.unpriced[k])})
	}
	return nil
}

// postedDays are the tab-days with an active accrual on or after a day, by "<model>/<name>:<date>", with the source.
func postedDays(ctx context.Context, q ledger.Querier, from string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(e.source_key, '') FROM entries e
		WHERE e.kind = ? AND substr(e.effective_at, 1, 10) >= ? AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`,
		KindAccrual, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		if day, _, source, ok := parseAccrualKey(key); ok {
			out[day] = source
		}
	}
	return out, rows.Err()
}

// MirrorDay is a day from the switch on where the sheet, now a mirror, differs from what the books accrued.
type MirrorDay struct {
	Person      string `json:"person"`
	Name        string `json:"name"`
	Date        string `json:"date"`
	SheetMicros int64  `json:"sheet_micros"`
	BooksMicros int64  `json:"books_micros"`
}

// mirrorSums are a person's days from the switch on: what the books accrued and what the sheet says, by day.
type mirrorSums struct {
	books, sheet map[string]money.Micros
}

func (m *mirrorSums) total() (books, sheet money.Micros) {
	for _, v := range m.books {
		books += v
	}
	for _, v := range m.sheet {
		sheet += v
	}
	return books, sheet
}

// mirror reads every person's days from the switch on, books and sheet.
func mirror(ctx context.Context, q ledger.Querier, from string) (map[string]*mirrorSums, error) {
	out := map[string]*mirrorSums{}
	get := func(slug string) *mirrorSums {
		if out[slug] == nil {
			out[slug] = &mirrorSums{books: map[string]money.Micros{}, sheet: map[string]money.Micros{}}
		}
		return out[slug]
	}
	rows, err := q.QueryContext(ctx, `SELECT substr(l.account, 9), substr(e.effective_at, 1, 10), -SUM(l.amount)
		FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE e.kind = ? AND l.account LIKE 'payable:%' AND substr(e.effective_at, 1, 10) >= ?
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		GROUP BY 1, 2`, KindAccrual, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var slug, date string
		var sum int64
		if err := rows.Scan(&slug, &date, &sum); err != nil {
			rows.Close()
			return nil, err
		}
		get(slug).books[date] += money.Micros(sum)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT person, date, amount FROM payroll_days WHERE person IS NOT NULL AND date >= ? AND amount <> 0`, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var slug, date string
		var amount float64
		if err := rows.Scan(&slug, &date, &amount); err != nil {
			rows.Close()
			return nil, err
		}
		get(slug).sheet[date] += money.FromFloat(amount)
	}
	rows.Close()
	return out, rows.Err()
}

// days are the person's days where the sheet and the books differ by a cent or more.
func (m *mirrorSums) days(person, name string) []MirrorDay {
	dates := map[string]bool{}
	for d := range m.books {
		dates[d] = true
	}
	for d := range m.sheet {
		dates[d] = true
	}
	var out []MirrorDay
	for d := range dates {
		if diff := m.books[d] - m.sheet[d]; diff >= 10_000 || diff <= -10_000 {
			out = append(out, MirrorDay{Person: person, Name: name, Date: d, SheetMicros: int64(m.sheet[d]), BooksMicros: int64(m.books[d])})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// sourceSuffix marks a facts accrual's source_key; a sheet's has none, as before the switch.
const sourceSuffix = ":" + SourceFacts

func formatCounter(n int, source string) string {
	s := strconv.Itoa(n)
	if source == SourceFacts {
		s += sourceSuffix
	}
	return s
}

// factCell is a person's day on a model by the facts: the net of their shares by Hub page (the base the rate takes).
type factCell struct {
	pages []PayoutPage
	value int64
}

// factCells reads fact_accruals of the sales in [from, to] by "<person>\x00<model slug>\x00<day>".
func factCells(ctx context.Context, q ledger.Querier, from, to string) (map[string]factCell, error) {
	rows, err := q.QueryContext(ctx, `SELECT person, model, day, page_label, SUM(net_micros) FROM fact_accruals
		WHERE person IS NOT NULL AND kind = 'sales' AND day BETWEEN ? AND ? GROUP BY person, model, day, page_label
		ORDER BY person, model, day, page_label`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]factCell{}
	for rows.Next() {
		var person, model, day, page string
		var net int64
		if err := rows.Scan(&person, &model, &day, &page, &net); err != nil {
			return nil, err
		}
		k := person + "\x00" + ledger.Slug(model) + "\x00" + day
		c := out[k]
		c.pages = append(c.pages, PayoutPage{Page: page, RevenueMicros: net})
		c.value += net
		out[k] = c
	}
	return out, rows.Err()
}
