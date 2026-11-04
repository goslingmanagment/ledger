package month

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Ordered is a float map that remembers insertion order, like a Python dict: sums run in the same order.
type Ordered struct {
	Keys []string
	Vals map[string]float64
}

func newOrdered() *Ordered { return &Ordered{Vals: map[string]float64{}} }

// Add adds to a key, creating it even for zero, as defaultdict(float) does.
func (o *Ordered) Add(k string, v float64) {
	if _, ok := o.Vals[k]; !ok {
		o.Keys = append(o.Keys, k)
		o.Vals[k] = 0
	}
	o.Vals[k] += v
}

// Sum adds the values in insertion order.
func (o *Ordered) Sum() float64 {
	xs := make([]float64, 0, len(o.Keys))
	for _, k := range o.Keys {
		xs = append(xs, o.Vals[k])
	}
	return money.PySum(xs)
}

func (o *Ordered) delete(k string) {
	delete(o.Vals, k)
	for i, x := range o.Keys {
		if x == k {
			o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
			return
		}
	}
}

// MarshalJSON writes the values rounded as stored (full float) in insertion order.
func (o *Ordered) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		ks, _ := json.Marshal(k)
		vs, _ := json.Marshal(o.Vals[k])
		b.Write(ks)
		b.WriteByte(':')
		b.Write(vs)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// LineKey is a (model, channel) line of the card.
type LineKey struct{ Model, Channel string }

// Line is one line of the card: net and the parts of what was earned.
type Line struct {
	Model   string
	Channel string
	Vals    *Ordered
}

// Extra is a payout above the table counted in the month.
type Extra struct {
	N      int
	Amount float64
	Who    string
}

// Expenses of the month.
type Expenses struct {
	Chat  *Ordered
	Extra []Extra
	Cats  *Ordered
	Total float64
	Notes []string
}

// OffModel is the money past the platforms of one model.
type OffModel struct {
	Received float64
	Earned   float64
	Rows     []string
}

// Card is the month card at a cut.
type Card struct {
	Month    string
	AsOf     time.Time
	LedgerN  int
	Cover    *Cover
	Lines    []*Line
	lineIdx  map[LineKey]*Line
	Agg      map[string]*Ordered
	AggOrder []string
	Off      map[string]*OffModel
	OffOrder []string
	Exp      *Expenses
	Notes    []string
	InMonth  []*Payment
	M0, M1   time.Time
}

func (c *Card) line(k LineKey) *Line {
	if l, ok := c.lineIdx[k]; ok {
		return l
	}
	l := &Line{Model: k.Model, Channel: k.Channel, Vals: newOrdered()}
	c.lineIdx[k] = l
	c.Lines = append(c.Lines, l)
	return l
}

var parts = []string{"arrived", "riding", "page", "hold", "stuck"}

// BuildCard computes the card of a month from a coverage. tabs nil means expenses are not counted.
func BuildCard(cv *Cover, month string, tabs []Tab) (*Card, error) {
	full := ts.Month(cv.Cfg.FullLedger)
	if month < full {
		return nil, failf("%s: месяцы до %s не считаются — ledger неполный, переводы не размечены", month, full)
	}
	m0, m1, err := ts.MonthStart(month)
	if err != nil {
		return nil, err
	}
	asOf := cv.AsOf
	card := &Card{Month: month, AsOf: asOf, Cover: cv, lineIdx: map[LineKey]*Line{}, Agg: map[string]*Ordered{}, Off: map[string]*OffModel{},
		Notes: append(append([]string(nil), cv.Notes...), cv.Cfg.CardNotes...), M0: m0, M1: m1}
	c := cv.Cfg
	for _, p := range cv.PageOrder {
		rs := cv.Rows[p]
		minimum := c.Routes[p].Min
		var freeParts []float64
		for _, r := range rs {
			if !r.Avail.After(asOf) {
				for _, part := range r.Parts {
					if part.Req == nil && !part.Anchor {
						freeParts = append(freeParts, part.Amt)
					}
				}
			}
		}
		free := money.PySum(freeParts)
		for _, r := range rs {
			if r.T.Before(m0) || !r.T.Before(m1) {
				continue
			}
			a := card.Agg[p]
			if a == nil {
				a = newOrdered()
				card.Agg[p] = a
				card.AggOrder = append(card.AggOrder, p)
			}
			card.InMonth = append(card.InMonth, r)
			a.Add("net", r.Net)
			for _, part := range r.Parts {
				if part.Anchor || (part.Req != nil && part.Req.Pre) {
					return nil, failf("%s: платёж %s покрыт до якоря — якорь позже начала месяца", p, iso(r.T))
				}
				est := float64(part.Amt * c.RateOf(p, part.Req) * c.Share)
				var t *Transfer
				if part.Req != nil {
					t = part.Req.Transfer
				}
				switch {
				case t != nil && len(t.Bad) == 0:
					if !t.HasK {
						return nil, failf("перевод #%d покрывает платёж месяца, но без k (срез не позже якоря)", t.N)
					}
					a.Add("arrived", float64(est*t.K))
				case part.Req != nil:
					a.Add("riding", est)
				case r.Avail.After(asOf):
					a.Add("hold", est)
				case minimum != 0 && free < minimum:
					a.Add("stuck", est)
				default:
					a.Add("page", est)
				}
			}
		}
	}
	for _, p := range card.AggOrder {
		a := card.Agg[p]
		var earned []float64
		for _, k := range parts {
			a.Add(k, 0)
			earned = append(earned, a.Vals[k])
		}
		a.Add("earned", money.PySum(earned))
		rt := c.Routes[p]
		l := card.line(LineKey{rt.Model, rt.Channel})
		for _, k := range a.Keys {
			l.Vals.Add(k, a.Vals[k])
		}
	}
	grant, err := cv.Src.Pages()
	if err != nil {
		return nil, err
	}
	for _, g := range grant {
		if _, ok := c.Routes[g.Label]; ok {
			continue
		}
		s := m0
		if st, ok := c.Exc.Starts[g.Label]; ok && st.After(s) {
			s = st
		}
		e := m1
		if asOf.Before(e) {
			e = asOf
		}
		net := 0.0
		if s.Before(e) {
			if net, err = cv.Src.SummaryNet(g.Label, s, e); err != nil {
				return nil, err
			}
		}
		card.line(LineKey{g.Model, g.Label + ": не согласовано"}).Vals.Add("net", net)
		card.Notes = append(card.Notes, g.Label+": страница без строки в таблице маршрутов — условия не согласованы, в цифру не входит")
	}
	off, order, err := offPlatform(c, month, cv.Ledger)
	if err != nil {
		return nil, err
	}
	card.Off, card.OffOrder = off, order
	for _, m := range order {
		v := off[m]
		l := card.line(LineKey{m, "вне платформ"})
		l.Vals.Add("net", v.Received)
		l.Vals.Add("earned", v.Earned)
		l.Vals.Add("arrived", v.Earned)
	}
	for _, r := range cv.Ledger {
		if r.Num > card.LedgerN {
			card.LedgerN = r.Num
		}
	}
	if tabs != nil {
		card.Exp, err = expenses(c, month, cv.Ledger, tabs, asOf)
		if err != nil {
			return nil, err
		}
	}
	return card, nil
}

// offPlatform: money past the platforms by date of arrival: received − the model's share of a deal; the
// share is taken once, in the deal's first payment; {доля 0} has none.
func offPlatform(c Config, month string, rows []journal.Row) (map[string]*OffModel, []string, error) {
	type terms struct {
		price, share float64
		model        string
	}
	deals := map[string]terms{}
	out := map[string]*OffModel{}
	var order []string
	for _, r := range rows {
		if r.Type != "income" || r.Category == "Платформы" || r.Category == "Owner вливание" || r.Category == "Возврат расхода" {
			continue
		}
		tg, err := journal.RowTags(r)
		if err != nil {
			return nil, nil, Fail{err.Error()}
		}
		share := 0.0
		if tg.Deal != nil {
			cur := terms{tg.Deal.Price, tg.Deal.Share, r.Model}
			if prev, ok := deals[tg.Deal.ID]; ok {
				if prev != cur {
					return nil, nil, failf("ledger #%d: сделка «%s» с другими ценой/долей/моделью, чем в первой строке", r.Num, tg.Deal.ID)
				}
			} else {
				share = tg.Deal.Share
				deals[tg.Deal.ID] = cur
			}
		}
		if r.Date[:7] != month {
			continue
		}
		if tg.Deal == nil && !tg.ShareZero {
			return nil, nil, failf("ledger #%d: поступление «%s» без тега {сделка: …} или {доля 0}", r.Num, r.Category)
		}
		if !c.HasModel(r.Model) {
			return nil, nil, failf("ledger #%d: поступление «%s» без модели из %v («%s»)", r.Num, r.Category, c.Models, r.Model)
		}
		v := out[r.Model]
		if v == nil {
			v = &OffModel{}
			out[r.Model] = v
			order = append(order, r.Model)
		}
		amt := mustFloat(r.Amount)
		v.Received += amt
		v.Earned += amt - share
		row := fmt.Sprintf("#%d %.2f", r.Num, amt)
		if share != 0 {
			row += fmt.Sprintf(" − доля %.2f", share)
		}
		v.Rows = append(v.Rows, row)
	}
	return out, order, nil
}

var owner = map[string]bool{"Owner зарплата": true, "Owner draw": true, "Owner вливание": true}

// expenses: chatters by work days from the tabs; above the table in its period or the month of payment;
// a prepayment for 2+ months evenly; {за ГГГГ-ММ: N} moves a part; the rest by the date of payment.
func expenses(c Config, month string, rows []journal.Row, tabs []Tab, asOf time.Time) (*Expenses, error) {
	e := &Expenses{Chat: newOrdered(), Cats: newOrdered()}
	asDate := ts.Date(asOf)
	for _, t := range tabs {
		e.Notes = append(e.Notes, t.Notes...)
		days := make([]string, 0, len(t.Days))
		for d := range t.Days {
			days = append(days, d)
		}
		sort.Strings(days)
		var worked []float64
		for _, d := range days {
			if d[:7] == month && d <= asDate {
				worked = append(worked, t.Days[d])
			}
		}
		e.Chat.Add(t.Model, money.PySum(worked))
	}
	for _, r := range rows {
		mo := r.Date[:7]
		if !(r.Type == "expense" || (r.Type == "income" && r.Category == "Возврат расхода")) || owner[r.Category] {
			continue
		}
		tg, err := journal.RowTags(r)
		if err != nil {
			return nil, Fail{err.Error()}
		}
		amt := mustFloat(r.Amount)
		switch {
		case r.Type == "income":
			if mo == month && !tg.BeforeAug {
				e.Cats.Add("Возврат расхода", -amt)
			}
		case r.Category == "Чаттер зарплата":
			if tg.AboveTable != nil {
				period := tg.AboveTable.Period
				if period == "" {
					period = mo
				}
				if period == month {
					e.Extra = append(e.Extra, Extra{r.Num, tg.AboveTable.Amount, r.Counterparty})
				}
			}
		case tg.InsideShare || (r.Category == "Комиссии" && r.Method == "Удержано из вывода"):
		case r.Category == "Доля модели" && !tg.AboveShare:
			e.Notes = append(e.Notes, fmt.Sprintf("#%d: «Доля модели» без тега {внутри доли} / {сверх доли} — считаю внутри доли", r.Num))
		case tg.Prepaid != nil:
			months := tg.Prepaid.Months()
			in := false
			for _, m := range months {
				in = in || m == month
			}
			v := 0.0
			if in {
				v = amt / float64(len(months))
			}
			e.Cats.Add(r.Category, v)
		default:
			var movedParts []float64
			here := 0.0
			for _, fm := range tg.ForMonth {
				movedParts = append(movedParts, fm.Amount)
				if fm.Month == month {
					here = fm.Amount
				}
			}
			movedSum := money.PySum(movedParts)
			own := 0.0
			if mo == month {
				own = amt - movedSum
			}
			e.Cats.Add(r.Category, here+own)
		}
	}
	y, m := 0, 0
	fmt.Sscanf(month, "%d-%d", &y, &m)
	prevY := y
	if m == 1 {
		prevY = y - 1
	}
	prev := fmt.Sprintf("%d-%02d", prevY, (m+10)%12+1)
	var lost []string
	for _, r := range rows {
		if r.Type != "expense" || r.Category != "Чаттер зарплата" || r.Date[:7] != month {
			continue
		}
		name := r.Counterparty
		if a, ok := c.TabAliases[name]; ok {
			name = a
		}
		found := false
		for _, t := range tabs {
			if t.Name != name || !(t.Model == r.Model || !c.HasModel(r.Model)) {
				continue
			}
			for _, d := range t.Worked {
				if d[:7] == month || d[:7] == prev {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			lost = append(lost, fmt.Sprintf("#%d %s (%s, %s)", r.Num, r.Counterparty, r.Model, r.Date))
		}
	}
	if len(lost) > 0 {
		return nil, failf("выплаты чаттерам без вкладки с днями работы за %s–%s (работа не посчитана или вкладка датирована не тем годом): %s",
			prev, month, strings.Join(lost, "; "))
	}
	if v, ok := e.Cats.Vals["Доля модели"]; ok {
		e.Cats.delete("Доля модели")
		e.Cats.Add("Доля модели сверх доли", v)
	}
	var extras []float64
	for _, x := range e.Extra {
		extras = append(extras, x.Amount)
	}
	e.Total = e.Chat.Sum() + money.PySum(extras) + e.Cats.Sum()
	kept := newOrdered()
	for _, k := range e.Cats.Keys {
		if math.Abs(e.Cats.Vals[k]) > 5e-3 {
			kept.Add(k, e.Cats.Vals[k])
		}
	}
	e.Cats = kept
	return e, nil
}

// Totals of the card lines; a page without an agreed route stays out.
type Totals struct {
	Net, Earned, Arrived, Riding, Page, Hold, Stuck, Transit float64
	Decision                                                 *float64
}

// Totals sums the lines.
func (c *Card) Totals() Totals {
	var t Totals
	for _, l := range c.Lines {
		if strings.HasSuffix(l.Channel, "не согласовано") {
			continue
		}
		t.Net += l.Vals.Vals["net"]
		t.Earned += l.Vals.Vals["earned"]
		t.Arrived += l.Vals.Vals["arrived"]
		t.Riding += l.Vals.Vals["riding"]
		t.Page += l.Vals.Vals["page"]
		t.Hold += l.Vals.Vals["hold"]
		t.Stuck += l.Vals.Vals["stuck"]
	}
	t.Transit = t.Hold + t.Page + t.Riding
	if c.Exp != nil {
		d := t.Earned - t.Stuck - c.Exp.Total
		t.Decision = &d
	}
	return t
}

// Verdict of the owner-salary safeguard: candidate ≤ share × decision figure; «на грани» when the margin
// is below max($100, what still stands as an estimate at the close).
func Verdict(decision, candidate, share float64, transit *float64) string {
	margin := decision - candidate/share
	if margin < 0 {
		return "не проходит"
	}
	if transit != nil && margin < math.Max(100, *transit) {
		return "на грани"
	}
	return "проходит"
}

// Check is one check of the card.
type Check struct {
	N   int    `json:"n"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
}

// Checks are plan §6: 1, 2, 3, 5, 6 of the script.
func (c *Card) Checks(tabsErr string) []Check {
	cv := c.Cover
	cfg := cv.Cfg
	var bad []string
	for _, p := range cv.PageOrder {
		for _, r := range cv.Rows[p] {
			var amts []float64
			for _, part := range r.Parts {
				amts = append(amts, part.Amt)
			}
			if math.Abs(money.PySum(amts)-r.Net) > 1e-6 {
				bad = append(bad, p+" "+iso(r.T))
			}
		}
	}
	out := []Check{}
	if len(bad) > 0 {
		out = append(out, Check{1, false, fmt.Sprintf("не сходится: %v", bad)})
	} else {
		out = append(out, Check{1, true, "части платежей = net; заработано = пришло + в пути"})
	}
	var over, part []string
	type cnt struct{ all, fit int }
	byModel := map[string]*cnt{}
	var modelOrder []string
	for _, q := range cv.Reqs {
		m := cfg.Routes[q.Page].Model
		if byModel[m] == nil {
			byModel[m] = &cnt{}
			modelOrder = append(modelOrder, m)
		}
		byModel[m].all++
		if q.Excess <= 1 && math.Abs(q.Avail-q.Amount) <= 1 {
			byModel[m].fit++
		}
		if q.Excess > 1 {
			over = append(over, fmt.Sprintf("%s %s %.2f > %.2f", q.Page, iso(q.T), q.Amount, q.Avail))
		} else if q.Avail-q.Amount > 1 {
			part = append(part, fmt.Sprintf("%s %s %.2f из %.2f", q.Page, q.T.In(ts.MSK).Format("02.01"), q.Amount, q.Avail))
		}
	}
	var counts []string
	for _, m := range modelOrder {
		counts = append(counts, fmt.Sprintf("%s %d из %d", cfg.Names[m], byModel[m].fit, byModel[m].all))
	}
	msg := "заявки Fansly и OF = net покрытых платежей ±$1: " + strings.Join(counts, ", ")
	if len(part) > 0 {
		msg += "; частичный вывод: " + strings.Join(part, "; ")
	}
	if len(over) > 0 {
		msg += "; БОЛЬШЕ доступного net: " + strings.Join(over, "; ")
	}
	out = append(out, Check{2, len(over) == 0, msg})
	type mt struct {
		m string
		t *Transfer
	}
	var trs []mt
	for _, m := range sortedTransferModels(cv) {
		for _, t := range cv.Transfers[m] {
			if t.HasK {
				trs = append(trs, mt{m, t})
			}
		}
	}
	var offs, ks []string
	for _, x := range trs {
		if len(x.t.Bad) > 0 {
			offs = append(offs, fmt.Sprintf("#%d не принят: %s", x.t.N, strings.Join(x.t.Bad, ", ")))
		}
		ks = append(ks, fmt.Sprintf("#%d %.4f", x.t.N, x.t.K))
	}
	out = append(out, Check{3, len(offs) == 0, strings.Join(append(offs, "k: "+strings.Join(ks, ", ")), "; ")})
	var fails, notes []string
	for _, x := range trs {
		t := x.t
		mult := cfg.Share
		if cfg.PayeeOf(x.m) == "agency" {
			mult = 1
		}
		calc, credited := float64(sum(t.Deps)*mult), t.Amount-t.Held-t.Fee
		if len(t.Deps) == 0 {
			notes = append(notes, fmt.Sprintf("#%d без депозитов в теге — арифметику не проверить", t.N))
		} else if math.Abs(calc-t.Amount) > .01 {
			m := fmt.Sprintf("#%d депозиты → %.2f, записано %.2f (округление %+.2f)", t.N, calc, t.Amount, calc-t.Amount)
			if math.Abs(calc-t.Amount) > 10 {
				fails = append(fails, m)
			} else {
				notes = append(notes, m)
			}
		}
		hits, ok := cv.Src.BybitIncomes(t.Date)
		found := false
		for _, a := range hits {
			found = found || math.Abs(a-credited) <= .01
		}
		if !ok {
			fails = append(fails, "не проверено: движения Bybit не читаются")
		} else if !found {
			fails = append(fails, fmt.Sprintf("#%d: зачисления %.2f на Bybit за %s нет", t.N, credited, t.Date))
		}
	}
	msg5 := "теги переводов сходятся с Bybit"
	if joined := uniqueJoin(append(fails, notes...)); joined != "" {
		msg5 = joined
	}
	out = append(out, Check{5, len(fails) == 0, msg5})
	if tabsErr != "" {
		out = append(out, Check{6, false, tabsErr})
	} else {
		out = append(out, Check{6, true, "таблицы выплат разобраны; выплаты чаттерам месяца нашли вкладки"})
	}
	return out
}

func sortedTransferModels(cv *Cover) []string {
	var out []string
	for _, m := range cv.Cfg.Models {
		if _, ok := cv.Transfers[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

func uniqueJoin(xs []string) string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return strings.Join(out, "; ")
}

// Snapshot renders the card in the shape of the closing snapshot of the script (rounded the same way).
func (c *Card) Snapshot() map[string]any {
	t := c.Totals()
	round2 := func(x float64) float64 { return money.Round2(x) }
	totals := map[string]any{"net": round2(t.Net), "earned": round2(t.Earned), "arrived": round2(t.Arrived), "riding": round2(t.Riding),
		"page": round2(t.Page), "hold": round2(t.Hold), "stuck": round2(t.Stuck), "transit": round2(t.Transit)}
	out := map[string]any{"month": c.Month, "as_of": iso(c.AsOf), "ledger_n": c.LedgerN, "totals": totals}
	lines := []map[string]any{}
	for _, l := range c.Lines {
		m := map[string]any{"model": l.Model, "channel": l.Channel}
		for _, k := range l.Vals.Keys {
			m[k] = round2(l.Vals.Vals[k])
		}
		lines = append(lines, m)
	}
	out["lines"] = lines
	if t.Decision != nil {
		totals["decision"] = round2(*t.Decision)
		out["decision"] = round2(*t.Decision)
	}
	if c.Exp != nil {
		extra := [][]any{}
		for _, x := range c.Exp.Extra {
			extra = append(extra, []any{x.N, x.Amount, x.Who})
		}
		out["expenses"] = map[string]any{"chat": c.Exp.Chat, "extra": extra, "cats": c.Exp.Cats, "total": round2(c.Exp.Total), "notes": c.Exp.Notes}
	}
	pages := map[string]float64{}
	for _, r := range c.InMonth {
		pages[r.Page+" "+ts.Date(r.T)] += r.Net
	}
	rp := map[string]float64{}
	for k, v := range pages {
		rp[k] = money.RoundN(v, 3)
	}
	out["pages"] = rp
	reqs := []map[string]any{}
	for _, q := range c.Cover.Reqs {
		var tr any = nil
		if q.Pre {
			tr = "pre"
		} else if q.Transfer != nil {
			tr = q.Transfer.N
		}
		reqs = append(reqs, map[string]any{"page": q.Page, "t": iso(q.T), "amount": q.Amount, "method": q.Method,
			"avail": money.RoundN(q.Avail, 3), "transfer": tr})
	}
	out["requests"] = reqs
	trs := []map[string]any{}
	for _, m := range sortedTransferModels(c.Cover) {
		for _, tr := range c.Cover.Transfers[m] {
			if tr.HasK {
				trs = append(trs, map[string]any{"n": tr.N, "model": m, "cut": iso(tr.Cut), "amount": tr.Amount, "est": round2(tr.Est),
					"k": money.RoundN(tr.K, 6), "month": round2(float64(tr.ByMonth[c.Month] * tr.K))})
			}
		}
	}
	out["transfers"] = trs
	return out
}
