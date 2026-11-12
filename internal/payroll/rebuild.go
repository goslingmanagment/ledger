package payroll

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Kinds of the postings and the journal category they follow.
const (
	KindAccrual  = "accrual:payroll"
	KindClearing = "clearing:payroll"
	KindOpening  = "opening:payroll"
	author       = "payroll"
	salaryRow    = "Чаттер зарплата"
	suspense     = "suspense:accrual"
	salary       = "expense:chatter_salary"
	docsCursor   = "payroll:documents"
)

// Report says what a rebuild changed.
type Report struct {
	Accruals  int `json:"accruals"`
	Clearings int `json:"clearings"`
	Reversals int `json:"reversals"`
	Unentered int `json:"unentered"`
}

// Changed reports whether any posting was written.
func (r Report) Changed() bool { return r.Accruals+r.Clearings+r.Reversals > 0 }

// AccrualFrom is the first day accounted by accrual (settings.accrual_from), as a date.
func AccrualFrom(ctx context.Context, q ledger.Querier) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'accrual_from'`).Scan(&v)
	if err == sql.ErrNoRows || (err == nil && strings.TrimSpace(v) == "") {
		return "", fmt.Errorf("нет настройки accrual_from: ledger import business")
	}
	if err != nil {
		return "", err
	}
	t, err := ts.Parse(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("настройка accrual_from: %w", err)
	}
	return ts.Date(t), nil
}

// Refresh brings the shadows the sources after the sheets read up to date before a rebuild (the accruals from the
// reports: their terms are the tabs' and move with the sheets). Set by internal/accruals; nil — nothing to refresh.
var Refresh func(ctx context.Context, store *db.Store, now time.Time) error

// Rebuild brings the books in line with what is known: who each tab belongs to, the accruals (from the sheets, and
// from Hub's facts from settings.payroll_facts_from on, or from the shift reports from settings.payroll_reports_from
// on), the clearing of payouts, the FIFO of days, then «не
// занесено». Only what differs is posted, the way the journal does it: a changed day or payout is reversed and posted
// anew, nothing is edited. It is idempotent and runs after every batch of sheet events, every read of the facts and
// every hour.
func Rebuild(ctx context.Context, store *db.Store, now time.Time) (Report, error) {
	var rep Report
	// From payroll_terms_hub_from the registry's newcomers have their people rows before anything reads people.
	if err := projectPeople(ctx, store, now); err != nil {
		return rep, err
	}
	if Refresh != nil {
		if err := Refresh(ctx, store, now); err != nil {
			return rep, fmt.Errorf("начисления по отчётам: %w", err)
		}
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	from, err := AccrualFrom(ctx, tx)
	if err != nil {
		return rep, err
	}
	res, err := NewResolver(ctx, tx)
	if err != nil {
		return rep, err
	}
	if err := assignPeople(ctx, tx, res); err != nil {
		return rep, err
	}
	plan, err := planAfter(ctx, tx, now)
	if err != nil {
		return rep, err
	}
	rp, guarded := plan.(*reportPlan)
	if guarded {
		// Under the reports a paid day does not change by itself: the payouts are cleared and laid over the days as
		// posted first, so that a day a payout reached is known before any day is changed.
		if err := reconcileClearings(ctx, tx, from, res, now, &rep); err != nil {
			return rep, err
		}
		if err := rebuildFIFO(ctx, tx); err != nil {
			return rep, err
		}
		if err := rp.loadGuard(ctx, tx); err != nil {
			return rep, err
		}
	}
	if err := reconcileAccruals(ctx, tx, from, plan, now, &rep); err != nil {
		return rep, err
	}
	if !guarded {
		if err := reconcileClearings(ctx, tx, from, res, now, &rep); err != nil {
			return rep, err
		}
		// Off the reports nothing waits for a decision any more: the sheets (or the facts) accrue the days.
		if err := closePending(ctx, tx, "", stateDismissed, "источник выключен", now); err != nil {
			return rep, err
		}
	}
	if err := rebuildFIFO(ctx, tx); err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	n, err := refreshUnentered(ctx, store, now)
	rep.Unentered = n
	return rep, err
}

// assignPeople writes the owner of each tab; a name that resolves to nobody leaves NULL.
func assignPeople(ctx context.Context, tx *sql.Tx, res *Resolver) error {
	rows, err := tx.QueryContext(ctx, `SELECT model, name, COALESCE(person, '') FROM payroll_tabs`)
	if err != nil {
		return err
	}
	type tab struct{ model, name, person string }
	var tabs []tab
	for rows.Next() {
		var t tab
		if err := rows.Scan(&t.model, &t.name, &t.person); err != nil {
			rows.Close()
			return err
		}
		tabs = append(tabs, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, t := range tabs {
		slug, err := res.Resolve(t.model, t.name)
		if err != nil {
			slug = ""
		}
		var person any
		if slug != "" {
			person = slug
		}
		if _, err := tx.ExecContext(ctx, `UPDATE payroll_tabs SET person = ? WHERE model = ? AND name = ?`, person, t.model, t.name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE payroll_days SET person = ? WHERE model = ? AND name = ? AND person IS NOT ?`,
			person, t.model, t.name, person); err != nil {
			return err
		}
	}
	return nil
}

// accrualKey is the source_key of an accrual: one tab's day, with a counter because a day changed
// back to an earlier amount needs a new entry, while a source_key can be used once. The counter is the day's
// whichever the source; a day accrued from Hub's facts ends in ":facts".
func accrualKey(model, name, date string, n int, source string) string {
	return fmt.Sprintf("%s:%s/%s:%s#%s", KindAccrual, model, name, date, formatCounter(n, source))
}

// parseAccrualKey returns "<model>/<name>:<date>", the counter and the source (sheet, facts or reports).
func parseAccrualKey(key string) (day string, n int, source string, ok bool) {
	day, ok = strings.CutPrefix(key, KindAccrual+":")
	if !ok {
		return "", 0, "", false
	}
	i := strings.LastIndexByte(day, '#')
	if i < 0 {
		return "", 0, "", false
	}
	counter, source := day[i+1:], SourceSheet
	if c, isFacts := strings.CutSuffix(counter, sourceSuffix); isFacts {
		counter, source = c, SourceFacts
	} else if c, isReports := strings.CutSuffix(counter, reportsSuffix); isReports {
		counter, source = c, SourceReports
	}
	n, err := strconv.Atoi(counter)
	return day[:i], n, source, err == nil
}

// splitDayKey cuts "<model>/<name>:<date>" apart.
func splitDayKey(day string) (model, name, date string, ok bool) {
	if len(day) < 12 || day[len(day)-11] != ':' {
		return "", "", "", false
	}
	date = day[len(day)-10:]
	model, name, ok = strings.Cut(day[:len(day)-11], "/")
	return model, name, date, ok
}

type activeEntry struct {
	id      int64
	date    string
	account string
	source  string
	amount  money.Micros
}

// reconcileAccruals posts one entry per person, tab and day with money since accrual_from: the sheet's day before the
// switch to a source after the sheets (plan nil — no switch), the source's from it. A day from the switch on that the
// source cannot answer for is left as it is posted (plan.holds); a sheet's value there is a mirror, never posted. Under
// the reports a paid day whose amount would change waits for the owner's decision instead (reportPlan.guard).
func reconcileAccruals(ctx context.Context, tx *sql.Tx, from string, plan afterPlan, now time.Time, rep *Report) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.effective_at, COALESCE(e.source_key, ''), l.account, l.amount,
		NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id) AS active
		FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE e.kind = ? AND l.account LIKE 'payable:%' ORDER BY e.id`, KindAccrual)
	if err != nil {
		return err
	}
	have := map[string][]activeEntry{}
	last := map[string]int{}
	for rows.Next() {
		var a activeEntry
		var key string
		var amt int64
		var on bool
		if err := rows.Scan(&a.id, &a.date, &key, &a.account, &amt, &on); err != nil {
			rows.Close()
			return err
		}
		day, n, source, ok := parseAccrualKey(key)
		if !ok {
			rows.Close()
			return fmt.Errorf("начисление %d: непонятный source_key %q", a.id, key)
		}
		a.amount, a.source = money.Micros(amt), source
		if n > last[day] {
			last[day] = n
		}
		if on {
			have[day] = append(have[day], a)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	type wanted struct {
		model, name, person, date string
		amount                    money.Micros
		event                     int64
		at                        time.Time
		source                    string
		basis                     *reportBasis
	}
	rows, err = tx.QueryContext(ctx, `SELECT d.model, d.name, d.person, d.date, d.amount, t.ok_event_id, t.ok_at, t.posted_event_id
		FROM payroll_days d JOIN payroll_tabs t ON t.model = d.model AND t.name = d.name
		WHERE d.person IS NOT NULL AND d.date >= ?`, from)
	if err != nil {
		return err
	}
	want := map[string]wanted{}
	for rows.Next() {
		var w wanted
		var amount float64
		var okAt sql.NullString
		var okEvent, posted sql.NullInt64
		if err := rows.Scan(&w.model, &w.name, &w.person, &w.date, &amount, &okEvent, &okAt, &posted); err != nil {
			rows.Close()
			return err
		}
		w.amount, w.event, w.at, w.source = money.FromFloat(amount), okEvent.Int64, now, SourceSheet
		// Recorded at the event's time only while that reading is news: a fix of who the tab belongs to
		// is a correction of today and must not slip into a month closed before it.
		if okAt.Valid && posted.Int64 != okEvent.Int64 {
			if t, err := ts.Parse(okAt.String); err == nil {
				w.at = t
			}
		}
		if w.amount != 0 && (plan == nil || w.date < plan.start()) {
			want[w.model+"/"+w.name+":"+w.date] = w
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if plan != nil {
		for day, f := range plan.wanted() {
			if f.amount != 0 && f.date >= from {
				want[day] = wanted{model: f.model, name: f.name, person: f.person, date: f.date, amount: f.amount, at: now,
					source: plan.source(), basis: f.basis}
			}
		}
	}

	days := make([]string, 0, len(want)+len(have))
	for d := range want {
		days = append(days, d)
	}
	for d := range have {
		if _, ok := want[d]; !ok {
			days = append(days, d)
		}
	}
	sort.Strings(days)
	rp, guarded := plan.(*reportPlan)
	for _, day := range days {
		if plan != nil {
			if model, name, date, ok := splitDayKey(day); ok && date >= plan.start() && plan.holds(model, name, date) {
				continue // the source does not answer for it yet: as posted, and in «не занесено» as unknown
			}
		}
		w, ok := want[day]
		hs := have[day]
		if guarded {
			if model, name, date, kok := splitDayKey(day); kok && date >= rp.from {
				var posted money.Micros
				var ids []int64
				var sources []string
				for _, h := range hs {
					posted -= h.amount
					ids = append(ids, h.id)
					sources = append(sources, h.source)
				}
				person := w.person
				if !ok && len(hs) > 0 {
					person = strings.TrimPrefix(hs[0].account, "payable:")
				}
				next := money.Micros(0)
				var basis *reportBasis
				if ok {
					next, basis = w.amount, w.basis
				}
				wait, err := rp.guard(ctx, tx, guardDay{person: person, model: model, name: name, date: date, posted: posted,
					next: next, basis: basis, entries: ids, sources: sources}, now)
				if err != nil {
					return err
				}
				if wait {
					continue // paid: the change waits for the owner in payroll_pending_changes
				}
			}
		}
		account := "payable:" + w.person
		if ok && len(hs) == 1 && hs[0].account == account && hs[0].amount == -w.amount && hs[0].date == w.date && hs[0].source == w.source {
			continue
		}
		at := now
		if ok {
			at = w.at
		}
		for _, h := range hs {
			if _, err := ledger.Reverse(ctx, tx, h.id, ledger.Reversal{Author: author, Memo: "сторно начисления: " + day + " изменился"}, at); err != nil {
				return err
			}
			rep.Reversals++
		}
		if !ok {
			continue
		}
		last[day]++
		memo := fmt.Sprintf("начисление %s/%s за %s", w.model, w.name, w.date)
		var events []int64
		switch w.source {
		case SourceFacts:
			memo += " из фактов хаба"
		case SourceReports:
			memo += " из отчёта смены"
		default:
			events = []int64{w.event}
		}
		id, err := ledger.Post(ctx, tx, ledger.Entry{EffectiveAt: w.date, Kind: KindAccrual, Memo: memo, Author: author,
			SourceKey: accrualKey(w.model, w.name, w.date, last[day], w.source), EventIDs: events,
			Lines: []ledger.Line{{Account: salary, Amount: w.amount}, {Account: account, Amount: -w.amount}}}, at)
		if err != nil {
			return fmt.Errorf("%s: %w", day, err)
		}
		if w.basis != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO report_accrual_entries(entry_id, net_micros, rate, fix_micros) VALUES (?, ?, ?, ?)`,
				id, int64(w.basis.net), w.basis.rate, int64(w.basis.fix)); err != nil {
				return err
			}
		}
		rep.Accruals++
	}
	_, err = tx.ExecContext(ctx, `UPDATE payroll_tabs SET posted_event_id = ok_event_id WHERE posted_event_id IS NOT ok_event_id`)
	return err
}

// Unresolved is a payout row the debts could not take in: its money stays on suspense:accrual.
type Unresolved struct {
	Num    int    `json:"num"`
	Date   string `json:"date"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Amount string `json:"amount"`
	Reason string `json:"reason"`
}

// payoutRow is the part of a journal row the clearing needs.
type payoutRow struct {
	doc    ledger.Document
	person string
	table  money.Micros
}

// payoutRows reads the salary rows that count: paid, since the accrual date, with a table part. A row
// without a person is returned apart.
func payoutRows(ctx context.Context, q ledger.Querier, from string, res *Resolver) ([]payoutRow, []Unresolved, error) {
	docs, err := ledger.DocumentsAsOf(ctx, q, "")
	if err != nil {
		return nil, nil, err
	}
	var rows []payoutRow
	var bad []Unresolved
	for _, d := range docs {
		if d.Type != "expense" || d.Category != salaryRow || d.Status != "paid" || d.Date < from {
			continue
		}
		u := Unresolved{Num: d.Num, Date: d.Date, Name: d.Counterparty, Model: d.Model, Amount: d.Amount}
		amount, err := d.Micros()
		if err != nil {
			u.Reason = err.Error()
			bad = append(bad, u)
			continue
		}
		t, err := journal.ParseTags(d.Note)
		if err != nil {
			u.Reason = err.Error()
			bad = append(bad, u)
			continue
		}
		table := amount
		if t.AboveTable != nil {
			table -= money.FromFloat(t.AboveTable.Amount)
		}
		if table == 0 {
			continue
		}
		slug, err := res.Resolve(d.Model, d.Counterparty)
		if err != nil {
			u.Reason = err.Error()
			bad = append(bad, u)
			continue
		}
		rows = append(rows, payoutRow{doc: d, person: slug, table: table})
	}
	return rows, bad, nil
}

func clearingEntry(r payoutRow) ledger.Entry {
	return ledger.Entry{EffectiveAt: r.doc.Date, Kind: KindClearing, DocumentID: r.doc.ID, Author: author,
		Memo:  fmt.Sprintf("#%d выплата по таблице: %s", r.doc.Num, r.doc.Counterparty),
		Lines: []ledger.Line{{Account: "payable:" + r.person, Amount: r.table}, {Account: suspense, Amount: -r.table}}}
}

// reconcileClearings moves the table part of each payout from suspense:accrual to the person's debt, by
// journal row: the entry stays while its date, person and amount stay, a new version of the row with
// other values is reversed and posted anew. A row that resolves to nobody stays in suspense; the report
// of those comes from Check.
func reconcileClearings(ctx context.Context, tx *sql.Tx, from string, res *Resolver, now time.Time, rep *Report) error {
	rows, _, err := payoutRows(ctx, tx, from, res)
	if err != nil {
		return err
	}
	seen, err := cursor(ctx, tx)
	if err != nil {
		return err
	}
	q, err := tx.QueryContext(ctx, `SELECT d.num, e.id, e.effective_at, l.account, l.amount
		FROM entries e JOIN documents d ON d.id = e.document_id JOIN lines l ON l.entry_id = e.id
		WHERE e.kind = ? AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id) ORDER BY e.id, l.id`, KindClearing)
	if err != nil {
		return err
	}
	type posted struct {
		id    int64
		date  string
		lines []ledger.Line
	}
	have := map[int][]*posted{}
	for q.Next() {
		var num int
		var id, amt int64
		var eff, account string
		if err := q.Scan(&num, &id, &eff, &account, &amt); err != nil {
			q.Close()
			return err
		}
		list := have[num]
		if len(list) == 0 || list[len(list)-1].id != id {
			list = append(list, &posted{id: id, date: eff})
			have[num] = list
		}
		p := list[len(list)-1]
		p.lines = append(p.lines, ledger.Line{Account: account, Amount: money.Micros(amt)})
	}
	if err := q.Err(); err != nil {
		q.Close()
		return err
	}
	q.Close()

	want := map[int]payoutRow{}
	for _, r := range rows {
		want[r.doc.Num] = r
	}
	nums := make([]int, 0, len(want)+len(have))
	for n := range want {
		nums = append(nums, n)
	}
	for n := range have {
		if _, ok := want[n]; !ok {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	var maxDoc int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM documents`).Scan(&maxDoc); err != nil {
		return err
	}
	for _, num := range nums {
		r, ok := want[num]
		list := have[num]
		var e ledger.Entry
		if ok {
			e = clearingEntry(r)
			if len(list) == 1 && journal.Signature(ledger.Entry{EffectiveAt: list[0].date, Kind: KindClearing, Lines: list[0].lines}) == journal.Signature(e) {
				continue
			}
		}
		// A new version of the row is recorded when the row was; anything else is a correction of today.
		at := now
		doc, err := ledger.CurrentDocument(ctx, tx, num)
		if err != nil {
			return fmt.Errorf("#%d: %w", num, err)
		}
		if doc.ID > seen {
			if t, err := ts.Parse(doc.RecordedAt); err == nil {
				at = t
			}
		}
		for _, p := range list {
			if _, err := ledger.Reverse(ctx, tx, p.id, ledger.Reversal{Author: author, Memo: fmt.Sprintf("сторно клиринга #%d: строка журнала изменилась", num)}, at); err != nil {
				return err
			}
			rep.Reversals++
		}
		if ok {
			if _, err := ledger.Post(ctx, tx, e, at); err != nil {
				return fmt.Errorf("#%d: %w", num, err)
			}
			rep.Clearings++
		}
	}
	return setCursor(ctx, tx, maxDoc, now)
}

func cursor(ctx context.Context, q ledger.Querier) (int64, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, docsCursor).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func setCursor(ctx context.Context, q ledger.Querier, v int64, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, docsCursor, strconv.FormatInt(v, 10), ts.Format(now))
	return err
}

// debt is something a person is owed, payment something that covers debts.
type debt struct {
	kind, date, model, name string
	source                  string // of a day: sheet, facts or reports
	entry                   int64
	amount, paid            money.Micros
	paidBy                  []int
}

type payment struct {
	date   string
	num    int
	entry  int64
	amount money.Micros
}

// fifo applies the payments, oldest first, to the debts, oldest first: the opening, then the days by
// date across the person's tabs. What the payments exceed all debts by is the advance.
func fifo(debts []debt, pays []payment) ([]debt, money.Micros, []int, string) {
	rank := func(d debt) int {
		if d.kind == "opening" {
			return 0
		}
		return 1
	}
	sort.SliceStable(debts, func(i, j int) bool {
		a, b := debts[i], debts[j]
		switch {
		case rank(a) != rank(b):
			return rank(a) < rank(b)
		case a.date != b.date:
			return a.date < b.date
		case a.model != b.model:
			return a.model < b.model
		case a.name != b.name:
			return a.name < b.name
		}
		return a.entry < b.entry
	})
	sort.SliceStable(pays, func(i, j int) bool {
		a, b := pays[i], pays[j]
		switch {
		case a.date != b.date:
			return a.date < b.date
		case a.num != b.num:
			return a.num < b.num
		}
		return a.entry < b.entry
	})
	cur := 0
	var advance money.Micros
	var advanceBy []int
	lastDate := ""
	add := func(list []int, n int) []int {
		if n > 0 && (len(list) == 0 || list[len(list)-1] != n) {
			return append(list, n)
		}
		return list
	}
	for _, p := range pays {
		left := p.amount
		lastDate = p.date
		for left > 0 && cur < len(debts) {
			d := &debts[cur]
			cover := min(left, d.amount-d.paid)
			d.paid += cover
			d.paidBy = add(d.paidBy, p.num)
			left -= cover
			if d.paid == d.amount {
				cur++
			}
		}
		if left > 0 {
			advance += left
			advanceBy = add(advanceBy, p.num)
		}
	}
	return debts, advance, advanceBy, lastDate
}

// rebuildFIFO writes payable_days from the active postings on the debts of people. Other accounts in the
// payable group (a model's share of a deal) have no days.
func rebuildFIFO(ctx context.Context, tx *sql.Tx) error {
	people, err := LoadPeople(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.kind, e.effective_at, COALESCE(e.source_key, ''), COALESCE(d.num, 0), l.account, l.amount
		FROM lines l JOIN entries e ON e.id = l.entry_id LEFT JOIN documents d ON d.id = e.document_id
		WHERE l.account LIKE 'payable:%' AND e.reverses_id IS NULL
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`)
	if err != nil {
		return err
	}
	debts := map[string][]debt{}
	pays := map[string][]payment{}
	for rows.Next() {
		var id, amt int64
		var kind, eff, key, account string
		var num int
		if err := rows.Scan(&id, &kind, &eff, &key, &num, &account, &amt); err != nil {
			rows.Close()
			return err
		}
		slug := strings.TrimPrefix(account, "payable:")
		if _, ok := people[slug]; !ok {
			continue
		}
		date := eff[:10]
		if amt > 0 { // a debit: a payout, or a day that took money back
			pays[slug] = append(pays[slug], payment{date: date, num: num, entry: id, amount: money.Micros(amt)})
			continue
		}
		d := debt{kind: "other", date: date, entry: id, amount: money.Micros(-amt)}
		switch kind {
		case KindOpening:
			d.kind = "opening"
		case KindAccrual:
			if day, _, source, ok := parseAccrualKey(key); ok {
				if model, name, _, ok := splitDayKey(day); ok {
					d.kind, d.model, d.name, d.source = "day", model, name, source
				}
			}
		}
		debts[slug] = append(debts[slug], d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM payable_days`); err != nil {
		return err
	}
	slugs := make([]string, 0, len(debts)+len(pays))
	for s := range debts {
		slugs = append(slugs, s)
	}
	for s := range pays {
		if _, ok := debts[s]; !ok {
			slugs = append(slugs, s)
		}
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		list, advance, advanceBy, lastDate := fifo(debts[slug], pays[slug])
		seq := 0
		insert := func(kind, date, model, name, source string, amount, paid, unpaid money.Micros, by []int) error {
			seq++
			if by == nil {
				by = []int{}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO payable_days(person, seq, kind, date, model, name, amount, paid, unpaid, paid_by, source)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, slug, seq, kind, date, model, name, int64(amount), int64(paid), int64(unpaid), jsonText(by), source)
			return err
		}
		for _, d := range list {
			if err := insert(d.kind, d.date, d.model, d.name, d.source, d.amount, d.paid, d.amount-d.paid, d.paidBy); err != nil {
				return err
			}
		}
		if advance > 0 {
			if err := insert("advance", lastDate, "", "", "", 0, advance, -advance, advanceBy); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- opening ----

// Opening is one person's line of `ledger payroll open`.
type Opening struct {
	Person  string       `json:"person"`
	Name    string       `json:"name"`
	KV      float64      `json:"kv"`
	Accrued money.Micros `json:"accrued"`
	Paid    money.Micros `json:"paid"`
	Before  float64      `json:"before"` // every day of the person's tabs before the accrual date
	Amount  money.Micros `json:"amount"`
	Flags   []string     `json:"flags,omitempty"`
	Frozen  bool         `json:"frozen,omitempty"` // posted earlier: Amount is what stands
	Skip    string       `json:"skip,omitempty"`   // why it cannot be computed now
}

// PlanOpenings computes each person's opening: К выплате of all the person's tabs now, minus what the
// books owe since the accrual date (accrued − table part of payouts). That is what stayed unpaid from
// before the date. Openings already frozen come back as they stand.
func PlanOpenings(ctx context.Context, q ledger.Querier) ([]Opening, string, error) {
	from, err := AccrualFrom(ctx, q)
	if err != nil {
		return nil, "", err
	}
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, "", err
	}
	plan := map[string]*Opening{}
	get := func(slug string) *Opening {
		o := plan[slug]
		if o == nil {
			o = &Opening{Person: slug, Name: people[slug].Name}
			plan[slug] = o
		}
		return o
	}
	rows, err := q.QueryContext(ctx, `SELECT person, COALESCE(SUM(kv), 0), SUM(status <> 'ok'), COUNT(*)
		FROM payroll_tabs WHERE person IS NOT NULL GROUP BY person`)
	if err != nil {
		return nil, "", err
	}
	hasTabs := map[string]bool{}
	for rows.Next() {
		var slug string
		var kv float64
		var bad, n int
		if err := rows.Scan(&slug, &kv, &bad, &n); err != nil {
			rows.Close()
			return nil, "", err
		}
		o := get(slug)
		o.KV, hasTabs[slug] = kv, n > 0
		if bad > 0 {
			o.Skip = fmt.Sprintf("вкладок с ошибкой: %d — долг не посчитать", bad)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT e.kind, substr(l.account, 9), SUM(l.amount)
		FROM lines l JOIN entries e ON e.id = l.entry_id
		WHERE e.kind IN (?, ?) AND l.account LIKE 'payable:%' AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		GROUP BY e.kind, l.account`, KindAccrual, KindClearing)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var kind, slug string
		var sum int64
		if err := rows.Scan(&kind, &slug, &sum); err != nil {
			rows.Close()
			return nil, "", err
		}
		if _, ok := people[slug]; !ok {
			continue
		}
		o := get(slug)
		if kind == KindAccrual {
			o.Accrued = money.Micros(-sum)
		} else {
			o.Paid = money.Micros(sum)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT person, SUM(amount) FROM payroll_days WHERE person IS NOT NULL AND date < ? GROUP BY person`, from)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var slug string
		var sum float64
		if err := rows.Scan(&slug, &sum); err != nil {
			rows.Close()
			return nil, "", err
		}
		get(slug).Before = sum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	rows.Close()
	frozen := map[string]money.Micros{}
	rows, err = q.QueryContext(ctx, `SELECT person, amount FROM payroll_openings`)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var slug string
		var amt int64
		if err := rows.Scan(&slug, &amt); err != nil {
			rows.Close()
			return nil, "", err
		}
		frozen[slug] = money.Micros(amt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	rows.Close()
	out := make([]Opening, 0, len(plan))
	for slug, o := range plan {
		o.Amount = money.FromFloat(o.KV) - (o.Accrued - o.Paid)
		if abs := max(o.Amount, -o.Amount); abs < 10_000 { // below a cent is the noise of rounding days to micros
			o.Amount = 0
		}
		if v, ok := frozen[slug]; ok {
			o.Amount, o.Frozen, o.Skip = v, true, ""
		}
		switch {
		case o.Amount < 0:
			o.Flags = append(o.Flags, "отрицательный: выплачено больше, чем заработано")
		case !hasTabs[slug] && o.Amount != 0:
			o.Flags = append(o.Flags, "нет вкладки: сумма только из выплат журнала, ничем не проверена")
		case o.Amount > money.FromFloat(o.Before)+money.Scale:
			o.Flags = append(o.Flags, fmt.Sprintf("больше всех дней вкладок до %s: %.2f", from, o.Before))
		}
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Person < out[j].Person })
	return out, from, nil
}

// OpeningDate is the day before the accrual date.
func OpeningDate(from string) (string, error) {
	t, err := time.Parse(ts.DateLayout, from)
	if err != nil {
		return "", err
	}
	return t.AddDate(0, 0, -1).Format(ts.DateLayout), nil
}

// PostOpenings freezes every computable opening not yet frozen: Cr payable:<slug>, Dr opening, with a
// source_key per person. A zero is frozen too, as a row without an entry.
func PostOpenings(ctx context.Context, tx *sql.Tx, plan []Opening, from string, now time.Time) (int, error) {
	date, err := OpeningDate(from)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range plan {
		if o.Frozen || o.Skip != "" {
			continue
		}
		var entry any
		if o.Amount != 0 {
			id, err := ledger.Post(ctx, tx, ledger.Entry{EffectiveAt: date, Kind: KindOpening, Author: author,
				Memo:      "неоплаченные дни до " + from + ": " + o.Name,
				SourceKey: KindOpening + ":" + o.Person,
				Lines:     []ledger.Line{{Account: "opening", Amount: o.Amount}, {Account: "payable:" + o.Person, Amount: -o.Amount}}}, now)
			if err != nil {
				return n, fmt.Errorf("%s: %w", o.Person, err)
			}
			entry = id
		}
		flags := o.Flags
		if flags == nil {
			flags = []string{}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO payroll_openings(person, amount, kv, accrued, paid, flags, entry_id, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, o.Person, int64(o.Amount), o.KV, int64(o.Accrued), int64(o.Paid), jsonText(flags), entry, ts.Format(now)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
