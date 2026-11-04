package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
)

// A conversion of a coin into USDT on Bybit moves money inside the account and has no journal row of
// its own: the row of the coin's deposit stands on bybit:btc in dollars at the date of the deposit. The
// entry takes the sold part of the coin's book value off bybit:btc and puts what the sale gave on bybit;
// the difference is a gain or loss of the price. A journal row written with the method «… → Convert USDT»
// already counted the USDT, so that conversion has no entry. Rows are written later than the movement, so
// every run compares what the conversion should post now with what stands: a change is a reversal and a
// new entry.

type convertRow struct {
	id    string
	at    string
	coin  string
	delta float64
}

// PostConverts brings the entries of the conversions in the Funding rows in line with the journal.
func PostConverts(ctx context.Context, q ledger.Querier, now time.Time) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT source_id, occurred_at, payload FROM events WHERE source = 'bybit' AND kind = 'funding'
		AND json_extract(payload, '$.type') = 'Convert' ORDER BY occurred_at`)
	if err != nil {
		return 0, err
	}
	bySecond := map[string][]convertRow{}
	for rows.Next() {
		var id, at, payload string
		if err := rows.Scan(&id, &at, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		var p struct {
			Coin   string          `json:"coin"`
			Signed json.RawMessage `json:"signed"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			rows.Close()
			return 0, err
		}
		s, err := strconv.ParseFloat(trimQuotes(string(p.Signed)), 64)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("funding %s: signed %s", id, p.Signed)
		}
		bySecond[at] = append(bySecond[at], convertRow{id: id, at: at, coin: p.Coin, delta: s})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	seconds := make([]string, 0, len(bySecond))
	for s := range bySecond {
		seconds = append(seconds, s)
	}
	sort.Strings(seconds)
	posted := 0
	for _, s := range seconds {
		var usdt *convertRow
		spent, sold := "", 0.0
		for i := range bySecond[s] {
			r := &bySecond[s][i]
			if r.coin == "USDT" && r.delta > 0 {
				usdt = r
			} else if r.delta < 0 {
				spent, sold = r.coin, sold-r.delta
			}
		}
		if usdt == nil || spent == "" {
			continue // anything else (USDC swaps, sales for another coin) stays for the owner
		}
		coinAccount := "bybit:" + lower(spent)
		var known int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE code = ?`, coinAccount).Scan(&known); err != nil {
			return posted, err
		}
		if known == 0 {
			continue // a coin without a book account stays for the owner
		}
		key := "bybit:convert:" + usdt.id
		want, err := convertLines(ctx, q, key, s, spent, coinAccount, sold, money.FromFloat(usdt.delta))
		if err != nil {
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
			if _, err := ledger.Reverse(ctx, q, active, ledger.Reversal{Author: "bybit", Memo: "сторно: журнал изменил продажу " + spent}, now); err != nil {
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
		if _, err := ledger.Post(ctx, q, ledger.Entry{EffectiveAt: s, Kind: "bybit:convert", Memo: "продажа " + spent + " за USDT на Bybit",
			Author: "bybit", SourceKey: next, Lines: want}, now); err != nil {
			return posted, fmt.Errorf("%s: %w", next, err)
		}
		posted++
	}
	return posted, nil
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
