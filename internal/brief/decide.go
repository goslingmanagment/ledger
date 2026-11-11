package brief

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Decisions of the owner.
const (
	Confirm  = "confirm"
	Personal = "personal"
	Custom   = "custom"
	Defer    = "defer"
	Recorded = "recorded"
)

// Refusal is a request refused as a whole: nothing was written, every reason is listed.
type Refusal struct{ Problems []string }

func (r *Refusal) Error() string { return strings.Join(r.Problems, "; ") }

func refuse(format string, args ...any) *Refusal {
	return &Refusal{Problems: []string{fmt.Sprintf(format, args...)}}
}

// Decision is one --set of ledger decide.
type Decision struct {
	Num  int    `json:"num"`
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

// ParseSet reads "3=custom:{…}", "5=recorded:388,389", "1=confirm".
func ParseSet(spec string) (Decision, error) {
	numText, val, ok := strings.Cut(spec, "=")
	num, err := strconv.Atoi(strings.TrimSpace(numText))
	if !ok || err != nil || num <= 0 {
		return Decision{}, fmt.Errorf("--set %s: нужно <номер>=<решение>[:текст]", spec)
	}
	d, err := ParseDecision(val)
	if err != nil {
		return Decision{}, fmt.Errorf("--set %s: %w", spec, err)
	}
	d.Num = num
	return d, nil
}

// ParseDecision reads "confirm", "custom:{…}", "recorded:388,389": a decision without an item number.
func ParseDecision(spec string) (Decision, error) {
	kind, text, _ := strings.Cut(strings.TrimSpace(spec), ":")
	switch kind {
	case Confirm, Personal, Custom, Defer, Recorded:
	default:
		return Decision{}, errors.New("решение — confirm, personal, custom, defer или recorded")
	}
	return Decision{Kind: kind, Text: strings.TrimSpace(text)}, nil
}

// Patch is the JSON of custom: fields over the suggested row; split makes several rows of it.
type Patch struct {
	Date         *string `json:"date"`
	Type         *string `json:"type"`
	Category     *string `json:"category"`
	Counterparty *string `json:"counterparty"`
	Model        *string `json:"model"`
	Amount       *flex   `json:"amount"`
	Currency     *string `json:"currency"`
	Method       *string `json:"method"`
	Status       *string `json:"status"`
	Note         *string `json:"note"`
	Split        []Patch `json:"split"`
}

// flex is an amount written as a string or a number.
type flex string

// UnmarshalJSON takes "12.50" and 12.50 alike, without going through a float.
func (f *flex) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flex(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("сумма: %s", b)
	}
	*f = flex(n.String())
	return nil
}

func parsePatch(text string) (Patch, error) {
	var p Patch
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, err
	}
	if dec.More() {
		return p, errors.New("лишнее после JSON")
	}
	return p, nil
}

func apply(p Patch, d bybit.SuggestedDocument) bybit.SuggestedDocument {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&d.Date, p.Date)
	set(&d.Type, p.Type)
	set(&d.Category, p.Category)
	set(&d.Counterparty, p.Counterparty)
	set(&d.Model, p.Model)
	if p.Amount != nil {
		d.Amount = string(*p.Amount)
	}
	set(&d.Currency, p.Currency)
	set(&d.Method, p.Method)
	set(&d.Status, p.Status)
	set(&d.Note, p.Note)
	return d
}

// baseDocuments is what a custom answer starts from: the suggested rows, or what the proposal knows.
func baseDocuments(c Content) []bybit.SuggestedDocument {
	if len(c.Documents) > 0 {
		return c.Documents
	}
	return c.Skeleton
}

// withKey keeps the movement's key in a note someone rewrote: the row must stay traceable.
func withKey(note, key string) string {
	if key == "" || strings.Contains(note, key) {
		return note
	}
	if note == "" {
		return key
	}
	return note + "; " + key
}

// patched applies a custom answer: the patch goes over the main row (several rows with split); other
// rows of the proposal, such as the network fee, keep their own and take only the counterparty and model.
func patched(c Content, key, text string) ([]bybit.SuggestedDocument, error) {
	p, err := parsePatch(text)
	if err != nil {
		return nil, fmt.Errorf("custom в боевом режиме — JSON-правка строки (category, counterparty, model, note, type, amount, split): %v", err)
	}
	if c.AwaitDeposit && p.Amount == nil && len(p.Split) == 0 {
		return nil, errors.New("custom: пополнение CinCin ждёт депозита от бота, нужна сумма, дошедшая до кошелька: amount")
	}
	base := baseDocuments(c)
	if len(base) == 0 { // nothing suggested: the answer is the whole row, the journal's checks name what is missing
		base = []bybit.SuggestedDocument{{Date: c.Date, Model: "—", Status: "paid", Note: key}}
	}
	main := apply(p, base[0])
	var out []bybit.SuggestedDocument
	if len(p.Split) > 0 {
		if p.Amount != nil {
			return nil, errors.New("custom: amount вместе со split — суммы задаются в split")
		}
		for _, part := range p.Split {
			if len(part.Split) > 0 {
				return nil, errors.New("custom: split внутри split")
			}
			if part.Amount == nil {
				return nil, errors.New("custom: в split у каждой части своя сумма")
			}
			out = append(out, apply(part, main))
		}
	} else {
		out = append(out, main)
	}
	for i := range out {
		out[i].Note = withKey(out[i].Note, key)
		if v, err := money.Parse(out[i].Amount); err == nil { // the journal writes 60.00, not 60
			out[i].Amount = v.String()
		}
	}
	for _, d := range base[1:] {
		if p.Counterparty != nil {
			d.Counterparty = *p.Counterparty
		}
		if p.Model != nil {
			d.Model = *p.Model
		}
		out = append(out, d)
	}
	return out, nil
}

// ownerName is how the journal names the owner in draw rows; only the database knows it.
func ownerName(ctx context.Context, q ledger.Querier) (string, error) {
	var name string
	err := q.QueryRowContext(ctx, `SELECT counterparty FROM v_documents WHERE category = 'Owner draw' ORDER BY num DESC LIMIT 1`).Scan(&name)
	if err == sql.ErrNoRows {
		return "Owner", nil
	}
	return name, err
}

// personalDocuments is a personal spend from the business wallet: an Owner draw row, the fee as it was.
func personalDocuments(ctx context.Context, q ledger.Querier, c Content, key string) ([]bybit.SuggestedDocument, error) {
	base := baseDocuments(c)
	if len(base) == 0 {
		return nil, nil
	}
	name, err := ownerName(ctx, q)
	if err != nil {
		return nil, err
	}
	out := append([]bybit.SuggestedDocument(nil), base...)
	out[0].Type, out[0].Category, out[0].Counterparty, out[0].Model = "expense", "Owner draw", name, "—"
	out[0].Note = withKey("личное", key)
	for i := 1; i < len(out); i++ {
		out[i].Counterparty = name
	}
	return out, nil
}

func parseNums(text string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(text, ",") {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(part), "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("recorded: «%s» не номер строки", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func toRows(docs []bybit.SuggestedDocument) []journal.Row {
	rows := make([]journal.Row, len(docs))
	for i, d := range docs {
		rows[i] = journal.Row{Date: d.Date, Type: d.Type, Category: d.Category, Counterparty: d.Counterparty, Model: d.Model,
			Amount: d.Amount, Currency: d.Currency, Method: d.Method, Status: d.Status, Note: d.Note}
	}
	return rows
}

// checkRows is the dry run of a write: the rows must map to entries and move the money the proposal
// says moved. What a row calls the money may change; how much moved may not.
func checkRows(ctx context.Context, q ledger.Querier, c Content, docs []bybit.SuggestedDocument) error {
	_, entries, err := journal.Check(ctx, q, toRows(docs))
	if err != nil {
		return err
	}
	if c.Money == nil {
		return nil
	}
	want, err := money.Parse(c.Money.Amount)
	if err != nil {
		return fmt.Errorf("сумма предложения: %w", err)
	}
	var all []ledger.Entry
	for _, e := range entries {
		all = append(all, e...)
	}
	if got := journal.Effect(all, c.Money.Account); got != want {
		return fmt.Errorf("строки двигают %s по счёту %s, а движение — %s", got, c.Money.Account, want)
	}
	return nil
}

// Written says what one decision did.
type Written struct {
	Num       int    `json:"num"`
	Key       string `json:"key"`
	Decision  string `json:"decision"`
	Status    string `json:"status"`
	Documents []int  `json:"documents,omitempty"`
	Proposal  int64  `json:"proposal,omitempty"` // decided directly, without a brief
}

// Result of Decide.
type Result struct {
	Brief   int64     `json:"brief"`
	Mode    string    `json:"mode"`
	Written []Written `json:"written"`
	Text    string    `json:"text"`
}

type planned struct {
	label string // «п.3» in a brief, «предложение #32» when decided directly
	d     Decision
	item  Item
	p     Proposal
	c     Content
	docs  []bybit.SuggestedDocument
	nums  []int
}

func validate(ctx context.Context, q ledger.Querier, mode string, b *Brief, d Decision) (planned, error) {
	for _, it := range b.Items {
		if it.Num == d.Num {
			return validateItem(ctx, q, mode, it, d, fmt.Sprintf("п.%d", d.Num))
		}
	}
	return planned{d: d}, fmt.Errorf("нет пункта %d", d.Num)
}

// validateItem checks one decision on one question; label names it in the refusals.
func validateItem(ctx context.Context, q ledger.Querier, mode string, it Item, d Decision, label string) (planned, error) {
	pl := planned{label: label, d: d, item: it}
	switch d.Kind {
	case Confirm, Personal, Custom, Defer, Recorded:
	default:
		return pl, fmt.Errorf("%s: решение «%s»: допустимо confirm, personal, custom, defer, recorded", label, d.Kind)
	}
	var err error
	if pl.p, err = GetProposal(ctx, q, pl.item.ProposalID); err != nil {
		return pl, err
	}
	last, _, err := LatestProposal(ctx, q, pl.item.Key)
	if err != nil {
		return pl, err
	}
	if last.Version != pl.item.ProposalVersion {
		return pl, fmt.Errorf("%s: предложение изменилось (в брифе версия %d, сейчас %d), нужен новый бриф", label, pl.item.ProposalVersion, last.Version)
	}
	switch pl.p.Status {
	case "open":
	case "auto":
		if d.Kind != Confirm {
			return pl, fmt.Errorf("%s: записано само по правилу, исправление через ledger dispute", label)
		}
		return pl, nil
	default:
		closedBy := pl.p.Status
		if pl.p.Decision != "" {
			closedBy += ", " + pl.p.Decision
		}
		return pl, fmt.Errorf("%s: вопрос уже закрыт (%s), решение не нужно", label, closedBy)
	}
	if pl.c, err = pl.p.ParseContent(); err != nil {
		return pl, err
	}
	if mode != "live" {
		return pl, nil
	}
	switch d.Kind {
	case Confirm:
		if pl.c.AwaitDeposit { // the row is the amount that reached the wallet: the bot has not said it yet
			return pl, fmt.Errorf("%s: пополнение CinCin ждёт депозита от бота; отложи или custom с суммой, дошедшей до кошелька", label)
		}
		if len(pl.c.Documents) == 0 {
			return pl, fmt.Errorf("%s: предложенной строки нет, нужен custom, personal или recorded", label)
		}
		pl.docs = pl.c.Documents
	case Custom:
		if pl.docs, err = patched(pl.c, pl.c.Key, d.Text); err != nil {
			return pl, fmt.Errorf("%s: %w", label, err)
		}
	case Personal:
		if pl.c.PersonalDraw {
			if pl.docs, err = personalDocuments(ctx, q, pl.c, pl.c.Key); err != nil {
				return pl, err
			}
		}
	case Recorded:
		text := d.Text
		if text == "" {
			if strings.Contains(pl.item.LedgerMatch, ",") || pl.item.LedgerMatch == "" {
				return pl, fmt.Errorf("%s: recorded без номеров строк, а подсказка неоднозначна: recorded:<номер,номер>", label)
			}
			text = pl.item.LedgerMatch
		}
		if pl.nums, err = parseNums(text); err != nil {
			return pl, fmt.Errorf("%s: %w", label, err)
		}
		for _, n := range pl.nums {
			if _, err := ledger.CurrentDocument(ctx, q, n); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return pl, fmt.Errorf("%s: строки #%d нет", label, n)
				}
				return pl, err
			}
		}
	}
	if len(pl.docs) > 0 {
		if err := checkRows(ctx, q, pl.c, pl.docs); err != nil {
			return pl, fmt.Errorf("%s: %w", label, err)
		}
	}
	return pl, nil
}

func formatNums(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// Decide applies a set of decisions to a brief. The caller gives one transaction: any problem refuses
// the whole set (*Refusal) and the caller rolls back.
func Decide(ctx context.Context, q ledger.Querier, briefID int64, set []Decision, by string, now time.Time) (*Result, error) {
	if len(set) == 0 {
		return nil, refuse("нет решений: --set <номер>=<решение>")
	}
	b, err := Get(ctx, q, briefID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refuse("бриф #%d не найден", briefID)
	}
	if err != nil {
		return nil, err
	}
	mode, err := bybit.Mode(ctx, q)
	if err != nil {
		return nil, err
	}
	var problems []string
	plans := make([]planned, 0, len(set))
	seen := map[int]bool{}
	for _, d := range set {
		if seen[d.Num] {
			problems = append(problems, fmt.Sprintf("п.%d: два решения в одном наборе", d.Num))
			continue
		}
		seen[d.Num] = true
		pl, err := validate(ctx, q, mode, b, d)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		plans = append(plans, pl)
	}
	if len(problems) > 0 {
		return nil, &Refusal{Problems: problems}
	}
	res := &Result{Brief: briefID, Mode: mode, Written: []Written{}}
	for _, pl := range plans {
		w, err := execute(ctx, q, mode, briefID, pl, by, now)
		if err != nil {
			return nil, err
		}
		res.Written = append(res.Written, w)
	}
	if err := closeResolved(ctx, q, now); err != nil {
		return nil, err
	}
	if b, err = Get(ctx, q, briefID); err != nil {
		return nil, err
	}
	res.Text = b.Text()
	return res, nil
}

// DecideProposal applies one owner decision to one proposal without a brief: the owner said it in the chat
// of the session that calls it. The proposal must be the latest version of its question; a question about
// it in a brief is answered with it. A deferral has nothing to do here: the brief asks anyway.
func DecideProposal(ctx context.Context, q ledger.Querier, id int64, d Decision, by string, now time.Time) (*Result, error) {
	label := fmt.Sprintf("предложение #%d", id)
	if d.Kind == Defer {
		return nil, refuse("%s: defer без брифа не нужен, вопрос и так придёт в бриф", label)
	}
	p, err := GetProposal(ctx, q, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refuse("%s не найдено", label)
	}
	if err != nil {
		return nil, err
	}
	last, _, err := LatestProposal(ctx, q, p.Key)
	if err != nil {
		return nil, err
	}
	if last.ID != p.ID {
		return nil, refuse("%s устарело: по %s сейчас #%d", label, p.Key, last.ID)
	}
	it := Item{ProposalID: p.ID, ProposalVersion: p.Version, Key: p.Key}
	if d.Kind == Recorded && d.Text == "" {
		c, err := p.ParseContent()
		if err != nil {
			return nil, err
		}
		matches, err := matchRows(ctx, q, c)
		if err != nil {
			return nil, err
		}
		it.LedgerMatch = matchText(matches)
	}
	mode, err := bybit.Mode(ctx, q)
	if err != nil {
		return nil, err
	}
	pl, err := validateItem(ctx, q, mode, it, d, label)
	if err != nil {
		return nil, &Refusal{Problems: []string{err.Error()}}
	}
	w, err := execute(ctx, q, mode, 0, pl, by, now)
	if err != nil {
		return nil, err
	}
	w.Proposal = p.ID
	if err := closeResolved(ctx, q, now); err != nil {
		return nil, err
	}
	text := fmt.Sprintf("%s (%s): %s, %s", label, p.Summary, d.Kind, w.Status)
	if len(w.Documents) > 0 {
		text += ", строки #" + formatNums(w.Documents)
	}
	return &Result{Mode: mode, Written: []Written{w}, Text: text}, nil
}

func execute(ctx context.Context, q ledger.Querier, mode string, briefID int64, pl planned, by string, now time.Time) (Written, error) {
	w := Written{Num: pl.d.Num, Key: pl.item.Key, Decision: pl.d.Kind, Status: pl.p.Status}
	at := ts.Format(now)
	// Decided directly (briefID 0): no brief and item of its own; a question about it in a brief is
	// answered with it, so the brief does not ask again.
	var bID, bItem any
	ref := fmt.Sprintf("ledger:proposal/%d", pl.p.ID)
	if briefID != 0 {
		bID, bItem = briefID, pl.d.Num
		ref = fmt.Sprintf("ledger:%d/%d", briefID, pl.d.Num)
	}
	markItem := func() error {
		if briefID == 0 {
			_, err := q.ExecContext(ctx, `UPDATE brief_items SET decision = ?, decision_text = ?, decided_at = ?, decided_by = ?, document_nums = ?
				WHERE proposal_id = ? AND decision IS NULL`, pl.d.Kind, pl.d.Text, at, by, formatNums(w.Documents), pl.p.ID)
			return err
		}
		_, err := q.ExecContext(ctx, `UPDATE brief_items SET decision = ?, decision_text = ?, decided_at = ?, decided_by = ?, document_nums = ?
			WHERE brief_id = ? AND num = ?`, pl.d.Kind, pl.d.Text, at, by, formatNums(w.Documents), briefID, pl.d.Num)
		return err
	}
	if pl.p.Status == "auto" { // the rule already wrote it: «да» is only an answer
		return w, markItem()
	}
	// In shadow, and for a deferral, the decision is recorded and the question stays open: the journal
	// import answers it. Only an agent's own proposal has no movement the journal could name, so in shadow
	// the owner's answer itself closes it, and the row is for /OS.
	if pl.p.Kind == KindAgent && mode != "live" && pl.d.Kind != Defer {
		if _, err := q.ExecContext(ctx, `UPDATE proposals SET status = 'approved', decided_at = ?, decided_by = ?, decision = ?, decision_text = ?,
			brief_id = ?, brief_item = ? WHERE id = ? AND status = 'open'`, at, by, pl.d.Kind, pl.d.Text, bID, bItem, pl.p.ID); err != nil {
			return w, err
		}
		w.Status = "approved"
		return w, markItem()
	}
	if mode != "live" || pl.d.Kind == Defer {
		if _, err := q.ExecContext(ctx, `UPDATE proposals SET decided_at = ?, decided_by = ?, decision = ?, decision_text = ?,
			brief_id = ?, brief_item = ? WHERE id = ? AND status = 'open'`, at, by, pl.d.Kind, pl.d.Text, bID, bItem, pl.p.ID); err != nil {
			return w, err
		}
		return w, markItem()
	}
	if len(pl.docs) > 0 {
		nums, err := writeRows(ctx, q, pl.p, pl.c, pl.docs, "mark", pl.d.Kind, ref, by, now)
		if err != nil {
			return w, &Refusal{Problems: []string{fmt.Sprintf("%s: %v", pl.label, err)}}
		}
		w.Documents = nums
	}
	for _, n := range pl.nums {
		if err := linkEvents(ctx, q, pl.c, n, "mark", pl.d.Kind, ref, now); err != nil {
			return w, err
		}
		w.Documents = append(w.Documents, n)
	}
	if len(w.Documents) == 0 { // personal with no row: the decision itself closes the movement
		if err := linkEvents(ctx, q, pl.c, 0, "mark", pl.d.Kind, ref, now); err != nil {
			return w, err
		}
	}
	res, err := q.ExecContext(ctx, `UPDATE proposals SET status = 'approved', decided_at = ?, decided_by = ?, decision = ?, decision_text = ?,
		brief_id = ?, brief_item = ? WHERE id = ? AND status = 'open'`, at, by, pl.d.Kind, pl.d.Text, bID, bItem, pl.p.ID)
	if err != nil {
		return w, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return w, &Refusal{Problems: []string{fmt.Sprintf("%s: предложение закрылось во время решения", pl.label)}}
	}
	w.Status = "approved"
	return w, markItem()
}

// linkEvents ties the proposal's events to a journal row (0: a decision without a row).
func linkEvents(ctx context.Context, q ledger.Querier, c Content, num int, how, decision, ref string, now time.Time) error {
	if c.EventID != 0 {
		if _, err := bybit.AddLink(ctx, q, bybit.Link{EventID: c.EventID, Part: c.Part, DocumentNum: num, How: how, Decision: decision, BriefRef: ref}, now); err != nil {
			return err
		}
	}
	for _, id := range c.AlsoEvents {
		if _, err := bybit.AddLink(ctx, q, bybit.Link{EventID: id, DocumentNum: num, How: how, Decision: decision, BriefRef: ref}, now); err != nil {
			return err
		}
	}
	return nil
}

// writeRows appends the rows with their entries and links them to the proposal's events.
func writeRows(ctx context.Context, q ledger.Querier, p Proposal, c Content, docs []bybit.SuggestedDocument, how, decision, ref, by string, now time.Time) ([]int, error) {
	var events []int64
	if c.EventID != 0 {
		events = append(events, c.EventID)
	}
	events = append(events, c.AlsoEvents...)
	written, err := journal.Append(ctx, q, toRows(docs), journal.Write{Author: by, Origin: fmt.Sprintf("proposal:%d.v%d", p.ID, p.Version),
		ProposalID: p.ID, EventIDs: events, Now: now})
	if err != nil {
		return nil, err
	}
	nums := make([]int, 0, len(written))
	for _, d := range written {
		if err := linkEvents(ctx, q, c, d.Num, how, decision, ref, now); err != nil {
			return nil, err
		}
		nums = append(nums, d.Num)
	}
	return nums, nil
}

// AutoRefused says the journal refused what a rule proposed: the question now belongs to the owner and
// a collector must not propose the same thing as exact again.
func (p Proposal) AutoRefused() bool {
	var c struct {
		Refused bool `json:"auto_refused"`
	}
	return json.Unmarshal(p.Content, &c) == nil && c.Refused
}

// ApplyAuto writes an exact match in live mode: the rows, the links and status auto, in q's
// transaction. What the rows cannot be yet (a CinCin top-up without its deposit) stays open; what the
// journal would refuse stays open as a question, so one odd rule never stops a collector.
func ApplyAuto(ctx context.Context, q ledger.Querier, id int64, now time.Time) (bool, error) {
	p, err := GetProposal(ctx, q, id)
	if err != nil {
		return false, err
	}
	if p.Status != "open" {
		return false, nil
	}
	c, err := p.ParseContent()
	if err != nil {
		return false, err
	}
	if c.AwaitDeposit || len(c.Documents) == 0 {
		return false, nil
	}
	if err := checkRows(ctx, q, c, c.Documents); err != nil {
		_, uerr := q.ExecContext(ctx, `UPDATE proposals SET block = 'likely', content = json_set(content, '$.hint', ?, '$.auto_refused', json('true')) WHERE id = ?`,
			"правило сработало, но строка не проходит проверку журнала: "+err.Error(), id)
		return false, uerr
	}
	if _, err := writeRows(ctx, q, p, c, c.Documents, "rule", "auto", "", "rule", now); err != nil {
		return false, err
	}
	_, err = q.ExecContext(ctx, `UPDATE proposals SET status = 'auto', decided_at = ?, decided_by = 'rule', decision = 'auto' WHERE id = ?`, ts.Format(now), id)
	return err == nil, err
}

func init() {
	bybit.LiveHook = func(ctx context.Context, q ledger.Querier, id int64, _ bybit.Content, now time.Time) error {
		_, err := ApplyAuto(ctx, q, id, now)
		return err
	}
}
