// Package cards is the CinCin leg of the books: the bot's messages become events (read on the owner's
// Mac, the only place with the Telegram session), and on the server each event becomes what it means:
// a load between own accounts, a fee, a purchase to name, a hidden charge in the balance chain, a
// deposit tied to its Bybit withdrawal, an anchor of the balance.
package cards

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Source is the events source of the bot.
const Source = "cincin_bot"

// Kinds of events.
const (
	KindDeposit        = "deposit"
	KindDepositPending = "deposit_pending"
	KindLoad           = "load"
	KindPurchase       = "purchase"
	KindDeclined       = "declined"
	KindWalletAnchor   = "wallet_anchor"
)

// The bot's text: markdown bold around numbers, any space that looks like one.
const (
	bold = `\*{0,2}`
	ws   = `[\s\p{Z}]*`
)

var (
	reDeposit = regexp.MustCompile(`(?s)Заявка №` + ws + bold + `(?P<id>[\d-]+)` + bold + `.*?Сумма оплаты:` + ws + bold + `(?P<gross>[\d.,]+)` + ws + `USDT` +
		`.*?Комиссия:` + ws + bold + `(?P<fee>[\d.,]+).*?Зачислено:` + ws + bold + `(?P<net>[\d.,]+)` +
		`.*?Статус транзакции:` + ws + bold + `(?P<status>[^*\n|]+)`)
	reLoad = regexp.MustCompile(`(?s)Баланс карты` + ws + bold + `(?P<card>\d{4})` + bold + ws + `пополнен на` + ws + bold + `(?P<amt>[\d.,]+)` + ws + `USD` +
		`.*?Доступный баланс:` + ws + bold + `(?P<bal>[\d.,]+)`)
	rePurchase = regexp.MustCompile(`(?s)💳` + ws + bold + `(?P<amt>[\d.,]+)` + ws + `USD` + bold + ws + `\|?` + ws + `Транзакция в` + ws + `(?P<merchant>.+?)` + ws + `\|` + ws +
		`Доступный баланс:` + ws + bold + `(?P<bal>[\d.,]+)`)
	reDeclined = regexp.MustCompile(`(?i)отклонен`)
	reWallet   = regexp.MustCompile(`Текущий баланс:` + ws + `\|?` + ws + `USDT:` + ws + bold + `([\d.,]+)`)
)

func group(re *regexp.Regexp, m []string, name string) string { return m[re.SubexpIndex(name)] }

// Message is one message of `tg history`.
type Message struct {
	ID   int64  `json:"id"`
	Date string `json:"date"`
	Out  bool   `json:"out"`
	Text string `json:"text"`
}

// History is the answer of `tg history`.
type History struct {
	Messages []Message `json:"messages"`
}

// Parsed is a message of the bot as a fact: the payload of its event.
type Parsed struct {
	MsgID        int64             `json:"msg_id"`
	TsMs         int64             `json:"ts_ms"`
	At           string            `json:"at"`
	Kind         string            `json:"kind"`
	Card         string            `json:"card,omitempty"`
	Amount       string            `json:"amount,omitempty"`
	Merchant     string            `json:"merchant,omitempty"`
	BalanceAfter string            `json:"balance_after,omitempty"`
	Raw          string            `json:"raw"`
	Extra        map[string]string `json:"extra"`
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ParseMessage reads one message: a fact of the bot, or nil for a service message, a menu or our own
// command. The match order matters and is the one the regular review has always used.
func ParseMessage(m Message) (*Parsed, error) {
	if m.Out {
		return nil, nil
	}
	text := strings.ReplaceAll(m.Text, "\n", " | ")
	base := func(kind string) (*Parsed, error) {
		t, err := ts.Parse(m.Date)
		if err != nil {
			return nil, fmt.Errorf("сообщение %d: %w", m.ID, err)
		}
		return &Parsed{MsgID: m.ID, TsMs: t.UnixMilli(), At: ts.Format(t), Kind: kind, Raw: firstRunes(text, 600), Extra: map[string]string{}}, nil
	}
	if g := reDeposit.FindStringSubmatch(text); g != nil {
		status := strings.TrimSpace(group(reDeposit, g, "status"))
		kind := KindDepositPending
		if strings.HasPrefix(status, "Проведен") {
			kind = KindDeposit
		}
		p, err := base(kind)
		if err != nil {
			return nil, err
		}
		p.Amount = group(reDeposit, g, "gross")
		p.Extra = map[string]string{"zayavka": group(reDeposit, g, "id"), "fee": group(reDeposit, g, "fee"), "net": group(reDeposit, g, "net"), "status": status}
		return p, nil
	}
	if g := reLoad.FindStringSubmatch(text); g != nil {
		p, err := base(KindLoad)
		if err != nil {
			return nil, err
		}
		p.Card, p.Amount, p.BalanceAfter = group(reLoad, g, "card"), group(reLoad, g, "amt"), group(reLoad, g, "bal")
		return p, nil
	}
	if g := rePurchase.FindStringSubmatch(text); g != nil {
		p, err := base(KindPurchase)
		if err != nil {
			return nil, err
		}
		p.Amount, p.BalanceAfter = group(rePurchase, g, "amt"), group(rePurchase, g, "bal")
		p.Merchant = strings.Trim(strings.TrimSpace(group(rePurchase, g, "merchant")), "*")
		return p, nil
	}
	if strings.Contains(text, "💳") && reDeclined.MatchString(text) {
		return base(KindDeclined)
	}
	if g := reWallet.FindStringSubmatch(text); g != nil {
		p, err := base(KindWalletAnchor)
		if err != nil {
			return nil, err
		}
		p.Amount, p.BalanceAfter = g[1], g[1]
		return p, nil
	}
	return nil, nil
}

// amountOf reads an amount as the bot writes it: a comma may be the decimal point, or thousands when a
// dot is there too.
func amountOf(s string) (money.Micros, error) {
	t := strings.TrimRight(strings.TrimSpace(s), ".,")
	if strings.Contains(t, ",") && strings.Contains(t, ".") {
		t = strings.ReplaceAll(t, ",", "")
	}
	return money.Parse(t)
}

// loadGross is what a load of net took from the wallet: the 4.3% are held back before crediting, so
// their base is the charge, not what the bot printed. A calculation by the tariff, not an observation.
func loadGross(net money.Micros) money.Micros {
	return roundCentsUp(money.Micros(int64(net) * 1000 / 957))
}

// loadFee is what the load cost, so the chain closes to the cent.
func loadFee(net money.Micros) (fee, gross money.Micros) {
	gross = loadGross(net)
	return gross - roundCentsUp(net), gross
}

// roundCentsUp rounds half up to a whole cent, the one way amounts of this package are rounded.
func roundCentsUp(m money.Micros) money.Micros {
	if m < 0 {
		return -roundCentsUp(-m)
	}
	return (m + 5_000) / 10_000 * 10_000
}

// Event is a stored event of the bot, from either door: the collector or the state of ledger-sync.
type Event struct {
	ID           int64
	MsgID        int64
	Kind         string
	At           string
	Time         time.Time
	Card         string
	Amount       string
	Merchant     string
	BalanceAfter string
	Extra        map[string]string
	Sync         *SyncState // what ledger-sync had decided when the event was imported
}

// SyncState is the processing state of ledger-sync kept with an imported event.
type SyncState struct {
	Status      string
	Delta       string
	DeltaStatus string
	BybitKey    string
}

type rawPayload struct {
	MsgID        json.Number     `json:"msg_id"`
	Card         *string         `json:"card"`
	Amount       *string         `json:"amount"`
	Merchant     *string         `json:"merchant"`
	BalanceAfter *string         `json:"balance_after"`
	Raw          *string         `json:"raw"`
	Extra        json.RawMessage `json:"extra"`
	LedgerSync   *struct {
		Status      *string `json:"status"`
		Delta       *string `json:"delta"`
		DeltaStatus *string `json:"delta_status"`
		BybitKey    *string `json:"bybit_key"`
	} `json:"ledger_sync"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// decodeEvent reads a payload. An imported one keeps text and extra as JSON inside raw.
func decodeEvent(id int64, sourceID, kind, at, payload string) (Event, error) {
	e := Event{ID: id, Kind: kind, At: at, Extra: map[string]string{}}
	var err error
	if e.MsgID, err = strconv.ParseInt(sourceID, 10, 64); err != nil {
		return e, fmt.Errorf("событие бота %q: %w", sourceID, err)
	}
	if e.Time, err = ts.Parse(at); err != nil {
		return e, fmt.Errorf("событие бота %d: %w", e.MsgID, err)
	}
	var p rawPayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return e, fmt.Errorf("событие бота %d: %w", e.MsgID, err)
	}
	e.Card, e.Amount, e.Merchant, e.BalanceAfter = str(p.Card), str(p.Amount), str(p.Merchant), str(p.BalanceAfter)
	extra := p.Extra
	if len(extra) == 0 && p.Raw != nil && strings.HasPrefix(*p.Raw, "{") {
		var inner struct {
			Extra json.RawMessage `json:"extra"`
		}
		if json.Unmarshal([]byte(*p.Raw), &inner) == nil {
			extra = inner.Extra
		}
	}
	if len(extra) > 0 {
		var vals map[string]any
		if err := json.Unmarshal(extra, &vals); err != nil {
			return e, fmt.Errorf("событие бота %d: extra: %w", e.MsgID, err)
		}
		for k, v := range vals {
			if s, ok := v.(string); ok {
				e.Extra[k] = s
			}
		}
	}
	if p.LedgerSync != nil {
		e.Sync = &SyncState{Status: str(p.LedgerSync.Status), Delta: str(p.LedgerSync.Delta), DeltaStatus: str(p.LedgerSync.DeltaStatus), BybitKey: str(p.LedgerSync.BybitKey)}
	}
	return e, nil
}
