// Package accruals counts what each person would be paid from Hub's facts (ledger release 2, L3), in shadow. Every
// sale of a proven window of the team's facts (hub_team_sync) goes through the split rules (internal/split). Each share
// of a shift or a look-in goes to its person at the rate of their tab for the page's model: payroll.Terms, the same
// header the sheet's days are counted by — «Ставка %» of the revenue, and «Фикс/см» for a day with a shift (from
// settings.payroll_terms_hub_from the person's term holding on the day in Hub's people registry, people.terms). The общак
// is summed per week with its sales; what waits, waits for a decision, is nobody's or unknown is listed with its
// sales, not accrued. Nothing here posts; from settings.payroll_facts_from on payroll.Rebuild posts fact_accruals of the
// proven weeks as the debts (L4), before it they are a shadow. `ledger accruals diff` puts a week of it beside the
// payout sheets.
package accruals

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	hub.AfterTeam = func(ctx context.Context, store *db.Store, now time.Time) error {
		_, err := Rebuild(ctx, store, now)
		return err
	}
}

// Report says what a rebuild found.
type Report struct {
	Windows  int `json:"windows"` // proven windows counted
	Sales    int `json:"sales"`
	Accruals int `json:"accruals"`
	Weeks    int `json:"weeks"`
	// Rewritten is how many weeks' rows changed and were written anew.
	Rewritten int      `json:"rewritten"`
	Unknown   []string `json:"unknownWeeks,omitempty"`
	Unmapped  []string `json:"unmapped,omitempty"` // Hub users no card names
	// Claims is how many of the owner's claims (sale_claims) the facts gave their person; ClaimsIgnored — the rest, why.
	Claims        int            `json:"claims"`
	ClaimsIgnored []IgnoredClaim `json:"claimsIgnored,omitempty"`
}

// IgnoredClaim is a claim the facts did not honour: the sale is not nobody's, not in the proven facts, or the person
// has no Hub user to credit.
type IgnoredClaim struct {
	Ref    string `json:"transactionRef"`
	Person string `json:"person"`
	Why    string `json:"why"`
}

type window struct {
	from, to     time.Time
	key          string
	status       string
	reasons      []string
	workTo       time.Time
	readAt, okAt string
}

// holder is a work that held a sale, as fact_homes.holders keeps it.
type holder struct {
	UserID int64  `json:"userId"`
	Person string `json:"person,omitempty"`
	Day    string `json:"day"`
	Kind   string `json:"kind"`
}

// homeRow is one row of fact_homes.
type homeRow struct {
	ref, window, at, page, model, kind string
	net                                int64 // micros
	seq                                int
	home, reason, pot, waitsUntil      string
	user                               int64
	username, person, workRef          string
	percent                            *int
	share                              int64 // micros
	day                                string
	holders                            []holder
}

type accrualKey struct {
	user        int64
	model, page string
	day         string
}

type accrualRow struct {
	username, person, kind string
	sales                  int
	net                    int64 // micros
	rate                   *float64
	amount                 *int64
	reason                 string
}

// Rebuild recomputes fact_homes, fact_accruals and fact_weeks from the mirror of the team's facts, in one
// transaction, then — once the accruals are switched to the facts (settings.payroll_facts_from) — the debts.
// Idempotent; it runs after every read of the facts and every claim.
func Rebuild(ctx context.Context, store *db.Store, now time.Time) (Report, error) {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	rep, err := rebuildTx(ctx, tx, now)
	if err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	return rep, settle(ctx, store, now)
}

// settle rebuilds the debts after the facts changed, when the facts accrue them (payroll L4); before the switch they
// are a shadow and the debts do not move.
func settle(ctx context.Context, store *db.Store, now time.Time) error {
	from, err := payroll.FactsFrom(ctx, store.RO)
	if err != nil || from == "" {
		return err
	}
	if _, err := payroll.Rebuild(ctx, store, now); err != nil {
		return fmt.Errorf("долги после фактов: %w", err)
	}
	return nil
}

// rebuildTx is Rebuild inside the caller's transaction.
func rebuildTx(ctx context.Context, tx *sql.Tx, now time.Time) (Report, error) {
	var rep Report
	rules := split.Default
	windows, err := loadWindows(ctx, tx)
	if err != nil {
		return rep, err
	}
	works, err := loadWorks(ctx, tx)
	if err != nil {
		return rep, err
	}
	sales, err := loadSales(ctx, tx)
	if err != nil {
		return rep, err
	}
	models, err := payroll.HubPageModels(ctx, tx)
	if err != nil {
		return rep, err
	}
	who, err := newPeople(ctx, tx)
	if err != nil {
		return rep, err
	}
	claims, err := loadClaims(ctx, tx)
	if err != nil {
		return rep, err
	}
	// The facts are read from Hub's first works (hub_team_since) for «не занесено»; the rule settles from its own
	// week. A window before it holds no sale of the rule, and its works must not stand in for the rule's windows'.
	ruleWeek := ts.WeekStart(rules.Since)
	ruled := windows[:0:0]
	for _, w := range windows {
		if w.from.Before(ruleWeek) {
			delete(works, w.key)
			delete(sales, w.key)
			continue
		}
		ruled = append(ruled, w)
	}
	windows = ruled

	// A work at a window's edge is cut (Hub computes it from that window's activity only): its whole version is the one
	// of the proven window its start falls in.
	whole := map[string]hub.TeamWork{}
	for _, w := range windows {
		if w.status != "ok" {
			continue
		}
		for _, x := range works[w.key] {
			if !x.StartedAt.Before(w.from) && x.StartedAt.Before(w.to) {
				whole[x.Ref] = x
			}
		}
	}
	usernames := map[int64]string{}
	for _, list := range works {
		for _, x := range list {
			usernames[x.UserID] = x.Username
		}
	}

	var homes []homeRow
	claimed := map[string][]homeRow{} // the rows of each claimed sale of the proven windows
	for _, w := range windows {
		if w.status != "ok" {
			continue
		}
		rep.Windows++
		byRef := map[string]hub.TeamWork{}
		splitWorks := make([]split.Work, 0, len(works[w.key]))
		for _, x := range works[w.key] {
			if c, ok := whole[x.Ref]; ok {
				x = c
			}
			byRef[x.Ref] = x
			splitWorks = append(splitWorks, x.Split())
		}
		list := sales[w.key]
		splitSales := make([]split.Sale, len(list))
		for i, s := range list {
			splitSales[i] = s.Split()
			if c, ok := claims[s.TransactionRef]; ok {
				if user, ok := who.userOf(c.person, models[s.PageLabel], usernames); ok {
					splitSales[i].ClaimedBy = &user
				}
			}
		}
		rep.Sales += len(list)
		// The works are known up to Hub's workTo: a PPV whose wait runs past it still waits.
		for i, h := range rules.HomesOf(splitSales, splitWorks, w.workTo) {
			rows := homesOf(list[i], h, w.key, models[list[i].PageLabel], byRef, usernames, who)
			homes = append(homes, rows...)
			if _, ok := claims[list[i].TransactionRef]; ok {
				claimed[list[i].TransactionRef] = rows
			}
		}
	}
	if err := checkClaims(ctx, tx, claims, claimed, models, who, &rep); err != nil {
		return rep, err
	}

	acc := map[accrualKey]*accrualRow{}
	for _, h := range homes {
		if (h.home != split.Shift && h.home != split.Visit) || h.user == 0 {
			continue
		}
		k := accrualKey{h.user, h.model, h.page, h.day}
		a := acc[k]
		if a == nil {
			a = &accrualRow{username: h.username, person: h.person, kind: "sales"}
			acc[k] = a
		}
		a.sales++
		a.net += h.share
	}
	// The fix of a tab is paid for each Moscow day with a shift (not a look-in) on the model's pages, as the sheet
	// spreads it over the days with records.
	ruleDay := ts.Date(rules.Since)
	for _, x := range whole {
		if x.Kind != split.Shift || x.Day() < ruleDay {
			continue
		}
		seen := map[string]bool{}
		for _, p := range x.Pages {
			m := models[p.PageLabel]
			if p.SalesFrom == nil || seen[m] {
				continue
			}
			seen[m] = true
			k := accrualKey{x.UserID, m, "", x.Day()}
			if acc[k] == nil {
				person, _ := who.person(x.UserID, x.Username, m)
				acc[k] = &accrualRow{username: x.Username, person: person, kind: "fix"}
			}
		}
	}
	unmapped := map[string]bool{}
	for k, a := range acc {
		if a.person == "" {
			_, a.reason = who.person(k.user, a.username, k.model)
			unmapped[fmt.Sprintf("%s (hub %d)", a.username, k.user)] = true
			continue
		}
		t, reason := who.terms(a.person, k.model, k.day)
		switch {
		case reason != "":
			a.reason = reason
		case a.kind == "fix":
			if t.ShiftFix == 0 {
				delete(acc, k) // no fix on the tab
				continue
			}
			v := int64(money.FromFloat(t.ShiftFix))
			a.amount = &v
		case t.Rate == nil:
			a.reason = "в шапке вкладки " + t.Model + "/" + t.Name + " нет «Ставка %»"
		default:
			// As the sheet and «не занесено» count a share of revenue: dollars × rate, then to micros.
			v := int64(money.FromFloat(float64(float64(a.net)/money.Scale) * *t.Rate))
			a.rate, a.amount = t.Rate, &v
		}
		if a.kind == "fix" && a.reason != "" {
			delete(acc, k) // a person without a tab: their sales say so, the days need not
		}
	}
	for u := range unmapped {
		rep.Unmapped = append(rep.Unmapped, u)
	}
	sort.Strings(rep.Unmapped)
	rep.Accruals = len(acc)

	weeks := weekStatuses(rules, windows, now)
	for _, w := range weeks {
		if w.status != "ok" {
			rep.Unknown = append(rep.Unknown, w.week)
		}
	}
	rep.Weeks = len(weeks)
	rep.Rewritten, err = write(ctx, tx, homes, acc, weeks, now)
	return rep, err
}

// homesOf turns a sale's home into rows: a share each, a «моё» with an own amount below the net leaves the rest
// nobody's; a home without shares is one row of the whole net. The day of a share is the Moscow day of the work it
// is credited to — the day holding the middle of the work (Hub's `dayOfWork`, as the payout sheet and Hub's own
// export date a shift; Hub's first-shift exception `dayOfShift` needs a fact the contract does not carry). A share
// without a work holding the sale (the owner's split, a «делим» naming someone off the page) is dated by the sale.
func homesOf(s hub.TeamSale, h split.Home, win, model string, works map[string]hub.TeamWork, usernames map[int64]string,
	who *people) []homeRow {
	net := *s.NetMills * 1000
	saleDay := ts.Date(s.OccurredAt)
	base := homeRow{ref: s.TransactionRef, window: win, at: ts.FormatMilli(s.OccurredAt), page: s.PageLabel, model: model,
		kind: s.Kind, net: net, home: h.Kind, reason: h.Reason, pot: h.Pot, day: saleDay}
	if !h.WaitsUntil.IsZero() {
		base.waitsUntil = ts.FormatMilli(h.WaitsUntil)
	}
	for _, ref := range s.Holders {
		if w, ok := works[ref]; ok {
			person, _ := who.person(w.UserID, w.Username, model)
			base.holders = append(base.holders, holder{UserID: w.UserID, Person: person, Day: w.Day(), Kind: w.Kind})
		}
	}
	if len(h.Shares) == 0 {
		base.share = net
		return []homeRow{base}
	}
	var out []homeRow
	var given int64
	for i, sh := range h.Shares {
		r := base
		r.seq, r.user, r.workRef, r.share = i, sh.UserID, sh.WorkRef, sh.Mills*1000
		pct := sh.Percent
		r.percent = &pct
		r.username = usernames[sh.UserID]
		if w, ok := works[sh.WorkRef]; ok {
			r.day, r.username = w.Day(), w.Username
		}
		if r.username == "" {
			r.username = "hub:" + strconv.FormatInt(sh.UserID, 10)
		}
		r.person, _ = who.person(sh.UserID, r.username, model)
		given += r.share
		out = append(out, r)
	}
	if given < net {
		r := base
		r.seq, r.home, r.reason, r.share = len(out), split.Nobody, split.ReasonMarks, net-given
		out = append(out, r)
	}
	return out
}

type weekStatus struct {
	week    string
	status  string
	reasons []string
	readAt  string
}

// weekStatuses: a week's facts are proven when every window whose sales can land in its days is: the two weeks before
// it (a PPV waits 7 days for its sender's shift, which may run past midnight), its own and the next one (a night shift
// of its last day holds sales after midnight), each from the rule's week on, if begun.
func weekStatuses(rules split.Rules, windows []window, now time.Time) []weekStatus {
	byKey := map[string]window{}
	for _, w := range windows {
		byKey[w.key] = w
	}
	first := ts.WeekStart(rules.Since)
	var out []weekStatus
	for wk := first; wk.Before(now); wk = wk.AddDate(0, 0, 7) {
		st := weekStatus{week: ts.Date(wk), status: "ok"}
		for d := -2; d <= 1; d++ {
			need := wk.AddDate(0, 0, 7*d)
			if need.Before(first) || !need.Before(now) {
				continue
			}
			w, ok := byKey[ts.Format(need)]
			switch {
			case !ok:
				st.status = "unknown"
				st.reasons = append(st.reasons, "окно "+ts.Date(need)+" не читалось: ledger hub team-sync")
			case w.status != "ok":
				st.status = "unknown"
				for _, r := range w.reasons {
					st.reasons = append(st.reasons, "окно "+ts.Date(need)+": "+r)
				}
			}
			if ok && (st.readAt == "" || w.readAt < st.readAt) {
				st.readAt = w.readAt
			}
		}
		out = append(out, st)
	}
	return out
}

// write stores the rows week by week: a week whose rows hash as before is left as it is, so the hourly rebuild
// rewrites only the weeks the last read changed (the replica copies every rewritten page). The status of every week
// is written each time.
func write(ctx context.Context, tx *sql.Tx, homes []homeRow, acc map[accrualKey]*accrualRow, weeks []weekStatus, now time.Time) (int, error) {
	homeArgs := map[string][][]any{}
	for _, h := range homes {
		hs := h.holders
		if hs == nil {
			hs = []holder{}
		}
		b, _ := json.Marshal(hs)
		var user, pct any
		if h.user != 0 {
			user = h.user
		}
		if h.percent != nil {
			pct = *h.percent
		}
		wk := weekOf(h.day)
		homeArgs[wk] = append(homeArgs[wk], []any{h.ref, h.seq, h.window, h.at, h.page, h.model, h.kind, h.net, h.home, h.reason,
			nullStr(h.pot), nullStr(h.waitsUntil), user, nullStr(h.username), nullStr(h.person), nullStr(h.workRef), pct, h.share,
			h.day, wk, string(b)})
	}
	keys := make([]accrualKey, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.user != b.user {
			return a.user < b.user
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.page < b.page
	})
	accArgs := map[string][][]any{}
	for _, k := range keys {
		a := acc[k]
		var rate, amount any
		if a.rate != nil {
			rate = *a.rate
		}
		if a.amount != nil {
			amount = *a.amount
		}
		wk := weekOf(k.day)
		accArgs[wk] = append(accArgs[wk], []any{k.user, a.username, nullStr(a.person), k.model, k.page, k.day, wk, a.kind, a.sales,
			a.net, rate, amount, nullStr(a.reason)})
	}
	hashOf := func(wk string) string {
		h := sha256.New()
		for _, r := range homeArgs[wk] {
			fmt.Fprintln(h, r...)
		}
		h.Write([]byte{0})
		for _, r := range accArgs[wk] {
			fmt.Fprintln(h, r...)
		}
		return hex.EncodeToString(h.Sum(nil))[:16]
	}
	stored := map[string][2]string{} // week → hash, computed_at
	rows, err := tx.QueryContext(ctx, `SELECT week, hash, computed_at FROM fact_weeks`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var wk, h, c string
		if err := rows.Scan(&wk, &h, &c); err != nil {
			rows.Close()
			return 0, err
		}
		stored[wk] = [2]string{h, c}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	all := map[string]weekStatus{}
	for _, w := range weeks {
		all[w.week] = w
	}
	for wk := range homeArgs {
		if _, ok := all[wk]; !ok {
			all[wk] = weekStatus{week: wk, status: "unknown", reasons: []string{"неделя вне фактов"}}
		}
	}
	for wk := range accArgs {
		if _, ok := all[wk]; !ok {
			all[wk] = weekStatus{week: wk, status: "unknown", reasons: []string{"неделя вне фактов"}}
		}
	}
	at := ts.Format(now)
	for wk := range stored {
		if _, ok := all[wk]; !ok {
			if err := clearWeek(ctx, tx, wk, true); err != nil {
				return 0, err
			}
		}
	}
	// Clear every changed week before writing any: a sale's row may move to another week (a PPV that waited lands in
	// its sender's shift).
	hashes := map[string]string{}
	var changed []string
	for wk := range all {
		hashes[wk] = hashOf(wk)
		if old, ok := stored[wk]; !ok || old[0] != hashes[wk] {
			changed = append(changed, wk)
			if err := clearWeek(ctx, tx, wk, false); err != nil {
				return 0, err
			}
		}
	}
	sort.Strings(changed)
	for _, wk := range changed {
		for _, r := range homeArgs[wk] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fact_homes(transaction_ref, seq, window_from, occurred_at, page_label, model,
				sale_kind, net_micros, home, reason, pot, waits_until, hub_user_id, username, person, work_ref, percent, share_micros,
				day, week, holders) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r...); err != nil {
				return 0, fmt.Errorf("fact_homes %v/%v: %w", r[0], r[1], err)
			}
		}
		for _, r := range accArgs[wk] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fact_accruals(hub_user_id, username, person, model, page_label, day, week, kind,
				sales, net_micros, rate, amount_micros, reason) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r...); err != nil {
				return 0, err
			}
		}
		stored[wk] = [2]string{hashes[wk], at}
	}
	for wk, w := range all {
		rs := w.reasons
		if rs == nil {
			rs = []string{}
		}
		b, _ := json.Marshal(rs)
		if _, err := tx.ExecContext(ctx, `INSERT INTO fact_weeks(week, status, reasons, read_at, hash, computed_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(week) DO UPDATE SET status = excluded.status, reasons = excluded.reasons, read_at = excluded.read_at,
			hash = excluded.hash, computed_at = excluded.computed_at`,
			wk, w.status, string(b), nullStr(w.readAt), hashes[wk], stored[wk][1]); err != nil {
			return 0, err
		}
	}
	return len(changed), nil
}

// clearWeek deletes a week's rows, and its status too when the week is gone.
func clearWeek(ctx context.Context, tx *sql.Tx, wk string, status bool) error {
	tables := []string{"fact_homes", "fact_accruals"}
	if status {
		tables = append(tables, "fact_weeks")
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE week = ?`, wk); err != nil {
			return err
		}
	}
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// weekOf is the Monday of a Moscow date.
func weekOf(day string) string {
	t, err := time.ParseInLocation(ts.DateLayout, day, ts.MSK)
	if err != nil {
		return day
	}
	return ts.Date(ts.WeekStart(t))
}

func loadWindows(ctx context.Context, q ledger.Querier) ([]window, error) {
	rows, err := q.QueryContext(ctx, `SELECT window_from, window_to, status, reasons, COALESCE(work_to, ''), read_at, COALESCE(ok_at, '')
		FROM hub_team_sync ORDER BY window_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []window
	for rows.Next() {
		var w window
		var from, to, reasons, workTo string
		if err := rows.Scan(&from, &to, &w.status, &reasons, &workTo, &w.readAt, &w.okAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(reasons), &w.reasons)
		w.key = from
		if w.from, err = ts.Parse(from); err != nil {
			return nil, err
		}
		if w.to, err = ts.Parse(to); err != nil {
			return nil, err
		}
		if w.status == "ok" {
			if w.workTo, err = ts.Parse(workTo); err != nil {
				return nil, fmt.Errorf("hub_team_sync %s: work_to %w", from, err)
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func loadWorks(ctx context.Context, q ledger.Querier) (map[string][]hub.TeamWork, error) {
	rows, err := q.QueryContext(ctx, `SELECT window_from, ref, payload FROM hub_works ORDER BY window_from, started_at, ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]hub.TeamWork{}
	for rows.Next() {
		var win, ref, payload string
		if err := rows.Scan(&win, &ref, &payload); err != nil {
			return nil, err
		}
		w, err := hub.DecodeTeamWork([]byte(payload))
		if err != nil {
			return nil, fmt.Errorf("hub_works %s %s: %w", win, ref, err)
		}
		out[win] = append(out[win], w)
	}
	return out, rows.Err()
}

func loadSales(ctx context.Context, q ledger.Querier) (map[string][]hub.TeamSale, error) {
	rows, err := q.QueryContext(ctx, `SELECT window_from, transaction_ref, payload FROM hub_sales ORDER BY window_from, occurred_at, transaction_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]hub.TeamSale{}
	for rows.Next() {
		var win, ref, payload string
		if err := rows.Scan(&win, &ref, &payload); err != nil {
			return nil, err
		}
		s, err := hub.DecodeTeamSale([]byte(payload))
		if err != nil {
			return nil, fmt.Errorf("hub_sales %s: %w", ref, err)
		}
		out[win] = append(out[win], s)
	}
	return out, rows.Err()
}

// people finds the person of a Hub user and their terms on a model's tab: the tab's header, or from
// settings.payroll_terms_hub_from the registry's term holding on the day (ecosystem H3).
type people struct {
	res   *payroll.Resolver
	tabs  []payroll.Terms
	cache map[string][2]string
	// termsFrom is settings.payroll_terms_hub_from; "" — the headers price every day and hub is not read.
	termsFrom string
	hub       payroll.HubTerms
}

func newPeople(ctx context.Context, q ledger.Querier) (*people, error) {
	res, err := payroll.NewResolver(ctx, q)
	if err != nil {
		return nil, err
	}
	terms, err := payroll.LoadTerms(ctx, q)
	if err != nil {
		return nil, err
	}
	p := &people{res: res, tabs: terms, cache: map[string][2]string{}}
	if p.termsFrom, err = payroll.TermsHubFrom(ctx, q); err != nil {
		return nil, err
	}
	if p.termsFrom != "" {
		if p.hub, err = payroll.LoadHubTerms(ctx, q); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// person is the card of a Hub user (payroll.Resolver.HubPerson). "" and why — nobody to credit.
func (p *people) person(user int64, username, model string) (string, string) {
	key := strconv.FormatInt(user, 10) + "\x00" + username + "\x00" + model
	if v, ok := p.cache[key]; ok {
		return v[0], v[1]
	}
	slug, err := p.res.HubPerson(model, user, username)
	reason := ""
	if err != nil {
		reason = "не сопоставлен с people: " + err.Error() + "; добавьте в aliases карточки имя хаба или hub:" +
			strconv.FormatInt(user, 10)
	}
	p.cache[key] = [2]string{slug, reason}
	return slug, reason
}

// terms are the person's terms of the day on their tab of the model: from settings.payroll_terms_hub_from the registry's
// term holding on the day (the first tab of the model takes the day; with no payouts tab, the tab of the books
// «реестр <код>»), else — or with no registry term for the day — the tab's header. Several tabs of one model must agree
// on the header.
func (p *people) terms(person, model, day string) (payroll.Terms, string) {
	found := p.tabsOf(person, model)
	h := p.hubTerm(person, model, day)
	switch {
	case h != nil && len(found) > 0:
		return h.Terms(found[0]), ""
	case h != nil:
		// A registry person with no payouts tab of the model: the tab of the books alone (payroll.RegistryTabName).
		return h.Terms(payroll.Terms{Model: model, Name: payroll.RegistryTabName(person), Person: person}), ""
	case len(found) == 0:
		return payroll.Terms{}, "нет вкладки человека в таблице выплат модели «" + model + "»: ставки нет"
	}
	return p.tabTerms(person, model)
}

// hubTerm is the registry's term that prices the person's day of the model; nil — the tab's header does.
func (p *people) hubTerm(person, model, day string) *payroll.HubTerm {
	if p.termsFrom == "" || day < p.termsFrom {
		return nil
	}
	return p.hub.On(person, model, day)
}

func (p *people) tabsOf(person, model string) []payroll.Terms {
	var found []payroll.Terms
	for _, t := range p.tabs {
		if t.Person == person && ledger.Slug(t.Model) == ledger.Slug(model) {
			found = append(found, t)
		}
	}
	return found
}

// tabTerms are the person's terms on their tab of the model as its header states them. Several tabs of one model must agree.
func (p *people) tabTerms(person, model string) (payroll.Terms, string) {
	found := p.tabsOf(person, model)
	if len(found) == 0 {
		return payroll.Terms{}, "нет вкладки человека в таблице выплат модели «" + model + "»: ставки нет"
	}
	for _, t := range found[1:] {
		if !sameRate(t.Rate, found[0].Rate) || t.ShiftFix != found[0].ShiftFix {
			return payroll.Terms{}, "у человека несколько вкладок модели «" + model + "» с разными ставками: " + found[0].Name + ", " + t.Name
		}
	}
	return found[0], ""
}

func sameRate(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
