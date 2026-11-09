package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/payouts is the chatters' pay for Hub's Workboard «Выплаты» (release 2, L6). Hub's server calls it
// on the owner's behalf with a `payouts` key, which opens this route and nothing else: the answer holds only
// people with a payout tab, never the owner's draws, a model's share, other expenses, cash or SQL.

// MaxPayoutDays is the widest window of GET /v1/payouts, in MSK days.
const MaxPayoutDays = 62

// How old the inputs may be before the answer is flagged stale: the sheets are collected every hour on the
// Mac, the debts are rebuilt every hour on the server, so two missed runs.
const (
	staleSheets  = 3 * time.Hour
	staleRebuild = 2 * time.Hour
)

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payouts", auth.RequireAny(s, []string{auth.ScopePayouts, auth.ScopeRead}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			from, to, err := PayoutWindow(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			ans, err := ReadPayouts(r.Context(), s.Store.RO, from, to, ts.Now())
			if err != nil {
				s.Log.Error("payouts", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "выплаты не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
	})
}

// PayoutPage is one page row of the sheet's day: the revenue cell the pay percent is taken from, not pay.
type PayoutPage struct {
	Page          string `json:"page"`
	RevenueMicros int64  `json:"revenueMicros"`
}

// PayoutDay is one accrued day of one tab.
type PayoutDay struct {
	Date         string       `json:"date"`
	Model        string       `json:"model"`
	AmountMicros int64        `json:"amountMicros"`
	Worked       bool         `json:"worked"`
	Pages        []PayoutPage `json:"pages,omitempty"`
	Source       string       `json:"source"` // "sheet": the payout tab; the facts of Hub come with L4
}

// UnpaidDay is what is left of a debt after the FIFO of payouts (payable_days).
type UnpaidDay struct {
	Date         string `json:"date"`
	Kind         string `json:"kind"` // opening (the frozen debt from before accrualFrom), day, other (a manual posting)
	Model        string `json:"model,omitempty"`
	UnpaidMicros int64  `json:"unpaidMicros"`
}

// PayoutPerson is one person's pay: the window's days and payouts, and the debt as it stands now.
type PayoutPerson struct {
	Slug             string      `json:"slug"`
	Name             string      `json:"name"`
	Aliases          []string    `json:"aliases"`
	Status           string      `json:"status"`
	Days             []PayoutDay `json:"days"`
	AccruedMicros    int64       `json:"accruedMicros"`    // the days of the window
	PaidMicros       int64       `json:"paidMicros"`       // the payouts dated in the window
	BalanceMicros    int64       `json:"balanceMicros"`    // now, v_payables: plus — the agency owes
	ToPayMicros      *int64      `json:"toPayMicros"`      // «К выплате» of all the person's tabs; null — unknown
	OldestUnpaidDate *string     `json:"oldestUnpaidDate"` // now
	UnpaidDays       []UnpaidDay `json:"unpaidDays"`       // now, oldest first
	// Unentered is «не занесено» now (v_payables): the tab's empty days on which Hub shows money in the
	// person's scheduled shift — pay the sheet has not got yet, so not in the debt.
	Unentered PayoutUnentered `json:"unentered"`
}

// PayoutUnentered is «не занесено» of a person: days, the share estimated at the tab's rate, and the days Hub
// could not prove (unknown, not zero).
type PayoutUnentered struct {
	Days        int   `json:"days"`
	ShareMicros int64 `json:"shareMicros"`
	Unknown     int   `json:"unknown"`
}

// PayoutChecks says how fresh the inputs are.
type PayoutChecks struct {
	Stale               bool    `json:"stale"`
	LastSheetsCollectAt *string `json:"lastSheetsCollectAt"`
	LastRebuildAt       *string `json:"lastRebuildAt"`
}

// PayoutsAnswer is the reply of GET /v1/payouts.
type PayoutsAnswer struct {
	OK          bool           `json:"ok"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	AccrualFrom string         `json:"accrualFrom"` // days before it are in the opening debt, not one by one
	GeneratedAt string         `json:"generatedAt"`
	People      []PayoutPerson `json:"people"`
	Checks      PayoutChecks   `json:"checks"`
}

// PayoutWindow checks a window of MSK dates: both given, from not after to, at most MaxPayoutDays days.
func PayoutWindow(from, to string) (string, string, error) {
	parse := func(name, v string) (time.Time, error) {
		t, err := time.ParseInLocation(ts.DateLayout, v, ts.MSK)
		if err != nil || t.Format(ts.DateLayout) != v {
			return t, fmt.Errorf("%s: нужна дата ГГГГ-ММ-ДД, а не %q", name, v)
		}
		return t, nil
	}
	f, err := parse("from", from)
	if err != nil {
		return "", "", err
	}
	t, err := parse("to", to)
	if err != nil {
		return "", "", err
	}
	if t.Before(f) {
		return "", "", fmt.Errorf("окно %s — %s: from позже to", from, to)
	}
	if days := int(t.Sub(f).Hours()/24) + 1; days > MaxPayoutDays {
		return "", "", fmt.Errorf("окно %s — %s: %d дней, можно не больше %d", from, to, days, MaxPayoutDays)
	}
	return from, to, nil
}

// ReadPayouts reads the pay of every person with a payout tab for the days from…to, in one snapshot.
//
// The days are the active accrual postings (the books, as v_payables and `ledger payables` count them),
// with worked and the page cells from payroll_days; a worked day that accrued nothing is listed at zero. Days
// before accrualFrom have no postings of their own: they are inside the opening debt. paidMicros is the sum
// of the active clearing postings dated in the window — the table part of each salary row of the journal
// (above-table money never touches the debt), by the row's date; it is not the FIFO share of the window's
// days, that is what unpaidDays shows. balance, toPay, oldestUnpaid and unpaidDays are now, not the window's.
func ReadPayouts(ctx context.Context, db *sql.DB, from, to string, now time.Time) (PayoutsAnswer, error) {
	ans := PayoutsAnswer{OK: true, From: from, To: to, GeneratedAt: ts.Format(now), People: []PayoutPerson{}}
	tx, err := db.BeginTx(ctx, nil) // on the read pool: one snapshot, a rebuild committing meanwhile is not half seen
	if err != nil {
		return ans, err
	}
	defer tx.Rollback()
	if ans.AccrualFrom, err = AccrualFrom(ctx, tx); err != nil {
		return ans, err
	}
	people, err := payoutPeople(ctx, tx)
	if err != nil {
		return ans, err
	}
	byID := map[string]*PayoutPerson{}
	for i := range people {
		byID[people[i].Slug] = &people[i]
	}
	if err := payoutDays(ctx, tx, from, to, ans.AccrualFrom, byID); err != nil {
		return ans, err
	}
	if err := payoutPaid(ctx, tx, from, to, byID); err != nil {
		return ans, err
	}
	if err := payoutUnpaid(ctx, tx, byID); err != nil {
		return ans, err
	}
	if ans.Checks, err = payoutChecks(ctx, tx, now); err != nil {
		return ans, err
	}
	ans.People = people
	return ans, tx.Commit()
}

// payoutPeople reads the people with at least one payout tab and their debt from v_payables. A model's
// share and anybody without a tab (the owner among them) stay out.
func payoutPeople(ctx context.Context, q ledger.Querier) ([]PayoutPerson, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.slug, p.name, p.aliases, p.status, v.balance_micros, v.to_pay, v.oldest_unpaid,
		v.unentered_days, v.unentered_share, v.unentered_unknown
		FROM people p JOIN v_payables v ON v.person = p.slug
		WHERE EXISTS (SELECT 1 FROM payroll_tabs t WHERE t.person = p.slug)
		ORDER BY p.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PayoutPerson{}
	for rows.Next() {
		p := PayoutPerson{Days: []PayoutDay{}, UnpaidDays: []UnpaidDay{}}
		var aliases string
		var toPay sql.NullFloat64
		var oldest sql.NullString
		var share float64
		if err := rows.Scan(&p.Slug, &p.Name, &aliases, &p.Status, &p.BalanceMicros, &toPay, &oldest,
			&p.Unentered.Days, &share, &p.Unentered.Unknown); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(aliases), &p.Aliases); err != nil {
			return nil, fmt.Errorf("people %s: aliases: %w", p.Slug, err)
		}
		if p.Aliases == nil {
			p.Aliases = []string{}
		}
		if toPay.Valid {
			v := int64(money.FromFloat(toPay.Float64))
			p.ToPayMicros = &v
		}
		if oldest.Valid {
			p.OldestUnpaidDate = &oldest.String
		}
		p.Unentered.ShareMicros = int64(money.FromFloat(share))
		out = append(out, p)
	}
	return out, rows.Err()
}

// sheetDay is a row of payroll_days.
type sheetDay struct {
	person string
	amount money.Micros
	worked bool
	pages  []PayoutPage
}

// sheetPages reads payroll_days.pages: the cells that hold a number, by page.
func sheetPages(pages string) ([]PayoutPage, error) {
	cells := map[string]*float64{}
	if err := json.Unmarshal([]byte(pages), &cells); err != nil {
		return nil, err
	}
	var out []PayoutPage
	for page, v := range cells {
		if v != nil {
			out = append(out, PayoutPage{Page: page, RevenueMicros: int64(money.FromFloat(*v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out, nil
}

func payoutDays(ctx context.Context, q ledger.Querier, from, to, accrualFrom string, people map[string]*PayoutPerson) error {
	rows, err := q.QueryContext(ctx, `SELECT model, name, person, date, amount, worked, pages FROM payroll_days
		WHERE person IS NOT NULL AND date >= ? AND date <= ?`, from, to)
	if err != nil {
		return err
	}
	sheet := map[string]*sheetDay{} // "<model>/<name>:<date>", as in an accrual's source_key
	for rows.Next() {
		var model, name, date, pages string
		var d sheetDay
		var amount float64
		if err := rows.Scan(&model, &name, &d.person, &date, &amount, &d.worked, &pages); err != nil {
			rows.Close()
			return err
		}
		d.amount = money.FromFloat(amount)
		if d.pages, err = sheetPages(pages); err != nil {
			rows.Close()
			return fmt.Errorf("payroll_days %s/%s %s: pages: %w", model, name, date, err)
		}
		sheet[model+"/"+name+":"+date] = &d
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = q.QueryContext(ctx, `SELECT substr(l.account, 9), COALESCE(e.source_key, ''), l.amount
		FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE e.kind = ? AND l.account LIKE 'payable:%' AND substr(e.effective_at, 1, 10) BETWEEN ? AND ?
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		ORDER BY e.id`, KindAccrual, from, to)
	if err != nil {
		return err
	}
	posted := map[string]bool{}
	for rows.Next() {
		var slug, key string
		var amt int64
		if err := rows.Scan(&slug, &key, &amt); err != nil {
			rows.Close()
			return err
		}
		day, _, ok := parseAccrualKey(key)
		model, _, date, ok2 := splitDayKey(day)
		if !ok || !ok2 {
			rows.Close()
			return fmt.Errorf("начисление %s: непонятный source_key %q", slug, key)
		}
		posted[day] = true
		p := people[slug]
		if p == nil {
			continue
		}
		d := PayoutDay{Date: date, Model: model, AmountMicros: -amt, Source: "sheet"}
		if s := sheet[day]; s != nil {
			d.Worked, d.Pages = s.worked, s.pages
		}
		p.Days = append(p.Days, d)
		p.AccruedMicros += d.AmountMicros
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// A worked day that came to zero has no posting, and is still a day of work.
	for day, s := range sheet {
		p := people[s.person]
		if p == nil || posted[day] || !s.worked || s.amount != 0 {
			continue
		}
		model, _, date, ok := splitDayKey(day)
		if !ok || date < accrualFrom {
			continue
		}
		p.Days = append(p.Days, PayoutDay{Date: date, Model: model, Worked: true, Pages: s.pages, Source: "sheet"})
	}
	for _, p := range people {
		sort.SliceStable(p.Days, func(i, j int) bool {
			a, b := p.Days[i], p.Days[j]
			if a.Date != b.Date {
				return a.Date < b.Date
			}
			return a.Model < b.Model
		})
	}
	return nil
}

func payoutPaid(ctx context.Context, q ledger.Querier, from, to string, people map[string]*PayoutPerson) error {
	rows, err := q.QueryContext(ctx, `SELECT substr(l.account, 9), SUM(l.amount)
		FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE e.kind = ? AND l.account LIKE 'payable:%' AND substr(e.effective_at, 1, 10) BETWEEN ? AND ?
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		GROUP BY l.account`, KindClearing, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var sum int64
		if err := rows.Scan(&slug, &sum); err != nil {
			return err
		}
		if p := people[slug]; p != nil {
			p.PaidMicros = sum
		}
	}
	return rows.Err()
}

func payoutUnpaid(ctx context.Context, q ledger.Querier, people map[string]*PayoutPerson) error {
	rows, err := q.QueryContext(ctx, `SELECT person, kind, date, model, unpaid FROM payable_days WHERE unpaid > 0 ORDER BY person, seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var u UnpaidDay
		if err := rows.Scan(&slug, &u.Kind, &u.Date, &u.Model, &u.UnpaidMicros); err != nil {
			return err
		}
		if p := people[slug]; p != nil {
			p.UnpaidDays = append(p.UnpaidDays, u)
		}
	}
	return rows.Err()
}

// payoutChecks: the last contact of the sheets collector (as the checks count it) and the last rebuild of
// the debts (the cursor every Rebuild writes).
func payoutChecks(ctx context.Context, q ledger.Querier, now time.Time) (PayoutChecks, error) {
	var c PayoutChecks
	var sheets, rebuilt sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT MAX(at) FROM (SELECT MAX(received_at) AS at FROM events WHERE source = ?
		UNION ALL SELECT value FROM cursors WHERE name = ?)`, SourceSheets, events.ContactCursor(SourceSheets)).Scan(&sheets); err != nil {
		return c, err
	}
	err := q.QueryRowContext(ctx, `SELECT updated_at FROM cursors WHERE name = ?`, docsCursor).Scan(&rebuilt)
	if err != nil && err != sql.ErrNoRows {
		return c, err
	}
	old := func(at sql.NullString, limit time.Duration) bool {
		if !at.Valid || strings.TrimSpace(at.String) == "" {
			return true
		}
		t, err := ts.Parse(at.String)
		return err != nil || now.Sub(t) > limit
	}
	c.Stale = old(sheets, staleSheets) || old(rebuilt, staleRebuild)
	if sheets.Valid {
		c.LastSheetsCollectAt = &sheets.String
	}
	if rebuilt.Valid {
		c.LastRebuildAt = &rebuilt.String
	}
	return c, nil
}
