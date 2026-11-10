package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A paid day does not change by itself (the owner, plan docs/plans/2026-11-10-shift-reports.md, R2 «Заморозка»). Under
// the reports, when the amount of a day a payout has reached (payable_days.paid > 0) would change, the posted accrual
// stays and the change waits in payroll_pending_changes for the owner: apply reverses the day and posts it anew, dismiss
// closes it with the books as they are. A later change of the same day replaces the open one's new amount.

// Reasons of a pending change.
const (
	ChangePostEdited   = "post_edited"
	ChangeNewPost      = "new_post"
	ChangeTermsChanged = "terms_changed"
)

// States of a pending change.
const (
	stateOpen      = "open"
	stateApplied   = "applied"
	stateDismissed = "dismissed"
)

// decidedByLedger is who closes a change that settled by itself.
const decidedByLedger = "ledger"

// ChangeReasonText says a reason in Russian.
var ChangeReasonText = map[string]string{
	ChangePostEdited:   "пост изменён после выплаты",
	ChangeNewPost:      "отчёт пришёл, когда день стоял по таблице и был выплачен",
	ChangeTermsChanged: "та же выручка, другие ставка или фикс вкладки",
}

// PendingChange is a row of payroll_pending_changes.
type PendingChange struct {
	ID           int64  `json:"id"`
	Person       string `json:"person"`
	Model        string `json:"model"`
	Name         string `json:"name"`
	Date         string `json:"date"`
	PostedMicros int64  `json:"postedMicros"`
	NewMicros    int64  `json:"newMicros"`
	Reason       string `json:"reason"`
	ReasonText   string `json:"reasonText"`
	NoticedAt    string `json:"noticedAt"`
	State        string `json:"state"`
	DecidedBy    string `json:"decidedBy,omitempty"`
	DecidedAt    string `json:"decidedAt,omitempty"`
	Note         string `json:"note,omitempty"`
}

// guardDay is a day of the reports the rebuild is about to change.
type guardDay struct {
	person, model, name, date string
	posted, next              money.Micros
	basis                     *reportBasis
	entries                   []int64
	sources                   []string
}

func (g guardDay) key() string { return g.model + "/" + g.name + ":" + g.date }

// loadGuard reads what the payouts cleared of each day as posted, and the open pending changes.
func (p *reportPlan) loadGuard(ctx context.Context, q ledger.Querier) error {
	p.paid, p.open = map[string]money.Micros{}, map[string]PendingChange{}
	rows, err := q.QueryContext(ctx, `SELECT model, name, date, SUM(paid) FROM payable_days WHERE kind = 'day' AND date >= ?
		GROUP BY model, name, date`, p.from)
	if err != nil {
		return err
	}
	for rows.Next() {
		var model, name, date string
		var paid int64
		if err := rows.Scan(&model, &name, &date, &paid); err != nil {
			rows.Close()
			return err
		}
		if paid > 0 {
			p.paid[model+"/"+name+":"+date] = money.Micros(paid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	open, err := PendingChanges(ctx, q, "", stateOpen)
	if err != nil {
		return err
	}
	for _, c := range open {
		p.open[c.Model+"/"+c.Name+":"+c.Date] = c
	}
	return nil
}

// guard says whether a change of a day waits for the owner: the day is paid, its amount would change, and no applied
// decision covers that change. A day that may change closes its open pending change: applied, when the rebuild now
// follows the report (the day is no longer paid), dismissed, when the report came back to what is posted.
func (p *reportPlan) guard(ctx context.Context, tx *sql.Tx, g guardDay, now time.Time) (bool, error) {
	open, isOpen := p.open[g.key()]
	if p.paid[g.key()] == 0 || g.next == g.posted {
		if isOpen {
			state, note := stateApplied, "день не выплачен: проведён по отчёту"
			if g.next == g.posted {
				state, note = stateDismissed, "отчёт вернулся к проведённой сумме"
			}
			if err := decide(ctx, tx, open.ID, state, decidedByLedger, note, now); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	// The owner applied exactly this change: let it through.
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payroll_pending_changes WHERE model = ? AND name = ? AND date = ?
		AND state = ? AND posted_micros = ? AND new_micros = ? AND decided_by IS NOT ?`, g.model, g.name, g.date, stateApplied,
		int64(g.posted), int64(g.next), decidedByLedger).Scan(&applied); err != nil {
		return false, err
	}
	if applied > 0 {
		return false, nil
	}
	reason, err := changeReason(ctx, tx, g)
	if err != nil {
		return true, err
	}
	at := ts.Format(now)
	if isOpen {
		if open.PostedMicros == int64(g.posted) && open.NewMicros == int64(g.next) {
			return true, nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE payroll_pending_changes SET posted_micros = ?, new_micros = ?, reason = ?, noticed_at = ?
			WHERE id = ?`, int64(g.posted), int64(g.next), reason, at, open.ID)
		return true, err
	}
	// The owner dismissed exactly this change: it does not come back.
	var dismissed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payroll_pending_changes WHERE model = ? AND name = ? AND date = ?
		AND state = ? AND posted_micros = ? AND new_micros = ? AND decided_by IS NOT ?`, g.model, g.name, g.date, stateDismissed,
		int64(g.posted), int64(g.next), decidedByLedger).Scan(&dismissed); err != nil {
		return true, err
	}
	if dismissed > 0 {
		return true, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO payroll_pending_changes(person, model, name, date, posted_micros, new_micros, reason, noticed_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, g.person, g.model, g.name, g.date, int64(g.posted), int64(g.next), reason, at, stateOpen)
	return true, err
}

// changeReason tells a change apart: a day posted from the sheet is a new post; one posted from a report is an edited
// post when the revenue moved (or the report is gone), new terms when it did not.
func changeReason(ctx context.Context, q ledger.Querier, g guardDay) (string, error) {
	for _, s := range g.sources {
		if s != SourceReports {
			return ChangeNewPost, nil
		}
	}
	if g.basis == nil || len(g.entries) == 0 {
		return ChangePostEdited, nil
	}
	var net int64
	err := q.QueryRowContext(ctx, `SELECT net_micros FROM report_accrual_entries WHERE entry_id = ?`, g.entries[len(g.entries)-1]).Scan(&net)
	if errors.Is(err, sql.ErrNoRows) {
		return ChangePostEdited, nil
	}
	if err != nil {
		return "", err
	}
	if money.Micros(net) != g.basis.net {
		return ChangePostEdited, nil
	}
	return ChangeTermsChanged, nil
}

// closePending closes every open change (of a person, or all with ""): the source that made them is off.
func closePending(ctx context.Context, tx *sql.Tx, person, state, note string, now time.Time) error {
	q := `UPDATE payroll_pending_changes SET state = ?, decided_by = ?, decided_at = ?, note = ? WHERE state = ?`
	args := []any{state, decidedByLedger, ts.Format(now), note, stateOpen}
	if person != "" {
		q += ` AND person = ?`
		args = append(args, person)
	}
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

func decide(ctx context.Context, q ledger.Querier, id int64, state, by, note string, now time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE payroll_pending_changes SET state = ?, decided_by = ?, decided_at = ?, note = ? WHERE id = ? AND state = ?`,
		state, by, ts.Format(now), nullText(note), id, stateOpen)
	return err
}

func nullText(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// PendingChanges reads the changes of a person ("" — everybody) in the states given (none — all), oldest day first.
func PendingChanges(ctx context.Context, q ledger.Querier, person string, states ...string) ([]PendingChange, error) {
	query := `SELECT id, person, model, name, date, posted_micros, new_micros, reason, noticed_at, state, COALESCE(decided_by, ''),
		COALESCE(decided_at, ''), COALESCE(note, '') FROM payroll_pending_changes WHERE 1 = 1`
	var args []any
	if person != "" {
		query += ` AND person = ?`
		args = append(args, person)
	}
	if len(states) > 0 {
		query += ` AND state IN (?` + strings.Repeat(", ?", len(states)-1) + `)`
		for _, s := range states {
			args = append(args, s)
		}
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY date, person, model, name, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingChange{}
	for rows.Next() {
		var c PendingChange
		if err := rows.Scan(&c.ID, &c.Person, &c.Model, &c.Name, &c.Date, &c.PostedMicros, &c.NewMicros, &c.Reason, &c.NoticedAt, &c.State,
			&c.DecidedBy, &c.DecidedAt, &c.Note); err != nil {
			return nil, err
		}
		c.ReasonText = ChangeReasonText[c.Reason]
		out = append(out, c)
	}
	return out, rows.Err()
}

// OpenChanges reads the changes waiting for the owner.
func OpenChanges(ctx context.Context, q ledger.Querier, person string) ([]PendingChange, error) {
	return PendingChanges(ctx, q, person, stateOpen)
}

// ChangeRefusal is a decision not taken: no such change, or it is not open.
type ChangeRefusal struct{ Msg string }

func (r *ChangeRefusal) Error() string { return r.Msg }

// Decisions on a pending change.
const (
	DecisionApply   = "apply"
	DecisionDismiss = "dismiss"
)

// DecideResult is the answer of a decision.
type DecideResult struct {
	OK     bool          `json:"ok"`
	Change PendingChange `json:"change"`
	Report Report        `json:"report"`
}

// DecideChange applies or dismisses an open change and rebuilds the books: apply reverses the day and posts what the
// report says (when the report still says it — otherwise the change stays open with the new amount), dismiss leaves
// the books as they are.
func DecideChange(ctx context.Context, store *db.Store, id int64, decision, by, note string, now time.Time) (DecideResult, error) {
	var out DecideResult
	state := map[string]string{DecisionApply: stateApplied, DecisionDismiss: stateDismissed}[decision]
	if state == "" {
		return out, &ChangeRefusal{Msg: "решение: apply или dismiss, а не " + strconv.Quote(decision)}
	}
	if strings.TrimSpace(by) == "" || by == decidedByLedger {
		return out, &ChangeRefusal{Msg: "кто решил: нужно имя ключа"}
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	list, err := PendingChanges(ctx, tx, "")
	if err != nil {
		return out, err
	}
	var c *PendingChange
	for i := range list {
		if list[i].ID == id {
			c = &list[i]
		}
	}
	if c == nil {
		return out, &ChangeRefusal{Msg: fmt.Sprintf("изменения %d нет", id)}
	}
	if c.State != stateOpen {
		return out, &ChangeRefusal{Msg: fmt.Sprintf("изменение %d уже решено: %s (%s %s)", id, c.State, c.DecidedBy, c.DecidedAt)}
	}
	if err := decide(ctx, tx, id, state, by, note, now); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	if out.Report, err = Rebuild(ctx, store, now); err != nil {
		return out, fmt.Errorf("решение записано, пересборка не прошла: %w", err)
	}
	after, err := PendingChanges(ctx, store.RO, "")
	if err != nil {
		return out, err
	}
	for _, x := range after {
		if x.ID == id {
			out.Change = x
		}
	}
	out.OK = true
	return out, nil
}

func init() {
	server.Register(func(s *server.Server) {
		for _, d := range []string{DecisionApply, DecisionDismiss} {
			s.Mux.Handle("POST /v1/payroll/changes/{id}/"+d, auth.Require(s, auth.ScopeDecide, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
				if err != nil || id <= 0 {
					server.WriteError(w, http.StatusBadRequest, "нужен номер изменения: /v1/payroll/changes/<id>/"+d)
					return
				}
				var in struct {
					Note string `json:"note"`
				}
				if r.ContentLength != 0 {
					if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
						server.WriteError(w, http.StatusBadRequest, "тело запроса: нужен JSON {note}")
						return
					}
				}
				out, err := DecideChange(r.Context(), s.Store, id, d, auth.KeyName(r.Context()), in.Note, ts.Now())
				var ref *ChangeRefusal
				switch {
				case errors.As(err, &ref):
					server.WriteError(w, http.StatusUnprocessableEntity, ref.Error())
				case err != nil:
					s.Log.Error("payroll change", "key", auth.KeyName(r.Context()), "err", err)
					server.WriteError(w, http.StatusInternalServerError, "решение не записано: сбой сервиса, подробности в журнале")
				default:
					server.WriteJSON(w, http.StatusOK, out)
				}
			})))
		}
	})
}
