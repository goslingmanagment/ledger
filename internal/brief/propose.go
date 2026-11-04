package brief

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// KindAgent is the kind of an agent's own proposal.
const KindAgent = "agent"

// AgentProposal is what an agent proposes for money no collector sees: cash and the like.
type AgentProposal struct {
	Key       string                    `json:"key"`
	Summary   string                    `json:"summary"`
	Block     string                    `json:"block"` // likely (default) or unknown: never auto
	Hint      string                    `json:"hint"`
	Documents []bybit.SuggestedDocument `json:"documents"`
	Evidence  string                    `json:"evidence"`
}

// agentContent is proposals.content of an agent's proposal.
type agentContent struct {
	Key       string                    `json:"key"`
	EventID   int64                     `json:"event_id"`
	Date      string                    `json:"date,omitempty"`
	Documents []bybit.SuggestedDocument `json:"documents"`
	Hint      string                    `json:"hint,omitempty"`
}

func describe(d bybit.SuggestedDocument) string {
	verb := "заплатил"
	if d.Type == "income" {
		verb = "пришло"
	} else if d.Type == "transfer" {
		verb = "перевёл"
	}
	return fmt.Sprintf("%s %s %s — %s (%s) (%s)", verb, d.Amount, d.Currency, d.Counterparty, d.Category, d.Date)
}

// Propose stores an agent's proposal: it goes through the same brief as a collector's. Its rows are
// checked now, so the owner's «да» cannot fail at night. The same text again changes nothing, another
// text under the same key is a new version.
func Propose(ctx context.Context, q ledger.Querier, in AgentProposal, by string, now time.Time) (Proposal, Change, error) {
	if len(in.Documents) == 0 {
		return Proposal{}, "", refuse("нужна предложенная строка: documents")
	}
	if in.Block == "" {
		in.Block = "likely"
	}
	if in.Block != "likely" && in.Block != "unknown" {
		return Proposal{}, "", refuse("block — likely или unknown: само пишет только точное правило сборщика")
	}
	for i := range in.Documents {
		d := &in.Documents[i]
		if d.Status == "" {
			d.Status = "paid"
		}
		if d.Model == "" {
			d.Model = "—"
		}
		if d.Date == "" {
			d.Date = ts.Date(now)
		}
		if v, err := money.Parse(d.Amount); err != nil || v <= 0 {
			return Proposal{}, "", refuse("строка %d: сумма «%s»", i+1, d.Amount)
		}
	}
	c := Content{Documents: in.Documents}
	if err := checkRows(ctx, q, c, in.Documents); err != nil {
		return Proposal{}, "", refuse("строка не проходит проверку журнала: %v", err)
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return Proposal{}, "", err
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])[:12]
	key := strings.TrimSpace(in.Key)
	if key == "" {
		key = digest
	}
	if _, isMovement := bybit.ParseKey(key); isMovement {
		return Proposal{}, "", refuse("ключ %s занят сборщиком: движение Bybit или событие бота предлагаются сами", key)
	}
	if !strings.HasPrefix(key, "agent:") {
		key = "agent:" + key
	}
	// The raw fact of an agent's proposal is the proposal itself.
	eventID, _, err := ledger.AddEvent(ctx, q, ledger.Event{Source: "manual", SourceID: key + "#" + digest, Kind: "proposal",
		OccurredAt: ts.Format(now), Payload: payload}, now)
	if err != nil {
		return Proposal{}, "", err
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = describe(in.Documents[0])
	}
	if strings.TrimSpace(in.Evidence) != "" {
		if _, err := AddEvidence(ctx, q, Evidence{Key: key, Source: "agent", Text: in.Evidence, Author: by}, now); err != nil {
			return Proposal{}, "", err
		}
	}
	return Upsert(ctx, q, Input{Key: key, Kind: KindAgent, Block: in.Block, Summary: summary, Author: by, Content: agentContent{Key: key,
		EventID: eventID, Date: in.Documents[0].Date, Documents: in.Documents, Hint: in.Hint}}, now)
}

// Evidence is what an investigation found about a movement.
type Evidence struct {
	Key       string `json:"key,omitempty"`
	Source    string `json:"source"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
	Author    string `json:"author,omitempty"`
}

// AddEvidence stores a fact once per (key, source, text); added is false for a repeat.
func AddEvidence(ctx context.Context, q ledger.Querier, e Evidence, now time.Time) (bool, error) {
	if strings.TrimSpace(e.Key) == "" || strings.TrimSpace(e.Text) == "" {
		return false, refuse("улике нужны ключ и текст")
	}
	if e.Source == "" {
		e.Source = "agent"
	}
	if e.CreatedAt == "" {
		e.CreatedAt = ts.Format(now)
	}
	res, err := q.ExecContext(ctx, `INSERT INTO evidence(key, source, text, created_at, author) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key, source, text) DO NOTHING`, e.Key, e.Source, e.Text, e.CreatedAt, e.Author)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// EvidenceFor lists what is known about a key, oldest first.
func EvidenceFor(ctx context.Context, q ledger.Querier, key string) ([]Evidence, error) {
	rows, err := q.QueryContext(ctx, `SELECT source, text, created_at, author FROM evidence WHERE key = ? ORDER BY created_at, id`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		if err := rows.Scan(&e.Source, &e.Text, &e.CreatedAt, &e.Author); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EvidenceReport counts what ImportEvidence did.
type EvidenceReport struct {
	New   int `json:"new"`
	Known int `json:"known"`
}

// ImportEvidence copies the evidence table of a ledger-sync state file. Repeatable.
func ImportEvidence(ctx context.Context, q ledger.Querier, st *sql.DB, now time.Time) (EvidenceReport, error) {
	var rep EvidenceReport
	var one int
	err := st.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'evidence'`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return rep, nil // a state file older than the table
	}
	if err != nil {
		return rep, err
	}
	rows, err := st.QueryContext(ctx, `SELECT key, source, text, created_ms FROM evidence ORDER BY created_ms, id`)
	if err != nil {
		return rep, fmt.Errorf("evidence: %w", err)
	}
	var list []Evidence
	for rows.Next() {
		var e Evidence
		var ms int64
		if err := rows.Scan(&e.Key, &e.Source, &e.Text, &ms); err != nil {
			rows.Close()
			return rep, fmt.Errorf("evidence: %w", err)
		}
		e.CreatedAt, e.Author = ts.Format(time.UnixMilli(ms)), "import:ledger-sync"
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("evidence: %w", err)
	}
	for _, e := range list {
		added, err := AddEvidence(ctx, q, e, now)
		if err != nil {
			return rep, err
		}
		if added {
			rep.New++
		} else {
			rep.Known++
		}
	}
	return rep, nil
}
