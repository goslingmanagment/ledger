// Package decisions keeps the agreements about whose a sale is (stage R3 of docs/plans/2026-11-10-shift-reports.md):
// the owner's rulings and the chatters' own agreements — a colleague's PPV bought in my shift, a subscription someone
// else arranged, a sale split in two, money a fan paid outside the platform. The team's chat agent writes each as a
// record (it used to be a paragraph in its memory file) and reads them back before composing a report; `ledger decision
// list --md` renders the open ones in the shape of that file, so the file becomes a mirror. Nothing here posts or
// accrues: a decision is applied by the report that uses it.
package decisions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Kinds of a decision.
const (
	KindOwner       = "owner"        // the whole sale is one person's
	KindSplit       = "split"        // shares
	KindPot         = "pot"          // goes to a pot
	KindOffPlatform = "off_platform" // money outside the platform, taken into a person's report by their word
	KindExclude     = "exclude"      // counts for nobody
)

// Statuses of a decision.
const (
	StatusOpen      = "open"
	StatusApplied   = "applied"
	StatusCancelled = "cancelled"
)

// MaxNote bounds the reasoning, in characters.
const MaxNote = 4000

// maxMills bounds an amount: a billion dollars.
const maxMills = 1_000_000_000_000

// Refusal is a request not done: it is wrong. Its message is Russian and names what is wrong.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

func refuse(format string, args ...any) *Refusal { return &Refusal{Msg: fmt.Sprintf(format, args...)} }

// ErrNotFound is a decision id with no row.
var ErrNotFound = errors.New("no such decision")

// Share is a person's part of a sale, in whole percent.
type Share struct {
	Person  string `json:"person"`
	Percent int    `json:"percent"`
}

// Input is the body of POST /v1/decisions. The amount is integer mills (thousandths of a dollar), as the agent's
// script has them.
type Input struct {
	PageLabel      string  `json:"page_label"`
	FanRef         string  `json:"fan_ref,omitempty"`
	FanName        string  `json:"fan_name,omitempty"`
	TransactionRef string  `json:"transaction_ref,omitempty"`
	NetMills       *int64  `json:"net_mills,omitempty"`
	OccurredAt     string  `json:"occurred_at,omitempty"`
	Kind           string  `json:"kind"`
	Shares         []Share `json:"shares,omitempty"`
	Pot            string  `json:"pot,omitempty"`
	SaidBy         string  `json:"said_by"`
	SaidAt         string  `json:"said_at"`
	SourceLink     string  `json:"source_link,omitempty"`
	Note           string  `json:"note,omitempty"`
	Supersedes     *int64  `json:"supersedes,omitempty"`
	Answers        *int64  `json:"answers,omitempty"` // the open question this decision answers (sale_questions.id)
}

// Decision is a stored decision as the API serves it.
type Decision struct {
	ID                int64   `json:"id"`
	PageLabel         string  `json:"page_label"`
	FanRef            string  `json:"fan_ref"`
	FanName           string  `json:"fan_name"`
	TransactionRef    *string `json:"transaction_ref"`
	NetMicros         *int64  `json:"net_micros"`
	OccurredAt        *string `json:"occurred_at"`
	Kind              string  `json:"kind"`
	Shares            []Share `json:"shares"`
	Pot               *string `json:"pot"`
	SaidBy            string  `json:"said_by"`
	SaidAt            string  `json:"said_at"`
	SourceLink        string  `json:"source_link"`
	Note              string  `json:"note"`
	Status            string  `json:"status"`
	AppliedReportDate *string `json:"applied_report_date"`
	AppliedPerson     *string `json:"applied_person"`
	AppliedAt         *string `json:"applied_at"`
	CancelledAt       *string `json:"cancelled_at"`
	CancelNote        *string `json:"cancel_note"`
	Supersedes        *int64  `json:"supersedes"`
	CreatedAt         string  `json:"created_at"`
	KeyName           string  `json:"key_name"`
}

// Decode reads a body strictly: unknown fields and trailing data are refused.
func Decode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse("тело запроса: нужен JSON: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse("тело запроса: после JSON лишние данные")
	}
	return nil
}

// NormTime reads an RFC 3339 time and renders it the way the schema stores times (MSK).
func NormTime(name, v string) (string, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return "", refuse("%s: нужно время RFC 3339 (2026-11-08T16:00:00+03:00), а не %q", name, v)
	}
	return ts.Format(t), nil
}

// NormDate checks a ГГГГ-ММ-ДД date.
func NormDate(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if d, err := time.ParseInLocation(ts.DateLayout, v, ts.MSK); err != nil || d.Format(ts.DateLayout) != v {
		return "", refuse("%s: нужна дата ГГГГ-ММ-ДД, а не %q", name, v)
	}
	return v, nil
}

// NormDateOrTime reads a date or an RFC 3339 time: the date as is, the time in MSK.
func NormDateOrTime(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) == len(ts.DateLayout) {
		return NormDate(name, v)
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return "", refuse("%s: нужна дата ГГГГ-ММ-ДД или время RFC 3339, а не %q", name, v)
	}
	return NormTime(name, v)
}

// CheckMills reads an amount in mills into micros.
func CheckMills(name string, v int64) (int64, error) {
	if v > maxMills || v < -maxMills {
		return 0, refuse("%s: сумма %d mills вне разумного", name, v)
	}
	return int64(money.FromMills(v)), nil
}

// PersonExists says whether a slug names a card of people.
func PersonExists(ctx context.Context, q ledger.Querier, slug string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM people WHERE slug = ?`, slug).Scan(&n)
	return n > 0, err
}

// normalize checks a decision against the rules of its kind and the people and pots of the book.
func normalize(ctx context.Context, q ledger.Querier, in Input) (Input, error) {
	in.PageLabel, in.FanRef, in.FanName = strings.TrimSpace(in.PageLabel), strings.TrimSpace(in.FanRef), strings.TrimSpace(in.FanName)
	in.TransactionRef, in.Kind, in.Pot = strings.TrimSpace(in.TransactionRef), strings.TrimSpace(in.Kind), strings.TrimSpace(in.Pot)
	in.SaidBy, in.SourceLink, in.Note = strings.TrimSpace(in.SaidBy), strings.TrimSpace(in.SourceLink), strings.TrimSpace(in.Note)
	if in.PageLabel == "" {
		return in, refuse("нужна страница хаба (page_label)")
	}
	if in.FanRef == "" && in.FanName == "" {
		return in, refuse("нужен фан: fan_ref или fan_name")
	}
	if in.NetMills != nil {
		if _, err := CheckMills("net_mills", *in.NetMills); err != nil {
			return in, err
		}
	}
	var err error
	if in.OccurredAt != "" {
		if in.OccurredAt, err = NormTime("occurred_at", in.OccurredAt); err != nil {
			return in, err
		}
	}
	if in.SaidBy == "" {
		return in, refuse("нужно, кто решил (said_by): владелец или какие чаттеры")
	}
	if strings.TrimSpace(in.SaidAt) == "" {
		return in, refuse("нужно, когда сказано (said_at): ГГГГ-ММ-ДД или время")
	}
	if in.SaidAt, err = NormDateOrTime("said_at", in.SaidAt); err != nil {
		return in, err
	}
	if n := utf8.RuneCountInString(in.Note); n > MaxNote {
		return in, refuse("note: %d знаков, можно до %d", n, MaxNote)
	}
	if in.Supersedes != nil && *in.Supersedes <= 0 {
		return in, refuse("supersedes: нужен номер решения больше нуля")
	}
	if in.Answers != nil && *in.Answers <= 0 {
		return in, refuse("answers: нужен номер вопроса больше нуля")
	}
	switch in.Kind {
	case KindOwner, KindSplit, KindOffPlatform:
		if in.Pot != "" {
			return in, refuse("pot бывает только у kind pot, а здесь %s", in.Kind)
		}
		if err := checkShares(ctx, q, in.Kind, in.Shares); err != nil {
			return in, err
		}
	case KindPot:
		if len(in.Shares) > 0 {
			return in, refuse("у kind pot долей нет (shares): доли считает общак")
		}
		if in.Pot == "" {
			return in, refuse("у kind pot нужен общак (pot)")
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pots WHERE slug = ?`, in.Pot).Scan(&n); err != nil {
			return in, err
		}
		if n == 0 {
			return in, refuse("общака %q нет: его объявляет владелец (ledger pot declare)", in.Pot)
		}
	case KindExclude:
		if len(in.Shares) > 0 || in.Pot != "" {
			return in, refuse("у kind exclude нет ни долей, ни общака: продажа не считается никому")
		}
	default:
		return in, refuse("kind бывает owner, split, pot, off_platform или exclude, а не %q", in.Kind)
	}
	if in.Shares == nil {
		in.Shares = []Share{}
	}
	return in, nil
}

func checkShares(ctx context.Context, q ledger.Querier, kind string, shares []Share) error {
	if len(shares) == 0 {
		return refuse("у kind %s нужны доли (shares): [{\"person\": код, \"percent\": целое}]", kind)
	}
	if kind == KindOwner && len(shares) != 1 {
		return refuse("у kind owner одна доля — вся продажа одного человека, а их %d", len(shares))
	}
	if kind == KindSplit && len(shares) < 2 {
		return refuse("у kind split нужно хотя бы две доли; продажа одного человека — kind owner")
	}
	seen := map[string]bool{}
	sum := 0
	for i := range shares {
		s := &shares[i]
		s.Person = strings.TrimSpace(s.Person)
		if s.Person == "" {
			return refuse("доля %d: нужен код человека (person)", i+1)
		}
		if seen[s.Person] {
			return refuse("человек %s в долях дважды", s.Person)
		}
		seen[s.Person] = true
		if s.Percent < 1 || s.Percent > 100 {
			return refuse("доля %s: процент от 1 до 100, а не %d", s.Person, s.Percent)
		}
		ok, err := PersonExists(ctx, q, s.Person)
		if err != nil {
			return err
		}
		if !ok {
			return refuse("человека %q нет в people", s.Person)
		}
		sum += s.Percent
	}
	if sum != 100 {
		return refuse("доли в сумме %d%%, а нужно 100%%", sum)
	}
	return nil
}

// Add checks a decision and writes it; a decision that supersedes another cancels it in the same transaction. A
// decision on a sale that already has an open decision must name it in supersedes. A decision that answers a question
// closes that open question in the same transaction; a missing or closed question refuses the whole decision.
func Add(ctx context.Context, store *db.Store, in Input, keyName string, now time.Time) (Decision, error) {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	if in, err = normalize(ctx, tx, in); err != nil {
		return Decision{}, err
	}
	at := ts.Format(now)
	if in.TransactionRef != "" {
		var open int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM sale_decisions WHERE transaction_ref = ? AND status = ?`, in.TransactionRef, StatusOpen).Scan(&open)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return Decision{}, err
		case in.Supersedes == nil || *in.Supersedes != open:
			return Decision{}, refuse("у продажи %s уже есть открытое решение %d: новое должно его заменить (supersedes: %d)", in.TransactionRef, open, open)
		}
	}
	if in.Supersedes != nil {
		old, err := get(ctx, tx, *in.Supersedes)
		if errors.Is(err, ErrNotFound) {
			return Decision{}, refuse("supersedes: решения %d нет", *in.Supersedes)
		}
		if err != nil {
			return Decision{}, err
		}
		if old.Status == StatusCancelled {
			return Decision{}, refuse("supersedes: решение %d уже отменено", old.ID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sale_decisions SET status = ?, cancelled_at = ?, cancel_note = ? WHERE id = ?`,
			StatusCancelled, at, "заменено новым решением", old.ID); err != nil {
			return Decision{}, err
		}
	}
	shares, _ := json.Marshal(in.Shares)
	var net any
	if in.NetMills != nil {
		net = int64(money.FromMills(*in.NetMills))
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO sale_decisions(page_label, fan_ref, fan_name, transaction_ref, net_micros, occurred_at, kind,
		shares, pot, said_by, said_at, source_link, note, status, supersedes, created_at, key_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.PageLabel, in.FanRef, in.FanName, nullStr(in.TransactionRef), net, nullStr(in.OccurredAt), in.Kind, string(shares),
		nullStr(in.Pot), in.SaidBy, in.SaidAt, in.SourceLink, in.Note, StatusOpen, in.Supersedes, at, keyName)
	if err != nil {
		return Decision{}, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return Decision{}, err
	}
	if in.Supersedes != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sale_decisions SET cancel_note = ? WHERE id = ?`,
			fmt.Sprintf("заменено решением %d", id), *in.Supersedes); err != nil {
			return Decision{}, err
		}
	}
	if in.Answers != nil {
		if err := closeQuestion(ctx, tx, *in.Answers, &id, "", now); err != nil {
			return Decision{}, err
		}
	}
	d, err := get(ctx, tx, id)
	if err != nil {
		return Decision{}, err
	}
	return d, tx.Commit()
}

// AppliedInput is the body of POST /v1/decisions/{id}/applied: in whose report of which date the decision was used.
type AppliedInput struct {
	Person     string `json:"person"`
	ReportDate string `json:"report_date"`
}

// Applied marks an open decision used in a person's report.
func Applied(ctx context.Context, store *db.Store, id int64, in AppliedInput, now time.Time) (Decision, error) {
	in.Person = strings.TrimSpace(in.Person)
	date, err := NormDate("report_date", in.ReportDate)
	if err != nil {
		return Decision{}, err
	}
	if in.Person == "" {
		return Decision{}, refuse("нужен человек, в чьём отчёте учтено (person)")
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	ok, err := PersonExists(ctx, tx, in.Person)
	if err != nil {
		return Decision{}, err
	}
	if !ok {
		return Decision{}, refuse("человека %q нет в people", in.Person)
	}
	d, err := get(ctx, tx, id)
	if err != nil {
		return Decision{}, err
	}
	if d.Status != StatusOpen {
		return Decision{}, refuse("решение %d не открыто (%s): учесть можно только открытое", id, d.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sale_decisions SET status = ?, applied_person = ?, applied_report_date = ?, applied_at = ? WHERE id = ?`,
		StatusApplied, in.Person, date, ts.Format(now), id); err != nil {
		return Decision{}, err
	}
	if d, err = get(ctx, tx, id); err != nil {
		return Decision{}, err
	}
	return d, tx.Commit()
}

// CancelInput is the body of POST /v1/decisions/{id}/cancel.
type CancelInput struct {
	Note string `json:"note"`
}

// Cancel cancels an open or applied decision, with a note why.
func Cancel(ctx context.Context, store *db.Store, id int64, in CancelInput, now time.Time) (Decision, error) {
	note := strings.TrimSpace(in.Note)
	if note == "" {
		return Decision{}, refuse("нужна заметка, почему отменено (note)")
	}
	if n := utf8.RuneCountInString(note); n > MaxNote {
		return Decision{}, refuse("note: %d знаков, можно до %d", n, MaxNote)
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	d, err := get(ctx, tx, id)
	if err != nil {
		return Decision{}, err
	}
	if d.Status == StatusCancelled {
		return Decision{}, refuse("решение %d уже отменено", id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sale_decisions SET status = ?, cancelled_at = ?, cancel_note = ? WHERE id = ?`,
		StatusCancelled, ts.Format(now), note, id); err != nil {
		return Decision{}, err
	}
	if d, err = get(ctx, tx, id); err != nil {
		return Decision{}, err
	}
	return d, tx.Commit()
}

const columns = `id, page_label, fan_ref, fan_name, transaction_ref, net_micros, occurred_at, kind, shares, pot, said_by, said_at,
	source_link, note, status, applied_report_date, applied_person, applied_at, cancelled_at, cancel_note, supersedes, created_at, key_name`

func scan(row interface{ Scan(...any) error }) (Decision, error) {
	var d Decision
	var ref, at, pot, ard, ap, aat, cat, cn sql.NullString
	var net, sup sql.NullInt64
	var shares string
	if err := row.Scan(&d.ID, &d.PageLabel, &d.FanRef, &d.FanName, &ref, &net, &at, &d.Kind, &shares, &pot, &d.SaidBy, &d.SaidAt,
		&d.SourceLink, &d.Note, &d.Status, &ard, &ap, &aat, &cat, &cn, &sup, &d.CreatedAt, &d.KeyName); err != nil {
		return Decision{}, err
	}
	d.TransactionRef, d.OccurredAt, d.Pot = ptr(ref), ptr(at), ptr(pot)
	d.AppliedReportDate, d.AppliedPerson, d.AppliedAt, d.CancelledAt, d.CancelNote = ptr(ard), ptr(ap), ptr(aat), ptr(cat), ptr(cn)
	if net.Valid {
		d.NetMicros = &net.Int64
	}
	if sup.Valid {
		d.Supersedes = &sup.Int64
	}
	d.Shares = []Share{}
	_ = json.Unmarshal([]byte(shares), &d.Shares)
	return d, nil
}

func get(ctx context.Context, q ledger.Querier, id int64) (Decision, error) {
	d, err := scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM sale_decisions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, ErrNotFound
	}
	return d, err
}

// Get reads one decision.
func Get(ctx context.Context, q ledger.Querier, id int64) (Decision, error) { return get(ctx, q, id) }

// Filter narrows List.
type Filter struct {
	Open     bool   // status open only
	Fan      string // fan_ref or fan_name, case aside; a substring of at least 3 characters
	Person   string // anyone in shares
	From, To string // the date of the sale, or of when it was said while the sale is unknown; inclusive
}

// MatchFan says whether a fan query matches a fan: equal to the ref or the name case aside, or — a query of three
// characters or more — inside one of them.
func MatchFan(query, ref, name string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	r, n := strings.ToLower(ref), strings.ToLower(name)
	if q == r || q == n {
		return true
	}
	return utf8.RuneCountInString(q) >= 3 && (strings.Contains(r, q) || strings.Contains(n, q))
}

// List reads the decisions by the filter, the newest last.
func List(ctx context.Context, q ledger.Querier, f Filter) ([]Decision, error) {
	where := []string{"1 = 1"}
	var args []any
	if f.Open {
		where = append(where, "status = ?")
		args = append(args, StatusOpen)
	}
	date := "COALESCE(substr(occurred_at, 1, 10), substr(said_at, 1, 10))"
	if f.From != "" {
		where = append(where, date+" >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, date+" <= ?")
		args = append(args, f.To)
	}
	if f.Person != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(shares) j WHERE json_extract(j.value, '$.person') = ?)")
		args = append(args, f.Person)
	}
	rows, err := q.QueryContext(ctx, `SELECT `+columns+` FROM sale_decisions WHERE `+strings.Join(where, " AND ")+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Decision{}
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if !MatchFan(f.Fan, d.FanRef, d.FanName) {
			continue
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Markdown renders decisions as the agent's memory file has them: one bullet per decision.
//
//   - **<fan name> / <fan ref>** (<page>). <what and how much>. <whose and why — the note>. Источник: <said_by>, <said_at>, <link>. Статус: <…>.
func Markdown(list []Decision, potNames map[string]string) string {
	var b strings.Builder
	for _, d := range list {
		fan := d.FanName
		switch {
		case fan == "":
			fan = d.FanRef
		case d.FanRef != "":
			fan += " / " + d.FanRef
		}
		fmt.Fprintf(&b, "- **%s** (%s). %s. %s", fan, d.PageLabel, what(d), whose(d, potNames))
		if d.Note != "" {
			fmt.Fprintf(&b, ": %s", strings.TrimRight(strings.Join(strings.Fields(d.Note), " "), "."))
		}
		src := []string{d.SaidBy, d.SaidAt}
		if d.SourceLink != "" {
			src = append(src, d.SourceLink)
		}
		fmt.Fprintf(&b, ". Источник: %s. Статус: %s.\n", strings.Join(src, ", "), statusText(d))
	}
	return b.String()
}

// what says the sale and how much.
func what(d Decision) string {
	var parts []string
	if d.TransactionRef != nil {
		parts = append(parts, "Продажа "+*d.TransactionRef)
	} else {
		parts = append(parts, "Продажи ещё нет")
	}
	if d.NetMicros != nil {
		parts = append(parts, "нетто "+money.Micros(*d.NetMicros).Cents())
	}
	if d.OccurredAt != nil {
		parts = append(parts, *d.OccurredAt)
	}
	return strings.Join(parts, ", ")
}

// whose says whose the sale is, by the kind.
func whose(d Decision, potNames map[string]string) string {
	shares := make([]string, len(d.Shares))
	for i, s := range d.Shares {
		shares[i] = fmt.Sprintf("%s %d%%", s.Person, s.Percent)
	}
	switch d.Kind {
	case KindOwner:
		if len(d.Shares) == 1 {
			return "Вся продажа — " + d.Shares[0].Person
		}
	case KindSplit:
		return "Делится: " + strings.Join(shares, ", ")
	case KindPot:
		name := ""
		if d.Pot != nil {
			name = *d.Pot
			if n := potNames[*d.Pot]; n != "" {
				name = n + " (" + *d.Pot + ")"
			}
		}
		return "В общак " + name
	case KindOffPlatform:
		return "Мимо платформы, в отчёт: " + strings.Join(shares, ", ")
	case KindExclude:
		return "Не считается никому"
	}
	return d.Kind
}

func statusText(d Decision) string {
	switch d.Status {
	case StatusOpen:
		return "открыто, ждёт отчёта"
	case StatusApplied:
		return fmt.Sprintf("учтено в отчёте %s за %s", deref(d.AppliedPerson), deref(d.AppliedReportDate))
	case StatusCancelled:
		return "отменено: " + deref(d.CancelNote)
	}
	return d.Status
}

// PotNames reads the names of the pots by slug.
func PotNames(ctx context.Context, q ledger.Querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug, name FROM pots WHERE slug IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, name string
		if err := rows.Scan(&slug, &name); err != nil {
			return nil, err
		}
		out[slug] = name
	}
	return out, rows.Err()
}

func ptr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
