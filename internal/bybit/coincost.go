package bybit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A coin account (bybit:btc) holds the coin at what it cost. A purchase for USDT puts what was paid on it,
// a deposit what its journal row put there; a payment in the coin is a journal row at the market price of
// its day, so the account drifts from the cost of the coin that is left. The cost is kept at the average
// price, as a sale already takes its share of the book (convert.go): the Funding log of the coin gives the
// quantity before and after every row, so each sale and payment takes cost × quantity / quantity before.
//
// From the setting coin_cost_from on, every payment gets an entry that brings the account to that cost,
// the rest goes to income:other as the difference of the price. A payment whose withdrawal has no journal
// row yet stops the run for its coin: its value would read as a difference of the price.

const (
	settingCoinCostFrom = "coin_cost_from"
	coinCostKey         = "bybit:coincost:"
	coinEps             = 1e-9
)

// CoinOutflow is a payment in the coin: a withdrawal by the Funding log.
type CoinOutflow struct {
	Key     string       `json:"key"`
	At      string       `json:"at"`
	Qty     float64      `json:"qty"`
	Cost    money.Micros `json:"-"` // of what left, at the average price
	Left    money.Micros `json:"-"` // the cost of what stays after it
	Journal bool         `json:"journal"`
	CostUSD string       `json:"cost"`
	t       time.Time
}

// CoinCost is a coin account at the price the coin was bought for.
type CoinCost struct {
	Account  string        `json:"account"`
	Coin     string        `json:"coin"`
	Qty      float64       `json:"qty"` // on the Funding wallet after its last row
	Cost     money.Micros  `json:"-"`
	Book     money.Micros  `json:"-"`
	Unknown  string        `json:"unknown,omitempty"` // why the cost is not known
	Rows     int           `json:"funding_rows"`
	Outflows []CoinOutflow `json:"outflows,omitempty"`
	CostUSD  string        `json:"cost"`
	BookUSD  string        `json:"book"`
}

type coinRow struct {
	id, at, typ   string
	t             time.Time
	signed, after float64
}

func parseQty(raw json.RawMessage) (float64, error) {
	return strconv.ParseFloat(trimQuotes(string(raw)), 64)
}

func coinRows(ctx context.Context, q ledger.Querier, coin string) ([]coinRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT source_id, occurred_at, payload FROM events WHERE source = 'bybit' AND kind = 'funding'
		AND json_extract(payload, '$.coin') = ? ORDER BY occurred_at, source_id`, coin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coinRow
	for rows.Next() {
		var r coinRow
		var payload string
		if err := rows.Scan(&r.id, &r.at, &payload); err != nil {
			return nil, err
		}
		var p struct {
			Type   string          `json:"type"`
			Signed json.RawMessage `json:"signed"`
			After  json.RawMessage `json:"after"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, fmt.Errorf("%s: %w", r.id, err)
		}
		if r.signed, err = parseQty(p.Signed); err != nil {
			return nil, fmt.Errorf("%s: signed %s", r.id, p.Signed)
		}
		if r.after, err = parseQty(p.After); err != nil {
			r.after = math.NaN() // an imported row without the balance after it
		}
		if r.t, err = ts.Parse(r.at); err != nil {
			return nil, fmt.Errorf("%s: %w", r.id, err)
		}
		r.typ = p.Type
		out = append(out, r)
	}
	return out, rows.Err()
}

// movementOf finds the deposit or withdrawal of a Funding row: the same coin within 2 s, the same quantity
// (a withdrawal's row is its amount and the network fee).
func movementOf(ctx context.Context, q ledger.Querier, kind, coin string, r coinRow) (int64, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, payload FROM events WHERE source = 'bybit' AND kind = ? AND json_extract(payload, '$.coin') = ?
		AND occurred_at BETWEEN ? AND ?`, kind, coin, ts.Format(r.t.Add(-3*time.Second)), ts.Format(r.t.Add(3*time.Second)))
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return 0, false, err
		}
		var m Movement
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return 0, false, fmt.Errorf("событие %d: %w", id, err)
		}
		if d := m.TimeMs - r.t.UnixMilli(); d > 2000 || d < -2000 {
			continue
		}
		amount, err := strconv.ParseFloat(m.Amount, 64)
		if err != nil {
			continue
		}
		fee, _ := strconv.ParseFloat(m.Fee, 64)
		if want := math.Abs(r.signed); math.Abs(amount-want) < coinEps || math.Abs(amount+fee-want) < coinEps {
			return id, true, nil
		}
	}
	return 0, false, rows.Err()
}

// journaled says whether an event has its journal rows, and what they put on the account.
func journaled(ctx context.Context, q ledger.Querier, event int64, account string) (bool, money.Micros, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM event_links WHERE event_id = ? AND document_num IS NOT NULL`, event).Scan(&n); err != nil {
		return false, 0, err
	}
	var v int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(l.amount), 0) FROM lines l JOIN entries e ON e.id = l.entry_id JOIN documents d ON d.id = e.document_id
		WHERE l.account = ? AND d.num IN (SELECT document_num FROM event_links WHERE event_id = ? AND document_num IS NOT NULL)`, account, event).Scan(&v)
	return n > 0, money.Micros(v), err
}

func ownCorrection(l ledger.TimedLine) bool {
	return strings.HasPrefix(l.SourceKey, coinCostKey) || strings.HasPrefix(l.Reverses, coinCostKey)
}

// bookBy is the account at moment t without the cost corrections; strict leaves out what moved at t itself.
func bookBy(lines []ledger.TimedLine, t time.Time, strict bool) money.Micros {
	var sum money.Micros
	for _, l := range lines {
		if ownCorrection(l) {
			continue
		}
		in := l.By(t)
		if strict {
			in = (!l.At.IsZero() && l.At.Before(t)) || (l.At.IsZero() && l.Day < ts.Date(t))
		}
		if in {
			sum += l.Amount
		}
	}
	return sum
}

// coinAccounts are the coin accounts of Bybit: bybit:<coin>.
func coinAccounts(ctx context.Context, q ledger.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT code FROM accounts WHERE code LIKE 'bybit:%' ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CoinCosts walks the Funding log of every coin account at the average price.
func CoinCosts(ctx context.Context, q ledger.Querier) ([]CoinCost, error) {
	accounts, err := coinAccounts(ctx, q)
	if err != nil {
		return nil, err
	}
	pairs, err := convertPairs(ctx, q)
	if err != nil {
		return nil, err
	}
	byLeg := map[string]convertPair{}
	for _, p := range pairs {
		for _, id := range p.ids {
			byLeg[id] = p
		}
	}
	var out []CoinCost
	for _, account := range accounts {
		c := CoinCost{Account: account, Coin: strings.ToUpper(strings.TrimPrefix(account, "bybit:"))}
		lines, err := ledger.AccountLines(ctx, q, account)
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			c.Book += l.Amount
		}
		rows, err := coinRows(ctx, q, c.Coin)
		if err != nil {
			return nil, err
		}
		if err := c.walk(ctx, q, rows, lines, byLeg); err != nil {
			return nil, err
		}
		c.Rows, c.CostUSD, c.BookUSD = len(rows), c.Cost.String(), c.Book.String()
		for i := range c.Outflows {
			c.Outflows[i].CostUSD = c.Outflows[i].Cost.String()
		}
		out = append(out, c)
	}
	return out, nil
}

func (c *CoinCost) walk(ctx context.Context, q ledger.Querier, rows []coinRow, lines []ledger.TimedLine, byLeg map[string]convertPair) error {
	if len(rows) == 0 {
		c.Cost = c.Book
		return nil
	}
	// What the coin held before the log starts stands at the books of that moment.
	cost := bookBy(lines, rows[0].t, true)
	for _, r := range rows {
		if math.IsNaN(r.after) {
			c.Unknown = fmt.Sprintf("строка Funding %s без остатка после неё", r.id)
			return nil
		}
		before := r.after - r.signed
		share := func() money.Micros { // the cost of what leaves at the average price
			out := math.Abs(r.signed)
			if before <= out+coinEps || r.after <= coinEps {
				return cost
			}
			return money.FromFloat(cost.Float() * out / before)
		}
		switch {
		case r.typ == "Convert":
			p, ok := byLeg[r.id]
			if !ok {
				c.Unknown = fmt.Sprintf("конвертация %s без второй ноги", r.id)
				return nil
			}
			if p.buy {
				cost += money.FromFloat(-p.usdt.delta)
			} else {
				cost -= share()
			}
		case r.typ == "Deposit" && r.signed > 0:
			id, ok, err := movementOf(ctx, q, "deposit", c.Coin, r)
			if err != nil {
				return err
			}
			if !ok {
				c.Unknown = fmt.Sprintf("депозит %s %s без события", r.at, c.Coin)
				return nil
			}
			has, value, err := journaled(ctx, q, id, c.Account)
			if err != nil {
				return err
			}
			if !has {
				c.Unknown = fmt.Sprintf("депозит %s %s без строки журнала", r.at, c.Coin)
				return nil
			}
			cost += value
		case r.typ == "Withdraw" && r.signed < 0:
			o := CoinOutflow{Key: r.id, At: r.at, Qty: -r.signed, Cost: share(), t: r.t}
			id, ok, err := movementOf(ctx, q, "withdrawal", c.Coin, r)
			if err != nil {
				return err
			}
			if ok {
				if o.Journal, _, err = journaled(ctx, q, id, c.Account); err != nil {
					return err
				}
			}
			cost -= o.Cost
			o.Left = cost
			c.Outflows = append(c.Outflows, o)
		default:
			c.Unknown = fmt.Sprintf("строка Funding %s типа %q: цены покупки не знаю", r.id, r.typ)
			return nil
		}
		if r.after <= coinEps {
			cost = 0
		}
		c.Qty = r.after
	}
	c.Cost = cost
	return nil
}

// CoinCorrection is the entry that brings a coin account to its cost after a payment.
type CoinCorrection struct {
	Key     string `json:"key"`
	At      string `json:"at"`
	Account string `json:"account"`
	Amount  string `json:"amount"` // on the coin account: − the price fell since the purchase, + it rose
	Stands  bool   `json:"stands"` // posted as it is
	active  int64
	lines   []ledger.Line
	revs    int
}

// CoinCostPlan is what PostCoinCost would post now.
type CoinCostPlan struct {
	From        string           `json:"from"`
	Coins       []CoinCost       `json:"coins"`
	Corrections []CoinCorrection `json:"corrections"`
	Stopped     []string         `json:"stopped,omitempty"`
}

// CoinCostFrom is the setting coin_cost_from: "" while the corrections are off.
func CoinCostFrom(ctx context.Context, q ledger.Querier) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingCoinCostFrom).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(v), err
}

// PlanCoinCost lists the corrections of payments from `from` on; it writes nothing.
func PlanCoinCost(ctx context.Context, q ledger.Querier, from string) (CoinCostPlan, error) {
	plan := CoinCostPlan{From: from}
	start, err := ts.Parse(from)
	if err != nil {
		return plan, fmt.Errorf("%s: %w", settingCoinCostFrom, err)
	}
	if plan.Coins, err = CoinCosts(ctx, q); err != nil {
		return plan, err
	}
	for _, c := range plan.Coins {
		if c.Unknown != "" {
			plan.Stopped = append(plan.Stopped, c.Account+": "+c.Unknown)
			continue
		}
		lines, err := ledger.AccountLines(ctx, q, c.Account)
		if err != nil {
			return plan, err
		}
		var wanted money.Micros
		for _, o := range c.Outflows {
			if o.t.Before(start) {
				continue
			}
			if !o.Journal {
				plan.Stopped = append(plan.Stopped, fmt.Sprintf("%s: платёж %s без строки журнала, дальше не правлю", c.Account, o.At))
				break
			}
			want := o.Left - bookBy(lines, o.t, false) - wanted
			wanted += want
			k := CoinCorrection{Key: coinCostKey + o.Key, At: o.At, Account: c.Account, Amount: want.String()}
			if want != 0 {
				memo := "разница цены " + c.Coin + " при оплате: счёт по средней цене покупки"
				k.lines = []ledger.Line{{Account: c.Account, Amount: want, Memo: memo}, {Account: "income:other", Amount: -want, Memo: memo}}
			}
			if k.active, k.revs, err = activeConvert(ctx, q, k.Key); err != nil {
				return plan, err
			}
			if k.active != 0 {
				if k.Stands, err = convertSame(ctx, q, k.active, k.lines); err != nil {
					return plan, err
				}
			} else {
				k.Stands = len(k.lines) == 0
			}
			plan.Corrections = append(plan.Corrections, k)
		}
	}
	return plan, nil
}

// PostCoinCost brings the coin accounts to their cost after every payment since coin_cost_from. Off while
// the setting is empty: the corrections change the books of payments already written.
func PostCoinCost(ctx context.Context, q ledger.Querier, now time.Time) (int, error) {
	from, err := CoinCostFrom(ctx, q)
	if err != nil || from == "" {
		return 0, err
	}
	plan, err := PlanCoinCost(ctx, q, from)
	if err != nil {
		return 0, err
	}
	return applyCoinCost(ctx, q, plan, now)
}

func applyCoinCost(ctx context.Context, q ledger.Querier, plan CoinCostPlan, now time.Time) (int, error) {
	posted := 0
	for _, k := range plan.Corrections {
		if k.Stands {
			continue
		}
		if k.active != 0 {
			if _, err := ledger.Reverse(ctx, q, k.active, ledger.Reversal{Author: "bybit", Memo: "сторно: изменилась разница цены " + k.Account}, now); err != nil {
				return posted, fmt.Errorf("%s: %w", k.Key, err)
			}
		}
		if len(k.lines) < 2 {
			continue
		}
		key := k.Key
		if k.revs > 0 {
			key = fmt.Sprintf("%s:r%d", k.Key, k.revs+1)
		}
		if _, err := ledger.Post(ctx, q, ledger.Entry{EffectiveAt: k.At, Kind: "bybit:coin-cost", Memo: k.lines[0].Memo,
			Author: "bybit", SourceKey: key, Lines: k.lines}, now); err != nil {
			return posted, fmt.Errorf("%s: %w", key, err)
		}
		posted++
	}
	return posted, nil
}

// StartCoinCost turns the corrections on from `from` and posts them now.
func StartCoinCost(ctx context.Context, q ledger.Querier, from string, now time.Time) (CoinCostPlan, int, error) {
	if _, err := ts.Parse(from); err != nil {
		return CoinCostPlan{}, 0, fmt.Errorf("--from: %w", err)
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, settingCoinCostFrom, from,
		"с этого времени платежи монетой приводят счёт монеты к средней цене покупки", ts.Format(now)); err != nil {
		return CoinCostPlan{}, 0, err
	}
	plan, err := PlanCoinCost(ctx, q, from)
	if err != nil {
		return plan, 0, err
	}
	n, err := applyCoinCost(ctx, q, plan, now)
	return plan, n, err
}
