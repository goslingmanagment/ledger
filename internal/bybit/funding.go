package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The Funding journal (amount of every movement of the Funding account and the balance after it) is
// an independent check of the movement streams: does the balance chain hold (no missing rows), and
// does every Funding movement have its operation. It is an observation: it adds no proposals.
const (
	fundingPath   = "/v5/asset/fundinghistory"
	fundingWindow = 7*24*time.Hour - time.Minute // the endpoint accepts at most 7 days
	fundingLimit  = "100"
	matchWindowMs = 2 * 3600 * 1000
)

var (
	chainEps       = mustRat("0.000001")
	p2pTolerance   = mustRat("0.001") // the P2P quantity and the Funding amount differ in the last place
	opTolerance    = mustRat("0.01")
	noFeeTolerance = mustRat("3") // old withdrawals without a fee: Funding = amount + network fee
)

// FundingRow is one movement of the Funding account.
type FundingRow struct {
	ID     string `json:"id"`
	TimeMs int64  `json:"time_ms"`
	At     string `json:"at"`
	Coin   string `json:"coin"`
	Signed string `json:"signed"`
	After  string `json:"after"`
	Type   string `json:"type"`
	Desc   string `json:"desc"`
	// Imported names the ledger-sync table the row came from.
	Imported string          `json:"imported,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

// Event is the ledger event of the row.
func (r FundingRow) Event() ledger.Event {
	return ledger.Event{Source: "bybit", SourceID: "funding:" + r.ID, Kind: "funding", OccurredAt: r.At, Payload: r}
}

// FetchFunding reads the Funding journal of [from, to) in 7-day windows.
func FetchFunding(ctx context.Context, c *Client, from, to time.Time) ([]FundingRow, error) {
	var out []FundingRow
	seen := map[string]bool{}
	for _, w := range windows(from.Truncate(time.Second), to, fundingWindow) {
		cursor := ""
		for {
			res, err := c.Get(ctx, fundingPath, map[string]string{"createTimeFrom": strconv.FormatInt(w.From.Unix(), 10),
				"createTimeTo": strconv.FormatInt(w.To.Unix(), 10), "limit": fundingLimit, "cursor": cursor})
			if err != nil {
				return nil, err
			}
			var page map[string]json.RawMessage
			if err := json.Unmarshal(res, &page); err != nil {
				return nil, fmt.Errorf("%s: %v", fundingPath, err)
			}
			listRaw, ok := page["list"]
			if !ok {
				return nil, fmt.Errorf("%s: в ответе нет list — изменилась схема API", fundingPath)
			}
			var list []json.RawMessage
			if err := json.Unmarshal(listRaw, &list); err != nil {
				return nil, fmt.Errorf("%s: list: %v", fundingPath, err)
			}
			for _, raw := range list {
				r, err := decodeRecord(raw)
				if err != nil {
					return nil, fmt.Errorf("%s: %v", fundingPath, err)
				}
				row, err := fundingRow(r)
				if err != nil {
					return nil, err
				}
				if !seen[row.ID] {
					seen[row.ID] = true
					out = append(out, row)
				}
			}
			var next string
			if n, ok := page["nextPageCursor"]; ok {
				_ = json.Unmarshal(n, &next)
			}
			if next == "" {
				break
			}
			if next == cursor {
				return nil, fmt.Errorf("%s: курсор повторился — выгрузка неполная", fundingPath)
			}
			cursor = next
		}
	}
	sortFunding(out)
	return out, nil
}

func fundingRow(r record) (FundingRow, error) {
	amt, ok1 := rat(r.s("txnAmt"))
	after, ok2 := rat(r.s("afterAmt"))
	if !ok1 || !ok2 {
		return FundingRow{}, fmt.Errorf("%s: строка без txnAmt/afterAmt — изменилась схема API: %s", fundingPath, snippet(r.raw))
	}
	sec, err := strconv.ParseInt(r.s("createTime"), 10, 64)
	if err != nil {
		return FundingRow{}, fmt.Errorf("%s: createTime %q", fundingPath, r.s("createTime"))
	}
	id := r.s("currcCursor")
	if id == "" {
		id = fmt.Sprintf("%s:%s:%s:%s", r.s("createTime"), r.s("currency"), r.s("txnAmt"), r.s("afterAmt"))
	}
	if r.s("ioDirection") == "O" {
		amt.Neg(amt)
	}
	t := time.Unix(sec, 0)
	return FundingRow{ID: id, TimeMs: sec * 1000, At: ts.Format(t), Coin: r.s("currency"), Signed: decString(amt),
		After: decString(after), Type: r.s("showBusiTypeEn"), Desc: r.s("descriptionEn"), Raw: r.raw}, nil
}

func sortFunding(rows []FundingRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].TimeMs != rows[j].TimeMs {
			return rows[i].TimeMs < rows[j].TimeMs
		}
		return rows[i].ID < rows[j].ID
	})
}

// Gap is a break of the balance chain: prev.after + cur.signed ≠ cur.after.
type Gap struct {
	Coin     string   `json:"coin"`
	AfterID  string   `json:"after_id"`
	BeforeID string   `json:"before_id"`
	Expected string   `json:"expected"`
	Actual   string   `json:"actual"`
	Delta    string   `json:"delta"`
	Between  [2]int64 `json:"between"`
}

// ordered puts rows of the same second in the order the chain holds (small groups are permuted).
func ordered(rows []FundingRow) []FundingRow {
	var out []FundingRow
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].TimeMs == rows[i].TimeMs {
			j++
		}
		group := rows[i:j]
		i = j
		if len(group) == 1 || len(out) == 0 || len(group) > 7 {
			out = append(out, group...)
			continue
		}
		prev, _ := rat(out[len(out)-1].After)
		best := group
		permute(len(group), func(idx []int) bool {
			cur := new(big.Rat).Set(prev)
			for _, k := range idx {
				s, _ := rat(group[k].Signed)
				a, _ := rat(group[k].After)
				cur.Add(cur, s)
				if !absDiffLE(cur, a, chainEps) {
					return false
				}
			}
			best = make([]FundingRow, len(idx))
			for n, k := range idx {
				best[n] = group[k]
			}
			return true
		})
		out = append(out, best...)
	}
	return out
}

// permute calls try with index permutations in lexicographic order until it returns true.
func permute(n int, try func([]int) bool) {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	for {
		if try(idx) {
			return
		}
		i := n - 2
		for i >= 0 && idx[i] >= idx[i+1] {
			i--
		}
		if i < 0 {
			return
		}
		j := n - 1
		for idx[j] <= idx[i] {
			j--
		}
		idx[i], idx[j] = idx[j], idx[i]
		for l, r := i+1, n-1; l < r; l, r = l+1, r-1 {
			idx[l], idx[r] = idx[r], idx[l]
		}
	}
}

// ChainGaps finds breaks of the balance chain per coin.
func ChainGaps(rows []FundingRow) []Gap {
	byCoin := map[string][]FundingRow{}
	var coins []string
	for _, r := range rows {
		if _, ok := byCoin[r.Coin]; !ok {
			coins = append(coins, r.Coin)
		}
		byCoin[r.Coin] = append(byCoin[r.Coin], r)
	}
	var gaps []Gap
	for _, coin := range coins {
		rs := append([]FundingRow(nil), byCoin[coin]...)
		sortFunding(rs)
		rs = ordered(rs)
		for i := 1; i < len(rs); i++ {
			prev, cur := rs[i-1], rs[i]
			pa, _ := rat(prev.After)
			cs, _ := rat(cur.Signed)
			ca, _ := rat(cur.After)
			expect := new(big.Rat).Add(pa, cs)
			if !absDiffLE(expect, ca, chainEps) {
				gaps = append(gaps, Gap{Coin: coin, AfterID: prev.ID, BeforeID: cur.ID, Expected: decString(expect),
					Actual: cur.After, Delta: decString(new(big.Rat).Sub(ca, expect)), Between: [2]int64{prev.TimeMs, cur.TimeMs}})
			}
		}
	}
	return gaps
}

// decString prints an exact decimal without trailing zeros (at most 12 places).
func decString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := fixed(r, 12)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	return s
}

// Op is a movement as the Funding check sees it.
type Op struct {
	Key       string
	Direction string
	Coin      string
	Amount    string
	Fee       string
	TimeMs    int64
}

func (o Op) total() (*big.Rat, *big.Rat, bool) {
	amt, ok := rat(o.Amount)
	if !ok {
		return nil, nil, false
	}
	kind, _, _ := strings.Cut(o.Key, ":")
	switch kind {
	case "withdrawal":
		if fee, ok := rat(o.Fee); ok {
			return amt.Add(amt, fee), opTolerance, true
		}
		return amt, noFeeTolerance, true
	case "p2p":
		return amt, p2pTolerance, true
	}
	return amt, opTolerance, true
}

// Match pairs Funding movements with operations: coin, direction, amount (with the withdrawal fee),
// within ±2 h, greedily by the closest time, one to one.
func Match(rows []FundingRow, ops []Op) map[string]string {
	coins := map[string]bool{}
	for _, r := range rows {
		coins[r.Coin] = true
	}
	var cand []Op
	for _, o := range ops {
		coin := o.Coin
		if coin == "" {
			coin = "USDT"
		}
		if coins[coin] {
			o.Coin = coin
			cand = append(cand, o)
		}
	}
	sorted := append([]FundingRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TimeMs < sorted[j].TimeMs })
	used, matched := map[string]bool{}, map[string]string{}
	for _, r := range sorted {
		sgn, ok := rat(r.Signed)
		if !ok {
			continue
		}
		want := "income"
		if sgn.Sign() < 0 {
			want = "expense"
		}
		abs := new(big.Rat).Abs(sgn)
		best, bestDt := "", int64(-1)
		for _, o := range cand {
			if used[o.Key] || o.Direction != want || o.Coin != r.Coin {
				continue
			}
			total, tol, ok := o.total()
			if !ok || !absDiffLE(abs, total, tol) {
				continue
			}
			dt := o.TimeMs - r.TimeMs
			if dt < 0 {
				dt = -dt
			}
			if dt <= matchWindowMs && (bestDt < 0 || dt < bestDt) {
				best, bestDt = o.Key, dt
			}
		}
		if best != "" {
			used[best], matched[r.ID] = true, best
		}
	}
	return matched
}

// FundingReport is the Funding check of a window, kept in the run status for the hourly checks.
type FundingReport struct {
	From             string            `json:"from"`
	To               string            `json:"to"`
	Rows             int               `json:"rows"`
	Matched          int               `json:"matched"`
	ChainGaps        []Gap             `json:"chain_gaps"`
	UnmatchedFunding []FundingRow      `json:"unmatched_funding"`
	UnmatchedOps     []string          `json:"unmatched_ops"`
	Transfers        int               `json:"transfers"`
	EndBalance       map[string]string `json:"end_balance"`
}

// Reconcile checks rows against ops; operations without a Funding movement count from opsSinceMs.
// Transfers between own accounts and Earn interest are not journal operations.
func Reconcile(rows []FundingRow, ops []Op, opsSinceMs int64) FundingReport {
	sorted := append([]FundingRow(nil), rows...)
	sortFunding(sorted)
	matched := Match(sorted, ops)
	rep := FundingReport{Rows: len(sorted), Matched: len(matched), ChainGaps: ChainGaps(sorted),
		UnmatchedFunding: []FundingRow{}, UnmatchedOps: []string{}, EndBalance: map[string]string{}}
	if rep.ChainGaps == nil {
		rep.ChainGaps = []Gap{}
	}
	used := map[string]bool{}
	for _, k := range matched {
		used[k] = true
	}
	for _, r := range sorted {
		if r.Type == "Transfer" {
			rep.Transfers++
		}
		if _, ok := matched[r.ID]; !ok && r.Type != "Transfer" && r.Type != "Earn" {
			r.Raw = nil
			rep.UnmatchedFunding = append(rep.UnmatchedFunding, r)
		}
		rep.EndBalance[r.Coin] = r.After
	}
	for _, o := range ops {
		if o.TimeMs >= opsSinceMs && !used[o.Key] {
			rep.UnmatchedOps = append(rep.UnmatchedOps, o.Key)
		}
	}
	return rep
}
