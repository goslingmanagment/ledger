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
// entry takes the coin's book value off bybit:btc and puts what the sale gave on bybit; the difference is
// a gain or loss of the price. A journal row written with the method «… → Convert USDT» already counted
// the USDT, so that conversion has no entry here.

type convertRow struct {
	id    string
	at    string
	coin  string
	delta float64
}

// PostConverts posts the conversions found in the Funding rows, once each.
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
		spent := ""
		for i := range bySecond[s] {
			r := &bySecond[s][i]
			if r.coin == "USDT" && r.delta > 0 {
				usdt = r
			} else if r.delta < 0 {
				spent = r.coin
			}
		}
		if usdt == nil || spent == "" {
			continue // anything else (USDC swaps, sales for another coin) stays for the owner
		}
		var journaled int
		day := s[:10]
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM v_documents WHERE method LIKE '%Convert USDT%' AND status = 'paid'
			AND abs(amount_micros - ?) < 20000 AND abs(julianday(date) - julianday(?)) <= 3`, int64(money.FromFloat(usdt.delta)), day).Scan(&journaled); err != nil {
			return posted, err
		}
		if journaled > 0 {
			continue
		}
		key := "bybit:convert:" + usdt.id
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM entries WHERE source_key = ?`, key).Scan(&exists); err != nil {
			return posted, err
		}
		if exists > 0 {
			continue
		}
		coinAccount := "bybit:" + lower(spent)
		var known int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE code = ?`, coinAccount).Scan(&known); err != nil {
			return posted, err
		}
		if known == 0 {
			continue // a coin without a book account stays for the owner
		}
		var bookAt int64
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(l.amount), 0) FROM lines l JOIN entries e ON e.id = l.entry_id
			WHERE l.account = ? AND e.effective_at <= ?`, coinAccount, s).Scan(&bookAt); err != nil {
			return posted, err
		}
		got := money.FromFloat(usdt.delta)
		book := money.Micros(bookAt)
		lines := []ledger.Line{{Account: "bybit", Amount: got, Currency: "USDT", Memo: "продажа " + spent}}
		if book != 0 {
			lines = append(lines, ledger.Line{Account: coinAccount, Amount: -book, Memo: "книжная стоимость проданного"})
		}
		if diff := got - book; diff != 0 {
			memo := "разница цены при продаже " + spent
			if math.Abs(diff.Float()) > got.Float() {
				memo += " (без книжной стоимости: проверить строку депозита)"
			}
			lines = append(lines, ledger.Line{Account: "income:other", Amount: -diff, Memo: memo})
		}
		if len(lines) < 2 {
			continue
		}
		if _, err := ledger.Post(ctx, q, ledger.Entry{EffectiveAt: s, Kind: "bybit:convert", Memo: "продажа " + spent + " за USDT на Bybit",
			Author: "bybit", SourceKey: key, Lines: lines}, now); err != nil {
			return posted, fmt.Errorf("%s: %w", key, err)
		}
		posted++
	}
	return posted, nil
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
