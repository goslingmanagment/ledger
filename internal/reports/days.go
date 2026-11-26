package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// States of a page of the pay base (v_report_pages).
const (
	StatePostedEqual   = "posted_equal"
	StatePostedChanged = "posted_changed"
	StatePostOnly      = "post_only"
	StateDraftOnly     = "draft_only"
)

// SameCents says whether a posted amount and a draft's are one amount: a post is written to the cent, the agent's
// draft keeps a page in thousandths of a dollar (as Hub gives it), so they may differ by up to half a cent.
func SameCents(posted, draft int64) bool {
	d := posted - draft
	if d < 0 {
		d = -d
	}
	return d <= 5000
}

// Day is a row of v_report_pages: a person's page on a report date, as posted, as drafted and as handed in in Hub.
type Day struct {
	Person       string  `json:"person"`
	ReportDate   string  `json:"report_date"`
	Page         string  `json:"page"`
	Model        string  `json:"model"`
	PostedMicros *int64  `json:"posted_micros"`
	DraftMicros  *int64  `json:"draft_micros"`
	DiffMicros   *int64  `json:"diff_micros"`
	State        string  `json:"state"`
	PostedAt     *string `json:"posted_at"`
	ReportID     *int64  `json:"report_id"`
	PostID       *int64  `json:"post_id"`
	// HubMicros is the page in the reports handed in in Hub for the person and day (their shifts added); nil — none.
	// HubDiffMicros is posted − Hub (0 — equal to the cent); HubState hub_equal, hub_changed, hub_only, hub_missing, or
	// nil when Hub has no report of the person and day.
	HubMicros     *int64  `json:"hub_micros"`
	HubDiffMicros *int64  `json:"hub_diff_micros"`
	HubState      *string `json:"hub_state"`
	// Basis is what accrues the day (hub or post) and AccrueMicros the page's amount it accrues; nil — nothing.
	Basis        string `json:"basis"`
	AccrueMicros *int64 `json:"accrue_micros"`
}

// DaysAnswer is the reply of GET /v1/report-days.
type DaysAnswer struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days []Day  `json:"days"`
}

// Days reads the pay base of the window. The filter works as for List, but a row has only a person: a Hub user
// filter is the person the resolver gives for that user, and a user without one gets no rows.
func Days(ctx context.Context, q ledger.Querier, f Filter) ([]Day, error) {
	where := []string{"report_date BETWEEN ? AND ?"}
	args := []any{f.From, f.To}
	if f.Person != "" {
		where = append(where, "person = ?")
		args = append(args, f.Person)
	}
	if f.HubUserID != nil || f.HubUsername != "" {
		person, err := personOf(ctx, q, f.HubUserID, f.HubUsername)
		if err != nil {
			return nil, err
		}
		if person == "" {
			return []Day{}, nil
		}
		where = append(where, "person = ?")
		args = append(args, person)
	}
	rows, err := q.QueryContext(ctx, `SELECT person, report_date, page, model, posted_micros, draft_micros, diff_micros, state,
		posted_at, report_id, post_id, hub_micros, hub_diff_micros, hub_state, basis, accrue_micros FROM v_report_pages
		WHERE `+strings.Join(where, " AND ")+` ORDER BY report_date, person, page`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Day{}
	for rows.Next() {
		var d Day
		var posted, draft, diff, report, post, hubAmount, hubDiff, accrue sql.NullInt64
		var at, hubState sql.NullString
		if err := rows.Scan(&d.Person, &d.ReportDate, &d.Page, &d.Model, &posted, &draft, &diff, &d.State, &at, &report, &post,
			&hubAmount, &hubDiff, &hubState, &d.Basis, &accrue); err != nil {
			return nil, err
		}
		d.PostedMicros, d.DraftMicros, d.DiffMicros = nullInt(posted), nullInt(draft), nullInt(diff)
		d.ReportID, d.PostID, d.PostedAt = nullInt(report), nullInt(post), nullable(at)
		d.HubMicros, d.HubDiffMicros, d.HubState, d.AccrueMicros = nullInt(hubAmount), nullInt(hubDiff), nullable(hubState), nullInt(accrue)
		out = append(out, d)
	}
	return out, rows.Err()
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// PostPageOut is an account line of a post as GET /v1/report-posts serves it.
type PostPageOut struct {
	Name         string  `json:"name"`
	Page         *string `json:"page"` // NULL — the name is not mapped to a Hub page
	InShift      bool    `json:"in_shift"`
	AmountMicros int64   `json:"amount_micros"`
}

// Post is the current version of a post with its flags.
type Post struct {
	ID          int64         `json:"id"`
	Peer        string        `json:"peer"`
	Topic       int64         `json:"topic"`
	MsgID       int64         `json:"msg_id"`
	PostedAt    string        `json:"posted_at"`
	SenderID    string        `json:"sender_id"`
	Sender      string        `json:"sender"`
	Tag         string        `json:"tag"`
	Person      *string       `json:"person"`
	Model       string        `json:"model"`
	ReportDate  *string       `json:"report_date"`
	TotalMicros int64         `json:"total_micros"`
	PagesMicros int64         `json:"pages_micros"`
	Flags       []string      `json:"flags"`
	FlagTexts   []string      `json:"flag_texts"`
	Blocked     bool          `json:"blocked"` // a flag keeps the whole post out of the pay base
	Accrues     bool          `json:"accrues"` // the day accrues from the post (postAccrues); false — its flags are information
	Versions    int           `json:"versions"`
	Text        string        `json:"text,omitempty"` // only to a key with read: Hub's payouts key gets the list without it
	Pages       []PostPageOut `json:"pages"`
}

// PostsAnswer is the reply of GET /v1/report-posts.
type PostsAnswer struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Posts []Post `json:"posts"`
}

// Posts reads the current posts whose report date (or, without one, the Moscow day of posting) is in the window;
// flagged — only those with a flag.
func Posts(ctx context.Context, q ledger.Querier, from, to string, flagged bool) ([]Post, error) {
	query := `SELECT p.id, p.peer, p.topic, p.msg_id, p.posted_at, p.sender_id, p.sender, p.tag, p.person, p.model, p.report_date,
		p.total_micros, p.pages_micros, p.flags, p.text,
		(SELECT COUNT(*) FROM shift_report_posts x WHERE x.peer = p.peer AND x.topic = p.topic AND x.msg_id = p.msg_id)
		FROM shift_report_posts p WHERE p.current = 1 AND COALESCE(p.report_date, substr(p.posted_at, 1, 10)) BETWEEN ? AND ?`
	if flagged {
		query += ` AND json_array_length(p.flags) > 0`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY p.posted_at, p.id`, from, to)
	if err != nil {
		return nil, err
	}
	out := []Post{}
	for rows.Next() {
		var p Post
		var person, date sql.NullString
		var flags string
		if err := rows.Scan(&p.ID, &p.Peer, &p.Topic, &p.MsgID, &p.PostedAt, &p.SenderID, &p.Sender, &p.Tag, &person, &p.Model, &date,
			&p.TotalMicros, &p.PagesMicros, &flags, &p.Text, &p.Versions); err != nil {
			rows.Close()
			return nil, err
		}
		p.Person, p.ReportDate = nullable(person), nullable(date)
		p.Flags, p.FlagTexts, p.Pages = []string{}, []string{}, []PostPageOut{}
		_ = json.Unmarshal([]byte(flags), &p.Flags)
		for _, f := range p.Flags {
			p.FlagTexts = append(p.FlagTexts, FlagText[f])
			p.Blocked = p.Blocked || Blocking(f)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	accrues, err := postAccrues(ctx, q, from)
	if err != nil {
		return nil, err
	}
	for i := range out {
		p := &out[i]
		p.Accrues = accrues(*p)
		rows, err := q.QueryContext(ctx, `SELECT name, page_label, in_shift, amount_micros FROM shift_report_post_pages
			WHERE post_id = ? ORDER BY seq`, p.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var pg PostPageOut
			var label sql.NullString
			if err := rows.Scan(&pg.Name, &label, &pg.InShift, &pg.AmountMicros); err != nil {
				rows.Close()
				return nil, err
			}
			pg.Page = nullable(label)
			p.Pages = append(p.Pages, pg)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// postAccrues says whether a post's day accrues from the post: not a day before payroll_reports_from (the sheets paid
// for it), not one Hub's handed-in report pays for (payroll.HubBasisDays, read from the day `from` on). A post without
// a person or a date accrues: it is asked about always. A post that does not accrue moves no money, its flags are
// information: neither the check «отчёты разобраны» nor Hub asks about it.
func postAccrues(ctx context.Context, q ledger.Querier, from string) (func(Post) bool, error) {
	on, err := payroll.ReportsFrom(ctx, q)
	if err != nil {
		return nil, err
	}
	hubDay, err := payroll.HubBasisDays(ctx, q, from)
	if err != nil {
		return nil, err
	}
	return func(p Post) bool {
		if on != "" && p.ReportDate != nil && *p.ReportDate < on {
			return false
		}
		return p.Person == nil || p.ReportDate == nil || !hubDay(*p.Person, *p.ReportDate)
	}, nil
}

// PostsSinceDays is how many days after a post-basis day was paid the collector still re-reads its posts, so a late
// edit to a recently paid post arrives and opens payroll_pending_changes.
const PostsSinceDays = 14

// MaxCollectDays bounds automatic Telegram reads. Explicit backfills choose their own start.
const MaxCollectDays = 45

// PostsSinceAnswer is the reply of GET /v1/report-posts/since: narrow, read-only, no sums and no people.
type PostsSinceAnswer struct {
	Since string `json:"since"`
}

// PostsSince returns the Telegram fetch date for the earliest unpaid or recently paid
// post-basis day. Include the previous evening and cap the automatic lookback at 45 days.
func PostsSince(ctx context.Context, q ledger.Querier, now time.Time) (string, error) {
	reportsFrom, err := payroll.ReportsFrom(ctx, q)
	if err != nil || reportsFrom == "" {
		return "", err
	}
	hubDay, err := payroll.HubBasisDays(ctx, q, reportsFrom)
	if err != nil {
		return "", err
	}
	cutoff := ts.Date(now.In(ts.MSK).AddDate(0, 0, -PostsSinceDays))
	rows, err := q.QueryContext(ctx, `SELECT pd.person, pd.date, pd.unpaid, pd.paid,
		COALESCE((SELECT MAX(d.date) FROM json_each(pd.paid_by) j JOIN v_documents d ON d.num = j.value), '')
		FROM payable_days pd
		WHERE pd.kind = 'day' AND pd.date >= ?
		ORDER BY pd.date`, reportsFrom)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var person, day, paidAt string
		var unpaid, paid int64
		if err := rows.Scan(&person, &day, &unpaid, &paid, &paidAt); err != nil {
			return "", err
		}
		if hubDay(person, day) {
			continue
		}
		if unpaid > 0 || (paid > 0 && (day >= cutoff || paidAt >= cutoff)) {
			d, err := time.Parse(ts.DateLayout, day)
			if err != nil {
				return "", err
			}
			// Telegram messages may precede their report date by one Moscow day.
			return max(d.AddDate(0, 0, -1).Format(ts.DateLayout), ts.Date(now.In(ts.MSK).AddDate(0, 0, -MaxCollectDays))), nil
		}
	}
	return "", rows.Err()
}

// postedOf reads the posted side of the pay base for the reports' people and dates, to show on GET /v1/reports.
func postedOf(ctx context.Context, q ledger.Querier, reports []Report) (map[[3]string]Day, error) {
	out := map[[3]string]Day{}
	from, to := "", ""
	for _, r := range reports {
		if r.Person == nil {
			continue
		}
		if from == "" || r.ReportDate < from {
			from = r.ReportDate
		}
		if r.ReportDate > to {
			to = r.ReportDate
		}
	}
	if from == "" {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT person, report_date, page, posted_micros, posted_at FROM v_report_pages
		WHERE report_date BETWEEN ? AND ? AND posted_micros IS NOT NULL`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d Day
		var posted int64
		var at string
		if err := rows.Scan(&d.Person, &d.ReportDate, &d.Page, &posted, &at); err != nil {
			return nil, err
		}
		d.PostedMicros, d.PostedAt = &posted, &at
		out[[3]string{d.Person, d.ReportDate, d.Page}] = d
	}
	return out, rows.Err()
}
