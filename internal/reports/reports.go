// Package reports keeps the chatters' shift reports (stage R1 of docs/plans/2026-11-10-shift-reports.md): the team's
// chat agent composes a report with its script at the end of a shift and saves the breakdown here — the pages and
// their amounts, every sale of the window in the total or not and why, the amounts added by hand. ledger keeps the
// versions and serves them back to Hub. Nothing here posts, accrues or changes a balance.
package reports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Statuses of a version.
const (
	StatusDraft      = "draft"
	StatusPosted     = "posted"
	StatusSuperseded = "superseded"
)

// SourceAgent is a report saved by the agent's script.
const SourceAgent = "agent"

// maxMills bounds every amount: a billion dollars, far past any shift, keeps ×1000 from overflowing.
const maxMills = 1_000_000_000_000

// Refusal is a report not saved: the request is wrong. Its message is Russian and names what is wrong.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

func refuse(format string, args ...any) *Refusal { return &Refusal{Msg: fmt.Sprintf(format, args...)} }

// Input is the body of POST /v1/reports. Amounts are integer mills (thousandths of a dollar), as the agent's script
// has them.
type Input struct {
	HubUsername string   `json:"hub_username"`
	HubUserID   *int64   `json:"hub_user_id,omitempty"`
	Chatter     string   `json:"chatter,omitempty"`
	ReportDate  string   `json:"report_date"`
	WindowFrom  string   `json:"window_from"`
	WindowTo    string   `json:"window_to"`
	CutAt       string   `json:"cut_at"`
	Source      string   `json:"source"`
	TotalMills  *int64   `json:"total_mills"`
	Pages       []PageIn `json:"pages"`
	Lines       []LineIn `json:"lines"`
	Adds        []AddIn  `json:"adds"`
}

// PageIn is one page row of the report.
type PageIn struct {
	Page        string `json:"page"`
	AmountMills *int64 `json:"amount_mills"`
}

// LineIn is one sale of the breakdown.
type LineIn struct {
	N              *int64   `json:"n,omitempty"`
	TransactionRef string   `json:"transaction_ref,omitempty"`
	Page           string   `json:"page"`
	FanRef         string   `json:"fan_ref,omitempty"`
	FanName        string   `json:"fan_name,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	OccurredAt     string   `json:"occurred_at,omitempty"`
	NetMills       *int64   `json:"net_mills"`
	GrossMills     *int64   `json:"gross_mills,omitempty"`
	Counted        bool     `json:"counted"`
	Decision       string   `json:"decision,omitempty"`
	Flags          []string `json:"flags"`
	Notes          []string `json:"notes"`
	PPVAuthor      string   `json:"ppv_author,omitempty"`
	PPVSentAt      string   `json:"ppv_sent_at,omitempty"`
}

// AddIn is an amount added by hand: outside the window or outside Hub.
type AddIn struct {
	Page        string `json:"page"`
	AmountMills *int64 `json:"amount_mills"`
	What        string `json:"what"`
}

// Decode reads a request body strictly: unknown fields and trailing data are refused.
func Decode(raw []byte) (Input, error) {
	var in Input
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Input{}, refuse("тело запроса: нужен JSON отчёта смены: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Input{}, refuse("тело запроса: после JSON отчёта лишние данные")
	}
	return in, nil
}

// normTime reads a time of the request and renders it the way the schema stores times (MSK, seconds).
func normTime(name, v string) (string, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return "", refuse("%s: нужно время RFC 3339 (2026-11-08T16:00:00+03:00), а не %q", name, v)
	}
	return ts.Format(t), nil
}

func checkMills(name string, v *int64) (int64, error) {
	if v == nil {
		return 0, refuse("%s: нужна сумма в mills (целые тысячные доллара)", name)
	}
	if *v > maxMills || *v < -maxMills {
		return 0, refuse("%s: сумма %d mills вне разумного", name, *v)
	}
	return *v, nil
}

func mills(v int64) string { return money.FromMills(v).String() }

// Normalize checks the report and brings it to its canonical form (trimmed text, times in MSK, empty lists not
// null), or says what is wrong.
func Normalize(in Input) (Input, error) {
	in.HubUsername, in.Chatter = strings.TrimSpace(in.HubUsername), strings.TrimSpace(in.Chatter)
	in.ReportDate, in.Source = strings.TrimSpace(in.ReportDate), strings.TrimSpace(in.Source)
	if in.HubUsername == "" {
		return in, refuse("нужно имя пользователя хаба (hub_username)")
	}
	if d, err := time.ParseInLocation(ts.DateLayout, in.ReportDate, ts.MSK); err != nil || d.Format(ts.DateLayout) != in.ReportDate {
		return in, refuse("report_date: нужна дата ГГГГ-ММ-ДД, а не %q", in.ReportDate)
	}
	var err error
	if in.WindowFrom, err = normTime("window_from", in.WindowFrom); err != nil {
		return in, err
	}
	if in.WindowTo, err = normTime("window_to", in.WindowTo); err != nil {
		return in, err
	}
	if in.CutAt, err = normTime("cut_at", in.CutAt); err != nil {
		return in, err
	}
	if in.WindowTo < in.WindowFrom {
		return in, refuse("окно смены %s — %s: конец раньше начала", in.WindowFrom, in.WindowTo)
	}
	if in.Source != SourceAgent {
		return in, refuse("source: сейчас бывает только %q, а не %q", SourceAgent, in.Source)
	}
	total, err := checkMills("total_mills", in.TotalMills)
	if err != nil {
		return in, err
	}
	if in.HubUserID != nil && *in.HubUserID <= 0 {
		return in, refuse("hub_user_id: нужен id пользователя хаба больше нуля, а не %d", *in.HubUserID)
	}
	if len(in.Pages) == 0 {
		return in, refuse("нужна хотя бы одна страница (pages)")
	}
	pageSum := int64(0)
	amount := map[string]int64{}
	counted := map[string]int64{}
	for i := range in.Pages {
		p := &in.Pages[i]
		p.Page = strings.TrimSpace(p.Page)
		if p.Page == "" {
			return in, refuse("страница %d: нужно имя страницы (page)", i+1)
		}
		if _, dup := amount[p.Page]; dup {
			return in, refuse("страница %s повторяется в pages", p.Page)
		}
		v, err := checkMills("страница "+p.Page+": amount_mills", p.AmountMills)
		if err != nil {
			return in, err
		}
		amount[p.Page] = v
		pageSum += v
	}
	if pageSum != total {
		return in, refuse("сумма страниц %s не равна итогу %s (total_mills %d, страницы %d mills)",
			mills(pageSum), mills(total), total, pageSum)
	}
	if in.Lines == nil {
		in.Lines = []LineIn{}
	}
	if in.Adds == nil {
		in.Adds = []AddIn{}
	}
	for i := range in.Lines {
		l := &in.Lines[i]
		name := fmt.Sprintf("строка %d", i+1)
		if l.N != nil {
			name = fmt.Sprintf("строка n=%d", *l.N)
		}
		l.Page, l.TransactionRef = strings.TrimSpace(l.Page), strings.TrimSpace(l.TransactionRef)
		l.Decision = strings.TrimSpace(l.Decision)
		if _, ok := amount[l.Page]; !ok {
			return in, refuse("%s: страницы %q нет в pages", name, l.Page)
		}
		v, err := checkMills(name+": net_mills", l.NetMills)
		if err != nil {
			return in, err
		}
		if l.GrossMills != nil {
			if _, err := checkMills(name+": gross_mills", l.GrossMills); err != nil {
				return in, err
			}
		}
		switch l.Decision {
		case "", "auto", "include", "exclude":
		default:
			return in, refuse("%s: decision бывает auto, include или exclude, а не %q", name, l.Decision)
		}
		if l.OccurredAt != "" {
			if l.OccurredAt, err = normTime(name+": occurred_at", l.OccurredAt); err != nil {
				return in, err
			}
		}
		if l.Flags == nil {
			l.Flags = []string{}
		}
		if l.Notes == nil {
			l.Notes = []string{}
		}
		if l.Counted {
			counted[l.Page] += v
		}
	}
	for i := range in.Adds {
		a := &in.Adds[i]
		a.Page, a.What = strings.TrimSpace(a.Page), strings.TrimSpace(a.What)
		name := fmt.Sprintf("добавка %d", i+1)
		if _, ok := amount[a.Page]; !ok {
			return in, refuse("%s: страницы %q нет в pages", name, a.Page)
		}
		v, err := checkMills(name+": amount_mills", a.AmountMills)
		if err != nil {
			return in, err
		}
		if a.What == "" {
			return in, refuse("%s (страница %s): нужно объяснение, что это за сумма (what)", name, a.Page)
		}
		counted[a.Page] += v
	}
	for _, p := range in.Pages {
		if got := counted[p.Page]; got != amount[p.Page] {
			return in, refuse("страница %s: включённые продажи и добавки дают %s, а у страницы %s (%d против %d mills)",
				p.Page, mills(got), mills(amount[p.Page]), got, amount[p.Page])
		}
	}
	return in, nil
}

// Hash is the hash of a normalized report's content: what the agent sent, nothing of when it came.
// The Hub name counts case aside, as the report is identified.
func Hash(in Input) string {
	in.HubUsername = strings.ToLower(in.HubUsername)
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SaveResult is the answer of POST /v1/reports.
type SaveResult struct {
	ID      int64   `json:"id"`
	Version int     `json:"version"`
	Created bool    `json:"created"`
	Person  *string `json:"person"`
}

// Save checks a report and writes it as a new draft version, superseding the current one, in one transaction. The
// same content as the current version writes nothing (Created false). raw is the request as received.
func Save(ctx context.Context, store *db.Store, raw []byte, keyName string, now time.Time) (SaveResult, error) {
	in, err := Decode(raw)
	if err != nil {
		return SaveResult{}, err
	}
	if in, err = Normalize(in); err != nil {
		return SaveResult{}, err
	}
	hash := Hash(in)
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return SaveResult{}, refuse("тело запроса: %v", err)
	}

	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return SaveResult{}, err
	}
	defer tx.Rollback()
	var cur struct {
		id      int64
		version int
		hash    string
		person  sql.NullString
	}
	err = tx.QueryRowContext(ctx, `SELECT id, version, content_hash, person FROM shift_reports
		WHERE lower(hub_username) = lower(?) AND report_date = ? ORDER BY version DESC LIMIT 1`,
		in.HubUsername, in.ReportDate).Scan(&cur.id, &cur.version, &cur.hash, &cur.person)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return SaveResult{}, err
	case cur.hash == hash:
		return SaveResult{ID: cur.id, Version: cur.version, Created: false, Person: nullable(cur.person)}, nil
	}

	userID := in.HubUserID
	if userID == nil {
		if userID, err = factUserID(ctx, tx, in.HubUsername); err != nil {
			return SaveResult{}, err
		}
	}
	res, err := payroll.NewResolver(ctx, tx)
	if err != nil {
		return SaveResult{}, err
	}
	models, err := payroll.HubPageModels(ctx, tx)
	if err != nil {
		return SaveResult{}, err
	}
	var pageModels []string
	for _, p := range in.Pages {
		pageModels = append(pageModels, models[p.Page])
	}
	person := resolve(res, pageModels, userID, in.HubUsername)

	if cur.id != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE shift_reports SET status = ? WHERE id = ?`, StatusSuperseded, cur.id); err != nil {
			return SaveResult{}, err
		}
	}
	out := SaveResult{Version: cur.version + 1, Created: true}
	if person != "" {
		out.Person = &person
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO shift_reports(person, hub_username, hub_user_id, chatter, report_date,
		window_from, window_to, cut_at, version, total_micros, source, status, content_hash, payload, created_at, key_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullStr(person), in.HubUsername, userID, in.Chatter, in.ReportDate, in.WindowFrom, in.WindowTo, in.CutAt,
		out.Version, int64(money.FromMills(*in.TotalMills)), in.Source, StatusDraft, hash, compact.String(), ts.Format(now), keyName)
	if err != nil {
		return SaveResult{}, err
	}
	if out.ID, err = r.LastInsertId(); err != nil {
		return SaveResult{}, err
	}
	for i, p := range in.Pages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO shift_report_pages(report_id, seq, page_label, amount_micros) VALUES (?, ?, ?, ?)`,
			out.ID, i+1, p.Page, int64(money.FromMills(*p.AmountMills))); err != nil {
			return SaveResult{}, err
		}
	}
	seq := 0
	insert := `INSERT INTO shift_report_lines(report_id, seq, n, transaction_ref, page_label, fan_ref, fan_name, kind,
		occurred_at, net_micros, gross_micros, counted, decision, added, what, flags, notes, ppv_author, ppv_sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, l := range in.Lines {
		seq++
		var gross any
		if l.GrossMills != nil {
			gross = int64(money.FromMills(*l.GrossMills))
		}
		if _, err := tx.ExecContext(ctx, insert, out.ID, seq, l.N, nullStr(l.TransactionRef), l.Page, l.FanRef, l.FanName,
			l.Kind, nullStr(l.OccurredAt), int64(money.FromMills(*l.NetMills)), gross, boolInt(l.Counted), l.Decision, 0, "",
			jsonList(l.Flags), jsonList(l.Notes), l.PPVAuthor, l.PPVSentAt); err != nil {
			return SaveResult{}, err
		}
	}
	for _, a := range in.Adds {
		seq++
		if _, err := tx.ExecContext(ctx, insert, out.ID, seq, nil, nil, a.Page, "", "", "", nil,
			int64(money.FromMills(*a.AmountMills)), nil, 1, "", 1, a.What, "[]", "[]", "", ""); err != nil {
			return SaveResult{}, err
		}
	}
	// The chatter may have posted before the agent saved: the new version is posted at once when the post agrees.
	if _, err := SyncStatuses(ctx, tx); err != nil {
		return SaveResult{}, err
	}
	return out, tx.Commit()
}

// factUserID is the Hub id of a username in ledger's copy of Hub's team facts, case aside; nil — unknown or not one.
func factUserID(ctx context.Context, q ledger.Querier, username string) (*int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT user_id FROM hub_works WHERE lower(username) = lower(?)
		UNION SELECT hub_user_id FROM fact_homes WHERE hub_user_id IS NOT NULL AND lower(username) = lower(?)`, username, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) != 1 {
		return nil, nil
	}
	return &ids[0], nil
}

// resolve is the person of a Hub user (payroll.Resolver.HubPerson) on the report's pages' models: the one card all
// models agree on; "" — none, or several.
func resolve(res *payroll.Resolver, models []string, userID *int64, username string) string {
	var id int64
	if userID != nil {
		id = *userID
	}
	found := map[string]bool{}
	for _, m := range append([]string{""}, models...) {
		if slug, err := res.HubPerson(m, id, username); err == nil && slug != "" {
			found[slug] = true
		}
	}
	if len(found) != 1 {
		return ""
	}
	for slug := range found {
		return slug
	}
	return ""
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonList(xs []string) string {
	if xs == nil {
		xs = []string{}
	}
	b, _ := json.Marshal(xs)
	return string(b)
}
