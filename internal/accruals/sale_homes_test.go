package accruals

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func TestTransferredShareAndShiftsUseHubPersonAndReportDay(t *testing.T) {
	s, ctx := open(t)
	first := work(401, "shift", "2026-11-10 13:00", "2026-11-10 18:00", "page-a")
	second := work(402, "shift", "2026-11-11 13:00", "2026-11-11 18:00", "page-b")
	first.Pages[0].SalesFrom, second.Pages[0].SalesFrom = nil, nil // shifts worked the pages without holding sales
	first.PersonCode, second.PersonCode = "bravo", "bravo"
	first.ReportDate, second.ReportDate = "2026-11-10", "2026-11-10"
	putWindow(t, s, "2026-11-02", nil, nil)
	putWindow(t, s, "2026-11-09", []hub.TeamWork{first, second}, []hub.TeamSale{sale("41", "tip", "page-a", "2026-11-12 12:00", 15000)})
	putWindow(t, s, "2026-11-16", nil, nil)
	sheets(t, s, ctx, map[string][][]payroll.Cell{"Браво": tab("2026-11-09", 0.2, 3, [7]any{})})
	freshHomes(t, s)
	putHome(t, s, 41, "shift", "holder", "", share(first, 15000))
	rebuild(t, s, ctx)

	// Hub transfers the sale to the second login and changes its report day, independently of the work's own day.
	putHome(t, s, 41, "shift", "owner_split", "", hub.SaleHomeShare{UserID: 402, PersonCode: "bravo", WorkRef: second.Ref,
		ReportDate: "2026-11-13", DaySource: "report", Percent: new(100), Mills: new(int64(15000))})
	setSwitch(t, s, ctx, switchDay)
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref='41' AND person='bravo' AND hub_user_id=402
		AND username='user402' AND day='2026-11-13' AND share_micros=15000000`); n != 1 {
		t.Fatal("incorrect transferred home")
	}
	if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE kind='fix'`); n != 1 {
		t.Fatalf("got %d fixes for the two logins", n)
	}
	for _, day := range []string{"2026-11-10", "2026-11-13"} {
		if n := count(t, s, `SELECT count(*) FROM payable_days WHERE person='bravo' AND date=? AND source='facts' AND amount=3000000`, day); n != 1 {
			t.Errorf("payroll did not post %s: %s", day, books(t, s))
		}
	}
	// A credited login with no work still has the identity used by the report reader.
	putHome(t, s, 41, "shift", "owner_split", "", hub.SaleHomeShare{UserID: 403, PersonCode: "bravo",
		ReportDate: "2026-11-13", DaySource: "sale_day", Percent: new(100), Mills: new(int64(15000))})
	rebuild(t, s, ctx)
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE person='bravo' AND hub_user_id=403 AND username='hub:403'`); n != 1 {
		t.Fatal("missing fallback login")
	}
}

func TestSaleHomeCoverageGatesFacts(t *testing.T) {
	for _, tc := range []struct{ name, mutation, reason string }{
		{"unresolved", `UPDATE hub_sale_homes SET payload=json_set(payload,'$.shares[0].reportDate',null,'$.shares[0].daySource','unresolved') WHERE transaction_id=1`, "нет даты отчёта"},
		{"missing", `DELETE FROM hub_sale_homes WHERE transaction_id=1`, "нет дома активной продажи"},
		{"held", `INSERT INTO hub_sale_homes_held VALUES(77,66,'page_outside_key')`, "придержал"},
		{"stale", `UPDATE hub_sale_homes_sync SET last_ok='2026-11-17T10:59:59+03:00'`, "больше часа"},
		{"never read", `UPDATE hub_sale_homes_sync SET last_ok=NULL,state='new'`, "не читалась"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := switchWeek(t)
			setSwitch(t, s, ctx, switchDay)
			before := books(t, s)
			exec(t, s, tc.mutation)
			if tc.name == "stale" {
				// Payroll can run even if the collector stopped.
				if _, err := payroll.Rebuild(ctx, s, switchNow); err != nil {
					t.Fatal(err)
				}
			} else {
				rebuild(t, s, ctx)
			}
			got := rows(t, s, `SELECT status,reasons FROM fact_weeks WHERE week='2026-11-09'`)
			if !strings.HasPrefix(got, "unknown") || !strings.Contains(got, tc.reason) {
				t.Fatal(got)
			}
			if tc.name == "unresolved" {
				if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref='1' AND day IS NULL AND share_micros=100000000`); n != 1 {
					t.Fatal("undated share lost")
				}
				if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE person='alfa' AND kind='sales'`); n != 0 {
					t.Fatal("undated share accrued")
				}
			}
			if after := books(t, s); after != before {
				t.Fatalf("unknown facts changed posted payroll:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

type homesHTTPPage struct {
	since, next string
	more        bool
	homes, held []map[string]any
	status      int
}

func syncHomesPages(t *testing.T, s *db.Store, pages ...homesHTTPPage) (hub.HomesStatus, error) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls >= len(pages) {
			t.Error("unexpected extra page")
			w.WriteHeader(400)
			return
		}
		p := pages[calls]
		calls++
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != hub.SaleHomesPath || r.Header.Get("Authorization") != "Bearer test" ||
			q.Get("limit") != "500" || q.Get("since") != p.since || (p.since == "" && q.Has("since")) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if p.status != 0 {
			w.WriteHeader(p.status)
			return
		}
		if p.homes == nil {
			p.homes = []map[string]any{}
		}
		if p.held == nil {
			p.held = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"homes": p.homes, "held": p.held, "next": p.next, "more": p.more, "serverNow": switchNow})
	}))
	defer srv.Close()
	st, err := hub.SyncSaleHomes(t.Context(), s, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "test"}, nil), switchNow)
	if calls != len(pages) {
		t.Errorf("requested %d/%d pages", calls, len(pages))
	}
	return st, err
}

func TestHomesFeedInitial404AndFreshness(t *testing.T) {
	s, ctx := open(t)
	health := func(now time.Time) checks.Result { return checks.HubHomes(ctx, checks.Env{Q: s.RO, Now: now}) }
	st, err := syncHomesPages(t, s, homesHTTPPage{status: 404})
	if err != nil || st.LastOK != "" || !health(switchNow).OK || count(t, s, `SELECT count(*) FROM cursors WHERE name='hub:sale_homes'`) != 0 {
		t.Fatalf("initial 404: %+v %v", st, err)
	}
	st, err = syncHomesPages(t, s, homesHTTPPage{next: "0"})
	if err != nil || st.LastOK != ts.Format(switchNow) || !health(switchNow.Add(time.Hour)).OK || health(switchNow.Add(time.Hour+time.Second)).OK {
		t.Fatalf("empty successful feed: %+v %v", st, err)
	}
	st, err = syncHomesPages(t, s, homesHTTPPage{since: "0", status: 404})
	if err == nil || st.LastOK != ts.Format(switchNow) || st.Cursor != "0" || health(switchNow).OK {
		t.Fatalf("later 404: %+v %v", st, err)
	}
}

func TestHomesPagesAreAtomicAndARetractionSurvivesReplay(t *testing.T) {
	s, ctx := open(t)
	putWindow(t, s, "2026-11-02", nil, nil)
	putWindow(t, s, "2026-11-09", nil, []hub.TeamSale{sale("51", "tip", "page-a", "2026-11-11 12:00", 7000)})
	putWindow(t, s, "2026-11-16", nil, nil)
	up := map[string]any{"seq": 1, "at": switchNow, "op": "upsert", "transactionId": 51, "pageLabel": "page-a", "occurredAt": "2026-11-11T09:00:00Z",
		"netMills": 7000, "saleKind": "tip", "home": "nobody", "reason": "no_holder", "pot": nil, "waitsUntil": nil, "ruleVersion": "fixture", "shares": []int{}}
	retract := map[string]any{"seq": 4, "at": switchNow, "op": "retract", "transactionId": 51, "pageLabel": "page-a", "occurredAt": nil,
		"netMills": nil, "saleKind": nil, "home": nil, "reason": nil, "pot": nil, "waitsUntil": nil, "ruleVersion": nil, "shares": []int{}}
	held := []map[string]any{{"seq": 2, "transactionId": 52, "reason": "page_outside_key"}}

	// A short held-only page is not EOF. On the final page the held list replaces the previous one.
	st, err := syncHomesPages(t, s, homesHTTPPage{next: "2", held: held, more: true}, homesHTTPPage{since: "2", next: "3", homes: []map[string]any{up}, held: held})
	if err != nil || st.Cursor != "3" || st.Held != 1 || count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref='51'`) != 1 {
		t.Fatalf("pages: %+v %v", st, err)
	}
	if checks.HubHomes(ctx, checks.Env{Q: s.RO, Now: switchNow}).OK {
		t.Fatal("held passed health")
	}

	// Fail the last write of the page, after both the retract and the replacement of held.
	exec(t, s, `CREATE TRIGGER reject_cursor BEFORE UPDATE ON cursors WHEN NEW.name='hub:sale_homes' BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	page := homesHTTPPage{since: "3", next: "4", homes: []map[string]any{retract}}
	if _, err := syncHomesPages(t, s, page); err == nil {
		t.Fatal("cursor failure accepted")
	}
	if st, err := hub.ReadHomesStatus(ctx, s.RO); err != nil || st.Cursor != "3" || st.Held != 1 || count(t, s, `SELECT seq FROM hub_sale_homes WHERE transaction_id=51 AND retracted=0`) != 1 {
		t.Fatalf("partial commit: %+v %v", st, err)
	}
	exec(t, s, `DROP TRIGGER reject_cursor`)
	st, err = syncHomesPages(t, s, page)
	if err != nil || st.Cursor != "4" || st.Held != 0 {
		t.Fatalf("retry: %+v %v", st, err)
	}

	for _, seq := range []int{1, 4} {
		up["seq"] = seq
		if _, err = syncHomesPages(t, s, homesHTTPPage{since: "4", next: "4", homes: []map[string]any{up}}); err != nil {
			t.Fatal(err)
		}
		if got := rows(t, s, `SELECT seq,retracted FROM hub_sale_homes WHERE transaction_id=51`); got != "4 1" {
			t.Fatalf("replay %d: %s", seq, got)
		}
		if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref='51'`); n != 0 {
			t.Fatal("retracted home resurrected")
		}
	}
	if got := rows(t, s, `SELECT status,reasons FROM fact_weeks WHERE week='2026-11-09'`); !strings.Contains(got, "нет дома активной продажи 51") {
		t.Fatal(got)
	}
	up["seq"] = 5
	if _, err = syncHomesPages(t, s, homesHTTPPage{since: "4", next: "5", homes: []map[string]any{up}}); err != nil {
		t.Fatal(err)
	}
	if got := rows(t, s, `SELECT status FROM fact_weeks WHERE week='2026-11-09'`); got != "ok" {
		t.Fatalf("newer upsert: %s", got)
	}
}
