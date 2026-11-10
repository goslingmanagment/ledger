package payroll

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payables", auth.Require(s, auth.ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			list, err := Payables(r.Context(), s.Store.RO, r.URL.Query().Get("person"))
			var check CheckReport
			if err == nil {
				check, err = Check(r.Context(), s.Store.RO)
			}
			if err != nil {
				s.Log.Error("payables", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "долги не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, Answer{OK: true, Payables: list, Check: check})
		})))
	})
}

// Payable is a row of v_payables.
type Payable struct {
	Person           string   `json:"person"`
	Name             string   `json:"name"`
	BalanceMicros    int64    `json:"balance_micros"`
	Balance          float64  `json:"balance"`
	ToPay            *float64 `json:"to_pay"`
	Difference       *float64 `json:"difference"`
	OldestUnpaid     string   `json:"oldest_unpaid,omitempty"`
	UnpaidDays       string   `json:"unpaid_days,omitempty"`
	UnenteredDays    int      `json:"unentered_days"`
	UnenteredNet     float64  `json:"unentered_net"`
	UnenteredShare   float64  `json:"unentered_share"`
	UnenteredUnknown int      `json:"unentered_unknown"`
	Tabs             int      `json:"tabs"`
	TabsStatus       string   `json:"tabs_status,omitempty"`
	FetchedAt        string   `json:"fetched_at,omitempty"`
}

// Answer is the reply of GET /v1/payables.
type Answer struct {
	OK       bool        `json:"ok"`
	Payables []Payable   `json:"payables"`
	Check    CheckReport `json:"check"`
}

// Payables reads v_payables; person "" means everybody, otherwise a slug or a name.
func Payables(ctx context.Context, q ledger.Querier, person string) ([]Payable, error) {
	query := `SELECT person, name, balance_micros, balance, to_pay, difference, COALESCE(oldest_unpaid, ''), COALESCE(unpaid_days, ''),
		unentered_days, unentered_net, unentered_share, unentered_unknown, COALESCE(tabs, 0), COALESCE(tabs_status, ''), COALESCE(fetched_at, '')
		FROM v_payables`
	var args []any
	if person != "" {
		query += ` WHERE person = ? OR name = ?`
		args = []any{person, person}
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY balance_micros DESC, person`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payable{}
	for rows.Next() {
		var p Payable
		var toPay, diff sql.NullFloat64
		if err := rows.Scan(&p.Person, &p.Name, &p.BalanceMicros, &p.Balance, &toPay, &diff, &p.OldestUnpaid, &p.UnpaidDays,
			&p.UnenteredDays, &p.UnenteredNet, &p.UnenteredShare, &p.UnenteredUnknown, &p.Tabs, &p.TabsStatus, &p.FetchedAt); err != nil {
			return nil, err
		}
		if toPay.Valid {
			p.ToPay = &toPay.Float64
		}
		if diff.Valid {
			p.Difference = &diff.Float64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// tolerance is how far a debt may stand from «К выплате» (the sheets round): $1.
const tolerance = money.Scale

// PersonCheck is one person's debt against their tabs. From the switch to Hub's facts on, Diff is what the sheet still
// governs: the debt with the switch's days as the sheet has them (Payable − Facts + Sheet) against «К выплате».
type PersonCheck struct {
	Person  string       `json:"person"`
	Name    string       `json:"name"`
	Payable money.Micros `json:"payable"`
	ToPay   float64      `json:"to_pay"`
	Diff    money.Micros `json:"diff"`
	OK      bool         `json:"ok"`
	Reason  string       `json:"reason,omitempty"`
	// Facts is what the books accrued from the switch day on (from the facts or the reports, whichever is on); Sheet what
	// the mirror has for those days.
	Facts money.Micros `json:"facts,omitempty"`
	Sheet money.Micros `json:"sheet,omitempty"`
}

// TabProblem is a table or tab whose state is not to be trusted.
type TabProblem struct {
	Model  string `json:"model"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// CheckReport is what the checks of the next PRs read: per person payable against К выплате, and every
// row or tab the debts could not take in. OK means nothing is out of line.
type CheckReport struct {
	OK       bool          `json:"ok"`
	TabsRead int           `json:"tabs_read"` // 0: the sheets were never collected, nothing below means much
	People   []PersonCheck `json:"people"`
	Rows     []Unresolved  `json:"rows"`
	Tabs     []TabProblem  `json:"tabs"`
	// FactsFrom is the switch to Hub's facts (settings.payroll_facts_from); "" — the sheets accrue everything. From it
	// the sheet is a mirror: Mirror are its days that differ from the books, Unaccrued the facts no tab takes. Both are
	// information, not failures.
	FactsFrom string `json:"facts_from,omitempty"`
	// ReportsFrom is the switch to the shift reports (settings.payroll_reports_from), the same way; only one of the two.
	ReportsFrom string      `json:"reports_from,omitempty"`
	Mirror      []MirrorDay `json:"mirror"`
	Unaccrued   []Unaccrued `json:"unaccrued"`
	// Pending are the changes of paid days waiting for the owner (payroll_pending_changes, open).
	Pending []PendingChange `json:"pending_changes"`
}

// Check compares the debts with the sheets. A person with a tab in error or without any tab is not OK:
// the comparison has nothing to stand on. From the switch to Hub's facts on it compares what the sheet still governs.
func Check(ctx context.Context, q ledger.Querier) (CheckReport, error) {
	rep := CheckReport{OK: true, People: []PersonCheck{}, Rows: []Unresolved{}, Tabs: []TabProblem{}, Mirror: []MirrorDay{},
		Unaccrued: []Unaccrued{}, Pending: []PendingChange{}}
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return rep, err
	}
	list, err := Payables(ctx, q, "")
	if err != nil {
		return rep, err
	}
	after, source, err := AfterFrom(ctx, q)
	if err != nil {
		return rep, err
	}
	if source == SourceReports {
		rep.ReportsFrom = after
	} else {
		rep.FactsFrom = after
	}
	var mir map[string]*mirrorSums
	if after != "" {
		if mir, err = mirror(ctx, q, after); err != nil {
			return rep, err
		}
		plan, err := planAfter(ctx, q, ts.Now())
		if err != nil {
			return rep, err
		}
		rep.Unaccrued = append(rep.Unaccrued, plan.lost()...)
	}
	if rep.Pending, err = OpenChanges(ctx, q, ""); err != nil {
		return rep, err
	}
	for _, p := range list {
		if _, isPerson := people[p.Person]; !isPerson && p.Tabs == 0 {
			continue // a model's share of a deal: no tab to compare with
		}
		c := PersonCheck{Person: p.Person, Name: p.Name, Payable: money.Micros(p.BalanceMicros), OK: true}
		if p.ToPay != nil {
			c.ToPay = *p.ToPay
		}
		c.Diff = c.Payable - money.FromFloat(c.ToPay)
		if m := mir[p.Person]; m != nil {
			c.Facts, c.Sheet = m.total()
			c.Diff += c.Sheet - c.Facts
			rep.Mirror = append(rep.Mirror, m.days(p.Person, p.Name)...)
		}
		switch {
		case p.Tabs == 0 && c.Payable != 0:
			c.OK, c.Reason = false, "долг есть, вкладки нет"
		case p.TabsStatus == "error":
			c.OK, c.Reason = false, "вкладка в ошибке: «К выплате» от последнего удачного разбора"
		case c.Diff > tolerance || c.Diff < -tolerance:
			c.OK, c.Reason = false, fmt.Sprintf("долг расходится с «К выплате» на %s", c.Diff.Cents())
		}
		if !c.OK {
			rep.OK = false
		}
		rep.People = append(rep.People, c)
	}
	sort.SliceStable(rep.People, func(i, j int) bool { return !rep.People[i].OK && rep.People[j].OK })
	res, err := NewResolver(ctx, q)
	if err != nil {
		return rep, err
	}
	if from, err := AccrualFrom(ctx, q); err != nil {
		rep.Tabs = append(rep.Tabs, TabProblem{Model: "настройки", Reason: err.Error()})
	} else {
		_, bad, err := payoutRows(ctx, q, from, res)
		if err != nil {
			return rep, err
		}
		rep.Rows = append(rep.Rows, bad...)
	}
	rows, err := q.QueryContext(ctx, `SELECT model, status, COALESCE(error, '') FROM payroll_tables WHERE status <> 'ok' ORDER BY model`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var t TabProblem
		var status string
		if err := rows.Scan(&t.Model, &status, &t.Reason); err != nil {
			rows.Close()
			return rep, err
		}
		rep.Tabs = append(rep.Tabs, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, err
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT model, name, COALESCE(person, ''), status, COALESCE(error, ''), listed FROM payroll_tabs ORDER BY model, name`)
	if err != nil {
		return rep, err
	}
	type row struct {
		model, name, person, status, errText string
		listed                               bool
	}
	var tabs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.model, &r.name, &r.person, &r.status, &r.errText, &r.listed); err != nil {
			rows.Close()
			return rep, err
		}
		tabs = append(tabs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, err
	}
	rows.Close()
	rep.TabsRead = len(tabs)
	for _, r := range tabs {
		if r.status != "ok" {
			rep.Tabs = append(rep.Tabs, TabProblem{r.model, r.name, r.errText})
		}
		if r.person == "" {
			_, err := res.Resolve(r.model, r.name)
			reason := "имя не сопоставлено с человеком"
			if err != nil {
				reason = err.Error()
			}
			rep.Tabs = append(rep.Tabs, TabProblem{r.model, r.name, reason})
		}
		if !r.listed {
			rep.Tabs = append(rep.Tabs, TabProblem{r.model, r.name, "вкладки больше нет в списках «Итого» и «Неактивные»: её начисления стоят, пока не решит человек"})
		}
	}
	if len(rep.Rows) > 0 || len(rep.Tabs) > 0 {
		rep.OK = false
	}
	return rep, nil
}
