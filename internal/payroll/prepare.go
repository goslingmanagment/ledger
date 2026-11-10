package payroll

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/payouts/prepare is step 1 of a payout to one person (release 2, L5; the owner's chatter-payout
// skill): what the agency owes now, for which days, which week blocks that closes, what to look at before
// paying, and the text to the person. It writes nothing, sends nothing and moves no money: the owner pays,
// and the journal row of the transfer closes the debt like any payout.

// ErrNoPayoutPerson: the code names nobody with a payout tab.
var ErrNoPayoutPerson = errors.New("человека с таким кодом и вкладкой выплат нет")

// TransferPlaceholder stands in the message for the amount actually sent, which only the transfer knows:
// the owner puts it in before the message goes.
const TransferPlaceholder = "{перевод}"

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payouts/prepare", auth.RequireAny(s, []string{auth.ScopePayouts, auth.ScopeRead}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug := r.URL.Query().Get("person")
			if !slugRe.MatchString(slug) {
				server.WriteError(w, http.StatusBadRequest, fmt.Sprintf("person: нужен код человека (slug), а не %q", slug))
				return
			}
			brief, err := PreparePayout(r.Context(), s.Store.RO, slug, ts.Now())
			if errors.Is(err, ErrNoPayoutPerson) {
				server.WriteError(w, http.StatusNotFound, err.Error())
				return
			}
			if err != nil {
				s.Log.Error("payout prepare", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "выплата не подготовлена: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, brief)
		})))
	})
}

// Kinds of a brief's line.
const (
	LineWeek      = "week"      // the fully unpaid days of a week block: cells, base, rate, pay
	LineRemainder = "remainder" // the unpaid part of a day an earlier payout paid partly
	LineDay       = "day"       // an unpaid day whose cells the tab no longer has
	LineOpening   = "opening"   // the frozen debt from before accrualFrom
	LineOther     = "other"     // a manual posting on the debt
	LineUnsplit   = "unsplit"   // the part of the debt the days do not explain (payable_days behind the books)
)

// PrepareDay is one unpaid day of a line.
type PrepareDay struct {
	Date        string       `json:"date"`
	Pages       []PayoutPage `json:"pages"`
	ValueMicros int64        `json:"valueMicros"` // the day's cells summed: the number the line shows
	PayMicros   int64        `json:"payMicros"`   // unpaid of the day
	// Source is "facts" for a day accrued from Hub's facts (from settings.payroll_facts_from): the pages are Hub's, the
	// value is the net of the person's shares. Empty — the sheet's.
	Source string `json:"source,omitempty"`
}

// PrepareLine is one line of the brief and of the message. The lines add up to amountMicros.
type PrepareLine struct {
	Kind       string       `json:"kind"`
	Model      string       `json:"model,omitempty"`
	Tab        string       `json:"tab,omitempty"`
	Source     string       `json:"source,omitempty"` // of a tab's days: "facts" — the cells are Hub's net of the person's shares; empty — the sheet's
	Week       string       `json:"week,omitempty"`   // the first day of the block
	From       string       `json:"from"`
	To         string       `json:"to"`
	Days       []PrepareDay `json:"days,omitempty"`
	BaseMicros *int64       `json:"baseMicros,omitempty"` // week: the cells summed
	Rate       *float64     `json:"rate,omitempty"`       // week: «Ставка %» of the tab
	// ExtraMicros is what the shown pay of a week line stands above base × rate: the tab's fix per shift,
	// or the sheet's own rounding.
	ExtraMicros int64 `json:"extraMicros,omitempty"`
	PayMicros   int64 `json:"payMicros"`
	// ShownMicros is the pay as the message prints it, in whole cents; the shown lines add up to the shown total.
	ShownMicros int64  `json:"shownMicros"`
	Text        string `json:"text"`
}

// PrepareBlock is a week block the payout pays to the end: the owner marks it paid in the tab.
type PrepareBlock struct {
	Model string `json:"model"`
	Tab   string `json:"tab"`
	Week  string `json:"week"`
	From  string `json:"from"` // the block's days in this payout
	To    string `json:"to"`
	// WeekOver is false while days may still come into the block: mark «оплатил по <to> включительно», not «да».
	WeekOver bool `json:"weekOver"`
}

// PrepareFlag is something to look at before paying. It never goes into the message.
type PrepareFlag struct {
	Kind    string `json:"kind"`
	Blocker bool   `json:"blocker"`
	Text    string `json:"text"`
}

// PreparePeriod is the oldest unpaid day (FIFO, the opening's date included) through the last accrued one.
type PreparePeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PrepareSuggestion is the amount rounded the way the owner usually pays: a hint, never in the message.
type PrepareSuggestion struct {
	TransferMicros int64 `json:"transferMicros"`
	// AboveTableMicros is the rounding up: a journal row of its own, «сверх таблицы».
	AboveTableMicros int64  `json:"aboveTableMicros"`
	Note             string `json:"note"`
}

// PayoutBrief is the reply of GET /v1/payouts/prepare.
type PayoutBrief struct {
	OK           bool               `json:"ok"`
	Slug         string             `json:"slug"`
	Name         string             `json:"name"`
	Status       string             `json:"status"`
	GeneratedAt  string             `json:"generatedAt"`
	AccrualFrom  string             `json:"accrualFrom"`
	AmountMicros int64              `json:"amountMicros"` // v_payables balance now; ≤ 0 — nothing to pay
	Nothing      string             `json:"nothing,omitempty"`
	ToPayMicros  *int64             `json:"toPayMicros"` // «К выплате» of the person's tabs
	Period       *PreparePeriod     `json:"period"`
	Lines        []PrepareLine      `json:"lines"`
	Blocks       []PrepareBlock     `json:"blocks"`
	Flags        []PrepareFlag      `json:"flags"`
	Blockers     int                `json:"blockers"`
	Suggestion   *PrepareSuggestion `json:"suggestion"`
	// Message is the text to the person (the template of the finance skill) with the exact amount; the
	// placeholder stands where the amount sent goes. Empty when there is nothing to pay.
	Message     string       `json:"message"`
	Placeholder string       `json:"placeholder"`
	Checks      PayoutChecks `json:"checks"`
}

type prepTab struct {
	model, name, status, errText, fetchedAt string
	rate                                    *float64
	listed                                  bool
	tableStatus, tableErr                   string
	shiftFix                                float64
}

type prepUnpaid struct {
	kind, date, model, name, source string
	amount, paid, unpaid            int64
}

type prepCells struct {
	week  string
	pages []PayoutPage
	value int64
}

func tabKey(model, name string) string { return model + "/" + name }

// PreparePayout reads one person's payout brief in one snapshot. The amount is the debt of the books
// (v_payables); the lines are the unpaid days of payable_days with the cells of payroll_days, so they add
// up to the amount to the micro; when payable_days lags the books, the difference is a line and a blocker.
func PreparePayout(ctx context.Context, db *sql.DB, slug string, now time.Time) (PayoutBrief, error) {
	b := PayoutBrief{OK: true, Slug: slug, GeneratedAt: ts.Format(now), Lines: []PrepareLine{}, Blocks: []PrepareBlock{},
		Flags: []PrepareFlag{}, Placeholder: TransferPlaceholder}
	tx, err := db.BeginTx(ctx, nil) // on the read pool: one snapshot
	if err != nil {
		return b, err
	}
	defer tx.Rollback()
	if b.AccrualFrom, err = AccrualFrom(ctx, tx); err != nil {
		return b, err
	}
	people, err := payoutPeople(ctx, tx)
	if err != nil {
		return b, err
	}
	var p *PayoutPerson
	for i := range people {
		if people[i].Slug == slug {
			p = &people[i]
		}
	}
	if p == nil {
		return b, fmt.Errorf("%s: %w", slug, ErrNoPayoutPerson)
	}
	b.Name, b.Status, b.AmountMicros, b.ToPayMicros = p.Name, p.Status, p.BalanceMicros, p.ToPayMicros
	if b.Checks, err = payoutChecks(ctx, tx, now); err != nil {
		return b, err
	}
	res, err := NewResolver(ctx, tx)
	if err != nil {
		return b, err
	}
	card, _ := res.BySlug(slug)
	tabs, err := prepareTabs(ctx, tx, slug)
	if err != nil {
		return b, err
	}
	unpaid, err := prepareUnpaid(ctx, tx, slug)
	if err != nil {
		return b, err
	}
	oldest := "" // nothing owed: every «не занесено» counts
	if len(unpaid) > 0 && b.AmountMicros > 0 {
		b.Period = &PreparePeriod{From: unpaid[0].date, To: unpaid[0].date}
		for _, u := range unpaid {
			b.Period.From, b.Period.To = min(b.Period.From, u.date), max(b.Period.To, u.date)
		}
		oldest = b.Period.From
	}
	cells, err := prepareCells(ctx, tx, slug, oldest, unpaid)
	if err != nil {
		return b, err
	}
	memos, err := prepareMemos(ctx, tx, slug)
	if err != nil {
		return b, err
	}

	today := ts.Date(now)
	f := &flagger{b: &b}
	if b.AmountMicros > 0 {
		b.Lines = prepareLines(unpaid, cells, tabs, b.AmountMicros, b.AccrualFrom, memos, f)
		b.Blocks = prepareBlocks(b.Lines, today)
		b.Message = prepareMessage(b.Lines, b.AmountMicros)
		b.Suggestion = suggest(b.AmountMicros)
		unfinished(b.Lines, card, now, f)
	} else {
		b.Nothing = "долга нет: платить нечего"
		if b.AmountMicros < 0 {
			b.Nothing = fmt.Sprintf("выплачено вперёд %s: платить нечего, аванс закроют следующие дни", money.Micros(-b.AmountMicros).Cents())
		}
	}
	if b.Checks.Stale {
		f.add("stale", true, "данные несвежие: таблицы прочитаны %s, долги сведены %s — дни могли доехать после", orNever(b.Checks.LastSheetsCollectAt), orNever(b.Checks.LastRebuildAt))
	}
	tabFlags(tabs, unpaid, f)
	if b.ToPayMicros != nil {
		if err := toPayFlags(ctx, tx, &b, f); err != nil {
			return b, err
		}
	}
	if err := unenteredFlags(ctx, tx, slug, oldest, f); err != nil {
		return b, err
	}
	if err := unresolvedFlags(ctx, tx, b.AccrualFrom, res, card, f); err != nil {
		return b, err
	}
	sort.SliceStable(b.Flags, func(i, j int) bool { return b.Flags[i].Blocker && !b.Flags[j].Blocker })
	return b, tx.Commit()
}

// toPayFlags compare the debt with «К выплате» of the tabs. From the switch to Hub's facts on the sheet is a mirror:
// what it governs — the debt with the switch's days as the sheet has them — must equal «К выплате»; the days where the
// mirror and the facts differ are a note, not a blocker.
func toPayFlags(ctx context.Context, q ledger.Querier, b *PayoutBrief, f *flagger) error {
	governed, what := b.AmountMicros, "долг"
	factsFrom, err := FactsFrom(ctx, q)
	if err != nil {
		return err
	}
	var differ []MirrorDay
	if factsFrom != "" {
		m, err := mirror(ctx, q, factsFrom)
		if err != nil {
			return err
		}
		if ms := m[b.Slug]; ms != nil {
			books, sheet := ms.total()
			governed += int64(sheet - books)
			differ = ms.days(b.Slug, b.Name)
		}
		what = "долг с днями с " + ddmm(factsFrom) + " по таблице"
	}
	diff := governed - *b.ToPayMicros
	switch {
	case diff > int64(tolerance) || diff < -int64(tolerance):
		f.add("to_pay", true, "%s %s ≠ «К выплате» вкладок %s: разница %s — сначала понять, откуда", what, money.Micros(governed).Cents(),
			money.Micros(*b.ToPayMicros).Cents(), signedCents(diff))
	case diff >= 10_000 || diff <= -10_000:
		f.add("to_pay", false, "%s %s, «К выплате» вкладок %s: разница %s в пределах округления таблиц", what, money.Micros(governed).Cents(),
			money.Micros(*b.ToPayMicros).Cents(), signedCents(diff))
	}
	if len(differ) > 0 {
		parts := make([]string, len(differ))
		for i, d := range differ {
			parts[i] = fmt.Sprintf("%s таблица %s, факты %s", ddmm(d.Date), money.Micros(d.SheetMicros).Cents(), money.Micros(d.BooksMicros).Cents())
		}
		f.add("mirror", false, "с %s начисляют факты хаба, таблица — зеркало и расходится с ними: %s", ddmm(factsFrom), strings.Join(parts, "; "))
	}
	return nil
}

type flagger struct {
	b    *PayoutBrief
	seen map[string]bool
}

// once adds a flag the first time its key comes.
func (f *flagger) once(key, kind string, blocker bool, format string, args ...any) {
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if !f.seen[key] {
		f.seen[key] = true
		f.add(kind, blocker, format, args...)
	}
}

func (f *flagger) add(kind string, blocker bool, format string, args ...any) {
	f.b.Flags = append(f.b.Flags, PrepareFlag{Kind: kind, Blocker: blocker, Text: fmt.Sprintf(format, args...)})
	if blocker {
		f.b.Blockers++
	}
}

func prepareTabs(ctx context.Context, q ledger.Querier, slug string) (map[string]*prepTab, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.model, t.name, t.status, COALESCE(t.error, ''), t.fetched_at, t.rate, t.listed,
		COALESCE(tb.status, ''), COALESCE(tb.error, '')
		FROM payroll_tabs t LEFT JOIN payroll_tables tb ON tb.model = t.model WHERE t.person = ? ORDER BY t.model, t.name`, slug)
	if err != nil {
		return nil, err
	}
	out := map[string]*prepTab{}
	for rows.Next() {
		var t prepTab
		var rate sql.NullFloat64
		if err := rows.Scan(&t.model, &t.name, &t.status, &t.errText, &t.fetchedAt, &rate, &t.listed, &t.tableStatus, &t.tableErr); err != nil {
			rows.Close()
			return nil, err
		}
		if rate.Valid {
			t.rate = &rate.Float64
		}
		out[tabKey(t.model, t.name)] = &t
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	terms, err := LoadTerms(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, t := range terms {
		if tab := out[tabKey(t.Model, t.Name)]; tab != nil {
			tab.shiftFix = t.ShiftFix
		}
	}
	return out, nil
}

func prepareUnpaid(ctx context.Context, q ledger.Querier, slug string) ([]prepUnpaid, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, date, model, name, source, amount, paid, unpaid FROM payable_days
		WHERE person = ? AND unpaid > 0 ORDER BY seq`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []prepUnpaid
	for rows.Next() {
		var u prepUnpaid
		if err := rows.Scan(&u.kind, &u.date, &u.model, &u.name, &u.source, &u.amount, &u.paid, &u.unpaid); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// prepareCells reads the person's days from the oldest unpaid one on, by "<model>/<name>:<date>" — the sheet's cells of
// a day the sheet accrued, and under "<model>/<name>:<date>:facts" Hub's net of the person's shares by page of a day
// the facts accrued, in the sheet's week block where the sheet still has the day (the owner marks it there), else in
// the Monday's.
func prepareCells(ctx context.Context, q ledger.Querier, slug, from string, unpaid []prepUnpaid) (map[string]prepCells, error) {
	out := map[string]prepCells{}
	if from == "" {
		return out, nil
	}
	if err := sheetCells(ctx, q, slug, from, out); err != nil {
		return nil, err
	}
	var factsTo string
	for _, u := range unpaid {
		if u.source == SourceFacts {
			factsTo = max(factsTo, u.date)
		}
	}
	if factsTo == "" {
		return out, nil
	}
	facts, err := factCells(ctx, q, from, factsTo)
	if err != nil {
		return nil, err
	}
	for _, u := range unpaid {
		if u.source != SourceFacts {
			continue
		}
		f := facts[slug+"\x00"+ledger.Slug(u.model)+"\x00"+u.date]
		c := prepCells{week: weekOfDate(u.date), pages: f.pages, value: f.value}
		if sh, ok := out[tabKey(u.model, u.name)+":"+u.date]; ok {
			c.week = sh.week
		}
		if c.pages == nil {
			c.pages = []PayoutPage{}
		}
		out[tabKey(u.model, u.name)+":"+u.date+sourceSuffix] = c
	}
	return out, nil
}

func sheetCells(ctx context.Context, q ledger.Querier, slug, from string, out map[string]prepCells) error {
	rows, err := q.QueryContext(ctx, `SELECT model, name, date, week, pages FROM payroll_days WHERE person = ? AND date >= ?`, slug, from)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var model, name, date, pages string
		var c prepCells
		if err := rows.Scan(&model, &name, &date, &c.week, &pages); err != nil {
			return err
		}
		if c.pages, err = sheetPages(pages); err != nil {
			return fmt.Errorf("payroll_days %s/%s %s: pages: %w", model, name, date, err)
		}
		if c.pages == nil {
			c.pages = []PayoutPage{}
		}
		for _, pg := range c.pages {
			c.value += pg.RevenueMicros
		}
		out[tabKey(model, name)+":"+date] = c
	}
	return rows.Err()
}

type memo struct {
	date   string
	amount int64
	text   string
	used   bool
}

// prepareMemos reads the manual postings on the person's debt (kind other in payable_days) with their memos.
func prepareMemos(ctx context.Context, q ledger.Querier, slug string) ([]*memo, error) {
	rows, err := q.QueryContext(ctx, `SELECT substr(e.effective_at, 1, 10), -l.amount, e.memo
		FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE l.account = ? AND l.amount < 0 AND e.kind NOT IN (?, ?) AND e.reverses_id IS NULL
		  AND NOT EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		ORDER BY e.id`, "payable:"+slug, KindAccrual, KindOpening)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*memo
	for rows.Next() {
		var m memo
		if err := rows.Scan(&m.date, &m.amount, &m.text); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// prepareLines turns the unpaid days into lines: per tab, the fully unpaid days of each week block as one
// line, a partly paid day as its own remainder; then the opening, manual postings and what the days do not
// explain. The pays add up to amount; the shown cents add up to the shown amount.
func prepareLines(unpaid []prepUnpaid, cells map[string]prepCells, tabs map[string]*prepTab, amount int64, accrualFrom string,
	memos []*memo, f *flagger) []PrepareLine {
	byTab := map[string][]*PrepareLine{}
	weeks := map[string]*PrepareLine{} // "<model>/<name>:<week>"
	var tail []*PrepareLine
	var total int64
	for _, u := range unpaid {
		total += u.unpaid
		switch u.kind {
		case "opening":
			tail = append(tail, &PrepareLine{Kind: LineOpening, From: u.date, To: u.date, PayMicros: u.unpaid})
		case "day":
			key := tabKey(u.model, u.name)
			source, cellKey := "", key+":"+u.date
			if u.source == SourceFacts {
				source, cellKey = SourceFacts, cellKey+sourceSuffix
			}
			c, ok := cells[cellKey]
			day := PrepareDay{Date: u.date, Pages: c.pages, ValueMicros: c.value, PayMicros: u.unpaid, Source: source}
			switch {
			case !ok:
				day.Pages = []PayoutPage{}
				byTab[key] = append(byTab[key], &PrepareLine{Kind: LineDay, Model: u.model, Tab: u.name, Source: source, From: u.date, To: u.date,
					Days: []PrepareDay{day}, PayMicros: u.unpaid})
				f.add("no_cells", false, "%s %s: долг %s за день, которого во вкладке больше нет", u.model, ddmm(u.date), money.Micros(u.unpaid).Cents())
			case u.paid > 0:
				byTab[key] = append(byTab[key], &PrepareLine{Kind: LineRemainder, Model: u.model, Tab: u.name, Source: source, Week: c.week,
					From: u.date, To: u.date, Days: []PrepareDay{day}, PayMicros: u.unpaid})
			default:
				// The sheet's days and the facts' of one block are two lines: their cells are not the same numbers.
				w := weeks[key+":"+c.week+":"+source]
				if w == nil {
					w = &PrepareLine{Kind: LineWeek, Model: u.model, Tab: u.name, Source: source, Week: c.week, From: u.date, To: u.date}
					if t := tabs[key]; t != nil {
						w.Rate = t.rate
					}
					weeks[key+":"+c.week+":"+source] = w
					byTab[key] = append(byTab[key], w)
				}
				w.Days = append(w.Days, day)
				w.From, w.To = min(w.From, u.date), max(w.To, u.date)
				w.PayMicros += u.unpaid
			}
		default:
			l := &PrepareLine{Kind: LineOther, From: u.date, To: u.date, PayMicros: u.unpaid}
			text := ""
			for _, m := range memos {
				if !m.used && m.date == u.date && m.amount == u.amount {
					m.used, text = true, m.text
					break
				}
			}
			tail = append(tail, l)
			if text != "" {
				text = " («" + text + "»)"
			}
			f.add("manual", false, "ручная проводка на долг %s: +%s%s — в сообщении впиши, за что", ddmm(u.date), money.Micros(u.unpaid).Cents(), text)
		}
	}
	if total != amount {
		tail = append(tail, &PrepareLine{Kind: LineUnsplit, PayMicros: amount - total})
		f.add("fifo", true, "дни долга (payable_days) дают %s, а долг %s: свод не пересобран после проводки — ledger payroll rebuild",
			money.Micros(total).Cents(), money.Micros(amount).Cents())
	}

	keys := make([]string, 0, len(byTab))
	for k := range byTab {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []*PrepareLine
	for _, k := range keys {
		list := byTab[k]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].From != list[j].From {
				return list[i].From < list[j].From
			}
			return list[i].Kind == LineRemainder && list[j].Kind != LineRemainder
		})
		lines = append(lines, list...)
	}
	sort.SliceStable(tail, func(i, j int) bool {
		rank := map[string]int{LineOpening: 0, LineOther: 1, LineUnsplit: 2}
		if rank[tail[i].Kind] != rank[tail[j].Kind] {
			return rank[tail[i].Kind] < rank[tail[j].Kind]
		}
		return tail[i].From < tail[j].From
	})
	lines = append(lines, tail...)

	pays := make([]int64, len(lines))
	for i, l := range lines {
		pays[i] = l.PayMicros
	}
	shown := splitCents(pays, amount)
	out := make([]PrepareLine, len(lines))
	for i, l := range lines {
		l.ShownMicros = shown[i]
		switch l.Kind {
		case LineWeek:
			weekText(l, tabs[tabKey(l.Model, l.Tab)], f)
		case LineRemainder:
			l.Text = fmt.Sprintf("остаток за %s: %s", ddmm(l.From), amt(l.ShownMicros))
		case LineDay:
			l.Text = fmt.Sprintf("%s: %s", ddmm(l.From), amt(l.ShownMicros))
		case LineOpening:
			l.Text = fmt.Sprintf("старый долг: %s — дни до %s", signedAmt(l.ShownMicros), ddmm(accrualFrom))
		case LineOther:
			l.Text = fmt.Sprintf("поправка %s: %s — {за что}", ddmm(l.From), signedAmt(l.ShownMicros))
		case LineUnsplit:
			l.Text = fmt.Sprintf("прочее: %s — {за что}", signedAmt(l.ShownMicros))
		}
		out[i] = *l
	}
	return out
}

// weekText writes «дд–дд.мм: a+b+… = база × ставка% = зп». A pay above base × rate is the fix when the tab
// has «Фикс/см», otherwise the sheet's rounding, and a flag.
func weekText(l *PrepareLine, tab *prepTab, f *flagger) {
	sort.Slice(l.Days, func(i, j int) bool { return l.Days[i].Date < l.Days[j].Date })
	var base int64
	values := make([]string, len(l.Days))
	for i, d := range l.Days {
		base += d.ValueMicros
		values[i] = cellNum(d.ValueMicros)
	}
	l.BaseMicros = &base
	sum := cellNum(base)
	if len(values) > 1 {
		sum = strings.Join(values, "+") + " = " + sum
	}
	when := span(l.From, l.To)
	if l.Rate == nil {
		l.Text = fmt.Sprintf("%s: %s, зп %s", when, sum, amt(l.ShownMicros))
		f.once("no_rate:"+tabKey(l.Model, l.Tab), "no_rate", false, "%s: в шапке вкладки нет «Ставка %%» — строки без формулы", l.Model)
		return
	}
	pct := int64(money.RoundCents(money.FromFloat(float64(base) / money.Scale * *l.Rate)))
	rate := strconv.FormatFloat(money.RoundN(*l.Rate*100, 6), 'f', -1, 64) + "%"
	// Against the line's own cents: a cent splitCents moved to make the total is no difference of the sheet.
	l.ExtraMicros = int64(money.RoundCents(money.Micros(l.PayMicros))) - pct
	switch {
	case l.ExtraMicros == 0:
		l.Text = fmt.Sprintf("%s: %s × %s = %s", when, sum, rate, amt(l.ShownMicros))
	case tab != nil && tab.shiftFix != 0:
		l.Text = fmt.Sprintf("%s: %s × %s + фикс %s = %s", when, sum, rate, amt(l.ExtraMicros), amt(l.ShownMicros))
	default:
		sign := "+"
		if l.ExtraMicros < 0 {
			sign = "−"
		}
		l.Text = fmt.Sprintf("%s: %s × %s %s %s = %s", when, sum, rate, sign, amt(abs(l.ExtraMicros)), amt(l.ShownMicros))
		f.add("formula", false, "%s %s: зп по таблице %s, а %s × %s = %s — проверь блок, в строке разница %s", l.Model, when,
			amt(l.ShownMicros), cellNum(base), rate, amt(pct), signedAmt(l.ExtraMicros))
	}
}

// splitCents rounds each pay to cents so that the rounded pays add up to the rounded amount: the cents
// left over go to the lines rounded the farthest the other way.
func splitCents(pays []int64, amount int64) []int64 {
	out := make([]int64, len(pays))
	var sum int64
	for i, p := range pays {
		out[i] = int64(money.RoundCents(money.Micros(p)))
		sum += out[i]
	}
	target := int64(money.RoundCents(money.Micros(amount)))
	for len(pays) > 0 && sum != target {
		step := int64(10_000)
		if sum > target {
			step = -step
		}
		best, bestGap := 0, int64(0)
		for i, p := range pays {
			gap := p - out[i]
			if step < 0 {
				gap = -gap
			}
			if i == 0 || gap > bestGap {
				best, bestGap = i, gap
			}
		}
		out[best] += step
		sum += step
	}
	return out
}

// prepareBlocks lists the week blocks the lines pay to the end.
func prepareBlocks(lines []PrepareLine, today string) []PrepareBlock {
	byKey := map[string]*PrepareBlock{}
	var keys []string
	for _, l := range lines {
		if l.Week == "" {
			continue
		}
		k := tabKey(l.Model, l.Tab) + ":" + l.Week
		b := byKey[k]
		if b == nil {
			b = &PrepareBlock{Model: l.Model, Tab: l.Tab, Week: l.Week, From: l.From, To: l.To, WeekOver: addDays(l.Week, 6) < today}
			byKey[k] = b
			keys = append(keys, k)
		}
		b.From, b.To = min(b.From, l.From), max(b.To, l.To)
	}
	sort.Strings(keys)
	out := make([]PrepareBlock, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}

// prepareMessage is the template of the finance skill, «Сообщение работнику об оплате», in lower case: the
// period, the lines per tab, the corrections, the total and the placeholder of the amount sent.
func prepareMessage(lines []PrepareLine, amount int64) string {
	from, to := "", ""
	opening := ""
	groups := 0
	for i, l := range lines {
		if l.Model != "" && (i == 0 || tabKey(lines[i-1].Model, lines[i-1].Tab) != tabKey(l.Model, l.Tab)) {
			groups++
		}
		switch l.Kind {
		case LineOpening:
			opening = l.To
		case LineUnsplit:
		default:
			if from == "" || l.From < from {
				from = l.From
			}
			to = max(to, l.To)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "закинул зп, %s usdt\n", TransferPlaceholder)
	if from != "" {
		fmt.Fprintf(&b, "период %s\n", span(from, to))
	} else if opening != "" {
		fmt.Fprintf(&b, "период по %s\n", ddmm(opening))
	}
	prev := ""
	tail := false
	for _, l := range lines {
		if l.Model != "" {
			if k := tabKey(l.Model, l.Tab); k != prev {
				fmt.Fprintf(&b, "\n%s\n", strings.ToLower(l.Model))
				prev = k
			}
		} else if !tail {
			tail = true
			if groups != 1 {
				b.WriteString("\n")
			}
		}
		b.WriteString(strings.ToLower(l.Text) + "\n")
	}
	if !tail && groups != 1 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "итого %s → %s\n\nчекни что дошло", amt(int64(money.RoundCents(money.Micros(amount)))), TransferPlaceholder)
	return b.String()
}

// suggest rounds the amount to the dollar, the way the owner usually pays.
func suggest(amount int64) *PrepareSuggestion {
	s := &PrepareSuggestion{TransferMicros: (amount + money.Scale/2) / money.Scale * money.Scale}
	diff := s.TransferMicros - amount
	switch {
	case diff > 0:
		s.AboveTableMicros = diff
		s.Note = fmt.Sprintf("до доллара вверх на %s: в журнал отдельной строкой {сверх таблицы: %s}", money.Micros(diff).Cents(), money.Micros(diff).Cents())
	case diff < 0:
		s.Note = fmt.Sprintf("до доллара вниз на %s: остаток останется долгом", money.Micros(-diff).Cents())
	default:
		s.Note = "сумма ровная"
	}
	return s
}

// unfinished flags a paid day whose shift is still on: «незавершённую смену не включать».
func unfinished(lines []PrepareLine, card Person, now time.Time, f *flagger) {
	sl, haveSlot := parseSlot(card.Schedule)
	seen := map[string]bool{}
	for _, l := range lines {
		for _, d := range l.Days {
			if seen[d.Date] {
				continue
			}
			seen[d.Date] = true
			date, err := time.ParseInLocation(ts.DateLayout, d.Date, ts.MSK)
			if err != nil {
				continue
			}
			if haveSlot {
				if _, end := sl.window(date); end.After(now) {
					f.add("unfinished", true, "смена %s ещё идёт (до %s): этот день платить рано", ddmm(d.Date), end.In(ts.MSK).Format("15:04 02.01"))
				}
			} else if d.Date >= ts.Date(now) {
				f.add("unfinished", false, "день %s — сегодня, а графика в карточке нет: смена могла не кончиться", ddmm(d.Date))
			}
		}
	}
}

// tabFlags: a tab or table in error, and a tab gone from the lists that still holds unpaid days.
func tabFlags(tabs map[string]*prepTab, unpaid []prepUnpaid, f *flagger) {
	keys := make([]string, 0, len(tabs))
	for k := range tabs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tables := map[string]bool{}
	for _, k := range keys {
		t := tabs[k]
		if t.tableStatus == "error" && !tables[t.model] {
			tables[t.model] = true
			f.add("table_error", true, "таблица %s не прочитана: список вкладок неизвестен — %s", t.model, t.tableErr)
		}
		if t.status != "ok" {
			f.add("tab_error", true, "вкладка %s в ошибке: дни и «К выплате» от последнего удачного разбора — %s", t.model, t.errText)
		}
		if !t.listed {
			n, sum := 0, int64(0)
			for _, u := range unpaid {
				if u.kind == "day" && tabKey(u.model, u.name) == k {
					n++
					sum += u.unpaid
				}
			}
			if n > 0 {
				f.add("tab_unlisted", true, "вкладки %s нет в списках «Итого» и «Неактивные»: её %d дн. на %s держатся на чтении %s — решить, платить ли",
					t.model, n, money.Micros(sum).Cents(), t.fetchedAt)
			}
		}
	}
}

// unenteredFlags: «не занесено» on or after the oldest unpaid day (all of it when nothing is owed). A day
// the hub shows money for is a blocker, saying whether it is the person's own sales by the facts or the page money of
// their slot; a day it could not prove is a note per tab. From the switch to Hub's facts on, a day the facts did not
// accrue (week not proven, no Hub user, no rate) is a blocker: the debt may lack it.
func unenteredFlags(ctx context.Context, q ledger.Querier, slug, from string, f *flagger) error {
	// A day the facts did not accrue counts whatever the oldest unpaid day: it is not in the debt at all.
	rows, err := q.QueryContext(ctx, `SELECT model, date, page, status, COALESCE(reason, ''), net_mills, share FROM payroll_unentered
		WHERE person = ? AND (date >= ? OR (status = 'unknown' AND reason LIKE ? || '%')) ORDER BY model, date, page`, slug, from, reasonPending)
	if err != nil {
		return err
	}
	type day struct {
		model, date string
		pages       []string
		net         int64
		share       float64
		noShare     bool
		facts       bool // counted from Hub's team facts, not the slot
	}
	var days []*day
	var pending []string             // the days from the switch to Hub's facts the facts did not accrue
	unknown := map[string][]string{} // model → dates
	reason := map[string]string{}
	var models []string
	for rows.Next() {
		var model, date, page, status, why string
		var net sql.NullInt64
		var share sql.NullFloat64
		if err := rows.Scan(&model, &date, &page, &status, &why, &net, &share); err != nil {
			rows.Close()
			return err
		}
		if status != "ok" && strings.HasPrefix(why, reasonPending) {
			pending = append(pending, fmt.Sprintf("%s %s: %s", model, ddmm(date), strings.TrimPrefix(strings.TrimPrefix(why, reasonPending), ": ")))
			continue
		}
		if status != "ok" {
			if _, ok := unknown[model]; !ok {
				models = append(models, model)
				reason[model] = why
			}
			if l := unknown[model]; len(l) == 0 || l[len(l)-1] != ddmm(date) {
				unknown[model] = append(l, ddmm(date))
			}
			continue
		}
		if n := len(days); n == 0 || days[n-1].model != model || days[n-1].date != date {
			days = append(days, &day{model: model, date: date})
		}
		d := days[len(days)-1]
		d.pages = append(d.pages, page)
		d.facts = d.facts || strings.HasPrefix(why, reasonFacts)
		d.net += net.Int64 * 1000
		if share.Valid {
			d.share += share.Float64
		} else {
			d.noShare = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range days {
		share := "доля ≈ " + money.FromFloat(d.share).Cents()
		if d.noShare {
			share = "доля не посчитана: нет ставки"
		}
		seen := "в окне графика хаб видит"
		if d.facts {
			seen = "по его сменам хаб видит"
		}
		f.add("unentered", true, "не занесено %s (%s): %s %s на %s, %s — в таблице пусто", ddmm(d.date), d.model, seen,
			money.Micros(d.net).Cents(), strings.Join(d.pages, ", "), share)
	}
	for _, m := range models {
		f.add("unentered_unknown", false, "не занесено? %s: %s — хаб не доказал: %s", m, strings.Join(unknown[m], ", "), reason[m])
	}
	for _, p := range pending {
		f.add("facts_pending", true, "начисление из фактов неизвестно, не ноль — %s", p)
	}
	return nil
}

// unresolvedFlags: salary rows of the journal no person was found for. One in this person's name may be a
// payout to them the debt does not know of yet.
func unresolvedFlags(ctx context.Context, q ledger.Querier, from string, res *Resolver, card Person, f *flagger) error {
	_, bad, err := payoutRows(ctx, q, from, res)
	if err != nil {
		return err
	}
	names := map[string]bool{strings.ToLower(card.Name): true, strings.ToLower(parenthetical.ReplaceAllString(card.Name, "")): true}
	for _, a := range card.Aliases {
		names[strings.ToLower(a)] = true
	}
	others := 0
	for _, u := range bad {
		if names[strings.ToLower(strings.TrimSpace(u.Name))] {
			f.add("unresolved", true, "строка журнала #%d от %s на %s не разнесена: %s — долг может быть завышен", u.Num, ddmm(u.Date), u.Amount, u.Reason)
		} else {
			others++
		}
	}
	if others > 0 {
		f.add("unresolved", false, "ещё строк «Чаттер зарплата» не разнесено ни на кого: %d (ledger payables) — если среди них выплата этому человеку, долг завышен", others)
	}
	return nil
}

func orNever(s *string) string {
	if s == nil {
		return "никогда"
	}
	return *s
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func addDays(date string, n int) string {
	t, err := time.Parse(ts.DateLayout, date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, n).Format(ts.DateLayout)
}

// ddmm prints a date as «07.09».
func ddmm(date string) string {
	if len(date) != 10 {
		return date
	}
	return date[8:10] + "." + date[5:7]
}

// span prints days as «07–13.09», «28.09–04.10» or «07.09».
func span(from, to string) string {
	switch {
	case from == to:
		return ddmm(from)
	case len(from) == 10 && len(to) == 10 && from[:7] == to[:7]:
		return from[8:10] + "–" + ddmm(to)
	}
	return ddmm(from) + "–" + ddmm(to)
}

// cellNum prints a sheet value exactly: «120», «123.45», «0.125».
func cellNum(m int64) string {
	if m%money.Scale == 0 {
		return strconv.FormatInt(m/money.Scale, 10)
	}
	return money.Micros(m).String()
}

// amt prints whole cents: «105», «197.50».
func amt(m int64) string {
	if m%money.Scale == 0 {
		return strconv.FormatInt(m/money.Scale, 10)
	}
	return money.Micros(m).Cents()
}

func signedAmt(m int64) string {
	if m < 0 {
		return "-" + amt(-m)
	}
	return "+" + amt(m)
}

func signedCents(m int64) string {
	if m > 0 {
		return "+" + money.Micros(m).Cents()
	}
	return money.Micros(m).Cents()
}
