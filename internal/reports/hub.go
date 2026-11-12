package reports

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The third source of a shift report (ecosystem F2): the report a person hands in in Hub — in Workboard, the chat
// extension, or by the team's agent «со слов» (hub#1225). The job hub-reports reads Hub's feed every 5 minutes after a
// cursor (cursors hub:shift_reports) with ledger's Hub key and keeps every version in hub_shift_reports and its pages,
// lines and «мимо площадки». A version is known by its shift and number: read again, it writes nothing. A newer version
// of a shift supersedes the older (current); two shifts of one person on one day are two reports, and v_report_pages adds
// their pages for the day (only this source: a post and an agent's draft keep «the last one wins»). A version that cannot
// be stored — the person's code is not in people, a page not in hub_pages, sums that do not add up — is kept with its
// problem and no pages, tried again on every pass, and shown by `ledger health`: never skipped, never a guessed person.
// What a version accrues is decided elsewhere: v_report_pages.basis by settings.payroll_basis_hub_from (empty — nothing).

// SourceHub is a report handed in in Hub.
const SourceHub = "hub"

// Bases of a day in v_report_pages: what accrues it.
const (
	BasisHub  = "hub"
	BasisPost = "post"
)

// States of a page of the pay base against Hub's handed-in report (v_report_pages.hub_state). StateHubOnly is also the
// state (against the draft) of a row with neither a post nor a draft.
const (
	StateHubOnly    = "hub_only"
	StateHubEqual   = "hub_equal"
	StateHubChanged = "hub_changed"
	StateHubMissing = "hub_missing"
)

const (
	hubCursor       = "hub:shift_reports"
	hubStatusCursor = "hub:shift_reports:status"
	// hubMaxRequests bounds a pass (the next pass goes on).
	hubMaxRequests = 25
	// hubPageSlack is how far a page of Hub's report may lie from its lines' sum: Hub rounds a page half-up to the cent.
	hubPageSlack = 5
)

// hubOverlap is how many places behind the cursor every pass reads again: a version an older Hub served late, behind
// a bigger one already read (its feed was ordered by id), is not lost. Read again, a stored version writes nothing. A
// Hub with hub#1266 places a late version after the cursor itself (seq); the overlap then only reads known versions.
var hubOverlap int64 = 200

// hubLimit is how many versions one request asks (tests ask fewer).
var hubLimit = 200

// HubJob is the scheduler's name of the job that reads Hub's feed.
const HubJob = "hub-reports"

func init() {
	server.Every(HubJob, 5*time.Minute, func(ctx context.Context, s *server.Server) error {
		cfg, err := hub.ConfigFromEnv()
		if err != nil {
			return err
		}
		_, err = SyncHub(ctx, s.Store, hub.NewClient(cfg, nil), time.Now())
		return err
	})
}

// HubStatus is the last pass over Hub's feed (cursors hub:shift_reports:status).
type HubStatus struct {
	At string `json:"at"`
	OK bool   `json:"ok"`
	// Absent: Hub has no feed yet (404, a Hub before hub#1225): nothing to read, not a failure.
	Absent   bool   `json:"absent,omitempty"`
	Error    string `json:"error,omitempty"`
	Since    string `json:"since"`
	Next     string `json:"next"`
	Requests int    `json:"requests"`
	Read     int    `json:"read"`     // versions served this pass
	Stored   int    `json:"stored"`   // new versions stored whole
	Same     int    `json:"same"`     // served again, already stored
	Problems int    `json:"problems"` // versions of this pass that could not be stored
	Fixed    int    `json:"fixed"`    // versions kept with a problem that are stored now
	LastOKAt string `json:"lastOkAt,omitempty"`
}

// ReadHubStatus reads the last pass; nil — the feed was never read.
func ReadHubStatus(ctx context.Context, q ledger.Querier) (*HubStatus, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, hubStatusCursor).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st HubStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("%s: %w", hubStatusCursor, err)
	}
	return &st, nil
}

func putCursor(ctx context.Context, q ledger.Querier, name, value string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, value, ts.Format(now))
	return err
}

// SyncHub reads Hub's feed from the cursor on and stores what is new, then tries again the versions kept with a problem.
// A Hub without the feed (404) is «nothing yet»: the status says so and the pass succeeds. Each request's versions and
// the cursor after them are written in one transaction. When anything was stored, the accruals from the reports follow.
func SyncHub(ctx context.Context, store *db.Store, c *hub.Client, now time.Time) (HubStatus, error) {
	st := HubStatus{At: ts.Format(now)}
	if prev, err := ReadHubStatus(ctx, store.RW); err != nil {
		return st, err
	} else if prev != nil {
		st.LastOKAt = prev.LastOKAt
	}
	var since string
	if err := store.RW.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, hubCursor).Scan(&since); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.Since, st.Next = since, since
	cursor, _ := strconv.ParseInt(since, 10, 64) // "" — from the first
	ask := ""
	if from := cursor - hubOverlap; from > 0 {
		ask = strconv.FormatInt(from, 10)
	}
	save := func() error {
		b, _ := json.Marshal(st)
		return brief.InTx(ctx, store, func(tx *sql.Tx) error { return putCursor(ctx, tx, hubStatusCursor, string(b), now) })
	}
	var readErr error
	for st.Requests < hubMaxRequests {
		ans, err := c.ShiftReports(ctx, ask, hubLimit)
		st.Requests++
		if err != nil {
			readErr = err
			break
		}
		st.Read += len(ans.Reports)
		err = brief.InTx(ctx, store, func(tx *sql.Tx) error {
			v, err := newValidator(ctx, tx)
			if err != nil {
				return err
			}
			for i, it := range ans.Reports {
				out, err := storeHub(ctx, tx, v, it, ans.Raw[i], now)
				if err != nil {
					return fmt.Errorf("%s: %w", it, err)
				}
				switch out {
				case hubStored:
					st.Stored++
				case hubSame:
					st.Same++
				case hubProblem:
					st.Problems++
				}
			}
			// The cursor never moves back: a read of the overlap ends behind it.
			if next, _ := strconv.ParseInt(ans.Next, 10, 64); next > cursor {
				cursor = next
				st.Next = ans.Next
				return putCursor(ctx, tx, hubCursor, ans.Next, now)
			}
			return nil
		})
		if err != nil {
			return st, err
		}
		done := len(ans.Reports) < hubLimit || ans.Next == ask
		ask = ans.Next
		if done {
			break
		}
	}
	switch {
	case readErr == nil:
		st.OK, st.LastOKAt = true, st.At
	case hub.IsAbsent(readErr):
		st.OK, st.Absent, st.LastOKAt = true, true, st.At
		st.Error = "хаб ещё не отдаёт сданные отчёты (" + hub.ShiftReportsPath + " — 404)"
		readErr = nil
	case hub.IsKeyRefused(readErr):
		st.Error = hub.KeyRefusalText(readErr)
	case hub.IsRefusal(readErr):
		st.Error = readErr.Error()
	default:
		return st, readErr // a cancelled pass or the database: the status stays as it was
	}
	// The versions kept with a problem, once more: a card or a page may have come since.
	err := brief.InTx(ctx, store, func(tx *sql.Tx) error {
		n, err := retryHub(ctx, tx, now)
		st.Fixed = n
		return err
	})
	if err != nil {
		return st, err
	}
	if err := save(); err != nil {
		return st, err
	}
	// A new version, stored or kept with its problem (it holds the day once Hub's reports accrue), moves the accruals.
	if st.Stored > 0 || st.Problems > 0 || st.Fixed > 0 {
		if err := Changed(ctx, store, now); err != nil {
			return st, err
		}
	}
	if readErr != nil {
		return st, errors.New(st.Error)
	}
	return st, nil
}

// Outcomes of storing a version.
const (
	hubStored  = "stored"
	hubSame    = "same"
	hubProblem = "problem"
)

// validator knows the people and the pages a version must name.
type validator struct {
	people map[string]bool
	pages  map[string]bool
}

func newValidator(ctx context.Context, q ledger.Querier) (*validator, error) {
	v := &validator{people: map[string]bool{}, pages: map[string]bool{}}
	for _, x := range []struct {
		query string
		into  map[string]bool
	}{{`SELECT slug FROM people`, v.people}, {`SELECT page_label FROM hub_pages`, v.pages}} {
		rows, err := q.QueryContext(ctx, x.query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			x.into[s] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return v, nil
}

var (
	hubDoors      = map[string]bool{"workboard": true, "extension": true, "telegram": true}
	hubGroups     = map[string]bool{"mine": true, "near": true}
	hubStatements = map[string]bool{"mine": true, "not_mine": true, "share": true}
	hubOffStates  = map[string]bool{"pending": true, "confirmed": true, "rejected": true}
)

func millsOK(v *int64) bool { return v != nil && *v <= maxMills && *v >= -maxMills }

// check says why a version cannot be stored ("" — it can) and whose it is: the person's code when people has it.
func (v *validator) check(it hub.ShiftReport) (person string, problem string) {
	var why []string
	add := func(format string, args ...any) { why = append(why, fmt.Sprintf(format, args...)) }
	switch {
	case it.Person == nil || strings.TrimSpace(*it.Person) == "":
		add("у пользователя хаба %d нет человека (person в ленте пуст)", it.HubUserID)
	case !v.people[*it.Person]:
		add("кода человека «%s» нет в people", *it.Person)
	default:
		person = *it.Person
	}
	if d, err := time.ParseInLocation(ts.DateLayout, it.ReportDate, ts.MSK); err != nil || d.Format(ts.DateLayout) != it.ReportDate {
		add("дата отчёта %q не ГГГГ-ММ-ДД", it.ReportDate)
	}
	if _, err := time.Parse(time.RFC3339Nano, it.SubmittedAt); err != nil {
		add("submittedAt %q не время RFC 3339", it.SubmittedAt)
	}
	if !hubDoors[it.Door] {
		add("door %q", it.Door)
	}
	// The money: the pages add up to the total exactly, and each page is its counted lines' sum rounded to the cent.
	sums := true
	if !millsOK(it.TotalMills) {
		add("нет totalMills или он вне разумного")
		sums = false
	}
	pageSum, page, lines := int64(0), map[string]int64{}, map[string]int64{}
	for _, p := range it.Pages {
		_, dup := page[p.PageLabel]
		switch {
		case !v.pages[p.PageLabel]:
			add("страницы «%s» нет в hub_pages", p.PageLabel)
		case !millsOK(p.AmountMills):
			add("страница %s без суммы", p.PageLabel)
			sums = false
		case dup:
			add("страница %s дважды", p.PageLabel)
			sums = false
		}
		if millsOK(p.AmountMills) && !dup {
			page[p.PageLabel] = *p.AmountMills
			pageSum += *p.AmountMills
		}
	}
	if sums && pageSum != *it.TotalMills {
		add("сумма страниц %d mills не равна итогу %d mills", pageSum, *it.TotalMills)
	}
	for i, l := range it.Lines {
		_, inVersion := page[l.PageLabel]
		switch {
		case l.TransactionID <= 0:
			add("строка %d без transactionId", i+1)
		case !millsOK(l.NetMills) || !millsOK(l.AmountMills) || !millsOK(l.RuleShareMills):
			add("строка %d (продажа %d) без netMills, ruleShareMills или amountMills", i+1, l.TransactionID)
			sums = false
		case !hubGroups[l.Group]:
			add("строка %d (продажа %d): group %q", i+1, l.TransactionID, l.Group)
		case l.Statement != nil && !hubStatements[*l.Statement]:
			add("строка %d (продажа %d): statement %q", i+1, l.TransactionID, *l.Statement)
		case !v.pages[l.PageLabel]:
			add("строка %d (продажа %d): страницы «%s» нет в hub_pages", i+1, l.TransactionID, l.PageLabel)
		case !inVersion:
			add("строка %d (продажа %d): страницы %s нет в pages версии", i+1, l.TransactionID, l.PageLabel)
			sums = false
		}
		if millsOK(l.AmountMills) {
			lines[l.PageLabel] += *l.AmountMills
		}
	}
	// «Мимо площадки» the owner confirmed counts in its page's sum (hub#1252): it is a line of that page here. One with
	// no page or not confirmed adds nothing.
	for i, o := range it.OffPlatform {
		label := o.CountsOnPage()
		_, inVersion := page[label]
		switch {
		case !millsOK(o.AmountMills) || !hubOffStates[o.State]:
			add("«мимо площадки» %d: нет суммы или state %q", i+1, o.State)
		case label == "":
		case !v.pages[label]:
			add("«мимо площадки» %d (запись %d): страницы «%s» нет в hub_pages", i+1, o.ID, label)
		case !inVersion:
			add("«мимо площадки» %d (запись %d): страницы %s нет в pages версии", i+1, o.ID, label)
			sums = false
		default:
			lines[label] += *o.AmountMills
		}
	}
	if sums {
		for _, p := range it.Pages {
			if d := page[p.PageLabel] - lines[p.PageLabel]; d > hubPageSlack || d < -hubPageSlack {
				add("страница %s: строки дают %d mills, у страницы %d mills", p.PageLabel, lines[p.PageLabel], page[p.PageLabel])
			}
		}
	}
	if len(why) > 6 {
		why = append(why[:6], fmt.Sprintf("и ещё %d", len(why)-6))
	}
	return person, strings.Join(why, "; ")
}

// storeHub stores one version: once whole, or with its problem until it can be. The same shift and version already
// stored whole writes nothing.
func storeHub(ctx context.Context, tx *sql.Tx, v *validator, it hub.ShiftReport, raw json.RawMessage, now time.Time) (string, error) {
	var rowID int64
	var problem sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, problem FROM hub_shift_reports WHERE id = ? OR (shift_id = ? AND version = ?)
		ORDER BY id = ? DESC LIMIT 1`, it.ID, it.ShiftID, it.Version, it.ID).Scan(&rowID, &problem)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rowID = 0
	case err != nil:
		return "", err
	case !problem.Valid:
		return hubSame, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", err
	}
	person, why := v.check(it)
	// Kept with the same problem and read again (the overlap, a retry): nothing changes, nothing to write.
	if rowID != 0 && why != "" && problem.String == why {
		return hubSame, nil
	}
	at := ts.Format(now)
	submitted := it.SubmittedAt
	if t, err := time.Parse(time.RFC3339Nano, it.SubmittedAt); err == nil {
		submitted = ts.Format(t)
	}
	var total int64
	if millsOK(it.TotalMills) {
		total = int64(money.FromMills(*it.TotalMills))
	}
	var dayPaid any
	if it.DayPaid != nil {
		dayPaid = boolInt(*it.DayPaid)
	}
	var stored any
	if why == "" {
		stored = at
	}
	args := []any{it.ShiftID, it.Version, nullStr(person), it.Person, it.HubUserID, it.ReportDate, submitted, it.Door, it.TelegramMessageRef,
		total, it.Comment, dayPaid, it.RuleVersion, nullStr(why), compact.String(), stored}
	if rowID == 0 {
		rowID = it.ID
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_shift_reports(shift_id, version, person, hub_person, hub_user_id, report_date,
			submitted_at, door, telegram_message_ref, total_micros, comment, day_paid, rule_version, problem, payload, stored_at, id, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(args, rowID, at)...); err != nil {
			return "", err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE hub_shift_reports SET shift_id = ?, version = ?, person = ?, hub_person = ?,
		hub_user_id = ?, report_date = ?, submitted_at = ?, door = ?, telegram_message_ref = ?, total_micros = ?, comment = ?, day_paid = ?,
		rule_version = ?, problem = ?, payload = ?, stored_at = ? WHERE id = ?`, append(args, rowID)...); err != nil {
		return "", err
	}
	// The newest version of the shift is its report.
	if _, err := tx.ExecContext(ctx, `UPDATE hub_shift_reports SET current = CASE WHEN version =
		(SELECT MAX(x.version) FROM hub_shift_reports x WHERE x.shift_id = hub_shift_reports.shift_id) THEN 1 ELSE 0 END
		WHERE shift_id = ?`, it.ShiftID); err != nil {
		return "", err
	}
	if why != "" {
		return hubProblem, nil
	}
	for i, p := range it.Pages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_shift_report_pages(report_id, seq, page_label, amount_micros) VALUES (?, ?, ?, ?)`,
			rowID, i+1, p.PageLabel, int64(money.FromMills(*p.AmountMills))); err != nil {
			return "", err
		}
	}
	for i, l := range it.Lines {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_shift_report_lines(report_id, seq, transaction_ref, page_label, grp, net_micros,
			rule_share_micros, rule_home, rule_reason, statement, share_with_user_id, note, amount_micros, counted, added, disputed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, rowID, i+1, strconv.FormatInt(l.TransactionID, 10), l.PageLabel, l.Group,
			int64(money.FromMills(*l.NetMills)), int64(money.FromMills(*l.RuleShareMills)), l.RuleHome, l.RuleReason, l.Statement,
			l.ShareWithUserID, l.Note, int64(money.FromMills(*l.AmountMills)), boolInt(*l.AmountMills != 0), boolInt(l.Added),
			boolInt(l.Disputed)); err != nil {
			return "", err
		}
	}
	// A confirmed amount with a page is in that page's sum (check): kept as counted on it, not as an uncounted add.
	for i, o := range it.OffPlatform {
		label := o.CountsOnPage()
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_shift_report_adds(report_id, seq, hub_id, amount_micros, from_whom, what_for, state,
			counted, page_label) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, rowID, i+1, o.ID, int64(money.FromMills(*o.AmountMills)), o.FromWhom,
			o.WhatFor, o.State, boolInt(label != ""), nullStr(label)); err != nil {
			return "", err
		}
	}
	return hubStored, nil
}

// retryHub tries again every current version kept with a problem (a superseded one no longer matters); it returns how
// many are stored now.
func retryHub(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, payload FROM hub_shift_reports WHERE problem IS NOT NULL AND current = 1 ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type kept struct {
		id  int64
		raw string
	}
	var list []kept
	for rows.Next() {
		var k kept
		if err := rows.Scan(&k.id, &k.raw); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return 0, err
	}
	v, err := newValidator(ctx, tx)
	if err != nil {
		return 0, err
	}
	fixed := 0
	for _, k := range list {
		var it hub.ShiftReport
		if err := json.Unmarshal([]byte(k.raw), &it); err != nil {
			return fixed, fmt.Errorf("hub_shift_reports %d: payload: %w", k.id, err)
		}
		out, err := storeHub(ctx, tx, v, it, json.RawMessage(k.raw), now)
		if err != nil {
			return fixed, fmt.Errorf("%s: %w", it, err)
		}
		if out == hubStored {
			fixed++
		}
	}
	return fixed, nil
}

// HubProblem is a current version Hub served that could not be stored.
type HubProblem struct {
	ID         int64   `json:"id"`
	ShiftID    int64   `json:"shiftId"`
	Version    int     `json:"version"`
	Person     *string `json:"person"`    // people.slug when known
	HubPerson  *string `json:"hubPerson"` // the code Hub sent
	HubUserID  *int64  `json:"hubUserId"`
	ReportDate string  `json:"reportDate"`
	Problem    string  `json:"problem"`
	ReceivedAt string  `json:"receivedAt"`
}

// HubProblems lists the current versions kept with a problem, oldest first.
func HubProblems(ctx context.Context, q ledger.Querier) ([]HubProblem, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, shift_id, version, person, hub_person, hub_user_id, report_date, problem, received_at
		FROM hub_shift_reports WHERE current = 1 AND problem IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HubProblem{}
	for rows.Next() {
		var p HubProblem
		var person, hubPerson sql.NullString
		var user sql.NullInt64
		if err := rows.Scan(&p.ID, &p.ShiftID, &p.Version, &person, &hubPerson, &user, &p.ReportDate, &p.Problem, &p.ReceivedAt); err != nil {
			return nil, err
		}
		p.Person, p.HubPerson, p.HubUserID = nullable(person), nullable(hubPerson), nullInt(user)
		out = append(out, p)
	}
	return out, rows.Err()
}

// HubVersionsByDay are the current, stored versions of each person, model and report date, by {person, model, date}:
// what a day of basis hub is accrued from.
func HubVersionsByDay(ctx context.Context, q ledger.Querier) (map[[3]string][]int64, error) {
	models, err := payroll.HubPageModels(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT r.id, r.person, r.report_date, p.page_label FROM hub_shift_reports r
		JOIN hub_shift_report_pages p ON p.report_id = r.id
		WHERE r.current = 1 AND r.problem IS NULL AND r.person IS NOT NULL ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[3]string][]int64{}
	for rows.Next() {
		var id int64
		var person, date, page string
		if err := rows.Scan(&id, &person, &date, &page); err != nil {
			return nil, err
		}
		k := [3]string{person, models[page], date}
		if list := out[k]; len(list) == 0 || list[len(list)-1] != id {
			out[k] = append(list, id)
		}
	}
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	}
	return out, rows.Err()
}
