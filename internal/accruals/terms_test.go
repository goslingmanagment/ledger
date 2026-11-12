package accruals

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/checks"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
)

// The pay terms of Hub's people registry (ecosystem H3) on the week of reportWeek. The registry (testdata/hub-people.json,
// the feed's documented shape): Альфа 25 % on Alpha from 2 November and 30 % from the 11th, no fix — her tab says 25 %;
// Браво 20 % with no fix — her tab says 20 % and 10 per shift; a person who left and an agent, with no terms.

// registry is a stub of Hub's people feed.
type registry struct {
	Answer struct {
		People           []map[string]any `json:"people"`
		PayTermsIncluded bool             `json:"payTermsIncluded"`
		Excluded         int              `json:"keyGrantExcludedPages"`
		ServerNow        string           `json:"serverNow"`
	}
	absent bool     // an older Hub: 404
	asked  []string // the since of each request; "" — none
}

func loadRegistry(t *testing.T) *registry {
	t.Helper()
	raw, err := os.ReadFile("testdata/hub-people.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &registry{}
	if err := json.Unmarshal(raw, &r.Answer); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *registry) person(code string) map[string]any {
	for _, p := range r.Answer.People {
		if p["code"] == code {
			return p
		}
	}
	panic("no " + code)
}

// change is the owner's edit of a person: the person's updatedAt moves, the feed serves them again.
func (r *registry) change(code, at string, terms ...map[string]any) {
	p := r.person(code)
	p["updatedAt"] = at
	list := []any{}
	for _, t := range terms {
		list = append(list, t)
	}
	p["payTerms"] = list
}

func term(id int64, from, to string, bp, fixMills int64) map[string]any {
	var validTo any
	if to != "" {
		validTo = to
	}
	return map[string]any{"id": id, "modelSlug": "alpha-hub", "modelName": "Alpha", "validFrom": from, "validTo": validTo,
		"shareBasisPoints": bp, "fixPerShiftMills": fixMills, "note": nil, "updatedAt": "2026-11-16T09:00:00.000Z"}
}

// read serves the registry as Hub's feed does — only people changed at or after since; without read:pay_terms no terms —
// and reads it the way the job does.
func (r *registry) read(t *testing.T, s *db.Store) payroll.TermsStatus {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != hub.PeoplePath {
			http.NotFound(w, req)
			return
		}
		since := req.URL.Query().Get("since")
		r.asked = append(r.asked, since)
		if r.absent {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not_found", "message": "Route GET:/api/v1/agent/people not found"})
			return
		}
		var from time.Time
		if since != "" {
			from, _ = time.Parse(time.RFC3339, since)
		}
		people := []map[string]any{}
		for _, p := range r.Answer.People {
			at, _ := time.Parse(time.RFC3339Nano, p["updatedAt"].(string))
			if at.Before(from) {
				continue
			}
			c := map[string]any{}
			for k, v := range p {
				if k != "payTerms" || r.Answer.PayTermsIncluded {
					c[k] = v
				}
			}
			people = append(people, c)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"people": people, "payTermsIncluded": r.Answer.PayTermsIncluded,
			"keyGrantExcludedPages": 0, "serverNow": "2026-11-17T09:00:00.000Z"})
	}))
	defer srv.Close()
	st, err := payroll.SyncHubTerms(context.Background(), s, hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "k"}, nil), reportNow)
	if err != nil {
		t.Fatalf("read the registry: %+v %v", st, err)
	}
	return st
}

func setTerms(t *testing.T, s *db.Store, ctx context.Context, day string) {
	t.Helper()
	if err := payroll.SetTermsHubFrom(ctx, s.RW, day, reportNow); err != nil {
		t.Fatal(err)
	}
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
}

// termsWeek is reportWeek with the reports accruing from the 10th, as in production, and Альфа's post of the 12th (40).
func termsWeek(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := reportWeek(t)
	post(t, s, 3, "501", "2026-11-12", postText("2026-11-12", "", "A - $40"))
	setReports(t, s, ctx, "2026-11-10")
	return s, ctx
}

func termsDays(d Diff) string {
	var out []string
	for _, x := range d.TermsDays {
		out = append(out, x.Person+" "+x.Day+" "+x.Basis+" "+orDash(x.TabAmountMicros)+" "+orDash(x.HubAmountMicros)+" "+strings.Join(x.Differences, ", "))
	}
	return strings.Join(out, "|")
}

func orDash(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}

func TestTheRegistrysTermsAreKeptWithTheirValidity(t *testing.T) {
	s, ctx := termsWeek(t)
	reg := loadRegistry(t)
	st := reg.read(t, s)
	if !st.OK || st.People != 4 || st.Terms != 3 || st.Written != 3 || st.Next != "2026-11-12T10:00:00.000Z" || reg.asked[0] != "" {
		t.Fatalf("first read %+v, asked %q", st, reg.asked)
	}
	if got, want := cols(t, s, `SELECT hub_id, person, model_slug, valid_from, COALESCE(valid_to, '-'), share_bp, fix_micros, COALESCE(gone_at, '-')
		FROM hub_pay_terms ORDER BY hub_id`), strings.Join([]string{
		"201 alfa alpha-hub 2026-11-02 2026-11-10 2500 0 -",
		"202 alfa alpha-hub 2026-11-11 - 3000 0 -",
		"203 bravo alpha-hub 2026-11-02 - 2000 0 -",
	}, "\n"); got != want {
		t.Fatalf("terms:\n%s\nwant\n%s", got, want)
	}
	if got := cols(t, s, `SELECT code, kind, state FROM hub_people ORDER BY hub_id`); got != "alfa human working\nbravo human working\ncarol human left\nhelper agent working" {
		t.Fatalf("people:\n%s", got)
	}
	// The term of a day, the model as ledger names it; a change mid-month holds from its day.
	hubs, err := payroll.LoadHubTerms(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	for day, want := range map[string]int64{"2026-11-01": 0, "2026-11-02": 2500, "2026-11-10": 2500, "2026-11-11": 3000, "2026-11-30": 3000} {
		var got int64
		if h := hubs.On("alfa", "Alpha", day); h != nil {
			got = h.ShareBP
		}
		if got != want {
			t.Fatalf("Альфа on %s: %d, want %d", day, got, want)
		}
	}

	// Read again: from the greatest updatedAt less the overlap; what is stored writes nothing.
	if st := reg.read(t, s); st.Written != 0 || st.Gone != 0 || reg.asked[1] != "2026-11-12T09:50:00Z" || st.People != 1 {
		t.Fatalf("second read %+v, asked %q", st, reg.asked)
	}
	// The owner gives Браво a fix of 10 and takes Альфа's raise back: her first term holds on, the second is gone, kept.
	reg.change("bravo", "2026-11-16T09:00:00.000Z", term(203, "2026-11-02", "", 2000, 10_000))
	reg.change("alfa", "2026-11-16T09:01:00.000Z", term(201, "2026-11-02", "", 2500, 0))
	if st := reg.read(t, s); st.Written != 2 || st.Gone != 1 || st.Next != "2026-11-16T09:01:00.000Z" {
		t.Fatalf("after the owner's edit %+v", st)
	}
	if got := cols(t, s, `SELECT hub_id, COALESCE(valid_to, '-'), fix_micros, gone_at IS NOT NULL FROM hub_pay_terms ORDER BY hub_id`); got != "201 - 0 0\n202 - 0 1\n203 - 10000000 0" {
		t.Fatalf("terms after the edit:\n%s", got)
	}
	if hubs, _ := payroll.LoadHubTerms(ctx, s.RO); hubs.On("alfa", "Alpha", "2026-11-20").ShareBP != 2500 {
		t.Fatal("the gone raise still holds")
	}
}

func TestAKeyWithoutPayTermsKeepsNothing(t *testing.T) {
	s, ctx := termsWeek(t)
	reg := loadRegistry(t)
	reg.Answer.PayTermsIncluded = false
	st := reg.read(t, s)
	if !st.OK || !st.NoTerms || st.People != 0 || st.Next != "" || !strings.Contains(st.Error, "read:pay_terms") {
		t.Fatalf("read %+v", st)
	}
	if n := count(t, s, `SELECT (SELECT count(*) FROM hub_people) + (SELECT count(*) FROM hub_pay_terms)`); n != 0 {
		t.Fatalf("kept %d rows", n)
	}
	// One line, not a failure; the next pass asks from the first again.
	r := checks.PayTerms(ctx, checks.Env{Q: s.RO, Now: reportNow})
	if !r.OK || r.Summary != "ключу ledger в хабе не дано read:pay_terms — условия реестра не читаются" {
		t.Fatalf("health %+v", r)
	}
	if reg.read(t, s); reg.asked[1] != "" {
		t.Fatalf("asked %q", reg.asked)
	}
	// Even with the setting on: the headers price the days, and the check says why it fails.
	setTerms(t, s, ctx, "2026-11-10")
	if got := dayOf(t, s, "bravo", "2026-11-10"); got != "20000000 0 20000000 reports" {
		t.Fatalf("Браво's 10th without the registry: %s", got)
	}
	if r := checks.PayTerms(ctx, checks.Env{Q: s.RO, Now: reportNow}); r.OK || r.Severity != "warn" || !strings.Contains(r.Summary, "read:pay_terms") ||
		!strings.Contains(r.Summary, "bravo (Alpha) 2026-11-10, дней 1 — по шапке вкладки") {
		t.Fatalf("health with the setting on %+v", r)
	}
}

func TestAnOlderHubIsNothingYet(t *testing.T) {
	s, ctx := termsWeek(t)
	reg := loadRegistry(t)
	reg.absent = true
	if st := reg.read(t, s); !st.OK || !st.Absent || st.LastOKAt == "" {
		t.Fatalf("read %+v", st)
	}
	if r := checks.PayTerms(ctx, checks.Env{Q: s.RO, Now: reportNow}); !r.OK || !strings.Contains(r.Summary, "404") {
		t.Fatalf("health %+v", r)
	}
}

func TestTheRegistryChangesNoAccrualWhileTheSettingIsOff(t *testing.T) {
	s, ctx := termsWeek(t)
	before := everything(t, s, ctx)
	loadRegistry(t).read(t, s)
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	if after := everything(t, s, ctx); after != before {
		t.Fatalf("the registry moved the accruals with the setting off:\n%s\nwas\n%s", after, before)
	}
	// The comparison lists where they differ today, the registry first; the check passes.
	d, err := payroll.CompareTerms(ctx, s.RO, "2026-11-17")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.DifferText(); got != "alfa (Alpha) доля 30% ≠ 25%; bravo (Alpha) фикс 0 ≠ 10" || d.Differ != 2 || d.Equal != 0 {
		t.Fatalf("diff %q %+v", got, d)
	}
	r := checks.PayTerms(ctx, checks.Env{Q: s.RO, Now: reportNow})
	if !r.OK || r.Summary != "в тени, условия начисляет шапка вкладки: условия в реестре и во вкладке расходятся у 2 (реестр ≠ вкладка): "+
		"alfa (Alpha) доля 30% ≠ 25%; bravo (Alpha) фикс 0 ≠ 10" {
		t.Fatalf("health %+v", r)
	}
	// `accruals diff` lists the days whose terms differ; nothing accrues by the registry.
	w, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := termsDays(w); got != "alfa 2026-11-12 tab 10000000 12000000 доля 30% ≠ 25%|bravo 2026-11-10 tab 20000000 10000000 фикс 0 ≠ 10" || w.TermsFrom != "" {
		t.Fatalf("terms days %s (from %q)", got, w.TermsFrom)
	}
}

func TestFromTheTermsDayADayTakesTheRegistrysTermOfThatDay(t *testing.T) {
	s, ctx := termsWeek(t)
	reg := loadRegistry(t)
	reg.read(t, s)
	ninth := dayOf(t, s, "alfa", "2026-11-09")
	setTerms(t, s, ctx, "2026-11-10")
	// Альфа's 10th at 25 % (the same), her 12th at the raise of the 11th; Браво's 10th without the header's fix.
	for _, c := range []struct{ person, date, want string }{
		{"alfa", "2026-11-09", ninth},
		{"alfa", "2026-11-10", "35000000 0 35000000 reports"},
		{"alfa", "2026-11-12", "12000000 0 12000000 reports"},
		{"bravo", "2026-11-10", "10000000 0 10000000 reports"},
	} {
		if got := dayOf(t, s, c.person, c.date); got != c.want {
			t.Fatalf("%s %s: %s, want %s\n%s", c.person, c.date, got, c.want, books(t, s))
		}
	}
	if got := cols(t, s, `SELECT person, day, rate, fix_micros, amount_micros FROM report_accruals ORDER BY person, day`); got !=
		"alfa 2026-11-10 0.25 0 35000000\nalfa 2026-11-12 0.3 0 12000000\nbravo 2026-11-10 0.2 0 10000000" {
		t.Fatalf("report_accruals:\n%s", got)
	}
	w, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := termsDays(w); got != "alfa 2026-11-12 hub 10000000 12000000 доля 30% ≠ 25%|bravo 2026-11-10 hub 20000000 10000000 фикс 0 ≠ 10" ||
		w.TermsFrom != "2026-11-10" {
		t.Fatalf("terms days %s (from %q)", got, w.TermsFrom)
	}
	if r := checks.PayTerms(ctx, checks.Env{Q: s.RO, Now: reportNow}); !r.OK || r.Summary !=
		"условия начисляет реестр хаба с 2026-11-10: у всех 2 вкладок работающих людей есть условие на 2026-11-17, шапки вкладок не читаются" {
		t.Fatalf("health %+v", r)
	}
	// Idempotent.
	n0 := count(t, s, `SELECT count(*) FROM entries`)
	if rep, err := payroll.Rebuild(ctx, s, reportNow); err != nil || rep.Changed() || count(t, s, `SELECT count(*) FROM entries`) != n0 {
		t.Fatalf("a second rebuild %+v %v", rep, err)
	}

	// The registry loses Браво's term: her day keeps the header, the books follow at once, and the check fails for her.
	reg.change("bravo", "2026-11-16T09:00:00.000Z")
	if st := reg.read(t, s); st.Gone != 1 {
		t.Fatalf("read %+v", st)
	}
	if got := dayOf(t, s, "bravo", "2026-11-10"); got != "20000000 0 20000000 reports" {
		t.Fatalf("Браво's 10th by the header: %s", got)
	}
	r := checks.PayTerms(ctx, checks.Env{Q: s.RO, Now: reportNow})
	if r.OK || r.Severity != "warn" || r.Summary != "условия начисляет реестр хаба с 2026-11-10, но: bravo (Alpha) 2026-11-10, дней 1 — по шапке вкладки; "+
		"bravo (Alpha) на 2026-11-17 в реестре нет условия — условие на день ставит владелец в карточке человека (хаб, «График и условия»)" {
		t.Fatalf("health %+v", r)
	}

	// Off: the headers again.
	setTerms(t, s, ctx, "")
	if got := dayOf(t, s, "alfa", "2026-11-12"); got != "10000000 0 10000000 reports" {
		t.Fatalf("Альфа's 12th after off: %s", got)
	}
}

func TestAPaidDayDoesNotFollowTheRegistryByItself(t *testing.T) {
	s, ctx := termsWeek(t)
	// A payout of 50 pays Альфа's 9th (20) and 30 of her 10th (35).
	importPayouts(t, s, ctx, payoutRow(1, "2026-11-12", "Альфа", "Alpha", "50.00"))
	if _, err := payroll.Rebuild(ctx, s, reportNow); err != nil {
		t.Fatal(err)
	}
	fifo := books(t, s)
	reg := loadRegistry(t)
	reg.change("alfa", "2026-11-12T10:00:00.000Z", term(201, "2026-11-02", "", 2000, 0))
	reg.read(t, s)
	setTerms(t, s, ctx, "2026-11-10")
	// The paid 10th by the registry would be 28: it waits for the owner, the books stay; the unpaid 12th follows (8).
	if got := pendingRows(t, s); got != "1 alfa 2026-11-10 35000000 28000000 terms_changed open  " {
		t.Fatalf("pending:\n%s", got)
	}
	if got := dayOf(t, s, "alfa", "2026-11-10"); got != "35000000 30000000 5000000 reports" {
		t.Fatalf("the paid 10th moved: %s\n%s\nwas\n%s", got, books(t, s), fifo)
	}
	if got := dayOf(t, s, "alfa", "2026-11-12"); got != "8000000 0 8000000 reports" {
		t.Fatalf("the unpaid 12th: %s", got)
	}
}
