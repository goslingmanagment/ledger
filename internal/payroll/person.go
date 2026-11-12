package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/payouts/person is one person's own pay, for Hub's «Мои смены» (the chatter's «Мои деньги»): the days the books
// accrued in a window with the posts of «Отчётность» a day of the reports came from, what payouts paid of each day and
// with which journal row, the manual postings on the debt, the payouts, and the debt now. Hub's server asks for the
// person of the session with its `payouts` key, and only that scope opens the route (a read key does not). The answer
// holds that one person and no pay terms: no rate, no fix per shift, no revenue a day was priced from, no «К выплате»
// of a tab and no estimate at a rate.

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payouts/person", auth.Require(s, auth.ScopePayouts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			slug := q.Get("person")
			if !slugRe.MatchString(slug) {
				server.WriteError(w, http.StatusBadRequest, fmt.Sprintf("person: нужен код человека (slug), а не %q", slug))
				return
			}
			from, to, err := PayoutWindow(q.Get("from"), q.Get("to"))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			ans, err := ReadPersonPay(r.Context(), s.Store.RO, slug, from, to, ts.Now())
			if errors.Is(err, ErrNoPayoutPerson) {
				server.WriteError(w, http.StatusNotFound, err.Error())
				return
			}
			if err != nil {
				s.Log.Error("person pay", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "деньги человека не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
	})
}

// Day states of a person's day.
const (
	DayPaid  = "paid"   // the payouts covered the day
	DayToPay = "to_pay" // some or all of the day is still owed; paidMicros says how much is paid
)

// PersonDay is one day the books accrued to the person on one model's tab.
type PersonDay struct {
	Date         string `json:"date"`
	Model        string `json:"model"`
	AmountMicros int64  `json:"amountMicros"`
	Worked       bool   `json:"worked"`
	// Source is where the day came from: "sheet" — the payout tab, "facts" — Hub's facts, "reports" — the posted shift
	// reports (from settings.payroll_reports_from).
	Source string `json:"source"`
	// Posts are the «Отчётность» posts (shift_report_posts.id) a day of the reports was counted from; empty otherwise.
	Posts []int64 `json:"posts"`
	// State is DayPaid or DayToPay by the FIFO of payouts (payable_days); "" — the day accrued nothing.
	State        string `json:"state"`
	PaidMicros   int64  `json:"paidMicros"`
	UnpaidMicros int64  `json:"unpaidMicros"`
	// PaidBy are the journal rows (documents.num) of the payouts that paid the day, in the order they paid.
	PaidBy []int `json:"paidBy"`
}

// PersonAdjustment is a manual posting on the person's debt: neither a day, nor a payout, nor the opening debt.
type PersonAdjustment struct {
	Date string `json:"date"`
	// AmountMicros: plus — adds to what the agency owes (a bonus), minus — takes off it (a fine, an advance).
	AmountMicros int64  `json:"amountMicros"`
	Memo         string `json:"memo"`
	Document     *int   `json:"document"` // the journal row (documents.num); null — a posting without one
}

// PersonPayout is a payout to the person: the part of a salary row of the journal that closed the debt.
type PersonPayout struct {
	Date         string `json:"date"`
	Document     int    `json:"document"` // the journal row of the transfer (documents.num)
	AmountMicros int64  `json:"amountMicros"`
}

// PersonPayChecks says how fresh the debts are: they are rebuilt every hour on the server.
type PersonPayChecks struct {
	Stale         bool    `json:"stale"`
	LastRebuildAt *string `json:"lastRebuildAt"`
}

// PersonPayAnswer is the reply of GET /v1/payouts/person.
type PersonPayAnswer struct {
	OK          bool   `json:"ok"`
	Person      string `json:"person"`
	Name        string `json:"name"`
	From        string `json:"from"`
	To          string `json:"to"`
	AccrualFrom string `json:"accrualFrom"` // days before it are in the opening debt, not one by one
	FactsFrom   string `json:"factsFrom"`
	ReportsFrom string `json:"reportsFrom"`
	GeneratedAt string `json:"generatedAt"`
	// AccruedMicros is the days of the window; PaidMicros the payouts dated in the window.
	AccruedMicros int64 `json:"accruedMicros"`
	PaidMicros    int64 `json:"paidMicros"`
	// BalanceMicros is the debt now, as v_payables and `ledger payables` count it: plus — the agency owes, minus — paid ahead.
	BalanceMicros    int64              `json:"balanceMicros"`
	OldestUnpaidDate *string            `json:"oldestUnpaidDate"`
	LastPayout       *PersonPayout      `json:"lastPayout"` // the latest payout ever, null — none yet
	Days             []PersonDay        `json:"days"`
	Adjustments      []PersonAdjustment `json:"adjustments"` // dated in the window
	Payouts          []PersonPayout     `json:"payouts"`     // dated in the window
	Checks           PersonPayChecks    `json:"checks"`
}

// ReadPersonPay reads one person's pay for the days from…to in one snapshot. The days are those of GET /v1/payouts for the
// person (the active accrual postings), one per date and model; balance, oldestUnpaid and lastPayout are now. Every query
// is of this person; ErrNoPayoutPerson when the code names nobody with a payout tab.
func ReadPersonPay(ctx context.Context, db *sql.DB, slug, from, to string, now time.Time) (PersonPayAnswer, error) {
	ans := PersonPayAnswer{OK: true, Person: slug, From: from, To: to, GeneratedAt: ts.Format(now), Days: []PersonDay{},
		Adjustments: []PersonAdjustment{}, Payouts: []PersonPayout{}}
	tx, err := db.BeginTx(ctx, nil) // on the read pool: one snapshot
	if err != nil {
		return ans, err
	}
	defer tx.Rollback()
	var oldest sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT p.name, v.balance_micros, v.oldest_unpaid FROM people p JOIN v_payables v ON v.person = p.slug
		WHERE p.slug = ? AND (EXISTS (SELECT 1 FROM payroll_tabs t WHERE t.person = p.slug) OR `+registryPayee+`)`, slug).Scan(&ans.Name, &ans.BalanceMicros, &oldest)
	if errors.Is(err, sql.ErrNoRows) {
		return ans, fmt.Errorf("%s: %w", slug, ErrNoPayoutPerson)
	}
	if err != nil {
		return ans, err
	}
	if oldest.Valid {
		ans.OldestUnpaidDate = &oldest.String
	}
	if ans.AccrualFrom, err = AccrualFrom(ctx, tx); err != nil {
		return ans, err
	}
	if ans.FactsFrom, err = FactsFrom(ctx, tx); err != nil {
		return ans, err
	}
	if ans.ReportsFrom, err = ReportsFrom(ctx, tx); err != nil {
		return ans, err
	}
	p := &PayoutPerson{Slug: slug, Days: []PayoutDay{}}
	byID := map[string]*PayoutPerson{slug: p}
	if err := payoutDays(ctx, tx, from, to, ans.AccrualFrom, byID); err != nil {
		return ans, err
	}
	if err := payoutPaid(ctx, tx, from, to, byID); err != nil {
		return ans, err
	}
	ans.AccruedMicros, ans.PaidMicros = p.AccruedMicros, p.PaidMicros
	if ans.Days, err = personDays(ctx, tx, slug, from, to, p.Days); err != nil {
		return ans, err
	}
	if ans.Adjustments, err = personAdjustments(ctx, tx, slug, from, to); err != nil {
		return ans, err
	}
	payouts, err := personPayouts(ctx, tx, slug)
	if err != nil {
		return ans, err
	}
	for _, x := range payouts {
		if x.Date >= from && x.Date <= to {
			ans.Payouts = append(ans.Payouts, x)
		}
	}
	if n := len(payouts); n > 0 {
		last := payouts[n-1]
		ans.LastPayout = &last
	}
	var rebuilt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM cursors WHERE name = ?`, docsCursor).Scan(&rebuilt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ans, err
	}
	ans.Checks.Stale = true
	if rebuilt.Valid {
		ans.Checks.LastRebuildAt = &rebuilt.String
		if t, err := ts.Parse(rebuilt.String); err == nil && now.Sub(t) <= staleRebuild {
			ans.Checks.Stale = false
		}
	}
	return ans, tx.Commit()
}

// personDays folds the days of the window into one per date and model and puts on each what the payouts paid of it
// (payable_days) and the posts a day of the reports came from (report_accruals.posts). The revenue, the pages and the
// pots of a day stay out: with the amount they would give the rate.
func personDays(ctx context.Context, q ledger.Querier, slug, from, to string, days []PayoutDay) ([]PersonDay, error) {
	type fifo struct {
		amount, paid, unpaid int64
		by                   []int
	}
	state := map[string]*fifo{} // "<model>\x00<date>"
	rows, err := q.QueryContext(ctx, `SELECT model, date, amount, paid, unpaid, paid_by FROM payable_days
		WHERE person = ? AND kind = 'day' AND date BETWEEN ? AND ? ORDER BY seq`, slug, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model, date, by string
		var amount, paid, unpaid int64
		if err := rows.Scan(&model, &date, &amount, &paid, &unpaid, &by); err != nil {
			rows.Close()
			return nil, err
		}
		var nums []int
		if err := json.Unmarshal([]byte(by), &nums); err != nil {
			rows.Close()
			return nil, fmt.Errorf("payable_days %s %s %s: paid_by: %w", slug, model, date, err)
		}
		k := model + "\x00" + date
		f := state[k]
		if f == nil {
			f = &fifo{}
			state[k] = f
		}
		f.amount, f.paid, f.unpaid = f.amount+amount, f.paid+paid, f.unpaid+unpaid
		for _, n := range nums {
			if len(f.by) == 0 || f.by[len(f.by)-1] != n {
				f.by = append(f.by, n)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	posts := map[string][]int64{} // "<model slug>\x00<date>"
	rows, err = q.QueryContext(ctx, `SELECT model, day, posts FROM report_accruals WHERE person = ? AND day BETWEEN ? AND ?
		ORDER BY model, day`, slug, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model, day, list string
		if err := rows.Scan(&model, &day, &list); err != nil {
			rows.Close()
			return nil, err
		}
		var ids []int64
		if err := json.Unmarshal([]byte(list), &ids); err != nil {
			rows.Close()
			return nil, fmt.Errorf("report_accruals %s %s %s: posts: %w", slug, model, day, err)
		}
		k := ledger.Slug(model) + "\x00" + day
		posts[k] = append(posts[k], ids...)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := []PersonDay{}
	at := map[string]int{} // "<model>\x00<date>" → index in out
	for _, d := range days {
		k := d.Model + "\x00" + d.Date
		if i, ok := at[k]; ok {
			out[i].AmountMicros += d.AmountMicros
			out[i].Worked = out[i].Worked || d.Worked
			continue
		}
		at[k] = len(out)
		pd := PersonDay{Date: d.Date, Model: d.Model, AmountMicros: d.AmountMicros, Worked: d.Worked, Source: d.Source,
			Posts: []int64{}, PaidBy: []int{}}
		if d.Source == SourceReports {
			pd.Posts = uniqueIDs(posts[ledger.Slug(d.Model)+"\x00"+d.Date])
		}
		out = append(out, pd)
	}
	for i := range out {
		f := state[out[i].Model+"\x00"+out[i].Date]
		if f == nil {
			continue
		}
		out[i].PaidMicros, out[i].UnpaidMicros = f.paid, f.unpaid
		if f.by != nil {
			out[i].PaidBy = f.by
		}
		switch {
		case f.unpaid > 0:
			out[i].State = DayToPay
		case f.paid > 0:
			out[i].State = DayPaid
		}
	}
	return out, nil
}

func uniqueIDs(ids []int64) []int64 {
	out := []int64{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// personAdjustments reads the active manual postings on the person's debt dated in the window: what is neither a day,
// nor a payout, nor the opening debt, nor a reversal.
func personAdjustments(ctx context.Context, q ledger.Querier, slug, from, to string) ([]PersonAdjustment, error) {
	rows, err := q.QueryContext(ctx, `SELECT substr(e.effective_at, 1, 10), -SUM(l.amount), e.memo, d.num
		FROM entries e JOIN lines l ON l.entry_id = e.id LEFT JOIN documents d ON d.id = e.document_id
		WHERE l.account = ? AND e.kind NOT IN (?, ?, ?) AND e.reverses_id IS NULL
		  AND substr(e.effective_at, 1, 10) BETWEEN ? AND ?
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		GROUP BY e.id ORDER BY e.effective_at, e.id`, "payable:"+slug, KindAccrual, KindClearing, KindOpening, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonAdjustment{}
	for rows.Next() {
		var a PersonAdjustment
		var num sql.NullInt64
		if err := rows.Scan(&a.Date, &a.AmountMicros, &a.Memo, &num); err != nil {
			return nil, err
		}
		if num.Valid {
			n := int(num.Int64)
			a.Document = &n
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// personPayouts reads every active payout to the person, oldest first: the clearing postings of the salary rows.
func personPayouts(ctx context.Context, q ledger.Querier, slug string) ([]PersonPayout, error) {
	rows, err := q.QueryContext(ctx, `SELECT substr(e.effective_at, 1, 10), COALESCE(d.num, 0), SUM(l.amount)
		FROM entries e JOIN lines l ON l.entry_id = e.id LEFT JOIN documents d ON d.id = e.document_id
		WHERE e.kind = ? AND l.account = ? AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		GROUP BY e.id ORDER BY e.effective_at, e.id`, KindClearing, "payable:"+slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonPayout{}
	for rows.Next() {
		var x PersonPayout
		if err := rows.Scan(&x.Date, &x.Document, &x.AmountMicros); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
