package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The team's facts (ledger release 2, L2): Hub's `agentTeamSales` gives a window's work of the team and every sale on
// the key's pages with the facts the split rules read (packages/contracts/src/agent-team.ts in the hub repo). ledger
// reads them by Moscow week — Monday 00:00 to the next Monday, the current week up to the moment of reading — each
// hour the weeks that touch the last 14 days, once a week every week since hub_team_since: the first day Hub has the
// team's works, before the rule's start too (the first window begins on that day). The rule pays from its own week;
// the earlier weeks tell «не занесено» who worked while the sheets still paid. A window is Hub's
// consistent snapshot: its works (from a day before it to 7 days after it) and its sales. An answer that is not whole
// — a refusal, a key without read:team, another window than asked, a holder that is not among the works, a page of
// the grant missing — leaves the window unknown: what it proved before stays, the accruals do not count it.

func init() {
	server.Every("hub-team", time.Hour, func(ctx context.Context, s *server.Server) error {
		cfg, err := ConfigFromEnv()
		if err != nil {
			return err
		}
		sy := NewTeamSyncer(s.Store, cfg)
		sy.Log = func(msg string) { s.Log.Info("hub-team", "msg", msg) }
		rep, err := sy.Hourly(ctx)
		if err != nil {
			return err
		}
		if AfterTeam != nil {
			if err := AfterTeam(ctx, s.Store, sy.Now()); err != nil {
				return err
			}
		}
		if n := rep.Unknown(); n > 0 {
			return fmt.Errorf("хаб не доказал %d окон фактов команды из %d: ledger hub status", n, len(rep.Windows))
		}
		return nil
	})
}

// AfterTeam runs after every read of the team's facts (the hub-team job, `ledger hub team-sync`); the accruals from
// the facts set it, so this package does not depend on them.
var AfterTeam func(ctx context.Context, store *db.Store, now time.Time) error

// TeamPath is Hub's operation of the team's work and sales.
const TeamPath = "/api/v1/agent/team/sales"

const (
	// teamWorkBefore and teamWorkAfter are how far around the window Hub reads the work
	// (AGENT_TEAM_SALES_WORK_BEFORE_DAYS, AGENT_TEAM_SALES_WORK_AFTER_DAYS).
	teamWorkBefore = 24 * time.Hour
	teamWorkAfter  = 7 * 24 * time.Hour
	// teamTail is how far back the hourly read goes; teamFullEvery how often every week since hub_team_since is read.
	teamTail      = 14 * 24 * time.Hour
	teamFullEvery = 7 * 24 * time.Hour
	teamFullAt    = "hub:team:full_at"
)

// SettingTeamSince is the first Moscow day of Hub's per-person works (the chat extension's activity reached Hub then):
// the team's facts are read from it. No window begins earlier, so no read says «nobody worked» where Hub has no data.
const SettingTeamSince = "hub_team_since"

// TeamSince is the first day of the team's facts: hub_team_since, the rule's start without it, never later than it.
func TeamSince(set Settings) (time.Time, error) {
	rule := split.Default.Since
	v := strings.TrimSpace(set.raw[SettingTeamSince])
	if v == "" {
		return rule, nil
	}
	d, err := time.ParseInLocation(ts.DateLayout, v, ts.MSK)
	if err != nil {
		return rule, fmt.Errorf("настройка %s: нужна дата ГГГГ-ММ-ДД, не «%s»", SettingTeamSince, v)
	}
	if d.After(rule) {
		return rule, nil
	}
	return d, nil
}

// TeamWork is one piece of the team's work in Hub's answer.
type TeamWork struct {
	Ref           string         `json:"ref"`
	UserID        int64          `json:"userId"`
	Username      string         `json:"username"`
	Role          string         `json:"role"`
	Kind          string         `json:"kind"`
	StartedAt     time.Time      `json:"startedAt"`
	EndedAt       time.Time      `json:"endedAt"`
	Ongoing       bool           `json:"ongoing"`
	WorkedMinutes int            `json:"workedMinutes"`
	Pages         []TeamWorkPage `json:"pages"`
}

// TeamWorkPage is a page the work worked; SalesFrom is from when it holds the page's sales, nil — it holds none.
type TeamWorkPage struct {
	PageLabel string     `json:"pageLabel"`
	Messages  int        `json:"messages"`
	SalesFrom *time.Time `json:"salesFrom"`
}

// TeamSale is one sale in Hub's answer with the facts whose it is.
type TeamSale struct {
	TransactionRef   string       `json:"transactionRef"`
	PageLabel        string       `json:"pageLabel"`
	Platform         string       `json:"platform"`
	FanRef           *string      `json:"fanRef"`
	Kind             string       `json:"kind"`
	NetMills         *int64       `json:"netMills"`
	OccurredAt       time.Time    `json:"occurredAt"`
	Holders          []string     `json:"holders"`
	PPVSender        *TeamPerson  `json:"ppvSender"`
	PPVSentAt        *time.Time   `json:"ppvSentAt"`
	PPVWithoutAuthor bool         `json:"ppvWithoutAuthor"`
	PostTip          bool         `json:"postTip"`
	Campaign         bool         `json:"campaign"`
	LastWriterUserID *int64       `json:"lastWriterUserId"`
	Writers          []TeamWriter `json:"writers"`
	Marks            []TeamMark   `json:"marks"`
	Resolution       []TeamShare  `json:"resolution"`
}

// TeamPerson is a bought PPV's sender with their role now.
type TeamPerson struct {
	UserID   int64  `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// TeamWriter is one who wrote to the fan in the 48 hours before the sale.
type TeamWriter struct {
	UserID   int64  `json:"userId"`
	Username string `json:"username"`
	Messages int    `json:"messages"`
}

// TeamMark is a chatter's mark of the sale in a shift report: mine («моё»), share («делим») or not_mine («не моё»).
type TeamMark struct {
	UserID          int64  `json:"userId"`
	Mark            string `json:"mark"`
	ShareWithUserID *int64 `json:"shareWithUserId"`
	AmountMills     *int64 `json:"amountMills"`
	// Confirmed: the chatter's confirmed shift report holds the mark. An answer without it (Hub before hub#1101) is a
	// draft: it decides nothing.
	Confirmed bool `json:"confirmed"`
}

// TeamShare is one person's percent of the owner's split of a disputed sale.
type TeamShare struct {
	UserID  int64 `json:"userId"`
	Percent int   `json:"percent"`
}

var (
	teamRoles     = map[string]bool{"owner": true, "team_lead": true, split.RoleChatter: true}
	teamWorkKinds = map[string]bool{split.Shift: true, split.Visit: true}
	teamSaleKinds = map[string]bool{split.SalePPV: true, split.SaleTip: true, split.SaleSubscription: true, split.SalePost: true}
	// Hub's marks → the split's: «делим» is Hub's share, the split's MarkSplit.
	teamMarks = map[string]string{"mine": split.MarkMine, "share": split.MarkSplit, "not_mine": split.MarkNotMine}
)

// Split is the work as the split rules read it: the pages it holds sales of, from when.
func (w TeamWork) Split() split.Work {
	out := split.Work{Ref: w.Ref, UserID: w.UserID, Username: w.Username, Kind: w.Kind, StartedAt: w.StartedAt,
		EndedAt: w.EndedAt, Ongoing: w.Ongoing, Role: w.Role}
	for _, p := range w.Pages {
		if p.SalesFrom != nil {
			out.Pages = append(out.Pages, split.WorkPage{PageLabel: p.PageLabel, From: *p.SalesFrom})
		}
	}
	return out
}

// Day is the Moscow day the work is paid on (split.DayOfWork).
func (w TeamWork) Day() string { return split.DayOfWork(w.Split()) }

// Split is the sale as the split rules read it.
func (s TeamSale) Split() split.Sale {
	out := split.Sale{TransactionRef: s.TransactionRef, PageLabel: s.PageLabel, Platform: s.Platform, Kind: s.Kind,
		OccurredAt: s.OccurredAt, Holders: s.Holders, PPVSentAt: s.PPVSentAt, PostTip: s.PostTip, Campaign: s.Campaign,
		LastWriter: s.LastWriterUserID}
	if s.FanRef != nil {
		out.FanRef = *s.FanRef
	}
	if s.NetMills != nil {
		out.NetMills = *s.NetMills
	}
	if s.PPVSender != nil {
		out.PPVSender = &split.Person{UserID: s.PPVSender.UserID, Username: s.PPVSender.Username, Role: s.PPVSender.Role}
	}
	for _, m := range s.Marks {
		out.Marks = append(out.Marks, split.Mark{UserID: m.UserID, Mark: teamMarks[m.Mark], ShareWith: m.ShareWithUserID,
			OwnAmountMills: m.AmountMills, Confirmed: m.Confirmed})
	}
	for _, r := range s.Resolution {
		out.Resolution = append(out.Resolution, split.OwnerShare{UserID: r.UserID, Percent: r.Percent})
	}
	return out
}

// DecodeTeamWork reads a work kept in hub_works.payload.
func DecodeTeamWork(raw []byte) (TeamWork, error) {
	var w TeamWork
	if err := json.Unmarshal(raw, &w); err != nil {
		return w, err
	}
	return w, w.check()
}

// DecodeTeamSale reads a sale kept in hub_sales.payload.
func DecodeTeamSale(raw []byte) (TeamSale, error) {
	var s TeamSale
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	return s, s.check()
}

func (w TeamWork) check() error {
	switch {
	case w.Ref == "" || w.UserID <= 0:
		return errors.New("работа без ref или userId")
	case !teamRoles[w.Role]:
		return fmt.Errorf("работа %s: роль %q", w.Ref, w.Role)
	case !teamWorkKinds[w.Kind]:
		return fmt.Errorf("работа %s: вид %q", w.Ref, w.Kind)
	case w.StartedAt.IsZero() || w.EndedAt.Before(w.StartedAt):
		return fmt.Errorf("работа %s: начало и конец не по порядку", w.Ref)
	case w.Pages == nil:
		return fmt.Errorf("работа %s без pages", w.Ref)
	}
	for _, p := range w.Pages {
		if p.PageLabel == "" {
			return fmt.Errorf("работа %s: страница без pageLabel", w.Ref)
		}
	}
	return nil
}

func (s TeamSale) check() error {
	switch {
	case s.TransactionRef == "" || s.PageLabel == "":
		return errors.New("продажа без transactionRef или pageLabel")
	case !teamSaleKinds[s.Kind]:
		return fmt.Errorf("продажа %s: вид %q", s.TransactionRef, s.Kind)
	case s.NetMills == nil:
		return fmt.Errorf("продажа %s без netMills", s.TransactionRef)
	case s.OccurredAt.IsZero():
		return fmt.Errorf("продажа %s без occurredAt", s.TransactionRef)
	case s.Holders == nil || s.Marks == nil || s.Writers == nil:
		return fmt.Errorf("продажа %s без holders, writers или marks", s.TransactionRef)
	case s.PPVSender != nil && (s.PPVSender.UserID <= 0 || !teamRoles[s.PPVSender.Role]):
		return fmt.Errorf("продажа %s: отправитель PPV не по контракту", s.TransactionRef)
	}
	for _, m := range s.Marks {
		if m.UserID <= 0 || teamMarks[m.Mark] == "" {
			return fmt.Errorf("продажа %s: метка %q", s.TransactionRef, m.Mark)
		}
	}
	for _, r := range s.Resolution {
		if r.UserID <= 0 || r.Percent < 0 || r.Percent > 100 {
			return fmt.Errorf("продажа %s: решение владельца не по контракту", s.TransactionRef)
		}
	}
	return nil
}

// teamAnswer is one window's answer, each work and sale with its raw JSON.
type teamAnswer struct {
	from, to, workFrom, workTo, serverNow time.Time
	pages                                 []string
	works                                 []TeamWork
	worksRaw                              []json.RawMessage
	sales                                 []TeamSale
	salesRaw                              []json.RawMessage
}

// decodeTeam reads the answer by the contract; anything missing or of another shape is an error.
func decodeTeam(raw []byte) (*teamAnswer, error) {
	var env struct {
		From      *time.Time        `json:"from"`
		To        *time.Time        `json:"to"`
		WorkFrom  *time.Time        `json:"workFrom"`
		WorkTo    *time.Time        `json:"workTo"`
		ServerNow *time.Time        `json:"serverNow"`
		Pages     []string          `json:"pages"`
		Works     []json.RawMessage `json:"works"`
		Sales     []json.RawMessage `json:"sales"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.From == nil || env.To == nil || env.WorkFrom == nil || env.WorkTo == nil || env.ServerNow == nil ||
		env.Pages == nil || env.Works == nil || env.Sales == nil {
		return nil, errors.New("нет from, to, workFrom, workTo, serverNow, pages, works или sales")
	}
	a := &teamAnswer{from: *env.From, to: *env.To, workFrom: *env.WorkFrom, workTo: *env.WorkTo, serverNow: *env.ServerNow,
		pages: env.Pages, worksRaw: env.Works, salesRaw: env.Sales}
	for _, r := range env.Works {
		w, err := DecodeTeamWork(r)
		if err != nil {
			return nil, err
		}
		a.works = append(a.works, w)
	}
	for _, r := range env.Sales {
		s, err := DecodeTeamSale(r)
		if err != nil {
			return nil, err
		}
		a.sales = append(a.sales, s)
	}
	return a, nil
}

// whole lists why the answer is not the whole window asked (none — it is): another window, works not covering the
// rule's wait, a holder that is not among the works, a sale outside the window or its pages, a page of the grant
// missing.
func (a *teamAnswer) whole(from, to time.Time, grant []string) []string {
	var out []string
	add := func(format string, args ...any) {
		if len(out) < 5 {
			out = append(out, fmt.Sprintf(format, args...))
		} else if len(out) == 5 {
			out = append(out, "и другое")
		}
	}
	if !a.from.Equal(from) || !a.to.Equal(to) {
		add("хаб ответил за %s..%s, а спрошено %s..%s", iso(a.from), iso(a.to), iso(from), iso(to))
	}
	needTo := to.Add(teamWorkAfter)
	if a.serverNow.Before(needTo) {
		needTo = a.serverNow
	}
	if a.workFrom.After(from.Add(-teamWorkBefore)) || a.workTo.Before(needTo) {
		add("работы прочитаны за %s..%s, нужно с %s до %s", iso(a.workFrom), iso(a.workTo), iso(from.Add(-teamWorkBefore)),
			iso(needTo))
	}
	refs := map[string]bool{}
	for _, w := range a.works {
		if refs[w.Ref] {
			add("работа %s дважды", w.Ref)
		}
		refs[w.Ref] = true
	}
	pages := map[string]bool{}
	for _, p := range a.pages {
		pages[p] = true
	}
	for _, p := range grant {
		if !pages[p] {
			add("страницы %s из гранта ключа нет в ответе", p)
		}
	}
	seen := map[string]bool{}
	for _, s := range a.sales {
		if seen[s.TransactionRef] {
			add("продажа %s дважды", s.TransactionRef)
		}
		seen[s.TransactionRef] = true
		if s.OccurredAt.Before(from) || !s.OccurredAt.Before(to) {
			add("продажа %s в %s вне окна", s.TransactionRef, iso(s.OccurredAt))
		}
		if !pages[s.PageLabel] {
			add("продажа %s на странице %s не из pages ответа", s.TransactionRef, s.PageLabel)
		}
		for _, h := range s.Holders {
			if !refs[h] {
				add("держателя %s продажи %s нет среди работ", h, s.TransactionRef)
			}
		}
	}
	return out
}

// TeamSyncer reads the team's facts window by window.
type TeamSyncer struct {
	Store  *db.Store
	Client *Client
	Now    func() time.Time
	Log    func(string)
	// Since is the first instant read: no window begins earlier. Zero — TeamSince of the settings.
	Since time.Time
}

// NewTeamSyncer builds a syncer that shares the hub_rpm throttle with the dataset sync.
func NewTeamSyncer(store *db.Store, cfg Config) *TeamSyncer {
	return &TeamSyncer{Store: store, Client: NewClient(cfg, &Throttle{DB: store.RW}), Now: time.Now}
}

func (s *TeamSyncer) since(set Settings) (time.Time, error) {
	if !s.Since.IsZero() {
		return s.Since, nil
	}
	return TeamSince(set)
}

// TeamWindow is one window of a run.
type TeamWindow struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Status   string   `json:"status"`
	Reasons  []string `json:"reasons,omitempty"`
	Works    int      `json:"works"`
	Sales    int      `json:"sales"`
	NetMills int64    `json:"netMills"`
	Requests int      `json:"requests"`
}

// TeamReport is one run.
type TeamReport struct {
	At       string       `json:"at"`
	Full     bool         `json:"full,omitempty"`
	Requests int          `json:"requests"`
	Windows  []TeamWindow `json:"windows"`
}

// Unknown counts the windows the hub did not prove.
func (r TeamReport) Unknown() int {
	n := 0
	for _, w := range r.Windows {
		if w.Status != "ok" {
			n++
		}
	}
	return n
}

func (s *TeamSyncer) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

// Hourly reads the weeks that touch the last 14 days, or every week since hub_team_since when the last full read that
// proved them all is a week old.
func (s *TeamSyncer) Hourly(ctx context.Context) (TeamReport, error) {
	now := s.Now()
	set, err := LoadSettings(ctx, s.Store.RW)
	if err != nil {
		return TeamReport{}, err
	}
	since, err := s.since(set)
	if err != nil {
		return TeamReport{}, err
	}
	var last string
	err = s.Store.RW.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, teamFullAt).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TeamReport{}, err
	}
	full := true
	if t, perr := ts.Parse(last); err == nil && perr == nil && now.Sub(t) < teamFullEvery {
		full = false
	}
	from := now.Add(-teamTail)
	if full {
		from = since
	}
	rep, err := s.Run(ctx, from, now)
	rep.Full = full
	if err != nil || !full || rep.Unknown() > 0 {
		return rep, err
	}
	return rep, writeTx(ctx, s.Store, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, teamFullAt, ts.Format(now), ts.Format(now))
		return err
	})
}

type teamSpan struct{ from, to time.Time }

// teamWindows are the Moscow weeks that touch [from, to), none before since (the week of since begins at it) and none
// not begun by now; the current week ends at now.
func teamWindows(since, from, to, now time.Time) []teamSpan {
	if from.Before(since) {
		from = since
	}
	var out []teamSpan
	for w := ts.WeekStart(from); w.Before(to) && w.Before(now); w = w.AddDate(0, 0, 7) {
		start, end := w, w.AddDate(0, 0, 7)
		if start.Before(since) {
			start = since
		}
		if end.After(now) {
			end = now.Truncate(time.Second)
		}
		if end.After(start) {
			out = append(out, teamSpan{start, end})
		}
	}
	return out
}

// Run reads every week that touches [from, to), one request at a time. A refused or partial answer marks its window
// unknown and the run goes on; a key the hub refuses (401, 403) is not asked again in the run.
func (s *TeamSyncer) Run(ctx context.Context, from, to time.Time) (TeamReport, error) {
	now := s.Now()
	rep := TeamReport{At: ts.Format(now), Windows: []TeamWindow{}}
	release, err := takeLease(ctx, s.Store, "hub:team:lease", "идёт другая выгрузка фактов команды", now)
	if err != nil {
		return rep, err
	}
	defer release()
	set, err := LoadSettings(ctx, s.Store.RW)
	if err != nil {
		return rep, err
	}
	rpm, err := set.Int("hub_rpm")
	if err != nil {
		return rep, err
	}
	since, err := s.since(set)
	if err != nil {
		return rep, err
	}
	if s.Client.Throttle != nil {
		s.Client.Throttle.RPM = rpm
	}
	grant, err := grantLabels(ctx, s.Store.RW)
	if err != nil {
		return rep, err
	}
	calls0 := s.Client.Calls
	defer func() { rep.Requests = s.Client.Calls - calls0 }()
	refused := "" // the key's refusal: the other windows are not asked
	for _, w := range teamWindows(since, from, to, now) {
		res := TeamWindow{From: ts.Format(w.from), To: ts.Format(w.to)}
		c0 := s.Client.Calls
		var a *teamAnswer
		var reasons []string
		if refused != "" {
			reasons = []string{refused}
		} else if a, err = s.fetch(ctx, w); err != nil {
			if !IsRefusal(err) {
				return rep, err
			}
			reasons = []string{err.Error()}
			var r *Refusal
			if errors.As(err, &r) && (r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden) {
				refused = keyRefusal(r)
				reasons = []string{refused}
			}
		} else {
			reasons = a.whole(w.from, w.to, grant)
		}
		res.Requests = s.Client.Calls - c0
		if len(reasons) > 0 {
			res.Status, res.Reasons = "unknown", reasons
			err = writeTx(ctx, s.Store, func(tx *sql.Tx) error { return failTeam(ctx, tx, w, reasons, res.Requests, now) })
			s.logf("%s..%s: неизвестно — %v", res.From, res.To, reasons)
		} else {
			res.Status, res.Works, res.Sales = "ok", len(a.works), len(a.sales)
			for _, sale := range a.sales {
				res.NetMills += *sale.NetMills
			}
			err = writeTx(ctx, s.Store, func(tx *sql.Tx) error { return saveTeam(ctx, tx, w, a, res, now) })
			s.logf("%s..%s: ok, работ %d, продаж %d", res.From, res.To, res.Works, res.Sales)
		}
		if err != nil {
			return rep, err
		}
		rep.Windows = append(rep.Windows, res)
	}
	return rep, nil
}

// keyRefusal says what the owner does about a key the hub refuses.
func keyRefusal(r *Refusal) string {
	if r.Status == http.StatusForbidden {
		return "у ключа ledger в хабе нет права read:team (или read:money): его выдаёт владелец в консоли хаба, " +
			"Settings → агентские ключи — " + r.Msg
	}
	return "ключ хаба не принят — " + r.Msg
}

func (s *TeamSyncer) fetch(ctx context.Context, w teamSpan) (*teamAnswer, error) {
	what := fmt.Sprintf("team/sales %s..%s", iso(w.from), iso(w.to))
	raw, err := s.Client.do(ctx, what, true, http.MethodPost, TeamPath, map[string]string{"from": iso(w.from), "to": iso(w.to)})
	if err != nil {
		return nil, err
	}
	a, err := decodeTeam(raw)
	if err != nil {
		return nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	return a, nil
}

// grantLabels are the pages of the key's grant the dataset sync last saw; none known — nothing to compare with.
func grantLabels(ctx context.Context, q ledger.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT page_label FROM hub_pages ORDER BY page_label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// saveTeam replaces the window's works and sales with the answer and marks it proven.
func saveTeam(ctx context.Context, tx *sql.Tx, w teamSpan, a *teamAnswer, res TeamWindow, now time.Time) error {
	win, at := ts.Format(w.from), ts.Format(now)
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_works WHERE window_from = ?`, win); err != nil {
		return err
	}
	for i, x := range a.works {
		ongoing := 0
		if x.Ongoing {
			ongoing = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_works(window_from, ref, user_id, username, role, kind, started_at, ended_at,
			ongoing, worked_minutes, day, payload, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			win, x.Ref, x.UserID, x.Username, x.Role, x.Kind, ts.FormatMilli(x.StartedAt), ts.FormatMilli(x.EndedAt), ongoing,
			x.WorkedMinutes, x.Day(), string(a.worksRaw[i]), at); err != nil {
			return err
		}
	}
	refs := make([]string, 0, len(a.sales))
	for i, x := range a.sales {
		refs = append(refs, x.TransactionRef)
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills,
			occurred_at, ppv_sender_user_id, last_writer_user_id, post_tip, campaign, payload, first_seen_at, read_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(transaction_ref) DO UPDATE SET window_from = excluded.window_from, page_label = excluded.page_label,
			platform = excluded.platform, kind = excluded.kind, net_mills = excluded.net_mills, occurred_at = excluded.occurred_at,
			ppv_sender_user_id = excluded.ppv_sender_user_id, last_writer_user_id = excluded.last_writer_user_id,
			post_tip = excluded.post_tip, campaign = excluded.campaign, payload = excluded.payload, read_at = excluded.read_at`,
			x.TransactionRef, win, x.PageLabel, x.Platform, x.Kind, *x.NetMills, ts.FormatMilli(x.OccurredAt), senderOf(x),
			x.LastWriterUserID, bit(x.PostTip), bit(x.Campaign), string(a.salesRaw[i]), at, at); err != nil {
			return err
		}
	}
	// A sale that left the hub (refunded, no longer active) leaves the window.
	keep, _ := json.Marshal(refs)
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_sales WHERE window_from = ?
		AND transaction_ref NOT IN (SELECT value FROM json_each(?))`, win, string(keep)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO hub_team_sync(window_from, window_to, status, reasons, works, sales, net_mills,
		work_from, work_to, server_now, requests, read_at, ok_at) VALUES (?, ?, 'ok', '[]', ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(window_from) DO UPDATE SET window_to = excluded.window_to, status = 'ok', reasons = '[]',
		works = excluded.works, sales = excluded.sales, net_mills = excluded.net_mills, work_from = excluded.work_from,
		work_to = excluded.work_to, server_now = excluded.server_now, requests = excluded.requests, read_at = excluded.read_at,
		ok_at = excluded.ok_at`,
		win, ts.Format(w.to), res.Works, res.Sales, res.NetMills, ts.FormatMilli(a.workFrom), ts.FormatMilli(a.workTo),
		ts.FormatMilli(a.serverNow), res.Requests, at, at)
	return err
}

// failTeam marks the window unknown; what it proved before stays, but the accruals refuse it until it is proven again.
func failTeam(ctx context.Context, tx *sql.Tx, w teamSpan, reasons []string, requests int, now time.Time) error {
	rs, _ := json.Marshal(reasons)
	_, err := tx.ExecContext(ctx, `INSERT INTO hub_team_sync(window_from, window_to, status, reasons, requests, read_at)
		VALUES (?, ?, 'unknown', ?, ?, ?)
		ON CONFLICT(window_from) DO UPDATE SET window_to = excluded.window_to, status = 'unknown', reasons = excluded.reasons,
		requests = excluded.requests, read_at = excluded.read_at`,
		ts.Format(w.from), ts.Format(w.to), string(rs), requests, ts.Format(now))
	return err
}

func senderOf(s TeamSale) any {
	if s.PPVSender == nil {
		return nil
	}
	return s.PPVSender.UserID
}

func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TeamStatus is one row of hub_team_sync.
type TeamStatus struct {
	WindowFrom string   `json:"windowFrom"`
	WindowTo   string   `json:"windowTo"`
	Status     string   `json:"status"`
	Reasons    []string `json:"reasons,omitempty"`
	Works      *int64   `json:"works,omitempty"`
	Sales      *int64   `json:"sales,omitempty"`
	NetMills   *int64   `json:"netMills,omitempty"`
	WorkTo     string   `json:"workTo,omitempty"`
	ReadAt     string   `json:"readAt"`
	OkAt       string   `json:"okAt,omitempty"`
}

// TeamStatuses lists every window of the team's facts, oldest first.
func TeamStatuses(ctx context.Context, q ledger.Querier) ([]TeamStatus, error) {
	rows, err := q.QueryContext(ctx, `SELECT window_from, window_to, status, reasons, works, sales, net_mills, COALESCE(work_to, ''),
		read_at, COALESCE(ok_at, '') FROM hub_team_sync ORDER BY window_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamStatus{}
	for rows.Next() {
		var t TeamStatus
		var reasons string
		var works, sales, net sql.NullInt64
		if err := rows.Scan(&t.WindowFrom, &t.WindowTo, &t.Status, &reasons, &works, &sales, &net, &t.WorkTo, &t.ReadAt, &t.OkAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(reasons), &t.Reasons)
		if works.Valid {
			t.Works, t.Sales, t.NetMills = &works.Int64, &sales.Int64, &net.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TeamSummary is the team's facts in `ledger hub status`: proven unless a window is unknown.
func TeamSummary(sts []TeamStatus) map[string]any {
	unknown := 0
	for _, s := range sts {
		if s.Status != "ok" {
			unknown++
		}
	}
	out := map[string]any{"ok": unknown == 0 && len(sts) > 0, "unknown": unknown, "windows": sts}
	if len(sts) == 0 {
		out["note"] = "факты команды ещё не читались: ledger hub team-sync или джоб hub-team"
	}
	return out
}
