package journal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
)

// Context is what the mapping of one row needs to know about the rest of the journal.
type Context struct {
	Models      map[string]bool  // model names allowed in the «Модель» column (settings.models); empty = any single name
	AccrualFrom string           // first day accounted by accrual (pages and people), e.g. 2026-09-01
	FirstOfDeal map[string]int   // deal id → number of its first row in (date, num) order
	Overrides   map[int]Override // rows whose money side the generic mapping cannot express
	Errors      map[int]string   // rows whose tags did not parse (history): mapped without tags
}

// Override splits the money side of a row over several accounts (e.g. one fee charged to two cards).
type Override struct {
	Money []Split `json:"money"`
	Note  string  `json:"note,omitempty"`
}

// Split is one account and amount of an override.
type Split struct {
	Account string `json:"account"`
	Amount  string `json:"amount"`
}

var (
	cardRe     = regexp.MustCompile(`^Карта CinCin (\d{4})$`)
	walletRe   = regexp.MustCompile(`^CinCin \(кошел[её]к\)$`)
	cardNameRe = regexp.MustCompile(`^CinCin •(\d{4})$`)
	arrowRe    = regexp.MustCompile(`^(.+?) → (.+)$`)
)

// MethodAccount maps the «Метод» column to the money account the row moves.
func MethodAccount(method, date string) (string, error) {
	switch {
	case method == "Удержано из вывода":
		// The model kept this from her transfer; the transfer row is written gross with a Bybit method,
		// so the withheld part leaves Bybit here (rules: «входят в Bybit-мост со знаком минус»).
		return "bybit", nil
	case method == "Кошелёк CinCin":
		return "cincin_wallet", nil
	case cardRe.MatchString(method):
		return "card:" + cardRe.FindStringSubmatch(method)[1], nil
	case strings.HasPrefix(method, "Зачёт"):
		return "clearing:offset", nil
	case strings.HasPrefix(method, "BTC (Bybit)") && strings.Contains(method, "Convert USDT"):
		return "bybit", nil
	case strings.HasPrefix(method, "BTC (Bybit)"):
		return "bybit:btc", nil
	case method == "—":
		if date < fullLedger {
			return "cash:unknown", nil
		}
		return "", fmt.Errorf("метод «—» после %s", fullLedger)
	case strings.Contains(method, "Bybit") || strings.HasPrefix(method, "USDT") || strings.HasPrefix(method, "USDC") ||
		strings.HasPrefix(method, "Internal Transfer") || strings.HasPrefix(method, "P2P"):
		return "bybit", nil
	}
	return "", fmt.Errorf("неизвестный метод «%s»", method)
}

// PlaceAccount maps a side of an internal transfer («Bybit → CinCin •1111») or a starting balance.
func PlaceAccount(name string) (string, error) {
	switch {
	case name == "Bybit":
		return "bybit", nil
	case walletRe.MatchString(name):
		return "cincin_wallet", nil
	case cardNameRe.MatchString(name):
		return "card:" + cardNameRe.FindStringSubmatch(name)[1], nil
	}
	return "", fmt.Errorf("неизвестный счёт «%s»", name)
}

var expenseAccounts = map[string]string{
	"КМ зарплата": "expense:km_salary", "Чаттер зарплата": "expense:chatter_salary", "Owner зарплата": "expense:owner_salary",
	"Owner draw": "owner:draw", "Owner вливание": "owner:injection", "Подрядчик": "expense:contractor",
	"Трафик": "expense:traffic", "SFS": "expense:sfs", "YouTube": "expense:youtube", "Инструменты": "expense:tools",
	"AI и разработка": "expense:ai_dev", "Комиссии": "expense:fees", "Прочее": "expense:other",
}

func modelSlug(r Row, c *Context) (string, error) {
	ok := c.Models[r.Model]
	if len(c.Models) == 0 {
		ok = r.Model != "" && r.Model != "—" && !strings.ContainsAny(r.Model, ", ")
	}
	if !ok {
		return "", fmt.Errorf("модель «%s» — нужна одна модель из settings.models", r.Model)
	}
	return ledger.Slug(r.Model), nil
}

// Entries maps a row to its balanced entries (none for a cancelled or pending row).
func Entries(r Row, t Tags, c *Context) ([]ledger.Entry, error) {
	if r.Status != "paid" {
		return nil, nil
	}
	amount, err := money.Parse(r.Amount)
	if err != nil {
		return nil, err
	}
	main := ledger.Entry{EffectiveAt: r.Date, Kind: "document", Memo: fmt.Sprintf("#%d %s · %s", r.Num, r.Category, r.Counterparty)}
	cur := r.Currency
	moneySide := func(sign money.Micros) ([]ledger.Line, error) {
		if o, ok := c.Overrides[r.Num]; ok {
			var lines []ledger.Line
			var sum money.Micros
			for _, s := range o.Money {
				v, err := money.Parse(s.Amount)
				if err != nil {
					return nil, err
				}
				sum += v
				lines = append(lines, ledger.Line{Account: s.Account, Amount: sign * v, Currency: cur})
			}
			if sum != amount {
				return nil, fmt.Errorf("разбивка #%d даёт %s вместо %s", r.Num, sum, amount)
			}
			return lines, nil
		}
		acct, err := MethodAccount(r.Method, r.Date)
		if err != nil {
			return nil, err
		}
		return []ledger.Line{{Account: acct, Amount: sign * amount, Currency: cur, Orig: r.Amount}}, nil
	}
	accrual := c.AccrualFrom != "" && r.Date >= c.AccrualFrom
	var extra []ledger.Entry
	switch r.Type {
	case "transfer":
		switch r.Category {
		case "Внутренний перевод":
			m := arrowRe.FindStringSubmatch(r.Counterparty)
			if m == nil {
				return nil, fmt.Errorf("перевод без «откуда → куда»: «%s»", r.Counterparty)
			}
			from, err := PlaceAccount(strings.TrimSpace(m[1]))
			if err != nil {
				return nil, err
			}
			to, err := PlaceAccount(strings.TrimSpace(m[2]))
			if err != nil {
				return nil, err
			}
			main.Lines = []ledger.Line{{Account: to, Amount: amount, Currency: cur}, {Account: from, Amount: -amount, Currency: cur}}
		case "Остаток на старте":
			acct, err := PlaceAccount(r.Counterparty)
			if err != nil {
				return nil, err
			}
			main.Kind = "document:opening"
			main.Lines = []ledger.Line{{Account: acct, Amount: amount, Currency: cur}, {Account: "opening", Amount: -amount}}
		default:
			return nil, fmt.Errorf("перевод с категорией «%s»", r.Category)
		}
	case "income":
		credit := ""
		switch r.Category {
		case "Платформы":
			if accrual {
				credit = "suspense:accrual" // resolved against pages and transfers (PR9)
			} else {
				s, err := modelSlug(r, c)
				if err != nil {
					return nil, err
				}
				credit = "income:platforms:" + s
			}
		case "Custom":
			credit = "income:custom"
		case "Рефералы":
			credit = "income:referrals"
		case "Owner вливание":
			credit = "owner:injection"
		case "Возврат расхода":
			credit = "expense_refund"
			if t.BeforeAug {
				credit = "opening" // refund of a cost before full bookkeeping: not this month's result
			}
		default:
			return nil, fmt.Errorf("доход с категорией «%s»", r.Category)
		}
		lines, err := moneySide(1)
		if err != nil {
			return nil, err
		}
		share := money.Micros(0)
		if t.Deal != nil && c.FirstOfDeal[t.Deal.ID] == r.Num && !notOff[r.Category] {
			share = money.FromFloat(t.Deal.Share)
		}
		if share != 0 {
			s, err := modelSlug(r, c)
			if err != nil {
				return nil, err
			}
			lines = append(lines, ledger.Line{Account: "payable:" + s, Amount: -share, Memo: "доля модели по сделке " + t.Deal.ID})
		}
		if rest := amount - share; rest != 0 {
			lines = append(lines, ledger.Line{Account: credit, Amount: -rest, Currency: cur})
		}
		main.Lines = lines
	case "expense":
		debit, ok := expenseAccounts[r.Category]
		if r.Category == "Доля модели" {
			s, err := modelSlug(r, c)
			if err != nil {
				return nil, err
			}
			debit, ok = "payable:"+s, true // the model's own share, owed when the money arrived
			if t.AboveShare {
				debit = "expense:model_share"
			}
		}
		if !ok {
			return nil, fmt.Errorf("расход с категорией «%s»", r.Category)
		}
		if t.InsideShare && r.Category != "Доля модели" {
			s, err := modelSlug(r, c)
			if err != nil {
				return nil, err
			}
			debit = "payable:" + s
		}
		if r.Category == "Комиссии" && r.Method == "Удержано из вывода" && accrual {
			debit = "suspense:accrual" // the route's own fee is inside the route rate
		}
		credit, err := moneySide(-1)
		if err != nil {
			return nil, err
		}
		var debits []ledger.Line
		rest := amount
		if r.Category == "Чаттер зарплата" && accrual {
			above := money.Micros(0)
			if t.AboveTable != nil {
				above = money.FromFloat(t.AboveTable.Amount)
			}
			if table := amount - above; table != 0 {
				debits = append(debits, ledger.Line{Account: "suspense:accrual", Amount: table, Memo: "выплата по таблице"})
			}
			if above != 0 {
				if t.AboveTable.Period != "" && t.AboveTable.Period != r.Date[:7] {
					debits = append(debits, ledger.Line{Account: "clearing:period", Amount: above, Memo: "сверх таблицы за " + t.AboveTable.Period})
					extra = append(extra, ledger.Entry{EffectiveAt: t.AboveTable.Period + "-01", Kind: "document:period",
						Memo:  fmt.Sprintf("#%d сверх таблицы за %s", r.Num, t.AboveTable.Period),
						Lines: []ledger.Line{{Account: "expense:chatter_salary", Amount: above}, {Account: "clearing:period", Amount: -above}}})
				} else {
					debits = append(debits, ledger.Line{Account: "expense:chatter_salary", Amount: above, Memo: "сверх таблицы"})
				}
			}
			rest = 0
		}
		if t.Prepaid != nil && rest != 0 {
			months := t.Prepaid.Months()
			prepaid := fmt.Sprintf("prepaid:doc-%d", r.Num)
			debits = append(debits, ledger.Line{Account: prepaid, Amount: rest, Memo: "предоплата " + t.Prepaid.From + ".." + t.Prepaid.To})
			part := rest / money.Micros(len(months))
			left := rest
			for i, m := range months {
				v := part
				if i == len(months)-1 {
					v = left
				}
				left -= v
				extra = append(extra, ledger.Entry{EffectiveAt: m + "-01", Kind: "document:prepaid",
					Memo:  fmt.Sprintf("#%d предоплата, доля %s", r.Num, m),
					Lines: []ledger.Line{{Account: debit, Amount: v}, {Account: prepaid, Amount: -v}}})
			}
			rest = 0
		}
		for _, fm := range t.ForMonth {
			v := money.FromFloat(fm.Amount)
			if v == 0 || rest == 0 {
				continue
			}
			rest -= v
			debits = append(debits, ledger.Line{Account: "clearing:period", Amount: v, Memo: "за " + fm.Month})
			extra = append(extra, ledger.Entry{EffectiveAt: fm.Month + "-01", Kind: "document:period",
				Memo:  fmt.Sprintf("#%d за %s", r.Num, fm.Month),
				Lines: []ledger.Line{{Account: debit, Amount: v}, {Account: "clearing:period", Amount: -v}}})
		}
		if rest != 0 {
			debits = append(debits, ledger.Line{Account: debit, Amount: rest, Currency: cur})
		}
		main.Lines = append(debits, credit...)
	}
	out := append([]ledger.Entry{main}, extra...)
	for i := range out {
		out[i].Lines = mergeLines(out[i].Lines)
	}
	return out, nil
}

// mergeLines sums lines of one account and drops zeros, so equal postings compare equal.
func mergeLines(lines []ledger.Line) []ledger.Line {
	var out []ledger.Line
	idx := map[string]int{}
	for _, l := range lines {
		if i, ok := idx[l.Account]; ok {
			out[i].Amount += l.Amount
			continue
		}
		idx[l.Account] = len(out)
		out = append(out, l)
	}
	kept := out[:0]
	for _, l := range out {
		if l.Amount != 0 {
			kept = append(kept, l)
		}
	}
	return kept
}

// Signature identifies an entry by date and lines, for comparing what is posted with what should be.
func Signature(e ledger.Entry) string {
	parts := make([]string, 0, len(e.Lines))
	for _, l := range e.Lines {
		parts = append(parts, fmt.Sprintf("%s=%d", l.Account, int64(l.Amount)))
	}
	sort.Strings(parts)
	return e.EffectiveAt + "|" + e.Kind + "|" + strings.Join(parts, ",")
}
