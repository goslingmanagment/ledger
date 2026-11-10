// Package checks runs the hourly verification of the books: every entry balances, cash against the
// exchange and the card bot, movements have journal rows, transfers cover their requests, debts match
// the sheets, closed months have not drifted, copies are fresh. A failure goes to the morning health
// line; an urgent one to the owner at once through the agent that polls `ledger health --urgent`.
package checks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/free"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/month"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Result of one check.
type Result struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"` // of a failure: info, warn, urgent
	Summary  string `json:"summary"`
	Details  any    `json:"details,omitempty"`
}

func pass(name, summary string) Result {
	return Result{Name: name, OK: true, Severity: "info", Summary: summary}
}
func fail(name, severity, summary string, details any) Result {
	return Result{Name: name, Severity: severity, Summary: summary, Details: details}
}

// Env is what the checks read.
type Env struct {
	Q       ledger.Querier
	Now     time.Time
	DataDir string // the directory holding the replica; "" skips the replica age
}

// Entries balanced.
func Balanced(ctx context.Context, e Env) Result {
	var n int
	if err := e.Q.QueryRowContext(ctx, `SELECT count(*) FROM v_unbalanced`).Scan(&n); err != nil {
		return fail("проводки сходятся", "urgent", err.Error(), nil)
	}
	if n > 0 {
		return fail("проводки сходятся", "urgent", fmt.Sprintf("%d проводок не сходятся в ноль", n), nil)
	}
	return pass("проводки сходятся", "все проводки сходятся в ноль")
}

func book(ctx context.Context, q ledger.Querier, account string) (float64, error) {
	var v int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM lines WHERE account = ?`, account).Scan(&v)
	return money.Micros(v).Float(), err
}

const cashCheck = "касса Bybit"

// BybitBalance: the `bybit` account against the balance read from the API, ±$1.
func BybitBalance(ctx context.Context, e Env) Result {
	const name = cashCheck
	var value int64
	var at string
	err := e.Q.QueryRowContext(ctx, `SELECT value, observed_at FROM observations WHERE kind = 'bybit_balance' AND subject = 'bybit'
		ORDER BY observed_at DESC LIMIT 1`).Scan(&value, &at)
	if err == sql.ErrNoRows {
		return fail(name, "warn", "баланса Bybit API ещё нет", nil)
	}
	if err != nil {
		return fail(name, "urgent", err.Error(), nil)
	}
	t, err := ts.Parse(at)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	if e.Now.Sub(t) > 3*time.Hour {
		return fail(name, "warn", fmt.Sprintf("баланс Bybit API старше 3 часов: %s", at), nil)
	}
	b, err := book(ctx, e.Q, "bybit")
	if err != nil {
		return fail(name, "urgent", err.Error(), nil)
	}
	pending, err := bybit.PendingUSDAt(ctx, e.Q, at)
	if err != nil {
		return fail(name, "urgent", err.Error(), nil)
	}
	api := money.Micros(value).Float()
	expected := b + pending
	if d := api - expected; math.Abs(d) > 1 {
		return fail(name, "urgent", fmt.Sprintf("по книгам %.2f, движения без строки журнала %+.2f, по API %.2f (Δ %+.2f)", b, pending, api, d),
			map[string]float64{"book": b, "pending": pending, "api": api})
	}
	return pass(name, fmt.Sprintf("по книгам %.2f, движения без строки журнала %+.2f, по API %.2f", b, pending, api))
}

// Coins: every coin on the exchange stands on an account. A stable coin is cash on `bybit`; any other coin
// without its own account bybit:<coin> is money the books do not see.
func Coins(ctx context.Context, e Env) Result {
	const name = "монеты Bybit на счетах"
	b, at, ok, err := bybit.LatestBalance(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	if !ok {
		return fail(name, "warn", "баланса Bybit API ещё нет", nil)
	}
	missing, err := bybit.CoinsWithoutAccount(ctx, e.Q, b)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	if len(missing) > 0 {
		return fail(name, "warn", fmt.Sprintf("монета без счёта на %s: %s — в книгах её нет", at, strings.Join(missing, "; ")), missing)
	}
	return pass(name, "у каждой монеты на бирже есть счёт")
}

// CoinsAtCost: each coin account against the coin left on the exchange at its average purchase price,
// ±$1. A payment in the coin is written at the market price of its day, so the account drifts from what
// the coin left cost; `ledger bybit coin-cost` shows the entries that bring it back. A coin that never
// moved through Funding (FUND or UNIFIED) and has a starting balance stands at that balance: it fails only
// when the quantity on the exchange moved away from the one read when the balance was posted.
func CoinsAtCost(ctx context.Context, e Env) Result {
	const name = "монеты Bybit по цене покупки"
	costs, err := bybit.CoinCosts(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	reading, at, read, err := bybit.LatestBalance(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	held := func(wallet, coin string) float64 {
		v, _ := strconv.ParseFloat(reading.Wallets[wallet][coin], 64)
		return v
	}
	var bad []string
	for _, c := range costs {
		if c.Unknown != "" {
			bad = append(bad, c.Account+": "+c.Unknown)
			continue
		}
		if read && reading.AllCoins {
			fund, unified := held("FUND", c.Coin), held("UNIFIED", c.Coin)
			price := 0.0
			if c.Qty > 1e-9 {
				price = c.Cost.Float() / c.Qty
			}
			switch o := c.Opened; { // set only for a coin without Funding rows
			case o != nil && !o.Known():
				bad = append(bad, fmt.Sprintf("%s: остаток на старте проведён %s, а полного показания биржи до него нет — с чем сверять количество, не знаю", c.Account, o.Posted))
			case o != nil && o.Qty <= 1e-9:
				bad = append(bad, fmt.Sprintf("%s: остаток на старте проведён %s, а %s на бирже тогда не было", c.Account, o.Posted, c.Coin))
			case o != nil:
				if qty := fund + unified; math.Abs(qty-o.Qty)*c.Cost.Float()/o.Qty > 1 {
					bad = append(bad, fmt.Sprintf("%s: на старте %g %s (%s, показание %s), на бирже %g на %s — монета двигалась мимо журнала Funding",
						c.Account, o.Qty, c.Coin, o.Wallets, o.At, qty, at))
				}
			case unified > 0:
				bad = append(bad, fmt.Sprintf("%s: в UNIFIED %g %s, журнала Funding у него нет — цены покупки не знаю", c.Account, unified, c.Coin))
			case c.Rows == 0 && fund > 0:
				bad = append(bad, fmt.Sprintf("%s: на бирже %g %s, строк Funding и остатка на старте нет — цены покупки не знаю", c.Account, fund, c.Coin))
			case math.Abs(fund-c.Qty)*price > 1:
				bad = append(bad, fmt.Sprintf("%s: остаток по Funding %g %s, на бирже %g на %s", c.Account, c.Qty, c.Coin, fund, at))
			}
		}
		if d := c.Book - c.Cost; math.Abs(d.Float()) > 1 {
			bad = append(bad, fmt.Sprintf("%s: по книгам %.2f, по средней цене покупки %.2f (%g %s), Δ %+.2f — разница цены платежей монетой, поправки: ledger bybit coin-cost",
				c.Account, c.Book.Float(), c.Cost.Float(), c.Qty, c.Coin, d.Float()))
		}
	}
	if len(bad) > 0 {
		return fail(name, "warn", strings.Join(bad, "; "), bad)
	}
	if len(costs) == 0 {
		return pass(name, "счетов монет нет")
	}
	return pass(name, fmt.Sprintf("%d счетов монет по цене покупки ±$1", len(costs)))
}

// frozenAccounts are accounts of closed or frozen cards: their balance is history, not cash.
func frozenAccounts(ctx context.Context, q ledger.Querier) map[string]bool {
	out := map[string]bool{}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'frozen_accounts'`).Scan(&raw); err == nil {
		var list []string
		_ = json.Unmarshal([]byte(raw), &list)
		for _, a := range list {
			out[a] = true
		}
	}
	return out
}

// CardAnchors: each active card against the bot's last «Доступный баланс», ±$1. Purchases the owner has
// not decided yet are open proposals: they already left the card but not the books.
func CardAnchors(ctx context.Context, e Env) Result {
	const name = "карты против бота"
	rows, err := e.Q.QueryContext(ctx, `SELECT o.subject, o.value, o.observed_at FROM observations o WHERE o.kind IN ('card_anchor', 'wallet_anchor')
		AND o.observed_at = (SELECT max(x.observed_at) FROM observations x WHERE x.kind = o.kind AND x.subject = o.subject)`)
	if err != nil {
		return fail(name, "urgent", err.Error(), nil)
	}
	type anchor struct {
		subject, at string
		value       float64
	}
	var list []anchor
	for rows.Next() {
		var a anchor
		var v int64
		if err := rows.Scan(&a.subject, &v, &a.at); err != nil {
			rows.Close()
			return fail(name, "urgent", err.Error(), nil)
		}
		a.value = money.Micros(v).Float()
		list = append(list, a)
	}
	rows.Close()
	frozen := frozenAccounts(ctx, e.Q)
	var bad, wallet []string
	seen := 0
	sort.Slice(list, func(i, j int) bool { return list[i].subject < list[j].subject })
	for _, a := range list {
		if frozen[a.subject] {
			continue
		}
		if a.subject == "cincin_wallet" {
			// The bot prints the wallet only when asked, so the anchor may be old: the books at its moment, and the
			// load fees by then not decided yet. Money on its way through the wallet can show here: a warning.
			at, err := ts.Parse(a.at)
			if err != nil {
				return fail(name, "urgent", err.Error(), nil)
			}
			micros, err := ledger.BalanceAt(ctx, e.Q, a.subject, at)
			if err != nil {
				return fail(name, "urgent", err.Error(), nil)
			}
			fees, err := loadFeesBy(ctx, e.Q, at)
			if err != nil {
				return fail(name, "urgent", err.Error(), nil)
			}
			b := micros.Float()
			if d := a.value - (b - fees); math.Abs(d) > 1 {
				wallet = append(wallet, fmt.Sprintf("кошелёк: по книгам на момент анкора %.2f (комиссии ждут решения %.2f), бот %.2f на %s, Δ %+.2f", b, fees, a.value, a.at, d))
			}
			continue
		}
		seen++
		b, err := book(ctx, e.Q, a.subject)
		if err != nil {
			return fail(name, "urgent", err.Error(), nil)
		}
		pending := 0.0
		card := strings.TrimPrefix(a.subject, "card:")
		_ = e.Q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CAST(json_extract(content, '$.amount') AS REAL)), 0) FROM proposals
			WHERE status = 'open' AND json_extract(content, '$.card') = ?`, card).Scan(&pending)
		if d := a.value - (b - pending); math.Abs(d) > 1 {
			bad = append(bad, fmt.Sprintf("%s: по книгам %.2f (ждёт решения %.2f), бот %.2f на %s, Δ %+.2f", a.subject, b, pending, a.value, a.at, d))
		}
	}
	if len(bad) > 0 {
		return fail(name, "urgent", strings.Join(append(bad, wallet...), "; "), nil)
	}
	if len(wallet) > 0 {
		return fail(name, "warn", strings.Join(wallet, "; "), nil)
	}
	if seen == 0 {
		return fail(name, "warn", "анкоров карт ещё нет", nil)
	}
	return pass(name, fmt.Sprintf("%d карт и кошелёк сходятся с ботом ±$1", seen))
}

// loadFeesBy sums the fees of card loads made by t that wait for the owner: the wallet paid them, the books not yet.
// coverDays is how far ahead an account must already hold the money of its coming charges.
const coverDays = 3

func accountName(code string) string {
	if n, ok := strings.CutPrefix(code, "card:"); ok {
		return "карта •" + n
	}
	return code
}

// CardCover: the charges of the next days against the money on the account they are taken from. A
// subscription that finds its card empty lapses, so the warning comes while the card can still be loaded.
// Today's charges are left out: they may be taken already.
func CardCover(ctx context.Context, e Env) Result {
	const name = "карты покрывают списания"
	n := e.Now.In(ts.MSK)
	from := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, ts.MSK)
	charges, err := free.Charges(ctx, e.Q, from, from.AddDate(0, 0, coverDays-1))
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	need, last, names := map[string]float64{}, map[string]string{}, map[string][]string{}
	var order []string
	for _, c := range charges {
		if _, ok := need[c.Account]; !ok {
			order = append(order, c.Account)
		}
		need[c.Account] += c.Amount
		last[c.Account] = c.Due
		names[c.Account] = append(names[c.Account], c.Name)
	}
	var short []string
	for _, a := range order {
		have, err := book(ctx, e.Q, a)
		if err != nil {
			return fail(name, "warn", err.Error(), nil)
		}
		if have+0.005 >= need[a] {
			continue
		}
		due, _ := time.Parse(ts.DateLayout, last[a])
		short = append(short, fmt.Sprintf("%s: на счёте %.2f, к списанию %.2f до %s (%s), не хватает %.2f",
			accountName(a), have, need[a], due.Format("02.01"), strings.Join(names[a], ", "), need[a]-have))
	}
	if len(short) == 0 {
		return pass(name, fmt.Sprintf("списаний на %d дня вперёд: %d, деньги на счетах есть", coverDays, len(charges)))
	}
	sum := strings.Join(short, "; ")
	if w, err := book(ctx, e.Q, "cincin_wallet"); err == nil && w >= 0.005 {
		sum += fmt.Sprintf("; в кошельке CinCin %.2f", w)
	}
	return fail(name, "warn", sum, nil)
}

func loadFeesBy(ctx context.Context, q ledger.Querier, t time.Time) (float64, error) {
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(CAST(json_extract(content, '$.fee') AS REAL), 0), COALESCE(json_extract(content, '$.at'), '')
		FROM proposals WHERE status = 'open' AND json_extract(content, '$.loaded_card') IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	sum := 0.0
	for rows.Next() {
		var fee float64
		var at string
		if err := rows.Scan(&fee, &at); err != nil {
			return 0, err
		}
		if loaded, err := ts.Parse(at); err == nil && loaded.After(t) {
			continue
		}
		sum += fee
	}
	return sum, rows.Err()
}

// Unlinked: a Bybit movement since the full journal is closed within 3 days (a row or an explicit personal).
func Unlinked(ctx context.Context, e Env) Result {
	const name = "движения Bybit разобраны"
	start := ""
	_ = e.Q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'full_ledger_from'`).Scan(&start)
	if start == "" {
		start = "2026-08-01"
	}
	limit := ts.Format(e.Now.AddDate(0, 0, -3))
	rows, err := e.Q.QueryContext(ctx, `SELECT key, occurred_at, amount, coin FROM v_bybit_unlinked WHERE occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at`, start, limit)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var k, at, amt, coin sql.NullString
		if err := rows.Scan(&k, &at, &amt, &coin); err != nil {
			return fail(name, "warn", err.Error(), nil)
		}
		bad = append(bad, fmt.Sprintf("%s %s %s %s", at.String[:min(10, len(at.String))], k.String, amt.String, coin.String))
	}
	if len(bad) > 0 {
		return fail(name, "warn", fmt.Sprintf("%d движений старше 3 дней без строки журнала: %s", len(bad), strings.Join(bad[:min(5, len(bad))], "; ")), bad)
	}
	return pass(name, "все движения старше 3 дней связаны со строками журнала или решены")
}

// Requests: a payout request equals the net it covers, ±$1.
func Requests(cv *month.Cover) Result {
	const name = "заявки равны покрытому net"
	var bad []string
	for _, q := range cv.Reqs {
		if q.Excess > 1 {
			bad = append(bad, fmt.Sprintf("%s %s: заявка %.2f больше доступного net %.2f", q.Page, ts.Date(q.T), q.Amount, q.Avail))
		}
	}
	if len(bad) > 0 {
		return fail(name, "warn", strings.Join(bad, "; "), nil)
	}
	return pass(name, fmt.Sprintf("%d заявок покрыты net ±$1", len(cv.Reqs)))
}

// KNorm: k of each transfer within the norm.
func KNorm(cv *month.Cover) Result {
	const name = "k переводов в норме"
	var bad []string
	n := 0
	for _, chain := range cv.Transfers {
		for _, t := range chain {
			if !t.HasK {
				continue
			}
			n++
			if t.K < cv.Cfg.KLow || t.K > cv.Cfg.KHigh {
				bad = append(bad, fmt.Sprintf("#%d k %.4f", t.N, t.K))
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return fail(name, "urgent", strings.Join(bad, "; ")+fmt.Sprintf(" вне %v–%v", cv.Cfg.KLow, cv.Cfg.KHigh), nil)
	}
	return pass(name, fmt.Sprintf("k %d переводов в %v–%v", n, cv.Cfg.KLow, cv.Cfg.KHigh))
}

// Overdue: a request whose transfer has not come for more than 11 days.
func Overdue(cv *month.Cover, now time.Time) Result {
	const name = "переводы не просрочены"
	const limit = 11 * 24 * time.Hour
	oldest := map[string]*month.Request{}
	for _, q := range cv.Reqs {
		if q.Pre || q.Transfer != nil {
			continue
		}
		if cv.Cfg.PayeeOf(cv.Cfg.Routes[q.Page].Model) != "model" {
			continue // an exchanger pays on its own schedule
		}
		m := cv.Cfg.Routes[q.Page].Model
		if oldest[m] == nil || q.T.Before(oldest[m].T) {
			oldest[m] = q
		}
	}
	var bad []string
	for m, q := range oldest {
		if age := now.Sub(q.T); age > limit {
			bad = append(bad, fmt.Sprintf("%s: заявка %s %s на %.2f без перевода %d дн.", m, q.Page, ts.Date(q.T), q.Amount, int(age.Hours()/24)))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return fail(name, "urgent", strings.Join(bad, "; "), nil)
	}
	return pass(name, "заявок без перевода дольше 11 дней нет")
}

// Deposits: the deposits in a transfer's tag agree with the requests it covers, by route; the recorded
// sum follows from them; the credited sum is on Bybit that date.
func Deposits(cv *month.Cover) Result {
	const name = "депозиты сходятся с заявками"
	covered := map[*month.Transfer]float64{}
	for _, q := range cv.Reqs {
		if q.Transfer != nil {
			covered[q.Transfer] += float64(q.Amount * cv.Cfg.RateOf(q.Page, q))
		}
	}
	var fails, notes []string
	for _, chain := range cv.Transfers {
		for _, t := range chain {
			if !t.HasK {
				continue
			}
			if len(t.Deps) == 0 {
				notes = append(notes, fmt.Sprintf("#%d без депозитов в пометке", t.N))
				continue
			}
			dep := 0.0
			for _, d := range t.Deps {
				dep += d
			}
			want := covered[t]
			if want > 0 {
				// On a route paying the agency 100% the deposits are the payout before the route's fee.
				r := (dep - t.Fee) / want
				if r < cv.Cfg.KLow || r > cv.Cfg.KHigh {
					fails = append(fails, fmt.Sprintf("#%d депозиты %.2f, по заявкам с учётом маршрута %.2f", t.N, dep, want))
				}
			}
			mult := cv.Cfg.Share
			if cv.Cfg.PayeeOf(t.Model) == "agency" {
				mult = 1
			}
			if calc := float64(dep * mult); math.Abs(calc-t.Amount) > 10 {
				fails = append(fails, fmt.Sprintf("#%d депозиты → %.2f, записано %.2f", t.N, calc, t.Amount))
			}
			hits, ok := cv.Src.BybitIncomes(t.Date)
			if !ok {
				fails = append(fails, "движения Bybit не читаются")
				continue
			}
			credited := t.Amount - t.Held - t.Fee
			found := false
			for _, a := range hits {
				found = found || math.Abs(a-credited) <= .01
			}
			if !found {
				fails = append(fails, fmt.Sprintf("#%d: зачисления %.2f на Bybit за %s нет", t.N, credited, t.Date))
			}
		}
	}
	sort.Strings(fails)
	if len(fails) > 0 {
		return fail(name, "warn", strings.Join(fails, "; "), notes)
	}
	return Result{Name: name, OK: true, Severity: "info", Summary: "пометки переводов сходятся с заявками и Bybit", Details: notes}
}

// Payables: the debt to each person against «К выплате» of their tabs, ±$1, tabs that do not parse and
// payout rows or tabs without a person.
func Payables(ctx context.Context, e Env) Result {
	const name = "долги равны «К выплате»"
	c, err := payroll.Check(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	if c.TabsRead == 0 {
		return fail(name, "warn", "таблицы выплат ещё не собраны", nil)
	}
	var bad []string
	for _, p := range c.People {
		if !p.OK {
			bad = append(bad, p.Person+": "+p.Reason)
		}
	}
	for _, r := range c.Rows {
		bad = append(bad, fmt.Sprintf("#%d %s: %s", r.Num, r.Name, r.Reason))
	}
	for _, t := range c.Tabs {
		bad = append(bad, t.Model+"/"+t.Name+": "+t.Reason)
	}
	if len(bad) > 0 {
		return fail(name, "warn", strings.Join(bad, "; "), nil)
	}
	if c.FactsFrom == "" {
		return pass(name, fmt.Sprintf("%d человек: долг равен К выплате ±$1", len(c.People)))
	}
	// From the switch to Hub's facts on the sheet is a mirror: its difference from the facts, and the facts no tab takes,
	// are said, never a failure.
	summary := fmt.Sprintf("%d человек: долг с днями с %s по таблице равен К выплате ±$1; с %s начисляют факты хаба", len(c.People),
		c.FactsFrom, c.FactsFrom)
	if len(c.Mirror) > 0 {
		const shown = 8
		var parts []string
		for i, d := range c.Mirror {
			if i == shown {
				parts = append(parts, fmt.Sprintf("и ещё %d", len(c.Mirror)-shown))
				break
			}
			parts = append(parts, fmt.Sprintf("%s %s: таблица %s, факты %s", d.Person, d.Date, money.Micros(d.SheetMicros).Cents(),
				money.Micros(d.BooksMicros).Cents()))
		}
		summary += "; таблица-зеркало расходится в " + fmt.Sprint(len(c.Mirror)) + " дн.: " + strings.Join(parts, ", ")
	}
	for _, u := range c.Unaccrued {
		who := u.Username + " (хаб " + fmt.Sprint(u.UserID) + ")"
		if u.Person != "" {
			who = u.Person
		}
		summary += fmt.Sprintf("; не начислено фактами: %s, %s, %s за %d дн. — %s", who, u.Model, money.Micros(u.NetMicros).Cents(), u.Days, u.Reason)
	}
	return Result{Name: name, OK: true, Severity: "info", Summary: summary, Details: map[string]any{"mirror": c.Mirror, "unaccrued": c.Unaccrued}}
}

// DriftOf: a closed month whose figure now differs by $100 or more from what the owner knows.
func DriftOf(ctx context.Context, q ledger.Querier, cv *month.Cover, tabs []month.Tab) Result {
	const name = "дрейф закрытых месяцев"
	rows, err := month.Drift(ctx, q, cv, tabs)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	var bad []string
	for _, r := range rows {
		if r.Flag {
			bad = append(bad, fmt.Sprintf("%s: сообщено %.2f, сейчас %.2f (Δ %+.2f): %s", r.Month, r.Reported, r.Now, r.Delta, r.Reason))
		}
	}
	if len(bad) > 0 {
		return fail(name, "warn", strings.Join(bad, "; "), nil)
	}
	return pass(name, fmt.Sprintf("%d закрытых месяцев без дрейфа ≥ $100", len(rows)))
}

// Copies: the age of the Litestream replica and of the last copy on the Mac.
func Copies(ctx context.Context, e Env) Result {
	const name = "копии базы"
	var problems []string
	if e.DataDir != "" {
		var newest time.Time
		_ = filepath.WalkDir(filepath.Join(e.DataDir, "replica"), func(_ string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		if newest.IsZero() {
			problems = append(problems, "реплики Litestream нет")
		} else if e.Now.Sub(newest) > 3*time.Hour {
			problems = append(problems, fmt.Sprintf("реплика Litestream старше 3 часов: %s", ts.Format(newest)))
		}
	}
	var at string
	err := e.Q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = 'backup:mac'`).Scan(&at)
	if err == sql.ErrNoRows {
		problems = append(problems, "копий на маке ещё не было")
	} else if err != nil {
		problems = append(problems, err.Error())
	} else if t, perr := ts.Parse(at); perr == nil && e.Now.Sub(t) > 24*time.Hour {
		problems = append(problems, fmt.Sprintf("копия на маке старше суток: %s", at))
	}
	if len(problems) > 0 {
		return fail(name, "warn", strings.Join(problems, "; "), nil)
	}
	return pass(name, "реплика и копия на маке свежие")
}

// Sources: collectors are alive. The Mac may sleep, so its sources only warn.
func Sources(ctx context.Context, e Env) Result {
	const name = "источники живы"
	var problems []string
	var lastHub sql.NullString
	if err := e.Q.QueryRowContext(ctx, `SELECT MAX(checked_at) FROM hub_sync`).Scan(&lastHub); err == nil {
		if lastHub.Valid {
			if t, err := ts.Parse(lastHub.String); err == nil && e.Now.Sub(t) > 3*time.Hour {
				problems = append(problems, "хаб не выгружался с "+lastHub.String)
			}
		} else {
			problems = append(problems, "хаб ещё не выгружался")
		}
	}
	sts, err := hub.Statuses(ctx, e.Q)
	if err == nil {
		for _, s := range sts {
			if s.Status != "ok" {
				problems = append(problems, fmt.Sprintf("хаб %s %s: неизвестно: %s", s.Page, s.Dataset, strings.Join(s.Reasons, "; ")))
			}
		}
	}
	var status string
	if err := e.Q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = 'bybit:status'`).Scan(&status); err == nil {
		var st bybit.Status
		if json.Unmarshal([]byte(status), &st) == nil {
			if !st.OK {
				var errs []string
				for k, v := range st.Errors {
					errs = append(errs, k+": "+v)
				}
				sort.Strings(errs)
				problems = append(problems, "Bybit: "+strings.Join(errs, "; "))
			}
			if t, err := ts.Parse(st.At); err == nil && e.Now.Sub(t) > time.Hour {
				problems = append(problems, "Bybit не собирался с "+st.At)
			}
			problems = append(problems, st.Mismatches...)
			if st.Funding != nil && len(st.Funding.ChainGaps) > 0 {
				problems = append(problems, fmt.Sprintf("Bybit Funding: %d разрывов цепочки остатка", len(st.Funding.ChainGaps)))
			}
			if st.Funding != nil {
				for _, f := range st.Funding.UnmatchedFunding {
					if t, err := ts.Parse(f.At); err == nil && e.Now.Sub(t) > 48*time.Hour {
						problems = append(problems, fmt.Sprintf("Bybit Funding: движение %s %s (%s) от %s без операции в выгрузке дольше 48 часов", f.Signed, f.Coin, f.Type, f.At))
					}
				}
			}
			if st.Key != nil && !st.Key.ReadOnly {
				problems = append(problems, "ключ Bybit НЕ только на чтение — пересоздать")
			}
		}
	} else if err == sql.ErrNoRows {
		problems = append(problems, "Bybit ещё не собирался")
	}
	for _, src := range []string{"sheets", "cincin_bot"} {
		// The last new fact or the last batch of repeats, whichever is later: a quiet bot is not a dead collector.
		var at sql.NullString
		_ = e.Q.QueryRowContext(ctx, `SELECT MAX(at) FROM (SELECT MAX(received_at) AS at FROM events WHERE source = ?
			UNION ALL SELECT value FROM cursors WHERE name = ?)`, src, events.ContactCursor(src)).Scan(&at)
		if at.Valid {
			if t, err := ts.Parse(at.String); err == nil && e.Now.Sub(t) > 24*time.Hour {
				problems = append(problems, fmt.Sprintf("%s молчит с %s (мак спит?)", src, at.String))
			}
		}
	}
	rows, err := e.Q.QueryContext(ctx, `SELECT key, first_seen_at, last_status FROM bybit_pending WHERE resolved_at IS NULL`)
	if err == nil {
		for rows.Next() {
			var k, at, st string
			if rows.Scan(&k, &at, &st) == nil {
				if t, err := ts.Parse(at); err == nil && e.Now.Sub(t) > 72*time.Hour {
					problems = append(problems, fmt.Sprintf("вывод %s висит в статусе %s дольше 72 часов", k, st))
				}
			}
		}
		rows.Close()
	}
	if len(problems) > 0 {
		return fail(name, "warn", strings.Join(problems, "; "), problems)
	}
	return pass(name, "хаб, Bybit и сборщики мака в порядке")
}

// ReportPosts: the chatters' posts of the last 14 days that the pay base cannot use — an author or a page name not in
// the settings, a total that does not add up, a date that does not read or lies far from the day of posting. A warning:
// nothing is accrued from posts yet, but a missing mapping should be noticed before it is.
func ReportPosts(ctx context.Context, e Env) Result {
	const name = "отчёты разобраны"
	from := ts.Date(e.Now.Add(-13 * 24 * time.Hour))
	list, err := reports.Posts(ctx, e.Q, from, ts.Date(e.Now), true)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	var bad []string
	for _, p := range list {
		var why []string
		for i, f := range p.Flags {
			if reports.Blocking(f) || f == reports.FlagUnknownPage {
				why = append(why, p.FlagTexts[i])
			}
		}
		if len(why) == 0 {
			continue
		}
		who, date := p.Sender, "без даты"
		if p.Tag != "" {
			who += " " + p.Tag
		}
		if p.Person != nil {
			who = *p.Person
		}
		if p.ReportDate != nil {
			date = *p.ReportDate
		}
		bad = append(bad, fmt.Sprintf("%s, %s (сообщение %d): %s", who, date, p.MsgID, strings.Join(why, "; ")))
	}
	if len(bad) > 0 {
		return fail(name, "warn", fmt.Sprintf("%d постов «Отчётности» не идут в базу оплаты целиком: %s", len(bad), strings.Join(bad, " | ")), bad)
	}
	return pass(name, "посты «Отчётности» за 14 дней разобраны без блокирующих пометок")
}

// Run executes every check. A coverage that cannot be built fails its own checks, not the rest.
func Run(ctx context.Context, e Env) []Result {
	out := []Result{Balanced(ctx, e), BybitBalance(ctx, e), Coins(ctx, e), CoinsAtCost(ctx, e), CardAnchors(ctx, e), CardCover(ctx, e), Unlinked(ctx, e), Payables(ctx, e), Copies(ctx, e), Sources(ctx, e), ReportPosts(ctx, e)}
	cv, tabs, _, err := month.PrepareCover(ctx, e.Q, month.Inputs{})
	if err != nil {
		for _, n := range []string{"заявки равны покрытому net", "k переводов в норме", "переводы не просрочены", "депозиты сходятся с заявками", "дрейф закрытых месяцев"} {
			out = append(out, fail(n, "warn", "неизвестно: "+err.Error(), nil))
		}
		return out
	}
	out = append(out, Rates(ctx, e))
	return append(out, Requests(cv), KNorm(cv), Overdue(cv, e.Now), Deposits(cv), DriftOf(ctx, e.Q, cv, tabs))
}

var percentRe = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*%`)

// Rates: the share in a payout tab's header against the rate on the person's card in /OS. The card is the
// canon of the rate; a tab with another share pays by the wrong number.
func Rates(ctx context.Context, e Env) Result {
	const name = "ставки вкладок как в карточках"
	rows, err := e.Q.QueryContext(ctx, `SELECT p.slug, p.rate, t.model, t.rate FROM payroll_tabs t JOIN people p ON p.slug = t.person
		WHERE t.rate IS NOT NULL AND t.listed = 1 ORDER BY p.slug, t.model`)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	defer rows.Close()
	var bad []string
	n := 0
	for rows.Next() {
		var slug, card, model string
		var tab float64
		if err := rows.Scan(&slug, &card, &model, &tab); err != nil {
			return fail(name, "warn", err.Error(), nil)
		}
		m := percentRe.FindStringSubmatch(card)
		if m == nil {
			continue // a fixed rate: nothing to compare in a share
		}
		want, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
		if err != nil {
			continue
		}
		n++
		if math.Abs(tab*100-want) > 0.01 {
			bad = append(bad, fmt.Sprintf("%s (%s): во вкладке %.4g%%, в карточке %s%%", slug, model, tab*100, m[1]))
		}
	}
	if err := rows.Err(); err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	if len(bad) > 0 {
		return fail(name, "warn", strings.Join(bad, "; "), nil)
	}
	return pass(name, fmt.Sprintf("%d вкладок с той же долей, что в карточке", n))
}

// Save writes the results to `checks`.
func Save(ctx context.Context, q ledger.Querier, rs []Result, started, finished time.Time) error {
	for _, r := range rs {
		raw, err := json.Marshal(r.Details)
		if err != nil || r.Details == nil {
			raw = []byte("{}")
		}
		ok := 0
		if r.OK {
			ok = 1
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO checks(name, started_at, finished_at, ok, severity, summary, details) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.Name, ts.Format(started), ts.Format(finished), ok, r.Severity, r.Summary, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// Latest reads the last result of every check.
func Latest(ctx context.Context, q ledger.Querier) ([]Result, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, ok, severity, summary, details, finished_at FROM v_checks ORDER BY name`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Result
	at := ""
	for rows.Next() {
		var r Result
		var ok int
		var details, finished string
		if err := rows.Scan(&r.Name, &ok, &r.Severity, &r.Summary, &details, &finished); err != nil {
			return nil, "", err
		}
		r.OK = ok == 1
		if details != "{}" && details != "null" {
			r.Details = json.RawMessage(details)
		}
		if finished > at {
			at = finished
		}
		out = append(out, r)
	}
	return out, at, rows.Err()
}

// WithStale adds a failure when the last run is old: the job stopped, and its old green must not read as green.
func WithStale(rs []Result, at string, now time.Time) []Result {
	const staleRun = 2 * time.Hour
	if at == "" {
		return append(rs, Result{Name: "проверки идут", Severity: "warn", Summary: "проверки ещё не запускались"})
	}
	if t, err := ts.Parse(at); err == nil && now.Sub(t) > staleRun {
		return append(rs, Result{Name: "проверки идут", Severity: "warn", Summary: "последний прогон проверок " + at + ": задание на сервере стоит, зелёному верить нельзя"})
	}
	return rs
}

// Urgent returns urgent failures to send: new ones and those last sent over `every` ago. It marks them
// sent and forgets the ones that recovered.
func Urgent(ctx context.Context, q ledger.Querier, rs []Result, now time.Time, every time.Duration) ([]Result, error) {
	failing := map[string]bool{}
	var out []Result
	for _, r := range rs {
		if r.OK || r.Severity != "urgent" {
			continue
		}
		failing[r.Name] = true
		if r.Name == cashCheck {
			// The balance and the movements are read at different moments: one run can show a passing difference.
			var before int
			err := q.QueryRowContext(ctx, `SELECT ok FROM checks WHERE name = ? ORDER BY id DESC LIMIT 1 OFFSET 1`, r.Name).Scan(&before)
			if err == sql.ErrNoRows || (err == nil && before == 1) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		var last string
		err := q.QueryRowContext(ctx, `SELECT last_sent FROM alerts WHERE name = ?`, r.Name).Scan(&last)
		send := err == sql.ErrNoRows
		if err == nil {
			if t, perr := ts.Parse(last); perr == nil && now.Sub(t) >= every {
				send = true
			}
		} else if err != sql.ErrNoRows {
			return nil, err
		}
		if send {
			out = append(out, r)
			if _, err := q.ExecContext(ctx, `INSERT INTO alerts(name, first_seen, last_sent, summary) VALUES (?, ?, ?, ?)
				ON CONFLICT(name) DO UPDATE SET last_sent = excluded.last_sent, summary = excluded.summary`,
				r.Name, ts.Format(now), ts.Format(now), r.Summary); err != nil {
				return nil, err
			}
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT name FROM alerts`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if !failing[n] {
			if _, err := q.ExecContext(ctx, `DELETE FROM alerts WHERE name = ?`, n); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
