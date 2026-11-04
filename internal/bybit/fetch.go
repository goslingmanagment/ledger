package bybit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Endpoints and statuses (v5 docs; the same as ledger-sync fetcher.py).
const (
	depositPath         = "/v5/asset/deposit/query-record"
	internalDepositPath = "/v5/asset/deposit/query-internal-record"
	withdrawPath        = "/v5/asset/withdraw/query-record"
	p2pPath             = "/v5/p2p/order/simplifyList"

	window      = 29 * 24 * time.Hour // the asset endpoints accept less than 30 days
	pageLimit   = "50"
	p2pPageSize = 30 // the endpoint refuses size 31 with retCode 40001
	p2pMaxPages = 40

	depositOK         = "3"
	internalDepositOK = "2"
	withdrawOK        = "success"
	p2pDone           = "50"
	p2pSell           = "1"
)

// Terminal withdrawal statuses without a money movement; any other non-success status is pending.
var withdrawFailed = map[string]bool{"CancelByUser": true, "Reject": true, "Fail": true}

// Movement kinds, also the prefixes of their keys.
var movementKinds = []string{"deposit", "internal_deposit", "withdrawal", "p2p"}

// Movement is one money movement, normalized like ledger-sync, with the raw Bybit record.
type Movement struct {
	Key              string          `json:"key"`
	Kind             string          `json:"kind"`
	Direction        string          `json:"direction"` // income | expense
	TxID             string          `json:"txid"`
	TimeMs           int64           `json:"time_ms"`
	At               string          `json:"at"`
	Date             string          `json:"date"`
	Coin             string          `json:"coin"`
	Amount           string          `json:"amount"`
	Fee              string          `json:"fee"`
	CounterpartyKind string          `json:"counterparty_kind"` // address | uid | p2p
	Counterparty     string          `json:"counterparty"`
	Method           string          `json:"method"`
	Chain            string          `json:"chain,omitempty"`
	Status           string          `json:"status,omitempty"`
	Fiat             string          `json:"fiat,omitempty"`
	Price            string          `json:"price,omitempty"`
	Imported         string          `json:"imported,omitempty"` // ledger-sync table the event came from
	Raw              json.RawMessage `json:"raw,omitempty"`
}

// Event is the ledger event of the movement.
func (m Movement) Event() ledger.Event {
	return ledger.Event{Source: "bybit", SourceID: m.Key, Kind: m.Kind, OccurredAt: m.At, Payload: m}
}

// record is one row of an answer: fields for reading, raw bytes for the event.
type record struct {
	f   map[string]any
	raw json.RawMessage
}

func decodeRecord(raw json.RawMessage) (record, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var f map[string]any
	if err := dec.Decode(&f); err != nil {
		return record{}, err
	}
	return record{f: f, raw: raw}, nil
}

// s reads a field as text whatever JSON type Bybit used.
func (r record) s(key string) string {
	switch v := r.f[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// toMs reads a time: Bybit sends seconds in some places and milliseconds in others.
func toMs(v string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("время %q: %v", v, err)
	}
	if n < 1_000_000_000_000 {
		n *= 1000
	}
	return n, nil
}

// chains maps Bybit chain codes to the journal's method names.
var chains = map[string]string{"TRX": "TRC-20", "ETH": "ERC-20", "BSC": "BEP-20"}

func method(chain, coin string, internal bool) string {
	if internal {
		return "Internal Transfer (Bybit)"
	}
	if n, ok := chains[chain]; ok {
		chain = n
	}
	return strings.TrimSpace(coin + " " + chain)
}

func newMovement(kind, id, direction, txid, timeVal, coin, amount, fee, cpKind, cp, meth string, raw json.RawMessage) (Movement, error) {
	if id == "" {
		return Movement{}, fmt.Errorf("%s без id: %s", kind, snippet(raw))
	}
	ms, err := toMs(timeVal)
	if err != nil {
		return Movement{}, fmt.Errorf("%s:%s: %v", kind, id, err)
	}
	t := time.UnixMilli(ms)
	return Movement{Key: kind + ":" + id, Kind: kind, Direction: direction, TxID: txid, TimeMs: ms, At: ts.FormatMilli(t),
		Date: ts.Date(t), Coin: coin, Amount: amount, Fee: fee, CounterpartyKind: cpKind, Counterparty: cp, Method: meth,
		Raw: raw}, nil
}

func depositMovement(r record) (Movement, error) {
	m, err := newMovement("deposit", r.s("id"), "income", r.s("txID"), r.s("successAt"), r.s("coin"), r.s("amount"),
		r.s("depositFee"), "address", r.s("fromAddress"), method(r.s("chain"), r.s("coin"), false), r.raw)
	m.Chain, m.Status = r.s("chain"), r.s("status")
	return m, err
}

func internalDepositMovement(r record) (Movement, error) {
	m, err := newMovement("internal_deposit", r.s("id"), "income", r.s("txID"), r.s("createdTime"), r.s("coin"),
		r.s("amount"), "", "uid", r.s("fromMemberId"), method("", r.s("coin"), true), r.raw)
	m.Status = r.s("status")
	return m, err
}

func withdrawalMovement(r record) (Movement, error) {
	internal := r.s("withdrawType") == "1"
	kind := "address"
	if internal {
		kind = "uid"
	}
	m, err := newMovement("withdrawal", r.s("withdrawId"), "expense", r.s("txID"), r.s("createTime"), r.s("coin"),
		r.s("amount"), r.s("withdrawFee"), kind, r.s("toAddress"), method(r.s("chain"), r.s("coin"), internal), r.raw)
	m.Chain, m.Status = r.s("chain"), r.s("status")
	return m, err
}

// p2pMovement maps a finished P2P order: side 1 sells the token for fiat (money leaves Bybit).
func p2pMovement(r record) (Movement, error) {
	direction := "income"
	if r.s("side") == p2pSell {
		direction = "expense"
	}
	cp := r.s("targetNickName")
	if cp == "" {
		cp = r.s("targetUserId")
	}
	qty, err := p2pQuantity(r)
	if err != nil {
		return Movement{}, err
	}
	m, err := newMovement("p2p", r.s("id"), direction, "", r.s("createDate"), r.s("tokenId"), qty, r.s("fee"), "p2p",
		cp, "P2P (Bybit)", r.raw)
	m.Status = r.s("status")
	m.Fiat = strings.TrimSpace(r.s("amount") + " " + r.s("currencyId"))
	m.Price = r.s("price")
	return m, err
}

// p2pQuantity is how much token really moved: notifyTokenQuantity, else quantity, else fiat / price.
func p2pQuantity(r record) (string, error) {
	for _, k := range []string{"notifyTokenQuantity", "quantity"} {
		if v := r.s(k); v != "" && v != "0" {
			return v, nil
		}
	}
	fiat, ok1 := rat(r.s("amount"))
	price, ok2 := rat(r.s("price"))
	if !ok1 || !ok2 || price.Sign() == 0 {
		return "", fmt.Errorf("p2p:%s: нет количества токена и курса", r.s("id"))
	}
	return fixed(fiat.Quo(fiat, price), 4), nil
}

// paged reads every page of a cursor-paginated list.
func paged(ctx context.Context, c *Client, path string, params map[string]string) ([]record, error) {
	var out []record
	cursor := ""
	for {
		p := map[string]string{"limit": pageLimit, "cursor": cursor}
		for k, v := range params {
			p[k] = v
		}
		res, err := c.Get(ctx, path, p)
		if err != nil {
			return nil, err
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		rowsRaw, ok := page["rows"]
		if !ok {
			return nil, fmt.Errorf("%s: в ответе нет rows — изменилась схема API", path)
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(rowsRaw, &rows); err != nil {
			return nil, fmt.Errorf("%s: rows: %v", path, err)
		}
		for _, raw := range rows {
			r, err := decodeRecord(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", path, err)
			}
			out = append(out, r)
		}
		var next string
		if n, ok := page["nextPageCursor"]; ok {
			_ = json.Unmarshal(n, &next)
		}
		if next == "" {
			return out, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("%s: курсор повторился — зацикленная пагинация", path)
		}
		cursor = next
	}
}

// Window is a half-open time range.
type Window struct {
	From time.Time
	To   time.Time
}

// windows splits [from, to) into pieces of at most size.
func windows(from, to time.Time, size time.Duration) []Window {
	var out []Window
	for s := from; s.Before(to); {
		e := s.Add(size)
		if e.After(to) {
			e = to
		}
		out = append(out, Window{From: s, To: e})
		s = e
	}
	return out
}

func msParam(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// PendingWithdrawal is a withdrawal seen in a non-terminal status.
type PendingWithdrawal struct {
	Movement Movement
	Status   string
}

// Fetched is what one pass over deposits, internal deposits and withdrawals found.
type Fetched struct {
	Movements []Movement // succeeded, unique by key, in time order
	Skipped   int        // rows in another status
	Pending   []PendingWithdrawal
}

// FetchMovements reads deposits, internal deposits and withdrawals of [from, to) in 29-day windows.
func FetchMovements(ctx context.Context, c *Client, from, to time.Time) (Fetched, error) {
	var f Fetched
	for _, w := range windows(from, to, window) {
		params := map[string]string{"startTime": msParam(w.From), "endTime": msParam(w.To)}
		rows, err := paged(ctx, c, depositPath, params)
		if err != nil {
			return f, err
		}
		for _, r := range rows {
			if r.s("status") != depositOK {
				f.Skipped++
				continue
			}
			m, err := depositMovement(r)
			if err != nil {
				return f, err
			}
			f.Movements = append(f.Movements, m)
		}
		if rows, err = paged(ctx, c, internalDepositPath, params); err != nil {
			return f, err
		}
		for _, r := range rows {
			if r.s("status") != internalDepositOK {
				f.Skipped++
				continue
			}
			m, err := internalDepositMovement(r)
			if err != nil {
				return f, err
			}
			f.Movements = append(f.Movements, m)
		}
		wp := map[string]string{"withdrawType": "2"} // on-chain and internal
		for k, v := range params {
			wp[k] = v
		}
		if rows, err = paged(ctx, c, withdrawPath, wp); err != nil {
			return f, err
		}
		for _, r := range rows {
			m, err := withdrawalMovement(r)
			if err != nil {
				return f, err
			}
			if st := r.s("status"); st != withdrawOK {
				f.Skipped++
				if !withdrawFailed[st] {
					f.Pending = append(f.Pending, PendingWithdrawal{Movement: m, Status: st})
				}
				continue
			}
			f.Movements = append(f.Movements, m)
		}
	}
	f.Movements = uniqueSorted(f.Movements)
	return f, nil
}

func uniqueSorted(ms []Movement) []Movement {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].TimeMs < ms[j].TimeMs })
	seen := map[string]bool{}
	out := ms[:0]
	for _, m := range ms {
		if !seen[m.Key] {
			seen[m.Key] = true
			out = append(out, m)
		}
	}
	return out
}

// FetchWithdrawal asks for one withdrawal by id: its status and movement, or "" when Bybit has none.
func FetchWithdrawal(ctx context.Context, c *Client, id string) (string, *Movement, error) {
	rows, err := paged(ctx, c, withdrawPath, map[string]string{"withdrawID": id})
	if err != nil {
		return "", nil, err
	}
	for _, r := range rows {
		if r.s("withdrawId") == id {
			m, err := withdrawalMovement(r)
			if err != nil {
				return "", nil, err
			}
			return r.s("status"), &m, nil
		}
	}
	return "", nil, nil
}

// FetchP2P reads finished P2P orders of [from, to). P2P is neither a deposit nor a withdrawal: without
// this stream the owner's P2P sales fell out of the sync entirely.
func FetchP2P(ctx context.Context, c *Client, from, to time.Time) ([]Movement, error) {
	var out []Movement
	for _, w := range windows(from, to, window) {
		for page := 1; ; page++ {
			if page > p2pMaxPages {
				return nil, fmt.Errorf("%s: больше %d страниц — сузь окно", p2pPath, p2pMaxPages)
			}
			res, err := c.Post(ctx, p2pPath, map[string]any{"page": page, "size": p2pPageSize,
				"beginTime": msParam(w.From), "endTime": msParam(w.To)})
			if err != nil {
				return nil, err
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(res, &body); err != nil {
				return nil, fmt.Errorf("%s: %v", p2pPath, err)
			}
			itemsRaw, ok := body["items"]
			if !ok {
				return nil, fmt.Errorf("%s: в ответе нет items — изменилась схема API", p2pPath)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(itemsRaw, &items); err != nil {
				return nil, fmt.Errorf("%s: items: %v", p2pPath, err)
			}
			for _, raw := range items {
				r, err := decodeRecord(raw)
				if err != nil {
					return nil, fmt.Errorf("%s: %v", p2pPath, err)
				}
				if r.s("status") != p2pDone {
					continue
				}
				m, err := p2pMovement(r)
				if err != nil {
					return nil, err
				}
				out = append(out, m)
			}
			if len(items) < p2pPageSize {
				break
			}
		}
	}
	return uniqueSorted(out), nil
}
