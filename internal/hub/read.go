package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// iso renders a window bound the way month.py does: MSK, whole seconds.
func iso(t time.Time) string { return ts.Format(t) }

// Planes whose capture floor is recorded for each dataset.
var floorPlane = map[string]string{
	"transactions":          "transactions",
	"payouts":               "page_payout_requests",
	"ofapi_payout_requests": "ofapi_read_snapshots",
	"page_profile_daily":    "fansly_page_profile_daily",
	"pending_balance":       "fansly_page_settings_versions",
}

var notPayoutReversal = []filter{{Field: "transactionType", Op: "neq", Value: "payout_reversal"}}

type listSpec struct {
	page, dataset string
	from, to      time.Time
	claim, sort   string
	filters       []filter
	// floor is the hub-exceptions date of the page's first payout request: a before_capture_floor
	// gap ending that day is the journal's real beginning, not a hole (payouts only).
	floor string
	// complete demands month.py's payout proof: one response page, exact count, nothing capped.
	complete bool
	// point is for point-in-time observations (balances): every returned row lies at or after the
	// plane's floor, so a hole before the floor does not touch them.
	point bool
}

type listResult struct {
	items    []item
	blockers map[string]bool // tolerated blockers seen
	floor    *time.Time
	pages    int
}

// listing follows the cursor to the end (month.py Hub.listing). Every list carries
// delivery_not_exhausted (the snapshot is never frozen) and a continued cursor carries
// mutable_sort_key_traversal; any other blocker or any gap refuses the whole list.
func (c *Client) listing(ctx context.Context, s listSpec) (listResult, error) {
	what := fmt.Sprintf("%s %s %s..%s", s.page, s.dataset, iso(s.from), iso(s.to))
	q := query{From: iso(s.from), To: iso(s.to), Claim: []string{s.claim}, Sort: []sortKey{{s.sort, "asc"}},
		Limit: 200, Filters: s.filters}
	res := listResult{blockers: map[string]bool{}}
	index := map[string]int{}
	for {
		r, bl, err := c.dataset(ctx, what, s.page, s.dataset, q)
		if err != nil {
			return res, err
		}
		res.pages++
		if f, ok := r.floorOf(floorPlane[s.dataset]); ok && res.floor == nil {
			res.floor = &f
		}
		gaps := r.Capture.Gaps
		ok := map[string]bool{"delivery_not_exhausted": true, "mutable_sort_key_traversal": true}
		if s.floor != "" || s.point {
			all := true
			for _, g := range gaps {
				if g.Kind != "before_capture_floor" {
					all = false
					break
				}
				if s.point {
					continue
				}
				if g.To == nil {
					return res, refusef("хаб %s: ответ не по контракту (gap без to)", what)
				}
				if len(*g.To) < 10 || (*g.To)[:10] != s.floor {
					all = false
					break
				}
			}
			if all {
				ok["window_before_capture_floor"], ok["gaps_present"] = true, true
				gaps = nil
			}
		}
		var extra []string
		for _, b := range sortedKeys(bl) {
			if !ok[b] {
				extra = append(extra, b)
			} else {
				res.blockers[b] = true
			}
		}
		if len(extra) > 0 || len(gaps) > 0 {
			g, _ := json.Marshal(gaps)
			if gaps == nil {
				g = []byte("[]")
			}
			return res, refusef("хаб %s: blockers %s gaps %s — ответ не считаю данными", what, pyStrList(extra), g)
		}
		for _, it := range r.Items {
			if i, seen := index[it.Key]; seen {
				res.items[i] = it
				continue
			}
			index[it.Key] = len(res.items)
			res.items = append(res.items, it)
		}
		if n := r.Delivery.NextCursor; n != nil && *n != "" {
			q = query{Cursor: *n}
			continue
		}
		if s.complete {
			m := r.Delivery.MatchedInScope
			if !(res.pages == 1 && *m.Exact && r.Delivery.CappedBy == nil && *m.Value == len(res.items)) {
				d, _ := json.Marshal(r.Delivery)
				return res, refusef("хаб %s: список выплат не доказан полным (delivery %s) — сверить нечем", what, d)
			}
		}
		return res, nil
	}
}

// summary is one terminal aggregate of the window's transactions without payout_reversal
// (month.py Hub.summary): any blocker or a non-USD group refuses it. It also brings the windowless
// capture floor of the transactions plane.
func (c *Client) summary(ctx context.Context, page string, a, b time.Time) (count, netMills int64, floor *time.Time, err error) {
	what := page + " summary"
	r, bl, err := c.dataset(ctx, what, page, "transactions", query{From: iso(a), To: iso(b), Summary: true,
		Filters: notPayoutReversal})
	if err != nil {
		return 0, 0, nil, err
	}
	if r.Summary == nil {
		return 0, 0, nil, refusef("хаб %s summary: ответ не по контракту (нет summary)", page)
	}
	nonUSD := false
	for _, g := range r.Summary.Groups {
		if g.Currency == nil || *g.Currency != "USD" {
			nonUSD = true
		}
	}
	if len(bl) > 0 || nonUSD {
		g, _ := json.Marshal(r.Summary.Groups)
		return 0, 0, nil, refusef("хаб %s summary: blockers %s или не-USD строки %s", page, pyStrList(sortedKeys(bl)), g)
	}
	if f, ok := r.floorOf("transactions"); ok {
		floor = &f
	}
	for _, g := range r.Summary.Groups {
		count += *g.TransactionCount
		netMills += *g.NetMills
	}
	return count, netMills, floor, nil
}

func pyStrList(xs []string) string { return pyList(xs) }
