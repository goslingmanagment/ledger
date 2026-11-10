package hub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// day is the window [d, d+1) in UTC.
func day(d string) (time.Time, time.Time) {
	t := mustTime(d + "T00:00:00Z")
	return t, t.Add(24 * time.Hour)
}

func TestRowsHoldsAndExceptions(t *testing.T) {
	b := newBench(t)
	h := b.hub
	h.tx("alfa-1", "1", "2026-09-02T10:00:00Z", "tip", 10_000, "USD", "101")
	h.tx("alfa-1", "2", "2026-09-03T10:00:00Z", "other", 50_000, "USD", "5001")     // приз
	h.tx("alfa-1", "3", "2026-09-04T10:00:00Z", "refund", -5_000, "USD", "5002")    // возврат
	h.tx("alfa-1", "4", "2026-09-05T10:00:00Z", "chargeback", 1_000, "USD", "5003") // исключить: any type
	h.tx("alfa-1", "5", "2026-09-06T10:00:00.000Z", "subscription", 2_500, "USD", "105")
	h.tx("alfa-1", "6", "2026-10-01T10:00:00Z", "other", 9_000, "USD", "106")   // no decision
	h.tx("alfa-1", "7", "2026-10-03T10:00:00Z", "tip", -1_000, "USD", "107")    // minus without «возврат»
	h.tx("alfa-1", "8", "2026-10-04T10:00:01Z", "other", 3_000, "USD", "6001")  // decision a second off
	h.tx("alfa-1", "9", "2026-10-05T10:00:00Z", "other", 3_006, "USD", "6002")  // net 0.006 off
	h.tx("alfa-1", "10", "2026-10-06T10:00:00Z", "other", 3_004, "USD", "6003") // net 0.004 off: fine
	h.tx("alfa-1", "11", "2026-10-07T10:00:00Z", "tip", 3_000, "USD", "6004")   // приз on a tip
	h.tx("alfa-1", "12", "2026-10-08T10:00:00Z", "tip", 3_000, "USD", "108")    // made EUR in the mirror below
	raw, _ := os.ReadFile("testdata/hub-exceptions.md")
	importExceptions(t, b, "hub-exceptions.md", string(raw)+"\n"+
		"| Страница | transactionRef | Дата UTC | Net | Решение | Основание |\n|---|---|---|---|---|---|\n"+
		"| alfa-1 | 6001 | 2026-10-04 10:00:00 | 3.00 | приз | x |\n"+
		"| alfa-1 | 6002 | 2026-10-05 10:00:00 | 3.00 | приз | x |\n"+
		"| alfa-1 | 6003 | 2026-10-06 10:00:00 | 3.00 | приз | x |\n"+
		"| alfa-1 | 6004 | 2026-10-07 10:00:00 | 3.00 | приз | x |\n")
	if r := b.result(b.sync("alfa-1"), "alfa-1", "transactions"); r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	b.exec(`UPDATE hub_transactions SET currency = 'EUR' WHERE key = '12'`)

	c, err := Rows(b.ctx, b.store.RO, "alfa-1", mustTime("2026-09-01"), mustTime("2026-09-10"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 4 || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "исключена строка 5003 — синтетическая ошибка платформы") {
		t.Fatalf("%+v", c)
	}
	var sum int64
	for _, r := range c.Rows {
		sum += r.NetMills
	}
	if sum != 57_500 {
		t.Fatalf("net %d", sum)
	}
	hold := 5 * 24 * time.Hour
	tip, prize, refund, sub := c.Rows[0], c.Rows[1], c.Rows[2], c.Rows[3]
	if tip.Prize || tip.Avail.Sub(tip.At) != hold || sub.Avail.Sub(sub.At) != hold {
		t.Fatalf("hold types: %+v %+v", tip, sub)
	}
	if !prize.Prize || !prize.Avail.Equal(prize.At) || prize.Decision != "приз" {
		t.Fatalf("prize: %+v", prize)
	}
	if refund.Prize || !refund.Avail.Equal(refund.At) || refund.NetMills != -5000 {
		t.Fatalf("refund: %+v", refund)
	}

	refused := map[string]string{
		"2026-10-01": "строка other 2026-10-01T10:00:00.000Z USD 9.00 (ref 106) — холд или решение не известны",
		"2026-10-03": "строка tip 2026-10-03T10:00:00.000Z USD -1.00 (ref 107)",
		"2026-10-04": "строка 6001 (other 2026-10-04 10:00:01 UTC 3.00) не совпадает с hub_exceptions (приз, 2026-10-04 10:00:00, 3.00)",
		"2026-10-05": "строка 6002 (other 2026-10-05 10:00:00 UTC 3.01) не совпадает",
		"2026-10-07": "строка 6004 (tip 2026-10-07 10:00:00 UTC 3.00) не совпадает",
		"2026-10-08": "строка tip 2026-10-08T10:00:00.000Z EUR 3.00 (ref 108)",
	}
	for d, want := range refused {
		a, z := day(d)
		if _, err := Rows(b.ctx, b.store.RO, "alfa-1", a, z); !IsRefusal(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", d, err, want)
		}
	}
	a, z := day("2026-10-06")
	if c, err := Rows(b.ctx, b.store.RO, "alfa-1", a, z); err != nil || len(c.Rows) != 1 || !c.Rows[0].Prize {
		t.Fatalf("net within 0.005: %+v %v", c, err)
	}
	// Past the proven window there is no answer, not an empty one.
	if _, err := Rows(b.ctx, b.store.RO, "alfa-1", mustTime("2026-10-01"), b.clock.Now().Add(time.Hour)); !IsRefusal(err) ||
		!strings.Contains(err.Error(), "вне доказанного") {
		t.Fatalf("future window: %v", err)
	}
}

func TestMonthNet(t *testing.T) {
	b := newBench(t)
	h := b.hub
	h.grant = append(h.grant, GrantPage{PageLabel: "beta-1", Platform: "fansly", ModelSlug: "Beta", ModelName: "Beta"},
		GrantPage{PageLabel: "gamma-1", Platform: "fansly", ModelSlug: "Alfaa", ModelName: "Alfa"})
	h.tx("alfa-1", "1", "2026-09-30T20:59:59Z", "tip", 10_000, "USD", "101")        // 23:59:59 MSK: September
	h.tx("alfa-1", "2", "2026-09-30T21:00:00Z", "tip", 20_000, "USD", "102")        // 00:00 MSK: October
	h.tx("alfa-1", "3", "2026-09-05T10:00:00Z", "chargeback", 1_000, "USD", "5003") // исключить
	h.tx("alfa-1", "4", "2026-10-20T10:00:00Z", "subscription", 4_000, "USD", "104")
	h.tx("alfa-of", "1", "2026-09-10T10:00:00Z", "message_purchase", 3_333, "USD", "201")
	h.tx("beta-1", "1", "2026-10-02T10:00:00Z", "post_purchase", 1_500, "USD", "301")
	h.tx("gamma-1", "1", "2026-08-20T12:00:00Z", "tip", 1_000, "USD", "401")
	h.tx("gamma-1", "2", "2026-09-07T10:00:00Z", "other", 2_000, "USD", "402") // no decision: the summary still counts it
	h.floors["gamma-1/transactions"] = mustTime("2026-08-20T12:00:00Z")
	raw, _ := os.ReadFile("testdata/hub-exceptions.md")
	importExceptions(t, b, "hub-exceptions.md", string(raw))
	rep := b.sync()
	if n := rep.Unknown(); n != 0 {
		t.Fatalf("%+v", rep.Results)
	}
	cut, err := DefaultAsOf(b.ctx, b.store.RO)
	if err != nil || !cut.Equal(b.clock.Now().Truncate(time.Second)) {
		t.Fatalf("default cut %v %v", cut, err)
	}
	sep, err := MonthNet(b.ctx, b.store.RO, "2026-09", cut)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"alfa-1": "10.00", "alfa-of": "3.333", "beta-1": "0.00"}
	for _, p := range sep.Pages {
		if p.Status != "ok" || p.Net != want[p.Page] {
			t.Errorf("September %s: %+v, want %s", p.Page, p, want[p.Page])
		}
	}
	if sep.Total != "13.333" || sep.Unknown != 0 || len(sep.Lines) != 3 {
		t.Fatalf("September total %s, lines %+v", sep.Total, sep.Lines)
	}
	if len(sep.Unrouted) != 1 || sep.Unrouted[0].Page != "gamma-1" || sep.Unrouted[0].Net != "2.00" || sep.Unrouted[0].Model != "Alfa" {
		t.Fatalf("unrouted %+v", sep.Unrouted)
	}
	aug, _ := MonthNet(b.ctx, b.store.RO, "2026-08", cut)
	if aug.Unrouted[0].Net != "1.00" {
		t.Fatalf("August of a page from its start: %+v", aug.Unrouted[0])
	}
	oct, err := MonthNet(b.ctx, b.store.RO, "2026-10", mustTime("2026-10-15T00:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	if oct.Pages[0].Net != "20.00" || oct.Lines[0].Net != "20.00" || oct.Total != "21.50" {
		t.Fatalf("October cut mid-month: %+v %+v %s", oct.Pages, oct.Lines, oct.Total)
	}
	b.hub.extra["beta-1/transactions"] = []string{"plane_not_read"}
	b.clock.advance(time.Hour)
	b.sync("beta-1")
	oct, _ = MonthNet(b.ctx, b.store.RO, "2026-10", mustTime("2026-10-15T00:00:00+03:00"))
	if oct.Unknown != 1 || oct.Total != "" || oct.Lines[2].Model != "Beta" || oct.Lines[2].Status != "unknown" ||
		oct.Lines[2].Net != "" || oct.Lines[0].Net != "20.00" {
		t.Fatalf("an unproven page made a number: %+v", oct)
	}
}

func TestImportBusiness(t *testing.T) {
	b := newBench(t) // imported once already
	raw, _ := os.ReadFile("testdata/business.json")
	rep, err := ImportBusiness(b.ctx, b.store.RW, raw, b.clock.Now())
	if err != nil || rep.Routes != 3 || rep.RouteRates != 3 || rep.Settings != 15 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := b.count(`SELECT count(*) FROM route_rates`); n != 3 {
		t.Fatalf("re-import doubled the rates: %d", n)
	}
	if n := b.count(`SELECT min_payout FROM routes WHERE page_label = 'beta-1'`); n != 250_000_000 {
		t.Fatalf("min_payout %d", n)
	}
	var from string
	b.store.RW.QueryRow(`SELECT valid_from FROM route_rates WHERE method = '*' ORDER BY valid_from DESC LIMIT 1`).Scan(&from)
	if from != "2026-10-10T00:00:00+03:00" {
		t.Fatalf("valid_from %s", from)
	}
	bad := map[string]string{
		`"payee": "model"`:        `"payee": "fan"`,
		`"rate": "0.900"`:         `"rate": "1.5"`,
		`"hub_rpm": "30"`:         `"hub_rpm": "0"`,
		`"hold_days": "5"`:        `"hold_days": "five"`,
		`"source"`:                `"sourse"`,
		`"page_label": "alfa-of"`: `"page_label": "Alfa_of"`,
	}
	for from, to := range bad {
		broken := strings.Replace(string(raw), from, to, 1)
		if _, err := ImportBusiness(b.ctx, b.store.RW, []byte(broken), b.clock.Now()); err == nil {
			t.Errorf("accepted %s", to)
		}
	}
	set, _ := LoadSettings(b.ctx, b.store.RW)
	if m, err := set.Map("hub_model_slugs"); err != nil || m["Alfaa"] != "Alfa" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := set.Int("no_such_key"); err == nil || !strings.Contains(err.Error(), "ledger import business") {
		t.Fatalf("missing key: %v", err)
	}
}

func TestImportReportSettings(t *testing.T) {
	b := newBench(t)
	seed := func(topics, authors string) error {
		raw, _ := json.Marshal(map[string]any{"source": "test", "settings": map[string]string{
			SettingReportTopics: topics, SettingReportAuthors: authors}})
		_, err := ImportBusiness(b.ctx, b.store.RW, raw, b.clock.Now())
		return err
	}
	if err := seed(`[{"peer": "-1001", "topic": 4, "model": "alfa", "pages": {" VIP ": "alfa-of"}}]`, `{"tg:501": "anna", "#Anna": "anna"}`); err != nil {
		t.Fatal(err)
	}
	set, _ := LoadSettings(b.ctx, b.store.RW)
	topics, err := ParseReportTopics(set.raw[SettingReportTopics])
	if err != nil || len(topics) != 1 || topics[0].Pages["vip"] != "alfa-of" {
		t.Fatalf("topics %+v %v", topics, err)
	}
	authors, err := ParseReportAuthors(set.raw[SettingReportAuthors])
	if err != nil || authors["#anna"] != "anna" || authors["tg:501"] != "anna" {
		t.Fatalf("authors %+v %v", authors, err)
	}
	for _, bad := range [][2]string{
		{`[{"peer": "-1001"}]`, `{}`},
		{`[{"peer": "-1001", "topic": 4, "extra": 1}]`, `{}`},
		{`[]`, `{"501": "anna"}`},
		{`[]`, `{"tg:x": "anna"}`},
		{`[]`, `{"#anna": ""}`},
	} {
		if err := seed(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %s %s", bad[0], bad[1])
		}
	}
}
