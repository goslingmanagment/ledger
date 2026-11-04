package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Business is the seed file of `ledger import business`: payout routes of pages, their rates by
// method and date, and the accounting settings. Real values live only in the database.
type Business struct {
	Source string `json:"source"`
	Routes []struct {
		PageLabel string `json:"page_label"`
		Model     string `json:"model"`
		Channel   string `json:"channel"`
		Rate      string `json:"rate"`
		MinPayout string `json:"min_payout"`
		Payee     string `json:"payee"`
		LagDays   *int   `json:"lag_days"`
		Note      string `json:"note"`
	} `json:"routes"`
	RouteRates []struct {
		PageLabel string `json:"page_label"`
		Method    string `json:"method"`
		Rate      string `json:"rate"`
		ValidFrom string `json:"valid_from"`
		Note      string `json:"note"`
	} `json:"route_rates"`
	Settings map[string]string `json:"settings"`
}

// BusinessReport counts what an import wrote.
type BusinessReport struct {
	Routes     int `json:"routes"`
	RouteRates int `json:"routeRates"`
	Settings   int `json:"settings"`
}

var rateRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func checkRate(what, r string) error {
	f, err := strconv.ParseFloat(r, 64)
	if !rateRe.MatchString(r) || err != nil || f <= 0 || f > 1 {
		return fmt.Errorf("%s: ставка «%s» — нужна доля от 0 до 1, например 0.95", what, r)
	}
	return nil
}

// ImportBusiness validates the whole file and writes it in the caller's transaction: routes are
// upserted by page, the rates of every page in the file are replaced, settings are upserted by key.
func ImportBusiness(ctx context.Context, q ledger.Querier, raw []byte, now time.Time) (BusinessReport, error) {
	var b Business
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return BusinessReport{}, fmt.Errorf("JSON: %w", err)
	}
	pages := map[string]bool{}
	for i, r := range b.Routes {
		what := fmt.Sprintf("routes[%d] %s", i, r.PageLabel)
		switch {
		case !pageRe.MatchString(r.PageLabel):
			return BusinessReport{}, fmt.Errorf("%s: страница — нужно имя-1", what)
		case pages[r.PageLabel]:
			return BusinessReport{}, fmt.Errorf("%s: страница дважды", what)
		case r.Model == "" || r.Channel == "":
			return BusinessReport{}, fmt.Errorf("%s: нужны model и channel", what)
		case r.Payee != "model" && r.Payee != "agency":
			return BusinessReport{}, fmt.Errorf("%s: payee «%s» — model или agency", what, r.Payee)
		case r.LagDays == nil || *r.LagDays < 0:
			return BusinessReport{}, fmt.Errorf("%s: нужен lag_days ≥ 0", what)
		}
		if err := checkRate(what, r.Rate); err != nil {
			return BusinessReport{}, err
		}
		if m, err := money.Parse(r.MinPayout); err != nil || m < 0 {
			return BusinessReport{}, fmt.Errorf("%s: min_payout «%s» — нужна сумма ≥ 0", what, r.MinPayout)
		}
		pages[r.PageLabel] = true
	}
	type rateKey struct{ page, method, from string }
	seenRate := map[rateKey]bool{}
	ratePages := map[string]bool{}
	froms := make([]string, len(b.RouteRates))
	for i, r := range b.RouteRates {
		what := fmt.Sprintf("route_rates[%d] %s %s", i, r.PageLabel, r.Method)
		if !pages[r.PageLabel] {
			var one int
			if err := q.QueryRowContext(ctx, `SELECT 1 FROM routes WHERE page_label = ?`, r.PageLabel).Scan(&one); err != nil {
				return BusinessReport{}, fmt.Errorf("%s: страницы нет в routes", what)
			}
		}
		if r.Method == "" {
			return BusinessReport{}, fmt.Errorf("%s: нужен method (* — любой)", what)
		}
		if err := checkRate(what, r.Rate); err != nil {
			return BusinessReport{}, err
		}
		t, err := ts.Parse(r.ValidFrom)
		if err != nil {
			return BusinessReport{}, fmt.Errorf("%s: valid_from: %w", what, err)
		}
		froms[i] = ts.Format(t)
		k := rateKey{r.PageLabel, r.Method, froms[i]}
		if seenRate[k] {
			return BusinessReport{}, fmt.Errorf("%s: ставка с %s дважды", what, froms[i])
		}
		seenRate[k], ratePages[r.PageLabel] = true, true
	}
	keys := make([]string, 0, len(b.Settings))
	for k, v := range b.Settings {
		if err := checkSetting(k, v); err != nil {
			return BusinessReport{}, err
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	note := b.Source
	for _, r := range b.Routes {
		m, _ := money.Parse(r.MinPayout)
		rn := r.Note
		if rn == "" {
			rn = note
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO routes(page_label, model, channel, rate, min_payout, payee, lag_days, note)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(page_label) DO UPDATE SET model = excluded.model,
			channel = excluded.channel, rate = excluded.rate, min_payout = excluded.min_payout, payee = excluded.payee,
			lag_days = excluded.lag_days, note = excluded.note`,
			r.PageLabel, r.Model, r.Channel, r.Rate, int64(m), r.Payee, *r.LagDays, rn); err != nil {
			return BusinessReport{}, fmt.Errorf("routes %s: %w", r.PageLabel, err)
		}
	}
	for p := range ratePages {
		if _, err := q.ExecContext(ctx, `DELETE FROM route_rates WHERE page_label = ?`, p); err != nil {
			return BusinessReport{}, err
		}
	}
	for i, r := range b.RouteRates {
		if _, err := q.ExecContext(ctx, `INSERT INTO route_rates(page_label, method, rate, valid_from, note) VALUES (?, ?, ?, ?, ?)`,
			r.PageLabel, r.Method, r.Rate, froms[i], r.Note); err != nil {
			return BusinessReport{}, fmt.Errorf("route_rates %s %s: %w", r.PageLabel, r.Method, err)
		}
	}
	at := ts.Format(now)
	for _, k := range keys {
		if _, err := q.ExecContext(ctx, `INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, note = excluded.note, updated_at = excluded.updated_at`,
			k, b.Settings[k], note, at); err != nil {
			return BusinessReport{}, fmt.Errorf("settings %s: %w", k, err)
		}
	}
	return BusinessReport{Routes: len(b.Routes), RouteRates: len(b.RouteRates), Settings: len(keys)}, nil
}

// Route is a page's payout route.
type Route struct {
	Page      string
	Model     string
	Channel   string
	Rate      string
	MinPayout money.Micros
	Payee     string
	LagDays   int
}

// Routes reads the routes table by page.
func Routes(ctx context.Context, q ledger.Querier) (map[string]Route, error) {
	rows, err := q.QueryContext(ctx, `SELECT page_label, model, channel, rate, min_payout, payee, lag_days FROM routes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Route{}
	for rows.Next() {
		var r Route
		var m int64
		if err := rows.Scan(&r.Page, &r.Model, &r.Channel, &r.Rate, &m, &r.Payee, &r.LagDays); err != nil {
			return nil, err
		}
		r.MinPayout = money.Micros(m)
		out[r.Page] = r
	}
	return out, rows.Err()
}
