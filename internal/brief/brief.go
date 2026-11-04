package brief

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// HangingFrom is how many different days a question may be asked before it goes to the tail.
const HangingFrom = 3

// The brief's blocks in their fixed order.
var blockOrder = []string{"auto", "likely", "unknown", "p2p", "non_usdt", "cards"}

var blockTitles = map[string]string{
	"auto": "Записал сам", "likely": "Похоже, проверь", "unknown": "Не разобрался", "p2p": "P2P", "non_usdt": "Не-USDT", "cards": "Карты",
}

// MatchRow is a journal row that may be the movement: a question for the owner, never a link.
type MatchRow struct {
	Num          int    `json:"num"`
	Date         string `json:"date"`
	Type         string `json:"type"`
	Counterparty string `json:"counterparty"`
	Amount       string `json:"amount"`
}

// Item is one numbered point of a brief.
type Item struct {
	Num             int             `json:"num"`
	ProposalID      int64           `json:"proposal_id"`
	ProposalVersion int             `json:"proposal_version"`
	Key             string          `json:"key"`
	Kind            string          `json:"kind"`
	Block           string          `json:"block"`
	Summary         string          `json:"summary"`
	AskedCount      int             `json:"asked_count"`
	FirstAskedAt    string          `json:"first_asked_at"`
	LedgerMatch     string          `json:"ledger_match,omitempty"`
	Decision        string          `json:"decision,omitempty"`
	DecisionText    string          `json:"decision_text,omitempty"`
	DecidedAt       string          `json:"decided_at,omitempty"`
	DecidedBy       string          `json:"decided_by,omitempty"`
	DocumentNums    string          `json:"document_nums,omitempty"`
	Status          string          `json:"status"` // of the proposal: open, auto, approved, superseded…
	ClosedBy        string          `json:"closed_by,omitempty"`
	Hanging         bool            `json:"hanging,omitempty"`
	HangingNights   int             `json:"hanging_nights,omitempty"`
	PriorEvidence   []Evidence      `json:"prior_evidence,omitempty"`
	Content         json.RawMessage `json:"content"`
}

// Live says the item still waits for the owner.
func (it Item) Live() bool { return it.Status == "open" }

// Brief is a numbered list for the owner.
type Brief struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
	Mode      string `json:"mode"`
	Status    string `json:"status"`
	SentAt    string `json:"sent_at,omitempty"`
	ClosedAt  string `json:"closed_at,omitempty"`
	Items     []Item `json:"items"`
}

// Answer is what the brief command returns: the brief, or that there is nothing to ask.
type Answer struct {
	Empty bool   `json:"empty,omitempty"`
	Text  string `json:"text"`
	Brief *Brief `json:"brief,omitempty"`
}

// EmptyText is what an empty brief says.
const EmptyText = "пусто: позиций для брифа нет"

// Options of Make.
type Options struct {
	New    bool // a new brief even when an open unsent one exists
	Author string
}

// displayBlock places a proposal in the brief. Something written by a rule is «записал сам» whatever
// it is; an unwritten exact match in live mode (a CinCin top-up waiting for its deposit) is a question.
func displayBlock(p Proposal, mode string) string {
	if p.Status == "auto" {
		return "auto"
	}
	switch {
	case p.Kind == bybit.KindP2P:
		return "p2p"
	case p.Kind == bybit.KindNonUSDT:
		return "non_usdt"
	case strings.HasPrefix(p.Kind, "card_"):
		return "cards"
	}
	if p.Block == "auto" && mode == "live" {
		return "likely"
	}
	return p.Block
}

func blockRank(b string) int {
	for i, x := range blockOrder {
		if x == b {
			return i
		}
	}
	return len(blockOrder)
}

// askedDays is how many different MSK days the key went to the owner in sent briefs, and the first
// time it did. A draft nobody received is not a question asked.
func askedDays(ctx context.Context, q ledger.Querier, key string) (map[string]bool, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT b.sent_at FROM brief_items i JOIN briefs b ON b.id = i.brief_id
		WHERE i.key = ? AND b.sent_at IS NOT NULL ORDER BY b.sent_at`, key)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	days := map[string]bool{}
	first := ""
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return nil, "", err
		}
		t, err := ts.Parse(at)
		if err != nil {
			return nil, "", fmt.Errorf("briefs.sent_at: %w", err)
		}
		days[ts.Date(t)] = true
		if first == "" {
			first = at
		}
	}
	return days, first, rows.Err()
}

// matchRows finds journal rows the owner may have written already: the same amount, a compatible type,
// the date within a day, not yet tied to another event.
func matchRows(ctx context.Context, q ledger.Querier, c Content) ([]MatchRow, error) {
	var amount, date, direction string
	switch {
	case c.Movement != nil:
		amount, date, direction = c.Movement.Amount, c.Movement.Date, c.Movement.Direction
	case len(c.Documents) > 0:
		amount, date, direction = c.Documents[0].Amount, c.Documents[0].Date, c.Documents[0].Type
	default:
		return nil, nil
	}
	v, err := bybit.AmountMicros(amount)
	if err != nil {
		return nil, nil
	}
	day, err := ts.Parse(date)
	if err != nil {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT num, date, type, counterparty, amount FROM v_documents
		WHERE amount_micros = ? AND type IN (?, 'transfer') AND date BETWEEN ? AND ?
		  AND num NOT IN (SELECT document_num FROM event_links WHERE document_num IS NOT NULL)
		ORDER BY num`, int64(v), direction, ts.Date(day.AddDate(0, 0, -1)), ts.Date(day.AddDate(0, 0, 1)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MatchRow
	for rows.Next() {
		var m MatchRow
		if err := rows.Scan(&m.Num, &m.Date, &m.Type, &m.Counterparty, &m.Amount); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func matchText(rows []MatchRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("#%d", r.Num)
	}
	return strings.Join(parts, ", ")
}

// candidates reads what Make puts into a new brief: every open proposal and what a rule wrote that no
// sent brief has shown yet.
func candidates(ctx context.Context, q ledger.Querier) ([]Proposal, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+proposalColumns+` FROM proposals p WHERE (p.status = 'open' OR (p.status = 'auto'
		AND NOT EXISTS (SELECT 1 FROM brief_items i JOIN briefs b ON b.id = i.brief_id WHERE i.proposal_id = p.id AND b.sent_at IS NOT NULL)))
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Make returns the open unsent brief, or creates one from the open proposals; nil when there is
// nothing to ask. Once created, the numbers of a brief never change: new proposals wait for the next.
func Make(ctx context.Context, q ledger.Querier, opt Options, now time.Time) (*Brief, error) {
	if _, err := bybit.SupersedeLinked(ctx, q, "brief", now); err != nil {
		return nil, err
	}
	if err := closeResolved(ctx, q, now); err != nil {
		return nil, err
	}
	if !opt.New {
		var id int64
		err := q.QueryRowContext(ctx, `SELECT id FROM briefs WHERE status = 'open' AND sent_at IS NULL ORDER BY id DESC LIMIT 1`).Scan(&id)
		if err == nil {
			return Get(ctx, q, id)
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	mode, err := bybit.Mode(ctx, q)
	if err != nil {
		return nil, err
	}
	proposals, err := candidates(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		return nil, nil
	}
	type planned struct {
		p       Proposal
		block   string
		count   int
		first   string
		matches []MatchRow
	}
	today := ts.Date(now)
	items := make([]planned, 0, len(proposals))
	for _, p := range proposals {
		days, first, err := askedDays(ctx, q, p.Key)
		if err != nil {
			return nil, err
		}
		days[today] = true
		if first == "" {
			first = ts.Format(now)
		}
		it := planned{p: p, block: displayBlock(p, mode), count: len(days), first: first}
		if it.block != "cards" && p.Status == "open" {
			c, err := p.ParseContent()
			if err != nil {
				return nil, err
			}
			if it.matches, err = matchRows(ctx, q, c); err != nil {
				return nil, err
			}
		}
		items = append(items, it)
	}
	// Blocks in their order; what hangs goes to the tail, so the owner reads the new first.
	sort.SliceStable(items, func(i, j int) bool {
		hi, hj := items[i].count >= HangingFrom, items[j].count >= HangingFrom
		if hi != hj {
			return hj
		}
		return blockRank(items[i].block) < blockRank(items[j].block)
	})
	if opt.Author == "" {
		opt.Author = "local"
	}
	res, err := q.ExecContext(ctx, `INSERT INTO briefs(created_at, created_by, mode) VALUES (?, ?, ?)`, ts.Format(now), opt.Author, mode)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	for i, it := range items {
		if _, err := q.ExecContext(ctx, `INSERT INTO brief_items(brief_id, num, proposal_id, proposal_version, key, block, summary,
			asked_count, first_asked_at, ledger_match) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, i+1, it.p.ID, it.p.Version, it.p.Key,
			it.block, it.p.Summary, it.count, it.first, matchText(it.matches)); err != nil {
			return nil, err
		}
	}
	if _, err := q.ExecContext(ctx, `UPDATE briefs SET sealed = 1 WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return Get(ctx, q, id)
}

// Get reads a brief with its items, sql.ErrNoRows when there is none.
func Get(ctx context.Context, q ledger.Querier, id int64) (*Brief, error) {
	b := &Brief{ID: id, Items: []Item{}}
	var sent, closed sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT created_at, created_by, mode, status, sent_at, closed_at FROM briefs WHERE id = ?`, id).
		Scan(&b.CreatedAt, &b.CreatedBy, &b.Mode, &b.Status, &sent, &closed); err != nil {
		return nil, err
	}
	b.SentAt, b.ClosedAt = sent.String, closed.String
	rows, err := q.QueryContext(ctx, `SELECT i.num, i.proposal_id, i.proposal_version, i.key, p.kind, i.block, i.summary, i.asked_count,
		i.first_asked_at, i.ledger_match, COALESCE(i.decision, ''), COALESCE(i.decision_text, ''), COALESCE(i.decided_at, ''),
		COALESCE(i.decided_by, ''), i.document_nums, p.status, COALESCE(p.decision, ''), p.content
		FROM brief_items i JOIN proposals p ON p.id = i.proposal_id WHERE i.brief_id = ? ORDER BY i.num`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it Item
		var closedBy, content string
		if err := rows.Scan(&it.Num, &it.ProposalID, &it.ProposalVersion, &it.Key, &it.Kind, &it.Block, &it.Summary, &it.AskedCount,
			&it.FirstAskedAt, &it.LedgerMatch, &it.Decision, &it.DecisionText, &it.DecidedAt, &it.DecidedBy, &it.DocumentNums,
			&it.Status, &closedBy, &content); err != nil {
			rows.Close()
			return nil, err
		}
		it.Content = json.RawMessage(content)
		if !it.Live() && it.Status != "auto" {
			it.ClosedBy = it.Status + ":" + closedBy
		}
		it.Hanging = it.AskedCount >= HangingFrom
		if it.AskedCount > 1 {
			it.HangingNights = it.AskedCount - 1
		}
		b.Items = append(b.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range b.Items {
		if b.Items[i].PriorEvidence, err = EvidenceFor(ctx, q, b.Items[i].Key); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Text is the draft of the message: blocks with frozen numbers, then what hangs. The agent may rephrase
// it, the numbers are the only ones allowed in the message.
func (b *Brief) Text() string {
	mode := "бой"
	if b.Mode != "live" {
		mode = "тень"
	}
	lines := []string{fmt.Sprintf("бриф #%d · %s", b.ID, mode)}
	cur, tail := "", false
	line := func(it Item, what string) {
		var extra []string
		if it.LedgerMatch != "" {
			extra = append(extra, "уже записано тобой? "+it.LedgerMatch+" — закрыть?")
		}
		if it.Hanging {
			extra = append(extra, fmt.Sprintf("висит %d ночей", it.HangingNights))
		}
		if len(it.PriorEvidence) > 0 {
			extra = append(extra, fmt.Sprintf("улик %d", len(it.PriorEvidence)))
		}
		if it.Decision != "" {
			d := "решение: " + it.Decision
			if it.DecisionText != "" {
				d += " «" + it.DecisionText + "»"
			}
			extra = append(extra, d)
		}
		if it.ClosedBy != "" {
			extra = append(extra, "закрыт: "+it.ClosedBy)
		}
		s := fmt.Sprintf("%d. %s", it.Num, what)
		if len(extra) > 0 {
			s += "  [" + strings.Join(extra, "; ") + "]"
		}
		lines = append(lines, s)
	}
	for _, it := range b.Items {
		if it.Hanging {
			if !tail {
				tail = true
				lines = append(lines, "[Висит]")
			}
			line(it, it.Summary)
			continue
		}
		if it.Block != cur {
			cur = it.Block
			title := blockTitles[cur]
			if title == "" {
				title = cur
			}
			if cur == "auto" && b.Mode != "live" {
				title = "Записал сам (тень: в журнал не пишу, подтверди «да»)"
			}
			lines = append(lines, "["+title+"]")
		}
		line(it, it.Summary)
	}
	return strings.Join(lines, "\n")
}

// Respond turns the result of Make into the answer of the command.
func Respond(b *Brief) Answer {
	if b == nil {
		return Answer{Empty: true, Text: EmptyText}
	}
	return Answer{Text: b.Text(), Brief: b}
}

// MarkSent records that the brief reached the owner, once: only sent briefs count as asked.
func MarkSent(ctx context.Context, q ledger.Querier, id int64, now time.Time) (string, error) {
	res, err := q.ExecContext(ctx, `UPDATE briefs SET sent_at = COALESCE(sent_at, ?) WHERE id = ?`, ts.Format(now), id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", &Refusal{Problems: []string{fmt.Sprintf("бриф #%d не найден", id)}}
	}
	var at string
	err = q.QueryRowContext(ctx, `SELECT sent_at FROM briefs WHERE id = ?`, id).Scan(&at)
	return at, err
}

// Pending lists the open briefs that still have something to decide.
func Pending(ctx context.Context, q ledger.Querier) ([]*Brief, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM briefs WHERE status = 'open' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []*Brief{}
	for _, id := range ids {
		b, err := Get(ctx, q, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, it := range b.Items {
			if it.Live() {
				out = append(out, b)
				break
			}
		}
	}
	return out, nil
}

// closeResolved closes open briefs with nothing left to decide. An unsent one with what a rule wrote
// stays: the owner has yet to see it.
func closeResolved(ctx context.Context, q ledger.Querier, now time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE briefs SET status = 'closed', closed_at = ? WHERE status = 'open' AND sealed = 1 AND NOT EXISTS (
		SELECT 1 FROM brief_items i JOIN proposals p ON p.id = i.proposal_id
		WHERE i.brief_id = briefs.id AND (p.status = 'open' OR (p.status = 'auto' AND briefs.sent_at IS NULL)))`, ts.Format(now))
	return err
}
