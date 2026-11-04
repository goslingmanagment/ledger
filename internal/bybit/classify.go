package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Proposal kinds of the collector.
const (
	KindMovement = "bybit"          // deposit, internal deposit, withdrawal
	KindP2P      = "bybit_p2p"      // the counterparty is a random exchange buyer: the owner decides salary or draw
	KindNonUSDT  = "bybit_non_usdt" // the journal keeps USDT; another coin needs the owner

	proposalAuthor = "bybit-collector"
)

// Journal vocabulary (finance rules): the fee row and the categories that are transfers, not P&L.
const (
	feeCategory      = "Комиссии"
	internalTransfer = "Внутренний перевод"

	// CinCinRoute is the journal's name of a top-up of the CinCin wallet from Bybit.
	CinCinRoute = "Bybit → CinCin (кошелёк)"
)

var transferCategories = map[string]bool{internalTransfer: true, "Остаток на старте": true}

// Rule is a row of `rules`: an exact identity with a direction, and what it means in the journal.
type Rule struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	UID       string `json:"uid,omitempty"`
	Address   string `json:"address,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Suffix    string `json:"suffix,omitempty"`
	Direction string `json:"direction,omitempty"`
	Slug      string `json:"slug,omitempty"`
	Name      string `json:"name"`
	Category  string `json:"category,omitempty"`
	Model     string `json:"model,omitempty"`
	Note      string `json:"note,omitempty"`
	validFrom string
	validTo   string
}

// LoadRules reads the counterparty and ignore rules in file order: the first match wins.
func LoadRules(ctx context.Context, q ledger.Querier) ([]Rule, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, kind, COALESCE(match_uid, ''), COALESCE(match_address, ''),
		COALESCE(match_prefix, ''), COALESCE(match_suffix, ''), COALESCE(direction, ''), COALESCE(slug, ''), name,
		COALESCE(category, ''), COALESCE(model, ''), note, COALESCE(valid_from, ''), COALESCE(valid_to, '')
		FROM rules WHERE kind IN ('counterparty', 'ignore') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.Kind, &r.UID, &r.Address, &r.Prefix, &r.Suffix, &r.Direction, &r.Slug, &r.Name,
			&r.Category, &r.Model, &r.Note, &r.validFrom, &r.validTo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (r Rule) validOn(date string) bool {
	return (r.validFrom == "" || r.validFrom <= date) && (r.validTo == "" || date < r.validTo)
}

// identityMatches: the uid or address is exactly the rule's (prefix + suffix for rotating addresses).
func (r Rule) identityMatches(m Movement) bool {
	switch {
	case r.UID != "":
		return m.CounterpartyKind == "uid" && m.Counterparty == r.UID
	case r.Address != "":
		return m.CounterpartyKind == "address" && m.Counterparty == r.Address
	case r.Prefix != "":
		return m.CounterpartyKind == "address" && strings.HasPrefix(m.Counterparty, r.Prefix) && strings.HasSuffix(m.Counterparty, r.Suffix)
	}
	return false
}

func (r Rule) matches(m Movement) bool {
	return (r.Direction == "" || r.Direction == "any" || r.Direction == m.Direction) && r.identityMatches(m)
}

// Verdict is how a movement is classified.
type Verdict struct {
	Ignored *Rule  // an ignore rule matched: no proposal, a link with decision ignore
	Kind    string // proposal kind
	Block   string // auto | likely | unknown
	Rule    *Rule  // the exact rule (auto) or the identity-only rule (likely)
	Hint    string // why likely
}

// Classify applies ignore rules, then the coin check, then counterparty rules. P2P never goes through
// rules: its counterparty is a stranger.
func Classify(m Movement, rules []Rule) Verdict {
	if m.Kind == "p2p" {
		return Verdict{Kind: KindP2P, Block: "unknown"}
	}
	for i := range rules {
		if rules[i].Kind == "ignore" && rules[i].validOn(m.Date) && rules[i].matches(m) {
			return Verdict{Ignored: &rules[i]}
		}
	}
	if m.Coin != "USDT" {
		return Verdict{Kind: KindNonUSDT, Block: "unknown"}
	}
	for i := range rules {
		if rules[i].Kind == "counterparty" && rules[i].validOn(m.Date) && rules[i].matches(m) {
			return Verdict{Kind: KindMovement, Block: "auto", Rule: &rules[i]}
		}
	}
	for i := range rules {
		if rules[i].Kind == "counterparty" && rules[i].validOn(m.Date) && rules[i].identityMatches(m) {
			hint := fmt.Sprintf("идентификатор совпадает с «%s», но направление %s — проверить вручную", rules[i].Name, m.Direction)
			return Verdict{Kind: KindMovement, Block: "likely", Rule: &rules[i], Hint: hint}
		}
	}
	return Verdict{Kind: KindMovement, Block: "unknown"}
}

// SuggestedDocument is a journal row the movement should become (the number is given when written).
type SuggestedDocument struct {
	Date         string `json:"date"`
	Type         string `json:"type"`
	Category     string `json:"category"`
	Counterparty string `json:"counterparty"`
	Model        string `json:"model"`
	Amount       string `json:"amount"`
	Currency     string `json:"currency"`
	Method       string `json:"method"`
	Status       string `json:"status"`
	Note         string `json:"note"`
}

// IsCinCin says the rule is the CinCin deposit address: such a withdrawal is a top-up of an own wallet.
func (r Rule) IsCinCin() bool { return strings.EqualFold(r.Name, "cincin") }

// Skeleton is what the movement says about its journal rows before anyone names the counterparty: the
// movement and, for a withdrawal, its network fee as a second row (finance rules: otherwise the cash
// drifts from Funding). Category and counterparty stay empty.
func Skeleton(m Movement) ([]SuggestedDocument, error) {
	amount, err := toMicros(m.Amount)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", m.Key, err)
	}
	docType := "income"
	if m.Direction == "expense" {
		docType = "expense"
	}
	docs := []SuggestedDocument{{Date: m.Date, Type: docType, Model: "—", Amount: amount.String(), Currency: "USDT",
		Method: m.Method, Status: "paid", Note: m.Key}}
	if m.Kind == "withdrawal" && m.Fee != "" {
		fee, err := toMicros(m.Fee)
		if err != nil {
			return nil, fmt.Errorf("%s: комиссия: %v", m.Key, err)
		}
		if fee > 0 {
			docs = append(docs, SuggestedDocument{Date: m.Date, Type: "expense", Category: feeCategory, Model: "—",
				Amount: fee.String(), Currency: "USDT", Method: m.Method, Status: "paid", Note: "комиссия сети " + m.Key})
		}
	}
	return docs, nil
}

// suggest builds the rows of an exact match on the movement's skeleton. A CinCin address means a
// top-up of the wallet: the journal names the route, and what reached the wallet comes with the deposit.
func suggest(m Movement, r Rule) ([]SuggestedDocument, error) {
	docs, err := Skeleton(m)
	if err != nil {
		return nil, err
	}
	model := r.Model
	if model == "" {
		model = "—"
	}
	for i := range docs {
		docs[i].Counterparty, docs[i].Model = r.Name, model
	}
	docs[0].Category = r.Category
	if transferCategories[r.Category] {
		docs[0].Type = "transfer"
	}
	if r.Category == internalTransfer && r.IsCinCin() {
		docs[0].Counterparty, docs[0].Currency = CinCinRoute, "USD"
		for i := 1; i < len(docs); i++ {
			docs[i].Currency = "USD"
		}
	}
	return docs, nil
}

// MoneyCheck is what a journal row of the proposal must do to a money account: an owner's correction
// may split or rename a row, not change what the money did. An account ending in ":" is a prefix.
type MoneyCheck struct {
	Account string `json:"account"`
	Amount  string `json:"amount"` // signed: + came in, − went out
}

// moneyOf is the Bybit side of a movement in USDT; other coins live under other accounts.
func moneyOf(m Movement) *MoneyCheck {
	if m.Coin != "USDT" {
		return nil
	}
	amount, err := toMicros(m.Amount)
	if err != nil {
		return nil
	}
	if m.Direction == "income" {
		return &MoneyCheck{Account: "bybit", Amount: amount.String()}
	}
	if m.Kind == "withdrawal" && m.Fee != "" {
		fee, err := toMicros(m.Fee)
		if err != nil {
			return nil
		}
		amount += fee
	}
	return &MoneyCheck{Account: "bybit", Amount: (-amount).String()}
}

// AmountMicros reads an amount as Bybit writes it, rounded to micro-dollars.
func AmountMicros(s string) (money.Micros, error) { return toMicros(s) }

// Sighting is an earlier movement with the same uid or address.
type Sighting struct {
	Key       string `json:"key"`
	Date      string `json:"date"`
	Amount    string `json:"amount"`
	Direction string `json:"direction"`
}

// priorSightings: the same counterparty earlier, newest first, at most five.
func priorSightings(ctx context.Context, q ledger.Querier, m Movement) ([]Sighting, error) {
	if m.Counterparty == "" {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT source_id, COALESCE(json_extract(payload, '$.date'), ''),
		COALESCE(json_extract(payload, '$.amount'), ''), COALESCE(json_extract(payload, '$.direction'), '')
		FROM events WHERE source = 'bybit' AND kind IN ('deposit', 'internal_deposit', 'withdrawal', 'p2p') AND source_id <> ?
		AND json_extract(payload, '$.counterparty_kind') = ? AND json_extract(payload, '$.counterparty') = ?
		ORDER BY occurred_at DESC, id DESC LIMIT 5`, m.Key, m.CounterpartyKind, m.Counterparty)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sighting
	for rows.Next() {
		var s Sighting
		if err := rows.Scan(&s.Key, &s.Date, &s.Amount, &s.Direction); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// amountPattern: «копейки» is an exchange-rate tail, the mark of card and service payments; «круглая»
// is a sum chosen by a person: whole, or ending in .5 or .50.
func amountPattern(amount string) string {
	_, frac, _ := strings.Cut(strings.TrimSpace(amount), ".")
	if t := strings.TrimRight(frac, "0"); t == "" || t == "5" {
		return "круглая"
	}
	return "копейки"
}

// Similar is a known counterparty that got exactly this amount in this direction.
type Similar struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func similarDocuments(ctx context.Context, q ledger.Querier, m Movement) ([]Similar, error) {
	amount, err := toMicros(m.Amount)
	if err != nil {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT counterparty, count(*) AS n FROM v_documents WHERE type = ? AND amount_micros = ?
		GROUP BY counterparty ORDER BY n DESC, counterparty LIMIT 3`, m.Direction, int64(amount))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Similar
	for rows.Next() {
		var s Similar
		if err := rows.Scan(&s.Name, &s.N); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SalaryContext is how much owner salary the journal holds for the month of a P2P deal: the number
// behind the owner's decision «salary or draw».
type SalaryContext struct {
	Month  string `json:"month"`
	Amount string `json:"amount"`
}

var salaryMoved = regexp.MustCompile(`\{за (\d{4}-\d{2}): ([\d.]+)\}`)

// ownerSalaryRecorded counts journal rows by their date; a tranche the owner moved to another month
// carries `{за ГГГГ-ММ: N}` and counts in that month. The cap itself lives in the finance rules.
func ownerSalaryRecorded(ctx context.Context, q ledger.Querier, month string) (SalaryContext, error) {
	res := SalaryContext{Month: month, Amount: "0"}
	rows, err := q.QueryContext(ctx, `SELECT date, amount, note FROM v_documents WHERE type = 'expense' AND category = 'Owner зарплата'`)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	paid := new(big.Rat)
	for rows.Next() {
		var date, amount, note string
		if err := rows.Scan(&date, &amount, &note); err != nil {
			return res, err
		}
		total, ok := rat(amount)
		if !ok {
			continue
		}
		moved, sum, bad := big.NewRat(0, 1), new(big.Rat), false
		for _, m := range salaryMoved.FindAllStringSubmatch(note, -1) {
			v, ok := rat(m[2])
			if !ok {
				bad = true
				break
			}
			sum.Add(sum, v)
			if m[1] == month {
				moved.Add(moved, v)
			}
		}
		if bad {
			continue
		}
		paid.Add(paid, moved)
		if strings.HasPrefix(date, month) {
			paid.Add(paid, total.Sub(total, sum))
		}
	}
	res.Amount = decString(paid)
	return res, rows.Err()
}

// Content is proposals.content of a Bybit movement.
type Content struct {
	Key                 string              `json:"key"`
	EventID             int64               `json:"event_id"`
	Mode                string              `json:"mode"`
	Date                string              `json:"date,omitempty"`
	Movement            Movement            `json:"movement"`
	Rule                *Rule               `json:"rule,omitempty"`
	Documents           []SuggestedDocument `json:"documents,omitempty"`
	Skeleton            []SuggestedDocument `json:"skeleton,omitempty"`      // the base of a custom answer when nothing is suggested
	PersonalDraw        bool                `json:"personal_draw,omitempty"` // «личное» from the business wallet is an Owner draw row
	AwaitDeposit        bool                `json:"await_deposit,omitempty"` // a CinCin top-up: written when its deposit tells what reached the wallet
	Money               *MoneyCheck         `json:"money,omitempty"`
	AlsoEvents          []int64             `json:"also_events,omitempty"` // other events the rows close: the CinCin deposit
	Hint                string              `json:"hint,omitempty"`
	PriorSightings      []Sighting          `json:"prior_sightings,omitempty"`
	AmountPattern       string              `json:"amount_pattern,omitempty"`
	SimilarDocuments    []Similar           `json:"similar_documents,omitempty"`
	OwnerSalaryRecorded *SalaryContext      `json:"owner_salary_recorded,omitempty"`
}

// summarize is one line for the brief.
func summarize(m Movement, v Verdict) string {
	when := time.UnixMilli(m.TimeMs).In(ts.MSK).Format("02.01 15:04")
	if m.Kind == "p2p" {
		verb := "купил"
		if m.Direction == "expense" {
			verb = "продал"
		}
		s := fmt.Sprintf("%s %s %s за %s", verb, short(m.Amount), m.Coin, m.Fiat)
		if m.Price != "" {
			s += " по " + m.Price
		}
		return s + " (" + when + ")"
	}
	verb := "пришло"
	if m.Direction == "expense" {
		verb = "заплатил"
	}
	s := fmt.Sprintf("%s %s %s", verb, short(m.Amount), m.Coin)
	if v.Block == "auto" && v.Rule != nil {
		s += " — " + v.Rule.Name
		if v.Rule.Category != "" {
			s += " (" + v.Rule.Category + ")"
		}
	}
	return s + " (" + when + ")"
}

// LiveHook writes the journal rows of an 'auto' proposal when settings.mode is live: the document and
// its entry in q's transaction, proposal status 'auto'. The switchover PR sets it. Until then ledger
// never creates documents, whatever the mode: in shadow the journal canon is elsewhere.
var LiveHook func(ctx context.Context, q ledger.Querier, proposalID int64, c Content, now time.Time) error

// ClassifyReport counts what one pass did.
type ClassifyReport struct {
	Auto    int `json:"auto"`
	Likely  int `json:"likely"`
	Unknown int `json:"unknown"`
	Ignored int `json:"ignored"`
}

// ClassifyEvents proposes every movement since start that has neither a proposal nor a link. One
// proposal per movement key: a repeat run finds nothing to do.
func ClassifyEvents(ctx context.Context, q ledger.Querier, mode string, start time.Time, now time.Time) (ClassifyReport, error) {
	var rep ClassifyReport
	rows, err := q.QueryContext(ctx, `SELECT e.id, e.payload FROM events e
		WHERE e.source = 'bybit' AND e.kind IN ('deposit', 'internal_deposit', 'withdrawal', 'p2p') AND e.occurred_at >= ?
		AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.key = e.source_id)
		AND NOT EXISTS (SELECT 1 FROM event_links l WHERE l.event_id = e.id AND l.part = '')
		ORDER BY e.occurred_at, e.id`, ts.Format(start))
	if err != nil {
		return rep, err
	}
	type item struct {
		id int64
		m  Movement
	}
	var items []item
	for rows.Next() {
		var it item
		var payload string
		if err := rows.Scan(&it.id, &payload); err != nil {
			rows.Close()
			return rep, err
		}
		if err := json.Unmarshal([]byte(payload), &it.m); err != nil {
			rows.Close()
			return rep, fmt.Errorf("event %d: %v", it.id, err)
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	if len(items) == 0 {
		return rep, nil
	}
	rules, err := LoadRules(ctx, q)
	if err != nil {
		return rep, err
	}
	for _, it := range items {
		v := Classify(it.m, rules)
		if v.Ignored != nil {
			note := fmt.Sprintf("правило #%d", v.Ignored.ID)
			if v.Ignored.Note != "" {
				note += ": " + v.Ignored.Note
			}
			if _, err := AddLink(ctx, q, Link{EventID: it.id, How: "rule", Decision: "ignore", Note: note}, now); err != nil {
				return rep, err
			}
			rep.Ignored++
			continue
		}
		if err := propose(ctx, q, it.id, it.m, v, mode, now); err != nil {
			return rep, err
		}
		switch v.Block {
		case "auto":
			rep.Auto++
		case "likely":
			rep.Likely++
		default:
			rep.Unknown++
		}
	}
	return rep, nil
}

func propose(ctx context.Context, q ledger.Querier, eventID int64, m Movement, v Verdict, mode string, now time.Time) error {
	bare := m
	bare.Raw = nil
	c := Content{Key: m.Key, EventID: eventID, Mode: mode, Date: m.Date, Movement: bare, Rule: v.Rule, Hint: v.Hint,
		PersonalDraw: m.Direction == "expense" && m.Coin == "USDT", Money: moneyOf(m)}
	var err error
	if v.Block != "auto" && m.Coin == "USDT" { // another coin: its dollars are for the owner to say
		if c.Skeleton, err = Skeleton(m); err != nil {
			return err
		}
	}
	switch {
	case v.Block == "auto":
		if c.Documents, err = suggest(m, *v.Rule); err != nil {
			return err
		}
		c.AwaitDeposit = v.Rule.Category == internalTransfer && v.Rule.IsCinCin()
	case v.Kind == KindP2P:
		s, err := ownerSalaryRecorded(ctx, q, m.Date[:min(7, len(m.Date))])
		if err != nil {
			return err
		}
		c.OwnerSalaryRecorded = &s
	case v.Kind == KindMovement:
		if c.PriorSightings, err = priorSightings(ctx, q, m); err != nil {
			return err
		}
		c.AmountPattern = amountPattern(m.Amount)
		if c.SimilarDocuments, err = similarDocuments(ctx, q, m); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx, `INSERT INTO proposals(key, kind, block, summary, content, status, created_at, author)
		VALUES (?, ?, ?, ?, ?, 'open', ?, ?)`, m.Key, v.Kind, v.Block, summarize(m, v), string(raw), ts.Format(now), proposalAuthor)
	if err != nil {
		return fmt.Errorf("proposal %s: %w", m.Key, err)
	}
	if v.Block == "auto" && mode == "live" && LiveHook != nil {
		id, _ := res.LastInsertId()
		return LiveHook(ctx, q, id, c, now)
	}
	return nil
}
