package month

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Part is the piece of a payment a request covered: Req nil and not Anchor = not covered yet.
type Part struct {
	Amt    float64
	Req    *Request
	Anchor bool // available before the anchor request: withdrawn by it or earlier
}

// Payment is a fan payment split into parts.
type Payment struct {
	Page  string
	T     time.Time
	Net   float64
	Key   string
	Prize bool
	Avail time.Time
	Parts []Part
	Event int64
}

// Request is a payout request.
type Request struct {
	Page     string
	Key      string
	T        time.Time
	Amount   float64
	Status   string
	Method   string
	Updated  time.Time
	Avail    float64 // net available at the request
	Excess   float64 // what the available net did not cover
	Transfer *Transfer
	Pre      bool // before the anchor: outside the labelled transfers
}

// Transfer is a model's transfer (or the exchanger's payout) from the journal.
type Transfer struct {
	N       int
	Model   string
	Date    string
	Amount  float64
	Cut     time.Time
	Est     float64
	Deps    []float64
	Held    float64
	Fee     float64
	ByMonth map[string]float64
	Bad     []string
	K       float64
	HasK    bool
}

// Cover is the coverage of payments by requests and transfers at a cut.
type Cover struct {
	Cfg       Config
	AsOf      time.Time
	Anchors   map[string]time.Time
	Ledger    []journal.Row
	Rows      map[string][]*Payment
	PageOrder []string
	Reqs      []*Request
	Transfers map[string][]*Transfer
	Notes     []string
	Src       Source
}

func sum(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s
}

func iso(t time.Time) string { return ts.Format(t) }

func mustFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(err)
	}
	return v
}

// transfersOf reads the tagged model transfers from the journal rows (paid, up to the cut's date).
func transfersOf(c Config, rows []journal.Row, asOf time.Time) (map[string][]*Transfer, []string, error) {
	out := map[string][]*Transfer{}
	var untagged []string
	full := ts.Date(c.FullLedger)
	for _, r := range rows {
		if r.Type != "income" || r.Category != "Платформы" {
			continue
		}
		tg, err := journal.RowTags(r)
		if err != nil {
			return nil, nil, Fail{err.Error()}
		}
		if tg.Transfer == nil {
			if r.Date >= full {
				untagged = append(untagged, fmt.Sprintf("#%d", r.Num))
			}
			continue
		}
		cut := tg.Transfer.CutTime()
		route := c.PayeeOf(r.Model)
		d, err := ts.Parse(r.Date)
		if err != nil {
			return nil, nil, Fail{err.Error()}
		}
		cutDay, _ := ts.Parse(ts.Date(cut))
		lag := int(d.Sub(cutDay).Hours() / 24)
		if route == "" || lag < 0 || lag > c.LagOf(r.Model) || (len(tg.Transfer.Fee) > 0 && route != "agency") {
			return nil, nil, failf("ledger #%d: перевод модели «%s» (нужна из %v), срез %s на %d дн. до строки %s (маршрут %s: 0–%d), «комиссия» — только у маршрута «агентству 100%%»",
				r.Num, r.Model, c.Models, tg.Transfer.Cut, lag, r.Date, route, c.LagOf(r.Model))
		}
		if !cut.After(asOf) {
			out[r.Model] = append(out[r.Model], &Transfer{N: r.Num, Model: r.Model, Date: r.Date, Amount: mustFloat(r.Amount),
				Cut: cut.Add(59 * time.Second), Deps: tg.Transfer.Deposits, Held: sum(tg.Transfer.Held), Fee: sum(tg.Transfer.Fee),
				ByMonth: map[string]float64{}})
		}
	}
	for m, chain := range out {
		sort.SliceStable(chain, func(i, j int) bool { return chain[i].Cut.Before(chain[j].Cut) })
		var dup []string
		for i := 1; i < len(chain); i++ {
			if chain[i-1].Cut.Equal(chain[i].Cut) {
				dup = append(dup, fmt.Sprintf("#%d/#%d", chain[i-1].N, chain[i].N))
			}
		}
		if len(dup) > 0 {
			return nil, nil, failf("переводы %s (%s) с одним срезом — перевод записан дважды?", strings.Join(dup, ", "), m)
		}
	}
	return out, untagged, nil
}

func init() {
	journal.RowsCheck = func(ctx context.Context, q ledger.Querier, rows []journal.Row) error {
		c, err := LoadConfig(ctx, q)
		if err != nil {
			return err
		}
		var paid []journal.Row
		for _, r := range rows {
			if r.Status == "paid" {
				paid = append(paid, r)
			}
		}
		_, _, err = transfersOf(c, paid, time.Now().AddDate(10, 0, 0))
		return err
	}
}

func payments(ck hub.Checked) []*Payment {
	out := make([]*Payment, 0, len(ck.Rows))
	for _, r := range ck.Rows {
		out = append(out, &Payment{Page: r.Page, T: r.At, Net: float64(r.NetMills) / 1000, Key: r.Key, Prize: r.Prize, Avail: r.Avail, Event: r.EventID})
	}
	return out
}

func requests(ps []hub.Payout) []*Request {
	out := make([]*Request, 0, len(ps))
	for _, p := range ps {
		out = append(out, &Request{Page: p.Page, Key: p.Key, T: p.At, Amount: float64(p.AmountMills) / 1000, Status: p.Status,
			Method: p.Method, Updated: p.Updated})
	}
	return out
}

// fifo: a request at time T covers the parts of payments available before T, in order of availability,
// up to its amount; a partial withdrawal leaves the rest for the next request. What was available
// before openT left with the anchor. A refund (negative) lowers what is available but is not covered.
func fifo(rows []*Payment, reqs []*Request, openT *time.Time) {
	order := append([]*Payment(nil), rows...)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if !a.Avail.Equal(b.Avail) {
			return a.Avail.Before(b.Avail)
		}
		if !a.T.Equal(b.T) {
			return a.T.Before(b.T)
		}
		return a.Key < b.Key
	})
	for _, r := range rows {
		r.Parts = []Part{{Amt: r.Net, Anchor: openT != nil && r.Avail.Before(*openT)}}
	}
	for _, q := range reqs {
		left := q.Amount
		avail := 0.0
		for _, r := range order {
			last := r.Parts[len(r.Parts)-1]
			if r.Avail.Before(q.T) && last.Req == nil && !last.Anchor {
				avail += last.Amt
			}
		}
		q.Avail = avail
		for _, r := range order {
			li := len(r.Parts) - 1
			last := &r.Parts[li]
			if !r.Avail.Before(q.T) || left < 5e-7 {
				break
			}
			if last.Req != nil || last.Anchor || last.Amt <= 0 {
				continue
			}
			take := math.Min(last.Amt, left)
			left -= take
			if take > last.Amt-5e-7 {
				last.Req = q
			} else {
				last.Amt -= take
				rest := *last
				r.Parts = append(r.Parts[:li], Part{Amt: take, Req: q}, rest)
			}
		}
		q.Excess = left
	}
}

// BuildCover computes the coverage at asOf from one anchor: the start of the full journal.
func BuildCover(c Config, src Source, rows []journal.Row, asOf time.Time) (*Cover, error) {
	asOf = asOf.In(ts.MSK)
	var ledger []journal.Row
	date := ts.Date(asOf)
	for _, r := range rows {
		if r.Status == "paid" && r.Date <= date {
			ledger = append(ledger, r)
		}
	}
	trs, untagged, err := transfersOf(c, ledger, asOf)
	if err != nil {
		return nil, err
	}
	if len(untagged) > 0 {
		return nil, failf("переводы без тега {перевод: …}: %s — разнести нельзя", strings.Join(untagged, ", "))
	}
	grant, err := src.Pages()
	if err != nil {
		return nil, err
	}
	inGrant := map[string]bool{}
	for _, g := range grant {
		inGrant[g.Label] = true
	}
	var stray []string
	for _, p := range c.Exc.Pages() {
		if !inGrant[p] {
			stray = append(stray, p)
		}
	}
	if len(stray) > 0 {
		return nil, failf("hub_exceptions: страницы не из гранта ключа хаба: %v", stray)
	}
	cv := &Cover{Cfg: c, AsOf: asOf, Anchors: map[string]time.Time{}, Ledger: ledger, Rows: map[string][]*Payment{},
		Transfers: trs, Src: src}
	nextTransfer := func(chain []*Transfer, q *Request) *Transfer {
		for _, t := range chain {
			if !q.T.After(t.Cut) {
				return t
			}
		}
		return nil
	}
	for _, model := range c.Models {
		var pages []string
		for _, p := range c.Pages {
			if c.Routes[p].Model == model {
				pages = append(pages, p)
			}
		}
		chain := trs[model]
		var c0 *time.Time
		for _, t := range chain {
			if !t.Cut.After(c.FullLedger) && (c0 == nil || t.Cut.After(*c0)) {
				v := t.Cut
				c0 = &v
			}
		}
		lim := c.FullLedger
		if c0 != nil {
			lim = *c0
			cv.Anchors[model] = *c0
		}
		reqs, opens, start := map[string][]*Request{}, map[string]*time.Time{}, map[string]time.Time{}
		for _, p := range pages {
			if c.Routes[p].Channel != "Fansly" {
				continue
			}
			ps, err := src.Payouts(p, lim.Add(-c.Lookback), asOf)
			if err != nil {
				return nil, err
			}
			po := requests(ps)
			var ok []*Request
			for _, q := range po {
				if q.Status == "Processed" && !q.Updated.After(asOf) {
					ok = append(ok, q)
				} else if q.T.After(lim) {
					cv.Notes = append(cv.Notes, fmt.Sprintf("%s: заявка %s %.2f — %s на срез, не покрывает", p, iso(q.T), q.Amount, q.Status))
				}
			}
			var prior []*Request
			for _, q := range ok {
				if !q.T.After(lim) {
					prior = append(prior, q)
				}
			}
			switch {
			case len(prior) > 0:
				base := prior[len(prior)-1]
				if len(prior) > 1 {
					base = prior[len(prior)-2]
				}
				bt := base.T
				opens[p], start[p] = &bt, base.T.Add(-c.Hold)
				var rest []*Request
				for _, q := range ok {
					if q.T.After(base.T) {
						rest = append(rest, q)
					}
				}
				reqs[p] = rest
			default:
				st, has := c.Exc.Starts[p]
				if !has {
					return nil, failf("%s: нет заявки-якоря за %d дней до %s и нет даты начала страницы в hub_exceptions",
						p, int(c.Lookback.Hours()/24), iso(lim))
				}
				opens[p], start[p], reqs[p] = nil, st, ok
			}
		}
		var sess []*Request
		for _, p := range pages {
			sess = append(sess, reqs[p]...)
		}
		sort.SliceStable(sess, func(i, j int) bool { return sess[i].T.Before(sess[j].T) })
		for _, q := range sess {
			if !q.T.After(lim) {
				q.Pre = true
			} else {
				q.Transfer = nextTransfer(chain, q)
			}
		}
		for _, p := range pages {
			if c.Routes[p].Channel == "Fansly" {
				ck, err := src.Rows(p, start[p], asOf)
				if err != nil {
					return nil, err
				}
				cv.Notes = append(cv.Notes, ck.Notes...)
				rs := payments(ck)
				if opens[p] == nil && len(rs) > 0 && !rs[0].T.Equal(start[p]) {
					return nil, failf("%s: начало страницы %s UTC из hub_exceptions не равно первой строке хаба — оборот до первой строки потерялся бы",
						p, start[p].UTC().Format("2006-01-02 15:04:05"))
				}
				cv.Rows[p] = rs
				cv.PageOrder = append(cv.PageOrder, p)
				fifo(rs, reqs[p], opens[p])
				cv.Reqs = append(cv.Reqs, reqs[p]...)
				cv.Notes = append(cv.Notes, invisible(p, reqs[p])...)
			} else if c0 == nil {
				return nil, failf("%s: нет перевода-якоря модели %s до %s — OF не разнести", p, model, iso(c.FullLedger))
			} else {
				ps, err := src.OFPayouts(p, lim.Add(-c.Lookback), asOf)
				if err != nil {
					return nil, err
				}
				var ok []*Request
				for _, q := range requests(ps) {
					if q.Status == "approved" {
						ok = append(ok, q)
					} else if q.T.After(lim) {
						cv.Notes = append(cv.Notes, fmt.Sprintf("%s: заявка OF %s %.2f — %s на срез, не покрывает", p, iso(q.T), q.Amount, q.Status))
					}
				}
				var prior, post []*Request
				for _, q := range ok {
					if !q.T.After(lim) {
						prior = append(prior, q)
					} else {
						post = append(post, q)
					}
				}
				if len(prior) == 0 {
					return nil, failf("%s: нет заявки OF-якоря за %d дней до %s", p, int(c.Lookback.Hours()/24), iso(lim))
				}
				for _, q := range post {
					q.Transfer = nextTransfer(chain, q)
				}
				anchor := prior[len(prior)-1].T
				ck, err := src.Rows(p, anchor.Add(-c.Hold), asOf)
				if err != nil {
					return nil, err
				}
				cv.Notes = append(cv.Notes, ck.Notes...)
				rs := payments(ck)
				cv.Rows[p] = rs
				cv.PageOrder = append(cv.PageOrder, p)
				fifo(rs, post, &anchor)
				cv.Reqs = append(cv.Reqs, post...)
				cv.Notes = append(cv.Notes, invisible(p, post)...)
			}
		}
	}
	for _, q := range cv.Reqs {
		if q.Excess > 1 && q.Transfer != nil {
			q.Transfer.Bad = append(q.Transfer.Bad, fmt.Sprintf("заявка %s %s больше net", q.Page, q.T.In(ts.MSK).Format("02.01 15:04")))
		}
	}
	for _, p := range cv.PageOrder {
		for _, r := range cv.Rows[p] {
			for _, part := range r.Parts {
				if part.Req != nil && part.Req.Transfer != nil {
					t := part.Req.Transfer
					// float64(...) forbids fused multiply-add: the same bits on arm64 and amd64 as in the script.
					v := float64(part.Amt * c.RateOf(p, part.Req) * c.Share)
					t.Est += v
					t.ByMonth[ts.Month(r.T)] += v
				}
			}
		}
	}
	for _, model := range c.Models {
		anchor := c.FullLedger
		if a, ok := cv.Anchors[model]; ok {
			anchor = a
		}
		for _, t := range trs[model] {
			if !t.Cut.After(anchor) {
				continue
			}
			if t.Est == 0 {
				return nil, failf("перевод #%d (%s): не покрыл ни одной заявки — проверь срез в теге", t.N, model)
			}
			t.K = kOf(t, c.PayeeOf(model), c.Share)
			t.HasK = true
			if t.K < c.KLow || t.K > c.KHigh {
				t.Bad = append(t.Bad, fmt.Sprintf("k %.4f вне %v–%v", t.K, c.KLow, c.KHigh))
			}
		}
	}
	return cv, nil
}

// kOf: (credited + withheld) ÷ Σ estimates of the covered payments; for a route that pays the agency
// 100%: share × (recorded − route fee) ÷ Σ estimates.
func kOf(t *Transfer, payee string, share float64) float64 {
	if payee == "model" {
		return t.Amount / t.Est
	}
	return share * (t.Amount - t.Fee) / t.Est
}

// invisible: a rest after a request that holds for three requests in a row looks like a write-off the
// hub cannot see (a refund to a fan?).
func invisible(page string, reqs []*Request) []string {
	left := map[*Request]bool{}
	for _, q := range reqs {
		if q.Excess <= 1 && q.Avail-q.Amount > 1 {
			left[q] = true
		}
	}
	for i := 2; i < len(reqs); i++ {
		if left[reqs[i-2]] && left[reqs[i-1]] && left[reqs[i]] {
			q := reqs[i]
			return []string{fmt.Sprintf("%s: остаток %.2f держится 3 заявки подряд до %s — невидимое списание платформы?",
				page, q.Avail-q.Amount, q.T.In(ts.MSK).Format("02.01"))}
		}
	}
	return nil
}
