package pots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The pots follow Hub (ecosystem review, round 2; Hub's side is hub#1280). «This sale is the pot's» is a decision about
// whose a sale is, and from settings.sale_decisions_hub_from Hub is the one home of those. For a sale of that Moscow day
// or later that ledger's mirror of Hub's sales has (hub_sales), an active decision of kind pot in the mirror of Hub's
// decisions (hub_sale_decisions) is a purchase of that pot — Hub's pot name is the pot's slug here — and nothing else is:
// the job hub-decisions writes the purchase (pot_sales.hub_decision_id, key hub-decisions) and takes it off again when
// the decision is cancelled, superseded or no longer that pot's; `ledger pot add-sale` refuses such a sale and
// `remove-sale` such a purchase. A sale of a day before the switch, and money Hub does not see, stay by hand as they
// were. How a pot is shared and paid does not change: a purchase is a purchase whoever wrote it.
//
// A decision that cannot become a purchase is never skipped silently: an unknown or closed pot, a page or a day outside
// the pot, a sale a report still counts in a person's own page (the guard against paying it twice), a sale already in
// another pot by hand, a sale that never reached hub_sales. They are the problems of the check «решения хаба о продажах
// доходят». With the setting empty nothing here does anything.

// HubKey is the key name of the purchases the job writes and removes.
const HubKey = decisions.HubJob

// hubWaits is how long a decision may wait for its sale to reach ledger's mirror of Hub's sales (the team's facts are
// read every hour) before it is a problem.
const hubWaits = 3 * time.Hour

func init() {
	decisions.AfterHub = func(ctx context.Context, store *db.Store, now time.Time) (any, error) {
		rep, err := FollowHub(ctx, store, now)
		return rep, err
	}
}

// HubPurchase is a purchase the pots owe a decision of Hub.
type HubPurchase struct {
	DecisionID     int64  `json:"decisionId"`
	Pot            string `json:"pot"`
	TransactionRef string `json:"transactionRef"`
	PageLabel      string `json:"pageLabel"`
	Day            string `json:"day"`
	NetMicros      int64  `json:"netMicros"`
	fanRef, at     string
	source         string
}

// HubRemoval is a purchase the job wrote whose decision no longer gives the sale to that pot.
type HubRemoval struct {
	SaleID     int64  `json:"saleId"`
	DecisionID int64  `json:"decisionId"`
	Pot        string `json:"pot"`
	Why        string `json:"why"`
	// Instead is the active decision that gives the same sale to the same pot now (one decision superseded by another
	// of the same pot): the purchase stays and names it. 0 — the purchase goes.
	Instead int64 `json:"instead,omitempty"`
}

// HubProblem is a decision of kind pot that cannot become a purchase, and why.
type HubProblem struct {
	DecisionID     int64  `json:"decisionId"`
	Pot            string `json:"pot"`
	TransactionRef string `json:"transactionRef"`
	Why            string `json:"why"`
}

// HubPlan is what the pots must do to be in line with Hub's decisions, and what they cannot.
type HubPlan struct {
	// From is settings.sale_decisions_hub_from; "" — the pots do not follow Hub and the plan is empty.
	From   string        `json:"from"`
	Kept   int           `json:"kept"` // decisions whose purchase is in place
	Add    []HubPurchase `json:"add"`
	Remove []HubRemoval  `json:"remove"`
	// Waiting are decisions whose sale has not reached hub_sales yet: the next read of the team's facts brings it.
	Waiting  []HubProblem `json:"waiting"`
	Problems []HubProblem `json:"problems"`
}

// FollowReport says what a pass of FollowHub did.
type FollowReport struct {
	From     string       `json:"from"`
	Kept     int          `json:"kept"`
	Added    int          `json:"added"`
	Removed  int          `json:"removed"`
	Waiting  int          `json:"waiting"`
	Problems []HubProblem `json:"problems"`
}

// hubDecision is a decision of the mirror as the plan reads it.
type hubDecision struct {
	id                                 int64
	ref, pot, source, decided, changed string
	readAt                             string
}

// PlanHub compares the pots with Hub's decisions of kind pot and says what to write, what to take off and what cannot
// be done. It reads only.
func PlanHub(ctx context.Context, q ledger.Querier, now time.Time) (HubPlan, error) {
	plan := HubPlan{Add: []HubPurchase{}, Remove: []HubRemoval{}, Waiting: []HubProblem{}, Problems: []HubProblem{}}
	from, err := decisions.HubFrom(ctx, q)
	if err != nil || from == "" {
		return plan, err
	}
	plan.From = from
	// The active decisions of kind pot that name a sale.
	rows, err := q.QueryContext(ctx, `SELECT id, transaction_ref, COALESCE(pot, ''), source, COALESCE(decided_at, ''), changed_at, read_at
		FROM hub_sale_decisions WHERE state = ? AND kind = ? AND transaction_ref IS NOT NULL ORDER BY id`,
		hub.SaleDecisionActive, hub.SaleDecisionPot)
	if err != nil {
		return plan, err
	}
	var active []hubDecision
	byRef := map[string]hubDecision{}
	for rows.Next() {
		var d hubDecision
		if err := rows.Scan(&d.id, &d.ref, &d.pot, &d.source, &d.decided, &d.changed, &d.readAt); err != nil {
			rows.Close()
			return plan, err
		}
		active = append(active, d)
		byRef[d.ref] = d // one active decision a sale; the latest stands
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return plan, err
	}
	// The purchases the job wrote that are in a pot now.
	type made struct {
		id, decision int64
		ref, pot     string
	}
	var mades []made
	rows, err = q.QueryContext(ctx, `SELECT id, hub_decision_id, COALESCE(transaction_ref, ''), pot FROM pot_sales
		WHERE hub_decision_id IS NOT NULL AND removed_at IS NULL ORDER BY id`)
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var m made
		if err := rows.Scan(&m.id, &m.decision, &m.ref, &m.pot); err != nil {
			rows.Close()
			return plan, err
		}
		mades = append(mades, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return plan, err
	}
	stays := map[int64]bool{} // decisions whose purchase is in place, or will be by a purchase that changes its decision
	for _, m := range mades {
		cur, ok := byRef[m.ref]
		switch {
		case ok && cur.id == m.decision && cur.pot == m.pot:
			stays[cur.id] = true
			plan.Kept++
		case ok && cur.pot == m.pot:
			// Another decision gives the same sale to the same pot: the purchase stays and names the new one.
			stays[cur.id] = true
			plan.Remove = append(plan.Remove, HubRemoval{SaleID: m.id, DecisionID: m.decision, Pot: m.pot, Instead: cur.id,
				Why: fmt.Sprintf("решение хаба %d заменено решением %d о том же общаке", m.decision, cur.id)})
		default:
			why, err := whyNotPot(ctx, q, m.decision, m.ref, m.pot)
			if err != nil {
				return plan, err
			}
			plan.Remove = append(plan.Remove, HubRemoval{SaleID: m.id, DecisionID: m.decision, Pot: m.pot, Why: why})
		}
	}
	removing := map[int64]bool{}
	for _, r := range plan.Remove {
		if r.Instead == 0 {
			removing[r.SaleID] = true
		}
	}
	for _, d := range active {
		if stays[d.id] {
			continue
		}
		problem := func(format string, args ...any) HubProblem {
			return HubProblem{DecisionID: d.id, Pot: d.pot, TransactionRef: d.ref, Why: fmt.Sprintf(format, args...)}
		}
		// Hub keeps one active decision a sale; should it ever serve two, the purchase goes by the later one, and it says so.
		if latest := byRef[d.ref]; latest.id != d.id {
			plan.Problems = append(plan.Problems, problem("у продажи %s два действующих решения хаба «в общак» (%d и %d): покупка идёт по "+
				"позднему, раннее отмените в хабе", d.ref, d.id, latest.id))
			continue
		}
		sale, err := fromHub(ctx, q, d.ref)
		if err != nil {
			return plan, err
		}
		if !sale.ok {
			// The sale is not in the mirror of Hub's sales. A decision made or changed since the switch day waits for the
			// next read of the team's facts, and not for long; an older one is about a sale of before the switch.
			if !decisions.HubRules(from, dayOf(d.decided)) && !decisions.HubRules(from, dayOf(d.changed)) {
				continue
			}
			p := problem("продажи %s нет в фактах хаба (hub_sales): её приносит часовое чтение фактов — ledger hub team-sync", d.ref)
			if read, err := ts.Parse(d.readAt); err == nil && now.Sub(read) > hubWaits {
				p.Why = fmt.Sprintf("продажи %s нет в фактах хаба (hub_sales) больше %d часов после решения: страница продажи вне ключа ledger, "+
					"продажа ушла из хаба, или факты не читаются — ledger hub status", d.ref, int(hubWaits.Hours()))
				plan.Problems = append(plan.Problems, p)
			} else {
				plan.Waiting = append(plan.Waiting, p)
			}
			continue
		}
		day := sale.at[:10]
		if !decisions.HubRules(from, day) {
			continue // a sale of a day before the switch: by hand, as it was
		}
		if d.pot == "" {
			plan.Problems = append(plan.Problems, problem("решение хаба %d отдаёт продажу %s в общак, но не называет общак: запишите его "+
				"заново с --pot <код общака ledger>", d.id, d.ref))
			continue
		}
		// The sale may be in a pot already. A purchase the job wrote for another decision is settled above (it stays and
		// changes its decision, or goes in this pass); what is left here was written by hand.
		var other int64
		var otherPot string
		err = q.QueryRowContext(ctx, `SELECT id, pot FROM pot_sales WHERE transaction_ref = ? AND removed_at IS NULL`, d.ref).Scan(&other, &otherPot)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return plan, err
		case removing[other]:
			// The purchase another decision made goes in this pass: this decision's purchase takes its place.
		case otherPot == d.pot:
			plan.Kept++ // in the pot already, by hand
			continue
		default:
			plan.Problems = append(plan.Problems, problem("продажа %s уже в общаке %s (покупка %d, записана вручную), а решение хаба %d "+
				"отдаёт её общаку %s: снимите покупку — ledger pot remove-sale %d", d.ref, otherPot, other, d.id, d.pot, other))
			continue
		}
		p, err := get(ctx, q, d.pot)
		if errors.Is(err, ErrNotFound) {
			plan.Problems = append(plan.Problems, problem("общака «%s» нет в ledger: название общака в хабе — код общака ledger (ledger pot list); "+
				"объявите общак (ledger pot declare) или запишите решение с верным кодом", d.pot))
			continue
		}
		if err != nil {
			return plan, err
		}
		if p.Status != StatusOpen {
			plan.Problems = append(plan.Problems, problem("общак %s закрыт: покупку по решению хаба %d (продажа %s) не принимает", d.pot, d.id, d.ref))
			continue
		}
		if sale.net <= 0 {
			plan.Problems = append(plan.Problems, problem("нетто продажи %s — %s: в общак идёт только покупка больше нуля", d.ref,
				money.Micros(sale.net).Cents()))
			continue
		}
		if err := p.inPot(sale.page, day); err != nil {
			var r *Refusal
			if !errors.As(err, &r) {
				return plan, err
			}
			plan.Problems = append(plan.Problems, problem("решение хаба %d, продажа %s: %s", d.id, d.ref, r.Msg))
			continue
		}
		counted, err := countedIn(ctx, q, d.ref)
		if err != nil {
			return plan, err
		}
		if counted != "" {
			plan.Problems = append(plan.Problems, problem("%s: пока отчёт считает её человеку, в общак %s она не берётся (иначе оплачена дважды) — "+
				"нужна версия отчёта без неё", counted, d.pot))
			continue
		}
		source := SourceAgent
		if d.source == "dispute" {
			source = SourceOwner
		}
		plan.Add = append(plan.Add, HubPurchase{DecisionID: d.id, Pot: d.pot, TransactionRef: d.ref, PageLabel: sale.page, Day: day,
			NetMicros: sale.net, fanRef: sale.fanRef, at: sale.at, source: source})
	}
	return plan, nil
}

// dayOf is the Moscow day of a stored time; "" — none.
func dayOf(at string) string {
	if len(at) < 10 {
		return ""
	}
	return at[:10]
}

// whyNotPot says why a purchase the job wrote no longer has its decision.
func whyNotPot(ctx context.Context, q ledger.Querier, decision int64, ref, pot string) (string, error) {
	var state, kind string
	var dRef, dPot sql.NullString
	err := q.QueryRowContext(ctx, `SELECT state, kind, transaction_ref, pot FROM hub_sale_decisions WHERE id = ?`, decision).
		Scan(&state, &kind, &dRef, &dPot)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Sprintf("решения хаба %d больше нет", decision), nil
	case err != nil:
		return "", err
	case state == hub.SaleDecisionCancelled:
		return fmt.Sprintf("решение хаба %d отменено", decision), nil
	case state == hub.SaleDecisionSuperseded:
		return fmt.Sprintf("решение хаба %d заменено другим", decision), nil
	}
	return fmt.Sprintf("решение хаба %d больше не отдаёт продажу %s общаку %s", decision, ref, pot), nil
}

// FollowHub brings the pots in line with Hub's decisions of kind pot, in one transaction, and recomputes the accruals
// when a purchase was written or taken off. Idempotent; nothing while settings.sale_decisions_hub_from is empty.
func FollowHub(ctx context.Context, store *db.Store, now time.Time) (FollowReport, error) {
	rep := FollowReport{Problems: []HubProblem{}}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	plan, err := PlanHub(ctx, tx, now)
	if err != nil {
		return rep, err
	}
	rep.From, rep.Kept, rep.Waiting, rep.Problems = plan.From, plan.Kept, len(plan.Waiting), plan.Problems
	at := ts.Format(now)
	for _, r := range plan.Remove {
		if r.Instead != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE pot_sales SET hub_decision_id = ?, note = ? WHERE id = ?`, r.Instead,
				hubNote(r.Instead), r.SaleID); err != nil {
				return rep, err
			}
			rep.Kept++
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pot_sales SET removed_at = ?, removed_note = ?, removed_by = ? WHERE id = ? AND removed_at IS NULL`,
			at, r.Why, HubKey, r.SaleID); err != nil {
			return rep, err
		}
		rep.Removed++
	}
	for _, a := range plan.Add {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pot_sales(pot, transaction_ref, page_label, fan_ref, fan_name, net_micros, occurred_at, day,
			source, note, added_at, key_name, hub_decision_id) VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.Pot, a.TransactionRef, a.PageLabel, a.fanRef, a.NetMicros, a.at, a.Day, a.source, hubNote(a.DecisionID), at, HubKey,
			a.DecisionID); err != nil {
			return rep, fmt.Errorf("покупка по решению хаба %d: %w", a.DecisionID, err)
		}
		rep.Added++
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	if rep.Added+rep.Removed > 0 {
		if err := Changed(ctx, store, now); err != nil {
			return rep, fmt.Errorf("покупки общаков записаны, начисления не пересчитаны: %w", err)
		}
	}
	return rep, nil
}

func hubNote(decision int64) string { return fmt.Sprintf("решение хаба %d", decision) }

// hubOnly says why a sale Hub sees is not put in a pot by hand; "" — it may be. From the switch day's sales on only
// Hub's decision gives a sale to a pot.
func hubOnly(ctx context.Context, q ledger.Querier, ref, slug string) (string, error) {
	from, err := decisions.HubFrom(ctx, q)
	if err != nil || from == "" {
		return "", err
	}
	sale, err := fromHub(ctx, q, ref)
	if err != nil || !sale.ok || !decisions.HubRules(from, sale.at[:10]) {
		return "", err
	}
	return fmt.Sprintf("продажу хаба %s (%s) с %s в общак отдаёт только решение в хабе: hub sale decide --sale %s --kind pot --pot %s … — "+
		"покупку запишет джоб %s; ledger pot add-sale — для денег, которых хаб не видит, и продаж раньше этого дня",
		ref, sale.at[:10], from, ref, slug, HubKey), nil
}

// hubKeeps says why a purchase is not taken off by hand; "" — it may be. A purchase the job wrote goes when its
// decision does: taken off by hand it would be written again on the next pass.
func hubKeeps(ctx context.Context, q ledger.Querier, s Sale) (string, error) {
	if s.HubDecisionID == nil || s.TransactionRef == nil {
		return "", nil
	}
	from, err := decisions.HubFrom(ctx, q)
	if err != nil || from == "" {
		return "", err
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM hub_sale_decisions WHERE id = ? AND state = ? AND kind = ? AND transaction_ref = ?
		AND pot = ?`, *s.HubDecisionID, hub.SaleDecisionActive, hub.SaleDecisionPot, *s.TransactionRef, s.Pot).Scan(&n); err != nil || n == 0 {
		return "", err
	}
	return fmt.Sprintf("покупку %d записал джоб %s по решению хаба %d: она снимается отменой или заменой решения в хабе "+
		"(hub sale cancel %d), не вручную", s.ID, HubKey, *s.HubDecisionID, *s.HubDecisionID), nil
}
