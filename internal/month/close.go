package month

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Unrecorded lists the Bybit incomes of the month without a journal row: the movement key in a note,
// or the same amount on that date (for a transfer: recorded − withheld − route fee).
func Unrecorded(ctx context.Context, q ledger.Querier, month string, rows []journal.Row) ([]string, bool) {
	res, err := q.QueryContext(ctx, `SELECT source_id, substr(occurred_at, 1, 10), payload FROM events WHERE source = 'bybit'
		AND kind IN ('deposit', 'internal_deposit', 'p2p') AND json_extract(payload, '$.direction') = 'income' AND substr(occurred_at, 1, 7) = ?`, month)
	if err != nil {
		return nil, false
	}
	defer res.Close()
	var notes strings.Builder
	for _, r := range rows {
		notes.WriteString(r.Note)
		notes.WriteByte('\n')
	}
	text := notes.String()
	var out []string
	for res.Next() {
		var key, day, payload string
		if err := res.Scan(&key, &day, &payload); err != nil {
			return nil, false
		}
		var p struct {
			Amount json.RawMessage `json:"amount"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, false
		}
		amt, err := strconv.ParseFloat(strings.Trim(string(p.Amount), `"`), 64)
		if err != nil {
			return nil, false
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `\b`).MatchString(text) {
			continue
		}
		found := false
		for _, r := range rows {
			if r.Type != "income" || r.Date != day {
				continue
			}
			v := mustFloat(r.Amount)
			cand := []float64{v}
			if t, err := journal.RowTags(r); err == nil && t.Transfer != nil {
				cand = append(cand, v-sum(t.Transfer.Held)-sum(t.Transfer.Fee))
			} else {
				cand = append(cand, v)
			}
			for _, s := range cand {
				found = found || math.Abs(s-amt) <= .01
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("%s %s %v", day, key, amt))
		}
	}
	return out, res.Err() == nil
}

// OpenBrief lists undecided items of the month from the proposals (no date counts for every month).
func OpenBrief(ctx context.Context, q ledger.Querier, month string) ([]string, bool) {
	res, err := q.QueryContext(ctx, `SELECT key, summary, COALESCE(json_extract(content, '$.date'), '') FROM proposals WHERE status IN ('open', 'deferred')`)
	if err != nil {
		return nil, false
	}
	defer res.Close()
	var out []string
	for res.Next() {
		var key, summary, date string
		if err := res.Scan(&key, &summary, &date); err != nil {
			return nil, false
		}
		if date == "" || strings.HasPrefix(date, month) {
			out = append(out, key+": "+summary)
		}
	}
	return out, res.Err() == nil
}

// Blockers are the conditions that stop a close: failed checks, before the 8th, money still in transit
// above $100, undecided brief items, Bybit incomes without a journal row.
func Blockers(res []Check, tot Totals, now, m1 time.Time, open []string, openOK bool, lost []string, lostOK bool) []string {
	var stop []string
	for _, c := range res {
		if !c.OK {
			stop = append(stop, fmt.Sprintf("проверка %d: %s", c.N, c.Msg))
		}
	}
	if earliest := m1.AddDate(0, 0, 7); now.Before(earliest) {
		stop = append(stop, fmt.Sprintf("до 8-го (%s 00:00 МСК по часам) месяц не закрывается", earliest.In(ts.MSK).Format("02.01")))
	}
	if tot.Transit > 100 {
		stop = append(stop, fmt.Sprintf("в пути без «застряло» %.2f > $100 (холд %.2f, на странице %.2f, едет %.2f) — ждать прихода, крайний срок 18-е",
			tot.Transit, tot.Hold, tot.Page, tot.Riding))
	}
	if !openOK {
		stop = append(stop, "ночной бриф: не читается — не проверено")
	} else if len(open) > 0 {
		stop = append(stop, "ночной бриф: "+strings.Join(open, "; "))
	}
	if !lostOK {
		stop = append(stop, "приходы Bybit без строки ledger: движения не читаются — не проверено")
	} else if len(lost) > 0 {
		stop = append(stop, "приходы Bybit без строки ledger: "+strings.Join(lost, "; "))
	}
	return stop
}

// sheetsMaxAge is how old the reading of a payout table may be at a close: the collector runs hourly on the
// Mac, and the script it replaces read the tables live.
const sheetsMaxAge = 3 * time.Hour

// SheetsStale lists the payout tables read too long ago for an immutable snapshot.
func SheetsStale(ctx context.Context, q ledger.Querier, now time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT model, fetched_at FROM payroll_tables ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var model, at string
		if err := rows.Scan(&model, &at); err != nil {
			return nil, err
		}
		t, err := ts.Parse(at)
		if err != nil {
			return nil, fmt.Errorf("payroll_tables.fetched_at: %w", err)
		}
		if now.Sub(t) > sheetsMaxAge {
			out = append(out, fmt.Sprintf("таблица выплат %s прочитана %s: сборщик мака не работает, закрытие без свежих таблиц занизит расходы", model, at))
		}
	}
	return out, rows.Err()
}

// Close writes the closing snapshot of a month once, or returns why it cannot be closed.
func Close(ctx context.Context, tx *sql.Tx, month string, asOf time.Time, ceoRate float64, now time.Time, source string) (*Card, []string, error) {
	m0, m1, err := ts.MonthStart(month)
	if err != nil {
		return nil, nil, err
	}
	_ = m0
	if _, err := ledger.GetPeriod(ctx, tx, month); err == nil {
		return nil, []string{month + " уже закрыт — снимок закрытия не меняется"}, nil
	} else if err != sql.ErrNoRows {
		return nil, nil, err
	}
	cfg, err := LoadConfig(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := JournalRows(ctx, tx, "")
	if err != nil {
		return nil, nil, err
	}
	tabs, tabsErr, err := LoadTabs(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	src := DBSource{Ctx: ctx, Q: tx}
	cv, err := BuildCover(cfg, src, rows, asOf)
	if err != nil {
		return nil, nil, err
	}
	if tabs == nil {
		tabs = []Tab{}
	}
	card, err := BuildCard(cv, month, tabs)
	if err != nil {
		return nil, nil, err
	}
	res := card.Checks(tabsErr)
	tot := card.Totals()
	open, openOK := OpenBrief(ctx, tx, month)
	lost, lostOK := Unrecorded(ctx, tx, month, cv.Ledger)
	stop := Blockers(res, tot, now, m1, open, openOK, lost, lostOK)
	stale, err := SheetsStale(ctx, tx, now)
	if err != nil {
		return nil, nil, err
	}
	stop = append(stop, stale...)
	if len(stop) > 0 {
		return card, stop, nil
	}
	snap := card.Snapshot()
	candidate := ceoRate + cv.Cfg.GuardStep
	verdict := Verdict(*tot.Decision, candidate, cv.Cfg.GuardShare, &tot.Transit)
	snap["status"], snap["source"], snap["closed_at"] = "закрыт", source, ts.Format(now)
	snap["ceo_rate"], snap["candidate"], snap["verdict"] = ceoRate, candidate, verdict
	if from, err := raiseFrom(ctx, tx, month, candidate); err != nil {
		return nil, nil, err
	} else if from != "" {
		snap["raise_from"] = from
	}
	snap["checks"] = res
	snap["payroll"] = PayrollOf(tabs, month)
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, nil, err
	}
	dec := money.FromFloat(money.Round2(*tot.Decision))
	if err := ledger.Lock(ctx, tx, ledger.Period{Month: month, LockedAt: ts.Format(now), AsOf: ts.Format(asOf), Snapshot: raw,
		Decision: &dec, Verdict: verdict, Source: source}); err != nil {
		return nil, nil, err
	}
	return card, nil, nil
}

// ImportClose locks a month with a closing snapshot written elsewhere (/OS finance/months/ГГГГ-ММ.json).
func ImportClose(ctx context.Context, tx *sql.Tx, raw []byte) (string, error) {
	var s struct {
		Month    string   `json:"month"`
		ClosedAt string   `json:"closed_at"`
		AsOf     string   `json:"as_of"`
		Decision *float64 `json:"decision"`
		Verdict  string   `json:"verdict"`
		Source   string   `json:"source"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	if s.Month == "" || s.Decision == nil {
		return "", fmt.Errorf("снимок без month или decision")
	}
	if s.ClosedAt == "" {
		_, m1, err := ts.MonthStart(s.Month)
		if err != nil {
			return "", err
		}
		s.ClosedAt = ts.Format(m1)
	}
	if s.AsOf == "" {
		s.AsOf = s.ClosedAt
	}
	la, err := ts.Parse(s.ClosedAt)
	if err != nil {
		return "", err
	}
	ao, err := ts.Parse(s.AsOf)
	if err != nil {
		return "", err
	}
	dec := money.FromFloat(*s.Decision)
	return s.Month, ledger.Lock(ctx, tx, ledger.Period{Month: s.Month, LockedAt: ts.Format(la), AsOf: ts.Format(ao), Snapshot: raw,
		Decision: &dec, Verdict: s.Verdict, Source: s.Source})
}

// DriftRow is a closed month: «сейчас» against what the owner was told.
type DriftRow struct {
	Month        string   `json:"month"`
	AtClose      float64  `json:"at_close"`
	VerdictClose string   `json:"verdict_at_close"`
	Reported     float64  `json:"reported"`
	VerdictTold  string   `json:"verdict_reported"`
	Now          float64  `json:"now"`
	VerdictNow   string   `json:"verdict_now"`
	Delta        float64  `json:"delta"`
	Same         bool     `json:"same_side"`
	Failed       []string `json:"failed_checks,omitempty"`
	Flag         bool     `json:"flag"`
	Reason       string   `json:"reason,omitempty"`
}

// Drift compares every closed month now with the figure the owner knows: a signal at |Δ| ≥ $100, a
// changed side of the verdict or a failed check 1–3.
func Drift(ctx context.Context, q ledger.Querier, cv *Cover, tabs []Tab) ([]DriftRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT month, snapshot, COALESCE(decision, 0), COALESCE(verdict, '') FROM periods ORDER BY month`)
	if err != nil {
		return nil, err
	}
	type closed struct {
		month, verdict string
		decision       int64
		snap           map[string]any
	}
	var list []closed
	for rows.Next() {
		var c closed
		var raw string
		if err := rows.Scan(&c.month, &raw, &c.decision, &c.verdict); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &c.snap)
		list = append(list, c)
	}
	rows.Close()
	var out []DriftRow
	for _, c := range list {
		card, err := BuildCard(cv, c.month, tabs)
		if err != nil {
			return nil, err
		}
		t := card.Totals()
		dec := *t.Decision
		atClose := money.Micros(c.decision).Float()
		rep, rv := atClose, c.verdict
		var repM int64
		var repV string
		err = q.QueryRowContext(ctx, `SELECT decision, verdict FROM period_reports WHERE month = ? ORDER BY id DESC LIMIT 1`, c.month).Scan(&repM, &repV)
		if err == nil {
			rep, rv = money.Micros(repM).Float(), repV
		} else if err != sql.ErrNoRows {
			return nil, err
		}
		v := c.verdict
		if cand, ok := c.snap["candidate"].(float64); ok {
			v = Verdict(dec, cand, card.Cover.Cfg.GuardShare, nil)
		}
		d := money.Round2(money.Round2(dec) - rep)
		r := DriftRow{Month: c.month, AtClose: atClose, VerdictClose: c.verdict, Reported: rep, VerdictTold: rv, Now: money.Round2(dec), VerdictNow: v, Delta: d,
			Same: (v == "не проходит") == (rv == "не проходит")}
		for _, ch := range card.Checks("") {
			if !ch.OK && ch.N <= 3 {
				r.Failed = append(r.Failed, fmt.Sprintf("проверка %d", ch.N))
			}
		}
		r.Flag = math.Abs(d) >= 100 || !r.Same || len(r.Failed) > 0
		r.Reason = reasonOf(c.snap, card)
		out = append(out, r)
	}
	return out, nil
}

// Accept records «сейчас» as what the owner now knows (months --accept).
func Accept(ctx context.Context, q ledger.Querier, rows []DriftRow, now time.Time) error {
	for _, r := range rows {
		if _, err := q.ExecContext(ctx, `INSERT INTO period_reports(month, reported_at, decision, verdict, note) VALUES (?, ?, ?, ?, 'принято владельцем')`,
			r.Month, ts.Format(now), int64(money.FromFloat(r.Now)), r.VerdictNow); err != nil {
			return err
		}
	}
	return nil
}

// ImportReported reads the «Сообщено» columns of /OS finance/months.md into period_reports.
func ImportReported(ctx context.Context, tx *sql.Tx, path string, now time.Time) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`^\d{4}-\d{2}$`)
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) != 11 || !re.MatchString(cells[0]) {
			continue
		}
		v := strings.ReplaceAll(strings.ReplaceAll(cells[9], " ", ""), " ", "")
		v = strings.ReplaceAll(strings.ReplaceAll(v, "−", "-"), "—", "0")
		m, err := money.Parse(v)
		if err != nil {
			return n, fmt.Errorf("months.md %s: %w", cells[0], err)
		}
		var exists int
		_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM period_reports WHERE month = ? AND decision = ? AND verdict = ?`, cells[0], int64(m), cells[10]).Scan(&exists)
		if exists > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO period_reports(month, reported_at, decision, verdict, note) VALUES (?, ?, ?, ?, 'импорт months.md')`,
			cells[0], ts.Format(now), int64(m), cells[10]); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// reasonOf names the 1–3 biggest contributions to «сейчас − на закрытии» of a closed month.
func reasonOf(s map[string]any, c *Card) string {
	linesAny, _ := s["lines"].([]any)
	if len(linesAny) == 0 {
		return "закрыт старым методом, пересчитан новым"
	}
	name := func(m string) string {
		if n, ok := c.Cover.Cfg.Names[m]; ok {
			return n
		}
		return m
	}
	old := map[string]float64{}
	for _, l := range linesAny {
		m, _ := l.(map[string]any)
		model, _ := m["model"].(string)
		ch, _ := m["channel"].(string)
		e, _ := m["earned"].(float64)
		old[name(model)+" · "+ch] = e
	}
	if tot, ok := s["totals"].(map[string]any); ok {
		st, _ := tot["stuck"].(float64)
		old["застряло"] = -st
	}
	if exp, ok := s["expenses"].(map[string]any); ok {
		if chat, ok := exp["chat"].(map[string]any); ok {
			for m, v := range chat {
				f, _ := v.(float64)
				old["чаттеры · "+name(m)] = -f
			}
		}
		x := 0.0
		if ex, ok := exp["extra"].([]any); ok {
			for _, it := range ex {
				if row, ok := it.([]any); ok && len(row) > 1 {
					f, _ := row[1].(float64)
					x += f
				}
			}
		}
		old["выплаты сверх таблиц"] = -x
		if cats, ok := exp["cats"].(map[string]any); ok {
			for k, v := range cats {
				f, _ := v.(float64)
				old["расходы «"+k+"»"] = -f
			}
		}
	}
	now := map[string]float64{}
	for _, l := range c.Lines {
		now[name(l.Model)+" · "+l.Channel] = l.Vals.Vals["earned"]
	}
	now["застряло"] = -c.Totals().Stuck
	if c.Exp != nil {
		for _, m := range c.Exp.Chat.Keys {
			now["чаттеры · "+name(m)] = -c.Exp.Chat.Vals[m]
		}
		x := 0.0
		for _, e := range c.Exp.Extra {
			x += e.Amount
		}
		now["выплаты сверх таблиц"] = -x
		for _, k := range c.Exp.Cats.Keys {
			now["расходы «"+k+"»"] = -c.Exp.Cats.Vals[k]
		}
	}
	type kv struct {
		k string
		v float64
	}
	var d []kv
	seen := map[string]bool{}
	for k := range old {
		seen[k] = true
	}
	for k := range now {
		seen[k] = true
	}
	for k := range seen {
		d = append(d, kv{k, now[k] - old[k]})
	}
	sort.Slice(d, func(i, j int) bool {
		if math.Abs(d[i].v) != math.Abs(d[j].v) {
			return math.Abs(d[i].v) > math.Abs(d[j].v)
		}
		return d[i].k < d[j].k
	})
	var parts []string
	for i := 0; i < 3 && i < len(d); i++ {
		if math.Abs(d[i].v) >= .5 {
			parts = append(parts, d[i].k+" "+USD(d[i].v, true))
		}
	}
	if len(parts) == 0 {
		return "правки меньше $1 по каждой статье"
	}
	return strings.Join(parts, ", ")
}
