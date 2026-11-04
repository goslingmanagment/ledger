package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Bundle is the history of ledger.md: for each commit the rows that are new or changed, and the final
// text (for the template). A plain file import is a bundle with one version.
type Bundle struct {
	Source   string    `json:"source"`
	Versions []Version `json:"versions"`
	Final    string    `json:"final"`
	FinalAt  string    `json:"final_at"`
	FinalRef string    `json:"final_ref"`
}

// Version is one state of the file.
type Version struct {
	Ref  string     `json:"ref"` // commit or "file"
	At   string     `json:"at"`  // MSK timestamp: recorded_at of the rows it changes
	Rows [][]string `json:"rows"`
}

// Align sets an account's balance to an observed value; the difference goes to `opening` (e.g. Bybit
// to the cash snapshot of the first reconciliation). The snapshot point is the end of Date or, with
// UpTo, the moment after journal row UpTo among the rows of that date (rows of earlier dates count
// whatever their number: a backfill row of May is in a snapshot of August).
type Align struct {
	Account string
	Date    string
	UpTo    int
	Amount  money.Micros
}

// Options of an import.
type Options struct {
	AccrualFrom string
	Models      []string // settings.models
	Align       []Align
	Author      string
}

// Report says what the import did.
type Report struct {
	Versions     int      `json:"versions"`
	Skipped      int      `json:"skippedVersions"`
	NewDocuments int      `json:"newDocuments"`
	NewVersions  int      `json:"newVersions"`
	Reposted     int      `json:"repostedDocuments"`
	Entries      int      `json:"entries"`
	Reversals    int      `json:"reversals"`
	Rows         int      `json:"rows"`
	Warnings     []string `json:"warnings,omitempty"`
	Alignments   []string `json:"alignments,omitempty"`
	Template     bool     `json:"templateChanged"`
}

// ProblemsError lists every row that stopped the import.
type ProblemsError struct{ Problems []string }

func (e ProblemsError) Error() string {
	return fmt.Sprintf("импорт остановлен, строк с ошибками: %d\n  %s", len(e.Problems), strings.Join(e.Problems, "\n  "))
}

func rowFromCells(c []string) (Row, error) {
	if len(c) != 11 {
		return Row{}, fmt.Errorf("строка из %d ячеек", len(c))
	}
	var n int
	if _, err := fmt.Sscanf(c[0], "%d", &n); err != nil || n <= 0 {
		return Row{}, fmt.Errorf("номер «%s»", c[0])
	}
	return Row{Num: n, Date: c[1], Type: c[2], Category: c[3], Counterparty: c[4], Model: c[5], Amount: c[6],
		Currency: c[7], Method: c[8], Status: c[9], Note: c[10]}, nil
}

func docFromRow(r Row, tags json.RawMessage, at, origin, author string) ledger.Document {
	return ledger.Document{Num: r.Num, Date: r.Date, Type: r.Type, Category: r.Category, Counterparty: r.Counterparty,
		Model: r.Model, Amount: r.Amount, Currency: r.Currency, Method: r.Method, Status: r.Status, Note: r.Note,
		Tags: tags, Incomplete: r.Date < fullLedger, RecordedAt: at, Author: author, Origin: origin}
}

func rowFromDoc(d ledger.Document) Row {
	return Row{Num: d.Num, Date: d.Date, Type: d.Type, Category: d.Category, Counterparty: d.Counterparty, Model: d.Model,
		Amount: d.Amount, Currency: d.Currency, Method: d.Method, Status: d.Status, Note: d.Note}
}

type active struct {
	ids  []int64
	sigs []string
}

// Import applies the bundle in one transaction: new row versions, entries reposted where the mapping
// result changed (reversal of what was posted, then the new entries), the template and alignments.
// The final state is checked strictly; any problem rolls everything back.
func Import(ctx context.Context, tx *sql.Tx, b Bundle, o Options) (Report, error) {
	var rep Report
	if o.Author == "" {
		o.Author = "ledger_import"
	}
	docs := map[int]ledger.Document{}
	cur, err := ledger.DocumentsAsOf(ctx, tx, "")
	if err != nil {
		return rep, err
	}
	for _, d := range cur {
		docs[d.Num] = d
	}
	posted, err := loadActive(ctx, tx)
	if err != nil {
		return rep, err
	}
	overrides, err := loadOverrides(ctx, tx)
	if err != nil {
		return rep, err
	}
	lastAt := time.Time{}
	for _, v := range b.Versions {
		at, err := ts.Parse(v.At)
		if err != nil {
			return rep, fmt.Errorf("версия %s: %w", v.Ref, err)
		}
		if v.Ref != "file" && v.Ref != "worktree" {
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM journal_imports WHERE ref = ?`, v.Ref).Scan(&one)
			if err == nil {
				rep.Skipped++
				continue
			}
			if err != sql.ErrNoRows {
				return rep, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO journal_imports(ref, at, imported_at) VALUES (?, ?, ?)`,
				v.Ref, ts.Format(at), ts.Format(time.Now())); err != nil {
				return rep, err
			}
		}
		lastAt = at
		changed := false
		for _, cells := range v.Rows {
			r, err := rowFromCells(cells)
			if err != nil {
				return rep, fmt.Errorf("версия %s: %w", v.Ref, err)
			}
			tags := json.RawMessage(`{}`)
			if t, err := RowTags(r); err == nil {
				tags = t.JSON()
			} else {
				msg, _ := json.Marshal(map[string]string{"error": err.Error()})
				tags = msg
			}
			prev, existed := docs[r.Num]
			d, isNew, err := ledger.PutDocument(ctx, tx, docFromRow(r, tags, ts.Format(at), "ledger.md@"+v.Ref, o.Author), at)
			if err != nil {
				return rep, err
			}
			if !isNew {
				continue
			}
			changed = true
			if existed && prev.Num == r.Num {
				rep.NewVersions++
			} else {
				rep.NewDocuments++
			}
			docs[r.Num] = d
			if _, _, err := ledger.AddEvent(ctx, tx, ledger.Event{Source: "ledger_import",
				SourceID: fmt.Sprintf("ledger.md#%d.v%d", d.Num, d.Version), Kind: "journal_row",
				OccurredAt: ts.Format(at), Payload: map[string]any{"ref": v.Ref, "cells": cells}}, at); err != nil {
				return rep, err
			}
		}
		rep.Versions++
		if !changed {
			continue
		}
		if _, err := reconcile(ctx, tx, docs, posted, overrides, o, at, false, &rep); err != nil {
			return rep, err
		}
	}
	// The final state is reconciled strictly once more: a split or a setting may have changed even
	// when no row did. Postings of this pass are recorded at the newest applied version (or now).
	if lastAt.IsZero() {
		lastAt = time.Now()
	}
	problems, err := reconcile(ctx, tx, docs, posted, overrides, o, lastAt, true, &rep)
	if err != nil {
		return rep, err
	}
	if len(problems) > 0 {
		return rep, ProblemsError{problems}
	}
	rows := make([]Row, 0, len(docs))
	for _, d := range docs {
		rows = append(rows, rowFromDoc(d))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Num < rows[j].Num })
	rep.Rows = len(rows)
	if b.Final != "" {
		f, errs := Parse(b.Final)
		for _, e := range errs {
			problems = append(problems, e.Error())
		}
		inFile := map[int]bool{}
		for _, r := range f.Rows {
			inFile[r.Num] = true
			if d, ok := docs[r.Num]; !ok || !rowFromDoc(d).sameCells(r) {
				problems = append(problems, fmt.Sprintf("#%d: строка файла не совпадает с базой после импорта", r.Num))
			}
		}
		for n := range docs {
			if !inFile[n] && strings.HasPrefix(docs[n].Origin, "ledger.md@") {
				problems = append(problems, fmt.Sprintf("#%d: строка пропала из файла", n))
			}
		}
		changedTpl, err := saveTemplate(ctx, tx, strings.Join(f.Lines, "\n"), "ledger.md@"+b.FinalRef, b.FinalAt)
		if err != nil {
			return rep, err
		}
		rep.Template = changedTpl
	}
	for _, e := range Lint(rows) {
		problems = append(problems, e.Error())
	}
	if len(problems) > 0 {
		return rep, ProblemsError{problems}
	}
	for _, a := range o.Align {
		msg, err := align(ctx, tx, a, o.Author)
		if err != nil {
			return rep, err
		}
		rep.Alignments = append(rep.Alignments, msg)
	}
	return rep, nil
}

func (r Row) sameCells(o Row) bool {
	a, b := r.Cells(), o.Cells()
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reconcile(ctx context.Context, tx *sql.Tx, docs map[int]ledger.Document, posted map[int]*active,
	overrides map[int]Override, o Options, at time.Time, final bool, rep *Report) ([]string, error) {
	rows := make([]Row, 0, len(docs))
	for _, d := range docs {
		rows = append(rows, rowFromDoc(d))
	}
	c := &Context{AccrualFrom: o.AccrualFrom, FirstOfDeal: map[string]int{}, Overrides: overrides, Models: map[string]bool{}}
	for _, m := range o.Models {
		c.Models[m] = true
	}
	tagsOf := map[int]Tags{}
	var problems []string
	for _, r := range SortedByDate(rows) {
		t, err := RowTags(r)
		if err != nil {
			if final {
				problems = append(problems, err.Error())
			} else {
				rep.Warnings = append(rep.Warnings, err.Error())
			}
		}
		tagsOf[r.Num] = t
		if t.Deal != nil && r.Type == "income" && !notOff[r.Category] {
			if _, ok := c.FirstOfDeal[t.Deal.ID]; !ok {
				c.FirstOfDeal[t.Deal.ID] = r.Num
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Num < rows[j].Num })
	for _, r := range rows {
		want, err := Entries(r, tagsOf[r.Num], c)
		if err != nil {
			if final {
				problems = append(problems, fmt.Sprintf("#%d: %v", r.Num, err))
			} else {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("#%d (история): %v", r.Num, err))
			}
			continue
		}
		var sigs []string
		for _, e := range want {
			sigs = append(sigs, Signature(e))
		}
		have := posted[r.Num]
		if have != nil && sameSet(have.sigs, sigs) {
			continue
		}
		if have != nil {
			for _, id := range have.ids {
				if _, err := ledger.Reverse(ctx, tx, id, ledger.Reversal{Author: o.Author,
					Memo: fmt.Sprintf("сторно #%d: строка журнала изменилась", r.Num)}, at); err != nil {
					return nil, err
				}
				rep.Reversals++
			}
			rep.Reposted++
		}
		na := &active{}
		for i, e := range want {
			e.DocumentID, e.Author = docs[r.Num].ID, o.Author
			id, err := ledger.Post(ctx, tx, e, at)
			if err != nil {
				return nil, fmt.Errorf("#%d: %w", r.Num, err)
			}
			na.ids, na.sigs = append(na.ids, id), append(na.sigs, sigs[i])
			rep.Entries++
		}
		posted[r.Num] = na
	}
	return problems, nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// loadActive reads the document entries that are not reversed, grouped by row number.
func loadActive(ctx context.Context, q ledger.Querier) (map[int]*active, error) {
	rows, err := q.QueryContext(ctx, `SELECT d.num, e.id, e.effective_at, e.kind, l.account, l.amount
		FROM entries e JOIN documents d ON d.id = e.document_id JOIN lines l ON l.entry_id = e.id
		WHERE e.kind LIKE 'document%' AND e.reverses_id IS NULL
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		ORDER BY e.id, l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		num  int
		date string
		kind string
		ls   []ledger.Line
	}
	byEntry := map[int64]*acc{}
	var order []int64
	for rows.Next() {
		var num int
		var id, amt int64
		var eff, kind, account string
		if err := rows.Scan(&num, &id, &eff, &kind, &account, &amt); err != nil {
			return nil, err
		}
		a, ok := byEntry[id]
		if !ok {
			a = &acc{num: num, date: eff, kind: kind}
			byEntry[id] = a
			order = append(order, id)
		}
		a.ls = append(a.ls, ledger.Line{Account: account, Amount: money.Micros(amt)})
	}
	out := map[int]*active{}
	for _, id := range order {
		a := byEntry[id]
		x := out[a.num]
		if x == nil {
			x = &active{}
			out[a.num] = x
		}
		x.ids = append(x.ids, id)
		x.sigs = append(x.sigs, Signature(ledger.Entry{EffectiveAt: a.date, Kind: a.kind, Lines: a.ls}))
	}
	return out, rows.Err()
}

func loadOverrides(ctx context.Context, q ledger.Querier) (map[int]Override, error) {
	rows, err := q.QueryContext(ctx, `SELECT num, data FROM journal_overrides`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]Override{}
	for rows.Next() {
		var n int
		var data string
		if err := rows.Scan(&n, &data); err != nil {
			return nil, err
		}
		var o Override
		if err := json.Unmarshal([]byte(data), &o); err != nil {
			return nil, fmt.Errorf("разбивка #%d: %w", n, err)
		}
		out[n] = o
	}
	return out, rows.Err()
}

// PutOverrides replaces the money splits from one source file.
func PutOverrides(ctx context.Context, tx *sql.Tx, items map[int]Override, source string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM journal_overrides WHERE source = ?`, source); err != nil {
		return err
	}
	for n, o := range items {
		data, _ := json.Marshal(o)
		if _, err := tx.ExecContext(ctx, `INSERT INTO journal_overrides(num, data, source, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(num) DO UPDATE SET data = excluded.data, source = excluded.source, created_at = excluded.created_at`,
			n, string(data), source, ts.Format(now)); err != nil {
			return err
		}
	}
	return nil
}

func saveTemplate(ctx context.Context, tx *sql.Tx, body, origin, at string) (bool, error) {
	var last string
	err := tx.QueryRowContext(ctx, `SELECT body FROM journal_templates ORDER BY id DESC LIMIT 1`).Scan(&last)
	if err == nil && last == body {
		return false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if at == "" {
		at = ts.Format(time.Now())
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_templates(created_at, origin, body) VALUES (?, ?, ?)`, at, origin, body)
	return err == nil, err
}

func align(ctx context.Context, tx *sql.Tx, a Align, author string) (string, error) {
	key := fmt.Sprintf("выравнивание %s к снимку %s", a.Account, a.Date)
	if a.UpTo > 0 {
		key += fmt.Sprintf(" после #%d", a.UpTo)
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id, l.amount FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE e.kind = 'opening:align' AND e.memo LIKE ? AND l.account = ? AND e.reverses_id IS NULL
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)`, key+" =%", a.Account)
	if err != nil {
		return "", err
	}
	var ids []int64
	var current money.Micros
	for rows.Next() {
		var id, v int64
		rows.Scan(&id, &v)
		ids, current = append(ids, id), current+money.Micros(v)
	}
	rows.Close()
	var bal int64
	if a.UpTo > 0 {
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(l.amount), 0) FROM lines l JOIN entries e ON e.id = l.entry_id
			LEFT JOIN documents d ON d.id = e.document_id
			WHERE l.account = ? AND e.kind <> 'opening:align'
			  AND (substr(e.effective_at, 1, 10) < ? OR (substr(e.effective_at, 1, 10) = ? AND d.num IS NOT NULL AND d.num <= ?))`,
			a.Account, a.Date, a.Date, a.UpTo).Scan(&bal)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(l.amount), 0) FROM lines l JOIN entries e ON e.id = l.entry_id
			WHERE l.account = ? AND e.kind <> 'opening:align' AND substr(e.effective_at, 1, 10) <= ?`,
			a.Account, a.Date).Scan(&bal)
	}
	if err != nil {
		return "", err
	}
	want := a.Amount - money.Micros(bal)
	if want == current && len(ids) <= 1 {
		return fmt.Sprintf("%s: %s, без изменений", key, want), nil
	}
	now := time.Now()
	for _, id := range ids {
		if _, err := ledger.Reverse(ctx, tx, id, ledger.Reversal{Author: author, Memo: "сторно: " + key}, now); err != nil {
			return "", err
		}
	}
	if want != 0 {
		if _, err := ledger.Post(ctx, tx, ledger.Entry{EffectiveAt: a.Date, Kind: "opening:align",
			Memo: fmt.Sprintf("%s = %s", key, a.Amount), Author: author, Lines: []ledger.Line{
				{Account: a.Account, Amount: want}, {Account: "opening", Amount: -want}}}, now); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s: %s (по журналу было %s)", key, want, money.Micros(bal)), nil
}

// Export renders ledger.md from the latest template and the current row versions. Rows the template
// does not know (written by ledger itself) follow the last row of the file.
func Export(ctx context.Context, q ledger.Querier) (string, error) {
	var body string
	if err := q.QueryRowContext(ctx, `SELECT body FROM journal_templates ORDER BY id DESC LIMIT 1`).Scan(&body); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("шаблона ledger.md нет: сначала импорт")
		}
		return "", err
	}
	docs, err := ledger.DocumentsAsOf(ctx, q, "")
	if err != nil {
		return "", err
	}
	byNum := map[int]Row{}
	for _, d := range docs {
		byNum[d.Num] = rowFromDoc(d)
	}
	lines := strings.Split(body, "\n")
	inTpl := map[int]bool{}
	for _, l := range lines {
		if n := PlaceholderNum(l); n > 0 {
			if _, ok := byNum[n]; !ok {
				return "", fmt.Errorf("в шаблоне #%d, а строки в базе нет", n)
			}
			inTpl[n] = true
		}
	}
	var extra []Row
	for _, d := range docs {
		if !inTpl[d.Num] {
			extra = append(extra, byNum[d.Num])
		}
	}
	return Render(lines, byNum, extra), nil
}

// OptionsFromSettings reads the accrual date and the model names.
func OptionsFromSettings(ctx context.Context, q ledger.Querier) (Options, error) {
	var o Options
	if err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'accrual_from'`).Scan(&o.AccrualFrom); err != nil {
		return o, fmt.Errorf("нет settings.accrual_from: ledger import business")
	}
	var models string
	_ = q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'models'`).Scan(&models)
	for _, m := range strings.Split(models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			o.Models = append(o.Models, m)
		}
	}
	return o, nil
}

// ParseAlign builds an alignment from its text parts.
func ParseAlign(account, date string, upTo int, amount string) (Align, error) {
	v, err := money.Parse(amount)
	if err != nil {
		return Align{}, fmt.Errorf("выравнивание %s: %w", account, err)
	}
	if !dateRe.MatchString(date) {
		return Align{}, fmt.Errorf("выравнивание %s: дата «%s»", account, date)
	}
	return Align{Account: account, Date: date, UpTo: upTo, Amount: v}, nil
}
