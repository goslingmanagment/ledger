// Package budgets is the agency's budget by month for Hub's console («Деньги → Бюджеты»): what came in and from
// where, what went out and on what, the plans of the items, the pages' turnover plan and the month's result.
//
// The facts are the journal's paid rows by their date (cash, as finance/closes.md keeps the budget's «Факт»);
// the plans are budget_plans, the CEO's salary rate standing in for a month without its plan; the
// pages' turnover plan is month_plans; the month's result is the month card (v_month). Nothing is written but
// plans: a plan is the owner's number, never a movement of money.
package budgets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Kinds of a journal row in a budget.
const (
	KindIncome   = "income"    // money came to the agency
	KindExpense  = "expense"   // money went out of the business
	KindRefund   = "refund"    // «Возврат расхода»: makes the month's expenses smaller, not income
	KindOwnerIn  = "owner_in"  // «Owner вливание»: the owner's money put in, financing, not income
	KindOwnerOut = "owner_out" // «Owner draw»: profit the owner took, not an expense
)

// Categories with a meaning of their own (finance/rules.md § Категории).
const (
	CategoryOwnerIn  = "Owner вливание"
	CategoryOwnerOut = "Owner draw"
	CategoryRefund   = "Возврат расхода"
	CategoryTraffic  = "Трафик"
	CategoryFees     = "Комиссии"
	// CategoryOwnerPay is the CEO's salary: without a plan of its own, the rate in force on the month's first day
	// (settings.owner_salary_rate) is its plan.
	CategoryOwnerPay = "Owner зарплата"
	// CategorySFS is the old name of paid SFS before «Трафик» (the journal before August 2026).
	CategorySFS = "SFS"
	// ItemIncome is the plan of the agency's income, an item with no category of its own.
	ItemIncome = "Доход"
)

// SettingTrafficChannels names the traffic channel of a counterparty: {"контрагент": "канал"}. A row of «Трафик»
// without one is its counterparty's channel; a row of another category is traffic only when its counterparty is here
// (a contractor whose work is traffic), and never a fee: the network fee of a traffic payment stays in «Комиссии».
const SettingTrafficChannels = "traffic_channels"

// MaxMonths is the widest window of one read.
const MaxMonths = 36

// Row is one paid journal row of the month.
type Row struct {
	Num          int    `json:"num"`
	Date         string `json:"date"`
	Kind         string `json:"kind"`
	Category     string `json:"category"`
	Counterparty string `json:"counterparty"`
	// Model is the row's model, "" for none («—»).
	Model        string `json:"model"`
	AmountMicros int64  `json:"amountMicros"`
	// Channel is the traffic channel of a traffic row, "" for any other.
	Channel    string `json:"channel,omitempty"`
	Note       string `json:"note"`
	Incomplete bool   `json:"incomplete,omitempty"`
	// AdvanceFrom is the month an advance for this month was paid in (a part of the row tagged {"за": [{"месяц": …}]}
	// with a later month): the budget of the month it is for carries it, as the owner's rule puts an advance «в кап
	// того месяца». A part for an earlier month stays where it was paid, as the closes keep it.
	AdvanceFrom string `json:"advanceFrom,omitempty"`
}

// Plan is an item's plan for the month.
type Plan struct {
	Item       string `json:"item"`
	PlanMicros int64  `json:"planMicros"`
	Note       string `json:"note"`
	// Source: "plan" — budget_plans, "rate" — the CEO's salary rate of the month (settings.owner_salary_rate).
	Source string `json:"source"`
	SetAt  string `json:"setAt,omitempty"`
	SetBy  string `json:"setBy,omitempty"`
}

// PagePlan is a page's turnover plan, dollars a day (month_plans).
type PagePlan struct {
	Page         string `json:"page"`
	PerDayMicros int64  `json:"perDayMicros"`
}

// Result is the month card's figures (finance/rules.md § Цифры месяца): turnover, earned, expenses and the
// decision figure, before the CEO's salary; at close — the figure the month was closed with.
type Result struct {
	Status                string `json:"status"`
	TurnoverMicros        int64  `json:"turnoverMicros"`
	EarnedMicros          int64  `json:"earnedMicros"`
	ArrivedMicros         int64  `json:"arrivedMicros"`
	TransitMicros         int64  `json:"transitMicros"`
	ExpensesMicros        int64  `json:"expensesMicros"`
	DecisionMicros        *int64 `json:"decisionMicros"`
	DecisionAtCloseMicros *int64 `json:"decisionAtCloseMicros"`
	AsOf                  string `json:"asOf"`
}

// Month is one month of the budget.
type Month struct {
	Month string `json:"month"`
	Days  int    `json:"days"`
	// Closed: the month is closed in the ledger (periods).
	Closed bool `json:"closed"`
	// Complete: the month is after settings.full_ledger_from; before it the journal holds only part of the movements.
	Complete  bool       `json:"complete"`
	Rows      []Row      `json:"rows"`
	Plans     []Plan     `json:"plans"`
	PagePlans []PagePlan `json:"pagePlans"`
	Result    *Result    `json:"result"`
}

// Answer is GET /v1/budgets.
type Answer struct {
	From           string `json:"from"`
	To             string `json:"to"`
	FullLedgerFrom string `json:"fullLedgerFrom"`
	GeneratedAt    string `json:"generatedAt"`
	// ModelNames are the models' names to show (settings.model_names): {"код": "имя"}.
	ModelNames map[string]string `json:"modelNames"`
	Months     []Month           `json:"months"`
}

// Fail is a refusal of the caller's input (400).
type Fail struct{ msg string }

func (f Fail) Error() string { return f.msg }

func failf(format string, a ...any) error { return Fail{fmt.Sprintf(format, a...)} }

// IsFail reports whether err is the caller's mistake.
func IsFail(err error) bool {
	var f Fail
	return errors.As(err, &f)
}

func checkMonth(name, v string) (time.Time, error) {
	m0, _, err := ts.MonthStart(v)
	if err != nil || ts.Month(m0) != v {
		return time.Time{}, failf("%s: нужен месяц ГГГГ-ММ, не «%s»", name, v)
	}
	return m0, nil
}

// Window checks a window of months: "" from — the journal's first month, "" to — the current month (MSK).
func Window(ctx context.Context, q ledger.Querier, from, to string, now time.Time) (string, string, error) {
	if to == "" {
		to = ts.Month(now)
	}
	if from == "" {
		var first sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT min(date) FROM v_documents WHERE status = 'paid'`).Scan(&first); err != nil {
			return "", "", err
		}
		from = to
		if first.Valid && len(first.String) >= 7 && first.String[:7] < to {
			from = first.String[:7]
		}
	}
	f0, err := checkMonth("from", from)
	if err != nil {
		return "", "", err
	}
	t0, err := checkMonth("to", to)
	if err != nil {
		return "", "", err
	}
	if t0.Before(f0) {
		return "", "", failf("from %s позже to %s", from, to)
	}
	if n := monthsBetween(f0, t0) + 1; n > MaxMonths {
		return "", "", failf("не больше %d месяцев за раз, а не %d", MaxMonths, n)
	}
	return from, to, nil
}

func monthsBetween(a, b time.Time) int {
	return (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
}

// KindOf is the budget kind of a journal row, "" for one outside the budget (a transfer).
func KindOf(typ, category string) string {
	switch {
	case category == CategoryOwnerIn:
		return KindOwnerIn
	case category == CategoryOwnerOut:
		return KindOwnerOut
	case category == CategoryRefund:
		return KindRefund
	case typ == "income":
		return KindIncome
	case typ == "expense":
		return KindExpense
	}
	return ""
}

// ChannelOf is the traffic channel of an expense row, "" when it is not traffic.
func ChannelOf(kind, category, counterparty string, channels map[string]string) string {
	if (kind != KindExpense && kind != KindRefund) || category == CategoryFees {
		return ""
	}
	if c := strings.TrimSpace(channels[counterparty]); c != "" {
		return c
	}
	switch category {
	case CategorySFS:
		return "SFS"
	case CategoryTraffic:
		if strings.TrimSpace(counterparty) == "" || counterparty == "—" {
			return CategoryTraffic
		}
		return counterparty
	}
	return ""
}

func setting(ctx context.Context, q ledger.Querier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// TrafficChannels reads settings.traffic_channels; no setting is no channels.
func TrafficChannels(ctx context.Context, q ledger.Querier) (map[string]string, error) {
	return stringMap(ctx, q, SettingTrafficChannels)
}

// stringMap reads a setting that is a JSON object of strings; no setting is an empty map.
func stringMap(ctx context.Context, q ledger.Querier, key string) (map[string]string, error) {
	v, err := setting(ctx, q, key)
	if err != nil || strings.TrimSpace(v) == "" {
		return map[string]string{}, err
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("настройка %s: нужен JSON-объект строк: %v", key, err)
	}
	return out, nil
}

func microsOf(x float64) int64 { return int64(math.Round(x * money.Scale)) }

// Read is the budget of the months from..to (ГГГГ-ММ, both included).
func Read(ctx context.Context, q ledger.Querier, from, to string, now time.Time) (Answer, error) {
	from, to, err := Window(ctx, q, from, to, now)
	if err != nil {
		return Answer{}, err
	}
	ans := Answer{From: from, To: to, GeneratedAt: ts.Format(now), Months: []Month{}}
	full, err := setting(ctx, q, "full_ledger_from")
	if err != nil {
		return Answer{}, err
	}
	var fullFrom time.Time
	if strings.TrimSpace(full) != "" {
		if fullFrom, err = ts.Parse(strings.TrimSpace(full)); err != nil {
			return Answer{}, fmt.Errorf("настройка full_ledger_from: %v", err)
		}
		ans.FullLedgerFrom = ts.Date(fullFrom)
	}
	channels, err := TrafficChannels(ctx, q)
	if err != nil {
		return Answer{}, err
	}
	if ans.ModelNames, err = stringMap(ctx, q, "model_names"); err != nil {
		return Answer{}, err
	}
	f0, _ := checkMonth("from", from)
	t0, _ := checkMonth("to", to)
	index := map[string]int{}
	for m := f0; !m.After(t0); m = m.AddDate(0, 1, 0) {
		key := ts.Month(m)
		_, m1, _ := ts.MonthStart(key)
		index[key] = len(ans.Months)
		ans.Months = append(ans.Months, Month{Month: key, Days: int(math.Round(m1.Sub(m).Hours() / 24)), Complete: fullFrom.IsZero() || !m.Before(fullFrom),
			Rows: []Row{}, Plans: []Plan{}, PagePlans: []PagePlan{}})
	}
	_, end, _ := ts.MonthStart(to)

	// An advance paid before the window for a month in it is read too.
	rows, err := q.QueryContext(ctx, `SELECT num, date, type, category, counterparty, model, amount_micros, note, incomplete, tags
		FROM v_documents WHERE status = 'paid' AND date < ? AND (date >= ? OR json_extract(tags, '$.за') IS NOT NULL)
		ORDER BY date, num`, ts.Date(end), from+"-01")
	if err != nil {
		return Answer{}, err
	}
	for rows.Next() {
		var r Row
		var typ, tags string
		var inc int
		if err := rows.Scan(&r.Num, &r.Date, &typ, &r.Category, &r.Counterparty, &r.Model, &r.AmountMicros, &r.Note, &inc, &tags); err != nil {
			rows.Close()
			return Answer{}, err
		}
		if r.Kind = KindOf(typ, r.Category); r.Kind == "" {
			continue
		}
		if r.Model == "—" {
			r.Model = ""
		}
		r.Incomplete = inc == 1
		r.Channel = ChannelOf(r.Kind, r.Category, r.Counterparty, channels)
		parts, err := Advances(r.Date[:7], tags)
		if err != nil {
			rows.Close()
			return Answer{}, fmt.Errorf("#%d: %v", r.Num, err)
		}
		for _, p := range parts {
			r.AmountMicros -= p.Micros
			if i, ok := index[p.Month]; ok {
				moved := r
				moved.AmountMicros, moved.AdvanceFrom = p.Micros, r.Date[:7]
				ans.Months[i].Rows = append(ans.Months[i].Rows, moved)
			}
		}
		if i, ok := index[r.Date[:7]]; ok && r.AmountMicros > 0 {
			ans.Months[i].Rows = append(ans.Months[i].Rows, r)
		}
	}
	if err := rows.Close(); err != nil {
		return Answer{}, err
	}

	plans, err := q.QueryContext(ctx, `SELECT month, item, plan_micros, note, created_at, author FROM v_budget_plans
		WHERE month >= ? AND month <= ? ORDER BY month, item`, from, to)
	if err != nil {
		return Answer{}, err
	}
	for plans.Next() {
		var month string
		p := Plan{Source: "plan"}
		if err := plans.Scan(&month, &p.Item, &p.PlanMicros, &p.Note, &p.SetAt, &p.SetBy); err != nil {
			plans.Close()
			return Answer{}, err
		}
		ans.Months[index[month]].Plans = append(ans.Months[index[month]].Plans, p)
	}
	if err := plans.Close(); err != nil {
		return Answer{}, err
	}
	for i := range ans.Months {
		m := &ans.Months[i]
		// Before full accounting there is no budget to stand in for.
		if m.Complete && !hasPlan(m.Plans, CategoryOwnerPay) {
			rate, from, ok, err := ledger.OwnerRate(ctx, q, m.Month+"-01")
			if err != nil {
				return Answer{}, err
			}
			if ok && rate > 0 {
				note := "ставка CEO"
				if from != "" {
					note += " с " + from
				}
				m.Plans = append(m.Plans, Plan{Item: CategoryOwnerPay, PlanMicros: microsOf(rate), Note: note, Source: "rate"})
			}
		}
		sort.SliceStable(m.Plans, func(a, b int) bool { return m.Plans[a].Item < m.Plans[b].Item })
	}

	pp, err := q.QueryContext(ctx, `SELECT month, page_label, per_day FROM month_plans WHERE month >= ? AND month <= ? ORDER BY month, page_label`, from, to)
	if err != nil {
		return Answer{}, err
	}
	for pp.Next() {
		var month string
		var p PagePlan
		var perDay float64
		if err := pp.Scan(&month, &p.Page, &perDay); err != nil {
			pp.Close()
			return Answer{}, err
		}
		p.PerDayMicros = microsOf(perDay)
		ans.Months[index[month]].PagePlans = append(ans.Months[index[month]].PagePlans, p)
	}
	if err := pp.Close(); err != nil {
		return Answer{}, err
	}

	cards, err := q.QueryContext(ctx, `SELECT month, state, status, COALESCE(net, 0), COALESCE(earned, 0), COALESCE(arrived, 0),
		COALESCE(transit, 0), COALESCE(expenses, 0), decision, decision_at_close, as_of FROM v_month WHERE month >= ? AND month <= ?`, from, to)
	if err != nil {
		return Answer{}, err
	}
	for cards.Next() {
		var month, state, status, asOf string
		var net, earned, arrived, transit, expenses float64
		var decision, atClose sql.NullFloat64
		if err := cards.Scan(&month, &state, &status, &net, &earned, &arrived, &transit, &expenses, &decision, &atClose, &asOf); err != nil {
			cards.Close()
			return Answer{}, err
		}
		r := &Result{Status: status, TurnoverMicros: microsOf(net), EarnedMicros: microsOf(earned), ArrivedMicros: microsOf(arrived),
			TransitMicros: microsOf(transit), ExpensesMicros: microsOf(expenses), AsOf: asOf}
		if decision.Valid {
			v := microsOf(decision.Float64)
			r.DecisionMicros = &v
		}
		if atClose.Valid {
			v := microsOf(atClose.Float64)
			r.DecisionAtCloseMicros = &v
		}
		m := &ans.Months[index[month]]
		m.Result, m.Closed = r, state == "закрыт"
	}
	if err := cards.Close(); err != nil {
		return Answer{}, err
	}
	// A month closed without a card (none computed) is still closed.
	closed, err := q.QueryContext(ctx, `SELECT month FROM periods WHERE month >= ? AND month <= ?`, from, to)
	if err != nil {
		return Answer{}, err
	}
	for closed.Next() {
		var month string
		if err := closed.Scan(&month); err != nil {
			closed.Close()
			return Answer{}, err
		}
		ans.Months[index[month]].Closed = true
	}
	return ans, closed.Close()
}

// Advance is a part of a row paid for a later month.
type Advance struct {
	Month  string
	Micros int64
}

// Advances reads a row's tag {"за": [{"месяц": "ГГГГ-ММ", "сумма": N}]} and keeps the parts for months after paid:
// those belong to that month's budget. Parts for earlier months, and rows without the tag, give none.
func Advances(paid, tags string) ([]Advance, error) {
	if !strings.Contains(tags, `"за"`) {
		return nil, nil
	}
	var t struct {
		For []struct {
			Month string  `json:"месяц"`
			Sum   float64 `json:"сумма"`
		} `json:"за"`
	}
	if err := json.Unmarshal([]byte(tags), &t); err != nil {
		return nil, fmt.Errorf("пометка «за»: %v", err)
	}
	var out []Advance
	for _, p := range t.For {
		if p.Month > paid && p.Sum > 0 {
			out = append(out, Advance{Month: p.Month, Micros: microsOf(p.Sum)})
		}
	}
	return out, nil
}

func hasPlan(plans []Plan, item string) bool {
	for _, p := range plans {
		if p.Item == item {
			return true
		}
	}
	return false
}

// PlanInput is one plan to set: Amount "" or "-" removes the item's plan.
type PlanInput struct {
	Month  string `json:"month"`
	Item   string `json:"item"`
	Amount string `json:"amount"`
	Note   string `json:"note"`
}

// SetInput is POST /v1/budgets/plans: plans written in one transaction.
type SetInput struct {
	Plans  []PlanInput `json:"plans"`
	Source string      `json:"source"`
}

// SetAnswer is what was written.
type SetAnswer struct {
	Written int `json:"written"`
	// Unchanged counts plans already as asked: no row is added for them.
	Unchanged int `json:"unchanged"`
}

// MaxPlans is the most plans of one write.
const MaxPlans = 500

// SetPlans writes plans; a plan equal to the item's current one (amount and note) adds no row.
func SetPlans(ctx context.Context, q ledger.Querier, in SetInput, author string, now time.Time) (SetAnswer, error) {
	var ans SetAnswer
	if len(in.Plans) == 0 {
		return ans, failf("нет ни одного плана")
	}
	if len(in.Plans) > MaxPlans {
		return ans, failf("не больше %d планов за раз", MaxPlans)
	}
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = "api"
	}
	if len(source) > 64 {
		return ans, failf("source длиннее 64 символов")
	}
	at := ts.Format(now)
	for i, p := range in.Plans {
		if _, err := checkMonth(fmt.Sprintf("plans[%d].month", i), strings.TrimSpace(p.Month)); err != nil {
			return ans, err
		}
		item := strings.TrimSpace(p.Item)
		if item == "" || len([]rune(item)) > 64 {
			return ans, failf("plans[%d].item: нужна статья, до 64 символов", i)
		}
		note := strings.TrimSpace(p.Note)
		if len([]rune(note)) > 500 {
			return ans, failf("plans[%d].note: до 500 символов", i)
		}
		var value any
		if amount := strings.TrimSpace(p.Amount); amount != "" && amount != "-" {
			v, err := money.Parse(strings.ReplaceAll(amount, " ", ""))
			if err != nil || v < 0 {
				return ans, failf("plans[%d].amount: нужна сумма ≥ 0 в долларах, не «%s»", i, p.Amount)
			}
			value = int64(v)
		}
		var cur sql.NullInt64
		var curNote string
		err := q.QueryRowContext(ctx, `SELECT plan_micros, note FROM budget_plans WHERE month = ? AND item = ? ORDER BY id DESC LIMIT 1`,
			strings.TrimSpace(p.Month), item).Scan(&cur, &curNote)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if value == nil {
				ans.Unchanged++
				continue
			}
		case err != nil:
			return ans, err
		default:
			same := (value == nil && !cur.Valid) || (value != nil && cur.Valid && cur.Int64 == value.(int64) && curNote == note)
			if same {
				ans.Unchanged++
				continue
			}
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO budget_plans(month, item, plan_micros, note, source, author, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, strings.TrimSpace(p.Month), item, value, note, source, author, at); err != nil {
			return ans, err
		}
		ans.Written++
	}
	return ans, nil
}
