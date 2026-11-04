package month

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// golden.json is synthetic: testdata/make_golden.py made it up with a fixed seed and asked the original
// monthly card script for the numbers. The port must give the same numbers.
type golden struct {
	AsOf      string `json:"as_of"`
	StartBeta string `json:"start_beta"`
	Routes    []struct {
		Page, Model, Channel, Payee string
		Rate, Min                   float64
	} `json:"routes"`
	Payments map[string][]struct {
		Key      string `json:"key"`
		T        string `json:"t"`
		NetMills int64  `json:"net_mills"`
		Prize    bool   `json:"prize"`
		Avail    string `json:"avail"`
	} `json:"payments"`
	Payouts map[string][]struct {
		Key         string `json:"key"`
		T           string `json:"t"`
		AmountMills int64  `json:"amount_mills"`
		Status      string `json:"status"`
		Method      string `json:"method"`
		Updated     string `json:"updated"`
	} `json:"payouts"`
	OFPayouts map[string][]struct {
		Key         string `json:"key"`
		T           string `json:"t"`
		AmountMills int64  `json:"amount_mills"`
		Status      string `json:"status"`
	} `json:"of_payouts"`
	Ledger   [][]string `json:"ledger"`
	Tabs     []Tab      `json:"tabs"`
	Expected map[string]struct {
		Totals map[string]float64 `json:"totals"`
		Lines  []map[string]any   `json:"lines"`
		Exp    struct {
			Chat  map[string]float64 `json:"chat"`
			Extra [][]any            `json:"extra"`
			Cats  map[string]float64 `json:"cats"`
			Total float64            `json:"total"`
		} `json:"expenses"`
		Transfers []struct {
			N         int
			Amount    float64
			Est, K, M float64
			Month     float64 `json:"month"`
			Bad       []string
		} `json:"transfers"`
		Requests []struct {
			Page     string  `json:"page"`
			T        string  `json:"t"`
			Amount   float64 `json:"amount"`
			Avail    float64 `json:"avail"`
			Transfer any     `json:"transfer"`
		} `json:"requests"`
	} `json:"expected"`
}

type fakeSource struct{ g *golden }

func parseT(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func (f fakeSource) Rows(page string, from, to time.Time) (hub.Checked, error) {
	var ck hub.Checked
	for _, r := range f.g.Payments[page] {
		t := parseT(r.T)
		if !t.Before(from) && t.Before(to) {
			ck.Rows = append(ck.Rows, hub.Row{Page: page, Key: r.Key, At: t, NetMills: r.NetMills, Prize: r.Prize, Avail: parseT(r.Avail)})
		}
	}
	sort.SliceStable(ck.Rows, func(i, j int) bool {
		if !ck.Rows[i].At.Equal(ck.Rows[j].At) {
			return ck.Rows[i].At.Before(ck.Rows[j].At)
		}
		return ck.Rows[i].Key < ck.Rows[j].Key
	})
	return ck, nil
}

func (f fakeSource) Payouts(page string, from, to time.Time) ([]hub.Payout, error) {
	var out []hub.Payout
	for _, q := range f.g.Payouts[page] {
		t := parseT(q.T)
		if !t.Before(from) && t.Before(to) {
			out = append(out, hub.Payout{Page: page, Key: q.Key, At: t, AmountMills: q.AmountMills, Status: q.Status, Method: q.Method, Updated: parseT(q.Updated)})
		}
	}
	return out, nil
}

func (f fakeSource) OFPayouts(page string, from, to time.Time) ([]hub.Payout, error) {
	var out []hub.Payout
	for _, q := range f.g.OFPayouts[page] {
		t := parseT(q.T)
		if !t.Before(from) && t.Before(to) {
			out = append(out, hub.Payout{Page: page, Key: q.Key, At: t, AmountMills: q.AmountMills, Status: q.Status, Method: "of", Updated: t})
		}
	}
	return out, nil
}

func (f fakeSource) Pages() ([]hub.Page, error) {
	return []hub.Page{{Label: "alfa-1", Model: "Alfa"}, {Label: "alfa-2", Model: "Alfa"}, {Label: "alfa-of", Model: "Alfa"},
		{Label: "beta-1", Model: "Beta"}, {Label: "gamma-1", Model: "Gamma"}, {Label: "delta-1", Model: "Delta"}}, nil
}
func (f fakeSource) SummaryNet(string, time.Time, time.Time) (float64, error) { return 123.456, nil }
func (f fakeSource) BybitIncomes(string) ([]float64, bool)                    { return nil, true }

func goldenConfig(g *golden) Config {
	c := Config{Models: []string{"Alfa", "Beta", "Gamma"}, Routes: map[string]Route{}, Share: 0.7, Hold: 7 * 24 * time.Hour,
		KLow: 0.985, KHigh: 1.015, Lookback: 100 * 24 * time.Hour, FullLedger: parseT("2026-08-01T00:00:00+03:00"),
		TabAliases: map[string]string{"Вася": "Василий"}, Names: map[string]string{"Alfa": "Альфа", "Beta": "Бета", "Gamma": "Гамма"}}
	c.Exc.Starts = map[string]time.Time{"beta-1": parseT(g.StartBeta)}
	c.Exc.Rows = map[hub.RowRef]hub.ExceptionRow{}
	c.Exc.Floors = map[string]string{}
	old := parseT("0001-01-01T00:00:00+03:00")
	for _, r := range g.Routes {
		lag := 3
		if r.Payee == "agency" {
			lag = 21
		}
		rt := Route{Page: r.Page, Model: r.Model, Channel: r.Channel, Rate: r.Rate, Min: r.Min, Payee: r.Payee, Lag: lag}
		if r.Channel == "Fansly" {
			rt.Rates = append(rt.Rates, MethodRate{"paxum", 0.958, old})
		}
		if r.Rate == 0.962 {
			rt.Rates = append(rt.Rates, MethodRate{"*", 0.973, parseT("2026-10-19T00:00:00+03:00")})
		}
		rt.Rates = append(rt.Rates, MethodRate{"*", r.Rate, old})
		c.Routes[r.Page] = rt
		c.Pages = append(c.Pages, r.Page)
	}
	return c
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPortMatchesTheScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	var rows []journal.Row
	for _, c := range g.Ledger {
		n, _ := strconv.Atoi(c[0])
		rows = append(rows, journal.Row{Num: n, Date: c[1], Type: c[2], Category: c[3], Counterparty: c[4], Model: c[5], Amount: c[6],
			Currency: c[7], Method: c[8], Status: c[9], Note: c[10]})
	}
	rows = journal.SortedByDate(rows)
	cv, err := BuildCover(goldenConfig(&g), fakeSource{&g}, rows, parseT(g.AsOf))
	if err != nil {
		t.Fatal(err)
	}
	for month, want := range g.Expected {
		card, err := BuildCard(cv, month, g.Tabs)
		if err != nil {
			t.Fatal(err)
		}
		snap := card.Snapshot()
		totals := snap["totals"].(map[string]any)
		for k, v := range want.Totals {
			if got, _ := totals[k].(float64); !near(got, v) {
				t.Errorf("%s totals.%s = %v, want %v", month, k, got, v)
			}
		}
		lines := snap["lines"].([]map[string]any)
		if len(lines) != len(want.Lines) {
			t.Errorf("%s lines %d, want %d", month, len(lines), len(want.Lines))
		}
		for _, wl := range want.Lines {
			found := false
			for _, l := range lines {
				if l["model"] == wl["model"] && l["channel"] == wl["channel"] {
					found = true
					for k, v := range wl {
						f, isNum := v.(float64)
						if !isNum {
							continue
						}
						got, _ := l[k].(float64)
						if !near(got, f) {
							t.Errorf("%s line %v/%v %s = %v, want %v", month, wl["model"], wl["channel"], k, got, f)
						}
					}
				}
			}
			if !found {
				t.Errorf("%s: no line %v/%v", month, wl["model"], wl["channel"])
			}
		}
		exp := card.Exp
		for k, v := range want.Exp.Chat {
			if !near(money6(exp.Chat.Vals[k]), v) {
				t.Errorf("%s chat %s = %v, want %v", month, k, exp.Chat.Vals[k], v)
			}
		}
		if len(exp.Cats.Keys) != len(want.Exp.Cats) {
			t.Errorf("%s cats %v, want %v", month, exp.Cats.Vals, want.Exp.Cats)
		}
		for k, v := range want.Exp.Cats {
			if !near(money6(exp.Cats.Vals[k]), v) {
				t.Errorf("%s cat %s = %v, want %v", month, k, exp.Cats.Vals[k], v)
			}
		}
		if len(exp.Extra) != len(want.Exp.Extra) {
			t.Errorf("%s extra %v, want %v", month, exp.Extra, want.Exp.Extra)
		}
		trs := snap["transfers"].([]map[string]any)
		if len(trs) != len(want.Transfers) {
			t.Errorf("%s transfers %d, want %d", month, len(trs), len(want.Transfers))
		}
		byN := map[int]map[string]any{}
		for _, tr := range trs {
			byN[tr["n"].(int)] = tr
		}
		for _, w := range want.Transfers {
			got := byN[w.N]
			if got == nil {
				t.Errorf("%s no transfer #%d", month, w.N)
				continue
			}
			if !near(got["est"].(float64), w.Est) || !near(got["k"].(float64), w.K) || !near(got["month"].(float64), w.Month) {
				t.Errorf("%s transfer #%d est %v k %v month %v, want %v %v %v", month, w.N, got["est"], got["k"], got["month"], w.Est, w.K, w.Month)
			}
		}
		reqs := snap["requests"].([]map[string]any)
		if len(reqs) != len(want.Requests) {
			t.Errorf("%s requests %d, want %d", month, len(reqs), len(want.Requests))
		}
		byReq := map[string]map[string]any{}
		for _, r := range reqs {
			byReq[r["page"].(string)+r["t"].(string)] = r
		}
		for _, w := range want.Requests {
			got := byReq[w.Page+ts.Format(parseT(w.T))]
			if got == nil {
				t.Errorf("%s no request %s %s", month, w.Page, w.T)
				continue
			}
			if !near(got["avail"].(float64), w.Avail) {
				t.Errorf("%s request %s %s avail %v, want %v", month, w.Page, w.T, got["avail"], w.Avail)
			}
			wt := 0
			if f, ok := w.Transfer.(float64); ok {
				wt = int(f)
			}
			gt, _ := got["transfer"].(int)
			if gt != wt {
				t.Errorf("%s request %s %s transfer %v, want %v", month, w.Page, w.T, got["transfer"], w.Transfer)
			}
		}
	}
}

func money6(x float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 6, 64), 64)
	return v
}
