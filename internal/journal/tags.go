package journal

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// Tags are the {…} marks of a note, parsed with the strictness of the monthly card script: an unknown,
// broken, repeated or contradictory tag is an error, so a typo never silently switches a rule off.
type Tags struct {
	ShareZero   bool       `json:"доля 0,omitempty"`
	InsideShare bool       `json:"внутри доли,omitempty"`
	AboveShare  bool       `json:"сверх доли,omitempty"`
	BeforeAug   bool       `json:"до 01.08,omitempty"`
	Transfer    *Transfer  `json:"перевод,omitempty"`
	Deal        *Deal      `json:"сделка,omitempty"`
	AboveTable  *Above     `json:"сверх таблицы,omitempty"`
	Prepaid     *Prepaid   `json:"предоплата,omitempty"`
	ForMonth    []ForMonth `json:"за,omitempty"`
}

// Transfer is {перевод: срез ГГГГ-ММ-ДД ЧЧ:ММ; депозиты: a+b; удержано: x; комиссия: y}.
type Transfer struct {
	Cut      string    `json:"срез"` // MSK, minute precision as written
	Deposits []float64 `json:"депозиты,omitempty"`
	Held     []float64 `json:"удержано,omitempty"`
	Fee      []float64 `json:"комиссия,omitempty"`
}

// CutTime is the cut as a time (MSK).
func (t Transfer) CutTime() time.Time {
	v, _ := time.ParseInLocation("2006-01-02 15:04", t.Cut, ts.MSK)
	return v
}

// Deal is {сделка: <id>; цена N; доля M}.
type Deal struct {
	ID    string  `json:"id"`
	Price float64 `json:"цена"`
	Share float64 `json:"доля"`
}

// Above is {сверх таблицы: N[; период ГГГГ-ММ]}.
type Above struct {
	Amount float64 `json:"сумма"`
	Period string  `json:"период,omitempty"`
}

// Prepaid is {предоплата ГГГГ-ММ..ГГГГ-ММ}.
type Prepaid struct {
	From string `json:"с"`
	To   string `json:"по"`
}

// ForMonth is {за ГГГГ-ММ: N}.
type ForMonth struct {
	Month  string  `json:"месяц"`
	Amount float64 `json:"сумма"`
}

// Months lists the months of the prepayment, inclusive.
func (p Prepaid) Months() []string {
	var out []string
	t, _ := time.Parse("2006-01", p.From)
	end, _ := time.Parse("2006-01", p.To)
	for !t.After(end) {
		out = append(out, t.Format("2006-01"))
		t = t.AddDate(0, 1, 0)
	}
	return out
}

// JSON renders the tags for documents.tags.
func (t Tags) JSON() json.RawMessage {
	b, _ := json.Marshal(t)
	return b
}

const numRe = `\d+(?:\.\d+)?`
const monRe = `\d{4}-(?:0[1-9]|1[0-2])`

var (
	tagBody      = regexp.MustCompile(`\{([^{}]*)\}`)
	transferRe   = regexp.MustCompile(`^перевод: (.+)$`)
	cutRe        = regexp.MustCompile(`^срез (\S+ \S+)$`)
	transferPart = regexp.MustCompile(`^(депозиты|удержано|комиссия): (` + numRe + `(?: ?\+ ?` + numRe + `)*)$`)
	plusRe       = regexp.MustCompile(` ?\+ ?`)
	dealRe       = regexp.MustCompile(`^сделка: ([^\s;]+); цена (` + numRe + `); доля (` + numRe + `)$`)
	aboveRe      = regexp.MustCompile(`^сверх таблицы: (` + numRe + `)(?:; период (` + monRe + `))?$`)
	prepaidRe    = regexp.MustCompile(`^предоплата (` + monRe + `)\.\.(` + monRe + `)$`)
	forMonthRe   = regexp.MustCompile(`^за (` + monRe + `): (` + numRe + `)$`)
)

func fl(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func short(s string) string {
	r := []rune(s)
	if len(r) > 90 {
		r = r[:90]
	}
	return string(r)
}

// ParseTags parses the tags of a note.
func ParseTags(note string) (Tags, error) {
	var t Tags
	if strings.ContainsAny(tagBody.ReplaceAllString(note, ""), "{}") {
		return t, fmt.Errorf("незакрытая или лишняя фигурная скобка: «%s»", short(note))
	}
	seen := map[string]bool{}
	for _, m := range tagBody.FindAllStringSubmatch(note, -1) {
		b := strings.Join(strings.Fields(m[1]), " ")
		kind := b
		switch {
		case b == "доля 0":
			t.ShareZero = true
		case b == "внутри доли":
			t.InsideShare = true
		case b == "сверх доли":
			t.AboveShare = true
		case b == "до 01.08":
			t.BeforeAug = true
		case transferRe.MatchString(b):
			kind = "перевод"
			tr := &Transfer{}
			got := map[string]bool{}
			for _, part := range strings.Split(transferRe.FindStringSubmatch(b)[1], ";") {
				part = strings.TrimSpace(part)
				var key string
				if p := cutRe.FindStringSubmatch(part); p != nil {
					key = "срез"
					if _, err := time.ParseInLocation("2006-01-02 15:04", p[1], ts.MSK); err != nil {
						return t, fmt.Errorf("не дата/время: «%s»", p[1])
					}
					if got[key] {
						return t, fmt.Errorf("тег перевода: «%s» дважды в {%s}", key, b)
					}
					tr.Cut = p[1]
				} else if p := transferPart.FindStringSubmatch(part); p != nil {
					key = p[1]
					var vals []float64
					for _, x := range plusRe.Split(p[2], -1) {
						vals = append(vals, fl(x))
					}
					prev := map[string][]float64{"депозиты": tr.Deposits, "удержано": tr.Held, "комиссия": tr.Fee}[key]
					if got[key] && len(prev) > 0 {
						return t, fmt.Errorf("тег перевода: «%s» дважды в {%s}", key, b)
					}
					switch key {
					case "депозиты":
						tr.Deposits = vals
					case "удержано":
						tr.Held = vals
					default:
						tr.Fee = vals
					}
				} else {
					return t, fmt.Errorf("тег перевода: не разобрал «%s» в {%s}", part, b)
				}
				got[key] = true
			}
			if tr.Cut == "" {
				return t, fmt.Errorf("тег перевода без среза: {%s}", b)
			}
			t.Transfer = tr
		case dealRe.MatchString(b):
			kind = "сделка"
			p := dealRe.FindStringSubmatch(b)
			t.Deal = &Deal{ID: p[1], Price: fl(p[2]), Share: fl(p[3])}
		case aboveRe.MatchString(b):
			kind = "сверх таблицы"
			p := aboveRe.FindStringSubmatch(b)
			t.AboveTable = &Above{Amount: fl(p[1]), Period: p[2]}
		case prepaidRe.MatchString(b):
			kind = "предоплата"
			p := prepaidRe.FindStringSubmatch(b)
			t.Prepaid = &Prepaid{From: p[1], To: p[2]}
		case forMonthRe.MatchString(b):
			p := forMonthRe.FindStringSubmatch(b)
			kind = "за " + p[1]
			t.ForMonth = append(t.ForMonth, ForMonth{Month: p[1], Amount: fl(p[2])})
		default:
			return t, fmt.Errorf("неизвестный или битый тег {%s}", b)
		}
		if seen[kind] {
			return t, fmt.Errorf("тег {%s…} дважды в одном примечании", kind)
		}
		seen[kind] = true
	}
	if (t.Deal != nil && t.Deal.Share > t.Deal.Price) || (t.Prepaid != nil && t.Prepaid.From >= t.Prepaid.To) {
		return t, fmt.Errorf("доля больше цены или период предоплаты не на 2+ месяца вперёд: «%s»", short(note))
	}
	if t.ShareZero && t.Deal != nil {
		return t, fmt.Errorf("противоречивые теги {доля 0} и {сделка} в одном примечании")
	}
	if t.InsideShare && t.AboveShare {
		return t, fmt.Errorf("противоречивые теги {внутри доли} и {сверх доли} в одном примечании")
	}
	sort.Slice(t.ForMonth, func(i, j int) bool { return t.ForMonth[i].Month < t.ForMonth[j].Month })
	return t, nil
}

// notOff are income categories that are not money past the platforms.
var notOff = map[string]bool{"Платформы": true, "Owner вливание": true, "Возврат расхода": true}

// RowTags parses the tags and checks that each fits its row, as the monthly card script does.
func RowTags(r Row) (Tags, error) {
	t, err := ParseTags(r.Note)
	if err != nil {
		return t, fmt.Errorf("#%d: %w", r.Num, err)
	}
	inc, exp := r.Type == "income", r.Type == "expense"
	check := func(kind string, ok bool) error {
		if !ok {
			return fmt.Errorf("#%d: тег {%s…} не подходит строке %s «%s»", r.Num, kind, r.Type, r.Category)
		}
		return nil
	}
	conds := []struct {
		on   bool
		kind string
		ok   bool
	}{
		{t.Transfer != nil, "перевод", inc && r.Category == "Платформы"},
		{t.BeforeAug, "до 01.08", inc && r.Category == "Возврат расхода"},
		{t.Deal != nil, "сделка", inc && !notOff[r.Category]},
		{t.ShareZero, "доля 0", inc && !notOff[r.Category]},
		{t.AboveTable != nil, "сверх таблицы", exp && r.Category == "Чаттер зарплата"},
		{t.InsideShare, "внутри доли", exp},
		{t.AboveShare, "сверх доли", exp},
		{t.Prepaid != nil, "предоплата", exp},
		{len(t.ForMonth) > 0, "за", exp},
	}
	for _, c := range conds {
		if c.on {
			if err := check(c.kind, c.ok); err != nil {
				return t, err
			}
		}
	}
	amt := fl(r.Amount)
	moved := 0.0
	for _, m := range t.ForMonth {
		moved += m.Amount
	}
	if (t.AboveTable != nil && t.AboveTable.Amount > amt+.005) || moved > amt+.005 {
		return t, fmt.Errorf("#%d: сумма в теге больше суммы строки %.2f", r.Num, amt)
	}
	text := strings.ToLower(tagBody.ReplaceAllString(r.Note, ""))
	if strings.Contains(text, "сверх таблиц") && t.AboveTable == nil {
		return t, fmt.Errorf("#%d: «сверх таблиц…» в примечании текстом без тега {сверх таблицы…} — разметь тегом", r.Num)
	}
	if strings.Contains(text, "сверх доли") && !t.AboveShare {
		return t, fmt.Errorf("#%d: «сверх доли…» в примечании текстом без тега {сверх доли…} — разметь тегом", r.Num)
	}
	return t, nil
}

// fullLedger is the start of complete bookkeeping; earlier rows are an incomplete period.
const fullLedger = "2026-08-01"

// Lint checks the whole journal like `month.py lint`: tags of every row, income past the platforms
// since 2026-08-01 tagged with a deal or {доля 0}, transfers since then tagged, deal terms consistent.
func Lint(rows []Row) []error {
	var errs []error
	tags := map[int]Tags{}
	for _, r := range rows {
		t, err := RowTags(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tags[r.Num] = t
		if r.Type == "income" && !notOff[r.Category] && r.Date >= fullLedger && t.Deal == nil && !t.ShareZero {
			errs = append(errs, fmt.Errorf("#%d: поступление «%s» без тега {сделка: …} или {доля 0}", r.Num, r.Category))
		}
	}
	if len(errs) > 0 {
		return errs
	}
	for _, r := range rows {
		if r.Status == "paid" && r.Type == "income" && r.Category == "Платформы" && r.Date >= fullLedger && tags[r.Num].Transfer == nil {
			errs = append(errs, fmt.Errorf("перевод #%d без тега {перевод: …}", r.Num))
		}
	}
	type terms struct {
		price, share float64
		model        string
	}
	deals := map[string]terms{}
	for _, r := range SortedByDate(rows) {
		if d := tags[r.Num].Deal; d != nil && r.Type == "income" && !notOff[r.Category] {
			cur := terms{d.Price, d.Share, r.Model}
			if prev, ok := deals[d.ID]; ok && prev != cur {
				errs = append(errs, fmt.Errorf("#%d: сделка «%s» с другими ценой/долей/моделью, чем в первой строке", r.Num, d.ID))
			}
			if _, ok := deals[d.ID]; !ok {
				deals[d.ID] = cur
			}
		}
	}
	return errs
}

// SortedByDate orders rows by (date, num), the order the monthly card script reads them in.
func SortedByDate(rows []Row) []Row {
	out := append([]Row(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Num < out[j].Num
	})
	return out
}
