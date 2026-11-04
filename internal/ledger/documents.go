package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Document is one journal row in the ledger.md format.
type Document struct {
	ID           int64           `json:"id,omitempty"`
	Num          int             `json:"num"`
	Version      int             `json:"version,omitempty"`
	Date         string          `json:"date"`
	Type         string          `json:"type"`
	Category     string          `json:"category"`
	Counterparty string          `json:"counterparty"`
	Model        string          `json:"model"`
	Amount       string          `json:"amount"`
	Currency     string          `json:"currency"`
	Method       string          `json:"method"`
	Status       string          `json:"status"`
	Note         string          `json:"note"`
	Tags         json.RawMessage `json:"tags,omitempty"`
	Incomplete   bool            `json:"incomplete,omitempty"`
	RecordedAt   string          `json:"recorded_at,omitempty"`
	Author       string          `json:"author,omitempty"`
	Origin       string          `json:"origin,omitempty"`
	ProposalID   int64           `json:"proposal_id,omitempty"`
	DisputeID    int64           `json:"dispute_id,omitempty"`
}

// Micros is the parsed amount.
func (d Document) Micros() (money.Micros, error) { return money.Parse(d.Amount) }

// SameContent compares the eleven journal columns.
func (d Document) SameContent(o Document) bool {
	return d.Num == o.Num && d.Date == o.Date && d.Type == o.Type && d.Category == o.Category &&
		d.Counterparty == o.Counterparty && d.Model == o.Model && d.Amount == o.Amount && d.Currency == o.Currency &&
		d.Method == o.Method && d.Status == o.Status && d.Note == o.Note
}

const docColumns = `id, num, version, date, type, category, counterparty, model, amount, currency, method, status,
	note, tags, incomplete, recorded_at, author, origin, COALESCE(proposal_id, 0), COALESCE(dispute_id, 0)`

func scanDocument(sc interface{ Scan(...any) error }) (Document, error) {
	var d Document
	var tags string
	var inc int
	err := sc.Scan(&d.ID, &d.Num, &d.Version, &d.Date, &d.Type, &d.Category, &d.Counterparty, &d.Model, &d.Amount,
		&d.Currency, &d.Method, &d.Status, &d.Note, &tags, &inc, &d.RecordedAt, &d.Author, &d.Origin, &d.ProposalID,
		&d.DisputeID)
	d.Tags, d.Incomplete = json.RawMessage(tags), inc == 1
	return d, err
}

// CurrentDocument returns the newest version of row num, or sql.ErrNoRows.
func CurrentDocument(ctx context.Context, q Querier, num int) (Document, error) {
	return scanDocument(q.QueryRowContext(ctx, `SELECT `+docColumns+` FROM documents WHERE num = ?
		ORDER BY version DESC LIMIT 1`, num))
}

// DocumentsAsOf returns, for every row number, the newest version recorded at or before cut
// (an MSK timestamp; "" means now), ordered by number.
func DocumentsAsOf(ctx context.Context, q Querier, cut string) ([]Document, error) {
	query, args := `SELECT `+docColumns+` FROM v_documents ORDER BY num`, []any{}
	if cut != "" {
		query = `SELECT ` + docColumns + ` FROM documents d WHERE d.version = (
			SELECT max(x.version) FROM documents x WHERE x.num = d.num AND x.recorded_at <= ?) ORDER BY d.num`
		args = []any{cut}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PutDocument stores d as version 1, or as a new version when the journal columns changed.
// It returns the stored document and whether a new version was written.
func PutDocument(ctx context.Context, q Querier, d Document, now time.Time) (Document, bool, error) {
	amount, err := money.Parse(d.Amount)
	if err != nil {
		return d, false, fmt.Errorf("#%d: %w", d.Num, err)
	}
	prev, err := CurrentDocument(ctx, q, d.Num)
	switch {
	case err == sql.ErrNoRows:
		d.Version = 1
	case err != nil:
		return d, false, err
	case prev.SameContent(d):
		return prev, false, nil
	default:
		d.Version = prev.Version + 1
	}
	if d.RecordedAt == "" {
		d.RecordedAt = ts.Format(now)
	}
	if len(d.Tags) == 0 {
		d.Tags = json.RawMessage("{}")
	}
	inc := 0
	if d.Incomplete {
		inc = 1
	}
	res, err := q.ExecContext(ctx, `INSERT INTO documents(num, version, date, type, category, counterparty, model, amount,
		amount_micros, currency, method, status, note, tags, incomplete, recorded_at, author, origin, proposal_id, dispute_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Num, d.Version, d.Date, d.Type, d.Category, d.Counterparty, d.Model, d.Amount, int64(amount), d.Currency,
		d.Method, d.Status, d.Note, string(d.Tags), inc, d.RecordedAt, d.Author, d.Origin, nullID(d.ProposalID),
		nullID(d.DisputeID))
	if err != nil {
		return d, false, fmt.Errorf("#%d: %w", d.Num, err)
	}
	d.ID, _ = res.LastInsertId()
	return d, true, nil
}
