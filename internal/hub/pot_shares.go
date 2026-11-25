package hub

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// PotShare uses the same representation in the computed day and the immutable posting.
type PotShare struct {
	Pot            string `json:"pot"`
	Name           string `json:"name"`
	Micros         int64  `json:"micros"`
	DistributionID int64  `json:"distributionId,omitempty"`
	Basis          string `json:"basis,omitempty"`
}

type DistributionLine struct {
	PotShare
	Person, Model, Day, Problem string
	held                        bool
}

const activePersonalLines = `SELECT l.* FROM v_report_lines l
 JOIN v_report_pages p ON p.person=l.person AND p.report_date=l.report_date AND p.page=l.page_label
 WHERE l.counted=1 AND ((l.source='hub' AND p.basis='hub') OR (l.source='agent' AND p.basis='post' AND p.report_id=l.report_id))`

// Booked and proposed references survive the replacement of a live version.
const personalRefs = `SELECT transaction_ref AS ref FROM active_lines
 UNION SELECT j.value FROM v_booked_report_days b,json_each(b.sale_refs) j
 UNION SELECT j.value FROM payroll_pending_changes c,json_each(c.sale_refs) j WHERE c.state='open'`

const potModel = `COALESCE((SELECT j.value FROM settings s,json_each(s.value) j WHERE s.key='hub_model_slugs' AND json_valid(s.value) AND j.key=d.model_slug COLLATE NOCASE),
 (SELECT t.model FROM payroll_tabs t WHERE t.model=d.model_slug COLLATE NOCASE LIMIT 1),
 (SELECT d.model_slug FROM hub_pay_terms t WHERE t.model_slug=d.model_slug COLLATE NOCASE LIMIT 1),'')`

func PotLines(ctx context.Context, q ledger.Querier, from string) ([]DistributionLine, error) {
	if from == "" {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `WITH active_lines AS MATERIALIZED (`+activePersonalLines+`), refs AS MATERIALIZED (`+personalRefs+`),
 -- SaleRefsOf in bulk for canonical Hub IDs: compare the ID and its platform reference.
 sale_ids AS MATERIALIZED (
   SELECT ref FROM refs
   UNION SELECT t.key FROM refs JOIN hub_transactions t ON t.ref=refs.ref
 ),
 -- A platform reference in a saved report may arrive before the hourly transaction mirror.
 -- Only pages with such an unmatched reference need to wait for the other identity.
 unresolved_pages AS MATERIALIZED (
   SELECT l.page_label FROM active_lines l WHERE l.source='agent'
     AND NOT EXISTS(SELECT 1 FROM hub_transactions t WHERE t.key=l.transaction_ref OR t.ref=l.transaction_ref)
   UNION
   SELECT l.page_label FROM (
     SELECT b.person,b.day,j.value AS ref FROM v_booked_report_days b,json_each(b.sale_refs) j WHERE b.hub_reports='[]'
     UNION
     SELECT c.person,c.date,j.value FROM payroll_pending_changes c,json_each(c.sale_refs) j WHERE c.state='open'
   ) saved JOIN shift_report_lines l ON l.transaction_ref=saved.ref AND l.counted=1
   JOIN shift_reports r ON r.id=l.report_id AND r.person=saved.person AND r.report_date=saved.day
   WHERE NOT EXISTS(SELECT 1 FROM hub_transactions t WHERE t.key=saved.ref OR t.ref=saved.ref)
 ),
 conflicts AS MATERIALIZED (
   SELECT i.distribution_id,
      min(i.transaction_id) FILTER (WHERE CAST(i.transaction_id AS TEXT) IN sale_ids) AS sale,
      min(i.transaction_id) FILTER (WHERE i.page_label IN unresolved_pages AND NOT EXISTS(
        SELECT 1 FROM hub_transactions t WHERE t.key=CAST(i.transaction_id AS TEXT) AND coalesce(t.ref,'')<>'')) AS unknown
   FROM hub_pot_distribution_inflows i JOIN hub_pot_distributions d ON d.id=i.distribution_id
   WHERE d.day>=? AND d.state='posted' GROUP BY i.distribution_id)
 SELECT d.id,d.pot_code,d.pot_name,`+potModel+`,d.model_slug,d.day,l.value->>'personCode',(l.value->>'baseMills')*1000,
 coalesce(nullif(l.value->>'basis',''), CASE d.formula_kind WHEN 'equal' THEN 'поровну на '||json_array_length(d.lines_json)
 WHEN 'weights' THEN 'по весам' WHEN 'manual' THEN 'вручную' ELSE d.formula_kind END),coalesce(c.sale,0),coalesce(c.unknown,0),
 EXISTS(SELECT 1 FROM hub_pot_distribution_held h WHERE h.distribution_id=d.id)
 FROM hub_pot_distributions d JOIN json_each(d.lines_json) l LEFT JOIN conflicts c ON c.distribution_id=d.id
 WHERE d.day>=? AND d.state='posted' ORDER BY d.day,d.pot_code,d.id,l.key`, from, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DistributionLine
	for rows.Next() {
		var x DistributionLine
		var slug string
		var sale, unknown int64
		if err := rows.Scan(&x.DistributionID, &x.Pot, &x.Name, &x.Model, &slug, &x.Day, &x.Person, &x.Micros, &x.Basis, &sale, &unknown, &x.held); err != nil {
			return nil, err
		}
		switch {
		case x.Model == "":
			x.Problem = "не сопоставлена модель " + slug
		case x.held:
			x.Problem = "раздачу придержал Workboard"
		case sale != 0:
			x.Problem = fmt.Sprintf("продажа %d учтена в отчёте, проводке или ожидающем изменении", sale)
		case unknown != 0:
			x.Problem = fmt.Sprintf("продажа %d: ждём ссылку площадки для сверки со строкой отчёта", unknown)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type PotTab struct{ Person, Model string }

// PotTabHolds returns feed problems for active person/model tabs at the cutover.
// A missing distribution can be the person's first, even on a day without a shift.
func PotTabHolds(ctx context.Context, q ledger.Querier, st PotStatus, person string, now time.Time) (map[PotTab][]string, error) {
	out := map[PotTab][]string{}
	today := ts.Date(now)
	if st.From == "" || today < st.From {
		return out, nil
	}
	why := st.FeedProblem(now)
	if why == "" && !st.Dirty && st.Held == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `WITH mapping AS MATERIALIZED (
        SELECT j.key slug,j.value model FROM settings s,json_each(s.value) j
        WHERE s.key='hub_model_slugs' AND json_valid(s.value)
    ), tabs AS (
        SELECT person,model COLLATE NOCASE AS model FROM payroll_tabs WHERE listed=1 AND person IS NOT NULL
        UNION SELECT person,model FROM payable_days WHERE kind='day' AND date>=?
        UNION SELECT t.person,coalesce(m.model,t.model_slug) FROM hub_pay_terms t
        LEFT JOIN mapping m ON m.slug=t.model_slug COLLATE NOCASE
        WHERE t.gone_at IS NULL AND t.valid_from<=? AND (t.valid_to IS NULL OR t.valid_to>=?)
          AND EXISTS(SELECT 1 FROM settings WHERE key='payroll_terms_hub_from' AND value<>'' AND value<=?)
    ), held AS (
        SELECT DISTINCT h.distribution_id,h.page_labels_json,h.reason,coalesce(m.model,nullif(h.model_slug,''),p.model_slug,d.model_slug) model
        FROM hub_pot_distribution_held h LEFT JOIN json_each(h.page_labels_json) j
        LEFT JOIN hub_pages p ON p.page_label=j.value
        LEFT JOIN hub_pot_distributions d ON d.id=h.distribution_id
        LEFT JOIN mapping m ON m.slug=coalesce(nullif(h.model_slug,''),p.model_slug,d.model_slug) COLLATE NOCASE
    )
    SELECT DISTINCT t.person,t.model,h.distribution_id,h.page_labels_json,h.reason FROM tabs t
    LEFT JOIN held h ON h.model IS NULL OR h.model=t.model COLLATE NOCASE
    WHERE (?='' OR t.person=?) ORDER BY t.person,t.model,h.distribution_id`, st.From, today, st.From, today, person, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k PotTab
		var id *int64
		var pages, reason *string
		if err := rows.Scan(&k.Person, &k.Model, &id, &pages, &reason); err != nil {
			return nil, err
		}
		k.Model = ledger.Slug(k.Model)
		if _, seen := out[k]; !seen {
			if why != "" {
				out[k] = append(out[k], why)
			}
			if st.Dirty {
				out[k] = append(out[k], "раздачи Workboard ожидают пересчёта начислений")
			}
		}
		if id != nil {
			out[k] = append(out[k], fmt.Sprintf("раздача %d, %s: %s", *id, *pages, *reason))
		}
	}
	return out, rows.Err()
}

// PotProblems is shared by health (all people) and callers needing every pot problem as a blocker.
func PotProblems(ctx context.Context, q ledger.Querier, person string, now time.Time) ([]string, error) {
	blockers, _, err := PotPayoutProblems(ctx, q, person, "", now)
	return blockers, err
}

// PotPayoutProblems separates blocking pot problems from non-blocking notes for one person's payout prepare.
// When periodTo is non-empty and strictly before pot_shares_hub_from (all unpaid days of the person are pre-cutover),
// a general K2 feed outage/stale/dirty state is returned as a non-blocking note rather than a payout blocker;
// model-specific held distributions, distribution conflicts, and personal-sale overlaps remain blockers.
func PotPayoutProblems(ctx context.Context, q ledger.Querier, person, periodTo string, now time.Time) (blockers, notes []string, err error) {
	st, err := ReadPotStatus(ctx, q)
	if err != nil || st.From == "" || (person != "" && ts.Date(now) < st.From) {
		return nil, nil, err
	}
	allBeforeCutover := person != "" && periodTo != "" && periodTo < st.From
	const dirtyMsg = "раздачи Workboard ожидают пересчёта начислений"
	generalWhy := st.FeedProblem(now)
	var out []string
	if person != "" {
		tabs, err := PotTabHolds(ctx, q, st, person, now)
		if err != nil {
			return nil, nil, err
		}
		seenBlock := map[string]bool{}
		seenNote := map[string]bool{}
		for _, reasons := range tabs {
			for _, why := range reasons {
				if allBeforeCutover && (why == generalWhy || why == dirtyMsg) {
					if !seenNote[why] {
						notes = append(notes, why)
						seenNote[why] = true
					}
					continue
				}
				if !seenBlock[why] {
					out = append(out, why)
					seenBlock[why] = true
				}
			}
		}
		sort.Strings(out)
		sort.Strings(notes)
	} else {
		if generalWhy != "" {
			out = append(out, generalWhy)
		}
		if st.Dirty {
			out = append(out, dirtyMsg)
		}
		rows, err := q.QueryContext(ctx, `SELECT distribution_id,page_labels_json,reason FROM hub_pot_distribution_held ORDER BY seq`)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id int64
			var pages, why string
			if err := rows.Scan(&id, &pages, &why); err != nil {
				rows.Close()
				return nil, nil, err
			}
			out = append(out, fmt.Sprintf("раздача %d, %s: %s", id, pages, why))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	lines, err := PotLines(ctx, q, st.From)
	if err != nil {
		return nil, nil, err
	}
	for _, l := range lines {
		// Held distributions are already attributed to the tabs of the affected models.
		if person != "" && (l.Person != person || l.held) {
			continue
		}
		if l.Problem == "" {
			if err := q.QueryRowContext(ctx, `SELECT coalesce((SELECT coalesce(a.reason,'') FROM report_accruals a,json_each(a.pots) j
 WHERE a.person=? AND a.model=? COLLATE NOCASE AND a.day=? AND j.value->>'distributionId'=? AND j.value->>'micros'=?),
 'раздача ожидает пересчёта начисления')`, l.Person, l.Model, l.Day, l.DistributionID, l.Micros).Scan(&l.Problem); err != nil {
				return nil, nil, err
			}
		}
		if l.Problem != "" {
			out = append(out, fmt.Sprintf("общак %s, раздача %d, %s за %s: %s", l.Name, l.DistributionID, l.Person, l.Day, l.Problem))
		}
	}
	// A newly personal sale can still be in somebody else's posted pot day awaiting the owner's decision.
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT r.who,i.transaction_id,i.distribution_id FROM v_booked_report_days b,json_each(b.hub_pots) p
 JOIN hub_pot_distribution_inflows i ON i.distribution_id=p.value->>'distributionId'
 JOIN (`+activePersonalLines+`) r ON `+SameSaleSQL("r.transaction_ref", "CAST(i.transaction_id AS TEXT)")+`
 WHERE (?='' OR r.person=?) ORDER BY r.who,i.transaction_id,i.distribution_id`, person, person)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var who string
		var sale, id int64
		if err := rows.Scan(&who, &sale, &id); err != nil {
			return nil, nil, err
		}
		out = append(out, fmt.Sprintf("%s: продажа %d из отчёта остаётся в проведённой раздаче %d", who, sale, id))
	}
	return out, notes, rows.Err()
}

func SetPotSharesFrom(ctx context.Context, store *db.Store, day string, dry bool, now time.Time) error {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := ReadPotStatus(ctx, tx)
	if err != nil {
		return err
	}
	if day == "" {
		if st.From == "" {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM hub_pot_distributions WHERE day>=?`, st.From).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("с %s уже проводились раздачи: остановите раздачи Workboard, сверьте начисления и оформите компенсации", st.From)
		}
	} else {
		if _, err := time.Parse(ts.DateLayout, day); err != nil {
			return fmt.Errorf("нужна дата ГГГГ-ММ-ДД: %w", err)
		}
		if st.From != "" && st.From != day {
			return fmt.Errorf("уже включено с %s; сначала проверьте возможность --off", st.From)
		}
		var sales, shares, unknown int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pot_sales WHERE day>=? AND removed_at IS NULL),(SELECT count(*) FROM v_pot_shares WHERE day>=?),
  (SELECT count(*) FROM hub_pot_distributions d WHERE d.day>=? AND d.state='posted' AND `+potModel+`='')`, day, day, day).Scan(&sales, &shares, &unknown); err != nil {
			return err
		}
		if sales+shares > 0 {
			return fmt.Errorf("с %s в ledger уже есть покупки (%d) или доли (%d)", day, sales, shares)
		}
		if unknown > 0 {
			return fmt.Errorf("не сопоставлены модели раздач: %d", unknown)
		}
	}
	if st.Held > 0 {
		return fmt.Errorf("Workboard придержал раздачи: %d", st.Held)
	}
	if day == "" && st.Dirty {
		return fmt.Errorf("раздачи Workboard ожидают пересчёта начислений")
	}
	if why := st.FeedProblem(now); why != "" {
		return fmt.Errorf("переключение невозможно: %s", why)
	}
	if dry || st.From == day {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settings SET value=?,updated_at=? WHERE key='pot_shares_hub_from'`, day, ts.Format(now)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE hub_pot_sync SET generation=generation+1,dirty=1`); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if AfterPots != nil {
		return AfterPots(ctx, store, now)
	}
	return nil
}
