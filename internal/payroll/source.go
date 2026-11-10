package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A source after the sheets accrues a person's days from a switch day on; the sheet becomes a mirror for them. There
// are two: Hub's facts (L4, settings.payroll_facts_from, factPlan) and the shift reports (R2, settings.payroll_reports_from,
// reportPlan). At most one is on. Both share how the books follow them (reconcileAccruals): the days the source wants are
// posted on the person's tab of the model with the source in the source_key, a day it cannot answer for keeps what is
// posted (holds) and stands in «не занесено» as unknown with the reason (marks), and what no tab takes is listed
// (lost). The reports add one rule the facts never had: a day a payout reached does not change by itself.

// SettingReportsFrom is the first Moscow day accrued from the shift reports; empty or absent — not from them.
const SettingReportsFrom = "payroll_reports_from"

// SourceReports is a day accrued from the shift reports (report_accruals).
const SourceReports = "reports"

// Reasons of the reports' days in «не занесено».
const (
	// reasonReportPending opens the reason of a day the reports did not accrue (held: kept as posted).
	reasonReportPending = "по отчёту не начислено"
	// reasonShiftNoReport opens the reason of a worked shift without a posted report.
	reasonShiftNoReport = "смена без отчёта"
)

// afterPlan is what a source after the sheets says about every tab's day from its switch day on.
type afterPlan interface {
	source() string
	start() string
	// wanted are the days the source accrues, by "<model>/<name>:<date>".
	wanted() map[string]factDay
	// holds says the source cannot answer for the day: it keeps what is posted.
	holds(model, name, date string) bool
	// marks are the rows of «не занесено» the source adds: the days it did not accrue, unknown.
	marks() []unenteredRow
	// lost is what the source has that no tab takes.
	lost() []Unaccrued
}

// ReportsFrom is the switch day to the reports, "" when they do not accrue.
func ReportsFrom(ctx context.Context, q ledger.Querier) (string, error) {
	return settingDay(ctx, q, SettingReportsFrom)
}

// SetReportsFrom sets the switch day to the reports ("" turns them off). It must not precede the accrual date, and the
// facts must be off: only one source after the sheets.
func SetReportsFrom(ctx context.Context, q ledger.Querier, day string, now time.Time) error {
	if day != "" {
		t, err := time.Parse(ts.DateLayout, day)
		if err != nil || t.Format(ts.DateLayout) != day {
			return fmt.Errorf("дата переключения: нужна ГГГГ-ММ-ДД, а не %q", day)
		}
		from, err := AccrualFrom(ctx, q)
		if err != nil {
			return err
		}
		if day < from {
			return fmt.Errorf("дата переключения %s раньше accrual_from %s", day, from)
		}
		other, err := FactsFrom(ctx, q)
		if err != nil {
			return err
		}
		if other != "" {
			return fmt.Errorf("уже включены начисления из фактов хаба с %s: два источника после таблиц сразу нельзя — сначала ledger payroll facts-from --off", other)
		}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingReportsFrom, day, reportsFromNote, ts.Format(now))
	return err
}

const reportsFromNote = "С какого дня (МСК) начисления людям — из отчётов смен (report_accruals), а не из таблиц выплат; пусто — только таблицы. " +
	"Ставит и снимает ledger payroll reports-from; откат — --off и пересборка. Вместе с payroll_facts_from не ставится"

// AfterFrom is the switch day of whichever source after the sheets is on and its name; "", "" — the sheets only.
func AfterFrom(ctx context.Context, q ledger.Querier) (day, source string, err error) {
	facts, err := FactsFrom(ctx, q)
	if err != nil {
		return "", "", err
	}
	reports, err := ReportsFrom(ctx, q)
	if err != nil {
		return "", "", err
	}
	switch {
	case facts != "" && reports != "":
		return "", "", fmt.Errorf("включены оба источника после таблиц: %s с %s и %s с %s — оставьте один (ledger payroll facts-from --off или reports-from --off)",
			SettingFactsFrom, facts, SettingReportsFrom, reports)
	case facts != "":
		return facts, SourceFacts, nil
	case reports != "":
		return reports, SourceReports, nil
	}
	return "", "", nil
}

// planAfter reads the source after the sheets that is on; nil — the sheets accrue everything.
func planAfter(ctx context.Context, q ledger.Querier, now time.Time) (afterPlan, error) {
	from, source, err := AfterFrom(ctx, q)
	if err != nil {
		return nil, err
	}
	switch source {
	case SourceFacts:
		return planFacts(ctx, q, from, now)
	case SourceReports:
		return planReports(ctx, q, from, now)
	}
	return nil, nil
}

// sourceName says a source after the sheets in Russian.
func sourceName(source string) string {
	if source == SourceReports {
		return "отчёты смен"
	}
	return "факты хаба"
}

// ---- the reports ----

// reportBasis is what a reports day was counted from: the posted revenue and the terms.
type reportBasis struct {
	net, fix money.Micros
	rate     *float64
}

// reportPlan is what the shift reports say about every tab's day from the switch on.
type reportPlan struct {
	from      string
	want      map[string]factDay
	held      map[string]string // "<model>/<name>:<date>" → why the reports cannot answer for it
	markers   []unenteredRow
	unaccrued []Unaccrued
	// paid is what the payouts cleared of each day as posted, by "<model>/<name>:<date>" (loadGuard).
	paid map[string]money.Micros
	open map[string]PendingChange // the open pending changes by day
}

func (p *reportPlan) source() string             { return SourceReports }
func (p *reportPlan) start() string              { return p.from }
func (p *reportPlan) wanted() map[string]factDay { return p.want }
func (p *reportPlan) marks() []unenteredRow      { return p.markers }
func (p *reportPlan) lost() []Unaccrued          { return p.unaccrued }
func (p *reportPlan) holds(model, name, date string) bool {
	_, ok := p.held[model+"/"+name+":"+date]
	return ok
}

// blockingFlags are the flags of a post that keep it out of the pay base (as v_report_pages and reports.Blocking).
var blockingFlags = map[string]string{
	"unknown_author": "автор не узнан",
	"no_date":        "дату не прочитать",
	"sum_mismatch":   "строки не сходятся с «Общим доходом»",
	"date_mismatch":  "дата дальше суток от дня отправки",
}

// planReports reads report_accruals from the switch day on, keyed to each person's tab of the model, and what holds a
// day: a post of the day blocked by a flag or with a page not mapped to Hub, no rate to price it, or money of the day
// (in the sheet or posted) with no posted report.
func planReports(ctx context.Context, q ledger.Querier, from string, now time.Time) (*reportPlan, error) {
	p := &reportPlan{from: from, want: map[string]factDay{}, held: map[string]string{}}
	tabs, tabOf, err := planTabs(ctx, q)
	if err != nil {
		return nil, err
	}
	hold := func(key, why string) {
		if _, ok := p.held[key]; !ok {
			p.held[key] = why
		}
	}
	reported := map[string]bool{}
	rows, err := q.QueryContext(ctx, `SELECT person, model, day, net_micros, rate, fix_micros, amount_micros, COALESCE(reason, '')
		FROM report_accruals WHERE day >= ? ORDER BY person, model, day`, from)
	if err != nil {
		return nil, err
	}
	type lostKey struct{ person, model string }
	lost := map[lostKey]*Unaccrued{}
	for rows.Next() {
		var person, model, day, reason string
		var net, fix int64
		var rate sql.NullFloat64
		var amount sql.NullInt64
		if err := rows.Scan(&person, &model, &day, &net, &rate, &fix, &amount, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		t, ok := tabOf[person+"\x00"+ledger.Slug(model)]
		if model == "" || !ok {
			k := lostKey{person, model}
			u := lost[k]
			if u == nil {
				u = &Unaccrued{Person: person, Model: model, Reason: reason}
				if u.Reason == "" {
					u.Reason = "нет вкладки человека в таблице выплат модели «" + model + "»"
				}
				lost[k] = u
			}
			u.NetMicros += net
			u.Days++
			continue
		}
		key := t.model + "/" + t.name + ":" + day
		reported[key] = true
		if !amount.Valid {
			hold(key, fmt.Sprintf("нет ставки: %s — выручка по отчёту %s", reason, money.Micros(net).Cents()))
			continue
		}
		b := &reportBasis{net: money.Micros(net), fix: money.Micros(fix)}
		if rate.Valid {
			v := rate.Float64
			b.rate = &v
		}
		p.want[key] = factDay{model: t.model, name: t.name, person: t.person, date: day, amount: money.Micros(amount.Int64), basis: b}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, u := range lost {
		p.unaccrued = append(p.unaccrued, *u)
	}
	sort.Slice(p.unaccrued, func(i, j int) bool {
		a, b := p.unaccrued[i], p.unaccrued[j]
		if a.Person != b.Person {
			return a.Person < b.Person
		}
		return a.Model < b.Model
	})

	// The current posts of the days that cannot be read whole: blocked by a flag, or with a page in shift not mapped.
	models, err := HubPageModels(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT p.id, p.person, p.report_date, p.model, p.flags,
		COALESCE((SELECT json_group_array(json_array(COALESCE(pp.page_label, ''), pp.name, pp.in_shift)) FROM shift_report_post_pages pp
		  WHERE pp.post_id = p.id), '[]')
		FROM shift_report_posts p WHERE p.current = 1 AND p.person IS NOT NULL AND p.report_date >= ? ORDER BY p.id`, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var person, date, topicModel, flagsText, pagesText string
		if err := rows.Scan(&id, &person, &date, &topicModel, &flagsText, &pagesText); err != nil {
			rows.Close()
			return nil, err
		}
		var flags []string
		_ = json.Unmarshal([]byte(flagsText), &flags)
		var pages [][]any
		_ = json.Unmarshal([]byte(pagesText), &pages)
		var blocked []string
		for _, f := range flags {
			if text, ok := blockingFlags[f]; ok {
				blocked = append(blocked, text)
			}
		}
		pageModels := map[string]bool{}
		var unmapped []string
		for _, pg := range pages {
			if len(pg) != 3 {
				continue
			}
			label, _ := pg[0].(string)
			name, _ := pg[1].(string)
			in, _ := pg[2].(float64)
			m := topicModel
			if label != "" {
				if hm, ok := models[label]; ok {
					m = hm
				}
			} else if in == 1 {
				unmapped = append(unmapped, "«"+name+"»")
			}
			if m != "" {
				pageModels[m] = true
			}
		}
		if len(pageModels) == 0 && topicModel != "" {
			pageModels[topicModel] = true
		}
		var why string
		switch {
		case len(blocked) > 0:
			why = fmt.Sprintf("отчёт не разобран: пост %d — %s (ledger report posts --flagged)", id, strings.Join(blocked, "; "))
		case len(unmapped) > 0:
			why = fmt.Sprintf("отчёт не разобран: в посте %d страница %s не сопоставлена со страницей хаба (pages топика в report_topics)", id,
				strings.Join(unmapped, ", "))
		default:
			continue
		}
		for m := range pageModels {
			if t, ok := tabOf[person+"\x00"+ledger.Slug(m)]; ok {
				hold(t.model+"/"+t.name+":"+date, why)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Money of a day with no posted report: in the sheet, or posted already.
	rows, err = q.QueryContext(ctx, `SELECT model, name, date FROM payroll_days WHERE person IS NOT NULL AND amount <> 0 AND date >= ?`, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model, name, date string
		if err := rows.Scan(&model, &name, &date); err != nil {
			rows.Close()
			return nil, err
		}
		if key := model + "/" + name + ":" + date; !reported[key] {
			hold(key, "отчёта нет: в таблице день есть, опубликованного отчёта за него нет")
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	posted, err := postedDays(ctx, q, from)
	if err != nil {
		return nil, err
	}
	for key := range posted {
		if !reported[key] {
			hold(key, "отчёта нет: день проведён, опубликованного отчёта за него нет")
		}
	}
	return p, p.mark(ctx, q, tabs, now)
}

// mark lists the held days of the last 45 days and the worked shifts without a posted report for «не занесено».
func (p *reportPlan) mark(ctx context.Context, q ledger.Querier, tabs []planTab, now time.Time) error {
	nowMSK := now.In(ts.MSK)
	today := time.Date(nowMSK.Year(), nowMSK.Month(), nowMSK.Day(), 0, 0, 0, 0, ts.MSK)
	first := max(p.from, ts.Date(today.AddDate(0, 0, -unenteredDays)))
	last := ts.Date(today)
	if first > last {
		return nil
	}
	personOf := map[string]string{}
	for _, t := range tabs {
		personOf[t.model+"/"+t.name] = t.person
	}
	keys := make([]string, 0, len(p.held))
	for k := range p.held {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		model, name, date, ok := splitDayKey(k)
		if !ok || date < first || date > last {
			continue
		}
		p.markers = append(p.markers, unenteredRow{model: model, name: name, person: personOf[model+"/"+name], date: date, page: "—",
			status: "unknown", reason: reasonReportPending + ": " + p.held[k]})
	}
	gaps, err := ShiftsWithoutReport(ctx, q, first, last, now)
	if err != nil {
		return err
	}
	_, tabOf, err := planTabs(ctx, q)
	if err != nil {
		return err
	}
	for _, g := range gaps {
		t, ok := tabOf[g.Person+"\x00"+ledger.Slug(g.Model)]
		if !ok {
			continue
		}
		p.markers = append(p.markers, unenteredRow{model: t.model, name: t.name, person: t.person, date: g.ReportDate, page: "—",
			from: g.StartedAt, to: g.EndedAt, status: "unknown",
			reason: fmt.Sprintf("%s: смена %s — %s (хаб, %s), опубликованного отчёта за %s нет", reasonShiftNoReport,
				g.StartedAt.In(ts.MSK).Format("02.01 15:04"), g.EndedAt.In(ts.MSK).Format("02.01 15:04"), g.Username, ddmm(g.ReportDate))})
	}
	return nil
}

// ---- the report date of a shift ----

// ReportDate is the report date of a shift: the Moscow date of its start, except that a shift starting within two
// hours before midnight that mostly lies in the next day belongs to the next day — the team dates such a report by the
// day it mostly worked. The same rule links a report to a shift everywhere.
func ReportDate(start, end time.Time) string {
	s := start.In(ts.MSK)
	midnight := time.Date(s.Year(), s.Month(), s.Day()+1, 0, 0, 0, 0, ts.MSK)
	before := midnight.Sub(s)
	if before <= 2*time.Hour && end.After(midnight) && end.Sub(midnight) > before {
		return ts.Date(midnight)
	}
	return ts.Date(s)
}

// ShiftGap is a worked shift of Hub's team facts with no posted report for its person on its report date.
type ShiftGap struct {
	Person     string    `json:"person"`
	Model      string    `json:"model"`
	ReportDate string    `json:"reportDate"`
	UserID     int64     `json:"userId"`
	Username   string    `json:"username"`
	Ref        string    `json:"ref"`
	StartedAt  time.Time `json:"startedAt"`
	EndedAt    time.Time `json:"endedAt"`
	Pages      []string  `json:"pages"`
}

// ShiftsWithoutReport lists the shifts of ledger's mirror of Hub's team facts (hub_works, kind shift) whose report date
// is in [from, to], that ended more than 24 hours before now and whose person resolves, with no posted report page
// (v_report_pages) of that person on that date. One row per shift and model of its pages.
func ShiftsWithoutReport(ctx context.Context, q ledger.Querier, from, to string, now time.Time) ([]ShiftGap, error) {
	res, err := NewResolver(ctx, q)
	if err != nil {
		return nil, err
	}
	models, err := HubPageModels(ctx, q)
	if err != nil {
		return nil, err
	}
	// A work is in every window its days touch: the version that ended last stands.
	rows, err := q.QueryContext(ctx, `SELECT ref, payload FROM hub_works WHERE kind = ? AND substr(started_at, 1, 10) BETWEEN ? AND ?
		ORDER BY ref, ended_at`, split.Shift, addDays(from, -1), to)
	if err != nil {
		return nil, err
	}
	works := map[string]hub.TeamWork{}
	var refs []string
	for rows.Next() {
		var ref, payload string
		if err := rows.Scan(&ref, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		w, err := hub.DecodeTeamWork([]byte(payload))
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("hub_works %s: %w", ref, err)
		}
		if _, seen := works[ref]; !seen {
			refs = append(refs, ref)
		}
		works[ref] = w
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reported := map[string]bool{}
	rows, err = q.QueryContext(ctx, `SELECT DISTINCT person, report_date FROM v_report_pages WHERE posted_micros IS NOT NULL
		AND report_date BETWEEN ? AND ?`, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var person, date string
		if err := rows.Scan(&person, &date); err != nil {
			rows.Close()
			return nil, err
		}
		reported[person+"\x00"+date] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ShiftGap
	seen := map[string]bool{}
	for _, ref := range refs {
		w := works[ref]
		if w.Ongoing || now.Sub(w.EndedAt) < 24*time.Hour {
			continue
		}
		date := ReportDate(w.StartedAt, w.EndedAt)
		if date < from || date > to {
			continue
		}
		byModel := map[string][]string{}
		var order []string
		for _, pg := range w.Pages {
			m, ok := models[pg.PageLabel]
			if !ok {
				continue
			}
			if _, ok := byModel[m]; !ok {
				order = append(order, m)
			}
			byModel[m] = append(byModel[m], pg.PageLabel)
		}
		for _, m := range order {
			person, err := res.HubPerson(m, w.UserID, w.Username)
			if err != nil || reported[person+"\x00"+date] {
				continue
			}
			k := ref + "\x00" + ledger.Slug(m)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, ShiftGap{Person: person, Model: m, ReportDate: date, UserID: w.UserID, Username: w.Username, Ref: ref,
				StartedAt: w.StartedAt, EndedAt: w.EndedAt, Pages: byModel[m]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ReportDate != b.ReportDate {
			return a.ReportDate < b.ReportDate
		}
		if a.Person != b.Person {
			return a.Person < b.Person
		}
		return a.StartedAt.Before(b.StartedAt)
	})
	return out, nil
}
