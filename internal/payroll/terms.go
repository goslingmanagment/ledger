package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
)

// Terms are a person's pay terms on a model's tab as its header states them in the last good reading: the share of
// the revenue («Ставка %», payroll_tabs.rate) and the fix per shift («Фикс/см»). ParseTab counts the sheet's days by
// them; the accruals from Hub's facts count by the same.
type Terms struct {
	Model     string   `json:"model"`
	Name      string   `json:"name"`
	Person    string   `json:"person"`
	Rate      *float64 `json:"rate,omitempty"`
	ShareBP   *int64   `json:"-"` // exact basis points when backed by hub_pay_terms
	ShiftFix  float64  `json:"shiftFix,omitempty"`
	FixMicros *int64   `json:"-"` // exact micros when backed by hub_pay_terms
}

// Share computes the person's share of net micros using exact integer basis points when available.
func (t Terms) Share(net int64) int64 {
	if t.ShareBP != nil {
		return money.ShareBP(net, *t.ShareBP)
	}
	if t.Rate == nil {
		return 0
	}
	return money.ShareRate(net, *t.Rate)
}

// Fix returns the per-shift fix in micros without float64 round-trip when backed by Hub terms.
func (t Terms) Fix() int64 {
	if t.FixMicros != nil {
		return *t.FixMicros
	}
	return int64(money.FromFloat(t.ShiftFix))
}

// LoadTerms reads the terms of every tab that has a person and a good reading.
func LoadTerms(ctx context.Context, q ledger.Querier) ([]Terms, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.model, t.name, t.person, t.rate, e.payload FROM payroll_tabs t
		JOIN events e ON e.id = t.ok_event_id WHERE t.person IS NOT NULL ORDER BY t.model, t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Terms
	for rows.Next() {
		var t Terms
		var rate sql.NullFloat64
		var payload string
		if err := rows.Scan(&t.Model, &t.Name, &t.Person, &rate, &payload); err != nil {
			return nil, err
		}
		if rate.Valid {
			v := rate.Float64
			t.Rate = &v
		}
		dec := json.NewDecoder(strings.NewReader(payload))
		dec.UseNumber() // cells are json.Number, as the collector read them
		var p TabPayload
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("вкладка %s/%s: %w", t.Model, t.Name, err)
		}
		var first, second []Cell
		if len(p.Values) > 0 {
			first = p.Values[0]
		}
		if len(p.Values) > 1 {
			second = p.Values[1]
		}
		_, t.ShiftFix = headerTerms(first, second)
		out = append(out, t)
	}
	return out, rows.Err()
}
