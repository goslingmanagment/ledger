// Package month computes who covered what and the month card: a faithful port of the monthly card
// script, so its numbers can be compared to the cent. Payments, requests and transfers come from the
// hub mirrors and the journal; money here is float64 in dollars in the same order of operations as
// the script, because the card rounds only at the end. Ledger lines stay integer micro-dollars.
package month

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Fail is a data error: the numbers cannot be given, like the script's exit code 2.
type Fail struct{ Msg string }

func (f Fail) Error() string { return f.Msg }

func failf(format string, a ...any) error { return Fail{fmt.Sprintf(format, a...)} }

// Route of a page, with the rate as a float.
type Route struct {
	Page    string
	Model   string
	Channel string
	Rate    float64
	Min     float64
	Payee   string
	Lag     int
	Rates   []MethodRate // rates by method and date, newest first
}

// MethodRate says from which request time a rate applies to a request method ("*" = any other).
type MethodRate struct {
	Method string
	Rate   float64
	From   time.Time
}

// Config is the business data of the computation, from the database.
type Config struct {
	Models     []string // the order of the card
	Pages      []string // routed pages in card order
	Routes     map[string]Route
	Share      float64
	Hold       time.Duration
	KLow       float64
	KHigh      float64
	Lookback   time.Duration
	FullLedger time.Time
	Exc        hub.Exceptions
	TabAliases map[string]string
	Names      map[string]string
	CardNotes  []string // standing remarks of the card, from the optional setting card_notes
}

// LoadConfig reads routes, rates, settings and hub exceptions.
func LoadConfig(ctx context.Context, q ledger.Querier) (Config, error) {
	var c Config
	set, err := hub.LoadSettings(ctx, q)
	if err != nil {
		return c, err
	}
	if c.Models, err = set.List("models"); err != nil {
		return c, err
	}
	if c.Share, err = set.Float("share"); err != nil {
		return c, err
	}
	if c.Hold, err = set.Days("hold_days"); err != nil {
		return c, err
	}
	if c.KLow, err = set.Float("k_norm_low"); err != nil {
		return c, err
	}
	if c.KHigh, err = set.Float("k_norm_high"); err != nil {
		return c, err
	}
	if c.Lookback, err = set.Days("lookback_days"); err != nil {
		return c, err
	}
	if c.FullLedger, err = set.Time("full_ledger_from"); err != nil {
		return c, err
	}
	if c.TabAliases, err = set.Map("tab_aliases"); err != nil {
		return c, err
	}
	if c.Names, err = set.Map("model_names"); err != nil {
		return c, err
	}
	var notes string
	switch err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'card_notes'`).Scan(&notes); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal([]byte(notes), &c.CardNotes); err != nil {
			return c, fmt.Errorf("настройка card_notes: нужен JSON-список строк: %w", err)
		}
	}
	if c.Exc, err = hub.LoadExceptions(ctx, q); err != nil {
		return c, err
	}
	routes, err := hub.Routes(ctx, q)
	if err != nil {
		return c, err
	}
	c.Routes = map[string]Route{}
	for p, r := range routes {
		rate, err := strconv.ParseFloat(r.Rate, 64)
		if err != nil {
			return c, fmt.Errorf("route %s: rate %q", p, r.Rate)
		}
		c.Routes[p] = Route{Page: p, Model: r.Model, Channel: r.Channel, Rate: rate, Min: r.MinPayout.Float(), Payee: r.Payee, Lag: r.LagDays}
	}
	rows, err := q.QueryContext(ctx, `SELECT page_label, method, rate, valid_from FROM route_rates`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var p, m, r, from string
		if err := rows.Scan(&p, &m, &r, &from); err != nil {
			return c, err
		}
		rate, err := strconv.ParseFloat(r, 64)
		if err != nil {
			return c, fmt.Errorf("route rate %s/%s: %q", p, m, r)
		}
		t, err := ts.Parse(from)
		if err != nil {
			return c, fmt.Errorf("route rate %s/%s: %w", p, m, err)
		}
		rt, ok := c.Routes[p]
		if !ok {
			continue
		}
		rt.Rates = append(rt.Rates, MethodRate{Method: m, Rate: rate, From: t})
		c.Routes[p] = rt
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	for p, rt := range c.Routes {
		sort.Slice(rt.Rates, func(i, j int) bool { return rt.Rates[i].From.After(rt.Rates[j].From) })
		c.Routes[p] = rt
	}
	order := map[string]int{}
	for i, m := range c.Models {
		order[m] = i
	}
	for p, rt := range c.Routes {
		if _, ok := order[rt.Model]; !ok {
			return c, fmt.Errorf("route %s: model %s is not in settings.models", p, rt.Model)
		}
		c.Pages = append(c.Pages, p)
	}
	sort.Slice(c.Pages, func(i, j int) bool {
		a, b := c.Routes[c.Pages[i]], c.Routes[c.Pages[j]]
		if order[a.Model] != order[b.Model] {
			return order[a.Model] < order[b.Model]
		}
		return a.Page < b.Page
	})
	return c, nil
}

// PayeeOf is who receives a model's payouts: "model" (she transfers 70%) or "agency" (100% comes in).
func (c Config) PayeeOf(model string) string {
	for _, p := range c.Pages {
		if c.Routes[p].Model == model {
			return c.Routes[p].Payee
		}
	}
	return ""
}

// LagOf is how many days after the cut a transfer of the model may be dated.
func (c Config) LagOf(model string) int {
	for _, p := range c.Pages {
		if c.Routes[p].Model == model {
			return c.Routes[p].Lag
		}
	}
	return 0
}

// HasModel says whether the model is one of the routed models.
func (c Config) HasModel(model string) bool {
	for _, m := range c.Models {
		if m == model {
			return true
		}
	}
	return false
}

// RateOf is the share of net that reaches the transfer: by the request's method and date for a
// covered part, the route's base rate for an uncovered one.
func (c Config) RateOf(page string, q *Request) float64 {
	rt := c.Routes[page]
	if q == nil {
		return rt.Rate
	}
	for _, m := range []string{q.Method, "*"} {
		for _, r := range rt.Rates {
			if r.Method == m && !q.T.Before(r.From) {
				return r.Rate
			}
		}
	}
	return rt.Rate
}

// Tab is a parsed payout sheet tab (days in dollars).
type Tab struct {
	Model  string             `json:"model"`
	Name   string             `json:"name"`
	Days   map[string]float64 `json:"days"`
	Worked []string           `json:"worked"`
	Notes  []string           `json:"notes"`
}

// Source supplies the checked facts; the production one reads the hub mirrors.
type Source interface {
	Rows(page string, from, to time.Time) (hub.Checked, error)
	Payouts(page string, from, to time.Time) ([]hub.Payout, error)
	OFPayouts(page string, from, to time.Time) ([]hub.Payout, error)
	Pages() ([]hub.Page, error)
	SummaryNet(page string, from, to time.Time) (float64, error)
	// BybitIncomes lists the amounts of Bybit income movements of an MSK date; ok is false when the
	// source cannot tell.
	BybitIncomes(date string) (amounts []float64, ok bool)
}

// DBSource is the production Source.
type DBSource struct {
	Ctx context.Context
	Q   ledger.Querier
}

func (s DBSource) Rows(page string, from, to time.Time) (hub.Checked, error) {
	return hub.Rows(s.Ctx, s.Q, page, from, to)
}
func (s DBSource) Payouts(page string, from, to time.Time) ([]hub.Payout, error) {
	return hub.Payouts(s.Ctx, s.Q, page, from, to)
}
func (s DBSource) OFPayouts(page string, from, to time.Time) ([]hub.Payout, error) {
	return hub.OFPayouts(s.Ctx, s.Q, page, from, to)
}
func (s DBSource) Pages() ([]hub.Page, error) { return hub.GrantPages(s.Ctx, s.Q) }
func (s DBSource) SummaryNet(page string, from, to time.Time) (float64, error) {
	_, sum, err := hub.PlainSum(s.Ctx, s.Q, page, from, to)
	return float64(sum) / 1000, err
}

// BybitIncomes reads the income movements stored as events.
func (s DBSource) BybitIncomes(date string) ([]float64, bool) {
	rows, err := s.Q.QueryContext(s.Ctx, `SELECT payload FROM events WHERE source = 'bybit'
		AND kind IN ('deposit', 'internal_deposit') AND substr(occurred_at, 1, 10) = ?`, date)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, false
		}
		var p struct {
			Amount json.RawMessage `json:"amount"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, false
		}
		v, err := strconv.ParseFloat(strings.Trim(string(p.Amount), `"`), 64)
		if err != nil {
			return nil, false
		}
		out = append(out, v)
	}
	return out, rows.Err() == nil
}

// JournalRows returns the journal rows as of a cut ("" = current), in the script's (date, num) order.
func JournalRows(ctx context.Context, q ledger.Querier, cut string) ([]journal.Row, error) {
	docs, err := ledger.DocumentsAsOf(ctx, q, cut)
	if err != nil {
		return nil, err
	}
	rows := make([]journal.Row, 0, len(docs))
	for _, d := range docs {
		rows = append(rows, journal.Row{Num: d.Num, Date: d.Date, Type: d.Type, Category: d.Category, Counterparty: d.Counterparty,
			Model: d.Model, Amount: d.Amount, Currency: d.Currency, Method: d.Method, Status: d.Status, Note: d.Note})
	}
	return journal.SortedByDate(rows), nil
}
