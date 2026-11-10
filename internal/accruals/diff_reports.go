package accruals

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
)

// The week of `ledger accruals diff` with the shift reports beside the sheet and the facts (R2): per person and day the
// sheet, the reports and the facts, the differences sheet ↔ reports and reports ↔ facts, and for a day the sheet and
// the reports differ on, its pages with both amounts. A difference under a dollar on a page whose sheet cell is a whole
// number is «округление таблицы»: shown, not counted in the totals.

// Presence of a day on one side only.
const (
	PresenceSheetOnly  = "в таблице есть, отчёта нет"
	PresenceReportOnly = "отчёт есть, в таблице нет"
)

// extraPeople adds the people of the week that v_accrual_diff does not list: those with reports only, and — when the
// week has no facts at all — those with sheet days.
func extraPeople(ctx context.Context, q ledger.Querier, d *Diff, known bool) error {
	have := map[string]bool{}
	for _, p := range d.People {
		have[p.Person] = true
	}
	rows, err := q.QueryContext(ctx, `SELECT person, COALESCE(SUM(sheet), 0) FROM (
		SELECT person, CAST(ROUND(amount * 1000000) AS INTEGER) AS sheet FROM payroll_days
		 WHERE person IS NOT NULL AND date BETWEEN ? AND ? AND amount <> 0
		UNION ALL SELECT person, 0 FROM report_accruals WHERE day BETWEEN ? AND ?)
		GROUP BY person ORDER BY person`, d.Week, d.Until, d.Week, d.Until)
	if err != nil {
		return err
	}
	defer rows.Close()
	names, err := personNames(ctx, q)
	if err != nil {
		return err
	}
	added := false
	for rows.Next() {
		var person string
		var sheet int64
		if err := rows.Scan(&person, &sheet); err != nil {
			return err
		}
		if have[person] {
			continue
		}
		p := PersonDiff{Person: person, Name: names[person], SheetMicros: sheet}
		if p.Name == "" {
			p.Name = person
		}
		if known {
			zero, diff, net, unpriced := int64(0), -sheet, int64(0), int64(0)
			p.FactsMicros, p.DiffMicros, p.FactsNetMicros, p.UnpricedMicros = &zero, &diff, &net, &unpriced
		}
		d.People = append(d.People, p)
		added = true
	}
	if added {
		sort.SliceStable(d.People, func(i, j int) bool { return d.People[i].Person < d.People[j].Person })
	}
	return rows.Err()
}

type reportRow struct {
	model  string
	day    string
	net    int64
	amount *int64
	rate   *float64
	reason string
	pages  map[string]int64
	potNet int64
	pots   []PotShare
}

// addReports puts the week's report_accruals beside each person's days.
func addReports(ctx context.Context, q ledger.Querier, d *Diff, factsKnown bool) error {
	var err error
	if d.ReportsFrom, err = payroll.ReportsFrom(ctx, q); err != nil {
		return err
	}
	byPerson := map[string][]reportRow{}
	rows, err := q.QueryContext(ctx, `SELECT person, model, day, net_micros, amount_micros, rate, COALESCE(reason, ''), pages, pot_micros, pots
		FROM report_accruals WHERE day BETWEEN ? AND ? ORDER BY person, day, model`, d.Week, d.Until)
	if err != nil {
		return err
	}
	for rows.Next() {
		var person, pages, pots string
		var r reportRow
		var amount sql.NullInt64
		var rate sql.NullFloat64
		if err := rows.Scan(&person, &r.model, &r.day, &r.net, &amount, &rate, &r.reason, &pages, &r.potNet, &pots); err != nil {
			rows.Close()
			return err
		}
		if amount.Valid {
			v := amount.Int64
			r.amount = &v
		}
		if rate.Valid {
			v := rate.Float64
			r.rate = &v
		}
		var list []reportPage
		_ = json.Unmarshal([]byte(pages), &list)
		_ = json.Unmarshal([]byte(pots), &r.pots)
		r.pages = map[string]int64{}
		for _, p := range list {
			r.pages[p.Page] += p.Micros
		}
		byPerson[person] = append(byPerson[person], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	label, err := payroll.PageLabeler(ctx, q)
	if err != nil {
		return err
	}
	for i := range d.People {
		p := &d.People[i]
		cells, err := sheetCellsOf(ctx, q, p.Person, d.Week, d.Until, label)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, x := range p.Days {
			known[x.Day] = true
		}
		for _, r := range byPerson[p.Person] {
			if !known[r.day] {
				known[r.day] = true
				p.Days = append(p.Days, DayDiff{Day: r.day})
			}
		}
		days := map[string]*DayDiff{}
		for j := range p.Days {
			days[p.Days[j].Day] = &p.Days[j]
		}
		rates := map[string]map[string]*float64{} // day → page → rate of its report row
		notes := map[string]bool{}
		for _, r := range byPerson[p.Person] {
			x := days[r.day]
			x.hasReport = true
			x.ReportRevenueMicros += r.net
			p.ReportsNetMicros += r.net
			x.PotMicros += r.potNet
			for _, pot := range r.pots {
				x.Pots = append(x.Pots, pot)
				x.PotNotes = append(x.PotNotes, PotNote(pot.Name, pot.Micros))
			}
			if r.amount != nil {
				x.ReportsMicros += *r.amount
				p.ReportsMicros += *r.amount
			} else {
				p.ReportsUnpricedMicros += r.net
				notes["по отчётам без ставки: "+r.reason] = true
			}
			if rates[r.day] == nil {
				rates[r.day] = map[string]*float64{}
			}
			for page := range r.pages {
				rates[r.day][page] = r.rate
			}
		}
		for n := range notes {
			p.Notes = append(p.Notes, n)
		}
		sort.Strings(p.Notes)
		for j := range p.Days {
			x := &p.Days[j]
			x.SheetReportsDiffMicros = x.ReportsMicros - x.SheetMicros
			if factsKnown {
				v := x.FactsMicros - x.ReportsMicros
				x.ReportsFactsDiffMicros = &v
			}
			switch {
			case x.hasSheet && !x.hasReport:
				x.Presence = PresenceSheetOnly
			case x.hasReport && !x.hasSheet:
				x.Presence = PresenceReportOnly
			}
			if differs(x.SheetReportsDiffMicros) {
				reported := map[string]int64{}
				for _, r := range byPerson[p.Person] {
					if r.day == x.Day {
						for page, v := range r.pages {
							reported[page] += v
						}
					}
				}
				x.Pages = pageDiffs(cells[x.Day], reported)
				for _, pg := range x.Pages {
					if !pg.Rounding {
						continue
					}
					if r := rates[x.Day][pg.Page]; r != nil {
						x.RoundingMicros += int64(money.FromFloat(float64(float64(*pg.ReportMicros-*pg.SheetMicros)/money.Scale) * *r))
					}
				}
			}
			counted := x.SheetReportsDiffMicros - x.RoundingMicros
			if !differs(counted) {
				counted = 0
			}
			p.SheetReportsDiffMicros += x.SheetReportsDiffMicros
			p.SheetReportsCountedMicros += counted
		}
		if factsKnown && p.FactsMicros != nil {
			v := *p.FactsMicros - p.ReportsMicros
			p.ReportsFactsDiffMicros = &v
		}
		sort.Slice(p.Days, func(i, j int) bool { return p.Days[i].Day < p.Days[j].Day })
	}
	return nil
}

// sheetCellsOf reads the person's sheet cells of the week by day and Hub page.
func sheetCellsOf(ctx context.Context, q ledger.Querier, person, from, to string, label func(string) string) (map[string]map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT date, pages FROM payroll_days WHERE person = ? AND date BETWEEN ? AND ?`, person, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var day, pages string
		if err := rows.Scan(&day, &pages); err != nil {
			return nil, err
		}
		var cells map[string]*float64
		_ = json.Unmarshal([]byte(pages), &cells)
		for page, v := range cells {
			if v == nil || *v == 0 {
				continue
			}
			if out[day] == nil {
				out[day] = map[string]int64{}
			}
			out[day][label(page)] += int64(money.FromFloat(*v))
		}
	}
	return out, rows.Err()
}

// pageDiffs puts the sheet's cells and the posted amounts of a day side by side, by page.
func pageDiffs(sheet, reported map[string]int64) []PageDiff {
	pages := map[string]bool{}
	for p := range sheet {
		pages[p] = true
	}
	for p := range reported {
		pages[p] = true
	}
	out := make([]PageDiff, 0, len(pages))
	for page := range pages {
		pd := PageDiff{Page: page}
		if v, ok := sheet[page]; ok {
			pd.SheetMicros = &v
		}
		if v, ok := reported[page]; ok {
			pd.ReportMicros = &v
		}
		if pd.SheetMicros != nil && pd.ReportMicros != nil {
			diff := *pd.ReportMicros - *pd.SheetMicros
			pd.Rounding = diff != 0 && diff < money.Scale && diff > -money.Scale && *pd.SheetMicros%money.Scale == 0
		}
		out = append(out, pd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out
}
