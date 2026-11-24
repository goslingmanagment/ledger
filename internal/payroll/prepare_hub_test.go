package payroll

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
)

// All names, IDs, pages, and amounts below are synthetic.

func execSQL(t *testing.T, s *db.Store, query string, args ...any) {
	t.Helper()
	if _, err := s.RW.Exec(query, args...); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
}

func flagsOfKind(b PayoutBrief, kind string) []PrepareFlag {
	var out []PrepareFlag
	for _, fl := range b.Flags {
		if fl.Kind == kind {
			out = append(out, fl)
		}
	}
	return out
}

func putHubPage(t *testing.T, s *db.Store, label, modelSlug string) {
	t.Helper()
	execSQL(t, s, `INSERT OR IGNORE INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
		VALUES (?, 'fansly', ?, ?, '2026-11-10T00:00:00+03:00')`, label, modelSlug, modelSlug)
}

func putHubSale(t *testing.T, s *db.Store, txRef, platformRef, page, occurredAt string, netMills int64) {
	t.Helper()
	execSQL(t, s, `INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at,
		post_tip, campaign, payload, first_seen_at, read_at)
		VALUES (?, '2026-11-09T00:00:00+03:00', ?, 'fansly', 'ppv', ?, ?, 0, 0, '{}', '2026-11-10T00:00:00+03:00', '2026-11-10T00:00:00+03:00')`,
		txRef, page, netMills, occurredAt)
	if platformRef != "" {
		execSQL(t, s, `INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
			VALUES ('hub', ?, 'transactions', ?, ?, '{}')`, page+"/transactions/"+txRef, occurredAt, occurredAt)
		execSQL(t, s, `INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash, event_id,
			first_seen_at, updated_at) VALUES (?, ?, ?, ?, 'USD', 'message_purchase', ?, '{}', 'h', last_insert_rowid(), ?, ?)`,
			page, txRef, occurredAt, netMills, platformRef, occurredAt, occurredAt)
	}
}

func putHubReport(t *testing.T, s *db.Store, id, shiftID int64, version int, person, reportDate string, current int, problem *string, pages map[string]int64) {
	t.Helper()
	var total int64
	for _, amt := range pages {
		total += amt
	}
	var stored any = "2026-11-18T10:00:00+03:00"
	if problem != nil {
		stored = nil
	}
	execSQL(t, s, `INSERT INTO hub_shift_reports(id, shift_id, version, person, hub_person, hub_user_id, report_date, submitted_at,
		door, total_micros, current, problem, payload, received_at, stored_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, '2026-11-18T09:00:00+03:00', 'workboard', ?, ?, ?, '{}', '2026-11-18T09:05:00+03:00', ?)`,
		id, shiftID, version, person, person, reportDate, total, current, problem, stored)
	seq := 0
	for page, amt := range pages {
		seq++
		execSQL(t, s, `INSERT INTO hub_shift_report_pages(report_id, seq, page_label, amount_micros) VALUES (?, ?, ?, ?)`,
			id, seq, page, amt)
	}
}

func putHubLine(t *testing.T, s *db.Store, reportID int64, seq int, txRef, page string, netMicros, amountMicros int64, counted, added, disputed int, claimed *int64) {
	t.Helper()
	execSQL(t, s, `INSERT INTO hub_shift_report_lines(report_id, seq, transaction_ref, page_label, grp, net_micros, rule_share_micros,
		amount_micros, counted, added, disputed, claimed_micros)
		VALUES (?, ?, ?, ?, 'mine', ?, ?, ?, ?, ?, ?, ?)`,
		reportID, seq, txRef, page, netMicros, amountMicros, amountMicros, counted, added, disputed, claimed)
}

func putHubAdd(t *testing.T, s *db.Store, reportID int64, seq int, hubID, amountMicros int64, whatFor, state string, counted int, page *string) {
	t.Helper()
	execSQL(t, s, `INSERT INTO hub_shift_report_adds(report_id, seq, hub_id, amount_micros, from_whom, what_for, state, counted, page_label)
		VALUES (?, ?, ?, ?, 'client', ?, ?, ?, ?)`,
		reportID, seq, hubID, amountMicros, whatFor, state, counted, page)
}

func putHubDecision(t *testing.T, s *db.Store, id int64, txRef *string, page, kind, shares, state, decidedAt string, appliedPerson, appliedDate *string) {
	t.Helper()
	execSQL(t, s, `INSERT INTO hub_sale_decisions(id, transaction_ref, page_label, kind, shares, state, source, said_by, decided_at,
		applied_person, applied_report_date, seq, change, changed_at, read_at)
		VALUES (?, ?, ?, ?, ?, ?, 'agent', 'владелец', ?, ?, ?, ?, 'made', ?, ?)`,
		id, txRef, page, kind, shares, state, decidedAt, appliedPerson, appliedDate, id, decidedAt, decidedAt)
}

func putPost(t *testing.T, s *db.Store, id int64, person, model, reportDate, postedAt string, pages map[string]int64) {
	t.Helper()
	var total int64
	for _, amt := range pages {
		total += amt
	}
	execSQL(t, s, `INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
		VALUES ('tg_reports', ?, 'post', ?, ?, '{}')`, "p-"+string(rune('0'+id%10))+"-"+reportDate+"-"+person+"-"+model, postedAt, postedAt)
	execSQL(t, s, `INSERT INTO shift_report_posts(id, event_id, peer, topic, msg_id, posted_at, person, model, report_date,
		total_micros, pages_micros, flags, text, current, digest, parsed_at)
		VALUES (?, last_insert_rowid(), 'chat-1', 1, ?, ?, ?, ?, ?, ?, ?, '[]', 'post', 1, 'd', ?)`,
		id, id, postedAt, person, model, reportDate, total, total, postedAt)
	seq := 0
	for page, amt := range pages {
		seq++
		execSQL(t, s, `INSERT INTO shift_report_post_pages(post_id, seq, name, page_label, in_shift, amount_micros)
			VALUES (?, ?, ?, ?, 1, ?)`, id, seq, page, page, amt)
	}
}

func strPtr(v string) *string { return &v }
func i64Ptr(v int64) *int64   { return &v }

// setupTwoPeopleTabs prepares Alfa and Bravo with payout tabs on Alpha and unpaid work in Nov 2026.
func setupTwoPeopleTabs(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := openStore(t)
	putHubPage(t, s, "page-1", "alpha")
	putHubPage(t, s, "page-2", "alpha")
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа", "Браво"}, map[string][][]Cell{
		"Альфа": sheet(25, 0, week("2026-11-16", map[string][7]any{"page-1": {nil, nil, 100, nil, nil, nil, nil}})),
		"Браво": sheet(25, 0, week("2026-11-16", map[string][7]any{"page-1": {nil, nil, 100, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-19T10:00:00+03:00")
	if err := SetReportsFrom(ctx, s.RW, "2026-11-10", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetHubFrom(ctx, s.RW, "2026-11-18", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-13", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	// Put both Alfa and Bravo on the board from 2026-11-18 (granted 2026-11-17 → v_board_spans.from_day = 2026-11-18).
	execSQL(t, s, `INSERT INTO hub_person_rights(hub_person_id, person, right_name, granted_at, granted_day, stored_at) VALUES
		(1, 'alfa', 'submits_shift', '2026-11-17T10:00:00+03:00', '2026-11-17', '2026-11-17T10:00:00+03:00'),
		(2, 'bravo', 'submits_shift', '2026-11-17T10:00:00+03:00', '2026-11-17', '2026-11-17T10:00:00+03:00')`)
	return s, ctx
}

// Split decision on a Hub-basis day: counted (or marked applied) for one of two participants.
// The first participant gets no hub_decision blocker; the second still gets a hub_decision blocker.
// Also verifies dual-ref matching via hub_transactions (decision holds Hub ID, report line holds platform ref).
func TestPrepareHubDecisionSplitPerPersonAndDepartedApplied(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)

	// Sale 8001 ($100) on Hub-basis day 2026-11-18, with platform ref pf-8001.
	putHubSale(t, s, "8001", "pf-8001", "page-1", "2026-11-18T14:00:00+03:00", 100_000)
	// Active split 50/50 between alfa and bravo; applied_person is set to alfa ONLY.
	putHubDecision(t, s, 10, strPtr("8001"), "page-1", "split",
		`[{"person":"alfa","userId":1,"percent":50},{"person":"bravo","userId":2,"percent":50}]`,
		"active", "2026-11-18T16:00:00+03:00", strPtr("alfa"), strPtr("2026-11-18"))

	// Alfa's active Hub report counts her 50% share via the platform ref (matched through hub_transactions).
	putHubReport(t, s, 101, 1001, 1, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": m("50")})
	putHubLine(t, s, 101, 1, "pf-8001", "page-1", m("100"), m("50"), 1, 0, 0, nil)
	// Bravo's active Hub report does NOT count sale 8001 (0 amount, counted = 0).
	putHubReport(t, s, 102, 1002, 1, "bravo", "2026-11-18", 1, nil, map[string]int64{"page-1": m("100")})
	putHubLine(t, s, 102, 1, "8001", "page-1", m("100"), 0, 0, 0, 0, nil)

	ba := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	if got := flagsOfKind(ba, "hub_decision"); len(got) != 0 {
		t.Fatalf("alfa should have no hub_decision blocker, got %+v", got)
	}
	bb := prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00")
	gotB := flagsOfKind(bb, "hub_decision")
	if len(gotB) != 1 || !gotB[0].Blocker || !strings.Contains(gotB[0].Text, "#10") || !strings.Contains(gotB[0].Text, "50%") {
		t.Fatalf("bravo should get hub_decision blocker for split #10, got %+v", gotB)
	}

	// v3 amendment: a departed person with no Hub report whose applied_person is marked for THIS person gets no blocker,
	// while the other split participant (bravo) still gets blocked.
	execSQL(t, s, `DELETE FROM hub_shift_report_lines WHERE report_id = 101`)
	execSQL(t, s, `DELETE FROM hub_shift_report_pages WHERE report_id = 101`)
	execSQL(t, s, `DELETE FROM hub_shift_reports WHERE id = 101`)
	ba = prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	if got := flagsOfKind(ba, "hub_decision"); len(got) != 0 {
		t.Fatalf("alfa with applied_person='alfa' should have no hub_decision blocker, got %+v", got)
	}
	bb = prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00")
	if got := flagsOfKind(bb, "hub_decision"); len(got) != 1 || !got[0].Blocker {
		t.Fatalf("bravo must stay blocked when applied_person='alfa', got %+v", got)
	}

	// Once bravo's new Hub report version counts bravo's 50% share, bravo's blocker disappears.
	execSQL(t, s, `UPDATE hub_shift_reports SET current = 0 WHERE id = 102`)
	putHubReport(t, s, 103, 1002, 2, "bravo", "2026-11-18", 1, nil, map[string]int64{"page-1": m("50")})
	putHubLine(t, s, 103, 1, "8001", "page-1", m("100"), m("50"), 1, 0, 0, nil)
	bb = prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00")
	if got := flagsOfKind(bb, "hub_decision"); len(got) != 0 {
		t.Fatalf("bravo should clear blocker once counted in current version, got %+v", got)
	}

	// Post-basis day split (2026-11-15 < payroll_basis_hub_from 2026-11-18):
	// without any applied mark both are blocked; with one applied mark (v3) neither is blocked forever.
	putHubSale(t, s, "8002", "pf-8002", "page-1", "2026-11-15T14:00:00+03:00", 80_000)
	putHubDecision(t, s, 11, strPtr("8002"), "page-1", "split",
		`[{"person":"alfa","userId":1,"percent":50},{"person":"bravo","userId":2,"percent":50}]`,
		"active", "2026-11-16T10:00:00+03:00", nil, nil)
	// A shadow Hub report before payroll_basis_hub_from does not accrue and must NOT silence a post-day decision.
	putHubReport(t, s, 104, 1004, 1, "alfa", "2026-11-15", 1, nil, map[string]int64{"page-1": m("40")})
	putHubLine(t, s, 104, 1, "8002", "page-1", m("80"), m("40"), 1, 0, 0, nil)

	if got := flagsOfKind(prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00"), "hub_decision"); len(got) != 1 || !got[0].Blocker {
		t.Fatalf("post-day split without applied mark must block alfa even with shadow Hub report, got %+v", got)
	}
	if got := flagsOfKind(prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00"), "hub_decision"); len(got) != 1 || !got[0].Blocker {
		t.Fatalf("post-day split without applied mark must block bravo, got %+v", got)
	}
	execSQL(t, s, `UPDATE hub_sale_decisions SET applied_person = 'alfa', applied_report_date = '2026-11-15' WHERE id = 11`)
	if got := flagsOfKind(prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00"), "hub_decision"); len(got) != 0 {
		t.Fatalf("post-day split with applied mark should clear alfa, got %+v", got)
	}
	if got := flagsOfKind(prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00"), "hub_decision"); len(got) != 0 {
		t.Fatalf("post-day split with applied mark should clear bravo (v3), got %+v", got)
	}
}

// Disputed line with zero amount and NULL claimed_micros still surfaces as a blocker;
// superseded and problem versions are ignored.
func TestPrepareHubDisputedZeroAmountNullClaimed(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)

	// Active version: disputed = 1, amount_micros = 0, claimed_micros = NULL.
	putHubReport(t, s, 201, 2001, 1, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 201, 1, "8101", "page-1", m("40"), 0, 0, 0, 1, nil)
	// Superseded version with disputed = 1: must be ignored.
	putHubReport(t, s, 202, 2002, 1, "alfa", "2026-11-18", 0, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 202, 1, "8102", "page-1", m("30"), 0, 0, 0, 1, i64Ptr(m("30")))
	// Version with problem: must be ignored.
	putHubReport(t, s, 203, 2003, 1, "alfa", "2026-11-18", 1, strPtr("broken"), map[string]int64{})
	putHubLine(t, s, 203, 1, "8103", "page-1", m("20"), 0, 0, 0, 1, nil)
	// Another person's disputed line: must not block alfa.
	putHubReport(t, s, 204, 2004, 1, "bravo", "2026-11-18", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 204, 1, "8104", "page-1", m("50"), 0, 0, 0, 1, i64Ptr(m("50")))

	b := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	got := flagsOfKind(b, "disputed")
	if len(got) != 1 || !got[0].Blocker || !strings.Contains(got[0].Text, "8101") {
		t.Fatalf("expected single disputed blocker for 8101, got %+v", got)
	}
}

// Pending off-platform add on active non-problem version is a blocker; ignored on problem version,
// superseded version, or confirmed/rejected adds.
func TestPrepareHubPendingAdd(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)

	// Active clean version with one pending, one confirmed, one rejected add.
	putHubReport(t, s, 301, 3001, 2, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": m("100")})
	putHubAdd(t, s, 301, 1, 701, m("15"), "custom clip", "pending", 0, nil)
	putHubAdd(t, s, 301, 2, 702, m("20"), "already ok", "confirmed", 1, strPtr("page-1"))
	putHubAdd(t, s, 301, 3, 703, m("10"), "declined", "rejected", 0, nil)
	// Superseded version with pending add: must be ignored.
	putHubReport(t, s, 300, 3001, 1, "alfa", "2026-11-18", 0, nil, map[string]int64{"page-1": m("100")})
	putHubAdd(t, s, 300, 1, 704, m("25"), "old version", "pending", 0, nil)
	// Active version with problem: must be ignored.
	putHubReport(t, s, 302, 3002, 1, "alfa", "2026-11-18", 1, strPtr("sum mismatch"), map[string]int64{})
	putHubAdd(t, s, 302, 1, 705, m("30"), "broken version", "pending", 0, nil)

	b := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	got := flagsOfKind(b, "pending_add")
	if len(got) != 1 || !got[0].Blocker || !strings.Contains(got[0].Text, "#701") || !strings.Contains(got[0].Text, "15.00") {
		t.Fatalf("expected single pending_add blocker for #701, got %+v", got)
	}
}

// Cutoff before sale_decisions_hub_from (and empty setting) suppresses hub_decision flags.
func TestPrepareHubDecisionCutoff(t *testing.T) {
	s, ctx := setupTwoPeopleTabs(t)

	// Sale before cutoff (2026-11-12 < 2026-11-13) and sale on cutoff (2026-11-13).
	putHubSale(t, s, "8201", "", "page-1", "2026-11-12T20:00:00+03:00", 50_000)
	putHubSale(t, s, "8202", "", "page-1", "2026-11-13T01:00:00+03:00", 60_000)
	putHubDecision(t, s, 21, strPtr("8201"), "page-1", "owner", `[{"person":"alfa","userId":1,"percent":100}]`,
		"active", "2026-11-14T10:00:00+03:00", nil, nil)
	putHubDecision(t, s, 22, strPtr("8202"), "page-1", "owner", `[{"person":"alfa","userId":1,"percent":100}]`,
		"active", "2026-11-14T10:00:00+03:00", nil, nil)

	b := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	got := flagsOfKind(b, "hub_decision")
	if len(got) != 1 || !strings.Contains(got[0].Text, "#22") || strings.Contains(got[0].Text, "#21") {
		t.Fatalf("expected only #22 (>= cutoff), got %+v", got)
	}

	// Clearing sale_decisions_hub_from disables all hub_decision flags.
	if err := decisions.SetHubFrom(ctx, s.RW, "", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	b = prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	if got := flagsOfKind(b, "hub_decision"); len(got) != 0 {
		t.Fatalf("empty sale_decisions_hub_from should emit no hub_decision flags, got %+v", got)
	}
}

// Zero balance (Nothing != "") still runs hubFlags and surfaces both disputed shares (in the fallback window
// from last payout / payroll_reports_from through today) and unapplied Hub decisions.
func TestPrepareZeroBalanceStillShowsHubFlags(t *testing.T) {
	s, ctx := openStore(t)
	putHubPage(t, s, "page-1", "alpha")
	f := newSheets("Alpha")
	// Alfa had a paid day on 2026-11-12 ($25), paid in full on 2026-11-15 → zero balance now.
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{
		"Альфа": sheet(0, 25, week("2026-11-09", map[string][7]any{"page-1": {nil, nil, nil, 100, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-15T10:00:00+03:00")
	importJournal(t, s, ctx, "2026-11-15T11:00:00+03:00", payout(1, "2026-11-15", "Альфа", "Alpha", "25.00", "выплата"))
	rebuild(t, s, ctx, "2026-11-15T12:00:00+03:00")

	if err := SetReportsFrom(ctx, s.RW, "2026-11-10", t0("2026-11-18T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetHubFrom(ctx, s.RW, "2026-11-18", t0("2026-11-18T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-13", t0("2026-11-18T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}

	// Disputed line on 2026-11-18 (after last payout 2026-11-15) with $0 amount: keeps balance at $0!
	putHubReport(t, s, 401, 4001, 1, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 401, 1, "8301", "page-1", m("60"), 0, 0, 0, 1, i64Ptr(m("60")))
	// Old disputed line BEFORE last payout (2026-11-11 < 2026-11-15): outside the zero-balance window.
	putHubReport(t, s, 402, 4002, 1, "alfa", "2026-11-11", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 402, 1, "8302", "page-1", m("30"), 0, 0, 0, 1, nil)
	// Unapplied Hub decision on 2026-11-18.
	putHubSale(t, s, "8303", "", "page-1", "2026-11-18T15:00:00+03:00", 70_000)
	putHubDecision(t, s, 31, strPtr("8303"), "page-1", "owner", `[{"person":"alfa","userId":1,"percent":100}]`,
		"active", "2026-11-18T17:00:00+03:00", nil, nil)

	b := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	if b.AmountMicros != 0 || b.Nothing == "" || b.Period != nil {
		t.Fatalf("expected zero balance Nothing brief, got amount=%d nothing=%q period=%+v", b.AmountMicros, b.Nothing, b.Period)
	}
	disp := flagsOfKind(b, "disputed")
	if len(disp) != 1 || !disp[0].Blocker || !strings.Contains(disp[0].Text, "8301") {
		t.Fatalf("expected disputed blocker for 8301 (and not 8302 before last payout), got %+v", disp)
	}
	dec := flagsOfKind(b, "hub_decision")
	if len(dec) != 1 || !dec[0].Blocker || !strings.Contains(dec[0].Text, "#31") {
		t.Fatalf("expected hub_decision blocker #31 on zero balance, got %+v", dec)
	}
	if b.Blockers < 2 {
		t.Fatalf("expected at least 2 blockers on zero balance, got %d (%+v)", b.Blockers, b.Flags)
	}

	// Never-paid person with zero balance uses payroll_reports_from through today.
	f.set("Alpha", []string{"Альфа", "Браво"}, map[string][][]Cell{
		"Альфа": sheet(0, 25, week("2026-11-09", map[string][7]any{"page-1": {nil, nil, nil, 100, nil, nil, nil}})),
		"Браво": sheet(0, 0, week("2026-11-09", map[string][7]any{"page-1": {nil, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-19T10:00:00+03:00")
	putHubReport(t, s, 403, 4003, 1, "bravo", "2026-11-12", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 403, 1, "8304", "page-1", m("40"), 0, 0, 0, 1, nil)
	putHubReport(t, s, 404, 4004, 1, "bravo", "2026-11-08", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 404, 1, "8305", "page-1", m("40"), 0, 0, 0, 1, nil)

	bb := prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00")
	if bb.AmountMicros != 0 || bb.Nothing == "" {
		t.Fatalf("expected zero balance for bravo, got %d %q", bb.AmountMicros, bb.Nothing)
	}
	dispB := flagsOfKind(bb, "disputed")
	if len(dispB) != 0 {
		t.Fatalf("shadow reports before the Hub basis blocked payout: %+v", dispB)
	}
}

// v3 amendment: a late Hub decision for an already-paid period (before b.Period.From and before last payout)
// still surfaces as a hub_decision blocker because decisions are searched by state, not only within the payout window.
func TestPrepareLateHubDecisionOnAlreadyPaidPeriod(t *testing.T) {
	s, ctx := openStore(t)
	putHubPage(t, s, "page-1", "alpha")
	f := newSheets("Alpha")
	// Paid day on 2026-11-14 ($25) + unpaid day on 2026-11-19 ($25).
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{
		"Альфа": sheet(25, 25,
			week("2026-11-09", map[string][7]any{"page-1": {nil, nil, nil, nil, nil, 100, nil}}),
			week("2026-11-16", map[string][7]any{"page-1": {nil, nil, nil, 100, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-19T10:00:00+03:00")
	importJournal(t, s, ctx, "2026-11-16T11:00:00+03:00", payout(1, "2026-11-16", "Альфа", "Alpha", "25.00", "выплата"))
	rebuild(t, s, ctx, "2026-11-19T10:10:00+03:00")

	if err := SetReportsFrom(ctx, s.RW, "2026-11-10", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetHubFrom(ctx, s.RW, "2026-11-18", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-13", t0("2026-11-19T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}

	// Late Hub decision made on 2026-11-20 for a sale from already-paid 2026-11-14 (>= sale_decisions_hub_from 11-13).
	putHubSale(t, s, "8401", "", "page-1", "2026-11-14T18:00:00+03:00", 80_000)
	putHubDecision(t, s, 41, strPtr("8401"), "page-1", "owner", `[{"person":"alfa","userId":1,"percent":100}]`,
		"active", "2026-11-20T09:00:00+03:00", nil, nil)

	b := prepare(t, s, "alfa", "2026-11-20T10:00:00+03:00")
	if b.Period == nil || b.Period.From != "2026-11-19" {
		t.Fatalf("expected unpaid period starting 2026-11-19, got %+v", b.Period)
	}
	got := flagsOfKind(b, "hub_decision")
	if len(got) != 1 || !got[0].Blocker || !strings.Contains(got[0].Text, "#41") || !strings.Contains(got[0].Text, "14.11") {
		t.Fatalf("expected late hub_decision blocker #41 for already-paid 14.11, got %+v", got)
	}
}

// Informational (non-blocking) flags:
// - board_post: post on a board/Hub-basis day differs from Hub report (absent when equal or no post).
// - post_day_add: Hub add line (added = 1) names a sale from a day paid by post (basis = 'post') with «сверить с постом».
func TestPrepareHubInfoFlagsBoardPostAndPostDayAdd(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)

	// 2026-11-17: post-basis day (< payroll_basis_hub_from 2026-11-18) with a post on page-1.
	putPost(t, s, 501, "alfa", "alpha", "2026-11-17", "2026-11-17T23:00:00+03:00", map[string]int64{"page-1": m("120")})
	// Shadow Hub report on 2026-11-17 differs from the post: basis is 'post', so NO board_post flag for 17.11!
	putHubReport(t, s, 510, 5010, 1, "alfa", "2026-11-17", 1, nil, map[string]int64{"page-1": m("100")})

	// 2026-11-18: board-basis day (basis = 'hub') where post ($120) differs from Hub report ($100) → board_post info flag.
	putPost(t, s, 502, "alfa", "alpha", "2026-11-18", "2026-11-18T23:00:00+03:00", map[string]int64{"page-1": m("120")})
	putHubReport(t, s, 511, 5011, 1, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": m("100")})

	// 2026-11-19: board-basis day where post equals Hub report → NO board_post flag.
	putPost(t, s, 503, "alfa", "alpha", "2026-11-19", "2026-11-19T23:00:00+03:00", map[string]int64{"page-1": m("90")})
	putHubReport(t, s, 512, 5012, 1, "alfa", "2026-11-19", 1, nil, map[string]int64{"page-1": m("90")})

	// 2026-11-20: board-basis day with Hub report only (NO post) → NO board_post flag.
	putHubReport(t, s, 513, 5013, 1, "alfa", "2026-11-20", 1, nil, map[string]int64{"page-1": m("75")})

	// Hub add line on 2026-11-18 naming sale 8501 from post-basis day 2026-11-17 → post_day_add info flag («сверить с постом»).
	putHubSale(t, s, "8501", "", "page-1", "2026-11-17T15:00:00+03:00", 50_000)
	putHubLine(t, s, 511, 1, "8501", "page-1", m("50"), m("50"), 1, 1, 0, nil)
	// Hub add line on 2026-11-19 naming sale 8502 from Hub-basis day 2026-11-18 → NO post_day_add flag!
	putHubSale(t, s, "8502", "", "page-1", "2026-11-18T15:00:00+03:00", 40_000)
	putHubLine(t, s, 512, 1, "8502", "page-1", m("40"), m("40"), 1, 1, 0, nil)

	b := prepare(t, s, "alfa", "2026-11-20T12:00:00+03:00")
	bp := flagsOfKind(b, "board_post")
	if len(bp) != 1 || bp[0].Blocker || !strings.Contains(bp[0].Text, "18.11") || !strings.Contains(bp[0].Text, "120.00") || !strings.Contains(bp[0].Text, "100.00") {
		t.Fatalf("expected single non-blocking board_post flag for 18.11, got %+v", bp)
	}
	pda := flagsOfKind(b, "post_day_add")
	if len(pda) != 1 || pda[0].Blocker || !strings.Contains(pda[0].Text, "8501") || !strings.Contains(pda[0].Text, "сверить с постом") {
		t.Fatalf("expected single non-blocking post_day_add flag for 8501 with «сверить с постом», got %+v", pda)
	}

	// Board day with a post missing a page that Hub reported (hub_only alongside a posted page of the same model)
	// and a page only in the post (hub_missing) also triggers board_post.
	putPost(t, s, 504, "bravo", "alpha", "2026-11-18", "2026-11-18T23:00:00+03:00", map[string]int64{"page-1": m("60")})
	putHubReport(t, s, 514, 5014, 1, "bravo", "2026-11-18", 1, nil, map[string]int64{"page-2": m("60")})
	bb := prepare(t, s, "bravo", "2026-11-20T12:00:00+03:00")
	bpB := flagsOfKind(bb, "board_post")
	if len(bpB) != 1 || bpB[0].Blocker || !strings.Contains(bpB[0].Text, "page-1") || !strings.Contains(bpB[0].Text, "page-2") {
		t.Fatalf("expected board_post mentioning both page-1 and page-2, got %+v", bpB)
	}
}

// Wrong share amount (e.g. 100% counted when split gives 50%), person matched by hub_people login when share.person
// is null, and a sale awarded to somebody else still counted in this person's Hub report all produce hub_decision blockers.
func TestPrepareHubDecisionMismatchAndLoginMatch(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)

	// Map bravo to Hub userId 77 via hub_people.logins.
	execSQL(t, s, `INSERT INTO hub_people(hub_id, code, kind, role, state, logins, updated_at, stored_at)
		VALUES (2, 'bravo', 'human', 'chatter', 'active', '[77]', '2026-11-18T00:00:00+03:00', '2026-11-18T00:00:00+03:00')`)

	// Sale 8701 ($100) on Hub-basis day 2026-11-18: split 50/50 (bravo named only by userId 77, person=null).
	putHubSale(t, s, "8701", "", "page-1", "2026-11-18T14:00:00+03:00", 100_000)
	putHubDecision(t, s, 71, strPtr("8701"), "page-1", "split",
		`[{"person":"alfa","userId":1,"percent":50},{"person":null,"userId":77,"percent":50}]`,
		"active", "2026-11-18T16:00:00+03:00", nil, nil)
	// Alfa's Hub report counts the full $100 instead of her 50% ($50) → must still block!
	putHubReport(t, s, 701, 7001, 1, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": m("100")})
	putHubLine(t, s, 701, 1, "8701", "page-1", m("100"), m("100"), 1, 0, 0, nil)

	// Sale 8702 ($60) on 2026-11-18: owner = bravo, but alfa's Hub report still counts $60 → blocks alfa too.
	putHubSale(t, s, "8702", "", "page-1", "2026-11-18T15:00:00+03:00", 60_000)
	putHubDecision(t, s, 72, strPtr("8702"), "page-1", "owner",
		`[{"person":"bravo","userId":77,"percent":100}]`,
		"active", "2026-11-18T16:30:00+03:00", nil, nil)
	putHubLine(t, s, 701, 2, "8702", "page-1", m("60"), m("60"), 1, 0, 0, nil)

	ba := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	gotA := flagsOfKind(ba, "hub_decision")
	if len(gotA) != 2 || !gotA[0].Blocker || !gotA[1].Blocker || !strings.Contains(gotA[0].Text, "#71") || !strings.Contains(gotA[1].Text, "#72") {
		t.Fatalf("expected two hub_decision blockers for alfa (#71 wrong share, #72 awarded to bravo), got %+v", gotA)
	}

	bb := prepare(t, s, "bravo", "2026-11-19T10:30:00+03:00")
	gotB := flagsOfKind(bb, "hub_decision")
	if len(gotB) != 2 || !strings.Contains(gotB[0].Text, "#71") || !strings.Contains(gotB[1].Text, "#72") {
		t.Fatalf("expected bravo matched by userId 77 for #71 and by slug for #72, got %+v", gotB)
	}
}

// Frozen ledger sale_decisions and sale_questions tables (frozen since 2026-11-13) produce NO flags.
func TestPrepareFrozenLedgerDecisionsAndQuestionsProduceNoFlags(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)

	execSQL(t, s, `INSERT INTO sale_decisions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, occurred_at, kind, shares,
		said_by, said_at, note, status, created_at, key_name)
		VALUES (1, 'page-1', 'fan-1', 'Fan', '8601', 50000000, '2026-11-18T12:00:00+03:00', 'owner',
		'[{"person":"alfa","percent":100}]', 'владелец', '2026-11-18', 'frozen', 'open', '2026-11-18T12:00:00+03:00', 'agent')`)
	execSQL(t, s, `INSERT INTO sale_questions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, occurred_at, question,
		asked_of, who, asked_at, note, status, created_at, key_name)
		VALUES (1, 'page-1', 'fan-1', 'Fan', '8602', 60000000, '2026-11-18T13:00:00+03:00', 'whose sale?',
		'owner', 'alfa', '2026-11-18', 'frozen', 'open', '2026-11-18T13:00:00+03:00', 'agent')`)

	b := prepare(t, s, "alfa", "2026-11-19T10:30:00+03:00")
	for _, fl := range b.Flags {
		switch fl.Kind {
		case "hub_decision", "disputed", "pending_add", "board_post", "post_day_add":
			t.Fatalf("frozen ledger tables must produce no Hub flags, got %+v", fl)
		}
		if strings.Contains(fl.Text, "8601") || strings.Contains(fl.Text, "8602") {
			t.Fatalf("frozen ledger rows leaked into flags: %+v", fl)
		}
	}
}

func TestDecisionsCompareCountedAmountsEvenAfterApplication(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)
	const at = "2026-11-18T11:00:00+03:00"
	putHubReport(t, s, 501, 501, 1, "alfa", at[:10], 1, nil, map[string]int64{"page-1": m("60")})
	for i, kind := range []string{"owner", "pot", "exclude", "owner"} {
		ref := strconv.Itoa(701 + i)
		net := int64(30000)
		if i == 3 {
			net = 0
		}
		putHubSale(t, s, ref, "", "page-1", at, net)
		if i != 3 {
			putHubLine(t, s, 501, i+1, ref, "page-1", net*1000, m("20"), 1, 0, 0, nil)
		}
		var applied *string
		if i == 0 {
			applied = strPtr("alfa")
		}
		shares := "[]"
		if kind == "owner" {
			shares = `[{"person":"alfa","percent":100}]`
		}
		putHubDecision(t, s, int64(i+1), strPtr(ref), "page-1", kind, shares, "active", at, applied, nil)
	}
	got := flagsOfKind(prepare(t, s, "alfa", at), "hub_decision")
	if len(got) != 3 {
		t.Fatalf("want wrong applied amount and both overcounts: %+v", got)
	}
	for i, f := range got {
		if !strings.Contains(f.Text, "#"+strconv.Itoa(i+1)+" ") {
			t.Fatal(got)
		}
	}
}

func TestShadowHubDisputeAndPendingAddDoNotBlock(t *testing.T) {
	s, _ := setupTwoPeopleTabs(t)
	putHubReport(t, s, 601, 601, 1, "alfa", "2026-11-18", 1, nil, map[string]int64{"page-1": 0})
	putHubLine(t, s, 601, 1, "901", "page-1", m("12"), 0, 0, 0, 1, nil)
	putHubAdd(t, s, 601, 1, 902, m("7"), "fixture", "pending", 0, nil)
	execSQL(t, s, `UPDATE settings SET value='2026-11-19' WHERE key='payroll_basis_hub_from'`)
	b := prepare(t, s, "alfa", "2026-11-19T12:00:00+03:00")
	if len(flagsOfKind(b, "disputed"))+len(flagsOfKind(b, "pending_add")) != 0 {
		t.Fatal(b.Flags)
	}
}
