package month

import (
	"math"
	"sort"
	"time"
)

// HubLagLimit is how long a transfer may wait for Hub to show the request it pays before its k is urgent again: the
// same 11 days a request may wait for its transfer (check «переводы не просрочены»). Hub shows a platform's request
// when its snapshot of the platform catches it, and that has taken up to a week.
const HubLagLimit = 11 * 24 * time.Hour

// Lag is a transfer whose k is above the norm because Hub does not show yet a request the transfer pays.
type Lag struct {
	T        *Transfer
	Pages    []string  // the silent pages holding the waiting money
	Channels []string  // their channels, in page order
	Net      float64   // the net waiting on them, available before the cut and taken by no request Hub shows
	Deposits float64   // the deposits of the tag
	Seen     float64   // the requests Hub shows that the transfer covers, at their route's rate: what Hub explains
	K        float64   // the transfer's k were the waiting net requested at the cut
	Until    time.Time // the cut + HubLagLimit: after it the transfer is urgent again
}

// Waits says the transfer still waits for Hub at now.
func (l *Lag) Waits(now time.Time) bool { return !now.After(l.Until) }

// Requested sums the requests Hub shows that a transfer covers at the rate of their route: what its deposits should be.
func (cv *Cover) Requested(t *Transfer) float64 {
	v := 0.0
	for _, q := range cv.Reqs {
		if q.Transfer == t {
			v += float64(q.Amount * cv.Cfg.RateOf(q.Page, q))
		}
	}
	return v
}

// tagHolds: the tag of a transfer stands without Hub. It lists deposits, they make the recorded sum (±$10, as the
// deposits check) and the credited sum is on Bybit that date.
func (cv *Cover) tagHolds(t *Transfer) bool {
	if len(t.Deps) == 0 || cv.Src == nil {
		return false
	}
	mult := cv.Cfg.Share
	if cv.Cfg.PayeeOf(t.Model) == "agency" {
		mult = 1
	}
	if math.Abs(float64(sum(t.Deps)*mult)-t.Amount) > 10 {
		return false
	}
	hits, ok := cv.Src.BybitIncomes(t.Date)
	if !ok {
		return false
	}
	credited := t.Amount - t.Held - t.Fee
	for _, a := range hits {
		if math.Abs(a-credited) <= .01 {
			return true
		}
	}
	return false
}

// Lags finds the transfers whose k is out of the norm only because Hub does not show yet a request they pay. Such a
// transfer
//   - has k above the norm: a request Hub does not show can only leave a transfer more money than its requests explain,
//     never less;
//   - has a tag that holds without Hub (tagHolds): a wrong sum, deposit or credit is an error, not a wait;
//   - has silent pages: pages of its model from which Hub has shown no request at all since the model's previous
//     transfer (Cover.Newest), so Hub's request data of the page may simply not reach the cut yet;
//   - comes back into the k norm, within both bounds, with the net waiting on the silent pages requested at the cut:
//     the money Hub cannot see withdrawn yet explains the excess, neither too little (an error the wait does not
//     cover) nor too much (a partial withdrawal or money of no request: not provable, so not quieted).
//
// The net waiting on a silent page goes to the earliest lagging transfer of the model and is not counted again for a
// later one. Whether the transfer still waits is Lag.Waits: the age is the caller's.
func (cv *Cover) Lags() map[*Transfer]*Lag {
	c := cv.Cfg
	out := map[*Transfer]*Lag{}
	for _, model := range c.Models {
		prev := c.FullLedger
		if a, ok := cv.Anchors[model]; ok {
			prev = a
		}
		taken := map[string]time.Time{}
		for _, t := range cv.Transfers[model] {
			since := prev
			prev = t.Cut
			if !t.HasK || t.K <= c.KHigh || t.Est <= 0 {
				continue
			}
			l := &Lag{T: t, Until: t.Cut.Add(HubLagLimit)}
			share, nets, channels := 0.0, map[string]float64{}, map[string]bool{}
			for _, p := range c.Pages {
				if c.Routes[p].Model != model {
					continue
				}
				newest := cv.Newest[p]
				if newest != nil && newest.T.After(since) {
					continue // Hub shows this page's requests past the previous transfer: its data reaches here
				}
				var parts []float64
				for _, r := range cv.Rows[p] {
					if !r.Avail.Before(t.Cut) || r.Avail.Before(taken[p]) {
						continue
					}
					for _, part := range r.Parts {
						if part.Req == nil && !part.Anchor {
							parts = append(parts, part.Amt)
						}
					}
				}
				net := sum(parts)
				if net < 0.005 {
					continue
				}
				q := &Request{Page: p, T: t.Cut}
				if newest != nil {
					q.Method = newest.Method
				}
				rate := c.RateOf(p, q)
				share += float64(net * rate * c.Share)
				nets[p] = net
				l.Pages = append(l.Pages, p)
				l.Net += net
				if ch := c.Routes[p].Channel; !channels[ch] {
					channels[ch] = true
					l.Channels = append(l.Channels, ch)
				}
			}
			if share <= 0 {
				continue
			}
			l.K = t.K * t.Est / (t.Est + share)
			if l.K < c.KLow || l.K > c.KHigh || !cv.tagHolds(t) {
				continue
			}
			l.Deposits, l.Seen = sum(t.Deps), cv.Requested(t)
			for p := range nets {
				taken[p] = t.Cut
			}
			sort.Strings(l.Pages)
			out[t] = l
		}
	}
	return out
}
