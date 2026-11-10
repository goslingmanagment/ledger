package accruals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The accruals from the shift reports (R2, plan docs/plans/2026-11-10-shift-reports.md): a person's day on a model is the
// sum of what they posted for the model's pages on the report date (v_report_pages, posted_micros), at the terms of their
// tab of the model — payroll.Terms, as the sheet and the facts count: «Ставка %» of the revenue, and «Фикс/см» once for a
// day with a posted page in shift. A person without a tab of the model, a page whose model is unknown, a tab without a
// rate are «без ставки»: listed with the revenue, not accrued. The person's pot shares of the purchases made that day in
// pots of the model (v_pot_shares, R3) are revenue of the day beside the pages (pot_micros, pots): the rate takes the sum,
// and a day with a share only — the person did not work it — gets no fix. report_accruals is rebuilt after every parse of the posts,
// every save of a draft and before every rebuild of the debts; before settings.payroll_reports_from it is a shadow.

func init() {
	payroll.Refresh = func(ctx context.Context, store *db.Store, now time.Time) error {
		_, err := RebuildReports(ctx, store, now)
		return err
	}
	reports.AfterChange = func(ctx context.Context, store *db.Store, now time.Time) error {
		from, err := payroll.ReportsFrom(ctx, store.RO)
		if err != nil {
			return err
		}
		if from == "" {
			_, err = RebuildReports(ctx, store, now)
			return err
		}
		// The reports accrue: the debts follow (payroll.Rebuild refreshes report_accruals first).
		if _, err := payroll.Rebuild(ctx, store, now); err != nil {
			return fmt.Errorf("долги после отчётов: %w", err)
		}
		return nil
	}
}

// ReportsReport says what a rebuild of report_accruals did.
type ReportsReport struct {
	Days      int `json:"days"`      // person × model × date rows
	Accrued   int `json:"accrued"`   // rows with an amount
	Unpriced  int `json:"unpriced"`  // «без ставки»
	Written   int `json:"written"`   // rows inserted or changed
	Removed   int `json:"removed"`   // rows gone
	Unchanged int `json:"unchanged"` // rows left as they were
}

// reportPage is a page of a report day.
type reportPage struct {
	Page   string `json:"page"`
	Micros int64  `json:"micros"`
}

// potPart is a person's share of a day's purchases in one pot: revenue of the tab like a page.
type potPart struct {
	Pot    string `json:"pot"`
	Name   string `json:"name"`
	Micros int64  `json:"micros"`
}

type reportDay struct {
	person, model, day string
	pages              []reportPage
	posts              []int64
	pots               []potPart
	potNet             int64
	net                int64 // the pages and the pot shares
	tabModel, tabName  *string
	rate               *float64
	fix                int64
	amount             *int64
	reason             string
}

func (d reportDay) args() []any {
	pages, _ := json.Marshal(d.pages)
	posts, _ := json.Marshal(d.posts)
	pots := []byte("[]")
	if len(d.pots) > 0 {
		pots, _ = json.Marshal(d.pots)
	}
	var tm, tn, rate, amount any
	if d.tabModel != nil {
		tm, tn = *d.tabModel, *d.tabName
	}
	if d.rate != nil {
		rate = *d.rate
	}
	if d.amount != nil {
		amount = *d.amount
	}
	return []any{d.person, d.model, d.day, weekOf(d.day), string(pages), string(posts), d.net, tm, tn, rate, d.fix, amount, nullStr(d.reason),
		d.potNet, string(pots)}
}

// RebuildReports recomputes report_accruals from the pay base and writes only the rows that changed. Idempotent; it
// posts nothing.
func RebuildReports(ctx context.Context, store *db.Store, now time.Time) (ReportsReport, error) {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return ReportsReport{}, err
	}
	defer tx.Rollback()
	rep, err := rebuildReportsTx(ctx, tx, now)
	if err != nil {
		return rep, err
	}
	return rep, tx.Commit()
}

// reportDays computes the rows of report_accruals from the pay base, without writing them.
func reportDays(ctx context.Context, q ledger.Querier) ([]reportDay, error) {
	who, err := newPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT person, report_date, page, model, posted_micros, post_id FROM v_report_pages
		WHERE posted_micros IS NOT NULL ORDER BY person, report_date, model, page`)
	if err != nil {
		return nil, err
	}
	byKey := map[[3]string]*reportDay{}
	var order [][3]string
	for rows.Next() {
		var person, day, page, model string
		var micros int64
		var post sql.NullInt64
		if err := rows.Scan(&person, &day, &page, &model, &micros, &post); err != nil {
			rows.Close()
			return nil, err
		}
		k := [3]string{person, model, day}
		d := byKey[k]
		if d == nil {
			d = &reportDay{person: person, model: model, day: day, pages: []reportPage{}, posts: []int64{}}
			byKey[k] = d
			order = append(order, k)
		}
		d.pages = append(d.pages, reportPage{Page: page, Micros: micros})
		d.net += micros
		if post.Valid && (len(d.posts) == 0 || d.posts[len(d.posts)-1] != post.Int64) {
			d.posts = append(d.posts, post.Int64)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The pot shares of the purchases of the day, in pots of the model: revenue of the person's tab beside the pages.
	rows, err = q.QueryContext(ctx, `SELECT person, model, day, pot, pot_name, SUM(share_micros) FROM v_pot_shares
		WHERE person IS NOT NULL GROUP BY person, model, day, pot ORDER BY person, day, model, pot`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var person, model, day string
		var part potPart
		if err := rows.Scan(&person, &model, &day, &part.Pot, &part.Name, &part.Micros); err != nil {
			rows.Close()
			return nil, err
		}
		k := [3]string{person, model, day}
		d := byKey[k]
		if d == nil {
			d = &reportDay{person: person, model: model, day: day, pages: []reportPage{}, posts: []int64{}}
			byKey[k] = d
			order = append(order, k)
		}
		d.pots = append(d.pots, part)
		d.potNet += part.Micros
		d.net += part.Micros
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		if a[2] != b[2] {
			return a[2] < b[2]
		}
		return a[1] < b[1]
	})
	out := make([]reportDay, 0, len(order))
	for _, k := range order {
		d := byKey[k]
		sort.Slice(d.posts, func(i, j int) bool { return d.posts[i] < d.posts[j] })
		price(d, who)
		out = append(out, *d)
	}
	return out, nil
}

// price puts the terms of the person's tab of the model on the day: revenue × rate, plus the fix once.
func price(d *reportDay, who *people) {
	if d.model == "" {
		pages := ""
		for i, p := range d.pages {
			if i > 0 {
				pages += ", "
			}
			pages += p.Page
		}
		d.reason = "модель страницы неизвестна (" + pages + "): нет в hub_pages — ставки нет"
		return
	}
	t, reason := who.terms(d.person, d.model)
	if reason != "" {
		d.reason = reason
		return
	}
	d.tabModel, d.tabName = &t.Model, &t.Name
	// The fix is the shift's: a day with only a pot share (the person did not work it) gets none.
	if len(d.pages) > 0 {
		d.fix = int64(money.FromFloat(t.ShiftFix))
	}
	if t.Rate == nil {
		d.reason = "в шапке вкладки " + t.Model + "/" + t.Name + " нет «Ставка %»"
		return
	}
	r := *t.Rate
	d.rate = &r
	// As the sheet counts a day: dollars of the day's revenue × rate, to micros, then the fix of the shift.
	v := int64(money.FromFloat(float64(float64(d.net)/money.Scale)*r)) + d.fix
	d.amount = &v
}

func rebuildReportsTx(ctx context.Context, tx *sql.Tx, now time.Time) (ReportsReport, error) {
	var rep ReportsReport
	list, err := reportDays(ctx, tx)
	if err != nil {
		return rep, err
	}
	stored := map[[3]string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT person, model, day, week, pages, posts, net_micros, tab_model, tab_name, rate, fix_micros,
		amount_micros, reason, pot_micros, pots FROM report_accruals`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		vals := make([]any, 15)
		ptrs := make([]any, 15)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return rep, err
		}
		stored[[3]string{fmt.Sprint(vals[0]), fmt.Sprint(vals[1]), fmt.Sprint(vals[2])}] = signature(vals)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	at := ts.Format(now)
	seen := map[[3]string]bool{}
	for _, d := range list {
		rep.Days++
		if d.amount != nil {
			rep.Accrued++
		} else {
			rep.Unpriced++
		}
		k := [3]string{d.person, d.model, d.day}
		seen[k] = true
		args := d.args()
		if old, ok := stored[k]; ok && old == signature(args) {
			rep.Unchanged++
			continue
		}
		rep.Written++
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_accruals(person, model, day, week, pages, posts, net_micros, tab_model, tab_name,
			rate, fix_micros, amount_micros, reason, pot_micros, pots, computed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(person, model, day) DO UPDATE SET week = excluded.week, pages = excluded.pages, posts = excluded.posts,
			net_micros = excluded.net_micros, tab_model = excluded.tab_model, tab_name = excluded.tab_name, rate = excluded.rate,
			fix_micros = excluded.fix_micros, amount_micros = excluded.amount_micros, reason = excluded.reason,
			pot_micros = excluded.pot_micros, pots = excluded.pots, computed_at = excluded.computed_at`, append(args, at)...); err != nil {
			return rep, err
		}
	}
	for k := range stored {
		if seen[k] {
			continue
		}
		rep.Removed++
		if _, err := tx.ExecContext(ctx, `DELETE FROM report_accruals WHERE person = ? AND model = ? AND day = ?`, k[0], k[1], k[2]); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// signature renders a row's values the same way whether read back or about to be written.
func signature(vals []any) string {
	out := ""
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			out += "∅"
		case []byte:
			out += string(x)
		case float64:
			out += fmt.Sprintf("%.12g", x)
		case int:
			out += fmt.Sprint(int64(x))
		default:
			out += fmt.Sprint(x)
		}
		out += "\x00"
	}
	return out
}
