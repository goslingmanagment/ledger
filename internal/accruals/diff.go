package accruals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/accruals/diff", auth.Require(s, auth.ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			week := r.URL.Query().Get("week")
			if _, err := Monday(week); err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			d, err := WeekDiff(r.Context(), s.Store.RO, week)
			if err != nil {
				s.Log.Error("accruals diff", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "было — стало не посчитано: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, d)
		})))
	})
}

// Diff is a week of the payout sheets beside the accruals from Hub's facts, in shadow.
type Diff struct {
	OK     bool         `json:"ok"`
	Week   string       `json:"week"`  // Monday
	Until  string       `json:"until"` // Sunday
	Facts  WeekFacts    `json:"facts"`
	People []PersonDiff `json:"people"`
	// Pot is the общак of the week: no person's, the owner splits it by hand.
	Pot        Group      `json:"pot"`
	Waiting    Group      `json:"waiting"`
	Decide     Group      `json:"decide"`
	Nobody     Group      `json:"nobody"`
	Unknown    Group      `json:"unknown"`
	BeforeRule Group      `json:"beforeRule"`
	Unmapped   []Unmapped `json:"unmapped"`
	// ReportsFrom is the switch to the shift reports (settings.payroll_reports_from); "" — the reports are a shadow.
	ReportsFrom string `json:"reportsFrom,omitempty"`
	// HubFrom is the day from which a reports day accrues from the report handed in in Hub (payroll_basis_hub_from); ""
	// — the posts only. HubDays are the week's days where Hub's handed-in report and the post differ.
	HubFrom string   `json:"hubFrom,omitempty"`
	HubDays []HubDay `json:"hubDays"`
	// TermsFrom is the day from which a day's accrual takes the person's terms from Hub's people registry
	// (payroll_terms_hub_from); "" — the tabs' headers. TermsDays are the week's reports days whose terms differ: the
	// registry's term holding on the day against the tab's header, or no registry term.
	TermsFrom string     `json:"termsFrom,omitempty"`
	TermsDays []TermsDay `json:"termsDays"`
}

// HubDay is a person's day where the report handed in in Hub and the post differ — a page by another amount, or on one
// side only — with every page of the day that Hub or the post has.
type HubDay struct {
	Person string `json:"person"`
	Name   string `json:"name"`
	Day    string `json:"day"`
	// Basis is what accrues the day: hub — Hub's report (from HubFrom), post — the post.
	Basis      string    `json:"basis"`
	PostMicros *int64    `json:"postMicros"` // the post's pages added up; nil — no post
	HubMicros  *int64    `json:"hubMicros"`  // Hub's pages added up
	Pages      []HubPage `json:"pages"`
}

// HubPage is a page of a HubDay: the post's amount and Hub's (nil — not on that side), and how they compare.
type HubPage struct {
	Page       string `json:"page"`
	PostMicros *int64 `json:"postMicros"`
	HubMicros  *int64 `json:"hubMicros"`
	State      string `json:"state"` // hub_equal, hub_changed, hub_only, hub_missing
}

// WeekFacts says whether the week's facts are proven.
type WeekFacts struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
	ReadAt  string   `json:"readAt,omitempty"`
}

// PersonDiff is one person's week: the sheet, the facts (nil — unknown), the difference and what makes it.
type PersonDiff struct {
	Person      string `json:"person"`
	Name        string `json:"name"`
	SheetMicros int64  `json:"sheetMicros"`
	FactsMicros *int64 `json:"factsMicros"`
	DiffMicros  *int64 `json:"differenceMicros"`
	// FactsNetMicros is the net of the person's shares; UnpricedMicros the part of it no rate prices.
	FactsNetMicros *int64     `json:"factsNetMicros"`
	UnpricedMicros *int64     `json:"unpricedNetMicros"`
	Days           []DayDiff  `json:"days"`
	Notes          []string   `json:"notes,omitempty"`
	Sales          []SaleLine `json:"sales"` // of the days that differ
	// Claimed are the nobody's sales of the week the owner gave the person by a claim (sale_claims), every one.
	Claimed []SaleLine `json:"claimed,omitempty"`
	// ReportsMicros is what the posted shift reports accrue for the week (report_accruals); ReportsNetMicros their
	// revenue, ReportsUnpricedMicros the part of it «без ставки».
	ReportsMicros         int64 `json:"reportsMicros"`
	ReportsNetMicros      int64 `json:"reportsNetMicros"`
	ReportsUnpricedMicros int64 `json:"reportsUnpricedNetMicros"`
	// SheetReportsDiffMicros is reports − sheet; SheetReportsCountedMicros the same without the sheet's rounding of
	// whole-dollar cells (what the totals count).
	SheetReportsDiffMicros    int64 `json:"sheetReportsDifferenceMicros"`
	SheetReportsCountedMicros int64 `json:"sheetReportsCountedMicros"`
	// ReportsFactsDiffMicros is facts − reports; nil — the facts of the week are unknown.
	ReportsFactsDiffMicros *int64 `json:"reportsFactsDifferenceMicros"`
	terms                  map[key]rate
}

type key struct {
	user        int64
	model, page string
	day         string
}

type rate struct{ v *float64 }

// DayDiff is one day of a person's week.
type DayDiff struct {
	Day string `json:"day"`
	// SheetRevenue is the revenue the sheet's cells hold for the day, dollars.
	SheetRevenue   float64 `json:"sheetRevenue"`
	SheetMicros    int64   `json:"sheetMicros"`
	FactsNetMicros int64   `json:"factsNetMicros"`
	FactsMicros    int64   `json:"factsMicros"`
	DiffMicros     int64   `json:"differenceMicros"`
	// The day by the posted reports: their revenue and what they accrue; the difference from the sheet (reports −
	// sheet), the part of it the sheet's whole-dollar cells explain, and from the facts (facts − reports; nil — unknown).
	ReportRevenueMicros    int64  `json:"reportRevenueMicros"`
	ReportsMicros          int64  `json:"reportsMicros"`
	SheetReportsDiffMicros int64  `json:"sheetReportsDifferenceMicros"`
	RoundingMicros         int64  `json:"roundingMicros,omitempty"`
	ReportsFactsDiffMicros *int64 `json:"reportsFactsDifferenceMicros"`
	// Presence says a day is only on one side: «в таблице есть, отчёта нет» or «отчёт есть, в таблице нет».
	Presence string `json:"presence,omitempty"`
	// Pages are the day's pages with both amounts, for a day where the sheet and the reports differ.
	Pages []PageDiff `json:"pages,omitempty"`
	// PotMicros is the part of the report revenue that is the person's pot shares of the day (R3), Pots by pot, and
	// PotNotes say it: «в т.ч. общак <название>: база X».
	PotMicros int64      `json:"potMicros,omitempty"`
	Pots      []PotShare `json:"pots,omitempty"`
	PotNotes  []string   `json:"potNotes,omitempty"`
	hasSheet  bool
	hasReport bool
}

// PotShare is a person's share of a day's purchases in one pot.
type PotShare struct {
	Pot    string `json:"pot"`
	Name   string `json:"name"`
	Micros int64  `json:"micros"`
}

// PotNote says a pot share of a day: «в т.ч. общак <название>: база X».
func PotNote(name string, micros int64) string {
	return "в т.ч. общак " + name + ": база " + money.Micros(micros).Cents()
}

// PageDiff is a page of a day: the sheet's cell and the posted amount (nil — not on that side). Rounding marks a
// difference under a dollar on a cell the sheet holds in whole dollars: «округление таблицы», not counted.
type PageDiff struct {
	Page         string `json:"page"`
	SheetMicros  *int64 `json:"sheetMicros"`
	ReportMicros *int64 `json:"reportMicros"`
	Rounding     bool   `json:"rounding,omitempty"`
}

// SaleLine is a sale: credited to the person ("+"), held by their shift and gone elsewhere ("−"), or in a group.
type SaleLine struct {
	Sign         string `json:"sign,omitempty"`
	Day          string `json:"day"`
	At           string `json:"at"`
	Page         string `json:"page"`
	Kind         string `json:"kind"`
	Ref          string `json:"ref"`
	NetMicros    int64  `json:"netMicros"`   // the sale's net
	ShareMicros  int64  `json:"shareMicros"` // the part this line is about
	AmountMicros *int64 `json:"amountMicros,omitempty"`
	To           string `json:"to,omitempty"` // where it went
	Reason       string `json:"reason"`
	Note         string `json:"note,omitempty"` // the owner's word of a claimed sale
}

// Group is the sales of one kind of home in the week.
type Group struct {
	Micros int64      `json:"micros"`
	Sales  []SaleLine `json:"sales"`
}

// Unmapped is a Hub user no card names: their facts are listed, never dropped.
type Unmapped struct {
	UserID    int64  `json:"userId"`
	Username  string `json:"username"`
	Model     string `json:"model"`
	NetMicros int64  `json:"netMicros"`
	Days      int    `json:"days"`
	Reason    string `json:"reason"`
}

// Monday checks a week's date.
func Monday(week string) (time.Time, error) {
	t, err := time.ParseInLocation(ts.DateLayout, week, ts.MSK)
	if err != nil || t.Weekday() != time.Monday {
		return t, fmt.Errorf("неделя — понедельник ГГГГ-ММ-ДД, не «%s»", week)
	}
	return t, nil
}

// differs: a difference of a cent or more.
func differs(v int64) bool { return v >= 10_000 || v <= -10_000 }

// WeekDiff reads the week: v_accrual_diff per person, their days and the sales of the days that differ, the общак and
// the rest of the week's homes, the Hub users no card names.
func WeekDiff(ctx context.Context, q ledger.Querier, week string) (Diff, error) {
	start, err := Monday(week)
	if err != nil {
		return Diff{}, err
	}
	d := Diff{Week: week, Until: ts.Date(start.AddDate(0, 0, 6)), People: []PersonDiff{}, Unmapped: []Unmapped{}}
	var reasons string
	var readAt sql.NullString
	switch err := q.QueryRowContext(ctx, `SELECT status, reasons, read_at FROM fact_weeks WHERE week = ?`, week).
		Scan(&d.Facts.Status, &reasons, &readAt); {
	case err == sql.ErrNoRows:
		d.Facts = WeekFacts{Status: "unknown", Reasons: []string{"фактов этой недели нет: она раньше правила (" +
			ts.Date(split.Default.Since) + ") или факты ещё не читались (ledger hub team-sync)"}}
	case err != nil:
		return d, err
	default:
		_ = json.Unmarshal([]byte(reasons), &d.Facts.Reasons)
		d.Facts.ReadAt = readAt.String
	}
	known := d.Facts.Status == "ok"
	d.OK = known

	rows, err := q.QueryContext(ctx, `SELECT person, name, sheet_micros, facts_micros, difference_micros, facts_net_micros,
		unpriced_net_micros FROM v_accrual_diff WHERE week = ? ORDER BY person`, week)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var p PersonDiff
		var facts, diff, net, unpriced sql.NullInt64
		if err := rows.Scan(&p.Person, &p.Name, &p.SheetMicros, &facts, &diff, &net, &unpriced); err != nil {
			rows.Close()
			return d, err
		}
		p.FactsMicros, p.DiffMicros, p.FactsNetMicros, p.UnpricedMicros = ptr(facts), ptr(diff), ptr(net), ptr(unpriced)
		d.People = append(d.People, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return d, err
	}
	rows.Close()
	if err := extraPeople(ctx, q, &d, known); err != nil {
		return d, err
	}

	homes, err := loadHomes(ctx, q, ts.Date(start.AddDate(0, 0, -15)), ts.Date(start.AddDate(0, 0, 8)))
	if err != nil {
		return d, err
	}
	names, err := personNames(ctx, q)
	if err != nil {
		return d, err
	}
	claims, err := loadClaims(ctx, q)
	if err != nil {
		return d, err
	}
	for i := range d.People {
		if err := personDays(ctx, q, &d.People[i], week, d.Until, known); err != nil {
			return d, err
		}
		p := &d.People[i]
		p.Claimed = claimedOf(p, week, d.Until, homes, claims)
		if !known || p.DiffMicros == nil || !differs(*p.DiffMicros) {
			continue
		}
		for _, day := range p.Days {
			if differs(day.DiffMicros) {
				p.Sales = append(p.Sales, salesOf(p, day.Day, homes, names)...)
			}
		}
	}

	if err := addReports(ctx, q, &d, known); err != nil {
		return d, err
	}
	if err := addHub(ctx, q, &d); err != nil {
		return d, err
	}
	if err := addTerms(ctx, q, &d); err != nil {
		return d, err
	}

	groups := map[string]*Group{split.Pot: &d.Pot, split.Waiting: &d.Waiting, split.Decide: &d.Decide,
		split.Nobody: &d.Nobody, split.Unknown: &d.Unknown, split.BeforeRule: &d.BeforeRule}
	for _, g := range groups {
		g.Sales = []SaleLine{}
	}
	for _, h := range homes {
		g, ok := groups[h.home]
		if !ok || h.day < week || h.day > d.Until {
			continue
		}
		g.Micros += h.share
		g.Sales = append(g.Sales, line("", h, nil, ""))
	}

	rows, err = q.QueryContext(ctx, `SELECT hub_user_id, username, model, SUM(net_micros), COUNT(DISTINCT day), MIN(COALESCE(reason, ''))
		FROM fact_accruals WHERE week = ? AND person IS NULL GROUP BY hub_user_id, username, model ORDER BY username, model`, week)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var u Unmapped
		if err := rows.Scan(&u.UserID, &u.Username, &u.Model, &u.NetMicros, &u.Days, &u.Reason); err != nil {
			return d, err
		}
		d.Unmapped = append(d.Unmapped, u)
	}
	return d, rows.Err()
}

func ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

// personDays fills a person's days of the week: the sheet's (payroll_days, the amount as the sheet computed it and the
// revenue its cells hold) beside the facts' (fact_accruals), and the notes of what the facts could not price.
func personDays(ctx context.Context, q ledger.Querier, p *PersonDiff, week, until string, known bool) error {
	byDay := map[string]*DayDiff{}
	get := func(day string) *DayDiff {
		if byDay[day] == nil {
			byDay[day] = &DayDiff{Day: day}
		}
		return byDay[day]
	}
	rows, err := q.QueryContext(ctx, `SELECT date, amount, pages FROM payroll_days WHERE person = ? AND date BETWEEN ? AND ?`,
		p.Person, week, until)
	if err != nil {
		return err
	}
	for rows.Next() {
		var day, pages string
		var amount float64
		if err := rows.Scan(&day, &amount, &pages); err != nil {
			rows.Close()
			return err
		}
		var cells map[string]*float64
		_ = json.Unmarshal([]byte(pages), &cells)
		revenue := 0.0
		for _, v := range cells {
			if v != nil {
				revenue += *v
			}
		}
		if amount == 0 && revenue == 0 {
			continue // an empty day of the block
		}
		x := get(day)
		x.hasSheet = true
		x.SheetMicros += int64(money.FromFloat(amount))
		x.SheetRevenue += revenue
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	p.terms = map[key]rate{}
	if known {
		rows, err = q.QueryContext(ctx, `SELECT hub_user_id, model, page_label, day, kind, net_micros, rate, amount_micros, COALESCE(reason, '')
			FROM fact_accruals WHERE person = ? AND week = ?`, p.Person, week)
		if err != nil {
			return err
		}
		notes := map[string]bool{}
		for rows.Next() {
			var k key
			var kind, reason string
			var net int64
			var r sql.NullFloat64
			var amount sql.NullInt64
			if err := rows.Scan(&k.user, &k.model, &k.page, &k.day, &kind, &net, &r, &amount, &reason); err != nil {
				rows.Close()
				return err
			}
			x := get(k.day)
			if kind == "sales" {
				x.FactsNetMicros += net
			}
			x.FactsMicros += amount.Int64
			if r.Valid {
				v := r.Float64
				p.terms[k] = rate{&v}
			}
			if reason != "" {
				notes[reason] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for n := range notes {
			p.Notes = append(p.Notes, n)
		}
		sort.Strings(p.Notes)
	}
	for _, x := range byDay {
		x.DiffMicros = x.FactsMicros - x.SheetMicros
		p.Days = append(p.Days, *x)
	}
	sort.Slice(p.Days, func(i, j int) bool { return p.Days[i].Day < p.Days[j].Day })
	return nil
}

// salesOf are the sales of a person's day that make its difference: credited to them by the facts, and those their
// shift of the day held that went elsewhere.
func salesOf(p *PersonDiff, day string, homes []homeRow, names map[string]string) []SaleLine {
	bySale := map[string][]homeRow{}
	var order []string
	for _, h := range homes {
		if _, ok := bySale[h.ref]; !ok {
			order = append(order, h.ref)
		}
		bySale[h.ref] = append(bySale[h.ref], h)
	}
	var out []SaleLine
	for _, ref := range order {
		rs := bySale[ref]
		mine := false
		for _, h := range rs {
			if h.person == p.Person && (h.home == split.Shift || h.home == split.Visit) {
				mine = true
				if h.day == day {
					var amount *int64
					if r := p.terms[key{h.user, h.model, h.page, h.day}]; r.v != nil {
						v := int64(money.FromFloat(float64(float64(h.share)/money.Scale) * *r.v))
						amount = &v
					}
					out = append(out, line("+", h, amount, ""))
				}
			}
		}
		if mine {
			continue
		}
		held := false
		for _, x := range rs[0].holders {
			if x.Person == p.Person && x.Day == day {
				held = true
			}
		}
		if !held {
			continue
		}
		for _, h := range rs {
			out = append(out, line("−", h, nil, whereTo(h, names)))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// claimedOf are the person's claimed sales of the week, with the amount at their rate and the owner's word.
func claimedOf(p *PersonDiff, week, until string, homes []homeRow, claims map[string]claim) []SaleLine {
	var out []SaleLine
	for _, h := range homes {
		if h.person != p.Person || h.reason != split.ReasonOwnerClaim || h.day < week || h.day > until {
			continue
		}
		var amount *int64
		if r := p.terms[key{h.user, h.model, h.page, h.day}]; r.v != nil {
			v := int64(money.FromFloat(float64(float64(h.share)/money.Scale) * *r.v))
			amount = &v
		}
		l := line("+", h, amount, "")
		l.Note = claims[h.ref].note
		out = append(out, l)
	}
	return out
}

func whereTo(h homeRow, names map[string]string) string {
	switch h.home {
	case split.Shift, split.Visit:
		if n := names[h.person]; n != "" {
			return n
		}
		if h.username != "" {
			return h.username + " (хаб)"
		}
		return "смене"
	case split.Pot:
		return "общак"
	case split.Waiting:
		return "ждёт смены отправителя"
	case split.Decide:
		return "ждёт решения"
	case split.Nobody:
		return "ничей"
	case split.BeforeRule:
		return "по-старому (таблица)"
	}
	return "неизвестно"
}

func line(sign string, h homeRow, amount *int64, to string) SaleLine {
	return SaleLine{Sign: sign, Day: h.day, At: h.at, Page: h.page, Kind: h.kind, Ref: h.ref, NetMicros: h.net, ShareMicros: h.share,
		AmountMicros: amount, To: to, Reason: ReasonText(h.reason, h.waitsUntil)}
}

// ReasonText says a split reason in Russian.
func ReasonText(reason, waitsUntil string) string {
	text, ok := reasonText[reason]
	if !ok {
		text = reason
	}
	if waitsUntil != "" {
		text += " до " + waitsUntil[:16]
	}
	return text
}

var reasonText = map[string]string{
	split.ReasonSaleBeforeRule:     "до начала правила: платит таблица",
	split.ReasonWorkBeforeRule:     "смена дня до правила: по-старому, платит таблица",
	split.ReasonHolderNotInWorks:   "держателя нет среди работ: факты не полны",
	split.ReasonPPVSender:          "PPV отправителя: его смена держала продажу",
	split.ReasonPPVSenderOtherPage: "PPV отправителя: его смена на другой странице",
	split.ReasonPPVSenderNextShift: "PPV отправителя: его следующая смена",
	split.ReasonPPVSenderWaiting:   "PPV ждёт смены отправителя",
	split.ReasonPPVSenderNoShift:   "PPV отправителя: смены за 7 дней не было",
	split.ReasonPostTip:            "чаевые под постом",
	split.ReasonCampaign:           "Campaign OnlyFans",
	split.ReasonHolder:             "страницу держала одна смена",
	split.ReasonLastWriter:         "последним писал фану",
	split.ReasonLookInLastWriter:   "заход: последним писал фану",
	split.ReasonNoHolder:           "страницу никто не держал",
	split.ReasonNoLastWriter:       "держали несколько смен, последним писал никто из них",
	split.ReasonMarksDisagree:      "метки чаттеров не сходятся",
	split.ReasonMarks:              "по меткам чаттеров",
	split.ReasonOwnerSplit:         "решение владельца",
	split.ReasonOwnerClaim:         "ничья, заявка по слову владельца",
}

const homeColumns = `SELECT transaction_ref, seq, window_from, occurred_at, page_label, model, sale_kind, net_micros, home,
	reason, COALESCE(pot, ''), COALESCE(waits_until, ''), COALESCE(hub_user_id, 0), COALESCE(username, ''), COALESCE(person, ''),
	COALESCE(work_ref, ''), share_micros, day, holders FROM fact_homes`

// loadHomes reads fact_homes of the sales whose days or holders' days may fall in [from, to].
func loadHomes(ctx context.Context, q ledger.Querier, from, to string) ([]homeRow, error) {
	return scanHomes(q.QueryContext(ctx, homeColumns+` WHERE day BETWEEN ? AND ? OR substr(occurred_at, 1, 10) BETWEEN ? AND ?
		ORDER BY occurred_at, transaction_ref, seq`, from, to, from, to))
}

// saleHomes reads the rows of one sale.
func saleHomes(ctx context.Context, q ledger.Querier, ref string) ([]homeRow, error) {
	return scanHomes(q.QueryContext(ctx, homeColumns+` WHERE transaction_ref = ? ORDER BY seq`, ref))
}

func scanHomes(rows *sql.Rows, err error) ([]homeRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []homeRow
	for rows.Next() {
		var h homeRow
		var holders string
		if err := rows.Scan(&h.ref, &h.seq, &h.window, &h.at, &h.page, &h.model, &h.kind, &h.net, &h.home, &h.reason, &h.pot,
			&h.waitsUntil, &h.user, &h.username, &h.person, &h.workRef, &h.share, &h.day, &holders); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(holders), &h.holders)
		out = append(out, h)
	}
	return out, rows.Err()
}

func personNames(ctx context.Context, q ledger.Querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug, name FROM people`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, name string
		if err := rows.Scan(&slug, &name); err != nil {
			return nil, err
		}
		out[slug] = name
	}
	return out, rows.Err()
}
