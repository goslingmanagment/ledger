package accruals

import (
	"context"
	"encoding/json"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
)

// The week of `ledger accruals diff` with the pay terms of Hub's people registry (ecosystem H3): the reports days whose
// terms differ — the registry's term holding on the day against the tab's header, field by field, with what the day
// comes to by each — and, from settings.payroll_terms_hub_from, the days that kept the header for lack of a registry term.

// TermsDay is a person's reports day of a model whose terms differ.
type TermsDay struct {
	Person string `json:"person"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Tab    string `json:"tab"`
	Day    string `json:"day"`
	// Basis is what prices the day: hub — the registry's term (from TermsFrom), tab — the tab's header.
	Basis     string `json:"basis"`
	NetMicros int64  `json:"netMicros"` // the day's revenue: pages and pot shares
	// The tab's header and what the day comes to by it (nil — the header has no «Ставка %»).
	TabRate         *float64 `json:"tabRate"`
	TabFixMicros    int64    `json:"tabFixMicros"`
	TabAmountMicros *int64   `json:"tabAmountMicros"`
	// The registry's term holding on the day and what the day comes to by it; nil — the registry has none.
	HubTerm         *int64 `json:"hubTerm"`
	HubShareBP      *int64 `json:"hubShareBasisPoints"`
	HubFixMicros    *int64 `json:"hubFixMicros"`
	HubAmountMicros *int64 `json:"hubAmountMicros"`
	// Differences as `ledger payroll terms diff` says them, the registry first.
	Differences []string `json:"differences"`
}

// addTerms lists the week's reports days (report_accruals on a tab) whose registry term differs from the tab's header,
// and from TermsFrom the days with no registry term. A day before the registry's terms with no term is not listed.
func addTerms(ctx context.Context, q ledger.Querier, d *Diff) error {
	var err error
	if d.TermsFrom, err = payroll.TermsHubFrom(ctx, q); err != nil {
		return err
	}
	d.TermsDays = []TermsDay{}
	who, err := newPeople(ctx, q)
	if err != nil {
		return err
	}
	hubs, err := payroll.LoadHubTerms(ctx, q)
	if err != nil {
		return err
	}
	names, err := personNames(ctx, q)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT person, model, day, net_micros, pages FROM report_accruals
		WHERE day BETWEEN ? AND ? AND tab_model IS NOT NULL ORDER BY person, day, model`, d.Week, d.Until)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var person, model, day, pagesText string
		var net int64
		if err := rows.Scan(&person, &model, &day, &net, &pagesText); err != nil {
			return err
		}
		tab, reason := who.tabTerms(person, model)
		if reason != "" {
			continue // no tab, or tabs that disagree: the reports say so themselves
		}
		h := hubs.On(person, model, day)
		on := d.TermsFrom != "" && day >= d.TermsFrom
		if h == nil && !on {
			continue
		}
		var pages []json.RawMessage
		_ = json.Unmarshal([]byte(pagesText), &pages)
		tabFix := int64(money.FromFloat(tab.ShiftFix))
		x := TermsDay{Person: person, Name: names[person], Model: tab.Model, Tab: tab.Name, Day: day, Basis: "tab", NetMicros: net,
			TabRate: tab.Rate, TabFixMicros: tabFix, Differences: []string{}}
		if x.Name == "" {
			x.Name = person
		}
		fix := func(v int64) int64 {
			if len(pages) == 0 {
				return 0 // a day with only a pot share gets no fix
			}
			return v
		}
		if tab.Rate != nil {
			v := dayAmount(net, *tab.Rate, fix(tabFix))
			x.TabAmountMicros = &v
		}
		if h == nil {
			x.Differences = append(x.Differences, "в реестре нет условия")
		} else {
			id, bp, hf := h.ID, h.ShareBP, h.FixMicros
			v := dayAmount(net, h.Rate(), fix(hf))
			x.HubTerm, x.HubShareBP, x.HubFixMicros, x.HubAmountMicros = &id, &bp, &hf, &v
			if on {
				x.Basis = "hub"
			}
			if tab.Rate == nil || !payroll.SameShare(*tab.Rate, bp) {
				was := "нет «Ставка %»"
				if tab.Rate != nil {
					was = payroll.RatePercent(*tab.Rate)
				}
				x.Differences = append(x.Differences, "доля "+payroll.BasisPercent(bp)+" ≠ "+was)
			}
			if hf != tabFix {
				x.Differences = append(x.Differences, "фикс "+payroll.Dollars(hf)+" ≠ "+payroll.Dollars(tabFix))
			}
			if len(x.Differences) == 0 {
				continue
			}
		}
		d.TermsDays = append(d.TermsDays, x)
	}
	return rows.Err()
}
