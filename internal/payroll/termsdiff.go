package payroll

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
)

// The comparison of the registry's terms with the tabs' headers (`ledger payroll terms diff`, the health check): for
// every working person with a payouts tab (people.status active) and every working person of the registry
// (hub_people.state working) with a term on the day, the term the registry says holds on the day against the terms the
// tab's header gives the ledger, field by field. With settings.payroll_terms_hub_from set it also lists the days from it
// that accrued by the tab's header for lack of a registry term.

// States of a row of the comparison.
const (
	TermsEqual   = "equal"
	TermsDiffer  = "differs"
	TermsTabOnly = "tab_only" // a tab, no registry term on the day
	TermsHubOnly = "hub_only" // a registry term, no tab of the model
)

// TermsRow is a person's tab of a model (or the registry's term without a tab) on the day.
type TermsRow struct {
	Person string `json:"person"`
	Model  string `json:"model"` // as ledger names it
	Tab    string `json:"tab,omitempty"`
	// TabRate is «Ставка %» of the tab's header; nil — none, or no tab. TabFixMicros is «Фикс/см»; nil — no tab.
	TabRate      *float64 `json:"tabRate"`
	TabFixMicros *int64   `json:"tabFixMicros"`
	// HubTerm is the registry's term holding on the day (hub_pay_terms.hub_id); nil — none.
	HubTerm      *int64 `json:"hubTerm"`
	HubValidFrom string `json:"hubValidFrom,omitempty"`
	HubShareBP   *int64 `json:"hubShareBasisPoints"`
	HubFixMicros *int64 `json:"hubFixMicros"`
	State        string `json:"state"`
	// Differences say each field that differs, the registry first: «доля 25% ≠ 22%», «фикс 0 ≠ 15».
	Differences []string `json:"differences"`
}

// TermsFallback is a person's tab whose days from payroll_terms_hub_from accrued by the header for lack of a registry term.
type TermsFallback struct {
	Person string `json:"person"`
	Model  string `json:"model"`
	Tab    string `json:"tab"`
	First  string `json:"first"`
	Last   string `json:"last"`
	Days   int    `json:"days"`
}

// TermsDiff is the comparison on a day.
type TermsDiff struct {
	Day  string       `json:"day"`
	From string       `json:"from"` // settings.payroll_terms_hub_from; "" — the headers price every day
	Read *TermsStatus `json:"read"` // the last pass of the job hub-terms; nil — never read
	Rows []TermsRow   `json:"rows"`
	// Equal and Differ count the rows: Differ is every row that is not equal (a field differs, or one side only).
	Equal    int             `json:"equal"`
	Differ   int             `json:"differ"`
	Fallback []TermsFallback `json:"fallback"`
}

// DifferText lists the rows that are not equal, the way the owner reads them: «alfa (Alpha) фикс 0 ≠ 15; …».
func (d TermsDiff) DifferText() string {
	var parts []string
	for _, r := range d.Rows {
		if r.State != TermsEqual {
			parts = append(parts, fmt.Sprintf("%s (%s) %s", r.Person, r.Model, strings.Join(r.Differences, ", ")))
		}
	}
	return strings.Join(parts, "; ")
}

// CompareTerms compares the registry's terms with the tabs' headers on the day.
func CompareTerms(ctx context.Context, q ledger.Querier, day string) (TermsDiff, error) {
	d := TermsDiff{Day: day, Rows: []TermsRow{}, Fallback: []TermsFallback{}}
	var err error
	if d.From, err = TermsHubFrom(ctx, q); err != nil {
		return d, err
	}
	if d.Read, err = ReadTermsStatus(ctx, q); err != nil {
		return d, err
	}
	tabs, err := LoadTerms(ctx, q)
	if err != nil {
		return d, err
	}
	hubs, err := LoadHubTerms(ctx, q)
	if err != nil {
		return d, err
	}
	active, err := slugSet(ctx, q, `SELECT slug FROM people WHERE status = 'active'`)
	if err != nil {
		return d, err
	}
	working, err := slugSet(ctx, q, `SELECT code FROM hub_people WHERE state = 'working'`)
	if err != nil {
		return d, err
	}
	type key struct{ person, model string }
	keys := map[key]string{} // → the model as named first
	for _, t := range tabs {
		if active[t.Person] {
			if _, ok := keys[key{t.Person, ledger.Slug(t.Model)}]; !ok {
				keys[key{t.Person, ledger.Slug(t.Model)}] = t.Model
			}
		}
	}
	for _, h := range hubs {
		if working[h.Person] && h.Holds(day) {
			if _, ok := keys[key{h.Person, ledger.Slug(h.Model)}]; !ok {
				keys[key{h.Person, ledger.Slug(h.Model)}] = h.Model
			}
		}
	}
	for k, model := range keys {
		h := hubs.On(k.person, model, day)
		found := false
		for _, t := range tabs {
			if t.Person != k.person || ledger.Slug(t.Model) != k.model {
				continue
			}
			found = true
			d.Rows = append(d.Rows, compareRow(&t, h, t.Model))
		}
		if !found {
			d.Rows = append(d.Rows, compareRow(nil, h, model))
		}
	}
	sort.Slice(d.Rows, func(i, j int) bool {
		a, b := d.Rows[i], d.Rows[j]
		if a.Person != b.Person {
			return a.Person < b.Person
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Tab < b.Tab
	})
	for _, r := range d.Rows {
		if r.State == TermsEqual {
			d.Equal++
		} else {
			d.Differ++
		}
	}
	if d.From != "" {
		if d.Fallback, err = termsFallback(ctx, q, d.From, tabs, hubs); err != nil {
			return d, err
		}
	}
	return d, nil
}

func slugSet(ctx context.Context, q ledger.Querier, query string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// compareRow compares a tab (nil — none) with the registry's term (nil — none).
func compareRow(t *Terms, h *HubTerm, model string) TermsRow {
	r := TermsRow{Model: model, Differences: []string{}}
	if t != nil {
		r.Person, r.Tab, r.TabRate = t.Person, t.Name, t.Rate
		fix := int64(money.FromFloat(t.ShiftFix))
		r.TabFixMicros = &fix
	}
	if h != nil {
		r.Person = h.Person
		id, bp, fix := h.ID, h.ShareBP, h.FixMicros
		r.HubTerm, r.HubValidFrom, r.HubShareBP, r.HubFixMicros = &id, h.ValidFrom, &bp, &fix
	}
	switch {
	case t == nil:
		r.State = TermsHubOnly
		r.Differences = append(r.Differences, "нет вкладки модели")
		return r
	case h == nil:
		r.State = TermsTabOnly
		r.Differences = append(r.Differences, "в реестре нет условия")
		return r
	}
	if r.TabRate == nil || !SameShare(*r.TabRate, h.ShareBP) {
		tab := "нет «Ставка %»"
		if r.TabRate != nil {
			tab = RatePercent(*r.TabRate)
		}
		r.Differences = append(r.Differences, "доля "+BasisPercent(h.ShareBP)+" ≠ "+tab)
	}
	if *r.TabFixMicros != h.FixMicros {
		r.Differences = append(r.Differences, "фикс "+Dollars(h.FixMicros)+" ≠ "+Dollars(*r.TabFixMicros))
	}
	r.State = TermsEqual
	if len(r.Differences) > 0 {
		r.State = TermsDiffer
	}
	return r
}

// SameShare says a header's rate is the registry's share to the basis point.
func SameShare(rate float64, bp int64) bool {
	x := rate * 10_000
	return math.Abs(x-math.Round(x)) < 1e-6 && int64(math.Round(x)) == bp
}

// BasisPercent writes a share in basis points as a percent: 2500 — «25%», 2250 — «22.5%».
func BasisPercent(bp int64) string {
	return strconv.FormatFloat(float64(bp)/100, 'f', -1, 64) + "%"
}

// RatePercent writes a header's rate as a percent: 0.25 — «25%».
func RatePercent(rate float64) string {
	return strconv.FormatFloat(money.RoundN(rate*100, 6), 'f', -1, 64) + "%"
}

// Dollars writes a fix in whole dollars when it is one: «0», «15», «12.5».
func Dollars(micros int64) string {
	return strconv.FormatFloat(float64(micros)/money.Scale, 'f', -1, 64)
}

// termsFallback lists the days from the day on that the source after the sheets accrued (report_accruals, or
// fact_accruals under the facts) on a person's tab by its header, for lack of a registry term holding on the day.
func termsFallback(ctx context.Context, q ledger.Querier, from string, tabs []Terms, hubs HubTerms) ([]TermsFallback, error) {
	_, source, err := AfterFrom(ctx, q)
	if err != nil {
		return nil, err
	}
	query := `SELECT DISTINCT person, model, day FROM report_accruals WHERE day >= ? ORDER BY person, model, day`
	if source == SourceFacts {
		query = `SELECT DISTINCT person, model, day FROM fact_accruals WHERE person IS NOT NULL AND day >= ? ORDER BY person, model, day`
	}
	rows, err := q.QueryContext(ctx, query, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TermsFallback{}
	byKey := map[string]int{}
	for rows.Next() {
		var person, model, day string
		if err := rows.Scan(&person, &model, &day); err != nil {
			return nil, err
		}
		var tab *Terms
		for i := range tabs {
			if tabs[i].Person == person && ledger.Slug(tabs[i].Model) == ledger.Slug(model) {
				tab = &tabs[i]
				break
			}
		}
		if tab == nil || hubs.On(person, model, day) != nil {
			continue // no tab: priced by nobody; a registry term: priced by it
		}
		k := person + "\x00" + ledger.Slug(model)
		i, ok := byKey[k]
		if !ok {
			i = len(out)
			byKey[k] = i
			out = append(out, TermsFallback{Person: person, Model: tab.Model, Tab: tab.Name, First: day})
		}
		out[i].Last = day
		out[i].Days++
	}
	return out, rows.Err()
}
