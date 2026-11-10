package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/payroll"
)

// Page is a page row of a report as GET /v1/reports serves it.
type Page struct {
	Page         string `json:"page"`
	Model        string `json:"model"` // hub_pages and hub_model_slugs; "" — the page is not in the grant
	AmountMicros int64  `json:"amount_micros"`
}

// Line is a sale of the breakdown, or an amount added by hand (Added, What).
type Line struct {
	N              *int64   `json:"n"`
	TransactionRef *string  `json:"transaction_ref"`
	Page           string   `json:"page"`
	FanRef         string   `json:"fan_ref"`
	FanName        string   `json:"fan_name"`
	Kind           string   `json:"kind"`
	OccurredAt     *string  `json:"occurred_at"`
	NetMicros      int64    `json:"net_micros"`
	GrossMicros    *int64   `json:"gross_micros"`
	Counted        bool     `json:"counted"`
	Decision       string   `json:"decision"`
	Added          bool     `json:"added"`
	What           string   `json:"what"`
	Flags          []string `json:"flags"`
	Notes          []string `json:"notes"`
	PPVAuthor      string   `json:"ppv_author"`
	PPVSentAt      string   `json:"ppv_sent_at"`
}

// Report is one version of a report.
type Report struct {
	ID          int64   `json:"id"`
	Person      *string `json:"person"`
	HubUsername string  `json:"hub_username"`
	HubUserID   *int64  `json:"hub_user_id"`
	Chatter     string  `json:"chatter"`
	ReportDate  string  `json:"report_date"`
	WindowFrom  string  `json:"window_from"`
	WindowTo    string  `json:"window_to"`
	CutAt       string  `json:"cut_at"`
	Version     int     `json:"version"`
	Versions    int     `json:"versions"`
	Status      string  `json:"status"`
	Source      string  `json:"source"`
	TotalMicros int64   `json:"total_micros"`
	CreatedAt   string  `json:"created_at"`
	Pages       []Page  `json:"pages"`
	Lines       []Line  `json:"lines"`
}

// ListAnswer is the reply of GET /v1/reports.
type ListAnswer struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Reports []Report `json:"reports"`
}

// Filter narrows GET /v1/reports.
type Filter struct {
	From, To    string // report_date, inclusive
	Person      string // people.slug
	HubUserID   *int64
	HubUsername string
	AllVersions bool
}

const reportColumns = `r.id, r.person, r.hub_username, r.hub_user_id, r.chatter, r.report_date, r.window_from, r.window_to,
	r.cut_at, r.version, (SELECT COUNT(*) FROM shift_reports x WHERE lower(x.hub_username) = lower(r.hub_username)
	AND x.report_date = r.report_date), r.status, r.source, r.total_micros, r.created_at`

// List reads the reports of the window by the filter. A Hub user filter (id or name) matches a report saved under
// that id, that name (case aside) or the person the resolver gives for them — and nothing else.
func List(ctx context.Context, q ledger.Querier, f Filter) ([]Report, error) {
	where := []string{"r.report_date BETWEEN ? AND ?"}
	args := []any{f.From, f.To}
	if !f.AllVersions {
		where = append(where, `r.version = (SELECT MAX(x.version) FROM shift_reports x
			WHERE lower(x.hub_username) = lower(r.hub_username) AND x.report_date = r.report_date)`)
	}
	if f.Person != "" {
		where = append(where, "r.person = ?")
		args = append(args, f.Person)
	}
	if f.HubUserID != nil || f.HubUsername != "" {
		var or []string
		if f.HubUserID != nil {
			or = append(or, "r.hub_user_id = ?")
			args = append(args, *f.HubUserID)
		}
		if f.HubUsername != "" {
			or = append(or, "lower(r.hub_username) = lower(?)")
			args = append(args, f.HubUsername)
		}
		person, err := personOf(ctx, q, f.HubUserID, f.HubUsername)
		if err != nil {
			return nil, err
		}
		if person != "" {
			or = append(or, "r.person = ?")
			args = append(args, person)
		}
		where = append(where, "("+strings.Join(or, " OR ")+")")
	}
	rows, err := q.QueryContext(ctx, `SELECT `+reportColumns+` FROM shift_reports r WHERE `+strings.Join(where, " AND ")+`
		ORDER BY r.report_date, lower(r.hub_username), r.version`, args...)
	if err != nil {
		return nil, err
	}
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, fill(ctx, q, out)
}

// ErrNotFound is a report id with no row.
var ErrNotFound = errors.New("no such report")

// Get reads one version of a report by its id.
func Get(ctx context.Context, q ledger.Querier, id int64) (Report, error) {
	r, err := scanReport(q.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM shift_reports r WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, err
	}
	out := []Report{r}
	if err := fill(ctx, q, out); err != nil {
		return Report{}, err
	}
	return out[0], nil
}

// personOf is the person of a Hub user on any model of the grant, the way Save resolves it; "" — none or several.
func personOf(ctx context.Context, q ledger.Querier, userID *int64, username string) (string, error) {
	res, err := payroll.NewResolver(ctx, q)
	if err != nil {
		return "", err
	}
	models, err := payroll.HubPageModels(ctx, q)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	var list []string
	for _, m := range models {
		if !seen[m] {
			seen[m] = true
			list = append(list, m)
		}
	}
	return resolve(res, list, userID, username), nil
}

func scanReport(row interface{ Scan(...any) error }) (Report, error) {
	var r Report
	var person sql.NullString
	var user sql.NullInt64
	if err := row.Scan(&r.ID, &person, &r.HubUsername, &user, &r.Chatter, &r.ReportDate, &r.WindowFrom, &r.WindowTo,
		&r.CutAt, &r.Version, &r.Versions, &r.Status, &r.Source, &r.TotalMicros, &r.CreatedAt); err != nil {
		return Report{}, err
	}
	r.Person = nullable(person)
	if user.Valid {
		r.HubUserID = &user.Int64
	}
	return r, nil
}

// fill reads the pages and lines of the reports.
func fill(ctx context.Context, q ledger.Querier, reports []Report) error {
	if len(reports) == 0 {
		return nil
	}
	models, err := payroll.HubPageModels(ctx, q)
	if err != nil {
		return err
	}
	for i := range reports {
		r := &reports[i]
		r.Pages, r.Lines = []Page{}, []Line{}
		rows, err := q.QueryContext(ctx, `SELECT page_label, amount_micros FROM shift_report_pages WHERE report_id = ? ORDER BY seq`, r.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p Page
			if err := rows.Scan(&p.Page, &p.AmountMicros); err != nil {
				rows.Close()
				return err
			}
			p.Model = models[p.Page]
			r.Pages = append(r.Pages, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = q.QueryContext(ctx, `SELECT n, transaction_ref, page_label, fan_ref, fan_name, kind, occurred_at, net_micros,
			gross_micros, counted, decision, added, what, flags, notes, ppv_author, ppv_sent_at
			FROM shift_report_lines WHERE report_id = ? ORDER BY seq`, r.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l Line
			var n, gross sql.NullInt64
			var ref, at sql.NullString
			var flags, notes string
			if err := rows.Scan(&n, &ref, &l.Page, &l.FanRef, &l.FanName, &l.Kind, &at, &l.NetMicros, &gross, &l.Counted,
				&l.Decision, &l.Added, &l.What, &flags, &notes, &l.PPVAuthor, &l.PPVSentAt); err != nil {
				rows.Close()
				return err
			}
			if n.Valid {
				l.N = &n.Int64
			}
			if gross.Valid {
				l.GrossMicros = &gross.Int64
			}
			l.TransactionRef, l.OccurredAt = nullable(ref), nullable(at)
			l.Flags, l.Notes = []string{}, []string{}
			_ = json.Unmarshal([]byte(flags), &l.Flags)
			_ = json.Unmarshal([]byte(notes), &l.Notes)
			r.Lines = append(r.Lines, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}
