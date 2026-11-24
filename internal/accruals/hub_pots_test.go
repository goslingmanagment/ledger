package accruals

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/reports"
)

const cutDay = "2026-11-10"

func distribution(seq int, change, day, person string, sales ...int) map[string]any {
	inflows := []map[string]any{}
	for _, sale := range sales {
		inflows = append(inflows, map[string]any{"transactionId": sale, "pageLabel": "page-a", "baseMills": 12000})
	}
	base := len(sales) * 12000
	return map[string]any{"seq": seq, "change": change, "at": reportNow, "distribution": map[string]any{"id": 73,
		"pot": map[string]any{"code": "demo-pool", "name": "Тестовый", "modelSlug": "alpha-hub"}, "day": day,
		"formula": map[string]any{"kind": "equal", "params": map[string]any{}}, "inflows": inflows, "baseMills": base,
		"lines": []map[string]any{{"personCode": person, "baseMills": base}}}}
}

type potPage struct {
	since, next   string
	more          bool
	status        int
	changes, held []map[string]any
}

func readPots(t *testing.T, s *db.Store, pages ...potPage) (hub.PotStatus, error) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n >= len(pages) {
			t.Error("extra request")
			w.WriteHeader(400)
			return
		}
		p := pages[n]
		n++
		if r.URL.Path != hub.PotDistributionsPath || r.URL.Query().Get("limit") != "500" || r.URL.Query().Get("since") != p.since || (p.since == "" && r.URL.Query().Has("since")) {
			t.Errorf("request %s", r.URL)
		}
		if p.status != 0 {
			w.WriteHeader(p.status)
			return
		}
		if p.changes == nil {
			p.changes = []map[string]any{}
		}
		if p.held == nil {
			p.held = []map[string]any{}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"changes": p.changes, "held": p.held, "next": p.next, "more": p.more, "serverNow": reportNow}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	st, err := hub.SyncPotDistributions(context.Background(), s, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "test"}, nil), reportNow)
	if n != len(pages) {
		t.Errorf("read %d/%d", n, len(pages))
	}
	return st, err
}

func enablePots(t *testing.T, s *db.Store) {
	t.Helper()
	if _, err := readPots(t, s, potPage{next: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := hub.SetPotSharesFrom(context.Background(), s, cutDay, false, reportNow); err != nil {
		t.Fatal(err)
	}
}

func deliverPot(t *testing.T, s *db.Store, seq int, change, day, person string, sales ...int) {
	t.Helper()
	cursor := cols(t, s, `SELECT value FROM cursors WHERE name='hub:pot_distributions'`)
	if _, err := readPots(t, s, potPage{since: cursor, next: strconv.Itoa(seq), changes: []map[string]any{distribution(seq, change, day, person, sales...)}}); err != nil {
		t.Fatal(err)
	}
}

func potFlag(t *testing.T, s *db.Store, person, part string, now time.Time) bool {
	t.Helper()
	b, err := payroll.PreparePayout(context.Background(), s.RO, person, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range b.Flags {
		if f.Kind == "hub_pots" && f.Blocker && strings.Contains(f.Text, part) {
			return true
		}
	}
	return false
}

func TestPotCutoverPreservesLegacyAndChecksRollback(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, "2026-11-09")
	declare(t, s, ctx, "test-set", "Набор", "Alpha", []string{"page-a"}, "alfa", "bravo")
	potSale(t, s, ctx, "test-set", "legacy-one", "page-a", "2026-11-09T12:00:00+03:00", 121)
	before := cols(t, s, `SELECT * FROM report_accruals WHERE day<'2026-11-10' ORDER BY person,model,day`)
	set := func(day string, dry bool) error { return hub.SetPotSharesFrom(ctx, s, day, dry, reportNow) }
	if err := set(cutDay, true); err == nil {
		t.Fatal("cutover without read")
	}
	if st, err := readPots(t, s, potPage{status: 404}); err != nil || st.Cursor != "" || st.LastOK != "" || !checks.HubPots(ctx, checks.Env{Q: s.RO, Now: reportNow}).OK {
		t.Fatalf("shadow 404: %+v %v", st, err)
	}
	if _, err := readPots(t, s, potPage{next: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := set("2026-11-09", true); err == nil {
		t.Fatal("cutover over legacy")
	}
	if err := set(cutDay, true); err != nil {
		t.Fatal(err)
	}
	if got := cols(t, s, `SELECT value FROM settings WHERE key='pot_shares_hub_from'`); got != "" {
		t.Fatal("dry-run wrote ", got)
	}
	if err := set(cutDay, false); err != nil {
		t.Fatal(err)
	}
	if err := set("", false); err != nil {
		t.Fatal(err)
	}
	if err := set(cutDay, false); err != nil {
		t.Fatal(err)
	}
	deliverPot(t, s, 1, "posted", cutDay, "bravo", 701)
	if after := cols(t, s, `SELECT * FROM report_accruals WHERE day<'2026-11-10' ORDER BY person,model,day`); after != before {
		t.Fatal("legacy day changed", after)
	}
	if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE day=? AND person='bravo'`, cutDay); got != "12000000" {
		t.Fatal(got)
	}
	if err := set("", false); err == nil || !strings.Contains(err.Error(), "компенсации") {
		t.Fatal("disabled after posting", err)
	}
	if !potFlag(t, s, "alfa", "больше часа", reportNow.Add(time.Hour+time.Second)) {
		t.Fatal("stale feed passed")
	}
	st, err := readPots(t, s, potPage{since: "1", status: 404})
	if err == nil || st.Cursor != "1" || checks.HubPots(ctx, checks.Env{Q: s.RO, Now: reportNow}).OK || !potFlag(t, s, "alfa", "404", reportNow) {
		t.Fatalf("active 404: %+v %v", st, err)
	}
}

func TestPotPagesAreAtomicAndReversalsSurviveReplay(t *testing.T) {
	s, ctx := reportWeek(t)
	held := []map[string]any{{"seq": 3, "distributionId": 75, "reason": "page_outside_key", "pageLabels": []string{"page-b"}}}
	first := distribution(1, "posted", cutDay, "alfa", 901, 902)
	st, err := readPots(t, s, potPage{next: "1", more: true, changes: []map[string]any{first}}, potPage{since: "1", next: "3", held: held, changes: []map[string]any{distribution(2, "reversed", cutDay, "alfa", 901, 902)}})
	if err != nil || st.Held != 1 || st.Cursor != "3" {
		t.Fatal(st, err)
	}
	if err = hub.SetPotSharesFrom(ctx, s, cutDay, true, reportNow); err == nil {
		t.Fatal("held cutover")
	}
	if _, err = readPots(t, s, potPage{since: "3", next: "3", changes: []map[string]any{first}}); err != nil {
		t.Fatal(err)
	}
	if got := cols(t, s, `SELECT state,seq FROM hub_pot_distributions`); got != "reversed 2" {
		t.Fatal(got)
	}
	// Fail the last write, after the distribution, inflows and held. Retry exactly this page.
	exec(t, s, `CREATE TRIGGER fail_pot_cursor BEFORE UPDATE ON cursors WHEN NEW.name='hub:pot_distributions' BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	page := potPage{since: "3", next: "4", held: held, changes: []map[string]any{distribution(4, "posted", cutDay, "alfa", 903)}}
	if _, err = readPots(t, s, page); err == nil {
		t.Fatal("accepted partial write")
	}
	if got := cols(t, s, `SELECT state,seq,(SELECT count(*) FROM hub_pot_distribution_inflows),(SELECT count(*) FROM hub_pot_distribution_held) FROM hub_pot_distributions`); got != "reversed 2 2 0" {
		t.Fatal(got)
	}
	exec(t, s, `DROP TRIGGER fail_pot_cursor`)
	if _, err = readPots(t, s, page); err != nil {
		t.Fatal(err)
	}
	if got := cols(t, s, `SELECT state,seq FROM hub_pot_distributions`); got != "reversed 2" {
		t.Fatal("higher-sequence post resurrected distribution", got)
	}
	fresh := distribution(5, "posted", cutDay, "alfa", 903)
	fresh["distribution"].(map[string]any)["id"] = 74
	fresh["distribution"].(map[string]any)["pot"].(map[string]any)["modelSlug"] = "unknown-demo"
	if _, err = readPots(t, s, potPage{since: "4", next: "5", changes: []map[string]any{fresh}}); err != nil {
		t.Fatal(err)
	}
	if _, err = readPots(t, s, potPage{since: "5", next: "5"}); err != nil {
		t.Fatal(err)
	}
	if err = hub.SetPotSharesFrom(ctx, s, cutDay, true, reportNow); err == nil || !strings.Contains(err.Error(), "модели") {
		t.Fatal(err)
	}
}

func savePotDraft(t *testing.T, s *db.Store, ctx context.Context, ref string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"hub_username": "alfa", "report_date": cutDay, "source": "agent", "total_mills": 80000,
		"window_from": cutDay + "T04:00:00+03:00", "window_to": cutDay + "T23:00:00+03:00", "cut_at": cutDay + "T22:50:00+03:00",
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 80000}},
		"lines": []any{map[string]any{"n": 1, "transaction_ref": ref, "page": "page-a", "net_mills": 80000, "counted": true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reports.Save(ctx, s, raw, "test", reportNow); err != nil {
		t.Fatal(err)
	}
	if err := reports.Changed(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
}

func TestMultiSalePotHeldInBothOrders(t *testing.T) {
	for _, potFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("pot-first=", potFirst), func(t *testing.T) {
			s, ctx := reportWeek(t)
			setReports(t, s, ctx, cutDay)
			setHub(t, s, ctx, cutDay)
			enablePots(t, s)
			// Sale 7000 is the second inflow. The report identifies it by the platform's reference.
			exec(t, s, `INSERT INTO events(source,source_id,kind,occurred_at,received_at,payload) VALUES('hub','demo-7000','transactions',?,?, '{}')`, cutDay, cutDay)
			exec(t, s, `INSERT INTO hub_transactions(page_label,key,occurred_at,net_mills,currency,type,ref,fields,hash,event_id,first_seen_at,updated_at)
 VALUES('page-a','7000',?,12000,'USD','tip','demo-platform-sale','{}','h',(SELECT id FROM events WHERE source_id='demo-7000'),?,?)`, cutDay, cutDay, cutDay)

			pot := func() { deliverPot(t, s, 1, "posted", cutDay, "bravo", 7001, 7000) }
			if potFirst {
				pot()
				if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE person='bravo' AND day=?`, cutDay); got != "24000000" {
					t.Fatal(got)
				}
				savePotDraft(t, s, ctx, "demo-platform-sale")
			} else {
				savePotDraft(t, s, ctx, "demo-platform-sale")
				pot()
			}
			if !potFlag(t, s, "bravo", "7000", reportNow) || potFlag(t, s, "alfa", "7000", reportNow) || checks.HubPots(ctx, checks.Env{Q: s.RO, Now: reportNow}).OK {
				t.Fatal("wrong recipients blocked")
			}
			exec(t, s, `UPDATE hub_transactions SET ref=NULL WHERE key='7000'`)
			if !potFlag(t, s, "bravo", "ждём ссылку площадки", reportNow) {
				t.Fatal("unresolved platform sale became payable")
			}
			if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE person='bravo' AND day=?`, cutDay); got != "0" {
				t.Fatal(got)
			}
		})
	}
}

func TestDismissKeepsBookedReportSaleOutOfDistribution(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, cutDay)
	setHub(t, s, ctx, cutDay)
	enablePots(t, s)
	handIn(t, s, hubVersion(77, 71, 1, "alfa", cutDay, hubPage{"page-a", 80}))
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "35.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	handIn(t, s, hubVersion(78, 71, 2, "alfa", cutDay, hubPage{"page-a", 48}))
	deliverPot(t, s, 1, "posted", cutDay, "bravo", 7700)
	open, err := payroll.OpenChanges(ctx, s.RO, "alfa")
	if err != nil || len(open) != 1 {
		t.Fatal(open, err)
	}
	if _, err := payroll.DecideChange(ctx, s, open[0].ID, payroll.DecisionDismiss, "owner", "fixture", reportNow); err != nil {
		t.Fatal(err)
	}
	if got := cols(t, s, `SELECT hub_reports,sale_refs FROM v_booked_report_days WHERE person='alfa' AND day=?`, cutDay); got != `[77] ["7700"]` {
		t.Fatal(got)
	}
	if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE person='bravo' AND day=?`, cutDay); got != "0" || !potFlag(t, s, "bravo", "7700", reportNow) {
		t.Fatal("paid version lost after dismiss", got)
	}
}

func TestPaidPotChangesAndPartialPrepare(t *testing.T) {
	for _, payout := range []string{"22.00", "30.00"} {
		t.Run(payout, func(t *testing.T) {
			s, ctx := reportBook(t)
			tables(t, s, ctx, map[string]map[string][][]payroll.Cell{"Alpha": {"Альфа": pageTab("2026-11-09", "Page A", 0.25, 0, [7]any{})}})
			setReports(t, s, ctx, cutDay)
			enablePots(t, s)
			// Pot-only day. Its last line disappearing must resolve to zero even without a report.
			deliverPot(t, s, 1, "posted", cutDay, "alfa", 801, 802, 803, 804, 805, 806, 807, 808, 809, 810)
			importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", payout))
			if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
				t.Fatal(err)
			}
			show := func() {
				b, err := payroll.PreparePayout(ctx, s.RO, "alfa", reportNow)
				if err != nil || !strings.Contains(b.Message, "общак тестовый, раздача 73: доля базы 120 (поровну на 1) × 25% = 30") {
					t.Fatal(b.Message, err)
				}
			}
			if payout == "22.00" {
				show()
			}
			deliverPot(t, s, 2, "reversed", cutDay, "alfa", 801, 802, 803, 804, 805, 806, 807, 808, 809, 810)
			open, err := payroll.OpenChanges(ctx, s.RO, "alfa")
			if err != nil || len(open) != 1 || open[0].Reason != "общак" || open[0].NewMicros != 0 {
				t.Fatal(open, err)
			}
			if _, err := payroll.DecideChange(ctx, s, open[0].ID, payroll.DecisionDismiss, "owner", "fixture", reportNow); err == nil {
				t.Fatal("reversal dismissed")
			}
			if payout == "22.00" {
				show()
			}
			if _, err := payroll.DecideChange(ctx, s, open[0].ID, payroll.DecisionApply, "owner", "fixture", reportNow); err != nil {
				t.Fatal(err)
			}
			if got := cols(t, s, `SELECT count(*) FROM v_booked_report_days WHERE day=?`, cutDay); got != "0" {
				t.Fatal("reversal not applied", got)
			}
		})
	}
}

func TestLatePotOnPaidReport(t *testing.T) {
	for _, payout := range []string{"50.00", "55.00"} {
		t.Run(payout, func(t *testing.T) {
			s, ctx := reportWeek(t)
			setReports(t, s, ctx, cutDay)
			enablePots(t, s)
			importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", payout))
			if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
				t.Fatal(err)
			}
			deliverPot(t, s, 1, "posted", cutDay, "alfa", 981)
			open, err := payroll.OpenChanges(ctx, s.RO, "alfa")
			if err != nil || len(open) != 1 || open[0].Reason != "общак" || open[0].NewMicros-open[0].PostedMicros != 3000000 {
				t.Fatal(open, err)
			}
		})
	}
}

func TestOpenChangeReservesProposedReportSale(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, cutDay)
	setHub(t, s, ctx, cutDay)
	enablePots(t, s)
	handIn(t, s, hubVersion(61, 60, 1, "alfa", cutDay, hubPage{"page-a", 80}))
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "35.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	handIn(t, s, hubVersion(62, 60, 2, "alfa", cutDay, hubPage{"page-a", 48}))
	if got := cols(t, s, `SELECT sale_refs FROM payroll_pending_changes WHERE state='open'`); got != `["6200"]` {
		t.Fatal(got)
	}
	deliverPot(t, s, 1, "posted", cutDay, "bravo", 6200)
	// The next report page has committed, before its post-commit rebuild replaces the open proposal.
	exec(t, s, `UPDATE hub_shift_reports SET current=0 WHERE id=62`)
	if got := cols(t, s, `SELECT sale_refs FROM v_booked_report_days WHERE person='alfa' AND day=?`, cutDay); got != `["6100"]` {
		t.Fatal(got)
	}
	if !potFlag(t, s, "bravo", "6200", reportNow) {
		t.Fatal("open change lost its proposed sale")
	}
}

func TestPots404AfterRestoredCutoverBlocksFirstRead(t *testing.T) {
	s, ctx := reportWeek(t)
	exec(t, s, `UPDATE settings SET value=? WHERE key='pot_shares_hub_from'`, cutDay)
	st, err := readPots(t, s, potPage{status: 404})
	if err == nil || st.Cursor != "" || st.LastOK != "" || !potFlag(t, s, "alfa", "404", reportNow) || checks.HubPots(ctx, checks.Env{Q: s.RO, Now: reportNow}).OK {
		t.Fatal(st, err)
	}
}

func TestWorkboardFormulaLabelsAndHeldScope(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, cutDay)
	enablePots(t, s)
	for seq, formula := range []struct{ kind, basis, want string }{{"equal", "", "поровну на 1"}, {"weights", "", "по весам"}, {"manual", "", "вручную"}, {"weights", "две части", "две части"}} {
		c := distribution(seq+1, "posted", cutDay, "bravo", 904)
		d := c["distribution"].(map[string]any)
		d["formula"] = map[string]any{"kind": formula.kind, "version": 2, "params": map[string]any{}}
		d["lines"].([]map[string]any)[0]["basis"] = formula.basis
		if _, err := readPots(t, s, potPage{since: strconv.Itoa(seq), next: strconv.Itoa(seq + 1), changes: []map[string]any{c}}); err != nil {
			t.Fatal(err)
		}
		b, err := payroll.PreparePayout(ctx, s.RO, "bravo", reportNow)
		if err != nil || !strings.Contains(b.Message, fmt.Sprintf("доля базы 12 (%s) × 20%% = 2.40", formula.want)) {
			t.Fatal(b.Message, err)
		}
	}
	held := []map[string]any{{"seq": 5, "distributionId": 73, "reason": "page_outside_key", "pageLabels": []string{"page-a"}}}
	if _, err := readPots(t, s, potPage{since: "4", next: "5", held: held}); err != nil {
		t.Fatal(err)
	}
	if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE person='bravo' AND day=?`, cutDay); got != "0" {
		t.Fatal(got)
	}
	if !potFlag(t, s, "alfa", "page_outside_key", reportNow) || checks.HubPots(ctx, checks.Env{Q: s.RO, Now: reportNow}).OK {
		t.Fatal("held snapshot did not block")
	}
}

func TestPersonalRecipientWaitsForPaidPotRemoval(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, cutDay)
	setHub(t, s, ctx, cutDay)
	enablePots(t, s)
	deliverPot(t, s, 1, "posted", cutDay, "bravo", 5100)
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Браво", "Alpha", "10.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	handIn(t, s, hubVersion(51, 51, 1, "alfa", cutDay, hubPage{"page-a", 80}))
	if !potFlag(t, s, "alfa", "остаётся в проведённой раздаче", reportNow) {
		t.Fatal("personal recipient can be paid before pot correction")
	}
	open, err := payroll.OpenChanges(ctx, s.RO, "bravo")
	if err != nil || len(open) != 1 || open[0].Reason != "общак" {
		t.Fatal(open, err)
	}
	if _, err := payroll.DecideChange(ctx, s, open[0].ID, payroll.DecisionApply, "owner", "fixture", reportNow); err != nil {
		t.Fatal(err)
	}
	if potFlag(t, s, "alfa", "остаётся в проведённой раздаче", reportNow) {
		t.Fatal("reversed entry still blocks person")
	}
}

func TestDistributionMustBalanceBeforePageCommits(t *testing.T) {
	s, _ := reportWeek(t)
	for _, field := range []string{"lines", "inflows"} {
		bad := distribution(2, "posted", cutDay, "alfa", 81)
		d := bad["distribution"].(map[string]any)
		d[field].([]map[string]any)[0]["baseMills"] = 11000
		st, err := readPots(t, s, potPage{next: "2", changes: []map[string]any{distribution(1, "posted", cutDay, "bravo", 80), bad}})
		if err == nil || st.Cursor != "" || st.State != "error" || count(t, s, `SELECT count(*) FROM hub_pot_distributions`) != 0 {
			t.Fatalf("unbalanced %s: %+v, %v", field, st, err)
		}
	}
}

func TestPotProblemsFollowMoscowCutoverAndActiveModels(t *testing.T) {
	s, ctx := reportBook(t)
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Alpha": {"Браво": pageTab(cutDay, "Page A", 0.25, 0, [7]any{})},
		"Beta":  {"Альфа": pageTab(cutDay, "Page C", 0.2, 0, [7]any{})},
	})
	setReports(t, s, ctx, cutDay)
	exec(t, s, `UPDATE settings SET value='2026-12-01' WHERE key='pot_shares_hub_from'`)
	before := at("2026-11-30T20:59:59Z")
	after := before.Add(time.Second)
	for _, state := range []string{"error", "stale", "dirty"} {
		exec(t, s, `UPDATE hub_pot_sync SET state='ok',error='',dirty=0,last_ok=?`, after.Format(time.RFC3339))
		switch state {
		case "error":
			exec(t, s, `UPDATE hub_pot_sync SET state='error',error='fixture outage'`)
		case "stale":
			exec(t, s, `UPDATE hub_pot_sync SET last_ok='2026-11-22'`)
		case "dirty":
			exec(t, s, `UPDATE hub_pot_sync SET dirty=1`)
		}
		for _, person := range []string{"alfa", "bravo"} {
			if potFlag(t, s, person, "", before) || !potFlag(t, s, person, "", after) {
				t.Fatalf("%s, %s: first pot share must block only from the cutover", state, person)
			}
		}
	}
	exec(t, s, `UPDATE hub_pot_sync SET dirty=0`)
	exec(t, s, `INSERT INTO hub_pot_distribution_held VALUES(1,21,'["page-a"]','page_outside_key')`)
	if potFlag(t, s, "alfa", "", after) || !potFlag(t, s, "bravo", "page_outside_key", after) {
		t.Fatal("held page must block only its model")
	}
}

func TestPrepareKeepsUnpaidHeldShiftAndPotBreakdown(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, cutDay)
	enablePots(t, s)
	deliverPot(t, s, 1, "posted", cutDay, "alfa", 83)
	brief, err := payroll.PreparePayout(ctx, s.RO, "alfa", reportNow)
	if err != nil || !strings.Contains(brief.Message, "раздача 73") {
		t.Fatal(brief.Message, err)
	}
	st, err := readPots(t, s, potPage{since: "1", next: "2", held: []map[string]any{{"seq": 2, "distributionId": 73, "reason": "page_outside_key", "pageLabels": []string{"page-a"}}}})
	if err != nil || st.Held != 1 {
		t.Fatal(st, err)
	}
	if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE person='alfa' AND day=?`, cutDay); got != "0" {
		t.Fatal(got)
	}
	held, err := payroll.PreparePayout(ctx, s.RO, "alfa", reportNow)
	if err != nil || held.Message != brief.Message || held.AmountMicros != brief.AmountMicros {
		t.Fatal(brief.Message, held.Message, err)
	}
}

func TestMissingShiftDoesNotRetractMixedPotBooking(t *testing.T) {
	s, ctx := reportWeek(t)
	setReports(t, s, ctx, cutDay)
	enablePots(t, s)
	deliverPot(t, s, 1, "posted", cutDay, "alfa", 84)
	want := dayOf(t, s, "alfa", cutDay)
	// Simulate missing report pages without an authoritative message edit.
	exec(t, s, `DELETE FROM shift_report_post_pages WHERE post_id IN (SELECT id FROM shift_report_posts WHERE person='alfa')`)
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	if got := dayOf(t, s, "alfa", cutDay); got != want {
		t.Fatalf("remaining pot replaced missing shift: %s -> %s", want, got)
	}
	deliverPot(t, s, 2, "reversed", cutDay, "alfa", 84)
	if got := dayOf(t, s, "alfa", cutDay); got != want {
		t.Fatalf("missing shift+pot released: %s -> %s", want, got)
	}
}

func TestShadowPotChecksAndCleanRollback(t *testing.T) {
	s, ctx := reportWeek(t)
	check := func() bool { return checks.HubPots(ctx, checks.Env{Q: s.RO, Now: reportNow}).OK }
	if _, err := readPots(t, s, potPage{status: 500}); err == nil || check() {
		t.Fatal("shadow server error passed")
	}
	if _, err := readPots(t, s, potPage{next: "0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readPots(t, s, potPage{since: "0", status: 404}); err == nil || check() {
		t.Fatal("shadow 404 after first read passed")
	}
	if _, err := readPots(t, s, potPage{since: "0", next: "0"}); err != nil {
		t.Fatal(err)
	}
	declare(t, s, ctx, "demo-removed", "Тест", "Alpha", []string{"page-a"}, "alfa")
	potSale(t, s, ctx, "demo-removed", "demo-ref", "page-a", cutDay+"T12:00:00+03:00", 7)
	exec(t, s, `UPDATE pot_sales SET removed_at=?`, cutDay)
	if err := hub.SetPotSharesFrom(ctx, s, cutDay, false, reportNow); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"dirty=1", "last_ok=NULL", "last_ok='2001-01-01'", "state='error',error='fixture'"} {
		exec(t, s, `UPDATE hub_pot_sync SET `+change)
		if err := hub.SetPotSharesFrom(ctx, s, "", false, reportNow); err == nil {
			t.Fatalf("disabled with %s", change)
		}
		exec(t, s, `UPDATE hub_pot_sync SET dirty=0,state='ok',error='',last_ok=?`, reportNow.Format(time.RFC3339))
	}
}

func TestMixedCaseModelCombinesReportVersionsRefsAndPot(t *testing.T) {
	for _, mapping := range []string{"", `{"aLpHa":"Alpha"}`} {
		s, ctx := reportWeek(t)
		exec(t, s, `UPDATE settings SET value=? WHERE key='hub_model_slugs'`, mapping)
		exec(t, s, `UPDATE hub_pages SET model_slug=CASE page_label WHEN 'page-a' THEN 'aLPhA' ELSE 'ALPHA' END WHERE page_label IN ('page-a','page-b')`)
		setReports(t, s, ctx, cutDay)
		setHub(t, s, ctx, cutDay)
		enablePots(t, s)
		handIn(t, s, hubVersion(41, 11, 1, "alfa", cutDay, hubPage{"page-a", 4}), hubVersion(42, 12, 1, "alfa", cutDay, hubPage{"page-b", 8}))
		c := distribution(1, "posted", cutDay, "alfa", 121)
		d := c["distribution"].(map[string]any)
		d["pot"].(map[string]any)["modelSlug"] = "AlPhA"
		d["formula"].(map[string]any)["kind"] = "fixture-future"
		if _, err := readPots(t, s, potPage{since: "0", next: "1", changes: []map[string]any{c}}); err != nil {
			t.Fatal(err)
		}
		if got := cols(t, s, `SELECT net_micros,hub_reports,sale_refs,json_array_length(pages),json_extract(hub_pots,'$[0].basis') FROM v_booked_report_days WHERE person='alfa' AND day=?`, cutDay); got != `24000000 [41,42] ["4100","4200"] 2 fixture-future` {
			t.Fatal(mapping, got)
		}
		if potFlag(t, s, "alfa", "", reportNow) {
			t.Fatal("canonical distribution incorrectly held")
		}
	}
}

func TestInactiveDraftLinesDoNotReservePotSales(t *testing.T) {
	for _, mode := range []string{"hub basis", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := reportWeek(t)
			setReports(t, s, ctx, cutDay)
			setHub(t, s, ctx, cutDay)
			enablePots(t, s)
			if mode == "hub basis" {
				handIn(t, s, hubVersion(52, 10, 1, "alfa", cutDay, hubPage{"page-a", 8}))
			}
			savePotDraft(t, s, ctx, "demo-shadow-ref")
			if mode == "superseded" {
				exec(t, s, `UPDATE shift_reports SET status='superseded' WHERE source='agent'`)
				if err := reports.Changed(ctx, s, reportNow); err != nil {
					t.Fatal(err)
				}
			}
			deliverPot(t, s, 1, "posted", cutDay, "bravo", 91)
			// An unresolved reference on this page must not hold the pot.
			if got := cols(t, s, `SELECT pot_micros FROM report_accruals WHERE person='bravo' AND day=?`, cutDay); got != "12000000" {
				t.Fatal(got)
			}
			exec(t, s, `INSERT INTO events(source,source_id,kind,occurred_at,received_at,payload) VALUES('hub','fixture-inflow','transactions',?,?, '{}')`, cutDay, cutDay)
			exec(t, s, `INSERT INTO hub_transactions(page_label,key,occurred_at,net_mills,currency,type,ref,fields,hash,event_id,first_seen_at,updated_at)
 VALUES('page-a','91',?,12000,'USD','tip','demo-shadow-ref','{}','h',(SELECT id FROM events WHERE source_id='fixture-inflow'),?,?)`, cutDay, cutDay, cutDay)
			if potFlag(t, s, "alfa", "", reportNow) || potFlag(t, s, "bravo", "", reportNow) {
				t.Fatal("inactive draft claimed a booked pot")
			}
		})
	}
}

func TestRebuildScopesHeldAndUnavailablePotFeed(t *testing.T) {
	s, ctx := reportBook(t)
	tables(t, s, ctx, map[string]map[string][][]payroll.Cell{
		"Beta":  {"Альфа": pageTab(cutDay, "Page C", 0.25, 0, [7]any{})},
		"Alpha": {"Браво": pageTab(cutDay, "Page A", 0.2, 0, [7]any{})},
	})
	setReports(t, s, ctx, cutDay)
	post(t, s, 51, "501", cutDay, postText(cutDay, "", "C - $8"))
	post(t, s, 52, "502", cutDay, postText(cutDay, "", "A - $10"))
	enablePots(t, s)
	if _, err := readPots(t, s, potPage{since: "0", next: "2", held: []map[string]any{{"seq": 2, "distributionId": 73, "pageLabels": []string{"page-c"}, "reason": "page_outside_key"}}}); err != nil {
		t.Fatal(err)
	}
	post(t, s, 51, "501", cutDay, postText(cutDay, "", "C - $12"))
	post(t, s, 52, "502", cutDay, postText(cutDay, "", "A - $15"))
	if got := dayOf(t, s, "alfa", cutDay); got != "2000000 0 2000000 reports" {
		t.Fatal("held model changed", got)
	}
	if got := dayOf(t, s, "bravo", cutDay); got != "3000000 0 3000000 reports" {
		t.Fatal("unrelated model held", got)
	}
	if _, err := readPots(t, s, potPage{since: "2", status: 404}); err == nil {
		t.Fatal("missing feed passed")
	}
	post(t, s, 52, "502", cutDay, postText(cutDay, "", "A - $20"))
	if got := dayOf(t, s, "bravo", cutDay); got != "3000000 0 3000000 reports" {
		t.Fatal("unknown first pot did not hold tab", got)
	}
	// Simulate a report for tomorrow posted on the evening before the switch.
	if _, err := payroll.Rebuild(ctx, s, at("2026-11-09T23:59:59+03:00")); err != nil {
		t.Fatal(err)
	}
	if got := dayOf(t, s, "bravo", cutDay); got != "4000000 0 4000000 reports" {
		t.Fatal("future cutover held the rebuild", got)
	}
}
