package brief

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Dispute is a row of disputes.
type Dispute struct {
	ID         int64           `json:"id"`
	OpenedAt   string          `json:"opened_at"`
	OpenedBy   string          `json:"opened_by"`
	Subject    string          `json:"subject"`
	Account    string          `json:"account,omitempty"`
	Amount     string          `json:"amount,omitempty"`
	Evidence   string          `json:"evidence,omitempty"`
	Status     string          `json:"status"`
	Resolution json.RawMessage `json:"resolution,omitempty"`
	ResolvedAt string          `json:"resolved_at,omitempty"`
	ResolvedBy string          `json:"resolved_by,omitempty"`
}

// OpenInput is what `ledger dispute open` says.
type OpenInput struct {
	Subject  string `json:"subject"`
	Account  string `json:"account"`
	Amount   string `json:"amount"`
	Evidence string `json:"evidence"`
}

// OpenDispute records that something is disputed. The old stays as it was until the owner resolves it.
func OpenDispute(ctx context.Context, q ledger.Querier, in OpenInput, by string, now time.Time) (Dispute, error) {
	if strings.TrimSpace(in.Subject) == "" {
		return Dispute{}, refuse("спору нужен предмет: --subject")
	}
	var account, amount any
	if in.Account != "" {
		if err := ledger.EnsureAccount(ctx, q, in.Account, ts.Date(now)); err != nil {
			return Dispute{}, refuse("счёт: %v", err)
		}
		account = in.Account
	}
	if in.Amount != "" {
		v, err := money.Parse(in.Amount)
		if err != nil {
			return Dispute{}, refuse("сумма: %v", err)
		}
		amount = int64(v)
	}
	res, err := q.ExecContext(ctx, `INSERT INTO disputes(opened_at, opened_by, subject, account, amount, evidence) VALUES (?, ?, ?, ?, ?, ?)`,
		ts.Format(now), by, in.Subject, account, amount, in.Evidence)
	if err != nil {
		return Dispute{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Dispute{}, err
	}
	return GetDispute(ctx, q, id)
}

const disputeColumns = `id, opened_at, opened_by, subject, COALESCE(account, ''), amount, evidence, status, resolution, COALESCE(resolved_at, ''), COALESCE(resolved_by, '')`

func scanDispute(sc interface{ Scan(...any) error }) (Dispute, error) {
	var d Dispute
	var amount sql.NullInt64
	var resolution sql.NullString
	err := sc.Scan(&d.ID, &d.OpenedAt, &d.OpenedBy, &d.Subject, &d.Account, &amount, &d.Evidence, &d.Status, &resolution, &d.ResolvedAt, &d.ResolvedBy)
	if amount.Valid {
		d.Amount = money.Micros(amount.Int64).String()
	}
	if resolution.Valid {
		if json.Valid([]byte(resolution.String)) {
			d.Resolution = json.RawMessage(resolution.String)
		} else {
			d.Resolution, _ = json.Marshal(resolution.String)
		}
	}
	return d, err
}

// GetDispute reads one dispute.
func GetDispute(ctx context.Context, q ledger.Querier, id int64) (Dispute, error) {
	return scanDispute(q.QueryRowContext(ctx, `SELECT `+disputeColumns+` FROM disputes WHERE id = ?`, id))
}

// ListDisputes lists disputes, newest first; status "" means all of them.
func ListDisputes(ctx context.Context, q ledger.Querier, status string) ([]Dispute, error) {
	query, args := `SELECT `+disputeColumns+` FROM disputes`, []any{}
	if status != "" {
		query, args = query+` WHERE status = ?`, []any{status}
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dispute{}
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RowPatch is a corrected row: with a number, fields over its current version; without one, a whole new row.
type RowPatch struct {
	Num *int `json:"num"`
	Patch
}

// ParseRowPatches reads the JSON of --document: one corrected row or a list of them.
func ParseRowPatches(text string) ([]RowPatch, error) {
	raw := bytes.TrimSpace([]byte(text))
	if len(raw) > 0 && raw[0] == '[' {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		out := make([]RowPatch, 0, len(list))
		for _, item := range list {
			p, err := parseRowPatch(item)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, nil
	}
	p, err := parseRowPatch(raw)
	if err != nil {
		return nil, err
	}
	return []RowPatch{p}, nil
}

func parseRowPatch(raw []byte) (RowPatch, error) {
	var p RowPatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, err
	}
	if len(p.Split) > 0 {
		return p, errors.New("split в споре не нужен: несколько строк — список")
	}
	return p, nil
}

func docOfRow(r journal.Row) bybit.SuggestedDocument {
	return bybit.SuggestedDocument{Date: r.Date, Type: r.Type, Category: r.Category, Counterparty: r.Counterparty, Model: r.Model,
		Amount: r.Amount, Currency: r.Currency, Method: r.Method, Status: r.Status, Note: r.Note}
}

// ResolveInput is what the owner decides about a dispute.
type ResolveInput struct {
	Documents []RowPatch `json:"documents"`
	Reject    bool       `json:"reject"`
	Note      string     `json:"note"`
}

// Resolution is the record of a correction.
type Resolution struct {
	Note        string               `json:"note,omitempty"`
	Corrections []journal.Correction `json:"corrections,omitempty"`
	NewRows     []int                `json:"new_rows,omitempty"`
}

// ResolveDispute closes a dispute. A correction writes a new version of each row, reverses its old
// entries and posts new ones, all carrying the dispute, in the caller's one transaction. In shadow the
// journal lives elsewhere, so a correction there is refused and the dispute stays open.
func ResolveDispute(ctx context.Context, q ledger.Querier, id int64, in ResolveInput, by string, now time.Time) (Dispute, error) {
	d, err := GetDispute(ctx, q, id)
	if errors.Is(err, sql.ErrNoRows) {
		return d, refuse("спор %d не найден", id)
	}
	if err != nil {
		return d, err
	}
	if d.Status != "open" {
		return d, refuse("спор %d уже закрыт (%s)", id, d.Status)
	}
	if in.Reject == (len(in.Documents) > 0) {
		return d, refuse("нужно одно: --document с исправленной строкой или --reject")
	}
	at := ts.Format(now)
	if in.Reject {
		_, err := q.ExecContext(ctx, `UPDATE disputes SET status = 'rejected', resolution = ?, resolved_at = ?, resolved_by = ? WHERE id = ?`,
			in.Note, at, by, id)
		if err != nil {
			return d, err
		}
		return GetDispute(ctx, q, id)
	}
	mode, err := bybit.Mode(ctx, q)
	if err != nil {
		return d, err
	}
	if mode != "live" {
		return d, refuse("режим %s: канон журнала в /OS, здесь строки не исправляются. Исправь ledger.md, импорт подтянет; спор закрой --reject с пояснением", mode)
	}
	res := Resolution{Note: in.Note}
	var problems []string
	w := journal.Write{Author: by, Origin: fmt.Sprintf("dispute:%d", id), DisputeID: id, Now: now}
	var fresh []journal.Row
	for _, p := range in.Documents {
		if p.Num == nil {
			fresh = append(fresh, toRows([]bybit.SuggestedDocument{apply(p.Patch, bybit.SuggestedDocument{Status: "paid", Model: "—"})})...)
			continue
		}
		cur, err := ledger.CurrentDocument(ctx, q, *p.Num)
		if errors.Is(err, sql.ErrNoRows) {
			problems = append(problems, fmt.Sprintf("строки #%d нет", *p.Num))
			continue
		}
		if err != nil {
			return d, err
		}
		base := journal.RowFromDocument(cur)
		rows := toRows([]bybit.SuggestedDocument{apply(p.Patch, docOfRow(base))})
		rows[0].Num = base.Num
		if rows[0].Format() == base.Format() {
			problems = append(problems, fmt.Sprintf("#%d: исправление ничего не меняет", base.Num))
			continue
		}
		c, err := journal.Correct(ctx, q, rows[0], w)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		res.Corrections = append(res.Corrections, c)
	}
	if len(fresh) > 0 && len(problems) == 0 {
		written, err := journal.Append(ctx, q, fresh, w)
		if err != nil {
			problems = append(problems, err.Error())
		}
		for _, x := range written {
			res.NewRows = append(res.NewRows, x.Num)
		}
	}
	if len(problems) > 0 {
		return d, &Refusal{Problems: problems}
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return d, err
	}
	if _, err := q.ExecContext(ctx, `UPDATE disputes SET status = 'resolved', resolution = ?, resolved_at = ?, resolved_by = ? WHERE id = ?`,
		string(raw), at, by, id); err != nil {
		return d, err
	}
	return GetDispute(ctx, q, id)
}
