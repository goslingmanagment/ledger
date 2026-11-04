package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The read API of the mirrors for the coverage of transfers (PR9) and the month card (PR10). Every
// function refuses (Refusal) when the hub did not prove the dataset for the asked window: the
// caller shows «неизвестно», never zero.

// proven checks that (page, ds) was proven by the last run over a window covering [from, to).
func proven(ctx context.Context, q ledger.Querier, page, ds string, from, to time.Time) (*SyncStatus, error) {
	st, err := loadStatus(ctx, q, page, ds)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, refusef("хаб %s %s: не выгружено (ledger hub sync)", page, ds)
	}
	if st.Status != "ok" {
		return nil, refusef("хаб %s %s не доказан на %s: %s", page, ds, st.CheckedAt, strings.Join(st.Reasons, "; "))
	}
	if st.From == nil || st.To == nil || from.Before(*st.From) || to.After(*st.To) {
		return nil, refusef("хаб %s %s: окно %s..%s вне доказанного %s..%s", page, ds, iso(from), iso(to), st.WindowFrom,
			st.WindowTo)
	}
	return st, nil
}

// Row is a fan payment with the moment it can be withdrawn (month.py Hub.rows).
type Row struct {
	Page     string    `json:"page"`
	Key      string    `json:"key"`
	At       time.Time `json:"at"`
	NetMills int64     `json:"netMills"`
	Type     string    `json:"type"`
	Ref      string    `json:"ref"`
	Decision string    `json:"decision,omitempty"` // приз or возврат from hub_exceptions
	Prize    bool      `json:"prize,omitempty"`
	Avail    time.Time `json:"avail"`
	EventID  int64     `json:"eventId"`
}

// Net is the row's net in micro-dollars.
func (r Row) Net() money.Micros { return money.Micros(r.NetMills * 1000) }

// Checked is what Rows returns: the rows and the notes about excluded ones.
type Checked struct {
	Rows  []Row    `json:"rows"`
	Notes []string `json:"notes,omitempty"`
}

// Rows returns the checked payments of a page over [from, to) like month.py Hub.rows: a row named in
// hub_exceptions must match it to the second (UTC), net ±0.005 and type; «приз» and «возврат» are
// available at once, «исключить» is dropped with a note; hold types are available after hold_days.
// Any other type without a decision, a non-USD row or a negative without «возврат» refuses the page.
// A page with a start in hub_exceptions has no rows before it, so the window starts there at most.
func Rows(ctx context.Context, q ledger.Querier, page string, from, to time.Time) (Checked, error) {
	var out Checked
	set, err := LoadSettings(ctx, q)
	if err != nil {
		return out, err
	}
	hold, err := set.Days("hold_days")
	if err != nil {
		return out, err
	}
	holdTypes, err := set.Set("hold_types")
	if err != nil {
		return out, err
	}
	ex, err := LoadExceptions(ctx, q)
	if err != nil {
		return out, err
	}
	a := from
	if st, ok := ex.Starts[page]; ok && st.After(a) {
		a = st
	}
	if !a.Before(to) {
		a = to
	}
	if _, err := proven(ctx, q, page, "transactions", a, to); err != nil {
		return out, err
	}
	rows, err := q.QueryContext(ctx, `SELECT key, occurred_at, net_mills, currency, type, ref, event_id, fields
		FROM hub_transactions WHERE page_label = ? AND occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at, key`,
		page, ts.FormatMilli(a), ts.FormatMilli(to))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	type raw struct {
		key, at, fields string // at: the column, for the window; the instant comes from fields
		net             sql.NullInt64
		cur, typ, ref   sql.NullString
		event           int64
	}
	var list []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.key, &r.at, &r.net, &r.cur, &r.typ, &r.ref, &r.event, &r.fields); err != nil {
			return out, err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	py := func(s sql.NullString) string { // how month.py prints a field that may be None
		if !s.Valid {
			return "None"
		}
		return s.String
	}
	for _, r := range list {
		if !r.net.Valid {
			return Checked{}, refusef("хаб %s: строка %s без поля netMills — ответ не по контракту", page, r.key)
		}
		var f struct {
			OccurredAt string `json:"occurredAt"`
		}
		_ = json.Unmarshal([]byte(r.fields), &f)
		t, err := parseHubTime(f.OccurredAt) // the hub's own instant, not the column's milliseconds
		if err != nil {
			return Checked{}, fmt.Errorf("hub_transactions %s/%s: occurredAt %q", page, r.key, f.OccurredAt)
		}
		t = t.UTC()
		net := float64(r.net.Int64) / 1000
		e, has := ex.Rows[RowRef{page, r.ref.String}]
		if !r.ref.Valid {
			has = false
		}
		if has {
			types := decisionTypes[e.Decision]
			if !e.At.Equal(t) || math.Abs(e.Net-net) > .005 || (types != nil && !(r.typ.Valid && contains(types, r.typ.String))) {
				return Checked{}, refusef("хаб %s: строка %s (%s %s UTC %.2f) не совпадает с hub_exceptions (%s, %s, %.2f)",
					page, py(r.ref), py(r.typ), t.Format("2006-01-02 15:04:05"), net, e.Decision,
					e.At.Format("2006-01-02 15:04:05"), e.Net)
			}
			if e.Decision == "исключить" {
				out.Notes = append(out.Notes, fmt.Sprintf("%s %s: исключена строка %s — %s", page, t.Format("2006-01-02"),
					r.ref.String, e.Basis))
				continue
			}
		}
		if (!has && !(r.typ.Valid && holdTypes[r.typ.String])) || !(r.cur.Valid && r.cur.String == "USD") ||
			(net < 0 && !(has && e.Decision == "возврат")) {
			return Checked{}, refusef("хаб %s: строка %s %s %s %.2f (ref %s) — холд или решение не известны, разобрать в "+
				"hub-exceptions.md до расчёта", page, py(r.typ), f.OccurredAt, py(r.cur), net, py(r.ref))
		}
		row := Row{Page: page, Key: r.key, At: t, NetMills: r.net.Int64, Type: r.typ.String, Ref: r.ref.String,
			Avail: t.Add(hold), EventID: r.event}
		if has {
			row.Decision, row.Prize, row.Avail = e.Decision, e.Decision == "приз", t
		}
		out.Rows = append(out.Rows, row)
	}
	sort.SliceStable(out.Rows, func(i, j int) bool {
		if !out.Rows[i].At.Equal(out.Rows[j].At) {
			return out.Rows[i].At.Before(out.Rows[j].At)
		}
		return out.Rows[i].Key < out.Rows[j].Key
	})
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Payout is a payout request of a page.
type Payout struct {
	Page        string    `json:"page"`
	Key         string    `json:"key"`
	Ref         string    `json:"ref,omitempty"`
	At          time.Time `json:"at"` // requestedAt
	AmountMills int64     `json:"amountMills"`
	Status      string    `json:"status"`   // Fansly statusLabel (Processed), OnlyFans state (approved, new)
	Method      string    `json:"method"`   // Fansly methodProviderLabel; "of" for OnlyFans
	Currency    string    `json:"currency"` // OnlyFans only
	Updated     time.Time `json:"updated"`  // Fansly: updatedAtPlatform or requestedAt; OnlyFans: requestedAt
	EventID     int64     `json:"eventId"`
}

// Amount is the request in micro-dollars.
func (p Payout) Amount() money.Micros { return money.Micros(p.AmountMills * 1000) }

// Payouts returns the Fansly payout requests of a page over [from, to) (month.py Hub.payouts): a
// method outside fansly_payout_methods refuses the page.
func Payouts(ctx context.Context, q ledger.Querier, page string, from, to time.Time) ([]Payout, error) {
	set, err := LoadSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	methods, err := set.Set("fansly_payout_methods")
	if err != nil {
		return nil, err
	}
	if _, err := proven(ctx, q, page, "payouts", from, to); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT key, requested_at, amount_mills, status, method, updated_at_platform,
		payout_ref, event_id FROM hub_payouts WHERE page_label = ? AND requested_at >= ? AND requested_at < ?
		ORDER BY requested_at, key`,
		page, ts.FormatMilli(from), ts.FormatMilli(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payout
	for rows.Next() {
		var p Payout
		var at string
		var amount sql.NullInt64
		var status, method, upd, ref sql.NullString
		if err := rows.Scan(&p.Key, &at, &amount, &status, &method, &upd, &ref, &p.EventID); err != nil {
			return nil, err
		}
		if p.At, err = ts.Parse(at); err != nil {
			return nil, err
		}
		p.At = p.At.UTC()
		if !method.Valid || !methods[method.String] {
			m := "None"
			if method.Valid {
				m = method.String
			}
			return nil, refusef("хаб %s: заявка %s с неизвестным методом «%s»", page, p.At.Format(time.RFC3339), m)
		}
		if !amount.Valid {
			return nil, refusef("хаб %s: заявка %s без amountMills — ответ не по контракту", page, p.At.Format(time.RFC3339))
		}
		p.Page, p.AmountMills, p.Status, p.Method, p.Ref, p.Updated = page, amount.Int64, status.String, method.String,
			ref.String, p.At
		if upd.Valid && upd.String != "" {
			if t, err := ts.Parse(upd.String); err == nil {
				p.Updated = t.UTC()
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortPayouts(out)
	return out, nil
}

// OFPayouts returns the OnlyFans payout requests of a page over [from, to) (month.py
// Hub.of_payouts): one row per invoice, USD only.
func OFPayouts(ctx context.Context, q ledger.Querier, page string, from, to time.Time) ([]Payout, error) {
	if _, err := proven(ctx, q, page, "ofapi_payout_requests", from, to); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT key, requested_at, amount_mills, currency, state, payout_ref, event_id
		FROM hub_of_payouts WHERE page_label = ? AND requested_at >= ? AND requested_at < ?
		ORDER BY requested_at, key`,
		page, ts.FormatMilli(from), ts.FormatMilli(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payout
	for rows.Next() {
		var p Payout
		var at string
		var amount sql.NullInt64
		var cur, state, ref sql.NullString
		if err := rows.Scan(&p.Key, &at, &amount, &cur, &state, &ref, &p.EventID); err != nil {
			return nil, err
		}
		if p.At, err = ts.Parse(at); err != nil {
			return nil, err
		}
		p.At = p.At.UTC()
		if !cur.Valid || cur.String != "USD" {
			c := "None"
			if cur.Valid {
				c = cur.String
			}
			return nil, refusef("хаб %s: заявка OF %s в %s", page, p.At.Format(time.RFC3339), c)
		}
		if !amount.Valid {
			return nil, refusef("хаб %s: заявка OF %s без amountMills — ответ не по контракту", page, p.At.Format(time.RFC3339))
		}
		p.Page, p.AmountMills, p.Currency, p.Status, p.Method, p.Ref, p.Updated = page, amount.Int64, cur.String,
			state.String, "of", ref.String, p.At
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortPayouts(out)
	return out, nil
}

func sortPayouts(ps []Payout) {
	sort.SliceStable(ps, func(i, j int) bool {
		if !ps[i].At.Equal(ps[j].At) {
			return ps[i].At.Before(ps[j].At)
		}
		return ps[i].Key < ps[j].Key
	})
}

// Page is a page of the key's grant with its ledger model.
type Page struct {
	Label     string `json:"page"`
	Platform  string `json:"platform"`
	ModelSlug string `json:"modelSlug"`
	ModelName string `json:"modelName"`
	Model     string `json:"model"` // hub_model_slugs of modelSlug, or modelSlug itself
}

// GrantPages returns the grant of the last run (month.py Hub.pages).
func GrantPages(ctx context.Context, q ledger.Querier) ([]Page, error) {
	st, err := loadStatus(ctx, q, "*", "capabilities")
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, refusef("хаб: грант ключа не прочитан (ledger hub sync)")
	}
	if st.Status != "ok" {
		return nil, refusef("хаб: грант ключа не прочитан на %s: %s", st.CheckedAt, strings.Join(st.Reasons, "; "))
	}
	set, err := LoadSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	slugs, err := set.Map("hub_model_slugs")
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT page_label, platform, model_slug, model_name FROM hub_pages ORDER BY page_label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.Label, &p.Platform, &p.ModelSlug, &p.ModelName); err != nil {
			return nil, err
		}
		p.Model = p.ModelSlug
		if m, ok := slugs[p.ModelSlug]; ok {
			p.Model = m
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CaptureFloor is when the hub's record of the page's transactions begins (the summary's
// transactions plane); ok is false when the hub could not compute one.
func CaptureFloor(ctx context.Context, q ledger.Querier, page string) (time.Time, bool, error) {
	st, err := loadStatus(ctx, q, page, "transactions")
	if err != nil {
		return time.Time{}, false, err
	}
	if st == nil || st.Status != "ok" {
		_, err := proven(ctx, q, page, "transactions", time.Time{}, time.Time{})
		return time.Time{}, false, err
	}
	if st.floor == nil {
		return time.Time{}, false, nil
	}
	return st.floor.UTC(), true, nil
}

// CheckExceptionPages refuses decisions about pages outside the key's grant (month.py cover()).
func CheckExceptionPages(ctx context.Context, q ledger.Querier) error {
	ex, err := LoadExceptions(ctx, q)
	if err != nil {
		return err
	}
	grant, err := GrantPages(ctx, q)
	if err != nil {
		return err
	}
	in := map[string]bool{}
	for _, p := range grant {
		in[p.Label] = true
	}
	var stray []string
	for _, p := range ex.Pages() {
		if !in[p] {
			stray = append(stray, p)
		}
	}
	if len(stray) > 0 {
		return fmt.Errorf("hub_exceptions: страницы не из гранта ключа хаба: %s", pyList(stray))
	}
	return nil
}

// ---- «Оборот» of a month ----

// PageNet is one page of the month.
type PageNet struct {
	Page     string   `json:"page"`
	Model    string   `json:"model"`
	Channel  string   `json:"channel,omitempty"`
	Status   string   `json:"status"` // ok | unknown
	Reason   string   `json:"reason,omitempty"`
	Net      string   `json:"net,omitempty"`
	NetMills int64    `json:"netMills"`
	Rows     int      `json:"rows"`
	Notes    []string `json:"notes,omitempty"`
}

// NetLine is a (model, channel) line of the month card.
type NetLine struct {
	Model    string   `json:"model"`
	Channel  string   `json:"channel"`
	Status   string   `json:"status"`
	Net      string   `json:"net,omitempty"`
	NetMills int64    `json:"netMills"`
	Pages    []string `json:"pages"`
}

// NetReport is the «Оборот» of a month per page and per line.
type NetReport struct {
	Month      string    `json:"month"`
	AsOf       string    `json:"asOf"`
	Pages      []PageNet `json:"pages"`
	Lines      []NetLine `json:"lines"`
	Total      string    `json:"total,omitempty"`
	TotalMills int64     `json:"totalMills"`
	Unrouted   []PageNet `json:"unrouted"`
	Unknown    int       `json:"unknown"`
}

func mills(v int64) string { return money.Micros(v * 1000).String() }

// DefaultAsOf is the latest cut every page's transactions are proven to: the earliest window end
// among the routed and granted pages.
func DefaultAsOf(ctx context.Context, q ledger.Querier) (time.Time, error) {
	sts, err := Statuses(ctx, q)
	if err != nil {
		return time.Time{}, err
	}
	var cut time.Time
	for _, s := range sts {
		if s.Dataset == "transactions" && s.Status == "ok" && s.To != nil && (cut.IsZero() || s.To.Before(cut)) {
			cut = *s.To
		}
	}
	if cut.IsZero() {
		return cut, refusef("хаб: нет доказанных транзакций ни одной страницы (ledger hub sync)")
	}
	return cut, nil
}

// MonthNet is month.py card() agg[p]["net"]: per routed page the sum of its checked rows' net with
// occurredAt in the MSK month and before asOf. Granted pages without a route are listed apart with
// the plain sum of the hub's rows (month.py takes it from the summary) and stay out of the total.
func MonthNet(ctx context.Context, q ledger.Querier, month string, asOf time.Time) (NetReport, error) {
	m0, m1, err := ts.MonthStart(month)
	if err != nil {
		return NetReport{}, err
	}
	rep := NetReport{Month: month, AsOf: ts.Format(asOf), Pages: []PageNet{}, Lines: []NetLine{}, Unrouted: []PageNet{}}
	end := m1
	if asOf.Before(end) {
		end = asOf
	}
	routes, err := Routes(ctx, q)
	if err != nil {
		return rep, err
	}
	labels := make([]string, 0, len(routes))
	for p := range routes {
		labels = append(labels, p)
	}
	sort.Strings(labels)
	type lineKey struct{ model, channel string }
	lines := map[lineKey]*NetLine{}
	var order []lineKey
	totalOK := true
	for _, p := range labels {
		r := routes[p]
		pn := PageNet{Page: p, Model: r.Model, Channel: r.Channel, Status: "ok"}
		c, err := Rows(ctx, q, p, m0, end)
		if err != nil {
			if !IsRefusal(err) {
				return rep, err
			}
			pn.Status, pn.Reason = "unknown", err.Error()
			rep.Unknown++
			totalOK = false
		} else {
			for _, row := range c.Rows {
				if !row.At.Before(m0) && row.At.Before(m1) {
					pn.NetMills += row.NetMills
					pn.Rows++
				}
			}
			pn.Net, pn.Notes = mills(pn.NetMills), c.Notes
		}
		rep.Pages = append(rep.Pages, pn)
		k := lineKey{r.Model, r.Channel}
		l := lines[k]
		if l == nil {
			l = &NetLine{Model: r.Model, Channel: r.Channel, Status: "ok"}
			lines[k] = l
			order = append(order, k)
		}
		l.Pages = append(l.Pages, p)
		l.NetMills += pn.NetMills
		if pn.Status != "ok" {
			l.Status = "unknown"
		}
		rep.TotalMills += pn.NetMills
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].model != order[j].model {
			return order[i].model < order[j].model
		}
		return order[i].channel < order[j].channel
	})
	for _, k := range order {
		l := lines[k]
		if l.Status == "ok" {
			l.Net = mills(l.NetMills)
		}
		rep.Lines = append(rep.Lines, *l)
	}
	if totalOK {
		rep.Total = mills(rep.TotalMills)
	}
	grant, err := GrantPages(ctx, q)
	if err != nil {
		return rep, err
	}
	ex, err := LoadExceptions(ctx, q)
	if err != nil {
		return rep, err
	}
	for _, g := range grant {
		if _, ok := routes[g.Label]; ok {
			continue
		}
		pn := PageNet{Page: g.Label, Model: g.Model, Status: "ok",
			Notes: []string{g.Label + ": страница без строки в таблице маршрутов — условия не согласованы, в цифру не входит"}}
		s := m0
		if st, ok := ex.Starts[g.Label]; ok && st.After(s) {
			s = st
		}
		if s.Before(end) {
			n, sum, err := PlainSum(ctx, q, g.Label, s, end)
			if err != nil {
				if !IsRefusal(err) {
					return rep, err
				}
				pn.Status, pn.Reason = "unknown", err.Error()
				rep.Unknown++
			}
			pn.Rows, pn.NetMills = n, sum
		}
		if pn.Status == "ok" {
			pn.Net = mills(pn.NetMills)
		}
		rep.Unrouted = append(rep.Unrouted, pn)
	}
	return rep, nil
}

// PlainSum is the summary of [from, to) from the mirror: every row, as the hub sums them.
func PlainSum(ctx context.Context, q ledger.Querier, page string, from, to time.Time) (int, int64, error) {
	if _, err := proven(ctx, q, page, "transactions", from, to); err != nil {
		return 0, 0, err
	}
	var n int
	var sum int64
	err := q.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(net_mills), 0) FROM hub_transactions
		WHERE page_label = ? AND occurred_at >= ? AND occurred_at < ?`, page, ts.FormatMilli(from), ts.FormatMilli(to)).
		Scan(&n, &sum)
	return n, sum, err
}

// NetExcluded is the net of a page over [from, to) in dollars, without rows decided «исключить»
// (the day figures of the plan line). It refuses like Rows when the window is not proven.
func NetExcluded(ctx context.Context, q ledger.Querier, page string, from, to time.Time) (float64, error) {
	ex, err := LoadExceptions(ctx, q)
	if err != nil {
		return 0, err
	}
	a := from
	if st, ok := ex.Starts[page]; ok && st.After(a) {
		a = st
	}
	if !a.Before(to) {
		return 0, nil
	}
	if _, err := proven(ctx, q, page, "transactions", a, to); err != nil {
		return 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT ref, net_mills FROM hub_transactions WHERE page_label = ? AND occurred_at >= ? AND occurred_at < ?`,
		page, ts.FormatMilli(a), ts.FormatMilli(to))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var sum int64
	for rows.Next() {
		var ref sql.NullString
		var net sql.NullInt64
		if err := rows.Scan(&ref, &net); err != nil {
			return 0, err
		}
		if e, ok := ex.Rows[RowRef{page, ref.String}]; ok && ref.Valid && e.Decision == "исключить" {
			continue
		}
		sum += net.Int64
	}
	return float64(sum) / 1000, rows.Err()
}
