// Package pots keeps the owner's pots («общак», stage R3 of docs/plans/2026-11-10-shift-reports.md): the owner declares
// that purchases of some content on some pages go into a shared pot for a period; every purchase is split equally
// between the pot's participants on the purchase date (v_pot_shares, the indivisible micro-dollars to the participants
// in slug order). A share is revenue of the person's tab: internal/accruals adds it to the person's day in
// report_accruals, and it is paid at the tab's rate. Only the owner's word declares or changes a pot (decide, or next to
// the database); the team's chat agent adds and removes the purchases (reports).
package pots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Statuses of a pot.
const (
	StatusOpen   = "open"
	StatusClosed = "closed"
)

// Sources of a purchase.
const (
	SourceAgent      = "agent"
	SourceReportLine = "report_line"
	SourceOwner      = "owner"
)

// Refusal is a request not done. Its message is Russian.
type Refusal = decisions.Refusal

func refuse(format string, args ...any) *Refusal { return &Refusal{Msg: fmt.Sprintf(format, args...)} }

// ErrNotFound is a pot or a purchase that is not there.
var ErrNotFound = errors.New("not found")

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Participant shares the purchases made from From through To (inclusive; nil — open-ended).
type Participant struct {
	Person string  `json:"person"`
	From   string  `json:"from"`
	To     *string `json:"to"`
}

// DeclareInput is the owner's word: the body of POST /v1/pots and of `ledger pot declare`. The same slug again changes
// the pot.
type DeclareInput struct {
	Slug         string          `json:"slug"`
	Name         string          `json:"name"`
	Model        string          `json:"model"`
	Pages        []string        `json:"pages"`
	StartsAt     string          `json:"starts_at"`
	EndsAt       *string         `json:"ends_at"`
	Participants []Participant   `json:"participants"`
	Recognise    json.RawMessage `json:"recognise,omitempty"`
	DeclaredBy   string          `json:"declared_by"`
	DeclaredAt   string          `json:"declared_at"`
	DecisionRef  string          `json:"decision_ref,omitempty"`
	Status       string          `json:"status,omitempty"`
	Note         string          `json:"note,omitempty"`
}

// Pot is a declared pot.
type Pot struct {
	Slug         string          `json:"slug"`
	Name         string          `json:"name"`
	Model        string          `json:"model"`
	Pages        []string        `json:"pages"`
	StartsAt     string          `json:"starts_at"`
	EndsAt       *string         `json:"ends_at"`
	Participants []Participant   `json:"participants"`
	Recognise    json.RawMessage `json:"recognise"`
	DeclaredBy   string          `json:"declared_by"`
	DeclaredAt   string          `json:"declared_at"`
	DecisionRef  string          `json:"decision_ref"`
	Status       string          `json:"status"`
	Note         string          `json:"note"`
	OpenedAt     string          `json:"opened_at"`
	UpdatedAt    string          `json:"updated_at"`
	Sales        int             `json:"sales"`      // purchases in the pot (not removed)
	NetMicros    int64           `json:"net_micros"` // their net
}

// Share is a participant's share of a purchase.
type Share struct {
	Person string `json:"person"`
	Micros int64  `json:"micros"`
}

// Sale is a purchase of a pot.
type Sale struct {
	ID             int64   `json:"id"`
	Pot            string  `json:"pot"`
	TransactionRef *string `json:"transaction_ref"`
	PageLabel      string  `json:"page_label"`
	FanRef         string  `json:"fan_ref"`
	FanName        string  `json:"fan_name"`
	NetMicros      int64   `json:"net_micros"`
	OccurredAt     string  `json:"occurred_at"`
	Day            string  `json:"day"`
	Source         string  `json:"source"`
	ReportID       *int64  `json:"report_id"`
	ReportN        *int64  `json:"report_n"`
	Note           string  `json:"note"`
	AddedAt        string  `json:"added_at"`
	KeyName        string  `json:"key_name"`
	RemovedAt      *string `json:"removed_at"`
	RemovedNote    *string `json:"removed_note"`
	// HubDecisionID is Hub's decision of kind pot the purchase follows (written by the job hub-decisions); nil — by hand.
	HubDecisionID *int64  `json:"hub_decision_id"`
	Shares        []Share `json:"shares"`
	Flag          *string `json:"flag"` // a purchase with no participant on its date: no share
}

// Total is a person's share of a pot in all.
type Total struct {
	Person string `json:"person"`
	Sales  int    `json:"sales"`
	Micros int64  `json:"micros"`
}

// Shown is a pot with its purchases and each person's total share.
type Shown struct {
	Pot     Pot     `json:"pot"`
	Sales   []Sale  `json:"sales"`
	Totals  []Total `json:"totals"`
	Removed []Sale  `json:"removed"`
}

// inPot says whether a purchase of a page on a Moscow day belongs to the pot by its pages and dates.
func (p Pot) inPot(page, day string) error {
	found := false
	for _, x := range p.Pages {
		found = found || x == page
	}
	if !found {
		return refuse("страница %s не в общаке %s (страницы: %s)", page, p.Slug, strings.Join(p.Pages, ", "))
	}
	if day < p.StartsAt || (p.EndsAt != nil && day > *p.EndsAt) {
		end := "без конца"
		if p.EndsAt != nil {
			end = "по " + *p.EndsAt
		}
		return refuse("покупка %s вне срока общака %s: с %s %s", day, p.Slug, p.StartsAt, end)
	}
	return nil
}

// normalize checks a declaration.
func normalize(ctx context.Context, q ledger.Querier, in DeclareInput) (DeclareInput, error) {
	in.Slug, in.Name, in.Model = strings.TrimSpace(in.Slug), strings.TrimSpace(in.Name), strings.TrimSpace(in.Model)
	in.DeclaredBy, in.DecisionRef, in.Note = strings.TrimSpace(in.DeclaredBy), strings.TrimSpace(in.DecisionRef), strings.TrimSpace(in.Note)
	in.Status = strings.TrimSpace(in.Status)
	if !slugRe.MatchString(in.Slug) {
		return in, refuse("slug: код общака — строчные латинские буквы, цифры и дефис, а не %q", in.Slug)
	}
	if in.Name == "" {
		return in, refuse("нужно название общака (name)")
	}
	if in.Model == "" {
		return in, refuse("нужна модель общака (model), как в report_accruals")
	}
	if in.Status == "" {
		in.Status = StatusOpen
	}
	if in.Status != StatusOpen && in.Status != StatusClosed {
		return in, refuse("status бывает open или closed, а не %q", in.Status)
	}
	if in.DeclaredBy == "" {
		return in, refuse("нужно, кто объявил (declared_by)")
	}
	var err error
	if in.DeclaredAt, err = decisions.NormDateOrTime("declared_at", in.DeclaredAt); err != nil {
		return in, err
	}
	if in.StartsAt, err = decisions.NormDate("starts_at", in.StartsAt); err != nil {
		return in, err
	}
	if in.EndsAt != nil && strings.TrimSpace(*in.EndsAt) == "" {
		in.EndsAt = nil
	}
	if in.EndsAt != nil {
		end, err := decisions.NormDate("ends_at", *in.EndsAt)
		if err != nil {
			return in, err
		}
		if end < in.StartsAt {
			return in, refuse("срок общака %s — %s: конец раньше начала", in.StartsAt, end)
		}
		in.EndsAt = &end
	}
	if len(in.Pages) == 0 {
		return in, refuse("нужна хотя бы одна страница (pages)")
	}
	models, err := payroll.HubPageModels(ctx, q)
	if err != nil {
		return in, err
	}
	seen := map[string]bool{}
	for i, p := range in.Pages {
		p = strings.TrimSpace(p)
		if p == "" {
			return in, refuse("страница %d: пустое имя", i+1)
		}
		if seen[p] {
			return in, refuse("страница %s повторяется в pages", p)
		}
		seen[p] = true
		if m, ok := models[p]; ok && m != in.Model {
			return in, refuse("страница %s — модели %s, а общак модели %s", p, m, in.Model)
		}
		in.Pages[i] = p
	}
	if len(in.Recognise) == 0 {
		in.Recognise = json.RawMessage("{}")
	}
	if !json.Valid(in.Recognise) {
		return in, refuse("recognise: нужен JSON")
	}
	if len(in.Participants) == 0 {
		return in, refuse("нужен хотя бы один участник (participants)")
	}
	byPerson := map[string][]Participant{}
	for i := range in.Participants {
		p := &in.Participants[i]
		p.Person = strings.TrimSpace(p.Person)
		if p.Person == "" {
			return in, refuse("участник %d: нужен код человека (person)", i+1)
		}
		ok, err := decisions.PersonExists(ctx, q, p.Person)
		if err != nil {
			return in, err
		}
		if !ok {
			return in, refuse("участник %q — не человек: его нет в people", p.Person)
		}
		if strings.TrimSpace(p.From) == "" {
			p.From = in.StartsAt
		}
		if p.From, err = decisions.NormDate("участник "+p.Person+": from", p.From); err != nil {
			return in, err
		}
		if p.To != nil && strings.TrimSpace(*p.To) == "" {
			p.To = nil
		}
		if p.To != nil {
			to, err := decisions.NormDate("участник "+p.Person+": to", *p.To)
			if err != nil {
				return in, err
			}
			if to < p.From {
				return in, refuse("участник %s: to %s раньше from %s", p.Person, to, p.From)
			}
			p.To = &to
		}
		for _, o := range byPerson[p.Person] {
			if overlap(o, *p) {
				return in, refuse("участник %s: сроки участия пересекаются (%s и %s)", p.Person, o.From, p.From)
			}
		}
		byPerson[p.Person] = append(byPerson[p.Person], *p)
	}
	return in, nil
}

func overlap(a, b Participant) bool {
	aEnd, bEnd := "9999-12-31", "9999-12-31"
	if a.To != nil {
		aEnd = *a.To
	}
	if b.To != nil {
		bEnd = *b.To
	}
	return a.From <= bEnd && b.From <= aEnd
}

// Changed recomputes what the pots feed after a write: the accruals from the reports, and the debts when the reports
// accrue. The write stands when it fails: the hourly job retries.
func Changed(ctx context.Context, store *db.Store, now time.Time) error {
	return reports.Changed(ctx, store, now)
}

// Declare writes a pot, or changes the pot of the slug. A change that would put a purchase in the pot outside its
// pages or dates is refused: the purchase is removed first.
func Declare(ctx context.Context, store *db.Store, in DeclareInput, keyName string, now time.Time) (Pot, error) {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Pot{}, err
	}
	defer tx.Rollback()
	if in, err = normalize(ctx, tx, in); err != nil {
		return Pot{}, err
	}
	next := Pot{Slug: in.Slug, Pages: in.Pages, StartsAt: in.StartsAt, EndsAt: in.EndsAt}
	sales, err := sales(ctx, tx, in.Slug, false)
	if err != nil {
		return Pot{}, err
	}
	for _, s := range sales {
		if err := next.inPot(s.PageLabel, s.Day); err != nil {
			return Pot{}, refuse("покупка %d (%s) выпадает из нового объявления: %s — сначала ledger pot remove-sale %d", s.ID, s.Day, err.Error(), s.ID)
		}
	}
	pages, _ := json.Marshal(in.Pages)
	parts, _ := json.Marshal(in.Participants)
	at := ts.Format(now)
	var ends any
	if in.EndsAt != nil {
		ends = *in.EndsAt
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pots(slug, name, model, pages, starts_at, ends_at, participants, recognise, declared_by,
		declared_at, decision_ref, status, note, opened_at, updated_at, key_name, scope, method)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', 'equal')
		ON CONFLICT(slug) WHERE slug IS NOT NULL DO UPDATE SET name = excluded.name, model = excluded.model, pages = excluded.pages,
		starts_at = excluded.starts_at, ends_at = excluded.ends_at, participants = excluded.participants, recognise = excluded.recognise,
		declared_by = excluded.declared_by, declared_at = excluded.declared_at, decision_ref = excluded.decision_ref,
		status = excluded.status, note = excluded.note, updated_at = excluded.updated_at, key_name = excluded.key_name`,
		in.Slug, in.Name, in.Model, string(pages), in.StartsAt, ends, string(parts), string(in.Recognise), in.DeclaredBy,
		in.DeclaredAt, in.DecisionRef, in.Status, in.Note, at, at, keyName); err != nil {
		return Pot{}, err
	}
	p, err := get(ctx, tx, in.Slug)
	if err != nil {
		return Pot{}, err
	}
	return p, tx.Commit()
}

const potColumns = `p.slug, p.name, p.model, p.pages, p.starts_at, p.ends_at, p.participants, p.recognise, p.declared_by,
	COALESCE(p.declared_at, ''), p.decision_ref, p.status, p.note, p.opened_at, COALESCE(p.updated_at, ''),
	(SELECT COUNT(*) FROM pot_sales s WHERE s.pot = p.slug AND s.removed_at IS NULL),
	(SELECT COALESCE(SUM(net_micros), 0) FROM pot_sales s WHERE s.pot = p.slug AND s.removed_at IS NULL)`

func scanPot(row interface{ Scan(...any) error }) (Pot, error) {
	var p Pot
	var pages, parts, rec string
	var ends sql.NullString
	if err := row.Scan(&p.Slug, &p.Name, &p.Model, &pages, &p.StartsAt, &ends, &parts, &rec, &p.DeclaredBy, &p.DeclaredAt,
		&p.DecisionRef, &p.Status, &p.Note, &p.OpenedAt, &p.UpdatedAt, &p.Sales, &p.NetMicros); err != nil {
		return Pot{}, err
	}
	if ends.Valid {
		p.EndsAt = &ends.String
	}
	p.Pages, p.Participants = []string{}, []Participant{}
	_ = json.Unmarshal([]byte(pages), &p.Pages)
	_ = json.Unmarshal([]byte(parts), &p.Participants)
	p.Recognise = json.RawMessage(rec)
	return p, nil
}

func get(ctx context.Context, q ledger.Querier, slug string) (Pot, error) {
	p, err := scanPot(q.QueryRowContext(ctx, `SELECT `+potColumns+` FROM pots p WHERE p.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return Pot{}, ErrNotFound
	}
	return p, err
}

// List reads every declared pot.
func List(ctx context.Context, q ledger.Querier) ([]Pot, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+potColumns+` FROM pots p WHERE p.slug IS NOT NULL ORDER BY p.starts_at, p.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pot{}
	for rows.Next() {
		p, err := scanPot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Show reads a pot with its purchases, their shares and each person's total.
func Show(ctx context.Context, q ledger.Querier, slug string) (Shown, error) {
	p, err := get(ctx, q, slug)
	if err != nil {
		return Shown{}, err
	}
	out := Shown{Pot: p, Sales: []Sale{}, Totals: []Total{}, Removed: []Sale{}}
	all, err := sales(ctx, q, slug, true)
	if err != nil {
		return Shown{}, err
	}
	byID := map[int64]int{}
	for _, s := range all {
		if s.RemovedAt != nil {
			out.Removed = append(out.Removed, s)
			continue
		}
		byID[s.ID] = len(out.Sales)
		out.Sales = append(out.Sales, s)
	}
	rows, err := q.QueryContext(ctx, `SELECT sale_id, person, share_micros, flag FROM v_pot_shares WHERE pot = ? ORDER BY sale_id, person`, slug)
	if err != nil {
		return Shown{}, err
	}
	defer rows.Close()
	totals := map[string]*Total{}
	for rows.Next() {
		var id int64
		var person, flag sql.NullString
		var share sql.NullInt64
		if err := rows.Scan(&id, &person, &share, &flag); err != nil {
			return Shown{}, err
		}
		i, ok := byID[id]
		if !ok {
			continue
		}
		if !person.Valid {
			f := flag.String
			out.Sales[i].Flag = &f
			continue
		}
		out.Sales[i].Shares = append(out.Sales[i].Shares, Share{Person: person.String, Micros: share.Int64})
		t := totals[person.String]
		if t == nil {
			t = &Total{Person: person.String}
			totals[person.String] = t
		}
		t.Sales++
		t.Micros += share.Int64
	}
	if err := rows.Err(); err != nil {
		return Shown{}, err
	}
	for _, t := range totals {
		out.Totals = append(out.Totals, *t)
	}
	sort.Slice(out.Totals, func(i, j int) bool { return out.Totals[i].Person < out.Totals[j].Person })
	return out, nil
}

const saleColumns = `id, pot, transaction_ref, page_label, fan_ref, fan_name, net_micros, occurred_at, day, source, report_id, report_n,
	note, added_at, key_name, removed_at, removed_note, hub_decision_id`

func scanSale(row interface{ Scan(...any) error }) (Sale, error) {
	var s Sale
	var ref, removed, removedNote sql.NullString
	var rid, rn, decision sql.NullInt64
	if err := row.Scan(&s.ID, &s.Pot, &ref, &s.PageLabel, &s.FanRef, &s.FanName, &s.NetMicros, &s.OccurredAt, &s.Day, &s.Source,
		&rid, &rn, &s.Note, &s.AddedAt, &s.KeyName, &removed, &removedNote, &decision); err != nil {
		return Sale{}, err
	}
	if decision.Valid {
		s.HubDecisionID = &decision.Int64
	}
	if ref.Valid {
		s.TransactionRef = &ref.String
	}
	if rid.Valid {
		s.ReportID = &rid.Int64
	}
	if rn.Valid {
		s.ReportN = &rn.Int64
	}
	if removed.Valid {
		s.RemovedAt = &removed.String
	}
	if removedNote.Valid {
		s.RemovedNote = &removedNote.String
	}
	s.Shares = []Share{}
	return s, nil
}

// sales reads the purchases of a pot; withRemoved — the removed ones too.
func sales(ctx context.Context, q ledger.Querier, slug string, withRemoved bool) ([]Sale, error) {
	query := `SELECT ` + saleColumns + ` FROM pot_sales WHERE pot = ?`
	if !withRemoved {
		query += ` AND removed_at IS NULL`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY occurred_at, id`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sale
	for rows.Next() {
		s, err := scanSale(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LineRef names a line of a saved report: the report version and the line's number as the agent's script printed it.
type LineRef struct {
	ReportID int64 `json:"report_id"`
	N        int64 `json:"n"`
}

// SaleInput is the body of POST /v1/pots/{slug}/sales. By transaction_ref — Hub's id of the sale or the platform's
// reference of it (hub.SaleRefsOf) — the page, fan, net and time come from ledger's mirror of Hub's sales when the
// transaction is there and must be given when it is not; by from_line they come from a line of a saved report.
// Amounts are mills.
type SaleInput struct {
	TransactionRef string   `json:"transaction_ref,omitempty"`
	FromLine       *LineRef `json:"from_line,omitempty"`
	PageLabel      string   `json:"page_label,omitempty"`
	FanRef         string   `json:"fan_ref,omitempty"`
	FanName        string   `json:"fan_name,omitempty"`
	NetMills       *int64   `json:"net_mills,omitempty"`
	OccurredAt     string   `json:"occurred_at,omitempty"`
	Source         string   `json:"source,omitempty"`
	Note           string   `json:"note,omitempty"`
	// Force adds a purchase that a saved report already counts in a chatter's own page amount.
	Force bool `json:"force,omitempty"`
}

// SaleResult is the answer of an added purchase.
type SaleResult struct {
	Sale     Sale     `json:"sale"`
	Warnings []string `json:"warnings"`
}

type found struct {
	page, fanRef, fanName, at string
	net                       int64
	ok                        bool
}

// purchase is a purchase that is in a pot now, and the reference of the sale it was written with.
type purchase struct {
	id       int64
	pot, ref string
}

// fill takes a value from the source unless the request gave another: a contradiction is refused.
func fill(name, given, have string) (string, error) {
	switch {
	case given == "":
		return have, nil
	case have == "" || given == have:
		return given, nil
	}
	return "", refuse("%s: в запросе %q, а у продажи %q", name, given, have)
}

// AddSale checks a purchase against its pot and writes it. defaultSource is the source when the request names none.
func AddSale(ctx context.Context, store *db.Store, slug string, in SaleInput, defaultSource, keyName string, now time.Time) (SaleResult, error) {
	res := SaleResult{Warnings: []string{}}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	p, err := get(ctx, tx, slug)
	if errors.Is(err, ErrNotFound) {
		return res, refuse("общака %q нет", slug)
	}
	if err != nil {
		return res, err
	}
	if p.Status != StatusOpen {
		return res, refuse("общак %s закрыт: новых покупок не принимает", slug)
	}
	in.TransactionRef, in.PageLabel = strings.TrimSpace(in.TransactionRef), strings.TrimSpace(in.PageLabel)
	in.FanRef, in.FanName, in.Note = strings.TrimSpace(in.FanRef), strings.TrimSpace(in.FanName), strings.TrimSpace(in.Note)
	in.Source = strings.TrimSpace(in.Source)
	var given found
	given.page, given.fanRef, given.fanName = in.PageLabel, in.FanRef, in.FanName
	if in.OccurredAt != "" {
		if given.at, err = decisions.NormTime("occurred_at", in.OccurredAt); err != nil {
			return res, err
		}
	}
	if in.NetMills != nil {
		if given.net, err = decisions.CheckMills("net_mills", *in.NetMills); err != nil {
			return res, err
		}
	}
	var src found
	var reportID, reportN any
	source := in.Source
	if in.FromLine != nil {
		src, err = fromLine(ctx, tx, *in.FromLine)
		if err != nil {
			return res, err
		}
		if !src.ok {
			return res, refuse("в отчёте %d нет продажи с номером %d", in.FromLine.ReportID, in.FromLine.N)
		}
		var ref sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT transaction_ref FROM shift_report_lines WHERE report_id = ? AND n = ? AND added = 0`,
			in.FromLine.ReportID, in.FromLine.N).Scan(&ref); err != nil {
			return res, err
		}
		if in.TransactionRef != "" && ref.String != in.TransactionRef {
			// The request may name the line's sale by its other reference: that is no contradiction.
			asked, err := hub.SaleRefsOf(ctx, tx, in.TransactionRef)
			if err != nil {
				return res, err
			}
			if a, b := asked.Either(); !ref.Valid || (ref.String != a && ref.String != b) {
				return res, refuse("transaction_ref: в запросе %q, а у строки %d:%d %q", in.TransactionRef, in.FromLine.ReportID, in.FromLine.N, ref.String)
			}
		}
		in.TransactionRef = ref.String
		reportID, reportN = in.FromLine.ReportID, in.FromLine.N
		if source == "" {
			source = SourceReportLine
		}
	}
	// One sale has two references — Hub's id and the platform's (hub.SaleRefsOf) — and the request, or its line, names
	// either: what follows knows the sale by both. The purchase keeps the reference it came with.
	refs, err := hub.SaleRefsOf(ctx, tx, in.TransactionRef)
	if err != nil {
		return res, err
	}
	if in.FromLine == nil && in.TransactionRef != "" {
		if src, err = fromHub(ctx, tx, refs.Hub()); err != nil {
			return res, err
		}
	}
	if source == "" {
		source = defaultSource
	}
	switch source {
	case SourceAgent, SourceReportLine, SourceOwner:
	default:
		return res, refuse("source бывает agent, report_line или owner, а не %q", source)
	}
	// From the switch day's sales on (settings.sale_decisions_hub_from) a sale Hub sees goes to a pot by Hub's decision
	// alone: the job writes the purchase. By hand — money Hub does not see, and the sales before that day. A line of a
	// report is no other door: its sale is the same sale.
	if in.TransactionRef != "" {
		if why, err := hubOnly(ctx, tx, refs, slug); err != nil || why != "" {
			if err != nil {
				return res, err
			}
			return res, refuse("%s", why)
		}
	}
	var s found
	if s.page, err = fill("page_label", given.page, src.page); err != nil {
		return res, err
	}
	if s.fanRef, err = fill("fan_ref", given.fanRef, src.fanRef); err != nil {
		return res, err
	}
	if s.fanName, err = fill("fan_name", given.fanName, src.fanName); err != nil {
		return res, err
	}
	if s.at, err = fill("occurred_at", given.at, src.at); err != nil {
		return res, err
	}
	s.net = src.net
	if in.NetMills != nil {
		if src.ok && src.net != given.net {
			return res, refuse("net_mills: в запросе %s, а у продажи %s", money.Micros(given.net).Cents(), money.Micros(src.net).Cents())
		}
		s.net = given.net
	}
	if !src.ok {
		var missing []string
		if s.page == "" {
			missing = append(missing, "page_label")
		}
		if in.NetMills == nil && s.net == 0 {
			missing = append(missing, "net_mills")
		}
		if s.at == "" {
			missing = append(missing, "occurred_at")
		}
		if len(missing) > 0 {
			what := "продажи нет в зеркале хаба"
			if in.TransactionRef == "" {
				what = "покупка без transaction_ref"
			}
			return res, refuse("%s: нужны %s", what, strings.Join(missing, ", "))
		}
	}
	if s.at == "" {
		return res, refuse("у продажи нет времени: нужно occurred_at")
	}
	if s.net <= 0 {
		return res, refuse("нетто покупки %s: нужно больше нуля", money.Micros(s.net).Cents())
	}
	day := s.at[:10]
	if err := p.inPot(s.page, day); err != nil {
		return res, err
	}
	if in.TransactionRef != "" {
		// The sale may be in a pot already — under this reference or under its other one.
		have, err := purchasesOf(ctx, tx, refs)
		if err != nil {
			return res, err
		}
		if len(have) > 0 {
			under := ""
			if have[0].ref != refs.Given {
				under = ", записана под другой ссылкой той же продажи — " + have[0].ref
			}
			return res, refuse("продажа %s уже в общаке %s (покупка %d%s)", refs.Given, have[0].pot, have[0].id, under)
		}
		counted, err := countedIn(ctx, tx, refs)
		if err != nil {
			return res, err
		}
		if counted != "" {
			if !in.Force {
				return res, refuse("%s: по правилу команды продажа остаётся в отчёте чаттера; добавить всё равно — --force (force: true)", counted)
			}
			res.Warnings = append(res.Warnings, counted)
		}
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO pot_sales(pot, transaction_ref, page_label, fan_ref, fan_name, net_micros, occurred_at, day,
		source, report_id, report_n, note, added_at, key_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		slug, nullStr(in.TransactionRef), s.page, s.fanRef, s.fanName, s.net, s.at, day, source, reportID, reportN, in.Note,
		ts.Format(now), keyName)
	if err != nil {
		return res, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return res, err
	}
	if res.Sale, err = scanSale(tx.QueryRowContext(ctx, `SELECT `+saleColumns+` FROM pot_sales WHERE id = ?`, id)); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// purchasesOf are the purchases of a sale that are in a pot now, whichever of the sale's two references each was
// written with (hub.SaleRefsOf), the oldest first.
func purchasesOf(ctx context.Context, q ledger.Querier, refs hub.SaleRefs) ([]purchase, error) {
	a, b := refs.Either()
	rows, err := q.QueryContext(ctx, `SELECT id, pot, transaction_ref FROM pot_sales WHERE transaction_ref IN (?, ?) AND removed_at IS NULL
		ORDER BY id`, a, b)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []purchase
	for rows.Next() {
		var p purchase
		if err := rows.Scan(&p.id, &p.pot, &p.ref); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// fromHub reads a sale of ledger's mirror of Hub's sales by Hub's id of it — what a reference of either kind names is
// hub.SaleRefsOf(…).Hub().
func fromHub(ctx context.Context, q ledger.Querier, ref string) (found, error) {
	var f found
	var mills int64
	var at, payload string
	err := q.QueryRowContext(ctx, `SELECT page_label, net_mills, occurred_at, payload FROM hub_sales WHERE transaction_ref = ?`, ref).
		Scan(&f.page, &mills, &at, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.ok, f.net = true, int64(money.FromMills(mills))
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		f.at = ts.Format(t)
	} else {
		f.at = at
	}
	var sale struct {
		FanRef *string `json:"fanRef"`
	}
	if json.Unmarshal([]byte(payload), &sale) == nil && sale.FanRef != nil {
		f.fanRef = *sale.FanRef
	}
	return f, nil
}

// fromLine reads a sale line of a saved report.
func fromLine(ctx context.Context, q ledger.Querier, l LineRef) (found, error) {
	var f found
	var at sql.NullString
	err := q.QueryRowContext(ctx, `SELECT page_label, fan_ref, fan_name, net_micros, occurred_at FROM shift_report_lines
		WHERE report_id = ? AND n = ? AND added = 0`, l.ReportID, l.N).Scan(&f.page, &f.fanRef, &f.fanName, &f.net, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.ok, f.at = true, at.String
	return f, nil
}

// countedIn says which report counts the sale in a person's own page amount — a counted line of a current version of
// an agent's draft or of a report handed in in Hub (v_report_lines); "" — none. The agent's line names the sale by the
// platform's reference and Hub's line by Hub's id, so the sale is looked for by both of its references.
func countedIn(ctx context.Context, q ledger.Querier, refs hub.SaleRefs) (string, error) {
	var source, date, who string
	var id int64
	var n sql.NullInt64
	a, b := refs.Either()
	err := q.QueryRowContext(ctx, `SELECT source, report_id, report_date, who, line FROM v_report_lines
		WHERE transaction_ref IN (?, ?) AND counted = 1 ORDER BY source = 'hub' DESC, report_id DESC LIMIT 1`, a, b).Scan(&source, &id, &date, &who, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ref := refs.Given
	if source == "hub" {
		return fmt.Sprintf("продажа %s учтена в отчёте, сданном в хабе: версия %d (%s, %s)", ref, id, who, date), nil
	}
	line := ""
	if n.Valid {
		line = fmt.Sprintf(" строкой %d", n.Int64)
	}
	return fmt.Sprintf("продажа %s учтена в отчёте %d (%s, %s)%s", ref, id, who, date, line), nil
}

// RemoveInput is the body of POST /v1/pot-sales/{id}/remove.
type RemoveInput struct {
	Note string `json:"note"`
}

// RemoveSale takes a purchase back out of its pot; the row stays, with when and why.
func RemoveSale(ctx context.Context, store *db.Store, id int64, in RemoveInput, keyName string, now time.Time) (Sale, error) {
	note := strings.TrimSpace(in.Note)
	if note == "" {
		return Sale{}, refuse("нужна заметка, почему покупка снята (note)")
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Sale{}, err
	}
	defer tx.Rollback()
	s, err := scanSale(tx.QueryRowContext(ctx, `SELECT `+saleColumns+` FROM pot_sales WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Sale{}, ErrNotFound
	}
	if err != nil {
		return Sale{}, err
	}
	if s.RemovedAt != nil {
		return Sale{}, refuse("покупка %d уже снята из общака %s", id, s.Pot)
	}
	if why, err := hubKeeps(ctx, tx, s); err != nil || why != "" {
		if err != nil {
			return Sale{}, err
		}
		return Sale{}, refuse("%s", why)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pot_sales SET removed_at = ?, removed_note = ?, removed_by = ? WHERE id = ?`,
		ts.Format(now), note, keyName, id); err != nil {
		return Sale{}, err
	}
	if s, err = scanSale(tx.QueryRowContext(ctx, `SELECT `+saleColumns+` FROM pot_sales WHERE id = ?`, id)); err != nil {
		return Sale{}, err
	}
	return s, tx.Commit()
}

// Candidate is a line of a saved report held out of the total with a flag about a pot, not in any pot yet.
type Candidate struct {
	ReportID       int64    `json:"report_id"`
	N              *int64   `json:"n"`
	ReportDate     string   `json:"report_date"`
	Person         *string  `json:"person"`
	HubUsername    string   `json:"hub_username"`
	TransactionRef *string  `json:"transaction_ref"`
	PageLabel      string   `json:"page_label"`
	FanRef         string   `json:"fan_ref"`
	FanName        string   `json:"fan_name"`
	NetMicros      int64    `json:"net_micros"`
	OccurredAt     *string  `json:"occurred_at"`
	Flags          []string `json:"flags"`
	Pots           []string `json:"pots"` // the open pots the line fits by page and date
	// HubSale is Hub's id of the line's sale — the line itself names the sale by the platform's reference
	// (hub.SaleRefsOf); nil — ledger's mirrors of Hub do not know the sale.
	HubSale *string `json:"hub_sale"`
	// HubDecides says which way the line goes to a pot. true — the sale is one Hub sees of
	// settings.sale_decisions_hub_from or later: only Hub's decision gives it (hub sale decide --sale <hub_sale> --kind
	// pot --pot <код>), add-sale refuses it. false — by hand: ledger pot add-sale <код> --from-line <отчёт>:<n>.
	HubDecides bool `json:"hub_decides"`
}

// Candidates lists the lines of the current versions of saved reports that were held out of the total with a flag
// mentioning «общак» and are not in pot_sales yet — under either of the sale's two references; slug — only those that
// fit that pot. Each says how it goes to a pot: by Hub's decision or by hand (HubDecides).
func Candidates(ctx context.Context, q ledger.Querier, slug string) ([]Candidate, error) {
	all, err := List(ctx, q)
	if err != nil {
		return nil, err
	}
	var pots []Pot
	for _, p := range all {
		if p.Status == StatusOpen && (slug == "" || p.Slug == slug) {
			pots = append(pots, p)
		}
	}
	if slug != "" && len(pots) == 0 {
		if _, err := get(ctx, q, slug); errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return []Candidate{}, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT r.id, l.n, r.report_date, r.person, r.hub_username, l.transaction_ref, l.page_label,
		l.fan_ref, l.fan_name, l.net_micros, l.occurred_at, l.flags
		FROM shift_report_lines l JOIN shift_reports r ON r.id = l.report_id
		WHERE l.counted = 0 AND l.added = 0 AND r.status <> 'superseded'
		  AND NOT EXISTS (SELECT 1 FROM pot_sales s WHERE s.removed_at IS NULL AND `+hub.SameSaleSQL("s.transaction_ref", "l.transaction_ref")+`)
		ORDER BY r.report_date, r.id, l.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		var n sql.NullInt64
		var person, ref, at sql.NullString
		var flags string
		if err := rows.Scan(&c.ReportID, &n, &c.ReportDate, &person, &c.HubUsername, &ref, &c.PageLabel, &c.FanRef, &c.FanName,
			&c.NetMicros, &at, &flags); err != nil {
			return nil, err
		}
		c.Flags = []string{}
		_ = json.Unmarshal([]byte(flags), &c.Flags)
		mentions := false
		for _, f := range c.Flags {
			mentions = mentions || strings.Contains(strings.ToLower(f), "общак")
		}
		if !mentions {
			continue
		}
		if n.Valid {
			c.N = &n.Int64
		}
		if person.Valid {
			c.Person = &person.String
		}
		if ref.Valid {
			c.TransactionRef = &ref.String
		}
		day := c.ReportDate
		if at.Valid {
			c.OccurredAt = &at.String
			day = at.String[:10]
		}
		c.Pots = []string{}
		for _, p := range pots {
			if p.inPot(c.PageLabel, day) == nil {
				c.Pots = append(c.Pots, p.Slug)
			}
		}
		if slug != "" && len(c.Pots) == 0 {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Which way each line goes to a pot: the same question add-sale asks of it (hubOnly).
	from, err := decisions.HubFrom(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range out {
		c := &out[i]
		if c.TransactionRef == nil {
			continue
		}
		refs, err := hub.SaleRefsOf(ctx, q, *c.TransactionRef)
		if err != nil {
			return nil, err
		}
		sale, err := fromHub(ctx, q, refs.Hub())
		if err != nil {
			return nil, err
		}
		if refs.Known() || sale.ok {
			id := refs.Hub()
			c.HubSale = &id
		}
		c.HubDecides = hubDecides(from, sale)
	}
	return out, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
