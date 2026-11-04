package cards

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Proposal kinds and vocabulary of the journal for cards.
const (
	KindCardPurchase = "card_purchase"
	KindCardLoad     = "card_load"
	KindCardDelta    = "card_delta"
	KindCardDeposit  = "card_deposit"

	author         = "cards"
	feeCategory    = "Комиссии"
	refundCategory = "Возврат расхода"
	walletMethod   = "Кошелёк CinCin"
	counterparty   = "CinCin"
	walletAccount  = "cincin_wallet"

	settingLoadFrom = "cards_load_from"
	// DefaultLoadFrom is the day the journal stopped writing what reached the card as a transfer from
	// Bybit: from here on a load is a move of own money from the wallet, which only this package books.
	DefaultLoadFrom = "2026-09-05"

	linkWindow = 3 * time.Hour
)

// Report says what a pass did.
type Report struct {
	Events         int      `json:"events"`
	LoadsPosted    int      `json:"loads_posted"`
	LoadsReversed  int      `json:"loads_reversed"`
	Anchors        int      `json:"anchors"`
	Proposed       int      `json:"proposed"`
	Versions       int      `json:"new_versions"`
	Applied        int      `json:"applied_by_rule"`
	Ignored        int      `json:"ignored"`
	DepositsLinked int      `json:"deposits_linked"`
	Superseded     int      `json:"proposals_superseded"`
	Problems       []string `json:"problems,omitempty"`
}

// merchantRule is a `merchant` row of rules: a regular expression over the bot's merchant text.
type merchantRule struct {
	Name      string `json:"name"`
	Category  string `json:"category"`
	re        *regexp.Regexp
	validFrom string
	validTo   string
}

func loadMerchants(ctx context.Context, q ledger.Querier) ([]merchantRule, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, COALESCE(category, ''), COALESCE(match_merchant, ''), COALESCE(valid_from, ''), COALESCE(valid_to, '')
		FROM rules WHERE kind = 'merchant' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []merchantRule
	for rows.Next() {
		var r merchantRule
		var pattern string
		if err := rows.Scan(&r.Name, &r.Category, &pattern, &r.validFrom, &r.validTo); err != nil {
			return nil, err
		}
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil || pattern == "" || r.Category == "" {
			continue // ledger rules import warned about it: such a rule matches nothing
		}
		r.re = re
		out = append(out, r)
	}
	return out, rows.Err()
}

func merchantHint(rules []merchantRule, merchant, date string) *merchantRule {
	for i := range rules {
		r := &rules[i]
		if (r.validFrom == "" || r.validFrom <= date) && (r.validTo == "" || date < r.validTo) && r.re.MatchString(merchant) {
			return r
		}
	}
	return nil
}

func setting(ctx context.Context, q ledger.Querier, key, def string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows || (err == nil && strings.TrimSpace(v) == "") {
		return def, nil
	}
	return strings.TrimSpace(v), err
}

// LoadEvents reads the bot's events in time order. An event that does not read is a problem to report,
// not a reason to stop the rest of the chain.
func LoadEvents(ctx context.Context, q ledger.Querier) ([]Event, []string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, source_id, kind, COALESCE(occurred_at, received_at), payload FROM events
		WHERE source = ? ORDER BY COALESCE(occurred_at, received_at), CAST(source_id AS INTEGER)`, Source)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Event
	var problems []string
	for rows.Next() {
		var id int64
		var sid, kind, at, payload string
		if err := rows.Scan(&id, &sid, &kind, &at, &payload); err != nil {
			return nil, nil, err
		}
		e, err := decodeEvent(id, sid, kind, at, payload)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out = append(out, e)
	}
	return out, problems, rows.Err()
}

type linkKey struct {
	event int64
	part  string
}

func loadLinks(ctx context.Context, q ledger.Querier) (map[linkKey]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.event_id, l.part FROM event_links l JOIN events e ON e.id = l.event_id WHERE e.source = ?`, Source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[linkKey]bool{}
	for rows.Next() {
		var k linkKey
		if err := rows.Scan(&k.event, &k.part); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// closedThere says ledger-sync had closed the event (or its hidden charge) when it was imported, so
// it asks nothing here. A charge ledger-sync never asked about stays unasked too.
func closedThere(e Event, part string) bool {
	if e.Sync == nil {
		return false
	}
	if part == "delta" {
		return e.Sync.DeltaStatus == "closed" || (e.Sync.Delta == "" && e.Sync.Status != "new")
	}
	return e.Sync.Status == "closed" || e.Sync.Status == "skipped"
}

type pass struct {
	ctx      context.Context
	q        ledger.Querier
	now      time.Time
	mode     string
	loadFrom string
	rules    []merchantRule
	links    map[linkKey]bool
	rep      Report
}

func (p *pass) closed(e Event, part string) bool {
	return p.links[linkKey{e.ID, part}] || closedThere(e, part)
}

func when(e Event) string { return e.Time.In(ts.MSK).Format("02.01 15:04") }

func date(e Event) string { return ts.Date(e.Time) }

func cleanMerchant(s string) string {
	return strings.NewReplacer("{", "(", "}", ")", "|", "/").Replace(s)
}

func cardMethod(card string) string {
	if card == "" {
		return ""
	}
	return "Карта CinCin " + card
}

func cardAccount(card string) string {
	if card == "" {
		return "card:"
	}
	return "card:" + card
}

// cardContent is proposals.content of a bot's event.
type cardContent struct {
	Key          string                    `json:"key"`
	EventID      int64                     `json:"event_id"`
	Part         string                    `json:"part,omitempty"`
	Date         string                    `json:"date"`
	At           string                    `json:"at"`
	Card         string                    `json:"card,omitempty"`        // the card the money left: `ledger health` counts it as not yet on the books
	LoadedCard   string                    `json:"loaded_card,omitempty"` // a load: the fee leaves the wallet, the card only received
	Amount       string                    `json:"amount"`
	Merchant     string                    `json:"merchant,omitempty"`
	BalanceAfter string                    `json:"balance_after,omitempty"`
	Gross        string                    `json:"gross,omitempty"`
	Fee          string                    `json:"fee,omitempty"`
	Net          string                    `json:"net,omitempty"`
	Rule         *merchantRule             `json:"rule,omitempty"`
	Documents    []bybit.SuggestedDocument `json:"documents,omitempty"`
	Skeleton     []bybit.SuggestedDocument `json:"skeleton,omitempty"`
	PersonalDraw bool                      `json:"personal_draw,omitempty"`
	Money        *bybit.MoneyCheck         `json:"money,omitempty"`
	Candidates   []string                  `json:"candidates,omitempty"`
	Hint         string                    `json:"hint,omitempty"`
}

// propose keeps the proposal of an open event up to date and writes it in live mode when it is exact.
func (p *pass) propose(key, kind, block, summary string, c any) error {
	if block == "auto" {
		last, ok, err := brief.LatestProposal(p.ctx, p.q, key)
		if err != nil {
			return err
		}
		if ok && last.Status == "open" && last.AutoRefused() {
			return nil
		}
	}
	got, change, err := brief.Upsert(p.ctx, p.q, brief.Input{Key: key, Kind: kind, Block: block, Summary: summary, Author: author, Content: c}, p.now)
	if err != nil {
		return err
	}
	switch change {
	case brief.ChangeNew:
		p.rep.Proposed++
	case brief.ChangeVersion:
		p.rep.Versions++
	}
	if block == "auto" && p.mode == "live" && got.Status == "open" {
		applied, err := brief.ApplyAuto(p.ctx, p.q, got.ID, p.now)
		if err != nil {
			return err
		}
		if applied {
			p.rep.Applied++
		}
	}
	return nil
}

// dropProposal closes what an event no longer asks: the chain closed after a late message arrived.
func (p *pass) dropProposal(key, why string) error {
	last, ok, err := brief.LatestProposal(p.ctx, p.q, key)
	if err != nil || !ok || last.Status != "open" {
		return err
	}
	p.rep.Superseded++
	return brief.Supersede(p.ctx, p.q, last.ID, brief.DecisionChain, why, author, p.now)
}

func (p *pass) anchor(kind, subject string, value money.Micros, e Event) error {
	res, err := p.q.ExecContext(p.ctx, `INSERT INTO observations(kind, subject, value, observed_at, source, event_id)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(kind, subject, observed_at, source) DO NOTHING`, kind, subject, int64(value), e.At, Source, e.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		p.rep.Anchors++
	}
	return nil
}

func (p *pass) ignore(e Event, note string) error {
	if p.links[linkKey{e.ID, ""}] {
		return nil
	}
	if closedThere(e, "") {
		return nil
	}
	if _, err := bybit.AddLink(p.ctx, p.q, bybit.Link{EventID: e.ID, How: "rule", Decision: "ignore", Note: note}, p.now); err != nil {
		return err
	}
	p.links[linkKey{e.ID, ""}] = true
	p.rep.Ignored++
	return nil
}

// walletLoad says whether the event is money from the wallet. The bot prints a credit by support or a
// move between cards the same way as a load; the owner's answer tells them apart: a decision on the
// event that names no row paying from the wallet means it was not a load. Until someone says so, it is.
func (p *pass) walletLoad(e Event) (bool, error) {
	rows, err := p.q.QueryContext(p.ctx, `SELECT COALESCE(d.method, '') FROM event_links l
		LEFT JOIN v_documents d ON d.num = l.document_num WHERE l.event_id = ? AND l.part = '' AND l.how = 'mark'`, e.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	marked := false
	for rows.Next() {
		var method string
		if err := rows.Scan(&method); err != nil {
			return false, err
		}
		if method == walletMethod {
			return true, nil
		}
		marked = true
	}
	return !marked, rows.Err()
}

// bookLoad keeps the entry of a load in line with what is known about it: posted once for a wallet
// load, reversed when the owner's answer says it was none. Idempotent by key.
func (p *pass) bookLoad(e Event, card string, amount money.Micros) error {
	key := fmt.Sprintf("cards:load:%d", e.MsgID)
	wallet, err := p.walletLoad(e)
	if err != nil {
		return err
	}
	var id int64
	switch err := p.q.QueryRowContext(p.ctx, `SELECT id FROM entries WHERE source_key = ?`, key).Scan(&id); {
	case err == sql.ErrNoRows:
		if !wallet {
			return nil
		}
		if _, err := ledger.Post(p.ctx, p.q, ledger.Entry{EffectiveAt: e.At, Kind: "card:load", Memo: fmt.Sprintf("загрузка карты %s из кошелька, card:%d", card, e.MsgID),
			Author: author, SourceKey: key, EventIDs: []int64{e.ID}, Lines: []ledger.Line{
				{Account: "card:" + card, Amount: amount, Currency: "USD", Orig: e.Amount},
				{Account: walletAccount, Amount: -amount, Currency: "USD", Orig: e.Amount}}}, p.now); err != nil {
			return err
		}
		p.rep.LoadsPosted++
		return nil
	case err != nil:
		return err
	}
	if wallet {
		return nil
	}
	var reversed int
	err = p.q.QueryRowContext(p.ctx, `SELECT 1 FROM entries WHERE reverses_id = ?`, id).Scan(&reversed)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err := ledger.Reverse(p.ctx, p.q, id, ledger.Reversal{Author: author, Memo: fmt.Sprintf("card:%d не загрузка с кошелька: решение брифа", e.MsgID)}, p.now); err != nil {
		return err
	}
	p.rep.LoadsReversed++
	return nil
}

func (p *pass) load(s Step) error {
	amount, after, err := amounts(s.Event)
	if err != nil {
		return err
	}
	if err := p.anchor("card_anchor", "card:"+s.Card, after, s.Event); err != nil {
		return err
	}
	if date(s.Event) >= p.loadFrom {
		if err := p.bookLoad(s.Event, s.Card, amount); err != nil {
			return err
		}
	}
	key := fmt.Sprintf("card:%d", s.MsgID)
	if p.closed(s.Event, "") {
		return nil
	}
	fee, gross := loadFee(amount)
	note := fmt.Sprintf("%s; комиссия загрузки карты %s, уведомление бота %s MSK. На карту зачислено %s, остаток карты после загрузки %s; с кошелька расчётно %s (%s / 0.957), %s × 4.3%% = %s",
		key, s.Card, when(s.Event), amount, after, gross.Cents(), amount, gross.Cents(), fee.Cents())
	doc := bybit.SuggestedDocument{Date: date(s.Event), Type: "expense", Category: feeCategory, Counterparty: counterparty, Model: "—", Amount: fee.Cents(),
		Currency: "USD", Method: walletMethod, Status: "paid", Note: note}
	summary := fmt.Sprintf("загрузил карту %s на %s USD (зачислено на карту — наблюдение бота); с кошелька расчётно %s, 4.3%% с него = комиссия %s (%s)",
		s.Card, s.Amount, gross.Cents(), fee.Cents(), when(s.Event))
	c := cardContent{Key: key, EventID: s.ID, Date: date(s.Event), At: s.At, LoadedCard: s.Card, Amount: amount.String(), BalanceAfter: after.String(),
		Gross: gross.Cents(), Fee: fee.Cents(), Documents: []bybit.SuggestedDocument{doc}, Money: &bybit.MoneyCheck{Account: walletAccount, Amount: (-fee).Cents()}}
	// A chain that closes on the load is the tariff at work; otherwise it may be no load at all.
	block := "auto"
	if s.HasDelta {
		block = "likely"
		c.Hint = "цепочка остатка на этой загрузке не сходится: возможно, это зачисление саппорта или перенос между картами, а не загрузка с кошелька. Если так — personal: комиссии нет, проводка загрузки сторнируется"
	}
	return p.propose(key, KindCardLoad, block, summary, c)
}

func (p *pass) purchase(s Step) error {
	amount, after, err := amounts(s.Event)
	if err != nil {
		return err
	}
	if s.Card != "" {
		if err := p.anchor("card_anchor", "card:"+s.Card, after, s.Event); err != nil {
			return err
		}
	}
	if p.closed(s.Event, "") {
		return nil
	}
	key := fmt.Sprintf("card:%d", s.MsgID)
	merchant := cleanMerchant(s.Merchant)
	hint := merchantHint(p.rules, s.Merchant, date(s.Event))
	base := bybit.SuggestedDocument{Date: date(s.Event), Type: "expense", Counterparty: merchant, Model: "—", Amount: amount.String(), Currency: "USD", Method: cardMethod(s.Card), Status: "paid",
		Note: fmt.Sprintf("%s; %s, %s MSK, остаток карты %s", key, merchant, when(s.Event), after)}
	c := cardContent{Key: key, EventID: s.ID, Date: date(s.Event), At: s.At, Card: s.Card, Amount: amount.String(), Merchant: s.Merchant, BalanceAfter: after.String(),
		PersonalDraw: true, Money: &bybit.MoneyCheck{Account: cardAccount(s.Card), Amount: (-amount).String()}, Rule: hint}
	who, what := s.Merchant, "категория? мерчант не в мапе"
	block := "unknown"
	if hint != nil {
		who, what = hint.Name, hint.Category
		base.Category, base.Counterparty = hint.Category, hint.Name
		if amount == probeAmount { // a merchant checking the card: the journal calls it a fee
			base.Category = feeCategory
			what = feeCategory
			base.Note = fmt.Sprintf("%s; проверка карты, %s, %s MSK", key, merchant, when(s.Event))
		}
		if s.Card != "" {
			block = "auto"
			c.Documents = []bybit.SuggestedDocument{base}
		}
	}
	if block != "auto" {
		c.Skeleton = []bybit.SuggestedDocument{base}
	}
	card := s.Card
	if card == "" {
		card = "?"
		c.Hint = "бот не назвал карту, и остаток ни к одной не подошёл"
	}
	summary := fmt.Sprintf("купил с карты %s на %s USD — %s (%s) (%s)", card, s.Amount, who, what, when(s.Event))
	return p.propose(key, KindCardPurchase, block, summary, c)
}

// delta asks about the hidden charge of a step, or closes the question when the chain closed.
func (p *pass) delta(s Step) error {
	key := fmt.Sprintf("card:%d:delta", s.MsgID)
	if p.closed(s.Event, "delta") {
		return nil
	}
	if !s.HasDelta {
		return p.dropProposal(key, "цепочка остатка сошлась")
	}
	c := cardContent{Key: key, EventID: s.ID, Part: "delta", Date: date(s.Event), At: s.At, Card: s.Card, Amount: s.Delta.String()}
	doc := bybit.SuggestedDocument{Date: date(s.Event), Category: feeCategory, Counterparty: counterparty, Model: "—", Currency: "USD", Method: cardMethod(s.Card), Status: "paid",
		Note: fmt.Sprintf("%s; скрытое списание по цепочке остатка карты %s, %s MSK; мерчант не подтверждён", key, s.Card, when(s.Event))}
	if s.Delta > 0 {
		doc.Type, doc.Amount = "expense", s.Delta.String()
		c.Documents, c.PersonalDraw = []bybit.SuggestedDocument{doc}, true
		c.Money = &bybit.MoneyCheck{Account: "card:" + s.Card, Amount: (-s.Delta).String()}
		summary := fmt.Sprintf("скрытое списание %s USD по карте %s при событии выше — записать «Комиссии, мерчант не подтверждён»? (%s)", s.Delta, s.Card, when(s.Event))
		return p.propose(key, KindCardDelta, "likely", summary, c)
	}
	// The balance rose past what the chain expects: a refund or a message out of order. Only a question.
	up := -s.Delta
	doc.Type, doc.Category, doc.Amount = "income", refundCategory, up.String()
	doc.Note = fmt.Sprintf("%s; остаток карты %s вырос сверх цепочки, %s MSK", key, s.Card, when(s.Event))
	c.Skeleton = []bybit.SuggestedDocument{doc}
	c.Money = &bybit.MoneyCheck{Account: "card:" + s.Card, Amount: up.String()}
	summary := fmt.Sprintf("остаток карты %s вырос на %s USD сверх цепочки — возврат или порядок сообщений? (%s)", s.Card, up, when(s.Event))
	return p.propose(key, KindCardDelta, "unknown", summary, c)
}

// Process turns events into what they mean, each at most once. It runs on arrival and as a job, over
// the whole chain: a late message can change the deltas after it.
func Process(ctx context.Context, q ledger.Querier, now time.Time) (Report, error) {
	var rep Report
	if _, err := bybit.LinkNotes(ctx, q, now); err != nil {
		return rep, err
	}
	events, problems, err := LoadEvents(ctx, q)
	if err != nil {
		return rep, err
	}
	p := &pass{ctx: ctx, q: q, now: now}
	p.rep.Problems = problems
	if p.mode, err = bybit.Mode(ctx, q); err != nil {
		return rep, err
	}
	if p.loadFrom, err = setting(ctx, q, settingLoadFrom, DefaultLoadFrom); err != nil {
		return rep, err
	}
	if p.rules, err = loadMerchants(ctx, q); err != nil {
		return rep, err
	}
	if p.links, err = loadLinks(ctx, q); err != nil {
		return rep, err
	}
	steps, _ := Chain(events)
	p.rep.Events = len(steps)
	for _, s := range steps {
		if s.Problem != "" {
			p.rep.Problems = append(p.rep.Problems, s.Problem)
			continue
		}
		if s.DupOf != 0 {
			if err := p.ignore(s.Event, fmt.Sprintf("повтор уведомления бота, card:%d", s.DupOf)); err != nil {
				return p.rep, err
			}
			continue
		}
		switch s.Kind {
		case KindWalletAnchor:
			v, err := amountOf(s.Amount)
			if err != nil {
				p.rep.Problems = append(p.rep.Problems, fmt.Sprintf("событие %d: %v", s.MsgID, err))
				continue
			}
			if err := p.anchor("wallet_anchor", walletAccount, v, s.Event); err != nil {
				return p.rep, err
			}
			if err := p.ignore(s.Event, "анкор кошелька: денег не двигает"); err != nil {
				return p.rep, err
			}
		case KindDepositPending, KindDeclined:
			if err := p.ignore(s.Event, "служебное событие бота: денег не двигает"); err != nil {
				return p.rep, err
			}
		case KindLoad:
			if err := p.load(s); err != nil {
				return p.rep, err
			}
			if err := p.delta(s); err != nil {
				return p.rep, err
			}
		case KindPurchase:
			if err := p.purchase(s); err != nil {
				return p.rep, err
			}
			if s.Card != "" {
				if err := p.delta(s); err != nil {
					return p.rep, err
				}
			}
		}
	}
	if err := p.deposits(steps); err != nil {
		return p.rep, err
	}
	sweep, err := brief.Sweep(ctx, q, "job:cards", now)
	if err != nil {
		return p.rep, err
	}
	p.rep.Superseded += sweep.Superseded
	return p.rep, nil
}

// withdrawal is a Bybit withdrawal that may be the source of a deposit.
type withdrawal struct {
	id   int64
	m    bybit.Movement
	time time.Time
}

func loadWithdrawals(ctx context.Context, q ledger.Querier) ([]withdrawal, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, payload FROM events WHERE source = 'bybit' AND kind = 'withdrawal' ORDER BY occurred_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []withdrawal
	for rows.Next() {
		var w withdrawal
		var payload string
		if err := rows.Scan(&w.id, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &w.m); err != nil {
			return nil, fmt.Errorf("событие %d: %w", w.id, err)
		}
		w.time = time.UnixMilli(w.m.TimeMs)
		out = append(out, w)
	}
	return out, rows.Err()
}

// cincinAddresses are the deposit addresses of CinCin from the counterparty rules.
func cincinAddresses(ctx context.Context, q ledger.Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT match_address FROM rules WHERE kind = 'counterparty' AND lower(name) = 'cincin' AND match_address IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}

func loadDepositLinks(ctx context.Context, q ledger.Querier) (byDeposit map[int64]int64, taken map[int64]int64, err error) {
	rows, err := q.QueryContext(ctx, `SELECT deposit_event_id, withdrawal_event_id FROM deposit_links`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byDeposit, taken = map[int64]int64{}, map[int64]int64{}
	for rows.Next() {
		var d, w int64
		if err := rows.Scan(&d, &w); err != nil {
			return nil, nil, err
		}
		byDeposit[d], taken[w] = w, d
	}
	return byDeposit, taken, rows.Err()
}

func addDepositLink(ctx context.Context, q ledger.Querier, deposit, withdrawalID int64, how string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO deposit_links(deposit_event_id, withdrawal_event_id, how, created_at) VALUES (?, ?, ?, ?)`,
		deposit, withdrawalID, how, ts.Format(now))
	return err
}

// deposits ties each deposit to its Bybit withdrawal: same amount in USDT, a CinCin address, ±3 h and
// exactly one candidate, and one deposit per withdrawal. Anything else is a question, never a guess.
func (p *pass) deposits(steps []Step) error {
	byDeposit, taken, err := loadDepositLinks(p.ctx, p.q)
	if err != nil {
		return err
	}
	withdrawals, err := loadWithdrawals(p.ctx, p.q)
	if err != nil {
		return err
	}
	byKey := map[string]int64{}
	for _, w := range withdrawals {
		byKey[w.m.Key] = w.id
	}
	addrs, err := cincinAddresses(p.ctx, p.q)
	if err != nil {
		return err
	}
	type open struct {
		s     Step
		cands []withdrawal
	}
	var opened []*open
	claims := map[int64][]*open{}
	for _, s := range steps {
		if s.Kind != KindDeposit || s.DupOf != 0 || s.Problem != "" {
			continue
		}
		if _, linked := byDeposit[s.ID]; linked {
			continue
		}
		// ledger-sync already tied this deposit: its word stands.
		if s.Sync != nil && s.Sync.BybitKey != "" {
			if id := byKey[s.Sync.BybitKey]; id != 0 {
				if _, busy := taken[id]; !busy {
					if err := addDepositLink(p.ctx, p.q, s.ID, id, "import", p.now); err != nil {
						return err
					}
					byDeposit[s.ID], taken[id] = id, s.ID
					p.rep.DepositsLinked++
				}
				continue
			}
		}
		if p.closed(s.Event, "") {
			continue
		}
		gross, err := amountOf(s.Amount)
		if err != nil {
			p.rep.Problems = append(p.rep.Problems, fmt.Sprintf("депозит %d: %v", s.MsgID, err))
			continue
		}
		o := &open{s: s}
		for _, w := range withdrawals {
			if _, busy := taken[w.id]; busy || w.m.Coin != "USDT" || !addrs[w.m.Counterparty] {
				continue
			}
			v, err := bybit.AmountMicros(w.m.Amount)
			if err != nil || v != gross {
				continue
			}
			if d := s.Time.Sub(w.time); d <= linkWindow && d >= -linkWindow {
				o.cands = append(o.cands, w)
			}
		}
		opened = append(opened, o)
		if len(o.cands) == 1 {
			claims[o.cands[0].id] = append(claims[o.cands[0].id], o)
		}
	}
	for _, o := range opened {
		key := fmt.Sprintf("card:%d", o.s.MsgID)
		if len(o.cands) == 1 && len(claims[o.cands[0].id]) == 1 {
			if err := addDepositLink(p.ctx, p.q, o.s.ID, o.cands[0].id, "auto", p.now); err != nil {
				return err
			}
			byDeposit[o.s.ID], taken[o.cands[0].id] = o.cands[0].id, o.s.ID
			p.rep.DepositsLinked++
			if err := p.dropProposal(key, "депозит связан с "+o.cands[0].m.Key); err != nil {
				return err
			}
			continue
		}
		c := cardContent{Key: key, EventID: o.s.ID, Date: date(o.s.Event), At: o.s.At, Amount: o.s.Amount, Fee: o.s.Extra["fee"], Net: o.s.Extra["net"]}
		var summary string
		if len(o.cands) == 0 {
			summary = fmt.Sprintf("депозит в кошелёк CinCin %s USDT (зачислено %s) — вывода Bybit рядом не вижу, откуда? (%s)", o.s.Amount, o.s.Extra["net"], when(o.s.Event))
		} else {
			for _, w := range o.cands {
				c.Candidates = append(c.Candidates, w.m.Key)
			}
			if len(o.cands) == 1 {
				c.Hint = fmt.Sprintf("на тот же вывод претендуют ещё %d депозита", len(claims[o.cands[0].id])-1)
			}
			summary = fmt.Sprintf("депозит в кошелёк CinCin %s USDT — вывод Bybit не определяется однозначно (%s), какой? (%s)", o.s.Amount, strings.Join(c.Candidates, ", "), when(o.s.Event))
		}
		if err := p.propose(key, KindCardDeposit, "unknown", summary, c); err != nil {
			return err
		}
	}
	return p.syncLinked(steps, byDeposit)
}

// syncLinked follows every tied deposit: the withdrawal's open proposal learns what reached the wallet,
// and a deposit whose withdrawal already has journal rows points at them too.
func (p *pass) syncLinked(steps []Step, byDeposit map[int64]int64) error {
	for _, s := range steps {
		wid, ok := byDeposit[s.ID]
		if !ok {
			continue
		}
		var key, payload string
		if err := p.q.QueryRowContext(p.ctx, `SELECT source_id, payload FROM events WHERE id = ?`, wid).Scan(&key, &payload); err != nil {
			return err
		}
		var m bybit.Movement
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return fmt.Errorf("событие %d: %w", wid, err)
		}
		rows, err := linkedRows(p.ctx, p.q, wid)
		if err != nil {
			return err
		}
		if !p.links[linkKey{s.ID, ""}] {
			for _, n := range rows {
				if _, err := bybit.AddLink(p.ctx, p.q, bybit.Link{EventID: s.ID, DocumentNum: n, How: "rule", Decision: "recorded", Note: "депозит вывода " + key}, p.now); err != nil {
					return err
				}
				p.links[linkKey{s.ID, ""}] = true
			}
		}
		last, exists, err := brief.LatestProposal(p.ctx, p.q, key)
		if err != nil {
			return err
		}
		if !exists || last.Status != "open" {
			continue
		}
		var c bybit.Content
		if err := json.Unmarshal(last.Content, &c); err != nil {
			return fmt.Errorf("предложение %d: %w", last.ID, err)
		}
		docs, err := depositDocuments(m, s.Event)
		if err != nil {
			p.rep.Problems = append(p.rep.Problems, err.Error())
			continue
		}
		c.Documents, c.Skeleton, c.AwaitDeposit, c.AlsoEvents = docs, nil, false, []int64{s.ID}
		summary := fmt.Sprintf("пополнил кошелёк CinCin: с Bybit %s USDT, зачислено %s (%s)", m.Amount, s.Extra["net"], time.UnixMilli(m.TimeMs).In(ts.MSK).Format("02.01 15:04"))
		if err := p.propose(key, last.Kind, "auto", summary, c); err != nil {
			return err
		}
	}
	return nil
}

func linkedRows(ctx context.Context, q ledger.Querier, event int64) ([]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT document_num FROM event_links WHERE event_id = ? AND part = '' AND document_num IS NOT NULL ORDER BY document_num`, event)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// depositDocuments are the journal's two rows of a top-up: a transfer of what reached the wallet and
// the route's fees (network + deposit), both from Bybit.
func depositDocuments(m bybit.Movement, d Event) ([]bybit.SuggestedDocument, error) {
	net, err := amountOf(d.Extra["net"])
	if err != nil {
		return nil, fmt.Errorf("депозит %d: зачислено: %w", d.MsgID, err)
	}
	fee, err := amountOf(d.Extra["fee"])
	if err != nil {
		return nil, fmt.Errorf("депозит %d: комиссия: %w", d.MsgID, err)
	}
	network := money.Micros(0)
	if m.Fee != "" {
		if network, err = bybit.AmountMicros(m.Fee); err != nil {
			return nil, fmt.Errorf("%s: комиссия: %w", m.Key, err)
		}
	}
	card := fmt.Sprintf("card:%d", d.MsgID)
	return []bybit.SuggestedDocument{
		{Date: m.Date, Type: "transfer", Category: "Внутренний перевод", Counterparty: bybit.CinCinRoute, Model: "—", Amount: net.String(), Currency: "USD", Method: m.Method, Status: "paid",
			Note: fmt.Sprintf("%s; %s; пополнение кошелька CinCin (заявка %s): с Bybit ушло %s + %s комиссия сети, зачислено %s", m.Key, card, d.Extra["zayavka"], m.Amount, network, net)},
		{Date: m.Date, Type: "expense", Category: feeCategory, Counterparty: counterparty, Model: "—", Amount: (network + fee).String(), Currency: "USD", Method: m.Method, Status: "paid",
			Note: fmt.Sprintf("комиссии маршрута %s: %s сеть Bybit + %s депозит CinCin", m.Key, network, fee)},
	}, nil
}

// LinkDeposit ties a deposit to a withdrawal by hand, when the agent has evidence the rule cannot see.
// The checks are only about state: an event that is a deposit, a withdrawal not tied to another one.
func LinkDeposit(ctx context.Context, q ledger.Querier, msgID int64, withdrawalKey string, now time.Time) (Report, error) {
	var deposit int64
	var kind string
	err := q.QueryRowContext(ctx, `SELECT id, kind FROM events WHERE source = ? AND source_id = ?`, Source, strconv.FormatInt(msgID, 10)).Scan(&deposit, &kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != KindDeposit) {
		return Report{}, &brief.Refusal{Problems: []string{fmt.Sprintf("событие %d — не депозит CinCin", msgID)}}
	}
	if err != nil {
		return Report{}, err
	}
	var wid int64
	err = q.QueryRowContext(ctx, `SELECT id FROM events WHERE source = 'bybit' AND kind = 'withdrawal' AND source_id = ?`, withdrawalKey).Scan(&wid)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, &brief.Refusal{Problems: []string{fmt.Sprintf("вывода %s нет", withdrawalKey)}}
	}
	if err != nil {
		return Report{}, err
	}
	byDeposit, taken, err := loadDepositLinks(ctx, q)
	if err != nil {
		return Report{}, err
	}
	if _, busy := byDeposit[deposit]; busy {
		return Report{}, &brief.Refusal{Problems: []string{fmt.Sprintf("депозит %d уже связан", msgID)}}
	}
	if other, busy := taken[wid]; busy {
		return Report{}, &brief.Refusal{Problems: []string{fmt.Sprintf("вывод %s уже связан с другим депозитом (событие %d)", withdrawalKey, other)}}
	}
	if err := addDepositLink(ctx, q, deposit, wid, "manual", now); err != nil {
		return Report{}, err
	}
	last, ok, err := brief.LatestProposal(ctx, q, fmt.Sprintf("card:%d", msgID))
	if err != nil {
		return Report{}, err
	}
	if ok && last.Status == "open" {
		if err := brief.Supersede(ctx, q, last.ID, "linked", "связан вручную с "+withdrawalKey, author, now); err != nil {
			return Report{}, err
		}
	}
	return Process(ctx, q, now)
}

// Balance is a card on the books against what the bot printed last.
type Balance struct {
	Account    string `json:"account"`
	Ledger     string `json:"ledger"`
	Anchor     string `json:"anchor,omitempty"`
	AnchorAt   string `json:"anchor_at,omitempty"`
	Difference string `json:"difference,omitempty"`
}

// Balances compares every card and the wallet with their last anchors.
func Balances(ctx context.Context, q ledger.Querier) ([]Balance, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.code, COALESCE((SELECT SUM(l.amount) FROM lines l WHERE l.account = a.code), 0),
		(SELECT o.value FROM observations o WHERE o.subject = a.code AND o.kind IN ('card_anchor', 'wallet_anchor') ORDER BY o.observed_at DESC, o.id DESC LIMIT 1),
		(SELECT o.observed_at FROM observations o WHERE o.subject = a.code AND o.kind IN ('card_anchor', 'wallet_anchor') ORDER BY o.observed_at DESC, o.id DESC LIMIT 1)
		FROM accounts a WHERE a.code = ? OR a.code LIKE 'card:%' ORDER BY a.code`, walletAccount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Balance{}
	for rows.Next() {
		var b Balance
		var v int64
		var anchor sql.NullInt64
		var at sql.NullString
		if err := rows.Scan(&b.Account, &v, &anchor, &at); err != nil {
			return nil, err
		}
		b.Ledger = money.Micros(v).Cents()
		if anchor.Valid {
			b.Anchor, b.AnchorAt = money.Micros(anchor.Int64).Cents(), at.String
			b.Difference = (money.Micros(v) - money.Micros(anchor.Int64)).Cents()
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
