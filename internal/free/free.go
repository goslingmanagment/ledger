// Package free answers how much can be withdrawn now and after the nearest transfers:
//
//	free now   = liquid money − debts to people − commitments due before the next expected income − cushion
//	free after = free now + expected model transfers of the money in transit (× average k)
//	           − the other commitments to the end of the month
package free

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Item is one line of a breakdown.
type Item struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
	Due    string  `json:"due,omitempty"`
	Note   string  `json:"note,omitempty"`
}

// Answer is the breakdown of ledger free.
type Answer struct {
	At              string            `json:"at"`
	NextIncome      string            `json:"next_income"`
	Liquid          float64           `json:"liquid"`
	LiquidItems     []Item            `json:"liquid_items"`
	Debts           float64           `json:"debts"`
	DebtItems       []Item            `json:"debt_items"`
	CommitmentsSoon float64           `json:"commitments_soon"`
	SoonItems       []Item            `json:"soon_items"`
	Cushion         float64           `json:"cushion"`
	CushionNote     string            `json:"cushion_note"`
	FreeNow         float64           `json:"free_now"`
	Expected        float64           `json:"expected"`
	ExpectedNote    string            `json:"expected_note"`
	TransitStatus   string            `json:"transit_status"`
	TransitReason   string            `json:"transit_reason,omitempty"`
	AvgK            float64           `json:"avg_k"`
	CommitmentsLate float64           `json:"commitments_later"`
	LaterItems      []Item            `json:"later_items"`
	FreeAfter       float64           `json:"free_after"`
	Sources         map[string]string `json:"sources"`
	Notes           []string          `json:"notes,omitempty"`
}

func setting(ctx context.Context, q ledger.Querier, key, def string) string {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil || v == "" {
		return def
	}
	return v
}

// nextIncome: transfers come after the weekly withdrawal: the next withdraw weekday from now, plus the
// usual lag. If this week's withdrawal already happened it is next week's.
func nextIncome(now time.Time, weekday time.Weekday, lagDays int) time.Time {
	d := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, ts.MSK).In(ts.MSK)
	for d.Weekday() != weekday {
		d = d.AddDate(0, 0, 1)
	}
	return d.AddDate(0, 0, lagDays)
}

// periodMonths is how many months one period covers (a monthly equivalent for the default cushion).
func periodMonths(p string) float64 {
	switch {
	case p == "month":
		return 1
	case p == "week":
		return 12.0 / 52
	case strings.HasSuffix(p, "d"):
		var n float64
		fmt.Sscanf(p, "%fd", &n)
		if n > 0 {
			return n / 30.4375
		}
	}
	return 0 // once
}

type commitment struct {
	kind, name, period, nextDue, until, note string
	amount                                   money.Micros
}

// addMonths keeps the day of the month, or the last day of a shorter month.
func addMonths(t time.Time, n int) time.Time {
	y, m, day := t.Date()
	last := time.Date(y, m+time.Month(n)+1, 0, 0, 0, 0, 0, t.Location()).Day()
	if day > last {
		day = last
	}
	return time.Date(y, m+time.Month(n), day, 0, 0, 0, 0, t.Location())
}

// occurrences yields the due dates of a commitment from the calendar day of from through to, rolling a
// recurring next_due forward. A one-off that is overdue is still owed: it counts as due today. A commitment
// with only a term is due at the term.
func occurrences(c commitment, from, to time.Time) []time.Time {
	due := c.nextDue
	if due == "" {
		due = c.until
	}
	if due == "" {
		return nil
	}
	d, err := ts.Parse(due)
	if err != nil {
		return nil
	}
	var until time.Time
	if c.until != "" {
		if until, err = ts.Parse(c.until); err != nil {
			return nil
		}
	}
	from = from.In(ts.MSK)
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, ts.MSK)
	if c.period == "once" && d.Before(day) {
		d = day
	}
	base := d
	var out []time.Time
	for i := 0; i < 400; i++ {
		if !d.After(to) && !d.Before(day) && (until.IsZero() || !d.After(until) || c.period == "once") {
			out = append(out, d)
		}
		switch {
		case c.period == "month":
			d = addMonths(base, i+1)
		case c.period == "week":
			d = d.AddDate(0, 0, 7)
		case strings.HasSuffix(c.period, "d"):
			var n int
			fmt.Sscanf(c.period, "%dd", &n)
			if n <= 0 {
				return out
			}
			d = d.AddDate(0, 0, n)
		default:
			return out
		}
		if d.After(to) {
			break
		}
	}
	return out
}

func loadCommitments(ctx context.Context, q ledger.Querier) ([]commitment, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, name, period, COALESCE(next_due, ''), COALESCE(until, ''), note, amount FROM commitments ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commitment
	for rows.Next() {
		var c commitment
		var amt int64
		if err := rows.Scan(&c.kind, &c.name, &c.period, &c.nextDue, &c.until, &c.note, &amt); err != nil {
			return nil, err
		}
		c.amount = money.Micros(amt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ownerPaid sums the owner's salary rows of a month: by the row's date, or by a {за ГГГГ-ММ: N} part.
func ownerPaid(ctx context.Context, q ledger.Querier, month string) (float64, error) {
	rows, err := q.QueryContext(ctx, `SELECT num, date, type, category, counterparty, model, amount, currency, method, status, note FROM v_documents
		WHERE category = 'Owner зарплата' AND status = 'paid'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	sum := 0.0
	for rows.Next() {
		var r journal.Row
		if err := rows.Scan(&r.Num, &r.Date, &r.Type, &r.Category, &r.Counterparty, &r.Model, &r.Amount, &r.Currency, &r.Method, &r.Status, &r.Note); err != nil {
			return 0, err
		}
		v, err := money.Parse(r.Amount)
		if err != nil {
			return 0, err
		}
		t, err := journal.RowTags(r)
		if err != nil {
			return 0, err
		}
		amt, moved := v.Float(), 0.0
		for _, m := range t.ForMonth {
			moved += m.Amount
			if m.Month == month {
				sum += m.Amount
			}
		}
		if r.Date[:7] == month {
			sum += amt - moved
		}
	}
	return sum, rows.Err()
}

// Compute answers at now. Only reads.
func Compute(ctx context.Context, q ledger.Querier, now time.Time) (Answer, error) {
	now = now.In(ts.MSK)
	a := Answer{At: ts.Format(now), Sources: map[string]string{}, SoonItems: []Item{}, LaterItems: []Item{}, DebtItems: []Item{}, LiquidItems: []Item{}}
	frozen := map[string]bool{}
	var list []string
	if raw := setting(ctx, q, "frozen_accounts", "[]"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &list)
		for _, x := range list {
			frozen[x] = true
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT account, name, grp, liquid, balance_micros FROM v_balances WHERE balance_micros <> 0`)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var acct, name, grp string
		var liquid int
		var v int64
		if err := rows.Scan(&acct, &name, &grp, &liquid, &v); err != nil {
			rows.Close()
			return a, err
		}
		m := money.Micros(v).Float()
		switch {
		case liquid == 1 && !frozen[acct]:
			a.Liquid += m
			a.LiquidItems = append(a.LiquidItems, Item{Name: acct, Amount: money.Round2(m)})
		case liquid == 1:
			a.Notes = append(a.Notes, fmt.Sprintf("%s заморожен: %.2f не считается", acct, m))
		case grp == "payable":
			// An advance to one person pays nobody else's debt: the money has already left the cash.
			if m > 0 {
				if m >= 0.005 {
					a.Notes = append(a.Notes, fmt.Sprintf("%s: аванс %.2f, остальные долги не уменьшает", acct, m))
				}
				continue
			}
			a.Debts += -m
			a.DebtItems = append(a.DebtItems, Item{Name: acct, Amount: money.Round2(-m)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}
	// What already left or reached the accounts and has no journal row yet: without it the free money is off
	// until the owner answers the brief.
	pendingIn, pendingOut, err := bybit.PendingUSDParts(ctx, q)
	if err != nil {
		return a, err
	}
	if pending := pendingIn + pendingOut; math.Abs(pending) >= 0.005 {
		a.Liquid += pending
		a.LiquidItems = append(a.LiquidItems, Item{Name: "движения Bybit без строки журнала", Amount: money.Round2(pending)})
	}
	crows, err := q.QueryContext(ctx, `SELECT json_extract(content, '$.card'), CAST(json_extract(content, '$.amount') AS REAL) FROM proposals
		WHERE status = 'open' AND json_extract(content, '$.card') IS NOT NULL`)
	if err != nil {
		return a, err
	}
	spent := 0.0
	for crows.Next() {
		var card string
		var v float64
		if err := crows.Scan(&card, &v); err != nil {
			crows.Close()
			return a, err
		}
		if !frozen["card:"+card] {
			spent += v
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return a, err
	}
	if math.Abs(spent) >= 0.005 {
		a.Liquid -= spent
		a.LiquidItems = append(a.LiquidItems, Item{Name: "покупки по картам без строки журнала", Amount: money.Round2(-spent)})
	}
	sort.Slice(a.LiquidItems, func(i, j int) bool { return a.LiquidItems[i].Name < a.LiquidItems[j].Name })
	sort.Slice(a.DebtItems, func(i, j int) bool { return a.DebtItems[i].Amount > a.DebtItems[j].Amount })
	lag := 1
	fmt.Sscanf(setting(ctx, q, "transfer_lag_days", "1"), "%d", &lag)
	wd := time.Monday
	var wdn int
	if _, err := fmt.Sscanf(setting(ctx, q, "withdraw_weekday", "1"), "%d", &wdn); err == nil {
		wd = time.Weekday(wdn)
	}
	next := nextIncome(now, wd, lag)
	a.NextIncome = ts.Date(next)
	_, m1, _ := ts.MonthStart(ts.Month(now))
	monthEnd := m1.Add(-time.Second)
	cs, err := loadCommitments(ctx, q)
	if err != nil {
		return a, err
	}
	fixed := 0.0
	for _, c := range cs {
		if c.until != "" && c.until < ts.Date(now) {
			continue // ended
		}
		if c.kind == "subscription" || c.kind == "contractor" {
			fixed += c.amount.Float() * func() float64 {
				if p := periodMonths(c.period); p > 0 {
					return 1 / p
				}
				return 0
			}()
		}
		if c.kind == "owner_salary" {
			continue
		}
		for _, d := range occurrences(c, now, monthEnd.AddDate(0, 1, 0)) {
			it := Item{Name: c.name, Amount: c.amount.Float(), Due: ts.Date(d), Note: c.note}
			switch {
			case !d.After(next):
				a.CommitmentsSoon += it.Amount
				a.SoonItems = append(a.SoonItems, it)
			case !d.After(monthEnd):
				a.CommitmentsLate += it.Amount
				a.LaterItems = append(a.LaterItems, it)
			}
		}
	}
	// Owner salary rest for the month: the cap minus what was paid; due at the end of the month.
	var rate float64
	fmt.Sscanf(setting(ctx, q, "owner_salary_rate", "0"), "%f", &rate)
	if rate > 0 {
		paid, err := ownerPaid(ctx, q, ts.Month(now))
		if err != nil {
			return a, err
		}
		if rest := rate - paid; rest > 0.005 {
			it := Item{Name: "зарплата владельца, остаток месяца", Amount: money.Round2(rest), Due: ts.Date(monthEnd)}
			if !monthEnd.After(next) {
				a.CommitmentsSoon += rest
				a.SoonItems = append(a.SoonItems, it)
			} else {
				a.CommitmentsLate += rest
				a.LaterItems = append(a.LaterItems, it)
			}
		}
	}
	since := ts.Date(now.AddDate(0, 0, -28))
	payouts := 0.0
	prow, err := q.QueryContext(ctx, `SELECT COALESCE(SUM(amount_micros), 0) FROM v_documents WHERE status = 'paid' AND type = 'expense'
		AND category IN ('Чаттер зарплата', 'КМ зарплата') AND date >= ? AND date <= ?`, since, ts.Date(now))
	if err != nil {
		return a, err
	}
	if prow.Next() {
		var v int64
		prow.Scan(&v)
		payouts = money.Micros(v).Float()
	}
	prow.Close()
	if s := setting(ctx, q, "cushion", ""); s != "" {
		v, err := money.Parse(s)
		if err != nil {
			return a, fmt.Errorf("settings.cushion: %w", err)
		}
		a.Cushion, a.CushionNote = v.Float(), "задал владелец"
	} else {
		a.Cushion = fixed + payouts/4
		a.CushionNote = fmt.Sprintf("по умолчанию: постоянные траты месяца %.2f + неделя выплат людям %.2f (за последние 4 недели %.2f)", fixed, payouts/4, payouts)
	}
	a.FreeNow = a.Liquid - a.Debts - a.CommitmentsSoon - a.Cushion
	// Money in transit, from the last coverage, as far as the next withdrawal can reach it.
	var status, reason, asOf string
	err = q.QueryRowContext(ctx, `SELECT status, reason, as_of FROM coverage_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &reason, &asOf)
	switch {
	case err == sql.ErrNoRows:
		a.TransitStatus, a.TransitReason = "unknown", "покрытие ещё не считалось"
	case err != nil:
		return a, err
	case status != "ok":
		a.TransitStatus, a.TransitReason = "unknown", reason
	default:
		a.TransitStatus = "ok"
	}
	a.AvgK = 1
	rowsK, err := q.QueryContext(ctx, `SELECT k FROM coverage_transfers WHERE k IS NOT NULL AND bad = '[]' ORDER BY date DESC, num DESC LIMIT 8`)
	if err != nil {
		return a, err
	}
	var ks []float64
	for rowsK.Next() {
		var k float64
		rowsK.Scan(&k)
		ks = append(ks, k)
	}
	rowsK.Close()
	if len(ks) > 0 {
		s := 0.0
		for _, k := range ks {
			s += k
		}
		a.AvgK = s / float64(len(ks))
	}
	if a.TransitStatus == "ok" {
		withdraw := next.AddDate(0, 0, -lag)
		cut := time.Date(withdraw.Year(), withdraw.Month(), withdraw.Day(), 23, 59, 59, 0, ts.MSK)
		// Riding is money at the model. A part with a transfer already arrived: a transfer the coverage did
		// not accept stays riding, but its cash is on Bybit, so it is not expected again.
		var riding, page, hold sql.NullFloat64
		if err := q.QueryRowContext(ctx, `SELECT SUM(est) FROM coverage_parts WHERE state = 'riding' AND transfer_num IS NULL`).Scan(&riding); err != nil {
			return a, err
		}
		// A page withdraws only when what it has by then reaches its minimum.
		at := ts.FormatMilli(cut)
		prows, err := q.QueryContext(ctx, `SELECT COALESCE(c.min_payout, 0),
			COALESCE(SUM(CASE WHEN p.state = 'page' THEN p.est END), 0),
			COALESCE(SUM(CASE WHEN p.state = 'hold' AND p.avail_at <= ? THEN p.est END), 0),
			COALESCE(SUM(CASE WHEN p.state IN ('page', 'stuck') OR (p.state = 'hold' AND p.avail_at <= ?) THEN p.part END), 0)
			FROM coverage_parts p LEFT JOIN coverage_pages c ON c.page_label = p.page_label GROUP BY p.page_label`, at, at)
		if err != nil {
			return a, err
		}
		for prows.Next() {
			var minimum, onPage, fromHold, net float64
			if err := prows.Scan(&minimum, &onPage, &fromHold, &net); err != nil {
				prows.Close()
				return a, err
			}
			if net >= minimum {
				page.Float64 += onPage
				hold.Float64 += fromHold
			}
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return a, err
		}
		// A deposit already counted in the cash above is most likely one of these transfers.
		a.Expected = math.Max(0, (riding.Float64+page.Float64+hold.Float64)*a.AvgK-pendingIn)
		a.ExpectedNote = fmt.Sprintf("у моделей %.2f + на страницах %.2f + выйдет из холда к %s %.2f, × средний k %.4f", riding.Float64, page.Float64, ts.Date(withdraw), hold.Float64, a.AvgK)
	} else {
		a.ExpectedNote = "деньги в пути неизвестны: " + a.TransitReason
	}
	a.FreeAfter = a.FreeNow + a.Expected - a.CommitmentsLate
	srows, err := q.QueryContext(ctx, `SELECT source, MAX(received_at) FROM events GROUP BY source`)
	if err != nil {
		return a, err
	}
	for srows.Next() {
		var src, at string
		srows.Scan(&src, &at)
		a.Sources[src] = at
	}
	srows.Close()
	if asOf != "" {
		a.Sources["покрытие (хаб)"] = asOf
	}
	var obs string
	if err := q.QueryRowContext(ctx, `SELECT MAX(observed_at) FROM observations WHERE kind = 'bybit_balance'`).Scan(&obs); err == nil && obs != "" {
		a.Sources["баланс Bybit"] = obs
	}
	round := func(x *float64) { *x = money.Round2(*x) }
	for _, x := range []*float64{&a.Liquid, &a.Debts, &a.CommitmentsSoon, &a.Cushion, &a.FreeNow, &a.Expected, &a.CommitmentsLate, &a.FreeAfter} {
		round(x)
	}
	return a, nil
}

// Render prints the answer for people.
func (a Answer) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Свободно сейчас: %.2f\nПосле ближайших переводов (приход ~%s): %.2f\n\n", a.FreeNow, a.NextIncome, a.FreeAfter)
	fmt.Fprintf(&b, "Ликвидные деньги %.2f:", a.Liquid)
	for _, i := range a.LiquidItems {
		fmt.Fprintf(&b, " %s %.2f;", i.Name, i.Amount)
	}
	fmt.Fprintf(&b, "\n− долги людям %.2f:", a.Debts)
	for _, i := range a.DebtItems {
		fmt.Fprintf(&b, " %s %.2f;", strings.TrimPrefix(i.Name, "payable:"), i.Amount)
	}
	fmt.Fprintf(&b, "\n− обязательства до прихода %.2f:", a.CommitmentsSoon)
	for _, i := range a.SoonItems {
		fmt.Fprintf(&b, " %s %.2f (%s);", i.Name, i.Amount, i.Due)
	}
	fmt.Fprintf(&b, "\n− подушка %.2f (%s)\n", a.Cushion, a.CushionNote)
	fmt.Fprintf(&b, "\n+ ожидаемые переводы %.2f (%s)\n− остальные обязательства до конца месяца %.2f:", a.Expected, a.ExpectedNote, a.CommitmentsLate)
	for _, i := range a.LaterItems {
		fmt.Fprintf(&b, " %s %.2f (%s);", i.Name, i.Amount, i.Due)
	}
	b.WriteString("\n\nСвежесть источников:")
	keys := make([]string, 0, len(a.Sources))
	for k := range a.Sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %s: %s", k, a.Sources[k])
	}
	for _, n := range a.Notes {
		fmt.Fprintf(&b, "\n! %s", n)
	}
	return b.String()
}

// Record stores the answer for v_free.
func Record(ctx context.Context, q ledger.Querier, a Answer, now time.Time) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO free_runs(computed_at, data) VALUES (?, ?)`, ts.Format(now), string(raw))
	return err
}

// CommitmentSeed is one row of the seed file of `ledger import commitments`.
type CommitmentSeed struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Amount   string `json:"amount"`
	Period   string `json:"period"`
	NextDue  string `json:"next_due"`
	Until    string `json:"until"`
	Note     string `json:"note"`
}

// ImportCommitments replaces the rows from one source.
func ImportCommitments(ctx context.Context, tx *sql.Tx, items []CommitmentSeed, source string, now time.Time) (int, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM commitments WHERE source = ?`, source); err != nil {
		return 0, err
	}
	for _, c := range items {
		v, err := money.Parse(c.Amount)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", c.Name, err)
		}
		if c.NextDue == "" && c.Until == "" {
			return 0, fmt.Errorf("%s: нужен срок: next_due или until", c.Name)
		}
		switch c.Period {
		case "month", "week", "once":
		default:
			var n int
			if _, err := fmt.Sscanf(c.Period, "%dd", &n); err != nil || n <= 0 {
				return 0, fmt.Errorf("%s: период «%s»: нужен month, week, once или Nd", c.Name, c.Period)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO commitments(kind, name, category, amount, period, next_due, until, note, created_at, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.Kind, c.Name, c.Category, int64(v), c.Period, c.NextDue, c.Until, c.Note, ts.Format(now), source); err != nil {
			return 0, fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return len(items), nil
}

func init() {
	server.Every("free", time.Hour, func(ctx context.Context, s *server.Server) error {
		now := time.Now()
		a, err := Compute(ctx, s.Store.RO, now)
		if err != nil {
			return err
		}
		return Record(ctx, s.Store.RW, a, now)
	})
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/free", auth.Require(s, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a, err := Compute(r.Context(), s.Store.RO, time.Now())
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "free": a, "text": a.Render()})
		})))
	})
}
