package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// hubFlags adds payout prepare flags from Hub's mirrors (decisions, handed-in shift reports, report pages), scoped to
// what can change this person's amount. Runs even when the person has zero balance (Nothing != ""), because a disputed
// line or unapplied Hub decision can still change what they are owed.
//
// Scoped checks (disputed, pending_add, board_post, post_day_add) inspect the payout period, or — when there is no
// period — from the person's last payout (or payroll_reports_from if never paid) through today.
// Active Hub decisions (hub_decision) are searched by state (not applied / not counted) across all sales on or after
// settings.sale_decisions_hub_from, so a late decision for an already-paid period still surfaces. Frozen ledger tables
// (sale_decisions, sale_questions) are never flagged.
func hubFlags(ctx context.Context, q ledger.Querier, b *PayoutBrief, card Person, today string, f *flagger) error {
	winFrom, winTo, err := hubFlagWindow(ctx, q, b, today)
	if err != nil {
		return err
	}
	hubFrom, err := HubFrom(ctx, q)
	if err != nil {
		return err
	}
	isHubBasis, err := HubBasisDays(ctx, q, "")
	if err != nil {
		return err
	}
	if err := hubDecisionFlags(ctx, q, b.Slug, card, hubFrom, isHubBasis, f); err != nil {
		return err
	}
	if err := hubDisputedFlags(ctx, q, b.Slug, winFrom, winTo, isHubBasis, f); err != nil {
		return err
	}
	if err := hubPendingAddFlags(ctx, q, b.Slug, winFrom, winTo, isHubBasis, f); err != nil {
		return err
	}
	if err := hubBoardPostFlags(ctx, q, b.Slug, winFrom, winTo, f); err != nil {
		return err
	}
	if err := hubPostDayAddFlags(ctx, q, b.Slug, b.ReportsFrom, hubFrom, winFrom, winTo, f); err != nil {
		return err
	}
	return nil
}

// hubFlagWindow is the date window for scoped Hub checks: the payout period (extended through today so zero-amount
// shifts with disputed lines or pending adds after the last positive unpaid day are still checked), or when there is
// no period, from the person's last payout (or payroll_reports_from if never paid) through today.
func hubFlagWindow(ctx context.Context, q ledger.Querier, b *PayoutBrief, today string) (from, to string, err error) {
	if b.Period != nil {
		return b.Period.From, max(b.Period.To, today), nil
	}
	payouts, err := personPayouts(ctx, q, b.Slug)
	if err != nil {
		return "", "", err
	}
	if len(payouts) > 0 {
		return payouts[len(payouts)-1].Date, today, nil
	}
	return b.ReportsFrom, today, nil
}

type hubDecisionRow struct {
	id                int64
	txRef             string
	pageLabel         string
	kind              string
	sharesRaw         string
	saidAt            string
	decidedAt         string
	appliedPerson     string
	appliedReportDate string
}

type hubSaleInfo struct {
	day       string
	netMicros int64
	netKnown  bool
	page      string
	refs      hub.SaleRefs
}

type countedSaleLine struct {
	amount int64
	net    int64
}

// hubDecisionFlags checks active owner, split, exclude and pot decisions from the switch day where this
// person's expected share is not counted (or where this person's current Hub report still counts a sale awarded to
// somebody else). Searched by state, not restricted to the payout window, so late decisions on already-paid periods
// still block.
func hubDecisionFlags(ctx context.Context, q ledger.Querier, slug string, card Person, hubFrom string, isHubBasis func(string, string) bool, f *flagger) error {
	decisionsFrom, err := decisions.HubFrom(ctx, q)
	if err != nil || decisionsFrom == "" {
		return err
	}
	userIDs, err := personHubUserIDs(ctx, q, slug, card)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, COALESCE(transaction_ref, ''), COALESCE(page_label, ''), kind, shares,
		COALESCE(said_at, ''), COALESCE(decided_at, ''), COALESCE(applied_person, ''), COALESCE(applied_report_date, '')
		FROM hub_sale_decisions WHERE state = ? AND kind IN (?, ?, ?, ?) ORDER BY id`,
		hub.SaleDecisionActive, hub.SaleDecisionOwner, hub.SaleDecisionSplit, hub.SaleDecisionExclude, hub.SaleDecisionPot)
	if err != nil {
		return err
	}
	var list []hubDecisionRow
	for rows.Next() {
		var d hubDecisionRow
		if err := rows.Scan(&d.id, &d.txRef, &d.pageLabel, &d.kind, &d.sharesRaw, &d.saidAt, &d.decidedAt,
			&d.appliedPerson, &d.appliedReportDate); err != nil {
			rows.Close()
			return err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}

	// Counted non-disputed lines of this person's current valid Hub-basis reports, plus the days they handed in a valid
	// Hub report (so even a zero-page Hub report on/after hubFrom counts as a Hub-basis day).
	countedLines, hubReportDays, err := personCountedHubLines(ctx, q, slug, hubFrom)
	if err != nil {
		return err
	}
	hubDayOf := func(day string) bool {
		if hubFrom == "" || day < hubFrom {
			return false
		}
		return isHubBasis(slug, day) || hubReportDays[day]
	}

	for _, d := range list {
		var shares []hub.SaleDecisionShare
		if err := json.Unmarshal([]byte(d.sharesRaw), &shares); err != nil {
			return fmt.Errorf("hub_sale_decisions %d: shares: %w", d.id, err)
		}
		myPercent := 0
		for _, sh := range shares {
			if d.kind == hub.SaleDecisionPot || d.kind == hub.SaleDecisionExclude || sh.Percent <= 0 {
				continue
			}
			if sh.Person != nil && *sh.Person != "" {
				if *sh.Person == slug {
					myPercent += sh.Percent
				}
			} else if sh.UserID > 0 && userIDs[sh.UserID] {
				myPercent += sh.Percent
			}
		}
		// Fast path for decisions that give this person nothing: only relevant if their Hub-basis report still counts it.
		if myPercent == 0 && (d.txRef == "" || len(countedLines) == 0) {
			continue
		}

		var info hubSaleInfo
		if d.txRef != "" {
			if info, err = lookupHubSale(ctx, q, d.txRef); err != nil {
				return err
			}
		}
		var countedMicros, lineNet int64
		if d.txRef != "" && len(countedLines) > 0 {
			refA, refB := info.refs.Either()
			if c, ok := countedLines[refA]; ok {
				countedMicros += c.amount
				lineNet = c.net
			}
			if refB != refA {
				if c, ok := countedLines[refB]; ok {
					countedMicros += c.amount
					if lineNet == 0 {
						lineNet = c.net
					}
				}
			}
		}
		if myPercent == 0 && countedMicros == 0 {
			continue
		}

		saleDay := info.day
		if saleDay == "" {
			saleDay = mskDayOf(d.saidAt)
		}
		if saleDay == "" {
			saleDay = mskDayOf(d.decidedAt)
		}
		if !decisions.HubRules(decisionsFrom, saleDay) {
			continue
		}

		netMicros := info.netMicros
		if !info.netKnown {
			netMicros = lineNet
		}
		page := d.pageLabel
		if page == "" {
			page = info.page
		}
		about := decisionSaleDesc(d.txRef, saleDay, page)

		if myPercent == 0 {
			f.add("hub_decision", true, "решение хаба #%d (%s): доля этому человеку 0%%, но действующий отчёт хаба считает %s",
				d.id, about, money.Micros(countedMicros).Cents())
			continue
		}

		netKnown := info.netKnown || lineNet != 0
		expectedMicros := netMicros/100*int64(myPercent) + (netMicros%100)*int64(myPercent)/100
		countedOK := d.txRef != "" && shareCounted(countedMicros, expectedMicros, netKnown)
		hubDay := hubDayOf(saleDay)
		if hubDay {
			// On a Hub-basis day, each participant's share is verified against their own active Hub report; an explicit
			// application can account for an absent line, but cannot correct a mismatched amount.
			if countedOK || (countedMicros == 0 && d.appliedPerson == slug) {
				continue
			}
		} else {
			// On a post-basis day, Hub carries one applied mark per decision (the second participant of a post-day split
			// is verified manually); a share already carried over into a later Hub-basis report also counts.
			if countedOK || (countedMicros == 0 && (d.appliedPerson != "" || d.appliedReportDate != "")) {
				continue
			}
		}

		shareText := fmt.Sprintf("доля %d%%", myPercent)
		if netKnown {
			shareText = fmt.Sprintf("доля %d%% (%s)", myPercent, money.Micros(expectedMicros).Cents())
		}
		var why string
		switch {
		case countedMicros != 0:
			why = fmt.Sprintf("в действующем отчёте хаба учтено %s", money.Micros(countedMicros).Cents())
		case hubDay:
			why = "не учтена в действующем отчёте хаба"
		default:
			why = "нет отметки применения в хабе"
		}
		f.add("hub_decision", true, "решение хаба #%d (%s): %s — %s", d.id, about, shareText, why)
	}
	return nil
}

// hubDisputedFlags flags every disputed line (disputed = 1, even with zero amount or NULL claimed_micros) in this
// person's active non-problem Hub shift reports within the window.
func hubDisputedFlags(ctx context.Context, q ledger.Querier, slug, winFrom, winTo string, isHubBasis func(string, string) bool, f *flagger) error {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.report_date, l.transaction_ref, l.page_label, l.net_micros, l.amount_micros, l.claimed_micros
		FROM hub_shift_report_lines l JOIN hub_shift_reports r ON r.id = l.report_id
		WHERE r.person = ? AND r.current = 1 AND r.problem IS NULL AND l.disputed = 1
		  AND r.report_date >= ? AND r.report_date <= ?
		ORDER BY r.report_date, r.id, l.seq`, slug, winFrom, winTo)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var reportID, netMicros, amountMicros int64
		var reportDate, txRef, pageLabel string
		var claimed sql.NullInt64
		if err := rows.Scan(&reportID, &reportDate, &txRef, &pageLabel, &netMicros, &amountMicros, &claimed); err != nil {
			return err
		}
		if !isHubBasis(slug, reportDate) {
			continue
		}
		if claimed.Valid {
			f.add("disputed", true, "спорная продажа %s за %s (%s, версия %d): заявлено %s, в отчёте хаба учтено %s — решить спор в хабе",
				txRef, ddmm(reportDate), pageLabel, reportID, money.Micros(claimed.Int64).Cents(), money.Micros(amountMicros).Cents())
		} else {
			f.add("disputed", true, "спорная продажа %s за %s (%s, версия %d): в отчёте хаба учтено %s (нетто %s) — решить спор в хабе",
				txRef, ddmm(reportDate), pageLabel, reportID, money.Micros(amountMicros).Cents(), money.Micros(netMicros).Cents())
		}
	}
	return rows.Err()
}

// hubPendingAddFlags flags pending off-platform adds (hub_shift_report_adds.state = 'pending') in this person's active
// Hub shift reports without problem within the window.
func hubPendingAddFlags(ctx context.Context, q ledger.Querier, slug, winFrom, winTo string, isHubBasis func(string, string) bool, f *flagger) error {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.report_date, a.hub_id, a.amount_micros, a.what_for, COALESCE(a.page_label, '')
		FROM hub_shift_report_adds a JOIN hub_shift_reports r ON r.id = a.report_id
		WHERE r.person = ? AND r.current = 1 AND r.problem IS NULL AND a.state = 'pending'
		  AND r.report_date >= ? AND r.report_date <= ?
		ORDER BY r.report_date, r.id, a.seq`, slug, winFrom, winTo)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var reportID, hubID, amountMicros int64
		var reportDate, whatFor, pageLabel string
		if err := rows.Scan(&reportID, &reportDate, &hubID, &amountMicros, &whatFor, &pageLabel); err != nil {
			return err
		}
		if !isHubBasis(slug, reportDate) {
			continue
		}
		what := ""
		if strings.TrimSpace(whatFor) != "" {
			what = " («" + strings.TrimSpace(whatFor) + "»)"
		}
		page := ""
		if pageLabel != "" {
			page = ", " + pageLabel
		}
		f.add("pending_add", true, "«мимо площадки» #%d за %s на %s%s (версия %d%s) ждёт подтверждения владельца в хабе",
			hubID, ddmm(reportDate), money.Micros(amountMicros).Cents(), what, reportID, page)
	}
	return rows.Err()
}

// hubBoardPostFlags adds informational (non-blocking) flags when a Telegram post on a Hub/board-basis day differs from
// the counted Hub shift report. A board day with no post at all is normal and produces no flag.
func hubBoardPostFlags(ctx context.Context, q ledger.Querier, slug, winFrom, winTo string, f *flagger) error {
	rows, err := q.QueryContext(ctx, `SELECT report_date, model, page, posted_micros, hub_micros, hub_state
		FROM v_report_pages
		WHERE person = ? AND basis = 'hub' AND hub_state IS NOT NULL
		  AND report_date >= ? AND report_date <= ?
		ORDER BY report_date, model, page`, slug, winFrom, winTo)
	if err != nil {
		return err
	}
	type pageRow struct {
		date, model, page, hubState string
		posted, hubAmt              sql.NullInt64
	}
	var list []pageRow
	hasPost := map[[2]string]bool{}
	for rows.Next() {
		var r pageRow
		if err := rows.Scan(&r.date, &r.model, &r.page, &r.posted, &r.hubAmt, &r.hubState); err != nil {
			rows.Close()
			return err
		}
		if r.posted.Valid {
			hasPost[[2]string{r.date, ledger.Slug(r.model)}] = true
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var dates []string
	diffs := map[string][]string{}
	addDiff := func(date, text string) {
		if _, ok := diffs[date]; !ok {
			dates = append(dates, date)
		}
		diffs[date] = append(diffs[date], text)
	}
	for _, r := range list {
		switch r.hubState {
		case "hub_changed":
			addDiff(r.date, fmt.Sprintf("%s: пост %s, хаб %s", r.page, money.Micros(r.posted.Int64).Cents(), money.Micros(r.hubAmt.Int64).Cents()))
		case "hub_missing":
			if r.posted.Valid && r.posted.Int64 != 0 {
				addDiff(r.date, fmt.Sprintf("%s: пост %s, в хабе нет", r.page, money.Micros(r.posted.Int64).Cents()))
			}
		case "hub_only":
			if hasPost[[2]string{r.date, ledger.Slug(r.model)}] && r.hubAmt.Valid && r.hubAmt.Int64 != 0 {
				addDiff(r.date, fmt.Sprintf("%s: в посте нет, хаб %s", r.page, money.Micros(r.hubAmt.Int64).Cents()))
			}
		}
	}
	for _, date := range dates {
		f.add("board_post", false, "пост за %s расходится с отчётом хаба (начислено по отчёту хаба): %s", ddmm(date), strings.Join(diffs[date], "; "))
	}
	return nil
}

// hubPostDayAddFlags adds informational (non-blocking) flags when a Hub shift report's added sale line («долёт»,
// added = 1) names a sale from a day paid by post (basis = 'post'), reminding to verify it against the post.
func hubPostDayAddFlags(ctx context.Context, q ledger.Querier, slug, reportsFrom, hubFrom, winFrom, winTo string, f *flagger) error {
	if hubFrom == "" {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.report_date, l.transaction_ref, l.page_label, l.amount_micros
		FROM hub_shift_report_lines l JOIN hub_shift_reports r ON r.id = l.report_id
		WHERE r.person = ? AND r.current = 1 AND r.problem IS NULL
		  AND l.added = 1 AND l.counted = 1 AND l.amount_micros <> 0
		  AND r.report_date >= ? AND r.report_date >= ? AND r.report_date <= ?
		ORDER BY r.report_date, r.id, l.seq`, slug, hubFrom, winFrom, winTo)
	if err != nil {
		return err
	}
	type addLine struct {
		reportID     int64
		reportDate   string
		txRef        string
		pageLabel    string
		amountMicros int64
	}
	var adds []addLine
	for rows.Next() {
		var a addLine
		if err := rows.Scan(&a.reportID, &a.reportDate, &a.txRef, &a.pageLabel, &a.amountMicros); err != nil {
			rows.Close()
			return err
		}
		adds = append(adds, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(adds) == 0 {
		return err
	}

	postDays := map[string]bool{}
	rows, err = q.QueryContext(ctx, `SELECT DISTINCT report_date FROM v_report_pages
		WHERE person = ? AND basis = 'post' AND posted_micros IS NOT NULL AND posted_micros <> 0 AND report_date >= ?`,
		slug, reportsFrom)
	if err != nil {
		return err
	}
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			rows.Close()
			return err
		}
		postDays[day] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(postDays) == 0 {
		return nil
	}

	for _, a := range adds {
		info, err := lookupHubSale(ctx, q, a.txRef)
		if err != nil {
			return err
		}
		if info.day == "" || !postDays[info.day] {
			continue
		}
		f.add("post_day_add", false, "добавка в отчёте хаба за %s (версия %d): продажа %s от %s (%s, %s) — день %s оплачен по посту, сверить с постом",
			ddmm(a.reportDate), a.reportID, a.txRef, ddmm(info.day), a.pageLabel, money.Micros(a.amountMicros).Cents(), ddmm(info.day))
	}
	return nil
}

// personHubUserIDs collects Hub user IDs known for this person from card aliases ("hub:<id>") and hub_people.logins.
func personHubUserIDs(ctx context.Context, q ledger.Querier, slug string, card Person) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, a := range card.Aliases {
		if raw, ok := strings.CutPrefix(strings.TrimSpace(a), "hub:"); ok {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
				out[id] = true
			}
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(logins, '[]') FROM hub_people WHERE code = ?`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var ids []int64
		if json.Unmarshal([]byte(raw), &ids) == nil {
			for _, id := range ids {
				if id > 0 {
					out[id] = true
				}
			}
		}
	}
	return out, rows.Err()
}

// personCountedHubLines reads the counted, non-disputed lines of this person's current valid Hub-basis reports
// (report_date >= hubFrom) and the set of dates of their current valid Hub-basis reports.
func personCountedHubLines(ctx context.Context, q ledger.Querier, slug, hubFrom string) (map[string]countedSaleLine, map[string]bool, error) {
	lines := map[string]countedSaleLine{}
	days := map[string]bool{}
	if hubFrom == "" {
		return lines, days, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT report_date FROM hub_shift_reports
		WHERE person = ? AND current = 1 AND problem IS NULL AND report_date >= ?`, slug, hubFrom)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			rows.Close()
			return nil, nil, err
		}
		days[day] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = q.QueryContext(ctx, `SELECT l.transaction_ref, SUM(l.amount_micros), MAX(l.net_micros)
		FROM hub_shift_report_lines l JOIN hub_shift_reports r ON r.id = l.report_id
		WHERE r.person = ? AND r.current = 1 AND r.problem IS NULL
		  AND r.report_date >= ?
		  AND l.counted = 1 AND l.disputed = 0 AND l.amount_micros <> 0
		GROUP BY l.transaction_ref`, slug, hubFrom)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var c countedSaleLine
		if err := rows.Scan(&ref, &c.amount, &c.net); err != nil {
			return nil, nil, err
		}
		lines[ref] = c
	}
	return lines, days, rows.Err()
}

// lookupHubSale resolves both references of a sale (hub.SaleRefsOf) and reads its Moscow day, net, and page from
// hub_sales, falling back to hub_transactions.
func lookupHubSale(ctx context.Context, q ledger.Querier, ref string) (hubSaleInfo, error) {
	refs, err := hub.SaleRefsOf(ctx, q, ref)
	if err != nil {
		return hubSaleInfo{}, err
	}
	out := hubSaleInfo{refs: refs}
	refA, refB := refs.Either()
	var at, page string
	var netMills sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT occurred_at, net_mills, page_label FROM hub_sales
		WHERE transaction_ref IN (?, ?) ORDER BY transaction_ref = ? DESC LIMIT 1`, refA, refB, refs.Hub()).
		Scan(&at, &netMills, &page)
	if err == nil {
		out.day, out.netMicros, out.page, out.netKnown = mskDayOf(at), netMills.Int64*1000, page, netMills.Valid
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return hubSaleInfo{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT occurred_at, net_mills, page_label FROM hub_transactions
		WHERE key = ? OR ref = ? OR key = ? OR ref = ? ORDER BY key = ? DESC, updated_at DESC, page_label LIMIT 1`,
		refA, refA, refB, refB, refs.Hub()).Scan(&at, &netMills, &page)
	if err == nil {
		out.day, out.netMicros, out.page, out.netKnown = mskDayOf(at), netMills.Int64*1000, page, netMills.Valid
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return hubSaleInfo{}, err
	}
	return out, nil
}

// shareCounted reports whether countedMicros matches the expected percent of netMicros (within half a cent to allow
// mill/cent rounding on splits). A known zero net has a zero expected share.
func shareCounted(countedMicros, expected int64, known bool) bool {
	if !known {
		return countedMicros != 0
	}
	if countedMicros == 0 || expected == 0 {
		return countedMicros == expected
	}
	return abs(countedMicros-expected) <= 5_000
}

func mskDayOf(at string) string {
	at = strings.TrimSpace(at)
	if at == "" {
		return ""
	}
	if t, err := ts.Parse(at); err == nil {
		return ts.Date(t)
	}
	if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
		return ts.Date(t)
	}
	if len(at) >= 10 {
		return at[:10]
	}
	return ""
}

func decisionSaleDesc(txRef, saleDay, page string) string {
	var parts []string
	if txRef != "" {
		parts = append(parts, "продажа "+txRef)
	} else {
		parts = append(parts, "без привязанной продажи")
	}
	if saleDay != "" {
		parts[0] += " от " + ddmm(saleDay)
	}
	if page != "" {
		parts = append(parts, page)
	}
	return strings.Join(parts, ", ")
}
