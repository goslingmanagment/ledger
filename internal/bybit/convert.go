package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A conversion of a coin into USDT on Bybit moves money inside the account and has no journal row of
// its own: the row of the coin's deposit stands on bybit:btc in dollars at the date of the deposit. The
// entry takes the sold part of the coin's book value off bybit:btc and puts what the sale gave on bybit;
// the difference is a gain or loss of the price. A journal row written with the method «… → Convert USDT»
// already counted the USDT, so that conversion has no entry. Rows are written later than the movement, so
// every run compares what the conversion should post now with what stands: a change is a reversal and a
// new entry. A purchase of a coin for USDT is the mirror: the USDT paid leaves bybit and stands on the
// coin's account as its book value, so the journal's payments in the coin come off what was paid for it.

// convertGap is how far apart the two legs of one conversion may stand in the Funding log: Bybit writes
// them in one second or in neighbouring ones.
const convertGap = 2 * time.Second

type convertRow struct {
	id    string
	at    string
	t     time.Time
	coin  string
	delta float64
}

// convertPair is a conversion of the Funding log: a coin went out and USDT came in (a sale), or USDT went
// out and a coin came in (a purchase).
type convertPair struct {
	at      string // the later leg: the conversion is done
	usdt    convertRow
	coin    string
	qty     float64 // how much of the coin was sold or bought
	buy     bool
	account string // the coin's book account; empty when there is none and the owner decides
	ids     []string
}

// convertLegs tells whether two Funding rows can be the legs of one conversion: opposite signs, one of them
// USDT and the other a coin.
func convertLegs(a, b convertRow) bool {
	return a.delta*b.delta < 0 && (a.coin == "USDT") != (b.coin == "USDT")
}

// convertPairs reads the conversions of the Funding log in time order. The legs of one second pair first,
// then a leg left alone pairs with the next lone leg at most convertGap later. Anything else (USDC swaps,
// sales for another coin) is not a pair or has no book account and stays for the owner.
func convertPairs(ctx context.Context, q ledger.Querier) ([]convertPair, error) {
	rows, err := q.QueryContext(ctx, `SELECT source_id, occurred_at, payload FROM events WHERE source = 'bybit' AND kind = 'funding'
		AND json_extract(payload, '$.type') = 'Convert' ORDER BY occurred_at, source_id`)
	if err != nil {
		return nil, err
	}
	var legs []convertRow
	for rows.Next() {
		var id, at, payload string
		if err := rows.Scan(&id, &at, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		var p struct {
			Coin   string          `json:"coin"`
			Signed json.RawMessage `json:"signed"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			rows.Close()
			return nil, err
		}
		s, err := strconv.ParseFloat(trimQuotes(string(p.Signed)), 64)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("funding %s: signed %s", id, p.Signed)
		}
		t, err := ts.Parse(at)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("funding %s: %w", id, err)
		}
		legs = append(legs, convertRow{id: id, at: at, t: t, coin: p.Coin, delta: s})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	used := make([]bool, len(legs))
	var found [][2]int
	pairUp := func(gap time.Duration) {
		for i := range legs {
			if used[i] {
				continue
			}
			for k := i + 1; k < len(legs) && legs[k].t.Sub(legs[i].t) <= gap; k++ {
				if !used[k] && convertLegs(legs[i], legs[k]) {
					used[i], used[k] = true, true
					found = append(found, [2]int{i, k})
					break
				}
			}
		}
	}
	pairUp(0)
	pairUp(convertGap)
	// Time order matters: a later sale of the same coin takes its book value after the earlier one.
	sort.Slice(found, func(a, b int) bool { return found[a][1] < found[b][1] })
	var pairs []convertPair
	for _, f := range found {
		a, b := legs[f[0]], legs[f[1]]
		usdt, coin := a, b
		if b.coin == "USDT" {
			usdt, coin = b, a
		}
		p := convertPair{at: b.at, usdt: usdt, coin: coin.coin, qty: math.Abs(coin.delta), buy: usdt.delta < 0, ids: []string{a.id, b.id}}
		var known int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE code = ?`, "bybit:"+lower(p.coin)).Scan(&known); err != nil {
			return nil, err
		}
		if known > 0 {
			p.account = "bybit:" + lower(p.coin)
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}

// ConvertedFunding is the set of Funding row ids that PostConverts answers for: a coin with a book account
// sold for USDT or bought for it, posted by PostConverts or counted by a journal row «… → Convert USDT».
// The Funding check does not raise them as movements without an operation.
func ConvertedFunding(ctx context.Context, q ledger.Querier) (map[string]bool, error) {
	pairs, err := convertPairs(ctx, q)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, p := range pairs {
		if p.account == "" {
			continue
		}
		for _, id := range p.ids {
			ids[strings.TrimPrefix(id, "funding:")] = true
		}
	}
	return ids, nil
}

// PostConverts brings the entries of the conversions in the Funding rows in line with the journal.
func PostConverts(ctx context.Context, q ledger.Querier, now time.Time) (int, error) {
	pairs, err := convertPairs(ctx, q)
	if err != nil {
		return 0, err
	}
	posted := 0
	for _, p := range pairs {
		if p.account == "" {
			continue // a coin without a book account stays for the owner
		}
		key := "bybit:convert:" + p.usdt.id
		memo, changed := "продажа "+p.coin+" за USDT на Bybit", "сторно: журнал изменил продажу "+p.coin
		var want []ledger.Line
		if p.buy {
			memo, changed = "покупка "+p.coin+" за USDT на Bybit", "сторно: изменилась покупка "+p.coin
			want = buyLines(p)
		} else if want, err = convertLines(ctx, q, key, p.at, p.coin, p.account, p.qty, money.FromFloat(p.usdt.delta)); err != nil {
			return posted, fmt.Errorf("%s: %w", key, err)
		}
		active, revisions, err := activeConvert(ctx, q, key)
		if err != nil {
			return posted, err
		}
		if active != 0 {
			same, err := convertSame(ctx, q, active, want)
			if err != nil {
				return posted, err
			}
			if same {
				continue
			}
			if _, err := ledger.Reverse(ctx, q, active, ledger.Reversal{Author: "bybit", Memo: changed}, now); err != nil {
				return posted, fmt.Errorf("%s: %w", key, err)
			}
		}
		if len(want) < 2 {
			continue
		}
		next := key
		if revisions > 0 {
			next = fmt.Sprintf("%s:r%d", key, revisions+1)
		}
		if _, err := ledger.Post(ctx, q, ledger.Entry{EffectiveAt: p.at, Kind: "bybit:convert", Memo: memo,
			Author: "bybit", SourceKey: next, Lines: want}, now); err != nil {
			return posted, fmt.Errorf("%s: %w", next, err)
		}
		posted++
	}
	return posted, nil
}

// buyLines is what a purchase posts: the USDT paid moves from bybit to the coin's account, where it is the
// coin's book value; there is no difference of the price until the coin is spent.
func buyLines(p convertPair) []ledger.Line {
	paid := money.FromFloat(-p.usdt.delta)
	if paid <= 0 {
		return nil
	}
	return []ledger.Line{
		{Account: "bybit", Amount: -paid, Currency: "USDT", Memo: "покупка " + p.coin},
		{Account: p.account, Amount: paid, Memo: "книжная стоимость купленного"},
	}
}

// convertLines is what a conversion should post now: nothing when a journal row already counted the USDT.
func convertLines(ctx context.Context, q ledger.Querier, key, at, coin, account string, sold float64, got money.Micros) ([]ledger.Line, error) {
	var journaled int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM v_documents WHERE method LIKE '%Convert USDT%' AND status = 'paid'
		AND abs(amount_micros - ?) < 20000 AND abs(julianday(date) - julianday(?)) <= 3`, int64(got), at[:10]).Scan(&journaled); err != nil {
		return nil, err
	}
	if journaled > 0 {
		return nil, nil
	}
	// The book value before this sale: everything on the account up to it, except this conversion's own entries.
	var bookAt int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(l.amount), 0) FROM lines l JOIN entries e ON e.id = l.entry_id
		LEFT JOIN entries o ON o.id = e.reverses_id
		WHERE l.account = ? AND e.effective_at <= ?
		  AND COALESCE(e.source_key, '') <> ? AND COALESCE(e.source_key, '') NOT LIKE ?
		  AND COALESCE(o.source_key, '') <> ? AND COALESCE(o.source_key, '') NOT LIKE ?`,
		account, at, key, key+":r%", key, key+":r%").Scan(&bookAt); err != nil {
		return nil, err
	}
	book := money.Micros(bookAt)
	// A part of the coin was sold when some of it stays after the sale, by the Funding log.
	var left float64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CAST(json_extract(payload, '$.signed') AS REAL)), 0) FROM events
		WHERE source = 'bybit' AND kind = 'funding' AND json_extract(payload, '$.coin') = ? AND occurred_at <= ?`, coin, at).Scan(&left); err != nil {
		return nil, err
	}
	cost := book
	if left > 1e-9 && sold > 0 {
		cost = money.FromFloat(book.Float() * sold / (left + sold))
	}
	lines := []ledger.Line{{Account: "bybit", Amount: got, Currency: "USDT", Memo: "продажа " + coin}}
	if cost != 0 {
		lines = append(lines, ledger.Line{Account: account, Amount: -cost, Memo: "книжная стоимость проданного"})
	}
	if diff := got - cost; diff != 0 {
		memo := "разница цены при продаже " + coin
		if math.Abs(diff.Float()) > got.Float() {
			memo += " (без книжной стоимости: проверить строку депозита)"
		}
		lines = append(lines, ledger.Line{Account: "income:other", Amount: -diff, Memo: memo})
	}
	return lines, nil
}

// activeConvert finds the conversion's entry that stands and how many revisions it had.
func activeConvert(ctx context.Context, q ledger.Querier, key string) (id int64, revisions int, err error) {
	rows, err := q.QueryContext(ctx, `SELECT e.id, NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id) FROM entries e
		WHERE e.source_key = ? OR e.source_key LIKE ?`, key, key+":r%")
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry int64
		var stands bool
		if err := rows.Scan(&entry, &stands); err != nil {
			return 0, 0, err
		}
		revisions++
		if stands {
			id = entry
		}
	}
	return id, revisions, rows.Err()
}

func convertSame(ctx context.Context, q ledger.Querier, id int64, want []ledger.Line) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT account, amount FROM lines WHERE entry_id = ?`, id)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	have := map[string]int64{}
	for rows.Next() {
		var a string
		var v int64
		if err := rows.Scan(&a, &v); err != nil {
			return false, err
		}
		have[a] = v
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(have) != len(want) {
		return false, nil
	}
	for _, l := range want {
		if have[l.Account] != int64(l.Amount) {
			return false, nil
		}
	}
	return true, nil
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
