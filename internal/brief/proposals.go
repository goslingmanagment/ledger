// Package brief holds what stands between a collected movement and a journal row: proposals, the
// owner's nightly brief with frozen numbers, decisions bound to the version shown, agents' own
// proposals, evidence and disputes. In shadow mode the journal lives elsewhere and decisions are only
// recorded; in live mode a decision writes the row, its entries and the links in one transaction.
package brief

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Proposal is a row of proposals.
type Proposal struct {
	ID           int64           `json:"id"`
	Key          string          `json:"key"`
	Version      int             `json:"version"`
	Kind         string          `json:"kind"`
	Block        string          `json:"block"`
	Summary      string          `json:"summary"`
	Content      json.RawMessage `json:"content"`
	Status       string          `json:"status"`
	CreatedAt    string          `json:"created_at"`
	Author       string          `json:"author"`
	DecidedAt    string          `json:"decided_at,omitempty"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	Decision     string          `json:"decision,omitempty"`
	DecisionText string          `json:"decision_text,omitempty"`
	BriefID      int64           `json:"brief_id,omitempty"`
	BriefItem    int             `json:"brief_item,omitempty"`
}

// Content is the part of proposals.content this package reads, whatever collector wrote it.
type Content struct {
	Key          string                    `json:"key"`
	EventID      int64                     `json:"event_id"`
	Part         string                    `json:"part"`
	Date         string                    `json:"date"`
	Documents    []bybit.SuggestedDocument `json:"documents"`
	Skeleton     []bybit.SuggestedDocument `json:"skeleton"`
	PersonalDraw bool                      `json:"personal_draw"`
	AwaitDeposit bool                      `json:"await_deposit"`
	Money        *bybit.MoneyCheck         `json:"money"`
	AlsoEvents   []int64                   `json:"also_events"`
	Movement     *bybit.Movement           `json:"movement"`
	Hint         string                    `json:"hint"`
}

// ParseContent reads proposals.content.
func (p Proposal) ParseContent() (Content, error) {
	var c Content
	if err := json.Unmarshal(p.Content, &c); err != nil {
		return c, fmt.Errorf("предложение %d: %w", p.ID, err)
	}
	return c, nil
}

const proposalColumns = `id, key, version, kind, block, summary, content, status, created_at, author, COALESCE(decided_at, ''),
	COALESCE(decided_by, ''), COALESCE(decision, ''), COALESCE(decision_text, ''), COALESCE(brief_id, 0), COALESCE(brief_item, 0)`

func scanProposal(sc interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	var content string
	err := sc.Scan(&p.ID, &p.Key, &p.Version, &p.Kind, &p.Block, &p.Summary, &content, &p.Status, &p.CreatedAt, &p.Author,
		&p.DecidedAt, &p.DecidedBy, &p.Decision, &p.DecisionText, &p.BriefID, &p.BriefItem)
	p.Content = json.RawMessage(content)
	return p, err
}

// GetProposal reads one proposal, sql.ErrNoRows when there is none.
func GetProposal(ctx context.Context, q ledger.Querier, id int64) (Proposal, error) {
	return scanProposal(q.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id))
}

// LatestProposal reads the newest version of a key; ok is false when the key has none.
func LatestProposal(ctx context.Context, q ledger.Querier, key string) (Proposal, bool, error) {
	p, err := scanProposal(q.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE key = ?
		ORDER BY version DESC LIMIT 1`, key))
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	return p, err == nil, err
}

// Input is a proposal as a collector or an agent makes it.
type Input struct {
	Key     string
	Kind    string
	Block   string
	Summary string
	Author  string
	Content any
}

// DecisionChain closes a question the balance chain no longer asks.
const DecisionChain = "chain"

// Change says what Upsert did.
type Change string

// Results of Upsert.
const (
	ChangeNew     Change = "new"     // first version
	ChangeVersion Change = "version" // the open proposal differed: new version, the old one superseded
	ChangeSame    Change = "same"    // the open proposal is exactly this
	ChangeClosed  Change = "closed"  // the key is decided: nothing changes
)

// Upsert keeps one live proposal per key. A changed proposal is a new version, so a decision given for
// the old text is refused. A closed key stays closed.
func Upsert(ctx context.Context, q ledger.Querier, in Input, now time.Time) (Proposal, Change, error) {
	raw, err := json.Marshal(in.Content)
	if err != nil {
		return Proposal{}, "", err
	}
	last, exists, err := LatestProposal(ctx, q, in.Key)
	if err != nil {
		return Proposal{}, "", err
	}
	version := 1
	change := ChangeNew
	if exists {
		// A question the balance chain withdrew may come back with the next late message.
		if last.Status != "open" && !(last.Status == "superseded" && last.Decision == DecisionChain) {
			return last, ChangeClosed, nil
		}
		if last.Status == "open" && last.Kind == in.Kind && last.Block == in.Block && last.Summary == in.Summary && string(last.Content) == string(raw) {
			return last, ChangeSame, nil
		}
		if last.Status == "open" {
			if _, err := q.ExecContext(ctx, `UPDATE proposals SET status = 'superseded', decided_at = ?, decided_by = ?,
				decision = 'new_version', decision_text = ? WHERE id = ?`, ts.Format(now), in.Author,
				fmt.Sprintf("новая версия %d", last.Version+1), last.ID); err != nil {
				return Proposal{}, "", err
			}
		}
		version, change = last.Version+1, ChangeVersion
	}
	res, err := q.ExecContext(ctx, `INSERT INTO proposals(key, version, kind, block, summary, content, status, created_at, author)
		VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?)`, in.Key, version, in.Kind, in.Block, in.Summary, string(raw), ts.Format(now), in.Author)
	if err != nil {
		return Proposal{}, "", fmt.Errorf("предложение %s: %w", in.Key, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Proposal{}, "", err
	}
	p, err := GetProposal(ctx, q, id)
	return p, change, err
}

// Supersede closes an open proposal without a decision of the owner: the fact it asked about is gone.
func Supersede(ctx context.Context, q ledger.Querier, id int64, decision, text, by string, now time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE proposals SET status = 'superseded', decided_at = ?, decided_by = ?, decision = ?,
		decision_text = ? WHERE id = ? AND status = 'open'`, ts.Format(now), by, decision, text, id)
	return err
}
