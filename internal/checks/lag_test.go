package checks

import (
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/month"
)

// lagCover is a model with a Fansly page and an OF page and two transfers. The second one, #10, pays a Fansly
// request Hub shows (1000 net) and an OF request Hub does not show yet: the OF page's newest request is from the
// first transfer, and 500 net wait on it since. Its deposits say 1000 + 500, so its k is 1.5 against the requests
// Hub shows and 1.0 with the waiting OF money requested at the cut. All figures are synthetic.
func lagCover() *month.Cover {
	old := at("0001-01-01T00:00:00+03:00")
	route := func(page, channel string) month.Route {
		return month.Route{Page: page, Model: "Alfa", Channel: channel, Rate: 1, Payee: "model",
			Rates: []month.MethodRate{{Method: "*", Rate: 1, From: old}}}
	}
	cfg := month.Config{Models: []string{"Alfa"}, Pages: []string{"alfa-1", "alfa-of"}, Share: 0.7, KLow: 0.985, KHigh: 1.015,
		Routes: map[string]month.Route{"alfa-1": route("alfa-1", "Fansly"), "alfa-of": route("alfa-of", "OF")},
		Names:  map[string]string{"Alfa": "Альфа"}}
	prev := &month.Transfer{N: 9, Model: "Alfa", Date: "2026-10-13", Amount: 700, Cut: at("2026-10-13T12:00:59+03:00"),
		Deps: []float64{1000}, Est: 700, K: 1, HasK: true, ByMonth: map[string]float64{"2026-10": 700}}
	t := &month.Transfer{N: 10, Model: "Alfa", Date: "2026-10-20", Amount: 1050, Cut: at("2026-10-20T12:00:59+03:00"),
		Deps: []float64{1000, 500}, Est: 700, K: 1.5, HasK: true, ByMonth: map[string]float64{"2026-10": 700}}
	ofOld := &month.Request{Page: "alfa-of", Key: "of0", T: at("2026-10-13T12:00:00+03:00"), Amount: 1000, Method: "of", Transfer: prev}
	q := &month.Request{Page: "alfa-1", Key: "q1", T: at("2026-10-20T12:00:00+03:00"), Amount: 1000, Avail: 1000, Method: "usdt", Transfer: t}
	fansly := &month.Payment{Page: "alfa-1", T: at("2026-10-08T10:00:00+03:00"), Avail: at("2026-10-15T10:00:00+03:00"), Net: 1000,
		Parts: []month.Part{{Amt: 1000, Req: q}}}
	of := &month.Payment{Page: "alfa-of", T: at("2026-10-11T10:00:00+03:00"), Avail: at("2026-10-18T10:00:00+03:00"), Net: 500,
		Parts: []month.Part{{Amt: 500}}}
	return &month.Cover{Cfg: cfg, AsOf: at("2026-10-22T12:00:00+03:00"), Anchors: map[string]time.Time{},
		Rows: map[string][]*month.Payment{"alfa-1": {fansly}, "alfa-of": {of}}, PageOrder: []string{"alfa-1", "alfa-of"},
		Reqs: []*month.Request{ofOld, q}, Transfers: map[string][]*month.Transfer{"Alfa": {prev, t}},
		Newest: map[string]*month.Request{"alfa-1": q, "alfa-of": ofOld},
		Src:    src{incomes: map[string][]float64{"2026-10-13": {700}, "2026-10-20": {1050}}}}
}

func coverageResults(cv *month.Cover, now time.Time) map[string]Result {
	return map[string]Result{"k": KNorm(cv, now), "lag": HubLag(cv, now), "deposits": Deposits(cv)}
}

// failing lists the failing results: what the health of that hour says.
func failing(rs map[string]Result) []Result {
	var out []Result
	for _, r := range rs {
		if !r.OK {
			out = append(out, r)
		}
	}
	return out
}

// A transfer whose k is high only because Hub does not show yet the request it pays is one warning, not urgent,
// and no other check repeats it.
func TestATransferWaitingForHubIsOneWarning(t *testing.T) {
	now := at("2026-10-22T12:00:00+03:00")
	rs := coverageResults(lagCover(), now)
	bad := failing(rs)
	if len(bad) != 1 || bad[0].Name != hubLagCheck || bad[0].Severity != "warn" {
		t.Fatalf("one warning of the wait, nothing else: %+v", rs)
	}
	for _, want := range []string{"#10 ждёт заявку OF", "модель Альфа", "депозиты 1500.00", "хаб видит заявки на 1000.00",
		"на alfa-of к выводу 500.00", "станет срочным 31.10 12:00"} {
		if !strings.Contains(bad[0].Summary, want) {
			t.Errorf("the line misses %q: %s", want, bad[0].Summary)
		}
	}
	if strings.Contains(bad[0].Summary, "—") {
		t.Errorf("no long dash in the owner's line: %s", bad[0].Summary)
	}
	if !strings.Contains(rs["k"].Summary, "1 ждут заявок хаба") {
		t.Errorf("k says it left a transfer to the wait: %+v", rs["k"])
	}
	s, ctx := openDB(t)
	if out, err := Urgent(ctx, s.RW, []Result{rs["k"], rs["lag"], rs["deposits"]}, now, UrgentEvery); err != nil || len(out) != 0 {
		t.Fatalf("a wait for Hub is not urgent: %v %v", out, err)
	}
}

// Past month.HubLagLimit the wait is urgent again, on the k line, which carries the deposits; the deposits check does
// not repeat it.
func TestAWaitLongerThanTheLimitIsUrgentAgain(t *testing.T) {
	cv := lagCover()
	if r := HubLag(cv, at("2026-10-31T12:00:00+03:00")); r.OK {
		t.Fatalf("11 days after the cut it still waits: %+v", r)
	}
	now := at("2026-10-31T12:01:00+03:00")
	rs := coverageResults(cv, now)
	bad := failing(rs)
	if len(bad) != 1 || bad[0].Name != "k переводов в норме" || bad[0].Severity != "urgent" {
		t.Fatalf("one urgent k line: %+v", rs)
	}
	for _, want := range []string{"#10 k 1.5000", "депозиты 1500.00, по заявкам хаба с учётом маршрута 1000.00", "заявки OF в хабе нет 11 дн."} {
		if !strings.Contains(bad[0].Summary, want) {
			t.Errorf("the line misses %q: %s", want, bad[0].Summary)
		}
	}
}

// Real errors stay urgent while the transfer is fresh: Hub's data of the page reaches the cut, the waiting money does
// not explain the excess (too little or too much), k is below the norm, the sum is not the deposits', nothing came on Bybit.
func TestRealErrorsStayUrgentWhileFresh(t *testing.T) {
	now := at("2026-10-22T12:00:00+03:00")
	cases := map[string]func(cv *month.Cover){
		"Hub shows the OF page's request after the previous transfer": func(cv *month.Cover) {
			cv.Newest["alfa-of"] = &month.Request{Page: "alfa-of", Key: "of1", T: at("2026-10-19T12:00:00+03:00"), Amount: 500, Method: "of"}
		},
		"Hub has no request of the OF page at all and nothing waits on it": func(cv *month.Cover) {
			delete(cv.Newest, "alfa-of")
			cv.Rows["alfa-of"] = nil
		},
		"the waiting money is too little": func(cv *month.Cover) {
			cv.Rows["alfa-of"][0].Net, cv.Rows["alfa-of"][0].Parts[0].Amt = 100, 100
		},
		"the waiting money is too much": func(cv *month.Cover) {
			cv.Rows["alfa-of"][0].Net, cv.Rows["alfa-of"][0].Parts[0].Amt = 2000, 2000
		},
		"the waiting money is available only after the cut": func(cv *month.Cover) {
			cv.Rows["alfa-of"][0].Avail = at("2026-10-21T10:00:00+03:00")
		},
		"k below the norm": func(cv *month.Cover) {
			tr := cv.Transfers["Alfa"][1]
			tr.Amount, tr.K, tr.Deps = 560, 0.8, []float64{800}
			cv.Src = src{incomes: map[string][]float64{"2026-10-20": {560}}}
		},
		"the deposits do not make the recorded sum": func(cv *month.Cover) {
			cv.Transfers["Alfa"][1].Deps = []float64{1000, 700}
		},
		"nothing credited on Bybit": func(cv *month.Cover) {
			cv.Src = src{incomes: map[string][]float64{}}
		},
		"no deposits in the tag": func(cv *month.Cover) {
			cv.Transfers["Alfa"][1].Deps = nil
		},
	}
	for name, change := range cases {
		cv := lagCover()
		change(cv)
		if r := KNorm(cv, now); r.OK || r.Severity != "urgent" || !strings.Contains(r.Summary, "#10 k") {
			t.Errorf("%s: k must stay urgent: %+v", name, r)
		}
		if r := HubLag(cv, now); !r.OK {
			t.Errorf("%s: not a wait for Hub: %+v", name, r)
		}
	}
}

// Two transfers in a row wait for the same silent page: the money waiting before the first cut is the first one's,
// the second gets only what came after it.
func TestTwoTransfersWaitingForTheSamePage(t *testing.T) {
	cv := lagCover()
	first := cv.Transfers["Alfa"][1]
	second := &month.Transfer{N: 11, Model: "Alfa", Date: "2026-10-27", Amount: 1050, Cut: at("2026-10-27T12:00:59+03:00"),
		Deps: []float64{1000, 500}, Est: 700, K: 1.5, HasK: true, ByMonth: map[string]float64{"2026-10": 700}}
	q := &month.Request{Page: "alfa-1", Key: "q2", T: at("2026-10-27T12:00:00+03:00"), Amount: 1000, Avail: 1000, Method: "usdt", Transfer: second}
	cv.Transfers["Alfa"] = append(cv.Transfers["Alfa"], second)
	cv.Reqs = append(cv.Reqs, q)
	cv.Newest["alfa-1"] = q
	cv.Rows["alfa-1"] = append(cv.Rows["alfa-1"], &month.Payment{Page: "alfa-1", T: at("2026-10-15T10:00:00+03:00"),
		Avail: at("2026-10-22T10:00:00+03:00"), Net: 1000, Parts: []month.Part{{Amt: 1000, Req: q}}})
	cv.Rows["alfa-of"] = append(cv.Rows["alfa-of"], &month.Payment{Page: "alfa-of", T: at("2026-10-18T10:00:00+03:00"),
		Avail: at("2026-10-25T10:00:00+03:00"), Net: 500, Parts: []month.Part{{Amt: 500}}})
	cv.Src = src{incomes: map[string][]float64{"2026-10-20": {1050}, "2026-10-27": {1050}}}
	lags := cv.Lags()
	if lags[first] == nil || lags[second] == nil || lags[first].Net != 500 || lags[second].Net != 500 {
		t.Fatalf("each transfer waits for its own 500: %+v %+v", lags[first], lags[second])
	}
	now := at("2026-10-28T12:00:00+03:00")
	if r := KNorm(cv, now); !r.OK {
		t.Fatalf("both wait: %+v", r)
	}
	if r := HubLag(cv, now); r.OK || !strings.Contains(r.Summary, "#10 ждёт") || !strings.Contains(r.Summary, "#11 ждёт") {
		t.Fatalf("both waits said: %+v", r)
	}
}
