package cards

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// All data in these tests is synthetic.

func at(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func open(t *testing.T, mode string) (*db.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"accrual_from": "2026-09-01", "models": "Alfa", "mode": mode, "cards_load_fee": "0.05"} {
		if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES (?, ?, 'now')`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range [][2]string{{"(?i)servicea", "Инструменты"}, {"mailer", "AI и разработка"}} {
		if _, err := s.RW.Exec(`INSERT INTO rules(kind, match_merchant, name, category, note, created_at, source) VALUES ('merchant', ?, ?, ?, '', 'now', 'test')`,
			r[0], "Сервис "+r[0][len(r[0])-3:], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"TcincinOne", "TcincinTwo"} {
		if _, err := s.RW.Exec(`INSERT INTO rules(kind, match_address, direction, name, category, model, note, created_at, source)
			VALUES ('counterparty', ?, 'expense', 'CinCin', 'Внутренний перевод', '—', '', 'now', 'test')`, a); err != nil {
			t.Fatal(err)
		}
	}
	return s, ctx
}

func count(t *testing.T, s *db.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.RW.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(query, err)
	}
	return n
}

func botEvent(t *testing.T, s *db.Store, ctx context.Context, msg int64, when, kind, card, amount, merchant, after string, extra map[string]string) int64 {
	t.Helper()
	w := at(when)
	if extra == nil {
		extra = map[string]string{}
	}
	p := Parsed{MsgID: msg, TsMs: w.UnixMilli(), At: ts.Format(w), Kind: kind, Card: card, Amount: amount, Merchant: merchant, BalanceAfter: after, Raw: "сообщение бота", Extra: extra}
	e, err := p.Event()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: e.Source, SourceID: e.SourceID, Kind: e.Kind, OccurredAt: e.OccurredAt, Payload: e.Payload}, w)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// imported stores an event the way the state of ledger-sync gives it.
func imported(t *testing.T, s *db.Store, ctx context.Context, msg int64, when, kind, card, amount, after, status, delta, deltaStatus, bybitKey string) int64 {
	t.Helper()
	w := at(when)
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	payload := map[string]any{"msg_id": msg, "ts_ms": w.UnixMilli(), "kind": kind, "card": nullable(card), "amount": nullable(amount), "merchant": nil, "balance_after": nullable(after),
		"raw": `{"raw":"сообщение бота","extra":{"net":"97","fee":"3"}}`, "at": ts.Format(w), "imported": "card_events",
		"ledger_sync": map[string]any{"status": status, "delta": nullable(delta), "delta_status": nullable(deltaStatus), "bybit_key": nullable(bybitKey)}}
	id, _, err := ledger.AddEvent(ctx, s.RW, ledger.Event{Source: Source, SourceID: strconv.FormatInt(msg, 10), Kind: kind, OccurredAt: ts.Format(w), Payload: payload}, w)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func process(t *testing.T, s *db.Store, ctx context.Context, now string) Report {
	t.Helper()
	var rep Report
	if err := brief.InTx(ctx, s, func(tx *sql.Tx) error {
		var err error
		rep, err = Process(ctx, tx, at(now))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rep
}

func proposal(t *testing.T, s *db.Store, key string) (brief.Proposal, brief.Content) {
	t.Helper()
	p, ok, err := brief.LatestProposal(context.Background(), s.RW, key)
	if err != nil || !ok {
		t.Fatalf("%s: %v %v", key, ok, err)
	}
	c, err := p.ParseContent()
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

func TestParseMessageOnSyntheticBotTexts(t *testing.T) {
	const date = "2026-09-10T09:00:00+00:00"
	cases := []struct {
		name, text, kind, card, amount, merchant, after string
		extra                                           map[string]string
	}{
		{"deposit", "Заявка № **599-1234**\nСумма оплаты: **100 USDT**\nЗаявка ID: **599-1234**\nСеть: **TRC20**\nКомиссия: **3 USDT**\nЗачислено: **97 USDT**\nДата операции: **10 сентября 2026, 12:00:00**\nСтатус транзакции: **Проведена**\nХеш транзакции: abc",
			KindDeposit, "", "100", "", "", map[string]string{"zayavka": "599-1234", "fee": "3", "net": "97", "status": "Проведена"}},
		{"pending", "Заявка № **599-1235**\nСумма оплаты: **50.5 USDT**\nКомиссия: **3 USDT**\nЗачислено: **47.5 USDT**\nСтатус транзакции: **В обработке**", KindDepositPending, "", "50.5", "", "",
			map[string]string{"zayavka": "599-1235", "fee": "3", "net": "47.5", "status": "В обработке"}},
		{"load", "💳 Баланс карты **1111** пополнен на **50.25 USD**\nДоступный баланс: **60.75 USD**", KindLoad, "1111", "50.25", "", "60.75", nil},
		{"purchase", "💳 **12.34 USD**\nТранзакция в ServiceA* Pro\nДоступный баланс: **48.41 USD**", KindPurchase, "", "12.34", "ServiceA* Pro", "48.41", nil},
		{"purchase with bold merchant", "💳 **1,5 USD** | Транзакция в **Mailer** | Доступный баланс: **8,5 USD**", KindPurchase, "", "1,5", "Mailer", "8,5", nil},
		{"declined", "💳 Покупка на **10 USD** в ServiceB была отклонена\nДоступный баланс: **5 USD**", KindDeclined, "", "", "", "", nil},
		{"wallet", "Текущий баланс:\nUSDT: 143.27", KindWalletAnchor, "", "143.27", "", "143.27", nil},
		{"wallet bold", "Текущий баланс: USDT: **0.01**", KindWalletAnchor, "", "0.01", "", "0.01", nil},
		{"service", "Ваш код подтверждения для карты **1111** — **123456**", "", "", "", "", "", nil},
		{"menu", "💳 Карта CinCin", "", "", "", "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseMessage(Message{ID: 42, Date: date, Text: c.text})
			if err != nil {
				t.Fatal(err)
			}
			if c.kind == "" {
				if p != nil {
					t.Fatalf("not a fact: %+v", p)
				}
				return
			}
			if p == nil {
				t.Fatal("no fact")
			}
			if p.Kind != c.kind || p.Card != c.card || p.Amount != c.amount || p.Merchant != c.merchant || p.BalanceAfter != c.after {
				t.Fatalf("%+v", p)
			}
			if p.MsgID != 42 || p.At != "2026-09-10T12:00:00+03:00" || p.TsMs != at(date).UnixMilli() || !strings.Contains(p.Raw, firstRunes(strings.ReplaceAll(c.text, "\n", " | "), 20)) {
				t.Fatalf("%+v", p)
			}
			for k, v := range c.extra {
				if p.Extra[k] != v {
					t.Fatalf("extra %s: %+v", k, p.Extra)
				}
			}
		})
	}
	if p, err := ParseMessage(Message{ID: 1, Date: date, Out: true, Text: "Текущий баланс: USDT: 1"}); p != nil || err != nil {
		t.Fatal("our own message", p, err)
	}
	if _, err := ParseMessage(Message{ID: 1, Date: "не дата", Text: "Текущий баланс: USDT: 1"}); err == nil {
		t.Fatal("bad date")
	}
	long, err := ParseMessage(Message{ID: 1, Date: date, Text: "Текущий баланс: USDT: 1\n" + strings.Repeat("я", 700)})
	if err != nil || len([]rune(long.Raw)) != 600 {
		t.Fatal(err)
	}
}

func TestLoadFee(t *testing.T) {
	for net, want := range map[string][2]string{"50": {"2.63", "52.63"}, "100": {"5.26", "105.26"}, "10.5": {"0.55", "11.05"}, "1": {"0.05", "1.05"}} {
		fee, gross := loadFee(money.MustParse(net), 50)
		if fee.Cents() != want[0] || gross.Cents() != want[1] {
			t.Fatalf("%s: %s %s", net, fee.Cents(), gross.Cents())
		}
	}
	for _, c := range [][2]string{{"12,5", "12.50"}, {"1,234.50", "1234.50"}, {"7.", "7.00"}} {
		v, err := amountOf(c[0])
		if err != nil || v.Cents() != c[1] {
			t.Fatalf("%s: %s %v", c[0], v, err)
		}
	}
}

func ev(msg int64, when, kind, card, amount, merchant, after string) Event {
	return Event{ID: msg, MsgID: msg, Kind: kind, At: when, Time: at(when), Card: card, Amount: amount, Merchant: merchant, BalanceAfter: after, Extra: map[string]string{}}
}

func TestChainDeltasAndCardInference(t *testing.T) {
	steps, balances := Chain([]Event{
		ev(1, "2026-09-01T10:00:00+03:00", KindLoad, "1111", "100", "", "100"),
		ev(2, "2026-09-01T11:00:00+03:00", KindPurchase, "", "30", "ServiceA", "70"),   // closes on 1111
		ev(3, "2026-09-01T12:00:00+03:00", KindLoad, "2222", "50", "", "50"),           // a second card
		ev(4, "2026-09-01T13:00:00+03:00", KindPurchase, "", "10", "ServiceB", "39.5"), // 2222, 0.50 went unseen
		ev(5, "2026-09-01T13:00:03+03:00", KindPurchase, "", "10", "ServiceB", "39.5"), // the bot repeated itself
		ev(6, "2026-09-01T14:00:00+03:00", KindPurchase, "", "5", "ServiceC", "500"),   // fits no card
		ev(7, "2026-09-01T15:00:00+03:00", KindLoad, "1111", "10", "", "85"),           // the balance rose past the chain by 5
		ev(8, "2026-09-02T15:00:00+03:00", KindLoad, "3333", "20", "", "51"),           // a new card with 31 already on it
		ev(9, "2026-09-03T10:00:00+03:00", KindWalletAnchor, "", "3", "", "3"),
		ev(10, "2026-09-03T11:00:00+03:00", KindPurchase, "", "bad", "ServiceD", "1"),
	}, nil)
	check := func(i int, card string, delta string) {
		t.Helper()
		s := steps[i]
		if s.Card != card {
			t.Fatalf("step %d card %q", i, s.Card)
		}
		if delta == "" {
			if s.HasDelta {
				t.Fatalf("step %d delta %s", i, s.Delta)
			}
			return
		}
		if !s.HasDelta || s.Delta != money.MustParse(delta) {
			t.Fatalf("step %d delta %v %s", i, s.HasDelta, s.Delta)
		}
	}
	check(0, "1111", "")
	check(1, "1111", "")
	check(2, "2222", "")
	check(3, "2222", "0.50")
	if steps[4].DupOf != 4 || steps[4].HasDelta {
		t.Fatalf("repeat: %+v", steps[4])
	}
	check(5, "", "")
	check(6, "1111", "-5")
	check(7, "3333", "-31")
	if steps[9].Problem == "" {
		t.Fatal("amount that does not read")
	}
	for card, want := range map[string]string{"1111": "85", "2222": "39.5", "3333": "51"} {
		if balances[card] != money.MustParse(want) {
			t.Fatalf("%s: %s", card, balances[card])
		}
	}
	// The same text after five minutes is not a repeat of the bot: it stays an event to look at.
	steps, _ = Chain([]Event{
		ev(1, "2026-09-01T10:00:00+03:00", KindLoad, "1111", "100", "", "100"),
		ev(2, "2026-09-01T11:00:00+03:00", KindPurchase, "", "30", "ServiceA", "70"),
		ev(3, "2026-09-01T11:05:00+03:00", KindPurchase, "", "30", "ServiceA", "70"),
	}, nil)
	if steps[2].DupOf != 0 {
		t.Fatalf("%+v", steps[2])
	}
}

func TestLoadIsPostedOnceWithItsFeeProposal(t *testing.T) {
	s, ctx := open(t, "shadow")
	event := botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "50", "", "50", nil)
	rep := process(t, s, ctx, "2026-09-10T10:05:00+03:00")
	if rep.LoadsPosted != 1 || rep.Proposed != 1 || rep.Anchors != 1 {
		t.Fatalf("%+v", rep)
	}
	card, err := ledger.Balance(ctx, s.RW, "card:1111", "", "")
	if err != nil || card != money.MustParse("50") {
		t.Fatal(card, err)
	}
	wallet, err := ledger.Balance(ctx, s.RW, "cincin_wallet", "", "")
	if err != nil || wallet != money.MustParse("-50") {
		t.Fatal(wallet, err)
	}
	if n := count(t, s, `SELECT count(*) FROM entries e JOIN entry_events x ON x.entry_id = e.id WHERE e.source_key = 'cards:load:100' AND x.event_id = ?`, event); n != 1 {
		t.Fatalf("entry %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM observations WHERE kind = 'card_anchor' AND subject = 'card:1111' AND value = 50000000 AND event_id = ?`, event); n != 1 {
		t.Fatalf("anchor %d", n)
	}
	p, c := proposal(t, s, "card:100")
	if p.Kind != KindCardLoad || p.Block != "auto" || len(c.Documents) != 1 {
		t.Fatalf("%+v %+v", p, c)
	}
	d := c.Documents[0]
	if d.Category != "Комиссии" || d.Amount != "2.63" || d.Method != "Кошелёк CinCin" || d.Counterparty != "CinCin" || d.Type != "expense" || d.Date != "2026-09-10" {
		t.Fatalf("%+v", d)
	}
	if !strings.Contains(d.Note, "card:100") || !strings.Contains(p.Summary, "комиссия 2.63") {
		t.Fatalf("%s | %s", d.Note, p.Summary)
	}
	// Twice is once, and the fee row leaves the wallet, not the card: `ledger health` must not count it.
	rep = process(t, s, ctx, "2026-09-10T11:00:00+03:00")
	if rep.LoadsPosted != 0 || rep.Proposed != 0 || rep.Versions != 0 || rep.Anchors != 0 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM entries WHERE kind = 'card:load'`); n != 1 {
		t.Fatalf("entries %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE json_extract(content, '$.card') IS NOT NULL`); n != 0 {
		t.Fatalf("load pending on a card: %d", n)
	}
}

func TestLoadsBeforeTheConventionAreInTheJournalAlready(t *testing.T) {
	s, ctx := open(t, "shadow")
	botEvent(t, s, ctx, 100, "2026-09-04T10:00:00+03:00", KindLoad, "1111", "50", "", "50", nil)
	botEvent(t, s, ctx, 101, "2026-09-05T10:00:00+03:00", KindLoad, "1111", "10", "", "60", nil)
	rep := process(t, s, ctx, "2026-09-10T10:05:00+03:00")
	if rep.LoadsPosted != 1 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM entries WHERE source_key = 'cards:load:100'`); n != 0 {
		t.Fatal("the old convention writes a transfer to the card")
	}
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('cards_load_from', '2026-09-01', 'now')`); err != nil {
		t.Fatal(err)
	}
	if rep := process(t, s, ctx, "2026-09-10T10:06:00+03:00"); rep.LoadsPosted != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestLoadTheOwnerSaidWasNotALoadIsReversed(t *testing.T) {
	s, ctx := open(t, "shadow")
	event := botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "50", "", "50", nil)
	process(t, s, ctx, "2026-09-10T10:05:00+03:00")
	if _, err := bybit.AddLink(ctx, s.RW, bybit.Link{EventID: event, How: "mark", Decision: "custom", BriefRef: "ledger-sync:1/1", Note: "перенос между картами"}, at("2026-09-11")); err != nil {
		t.Fatal(err)
	}
	rep := process(t, s, ctx, "2026-09-11T10:00:00+03:00")
	if rep.LoadsReversed != 1 {
		t.Fatalf("%+v", rep)
	}
	wallet, err := ledger.Balance(ctx, s.RW, "cincin_wallet", "", "")
	if err != nil || wallet != 0 {
		t.Fatal(wallet, err)
	}
	if rep := process(t, s, ctx, "2026-09-11T11:00:00+03:00"); rep.LoadsReversed != 0 {
		t.Fatalf("%+v", rep)
	}
	var status string
	if err := s.RW.QueryRow(`SELECT status FROM proposals WHERE key = 'card:100'`).Scan(&status); err != nil || status != "superseded" {
		t.Fatal(status, err)
	}
	// A decision with a fee row from the wallet means a load.
	second := botEvent(t, s, ctx, 101, "2026-09-12T10:00:00+03:00", KindLoad, "1111", "20", "", "70", nil)
	if _, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 1, Date: "2026-09-12", Type: "expense", Category: "Комиссии", Counterparty: "CinCin", Model: "—",
		Amount: "0.90", Currency: "USD", Method: "Кошелёк CinCin", Status: "paid", Note: "комиссия", Author: "test", Origin: "test"}, at("2026-09-12")); err != nil {
		t.Fatal(err)
	}
	if _, err := bybit.AddLink(ctx, s.RW, bybit.Link{EventID: second, How: "mark", Decision: "confirm", DocumentNum: 1, BriefRef: "ledger-sync:1/2"}, at("2026-09-12")); err != nil {
		t.Fatal(err)
	}
	if rep := process(t, s, ctx, "2026-09-12T11:00:00+03:00"); rep.LoadsPosted != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestLoadThatDoesNotCloseTheChainIsAQuestion(t *testing.T) {
	s, ctx := open(t, "live")
	botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "80", "", "30", nil)
	process(t, s, ctx, "2026-09-10T10:05:00+03:00")
	p, c := proposal(t, s, "card:100")
	if p.Status != "open" || p.Block != "likely" || !strings.Contains(c.Hint, "цепочка") {
		t.Fatalf("%+v %+v", p, c)
	}
	if n := count(t, s, `SELECT count(*) FROM documents`); n != 0 {
		t.Fatal("nothing is written on a guess")
	}
	d, _ := proposal(t, s, "card:100:delta")
	if d.Kind != KindCardDelta || d.Status != "open" {
		t.Fatalf("%+v", d)
	}
	if rep := process(t, s, ctx, "2026-09-10T10:06:00+03:00"); rep.LoadsPosted != 0 || rep.Versions != 0 {
		t.Fatalf("%+v", rep)
	}
	// The owner: not a load. The fee goes, and so does the entry of the load.
	b, err := brief.Make(ctx, s.RW, brief.Options{}, at("2026-09-11T00:30:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	num := 0
	for _, it := range b.Items {
		if it.Key == "card:100" {
			num = it.Num
		}
	}
	if _, err := brief.Decide(ctx, s.RW, b.ID, []brief.Decision{{Num: num, Kind: brief.Personal}}, "owner", at("2026-09-11T01:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM documents`); n != 0 {
		t.Fatal("not a load, no fee row")
	}
	if rep := process(t, s, ctx, "2026-09-11T01:05:00+03:00"); rep.LoadsReversed != 1 {
		t.Fatalf("%+v", rep)
	}
	if wallet, err := ledger.Balance(ctx, s.RW, "cincin_wallet", "", ""); err != nil || wallet != 0 {
		t.Fatal(wallet, err)
	}
}

func TestRuleTheJournalRefusesIsAskedOnce(t *testing.T) {
	s, ctx := open(t, "live")
	if _, err := s.RW.Exec(`INSERT INTO rules(kind, match_merchant, name, category, note, created_at, source) VALUES ('merchant', 'oddshop', 'Лавка', 'Нет такой категории', '', 'now', 'test')`); err != nil {
		t.Fatal(err)
	}
	botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "", "100", nil)
	botEvent(t, s, ctx, 101, "2026-09-10T11:00:00+03:00", KindPurchase, "", "30", "OddShop", "70", nil)
	for i := 0; i < 3; i++ {
		process(t, s, ctx, "2026-09-10T12:00:00+03:00")
	}
	p, c := proposal(t, s, "card:101")
	if p.Version != 1 || p.Status != "open" || p.Block != "likely" || !strings.Contains(c.Hint, "не проходит") {
		t.Fatalf("no new version each pass: %+v %+v", p, c)
	}
	if n := count(t, s, `SELECT count(*) FROM documents WHERE category = 'Нет такой категории'`); n != 0 {
		t.Fatal(n)
	}
}

func TestRepeatedDepositMessageIsOneDeposit(t *testing.T) {
	s, ctx := open(t, "shadow")
	addWithdrawal(t, s, ctx, "1", "100", "1", "TcincinOne", "2026-09-10T10:00:00+03:00")
	deposit(t, s, ctx, 200, "2026-09-10T10:30:00+03:00", "100", "3", "97")
	botEvent(t, s, ctx, 201, "2026-09-10T11:45:00+03:00", KindDeposit, "", "100", "", "", map[string]string{"zayavka": "599-200", "fee": "3", "net": "97", "status": "Проведена"})
	rep := process(t, s, ctx, "2026-09-10T12:00:00+03:00")
	if rep.DepositsLinked != 1 || rep.Ignored != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, ok, err := brief.LatestProposal(ctx, s.RW, "card:201"); err != nil || ok {
		t.Fatal("asked about a repeat", err)
	}
}

func TestPurchaseProposals(t *testing.T) {
	s, ctx := open(t, "shadow")
	botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "", "100", nil)
	first := botEvent(t, s, ctx, 101, "2026-09-10T11:00:00+03:00", KindPurchase, "", "30", "ServiceA* Pro", "70", nil)
	botEvent(t, s, ctx, 102, "2026-09-10T12:00:00+03:00", KindPurchase, "", "20", "Незнакомый {магазин}|", "50", nil)
	botEvent(t, s, ctx, 103, "2026-09-10T13:00:00+03:00", KindPurchase, "", "1", "ServiceA* Pro", "9000", nil)
	botEvent(t, s, ctx, 104, "2026-09-10T14:00:00+03:00", KindPurchase, "", "0.03", "mailer", "49.97", nil)
	process(t, s, ctx, "2026-09-10T15:00:00+03:00")

	p, c := proposal(t, s, "card:101")
	if p.Block != "auto" || p.Kind != KindCardPurchase || len(c.Documents) != 1 || !c.PersonalDraw {
		t.Fatalf("%+v %+v", p, c)
	}
	if d := c.Documents[0]; d.Category != "Инструменты" || d.Method != "Карта CinCin 1111" || d.Amount != "30.00" || d.Counterparty == "" {
		t.Fatalf("%+v", d)
	}
	if !strings.Contains(p.Summary, "купил с карты 1111 на 30 USD") || c.Money == nil || c.Money.Account != "card:1111" || c.Money.Amount != "-30.00" {
		t.Fatalf("%s %+v", p.Summary, c.Money)
	}
	// `ledger health` counts open purchases as already off the card.
	if n := count(t, s, `SELECT count(*) FROM proposals WHERE key = 'card:101' AND json_extract(content, '$.card') = '1111' AND CAST(json_extract(content, '$.amount') AS REAL) = 30`); n != 1 {
		t.Fatalf("pending fields %d", n)
	}

	p, c = proposal(t, s, "card:102")
	if p.Block != "unknown" || len(c.Documents) != 0 || len(c.Skeleton) != 1 || !strings.Contains(p.Summary, "категория? мерчант не в мапе") {
		t.Fatalf("%+v %+v", p, c)
	}
	if strings.ContainsAny(c.Skeleton[0].Note, "{}|") {
		t.Fatalf("a merchant must not break the row: %s", c.Skeleton[0].Note)
	}

	p, c = proposal(t, s, "card:103")
	if p.Block != "unknown" || !strings.Contains(p.Summary, "с карты ?") || !strings.Contains(c.Hint, "не назвал карту") {
		t.Fatalf("a purchase without a card is not exact: %+v %+v", p, c)
	}
	if n := count(t, s, `SELECT count(*) FROM observations WHERE event_id = (SELECT id FROM events WHERE source_id = '103')`); n != 0 {
		t.Fatal("anchor of an unknown card")
	}

	// A merchant checking the card is a fee in the journal's words.
	p, c = proposal(t, s, "card:104")
	if p.Block != "auto" || c.Documents[0].Category != "Комиссии" {
		t.Fatalf("%+v %+v", p, c)
	}

	// A journal row with the key: the purchase links without a question, the proposal closes.
	if _, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 1, Date: "2026-09-10", Type: "expense", Category: "Инструменты", Counterparty: "Сервис", Model: "—",
		Amount: "30.00", Currency: "USD", Method: "Карта CinCin 1111", Status: "paid", Note: "card:101; подписка", Author: "test", Origin: "test"}, at("2026-09-10")); err != nil {
		t.Fatal(err)
	}
	process(t, s, ctx, "2026-09-10T16:00:00+03:00")
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND how = 'note_key' AND document_num = 1`, first); n != 1 {
		t.Fatalf("links %d", n)
	}
	p, _ = proposal(t, s, "card:101")
	if p.Status != "superseded" || p.Decision != "recorded" {
		t.Fatalf("%+v", p)
	}
	// A row written before the event arrives leaves nothing to ask.
	if _, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 2, Date: "2026-09-10", Type: "expense", Category: "Инструменты", Counterparty: "Сервис", Model: "—",
		Amount: "5.00", Currency: "USD", Method: "Карта CinCin 1111", Status: "paid", Note: "card:105", Author: "test", Origin: "test"}, at("2026-09-10")); err != nil {
		t.Fatal(err)
	}
	botEvent(t, s, ctx, 105, "2026-09-10T17:00:00+03:00", KindPurchase, "", "5", "ServiceA", "44.97", nil)
	process(t, s, ctx, "2026-09-10T18:00:00+03:00")
	if _, ok, err := brief.LatestProposal(ctx, s.RW, "card:105"); err != nil || ok {
		t.Fatal("asked about a recorded purchase", err)
	}
}

func TestDuplicateNotificationAsksNothing(t *testing.T) {
	s, ctx := open(t, "shadow")
	botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "", "100", nil)
	botEvent(t, s, ctx, 101, "2026-09-10T11:00:00+03:00", KindPurchase, "", "30", "ServiceA", "70", nil)
	dup := botEvent(t, s, ctx, 102, "2026-09-10T11:00:03+03:00", KindPurchase, "", "30", "ServiceA", "70", nil)
	botEvent(t, s, ctx, 103, "2026-09-10T12:00:00+03:00", KindLoad, "1111", "10", "", "80", nil)
	botEvent(t, s, ctx, 104, "2026-09-10T12:00:02+03:00", KindLoad, "1111", "10", "", "80", nil)
	rep := process(t, s, ctx, "2026-09-10T13:00:00+03:00")
	if rep.LoadsPosted != 2 || rep.Ignored != 2 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND decision = 'ignore'`, dup); n != 1 {
		t.Fatalf("links %d", n)
	}
	for _, key := range []string{"card:102", "card:102:delta", "card:104"} {
		if _, ok, err := brief.LatestProposal(ctx, s.RW, key); err != nil || ok {
			t.Fatalf("%s asks: %v", key, err)
		}
	}
}

func TestDeltaProposalFollowsTheChain(t *testing.T) {
	s, ctx := open(t, "shadow")
	botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "", "100", nil)
	botEvent(t, s, ctx, 102, "2026-09-10T12:00:00+03:00", KindPurchase, "", "30", "ServiceA", "68", nil)
	process(t, s, ctx, "2026-09-10T13:00:00+03:00")
	p, c := proposal(t, s, "card:102:delta")
	if p.Kind != KindCardDelta || p.Block != "likely" || len(c.Documents) != 1 || c.Part != "delta" {
		t.Fatalf("%+v %+v", p, c)
	}
	d := c.Documents[0]
	if d.Category != "Комиссии" || d.Amount != "2.00" || d.Method != "Карта CinCin 1111" || !strings.Contains(d.Note, "card:102:delta") || !strings.Contains(d.Note, "мерчант не подтверждён") {
		t.Fatalf("%+v", d)
	}
	// The message that explains it arrives late: the chain closes and the question goes.
	botEvent(t, s, ctx, 101, "2026-09-10T11:00:00+03:00", KindPurchase, "", "2", "ServiceA", "98", nil)
	process(t, s, ctx, "2026-09-10T14:00:00+03:00")
	p, _ = proposal(t, s, "card:102:delta")
	if p.Status != "superseded" || p.Decision != brief.DecisionChain {
		t.Fatalf("%+v", p)
	}
	// The balance rose past the chain: only a question, nothing to confirm.
	botEvent(t, s, ctx, 103, "2026-09-10T15:00:00+03:00", KindPurchase, "", "3", "ServiceA", "70", nil)
	process(t, s, ctx, "2026-09-10T16:00:00+03:00")
	p, c = proposal(t, s, "card:103:delta")
	if p.Block != "unknown" || len(c.Documents) != 0 || len(c.Skeleton) != 1 || c.Skeleton[0].Type != "income" || !strings.Contains(p.Summary, "вырос на 5") {
		t.Fatalf("%+v %+v", p, c)
	}
}

func TestImportedEventsClosedThereAskNothing(t *testing.T) {
	s, ctx := open(t, "shadow")
	load := imported(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "100", "closed", "", "", "")
	imported(t, s, ctx, 101, "2026-09-10T11:00:00+03:00", KindPurchase, "", "30", "68", "closed", "2", "closed", "")
	imported(t, s, ctx, 102, "2026-09-10T12:00:00+03:00", KindPurchase, "", "1", "66", "closed", "", "", "")
	imported(t, s, ctx, 103, "2026-09-10T13:00:00+03:00", KindDepositPending, "", "100", "", "skipped", "", "", "")
	imported(t, s, ctx, 104, "2026-09-10T14:00:00+03:00", KindDeposit, "", "100", "", "closed", "", "", "")
	imported(t, s, ctx, 105, "2026-09-10T14:30:00+03:00", KindWalletAnchor, "", "5", "5", "skipped", "", "", "")
	rep := process(t, s, ctx, "2026-09-11T10:00:00+03:00")
	if rep.Proposed != 0 || rep.LoadsPosted != 1 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM proposals`); n != 0 {
		t.Fatalf("proposals %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM observations WHERE kind = 'wallet_anchor' AND subject = 'cincin_wallet' AND value = 5000000`); n != 1 {
		t.Fatalf("wallet anchor %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM entries e JOIN entry_events x ON x.entry_id = e.id WHERE x.event_id = ? AND e.kind = 'card:load'`, load); n != 1 {
		t.Fatalf("load %d", n)
	}
	// What ledger-sync left open goes the normal way, hidden charge included.
	imported(t, s, ctx, 106, "2026-09-10T15:00:00+03:00", KindPurchase, "", "10", "55", "new", "", "", "")
	process(t, s, ctx, "2026-09-11T11:00:00+03:00")
	if p, _ := proposal(t, s, "card:106"); p.Status != "open" {
		t.Fatalf("%+v", p)
	}
	if p, _ := proposal(t, s, "card:106:delta"); p.Status != "open" {
		t.Fatalf("%+v", p)
	}
}

func addWithdrawal(t *testing.T, s *db.Store, ctx context.Context, id, amount, fee, address, when string) int64 {
	t.Helper()
	w := at(when)
	m := bybit.Movement{Key: "withdrawal:" + id, Kind: "withdrawal", Direction: "expense", TimeMs: w.UnixMilli(), At: ts.FormatMilli(w), Date: ts.Date(w), Coin: "USDT",
		Amount: amount, Fee: fee, CounterpartyKind: "address", Counterparty: address, Method: "USDT TRC-20"}
	eid, _, err := ledger.AddEvent(ctx, s.RW, m.Event(), w)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := bybit.Mode(ctx, s.RW)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bybit.ClassifyEvents(ctx, tx, mode, at("2026-08-01"), w); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return eid
}

func deposit(t *testing.T, s *db.Store, ctx context.Context, msg int64, when, gross, fee, net string) int64 {
	t.Helper()
	return botEvent(t, s, ctx, msg, when, KindDeposit, "", gross, "", "", map[string]string{"zayavka": fmt.Sprintf("599-%d", msg), "fee": fee, "net": net, "status": "Проведена"})
}

func TestDepositLinksToItsOnlyWithdrawal(t *testing.T) {
	s, ctx := open(t, "shadow")
	w := addWithdrawal(t, s, ctx, "1", "100", "1", "TcincinOne", "2026-09-10T10:00:00+03:00")
	p, c := proposal(t, s, "withdrawal:1")
	if p.Block != "auto" || !c.AwaitDeposit || c.Documents[0].Counterparty != bybit.CinCinRoute {
		t.Fatalf("a top-up waits for what reached the wallet: %+v %+v", p, c)
	}
	d := deposit(t, s, ctx, 200, "2026-09-10T10:30:00+03:00", "100", "3", "97")
	rep := process(t, s, ctx, "2026-09-10T11:00:00+03:00")
	if rep.DepositsLinked != 1 || rep.Versions != 1 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM deposit_links WHERE deposit_event_id = ? AND withdrawal_event_id = ? AND how = 'auto'`, d, w); n != 1 {
		t.Fatalf("links %d", n)
	}
	if _, ok, err := brief.LatestProposal(ctx, s.RW, "card:200"); err != nil || ok {
		t.Fatal("a tied deposit asks nothing", err)
	}
	p, c = proposal(t, s, "withdrawal:1")
	if p.Version != 2 || p.Status != "open" || c.AwaitDeposit || len(c.Documents) != 2 || fmt.Sprint(c.AlsoEvents) != fmt.Sprint([]int64{d}) {
		t.Fatalf("%+v %+v", p, c)
	}
	if main := c.Documents[0]; main.Amount != "97.00" || main.Type != "transfer" || main.Counterparty != bybit.CinCinRoute || main.Currency != "USD" ||
		!strings.Contains(main.Note, "withdrawal:1") || !strings.Contains(main.Note, "card:200") {
		t.Fatalf("%+v", main)
	}
	if fee := c.Documents[1]; fee.Amount != "4.00" || fee.Category != "Комиссии" || fee.Counterparty != "CinCin" {
		t.Fatalf("%+v", fee)
	}
	// Repeat: nothing changes.
	if rep := process(t, s, ctx, "2026-09-10T12:00:00+03:00"); rep.DepositsLinked != 0 || rep.Versions != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestDepositAmbiguityIsAQuestion(t *testing.T) {
	s, ctx := open(t, "shadow")
	// Two withdrawals fit one deposit.
	addWithdrawal(t, s, ctx, "1", "100", "1", "TcincinOne", "2026-09-10T10:00:00+03:00")
	addWithdrawal(t, s, ctx, "2", "100", "1", "TcincinTwo", "2026-09-10T10:10:00+03:00")
	deposit(t, s, ctx, 200, "2026-09-10T10:30:00+03:00", "100", "3", "97")
	// Two deposits fit one withdrawal.
	addWithdrawal(t, s, ctx, "3", "50", "1", "TcincinOne", "2026-09-11T10:00:00+03:00")
	deposit(t, s, ctx, 201, "2026-09-11T10:30:00+03:00", "50", "3", "47")
	deposit(t, s, ctx, 202, "2026-09-11T10:31:00+03:00", "50", "3", "47")
	// Nothing fits: too far, another address, another amount.
	addWithdrawal(t, s, ctx, "4", "70", "1", "TcincinOne", "2026-09-12T01:00:00+03:00")
	addWithdrawal(t, s, ctx, "5", "70", "1", "TstrangerAddress", "2026-09-12T10:00:00+03:00")
	addWithdrawal(t, s, ctx, "6", "71", "1", "TcincinOne", "2026-09-12T10:00:00+03:00")
	deposit(t, s, ctx, 203, "2026-09-12T10:30:00+03:00", "70", "3", "67")
	rep := process(t, s, ctx, "2026-09-13T10:00:00+03:00")
	if rep.DepositsLinked != 0 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM deposit_links`); n != 0 {
		t.Fatalf("links %d", n)
	}
	p, c := proposal(t, s, "card:200")
	if p.Kind != KindCardDeposit || p.Block != "unknown" || !strings.Contains(p.Summary, "не определяется однозначно (withdrawal:1, withdrawal:2)") || len(c.Documents) != 0 {
		t.Fatalf("%+v %+v", p, c)
	}
	p, c = proposal(t, s, "card:201")
	if !strings.Contains(c.Hint, "претендуют") || !strings.Contains(p.Summary, "withdrawal:3") {
		t.Fatalf("%+v %+v", p, c)
	}
	proposal(t, s, "card:202")
	p, _ = proposal(t, s, "card:203")
	if !strings.Contains(p.Summary, "вывода Bybit рядом не вижу") {
		t.Fatalf("%+v", p)
	}
	// The agent has evidence: a manual link closes the question, and the checks are about state only.
	var r *brief.Refusal
	for _, c := range []struct {
		msg int64
		key string
	}{{999, "withdrawal:1"}, {200, "withdrawal:999"}} {
		if _, err := LinkDeposit(ctx, s.RW, c.msg, c.key, at("2026-09-13T11:00:00+03:00")); !errors.As(err, &r) {
			t.Fatal(c, err)
		}
	}
	if _, err := LinkDeposit(ctx, s.RW, 200, "withdrawal:2", at("2026-09-13T11:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if _, err := LinkDeposit(ctx, s.RW, 200, "withdrawal:1", at("2026-09-13T11:00:00+03:00")); !errors.As(err, &r) {
		t.Fatal("tied already", err)
	}
	if _, err := LinkDeposit(ctx, s.RW, 201, "withdrawal:2", at("2026-09-13T11:00:00+03:00")); !errors.As(err, &r) {
		t.Fatal("withdrawal taken", err)
	}
	p, _ = proposal(t, s, "card:200")
	if p.Status != "superseded" || p.Decision != "linked" {
		t.Fatalf("%+v", p)
	}
	p, c = proposal(t, s, "withdrawal:2")
	if len(c.Documents) != 2 || c.Documents[0].Amount != "97.00" {
		t.Fatalf("%+v %+v", p, c)
	}
	// With one of the two withdrawals taken, the other deposit question would now have one candidate.
	if p, _ := proposal(t, s, "withdrawal:1"); p.Status != "open" {
		t.Fatalf("%+v", p)
	}
}

func TestImportedDepositKeepsItsLink(t *testing.T) {
	s, ctx := open(t, "shadow")
	w := addWithdrawal(t, s, ctx, "1", "100", "1", "TcincinOne", "2026-09-10T10:00:00+03:00")
	d := imported(t, s, ctx, 200, "2026-09-10T10:30:00+03:00", KindDeposit, "", "100", "", "closed", "", "", "withdrawal:1")
	rep := process(t, s, ctx, "2026-09-11T10:00:00+03:00")
	if rep.DepositsLinked != 1 {
		t.Fatalf("%+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM deposit_links WHERE deposit_event_id = ? AND withdrawal_event_id = ? AND how = 'import'`, d, w); n != 1 {
		t.Fatalf("links %d", n)
	}
	// The withdrawal took its journal rows elsewhere: the deposit points at them too.
	if _, err := bybit.AddLink(ctx, s.RW, bybit.Link{EventID: w, DocumentNum: 7, How: "mark", Decision: "confirm", BriefRef: "ledger-sync:1/1"}, at("2026-09-11")); err != nil {
		t.Fatal(err)
	}
	process(t, s, ctx, "2026-09-11T11:00:00+03:00")
	if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND document_num = 7 AND how = 'rule'`, d); n != 1 {
		t.Fatalf("links %d", n)
	}
}

func TestLiveWritesWhatIsExact(t *testing.T) {
	s, ctx := open(t, "live")
	botEvent(t, s, ctx, 100, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "", "100", nil)
	botEvent(t, s, ctx, 101, "2026-09-10T11:00:00+03:00", KindPurchase, "", "30", "ServiceA", "70", nil)
	botEvent(t, s, ctx, 102, "2026-09-10T12:00:00+03:00", KindPurchase, "", "10", "Незнакомый", "60", nil)
	rep := process(t, s, ctx, "2026-09-10T13:00:00+03:00")
	if rep.Applied != 2 {
		t.Fatalf("%+v", rep)
	}
	feeRow, err := ledger.CurrentDocument(ctx, s.RW, 1)
	if err != nil || feeRow.Category != "Комиссии" || feeRow.Amount != "5.26" {
		t.Fatalf("%+v %v", feeRow, err)
	}
	buy, err := ledger.CurrentDocument(ctx, s.RW, 2)
	if err != nil || buy.Category != "Инструменты" || buy.Method != "Карта CinCin 1111" {
		t.Fatalf("%+v %v", buy, err)
	}
	card, _ := ledger.Balance(ctx, s.RW, "card:1111", "", "")
	wallet, _ := ledger.Balance(ctx, s.RW, "cincin_wallet", "", "")
	if card != money.MustParse("70") || wallet != money.MustParse("-105.26") {
		t.Fatal(card, wallet)
	}
	for key, want := range map[string]string{"card:100": "auto", "card:101": "auto", "card:102": "open"} {
		if p, _ := proposal(t, s, key); p.Status != want {
			t.Fatalf("%s: %s", key, p.Status)
		}
	}
	// The owner names the unknown merchant: the row closes the money on the card.
	b, err := brief.Make(ctx, s.RW, brief.Options{}, at("2026-09-11T00:30:00+03:00"))
	if err != nil || b == nil {
		t.Fatal(b, err)
	}
	var num int
	for _, it := range b.Items {
		if it.Key == "card:102" {
			num = it.Num
			if it.Block != "cards" {
				t.Fatalf("%+v", it)
			}
		} else if it.Block != "auto" {
			t.Fatalf("%+v", it)
		}
	}
	bad := brief.Decision{Num: num, Kind: brief.Custom, Text: `{"category":"Прочее","amount":"11"}`}
	if _, err := brief.Decide(ctx, s.RW, b.ID, []brief.Decision{bad}, "owner", at("2026-09-11T01:00:00+03:00")); err == nil || !strings.Contains(err.Error(), "двигают") {
		t.Fatal(err)
	}
	good := brief.Decision{Num: num, Kind: brief.Custom, Text: `{"category":"Прочее","counterparty":"Магазин"}`}
	if _, err := brief.Decide(ctx, s.RW, b.ID, []brief.Decision{good}, "owner", at("2026-09-11T01:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	card, _ = ledger.Balance(ctx, s.RW, "card:1111", "", "")
	if card != money.MustParse("60") {
		t.Fatal(card)
	}
	rows, err := cardsBalances(ctx, s)
	if err != nil || rows["card:1111"].Difference != "0.00" {
		t.Fatalf("%+v %v", rows, err)
	}
}

func cardsBalances(ctx context.Context, s *db.Store) (map[string]Balance, error) {
	list, err := Balances(ctx, s.RO)
	out := map[string]Balance{}
	for _, b := range list {
		out[b.Account] = b
	}
	return out, err
}

func TestLiveTopUpIsWrittenWhenItsDepositArrives(t *testing.T) {
	s, ctx := open(t, "live")
	w := addWithdrawal(t, s, ctx, "1", "100", "1", "TcincinOne", "2026-09-10T10:00:00+03:00")
	if p, _ := proposal(t, s, "withdrawal:1"); p.Status != "open" {
		t.Fatalf("no deposit yet: %+v", p)
	}
	if n := count(t, s, `SELECT count(*) FROM documents`); n != 0 {
		t.Fatalf("documents %d", n)
	}
	b, err := brief.Make(ctx, s.RW, brief.Options{}, at("2026-09-10T10:05:00+03:00"))
	if err != nil || b.Items[0].Block != "likely" {
		t.Fatalf("not written, so not «записал сам»: %+v %v", b, err)
	}
	d := deposit(t, s, ctx, 200, "2026-09-10T10:30:00+03:00", "100", "3", "97")
	rep := process(t, s, ctx, "2026-09-10T11:00:00+03:00")
	if rep.DepositsLinked != 1 || rep.Applied != 1 {
		t.Fatalf("%+v", rep)
	}
	p, _ := proposal(t, s, "withdrawal:1")
	if p.Status != "auto" || p.Version != 2 {
		t.Fatalf("%+v", p)
	}
	bybitBal, _ := ledger.Balance(ctx, s.RW, "bybit", "", "")
	wallet, _ := ledger.Balance(ctx, s.RW, "cincin_wallet", "", "")
	if bybitBal != money.MustParse("-101") || wallet != money.MustParse("97") {
		t.Fatal(bybitBal, wallet)
	}
	for _, e := range []int64{w, d} {
		if n := count(t, s, `SELECT count(*) FROM event_links WHERE event_id = ? AND document_num = 1`, e); n == 0 {
			t.Fatalf("event %d is not on the row", e)
		}
	}
	// The old number of the withdrawal is refused: its version changed.
	if _, err := brief.Decide(ctx, s.RW, b.ID, []brief.Decision{{Num: 1, Kind: brief.Confirm}}, "owner", at("2026-09-10T12:00:00+03:00")); err == nil || !strings.Contains(err.Error(), "изменилось") {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM v_bybit_unlinked`); n != 0 {
		t.Fatalf("unlinked %d", n)
	}
	mirrorRows, err := ledger.DocumentsAsOf(ctx, s.RW, "")
	if err != nil || len(mirrorRows) != 2 {
		t.Fatal(mirrorRows, err)
	}
}

func TestCollectAndSpool(t *testing.T) {
	s, ctx := open(t, "shadow")
	dir := t.TempDir()
	path := filepath.Join(dir, "cards.json")
	example, err := os.ReadFile("testdata/cards.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, example, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Peer.String() != "1000000001" || cfg.Account != "synthetic-account" || strings.HasPrefix(cfg.TgDir, "~") {
		t.Fatalf("%+v %v", cfg, err)
	}
	messages := []Message{
		{ID: 1, Date: "2026-09-10T07:00:00+00:00", Text: "💳 Баланс карты **1111** пополнен на **50 USD**\nДоступный баланс: **50 USD**"},
		{ID: 2, Date: "2026-09-10T07:30:00+00:00", Out: true, Text: "Кошелёк"},
		{ID: 3, Date: "2026-09-10T08:00:00+00:00", Text: "Текущий баланс:\nUSDT: 1.50"},
		{ID: 4, Date: "2026-09-10T08:30:00+00:00", Text: "Главное меню"},
	}
	reads := 0
	var since, peer, account string
	read := func(_ context.Context, p, a, s string, limit int) ([]byte, error) {
		reads++
		since, peer, account = s, p, a
		return json.Marshal(History{Messages: messages})
	}
	state := filepath.Join(dir, "state.json")
	first := SinceFrom(state, cfg, at("2026-09-10T12:00:00+03:00"))
	if first != "2026-09-03" {
		t.Fatal(first)
	}
	got, err := Collect(ctx, cfg, read, first, 10)
	if err != nil || got.Messages != 4 || len(got.Events) != 2 || since != first || peer != "1000000001" || account != "synthetic-account" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := SaveState(state, got.Last); err != nil {
		t.Fatal(err)
	}
	if next := SinceFrom(state, cfg, at("2026-09-20T12:00:00+03:00")); next != "2026-09-09" {
		t.Fatal(next)
	}
	spool := events.Spool{Dir: filepath.Join(dir, "spool")}
	list := make([]events.Event, 0, len(got.Events))
	for _, p := range got.Events {
		e, err := p.Event()
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, e)
	}
	if err := spool.Put(list); err != nil {
		t.Fatal(err)
	}
	down := errors.New("API недоступен")
	rep, err := spool.Flush(ctx, func(context.Context, []events.Event) (events.Answer, error) { return events.Answer{}, down })
	if err != down || rep.Pending != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	send := func(ctx context.Context, batch []events.Event) (events.Answer, error) {
		return events.Accept(ctx, s, batch, at("2026-09-10T12:00:00+03:00"))
	}
	rep, err = spool.Flush(ctx, send)
	if err != nil || rep.Inserted != 2 || rep.Pending != 0 || len(rep.Settle) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	// On arrival: the load is posted and the anchors are in, without waiting for the job.
	if n := count(t, s, `SELECT count(*) FROM entries WHERE source_key = 'cards:load:1'`); n != 1 {
		t.Fatalf("load %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM observations`); n != 2 {
		t.Fatalf("observations %d", n)
	}
	// The API said yes and the answer was lost: the next run sends the same again, nothing doubles.
	if err := spool.Put(list); err != nil {
		t.Fatal(err)
	}
	if rep, err = spool.Flush(ctx, send); err != nil || rep.Inserted != 0 || rep.Repeated != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := count(t, s, `SELECT count(*) FROM entries WHERE kind = 'card:load'`); n != 1 {
		t.Fatalf("load %d", n)
	}
}

func TestCollectorAndImportDescribeOneMessage(t *testing.T) {
	s, ctx := open(t, "shadow")
	imported(t, s, ctx, 5, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "50", "50", "closed", "", "", "")
	p, err := ParseMessage(Message{ID: 5, Date: "2026-09-10T07:00:00+00:00", Text: "💳 Баланс карты **1111** пополнен на **50 USD**\nДоступный баланс: **50 USD**"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Event()
	if err != nil {
		t.Fatal(err)
	}
	ans, err := events.Accept(ctx, s, []events.Event{e}, at("2026-09-10T12:00:00+03:00"))
	if err != nil || !ans.OK || ans.Repeated != 1 || ans.Results[0].Conflict != "" {
		t.Fatalf("%+v %v", ans, err)
	}
	var payload string
	if err := s.RW.QueryRow(`SELECT payload FROM events WHERE source = ? AND source_id = '5'`, Source).Scan(&payload); err != nil || !strings.Contains(payload, "ledger_sync") {
		t.Fatal("the first record stays", err)
	}
	var contact string
	if err := s.RW.QueryRow(`SELECT value FROM cursors WHERE name = ?`, events.ContactCursor(Source)).Scan(&contact); err != nil || contact != "2026-09-10T12:00:00+03:00" {
		t.Fatalf("a repeat is a contact of the collector: %q %v", contact, err)
	}
}

// Two messages of one load: the books stand on the one posted first, whichever is earlier, and only once.
func TestLateRepeatOfALoadPostsNothing(t *testing.T) {
	s, ctx := open(t, "shadow")
	run := func() { t.Helper(); process(t, s, ctx, "2026-09-11T10:00:00+03:00") }
	botEvent(t, s, ctx, 501, "2026-09-10T10:00:05+03:00", KindLoad, "1111", "100", "", "100", nil)
	run()
	if got := accountBalance(t, s, "card:1111"); got != money.MustParse("100") {
		t.Fatalf("after the load: %s", got)
	}
	// The same notice, a moment earlier, read only now.
	botEvent(t, s, ctx, 500, "2026-09-10T10:00:01+03:00", KindLoad, "1111", "100", "", "100", nil)
	run()
	run()
	if got := accountBalance(t, s, "card:1111"); got != money.MustParse("100") {
		t.Fatalf("after the late repeat: %s", got)
	}
}

// An answer can take a load back and a later one can return it: the entry follows.
func TestLoadComesBackAfterAReversal(t *testing.T) {
	s, ctx := open(t, "shadow")
	now := time.Now()
	id := botEvent(t, s, ctx, 601, "2026-09-10T10:00:00+03:00", KindLoad, "1111", "100", "", "100", nil)
	run := func() { t.Helper(); process(t, s, ctx, "2026-09-11T10:00:00+03:00") }
	run()
	if _, err := bybit.AddLink(ctx, s.RW, bybit.Link{EventID: id, How: "mark", Decision: "personal"}, now); err != nil {
		t.Fatal(err)
	}
	run()
	if got := accountBalance(t, s, "card:1111"); got != 0 {
		t.Fatalf("reversed: %s", got)
	}
	if _, _, err := ledger.PutDocument(ctx, s.RW, ledger.Document{Num: 1, Date: "2026-09-10", Type: "expense", Category: "Комиссии", Counterparty: "CinCin", Model: "—",
		Amount: "5.26", Currency: "USD", Method: "Кошелёк CinCin", Status: "paid", Note: "card:601"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.Exec(`DELETE FROM event_links WHERE event_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := bybit.AddLink(ctx, s.RW, bybit.Link{EventID: id, DocumentNum: 1, How: "mark", Decision: "confirm"}, now); err != nil {
		t.Fatal(err)
	}
	run()
	if got := accountBalance(t, s, "card:1111"); got != money.MustParse("100") {
		t.Fatalf("back: %s", got)
	}
}

// A full page of tg history means there is more: the next page starts at its newest message.
func TestCollectReadsEveryPage(t *testing.T) {
	var froms []string
	all := make([]Message, 0, 7)
	for i := 0; i < 7; i++ {
		all = append(all, Message{ID: int64(i + 1), Date: fmt.Sprintf("2026-09-10T10:00:%02d+03:00", i), Text: "Текущий баланс: USDT: 1"})
	}
	read := func(_ context.Context, _, _, since string, limit int) ([]byte, error) {
		froms = append(froms, since)
		var page []Message
		for _, m := range all {
			if (since == "2026-09-10" || m.Date >= since) && len(page) < limit {
				page = append(page, m)
			}
		}
		return json.Marshal(History{Messages: page})
	}
	got, err := Collect(context.Background(), Config{Peer: "1", Account: "a"}, read, "2026-09-10", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages != 7 || len(got.Events) != 7 {
		t.Fatalf("messages %d, events %d, read from %v", got.Messages, len(got.Events), froms)
	}
}

func TestAnchorFromAScreen(t *testing.T) {
	s, ctx := open(t, "shadow")
	now := time.Now()
	if _, err := AddAnchor(ctx, s.RW, "1111", "12,5", "", "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := AddAnchor(ctx, s.RW, "wallet", "1.00", "", "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := AddAnchor(ctx, s.RW, "12", "1.00", "", "main", now); err == nil {
		t.Fatal("a wrong card must be refused")
	}
	var n int
	if err := s.RW.QueryRow(`SELECT count(*) FROM observations WHERE source = 'screen:main'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("anchors %d %v", n, err)
	}
}

func accountBalance(t *testing.T, s *db.Store, acct string) money.Micros {
	t.Helper()
	v, err := ledger.Balance(context.Background(), s.RW, acct, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
