package month

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// RefreshReport says what a refresh did.
type RefreshReport struct {
	AsOf     string   `json:"asOf"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
	Pages    int      `json:"pages"`
	Parts    int      `json:"parts"`
	Requests int      `json:"requests"`
	Transfer int      `json:"transfers"`
	Posted   int      `json:"posted"`
	Reversed int      `json:"reversed"`
	Same     int      `json:"unchanged"`
	Notes    []string `json:"notes,omitempty"`
}

// Refresh recomputes the coverage at asOf, replaces the derived tables and reconciles the entries of
// money in transit in one transaction. When the hub did not prove its data, the run is recorded as
// unknown, the derived tables keep their last values and nothing is posted.
func Refresh(ctx context.Context, tx *sql.Tx, asOf time.Time, now time.Time) (RefreshReport, error) {
	rep := RefreshReport{AsOf: ts.Format(asOf), Status: "ok"}
	cfg, err := LoadConfig(ctx, tx)
	if err != nil {
		return rep, err
	}
	rows, err := JournalRows(ctx, tx, "")
	if err != nil {
		return rep, err
	}
	src := DBSource{Ctx: ctx, Q: tx}
	cv, err := BuildCover(cfg, src, rows, asOf)
	if err != nil {
		var f Fail
		if !hub.IsRefusal(err) && !errors.As(err, &f) {
			return rep, err
		}
		rep.Status, rep.Reason = "unknown", err.Error()
		_, derr := tx.ExecContext(ctx, `INSERT INTO coverage_runs(as_of, started_at, finished_at, status, reason)
			VALUES (?, ?, ?, 'unknown', ?)`, ts.Format(asOf), ts.Format(now), ts.Format(time.Now()), err.Error())
		return rep, derr
	}
	rep.Notes = cv.Notes
	if err := writeDerived(ctx, tx, cv, &rep); err != nil {
		return rep, err
	}
	accrualFrom := ""
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'accrual_from'`).Scan(&accrualFrom); err != nil {
		return rep, fmt.Errorf("settings.accrual_from: %w", err)
	}
	want, err := DesiredEntries(cv, accrualFrom)
	if err != nil {
		return rep, err
	}
	if err := reconcileEntries(ctx, tx, want, now, &rep); err != nil {
		return rep, err
	}
	notes, _ := json.Marshal(cv.Notes)
	_, err = tx.ExecContext(ctx, `INSERT INTO coverage_runs(as_of, started_at, finished_at, status, notes) VALUES (?, ?, ?, 'ok', ?)`,
		ts.Format(asOf), ts.Format(now), ts.Format(time.Now()), string(notes))
	return rep, err
}

func partState(cv *Cover, p string, r *Payment, part Part, free float64) (string, *Transfer) {
	if part.Anchor {
		return "anchor", nil
	}
	if part.Req != nil {
		if part.Req.Pre {
			return "pre", nil
		}
		t := part.Req.Transfer
		if t != nil && len(t.Bad) == 0 && t.HasK {
			return "arrived", t
		}
		return "riding", t
	}
	if r.Avail.After(cv.AsOf) {
		return "hold", nil
	}
	if m := cv.Cfg.Routes[p].Min; m != 0 && free < m {
		return "stuck", nil
	}
	return "page", nil
}

func freeOf(cv *Cover, p string) float64 {
	free := 0.0
	for _, r := range cv.Rows[p] {
		if !r.Avail.After(cv.AsOf) {
			for _, part := range r.Parts {
				if part.Req == nil && !part.Anchor {
					free += part.Amt
				}
			}
		}
	}
	return free
}

func writeDerived(ctx context.Context, tx *sql.Tx, cv *Cover, rep *RefreshReport) error {
	for _, t := range []string{"coverage_pages", "coverage_parts", "coverage_requests", "coverage_transfers"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	c := cv.Cfg
	asOf := ts.Format(cv.AsOf)
	for _, p := range cv.PageOrder {
		free := freeOf(cv, p)
		var holdN, hold, availN, avail, stuck, ridingN, riding float64
		for _, r := range cv.Rows[p] {
			for _, part := range r.Parts {
				state, t := partState(cv, p, r, part, free)
				rate := c.RateOf(p, part.Req)
				est := float64(part.Amt * rate * c.Share)
				switch state {
				case "hold":
					holdN, hold = holdN+part.Amt, hold+est
				case "page":
					availN, avail = availN+part.Amt, avail+est
				case "stuck":
					availN, stuck = availN+part.Amt, stuck+est
				case "riding":
					ridingN, riding = ridingN+part.Amt, riding+est
				}
				var reqKey, tnum any
				if part.Req != nil {
					reqKey = part.Req.Key
				}
				if t != nil {
					tnum = t.N
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO coverage_parts(page_label, payment_key, paid_at, avail_at, net, part,
					state, request_key, transfer_num, rate, est) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					p, r.Key, ts.FormatMilli(r.T), ts.FormatMilli(r.Avail), r.Net, part.Amt, state, reqKey, tnum, rate, est); err != nil {
					return err
				}
				rep.Parts++
			}
		}
		rt := c.Routes[p]
		if _, err := tx.ExecContext(ctx, `INSERT INTO coverage_pages(page_label, model, channel, as_of, hold_net, hold, avail_net,
			available, stuck, riding_net, riding, min_payout) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p, rt.Model, rt.Channel, asOf, holdN, hold, availN, avail, stuck, ridingN, riding, rt.Min); err != nil {
			return err
		}
		rep.Pages++
	}
	for _, q := range cv.Reqs {
		var tnum any
		if q.Transfer != nil {
			tnum = q.Transfer.N
		}
		pre := 0
		if q.Pre {
			pre = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO coverage_requests(page_label, key, requested_at, amount, method, status, avail,
			excess, transfer_num, pre) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			q.Page, q.Key, ts.FormatMilli(q.T), q.Amount, q.Method, q.Status, q.Avail, q.Excess, tnum, pre); err != nil {
			return err
		}
		rep.Requests++
	}
	for _, m := range sortedTransferModels(cv) {
		for _, t := range cv.Transfers[m] {
			deps, _ := json.Marshal(t.Deps)
			bad, _ := json.Marshal(t.Bad)
			if bad == nil || string(bad) == "null" {
				bad = []byte("[]")
			}
			if string(deps) == "null" {
				deps = []byte("[]")
			}
			by, _ := json.Marshal(t.ByMonth)
			var k any
			if t.HasK {
				k = t.K
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO coverage_transfers(num, model, date, cut, amount, est, k, held, fee, deposits, bad, by_month)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.N, m, t.Date, ts.Format(t.Cut), t.Amount, t.Est, k, t.Held, t.Fee,
				string(deps), string(bad), string(by)); err != nil {
				return err
			}
			rep.Transfer++
		}
	}
	return nil
}

// Desired is one entry the current coverage calls for, under a stable logical key.
type Desired struct {
	Key   string
	Entry ledger.Entry
}

type dayKey struct{ page, date string }

// DesiredEntries turns the coverage into entries, aggregated per page and day:
//
//	payment since accrualFrom:  Dr page:<p>:hold      Cr income:platforms:<p>   estimate of the day's payments
//	end of hold:                Dr page:<p>:available Cr page:<p>:hold
//	request:                    Dr model:<m>:due      Cr page:<p>:available
//	transfer (from the journal's suspense):
//	                            Dr suspense:accrual   Cr model:<m>:due (covered estimate since accrualFrom)
//	                                                  Cr income:platforms:<m> (covered earnings before accrualFrom, at actual k)
//	                                                  Cr payable:<m> (the model's share where the agency receives 100%)
//	                                                  Cr income:true_up:<m> (fact − estimate, per month of the payments)
func DesiredEntries(cv *Cover, accrualFrom string) ([]Desired, error) {
	c := cv.Cfg
	accrual := map[dayKey]float64{}
	available := map[dayKey]float64{}
	type reqAgg struct {
		q   *Request
		est float64
	}
	reqs := map[*Request]*reqAgg{}
	var reqOrder []*Request
	type monthly struct {
		est float64 // covered estimate since accrualFrom
		pre float64 // covered estimate of earlier payments
	}
	byTransfer := map[*Transfer]map[string]*monthly{}
	for _, p := range cv.PageOrder {
		for _, r := range cv.Rows[p] {
			day := ts.Date(r.T)
			since := day >= accrualFrom
			month := ts.Month(r.T)
			for _, part := range r.Parts {
				if part.Anchor || (part.Req != nil && part.Req.Pre) {
					continue
				}
				est := float64(part.Amt * c.RateOf(p, part.Req) * c.Share)
				if since {
					accrual[dayKey{p, day}] += est
					if !r.Avail.After(cv.AsOf) || part.Req != nil {
						available[dayKey{p, ts.Date(r.Avail)}] += est
					}
				}
				if part.Req == nil {
					continue
				}
				if since {
					a := reqs[part.Req]
					if a == nil {
						a = &reqAgg{q: part.Req}
						reqs[part.Req] = a
						reqOrder = append(reqOrder, part.Req)
					}
					a.est += est
				}
				if t := part.Req.Transfer; t != nil {
					if byTransfer[t] == nil {
						byTransfer[t] = map[string]*monthly{}
					}
					mm := byTransfer[t][month]
					if mm == nil {
						mm = &monthly{}
						byTransfer[t][month] = mm
					}
					if since {
						mm.est += est
					} else {
						mm.pre += est
					}
				}
			}
		}
	}
	var out []Desired
	add := func(key, effective, kind, memo string, lines ...ledger.Line) {
		kept := lines[:0]
		for _, l := range lines {
			if l.Amount != 0 {
				kept = append(kept, l)
			}
		}
		if len(kept) < 2 {
			return
		}
		out = append(out, Desired{Key: key, Entry: ledger.Entry{EffectiveAt: effective, Kind: kind, Memo: memo, Author: "transit", Lines: kept}})
	}
	for _, k := range sortedDayKeys(accrual) {
		v := money.FromFloat(accrual[k])
		add("transit:accrual:"+k.page+":"+k.date, k.date, "transit:accrual", "начисление за платежи дня: "+k.page,
			ledger.Line{Account: "page:" + k.page + ":hold", Amount: v}, ledger.Line{Account: "income:platforms:" + k.page, Amount: -v})
	}
	for _, k := range sortedDayKeys(available) {
		v := money.FromFloat(available[k])
		add("transit:available:"+k.page+":"+k.date, k.date, "transit:available", "конец холда: "+k.page,
			ledger.Line{Account: "page:" + k.page + ":available", Amount: v}, ledger.Line{Account: "page:" + k.page + ":hold", Amount: -v})
	}
	for _, q := range reqOrder {
		a := reqs[q]
		v := money.FromFloat(a.est)
		m := ledger.Slug(c.Routes[q.Page].Model)
		add("transit:request:"+q.Page+":"+q.Key, ts.Date(q.T), "transit:request", "заявка на вывод: "+q.Page,
			ledger.Line{Account: "model:" + m + ":due", Amount: v}, ledger.Line{Account: "page:" + q.Page + ":available", Amount: -v})
	}
	for _, m := range sortedTransferModels(cv) {
		for _, t := range cv.Transfers[m] {
			// A transfer dated before accrualFrom is cash era: the journal row already went to income.
			// A transfer not accepted (k out of norm, a request above the net) is not resolved: the card
			// keeps its payments as «едет» by estimate, so the money stays at the model and on suspense.
			if !t.HasK || t.Est == 0 || t.Date < accrualFrom || len(t.Bad) > 0 {
				continue
			}
			slug := ledger.Slug(m)
			mm := byTransfer[t]
			months := make([]string, 0, len(mm))
			for x := range mm {
				months = append(months, x)
			}
			sort.Strings(months)
			// Fact counted for the agency: the whole transfer for a model route, the agency's share of
			// the credited sum for a route that pays the agency 100%.
			agency := t.Amount
			gross := t.Amount
			share := 0.0
			if c.PayeeOf(m) == "agency" {
				gross = t.Amount - t.Fee
				agency = float64(c.Share * gross)
				share = gross - agency
			}
			var sumDue, sumPre, sumTrue money.Micros
			lines := []ledger.Line{}
			for _, x := range months {
				sumDue += money.FromFloat(mm[x].est)
				sumPre += money.FromFloat(float64(mm[x].pre * t.K))
			}
			total := money.FromFloat(gross)
			shareM := money.FromFloat(share)
			agencyM := total - shareM
			sumTrue = agencyM - sumDue - sumPre
			lines = append(lines, ledger.Line{Account: "suspense:accrual", Amount: total, Memo: fmt.Sprintf("перевод #%d", t.N)})
			if sumDue != 0 {
				lines = append(lines, ledger.Line{Account: "model:" + slug + ":due", Amount: -sumDue, Memo: "покрытая оценка"})
			}
			if sumPre != 0 {
				lines = append(lines, ledger.Line{Account: "income:platforms:" + slug, Amount: -sumPre, Memo: "заработок до начала учёта по начислению"})
			}
			if shareM != 0 {
				lines = append(lines, ledger.Line{Account: "payable:" + slug, Amount: -shareM, Memo: "доля модели"})
			}
			// The true-up of each month of payments lands in that month, so the month's income equals its
			// estimate × k; the last month takes the rounding remainder.
			used := money.Micros(0)
			parts := 0
			for _, x := range months {
				if mm[x].est != 0 {
					parts++
				}
			}
			seen := 0
			for _, x := range months {
				if mm[x].est == 0 {
					continue
				}
				seen++
				v := money.FromFloat(float64(mm[x].est * (t.K - 1)))
				if seen == parts {
					v = sumTrue - used
				}
				used += v
				eff := t.Date
				if x != t.Date[:7] {
					_, m1, err := ts.MonthStart(x)
					if err != nil {
						return nil, err
					}
					eff = ts.Date(m1.AddDate(0, 0, -1))
				}
				if v == 0 {
					continue
				}
				if eff == t.Date {
					lines = append(lines, ledger.Line{Account: "income:true_up:" + slug, Amount: -v, Memo: "поправка k за " + x})
					continue
				}
				add(fmt.Sprintf("transit:trueup:%d:%s", t.N, x), eff, "transit:trueup", fmt.Sprintf("поправка k перевода #%d за %s", t.N, x),
					ledger.Line{Account: "income:true_up:" + slug, Amount: -v}, ledger.Line{Account: "clearing:period", Amount: v})
				lines = append(lines, ledger.Line{Account: "clearing:period", Amount: -v, Memo: "поправка k за " + x})
			}
			if parts == 0 && sumTrue != 0 {
				lines = append(lines, ledger.Line{Account: "income:true_up:" + slug, Amount: -sumTrue, Memo: "поправка k"})
			}
			merged := mergeByAccount(lines)
			if balance(merged) != 0 {
				return nil, fmt.Errorf("перевод #%d: проводка не сходится на %s", t.N, balance(merged))
			}
			add(fmt.Sprintf("transit:transfer:%d", t.N), t.Date, "transit:transfer", fmt.Sprintf("перевод #%d: покрытие и k %.4f", t.N, t.K), merged...)
		}
	}
	return out, nil
}

func balance(ls []ledger.Line) money.Micros {
	var s money.Micros
	for _, l := range ls {
		s += l.Amount
	}
	return s
}

func mergeByAccount(ls []ledger.Line) []ledger.Line {
	idx := map[string]int{}
	var out []ledger.Line
	for _, l := range ls {
		if i, ok := idx[l.Account]; ok {
			out[i].Amount += l.Amount
			continue
		}
		idx[l.Account] = len(out)
		out = append(out, l)
	}
	return out
}

func sortedDayKeys(m map[dayKey]float64) []dayKey {
	out := make([]dayKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].page != out[j].page {
			return out[i].page < out[j].page
		}
		return out[i].date < out[j].date
	})
	return out
}

func signature(effective string, ls []ledger.Line) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		parts = append(parts, l.Account+"="+strconv.FormatInt(int64(l.Amount), 10))
	}
	sort.Strings(parts)
	return effective + "|" + strings.Join(parts, ",")
}

type activeEntry struct {
	id  int64
	sig string
}

// reconcileEntries compares the desired entries with the posted ones by logical key: unchanged ones
// stay, changed ones are reversed and posted again under the next revision, vanished ones are reversed.
func reconcileEntries(ctx context.Context, tx *sql.Tx, want []Desired, now time.Time, rep *RefreshReport) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.source_key, e.effective_at, l.account, l.amount,
		EXISTS (SELECT 1 FROM entries r WHERE r.reverses_id = e.id)
		FROM entries e JOIN lines l ON l.entry_id = e.id
		WHERE e.source_key LIKE 'transit:%' ORDER BY e.id, l.id`)
	if err != nil {
		return err
	}
	type acc struct {
		id       int64
		key, eff string
		reversed bool
		ls       []ledger.Line
	}
	byID := map[int64]*acc{}
	var ids []int64
	for rows.Next() {
		var id, amt int64
		var key, eff, account string
		var reversed bool
		if err := rows.Scan(&id, &key, &eff, &account, &amt, &reversed); err != nil {
			rows.Close()
			return err
		}
		a := byID[id]
		if a == nil {
			a = &acc{id: id, key: key, eff: eff, reversed: reversed}
			byID[id] = a
			ids = append(ids, id)
		}
		a.ls = append(a.ls, ledger.Line{Account: account, Amount: money.Micros(amt)})
	}
	rows.Close()
	active := map[string]activeEntry{}
	revision := map[string]int{}
	for _, id := range ids {
		a := byID[id]
		logical, rev := splitRevision(a.key)
		if rev > revision[logical] {
			revision[logical] = rev
		}
		if !a.reversed {
			active[logical] = activeEntry{id: id, sig: signature(a.eff, a.ls)}
		}
	}
	seen := map[string]bool{}
	for _, d := range want {
		seen[d.Key] = true
		sig := signature(d.Entry.EffectiveAt, d.Entry.Lines)
		cur, ok := active[d.Key]
		if ok && cur.sig == sig {
			rep.Same++
			continue
		}
		if ok {
			if _, err := ledger.Reverse(ctx, tx, cur.id, ledger.Reversal{Author: "transit", Memo: "сторно: покрытие изменилось: " + d.Key}, now); err != nil {
				return err
			}
			rep.Reversed++
		}
		e := d.Entry
		e.SourceKey = fmt.Sprintf("%s:r%d", d.Key, revision[d.Key]+1)
		revision[d.Key]++
		if _, err := ledger.Post(ctx, tx, e, now); err != nil {
			return fmt.Errorf("%s: %w", d.Key, err)
		}
		rep.Posted++
	}
	var vanished []string
	for k := range active {
		if !seen[k] {
			vanished = append(vanished, k)
		}
	}
	sort.Strings(vanished)
	for _, k := range vanished {
		if _, err := ledger.Reverse(ctx, tx, active[k].id, ledger.Reversal{Author: "transit", Memo: "сторно: больше не нужно: " + k}, now); err != nil {
			return err
		}
		rep.Reversed++
	}
	return nil
}

func splitRevision(key string) (string, int) {
	i := strings.LastIndex(key, ":r")
	if i < 0 {
		return key, 0
	}
	n, err := strconv.Atoi(key[i+2:])
	if err != nil {
		return key, 0
	}
	return key[:i], n
}

// LiveAsOf is the latest cut every routed page is proven to by the last hub sync.
func LiveAsOf(ctx context.Context, q ledger.Querier, cfg Config) (time.Time, error) {
	sts, err := hub.Statuses(ctx, q)
	if err != nil {
		return time.Time{}, err
	}
	var cut time.Time
	for _, s := range sts {
		rt, routed := cfg.Routes[s.Page]
		if !routed {
			continue
		}
		need := s.Dataset == "transactions" || (s.Dataset == "payouts" && rt.Channel == "Fansly") ||
			(s.Dataset == "ofapi_payout_requests" && rt.Channel != "Fansly")
		if !need {
			continue
		}
		if s.Status != "ok" || s.To == nil {
			return time.Time{}, hub.Refusef("хаб %s %s не доказан: %s", s.Page, s.Dataset, strings.Join(s.Reasons, "; "))
		}
		if cut.IsZero() || s.To.Before(cut) {
			cut = *s.To
		}
	}
	if cut.IsZero() {
		return cut, hub.Refusef("хаб: нет доказанных выгрузок (ledger hub sync)")
	}
	return cut, nil
}
