package decisions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Hub's decisions about whose a sale is, as ledger follows them (ecosystem review, round 2; Hub's side is hub#1280). The
// job hub-decisions reads Hub's feed of every change of a decision after a cursor (cursors hub:sale_decisions) with
// ledger's Hub key (read:money) and keeps each decision as it is now in hub_sale_decisions, by Hub's id: a mirror, never
// written by hand. A change Hub holds back from ledger's key (a decision on a page outside the key's pages) stands in
// hub_sale_decisions_held until the key may read it; then Hub places it anew at the end of the feed and it comes by the
// ordinary cursor. Every change carries the decision as it is now, so the mirror never depends on a change's place.
//
// Nothing here moves the books by itself. What reads the mirror does so only for the sales of
// settings.sale_decisions_hub_from and later: the pots follow the decisions of kind pot (internal/pots, AfterHub), a
// report line's agreement is Hub's decision (internal/reports). With the setting empty the mirror is only kept.

// HubJob is the scheduler's name of the job that reads Hub's feed of decisions.
const HubJob = "hub-decisions"

const (
	hubCursor       = "hub:sale_decisions"
	hubStatusCursor = "hub:sale_decisions:status"
	// hubMaxRequests bounds a pass (the next pass goes on).
	hubMaxRequests = 25
)

// hubLimit is how many changes one request asks (tests ask fewer).
var hubLimit = 200

// AfterHub runs at the end of every pass over the feed, whether or not it read anything new — a sale a decision names
// may have reached ledger's mirror of Hub's sales since, the switch day may have been set. internal/pots sets it: the
// pots follow the decisions. What it returns is kept in the pass's status. nil — nothing follows.
var AfterHub func(ctx context.Context, store *db.Store, now time.Time) (any, error)

func init() {
	server.Every(HubJob, 5*time.Minute, func(ctx context.Context, s *server.Server) error {
		cfg, err := hub.ConfigFromEnv()
		if err != nil {
			return err
		}
		_, err = SyncHub(ctx, s.Store, hub.NewClient(cfg, nil), time.Now())
		return err
	})
}

// HubStatus is the last pass over Hub's feed of decisions (cursors hub:sale_decisions:status).
type HubStatus struct {
	At string `json:"at"`
	OK bool   `json:"ok"`
	// Absent: Hub has no feed yet (404, never read before): nothing to read, not a failure.
	Absent   bool   `json:"absent,omitempty"`
	Error    string `json:"error,omitempty"`
	Since    string `json:"since"`
	Next     string `json:"next"`
	Requests int    `json:"requests"`
	Read     int    `json:"read"`    // changes served this pass
	Changed  int    `json:"changed"` // decisions of the mirror written
	Held     int    `json:"held"`    // changes Hub holds back from ledger's key now
	// Pots is what followed the decisions (AfterHub): the purchases added and removed, what waits, what cannot be done.
	Pots     any    `json:"pots,omitempty"`
	LastOKAt string `json:"lastOkAt,omitempty"`
}

// ReadHubStatus reads the last pass; nil — the feed was never read.
func ReadHubStatus(ctx context.Context, q ledger.Querier) (*HubStatus, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, hubStatusCursor).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st HubStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("%s: %w", hubStatusCursor, err)
	}
	return &st, nil
}

func putCursor(ctx context.Context, q ledger.Querier, name, value string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, value, ts.Format(now))
	return err
}

func inTx(ctx context.Context, store *db.Store, fn func(*sql.Tx) error) error {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SyncHub reads Hub's feed of decisions from the cursor on and keeps the mirror, a request's changes and the cursor
// after them in one transaction; then what follows the decisions runs (AfterHub). A Hub without the feed (404) before it
// was ever read is «nothing yet»; after, a failure.
func SyncHub(ctx context.Context, store *db.Store, c *hub.Client, now time.Time) (HubStatus, error) {
	st := HubStatus{At: ts.Format(now)}
	if prev, err := ReadHubStatus(ctx, store.RW); err != nil {
		return st, err
	} else if prev != nil {
		st.LastOKAt = prev.LastOKAt
	}
	var since string
	if err := store.RW.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, hubCursor).Scan(&since); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.Since, st.Next = since, since
	cursor, _ := strconv.ParseInt(since, 10, 64) // "" — from the first
	ask := since
	var readErr error
	for st.Requests < hubMaxRequests {
		ans, err := c.SaleDecisions(ctx, ask, hubLimit)
		st.Requests++
		if err != nil {
			readErr = err
			break
		}
		st.Read += len(ans.Changes)
		err = inTx(ctx, store, func(tx *sql.Tx) error {
			for _, ch := range ans.Changes {
				n, err := keepDecision(ctx, tx, ch, now)
				if err != nil {
					return fmt.Errorf("изменение %d решения %d: %w", ch.Seq, ch.Decision.ID, err)
				}
				st.Changed += n
			}
			if err := keepHeld(ctx, tx, ans, now); err != nil {
				return err
			}
			// The cursor never moves back.
			if next, _ := strconv.ParseInt(ans.Next, 10, 64); next > cursor {
				cursor = next
				st.Next = ans.Next
				return putCursor(ctx, tx, hubCursor, ans.Next, now)
			}
			return nil
		})
		if err != nil {
			return st, err
		}
		// The feed is read to its end when it placed nothing after the place asked from — never when a page serves fewer
		// changes than asked: Hub's `limit` counts places, held ones too, and a held place serves no change.
		from, _ := strconv.ParseInt(ask, 10, 64)
		next, _ := strconv.ParseInt(ans.Next, 10, 64)
		ask = ans.Next
		if next <= from {
			break
		}
	}
	switch {
	case readErr == nil:
		st.OK, st.LastOKAt = true, st.At
	case hub.IsAbsent(readErr) && (since == "" || since == "0"):
		st.OK, st.Absent, st.LastOKAt = true, true, st.At
		st.Error = "хаб ещё не отдаёт решения «чья продажа» (" + hub.SaleDecisionsPath + " — 404)"
		readErr = nil
	case hub.IsAbsent(readErr):
		st.Error = "лента решений «чья продажа» ответила 404 (" + hub.SaleDecisionsPath + "), хотя уже читалась: курсор " + since +
			" — адрес хаба или его выкатка; ничего не прочитано"
	case hub.IsKeyRefused(readErr):
		st.Error = hub.KeyRefusalText(readErr)
	case hub.IsRefusal(readErr):
		st.Error = readErr.Error()
	default:
		return st, readErr // a cancelled pass or the database: the status stays as it was
	}
	if err := store.RW.QueryRowContext(ctx, `SELECT COUNT(*) FROM hub_sale_decisions_held WHERE cleared_at IS NULL`).Scan(&st.Held); err != nil {
		return st, err
	}
	// What follows the decisions, on what the mirror holds now — also when this pass could not read the feed.
	var afterErr error
	if AfterHub != nil {
		st.Pots, afterErr = AfterHub(ctx, store, now)
	}
	b, _ := json.Marshal(st)
	if err := inTx(ctx, store, func(tx *sql.Tx) error { return putCursor(ctx, tx, hubStatusCursor, string(b), now) }); err != nil {
		return st, err
	}
	if readErr != nil {
		return st, errors.New(st.Error)
	}
	if afterErr != nil {
		return st, fmt.Errorf("после решений хаба: %w", afterErr)
	}
	return st, nil
}

// mskTime renders an RFC 3339 time of Hub the way the schema stores times (MSK); what is not a time stays as it came.
func mskTime(v string) string {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return ts.Format(t)
	}
	return v
}

func mskTimePtr(v *string) any {
	if v == nil {
		return nil
	}
	return mskTime(*v)
}

// keepDecision writes the decision of a change as it is now; 1 — the row was written, 0 — it already is that. The
// place kept is the latest read; a change read at an earlier place than that still carries the decision as it is now.
func keepDecision(ctx context.Context, tx *sql.Tx, ch hub.SaleDecisionChange, now time.Time) (int, error) {
	d := ch.Decision
	shares := d.Shares
	if shares == nil {
		shares = []hub.SaleDecisionShare{}
	}
	sh, _ := json.Marshal(shares)
	var ref, appliedPerson, appliedDate any
	if d.TransactionID != nil {
		ref = strconv.FormatInt(*d.TransactionID, 10)
	}
	if d.Applied != nil {
		appliedPerson, appliedDate = d.Applied.Person, d.Applied.ReportDate
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO hub_sale_decisions(id, transaction_ref, page_label, kind, shares, pot, outcome, rule_home,
		state, source, said_by, said_at, decided_at, note, applied_person, applied_report_date, ledger_decision_id, seq, change,
		changed_at, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET transaction_ref = excluded.transaction_ref, page_label = excluded.page_label, kind = excluded.kind,
		  shares = excluded.shares, pot = excluded.pot, outcome = excluded.outcome, rule_home = excluded.rule_home, state = excluded.state,
		  source = excluded.source, said_by = excluded.said_by, said_at = excluded.said_at, decided_at = excluded.decided_at,
		  note = excluded.note, applied_person = excluded.applied_person, applied_report_date = excluded.applied_report_date,
		  ledger_decision_id = excluded.ledger_decision_id,
		  seq = max(hub_sale_decisions.seq, excluded.seq),
		  change = CASE WHEN excluded.seq >= hub_sale_decisions.seq THEN excluded.change ELSE hub_sale_decisions.change END,
		  changed_at = CASE WHEN excluded.seq >= hub_sale_decisions.seq THEN excluded.changed_at ELSE hub_sale_decisions.changed_at END,
		  read_at = excluded.read_at
		WHERE excluded.seq > hub_sale_decisions.seq
		   OR hub_sale_decisions.transaction_ref IS NOT excluded.transaction_ref OR hub_sale_decisions.page_label IS NOT excluded.page_label
		   OR hub_sale_decisions.kind IS NOT excluded.kind OR hub_sale_decisions.shares IS NOT excluded.shares
		   OR hub_sale_decisions.pot IS NOT excluded.pot OR hub_sale_decisions.outcome IS NOT excluded.outcome
		   OR hub_sale_decisions.rule_home IS NOT excluded.rule_home OR hub_sale_decisions.state IS NOT excluded.state
		   OR hub_sale_decisions.source IS NOT excluded.source OR hub_sale_decisions.said_by IS NOT excluded.said_by
		   OR hub_sale_decisions.said_at IS NOT excluded.said_at OR hub_sale_decisions.decided_at IS NOT excluded.decided_at
		   OR hub_sale_decisions.note IS NOT excluded.note OR hub_sale_decisions.applied_person IS NOT excluded.applied_person
		   OR hub_sale_decisions.applied_report_date IS NOT excluded.applied_report_date
		   OR hub_sale_decisions.ledger_decision_id IS NOT excluded.ledger_decision_id`,
		d.ID, ref, d.PageLabel, d.Kind, string(sh), d.Pot, d.Outcome, d.RuleHome, d.State, d.Source, d.SaidBy, mskTimePtr(d.SaidAt),
		mskTime(d.DecidedAt), d.Note, appliedPerson, appliedDate, d.LedgerDecisionID, ch.Seq, ch.Change, mskTime(ch.At), ts.Format(now))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// keepHeld brings hub_sale_decisions_held in line with the answer's list of what Hub holds back: a change newly listed
// is held; one the list no longer names is cleared — Hub released it (it comes at a new place at the end of the feed,
// with the decision as it is now) or no longer has its decision. While the list stays the same nothing is written; an
// answer without the list says nothing.
func keepHeld(ctx context.Context, tx *sql.Tx, ans *hub.SaleDecisionsAnswer, now time.Time) error {
	if ans.Held == nil {
		return nil
	}
	at := ts.Format(now)
	open := map[int64]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT seq FROM hub_sale_decisions_held WHERE cleared_at IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return err
		}
		open[seq] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	listed := map[int64]bool{}
	for _, h := range ans.Held {
		listed[h.Seq] = true
		if open[h.Seq] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_sale_decisions_held(seq, decision_id, page_label, reason, first_seen_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(seq) DO UPDATE SET decision_id = excluded.decision_id, page_label = excluded.page_label, reason = excluded.reason,
			cleared_at = NULL`, h.Seq, h.DecisionID, h.PageLabel, h.Reason, at); err != nil {
			return err
		}
	}
	for seq := range open {
		if listed[seq] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hub_sale_decisions_held SET cleared_at = ? WHERE seq = ?`, at, seq); err != nil {
			return err
		}
	}
	return nil
}

// HubHeldChange is a change of a decision Hub holds back from ledger's key.
type HubHeldChange struct {
	Seq        int64   `json:"seq"`
	DecisionID int64   `json:"decisionId"`
	PageLabel  *string `json:"pageLabel"`
	Reason     string  `json:"reason"`
	Since      string  `json:"since"` // when ledger first saw it held
}

// HubHeld lists the changes held back now, in the feed's order.
func HubHeld(ctx context.Context, q ledger.Querier) ([]HubHeldChange, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, decision_id, page_label, reason, first_seen_at FROM hub_sale_decisions_held
		WHERE cleared_at IS NULL ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HubHeldChange{}
	for rows.Next() {
		var h HubHeldChange
		var page sql.NullString
		if err := rows.Scan(&h.Seq, &h.DecisionID, &page, &h.Reason, &h.Since); err != nil {
			return nil, err
		}
		if page.Valid {
			h.PageLabel = &page.String
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
