package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/payroll"
)

// Page is a page row of a report as GET /v1/reports serves it.
type Page struct {
	Page         string `json:"page"`
	Model        string `json:"model"` // hub_pages and hub_model_slugs; "" — the page is not in the grant
	AmountMicros int64  `json:"amount_micros"`
	// PostedMicros is the amount of the page in the chatter's post (v_report_pages); nil — not posted. PostState
	// compares it with this version's amount: posted_equal, posted_changed or draft_only.
	PostedMicros *int64 `json:"posted_micros"`
	PostState    string `json:"post_state"`
}

// Line is a sale of the breakdown, or an amount added by hand (Added, What). TransactionRef of a sale is the
// platform's reference of it; of an added amount — the sale it pays, by either of the sale's two references
// (hub.SaleRefsOf), nil when it names none. An added amount that names its sale carries the sale's Agreement too.
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
	// Agreement is the open or applied decision about whose the sale is; nil — none. Ledger's own (sale_decisions, R3),
	// or — for a sale of settings.sale_decisions_hub_from and later — Hub's active decision of the sale, when it has one.
	Agreement *Agreement `json:"agreement"`
}

// Agreement is a decision about whose a sale is, as a report line carries it.
type Agreement struct {
	ID     int64           `json:"id"`
	Kind   string          `json:"kind"`
	Shares json.RawMessage `json:"shares"`
	Pot    *string         `json:"pot,omitempty"`
	Status string          `json:"status"`
	SaidBy string          `json:"said_by"`
	Note   string          `json:"note"`
	// Source is "hub" when the decision is Hub's (hub_sale_decisions: ID is Hub's id); absent — ledger's own.
	Source string `json:"source,omitempty"`
}

// SourceHubDecision marks an agreement that is Hub's decision.
const SourceHubDecision = "hub"

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
	PostedAt    *string `json:"posted_at"` // the latest post that touches a page of the report; nil — none
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
	posted, err := postedOf(ctx, q, reports)
	if err != nil {
		return err
	}
	agreements, err := agreementsOf(ctx, q, reports)
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
			p.PostState = StateDraftOnly
			if r.Person != nil {
				if d, ok := posted[[3]string{*r.Person, r.ReportDate, p.Page}]; ok {
					p.PostedMicros = d.PostedMicros
					p.PostState = StatePostedChanged
					if SameCents(*d.PostedMicros, p.AmountMicros) {
						p.PostState = StatePostedEqual
					}
					if r.PostedAt == nil || *d.PostedAt > *r.PostedAt {
						r.PostedAt = d.PostedAt
					}
				}
			}
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
			if ref.Valid {
				// The sale's Moscow day; a line without the sale's time is taken as of the report's date.
				day := r.ReportDate
				if at.Valid && len(at.String) >= 10 {
					day = at.String[:10]
				}
				l.Agreement = agreements.of(ref.String, day)
			}
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

// agreements are the decisions about the sales of some reports' lines: ledger's own, and Hub's for the sales its word
// rules. Both maps are keyed by the reference the line carries — the platform's; an added amount's may be either —,
// whichever of the sale's two references the decision was written with (hub.SameSaleSQL).
type agreements struct {
	own map[string]*Agreement
	// hub are Hub's active decisions by sale; hubFrom is settings.sale_decisions_hub_from ("" — Hub's are not read).
	hub     map[string]*Agreement
	hubFrom string
}

// of is the agreement of a sale of a Moscow day: for a sale of the switch day or later Hub's active decision when Hub has
// one — ledger's own decisions are frozen from that day, the word is recorded in Hub —, else ledger's own.
func (a agreements) of(ref, day string) *Agreement {
	if decisions.HubRules(a.hubFrom, day) {
		if h := a.hub[ref]; h != nil {
			return h
		}
	}
	return a.own[ref]
}

// agreementsOf reads the open or applied decisions about the sales of the reports' lines, the latest of each sale: from
// ledger's sale_decisions and, once settings.sale_decisions_hub_from is set, the active ones of the mirror of Hub's.
// A line names its sale by the platform's reference and Hub's decision by Hub's id (ledger's own by whichever its
// writer had), so a decision is the line's when the two references name one sale.
func agreementsOf(ctx context.Context, q ledger.Querier, reports []Report) (agreements, error) {
	out := agreements{own: map[string]*Agreement{}, hub: map[string]*Agreement{}}
	ids := make([]string, len(reports))
	args := make([]any, len(reports))
	for i, r := range reports {
		ids[i], args[i] = "?", r.ID
	}
	// The references the lines carry, each once.
	lines := `(SELECT DISTINCT transaction_ref AS ref FROM shift_report_lines WHERE transaction_ref IS NOT NULL AND report_id IN (` +
		strings.Join(ids, ", ") + `)) l`
	rows, err := q.QueryContext(ctx, `SELECT l.ref, d.id, d.kind, d.shares, d.pot, d.status, d.said_by, d.note
		FROM `+lines+` JOIN sale_decisions d ON `+hub.SameSaleSQL("d.transaction_ref", "l.ref")+`
		WHERE d.status IN ('open', 'applied') ORDER BY d.id`, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var ref, shares string
		var pot sql.NullString
		a := &Agreement{}
		if err := rows.Scan(&ref, &a.ID, &a.Kind, &shares, &pot, &a.Status, &a.SaidBy, &a.Note); err != nil {
			rows.Close()
			return out, err
		}
		a.Shares, a.Pot = json.RawMessage(shares), nullable(pot)
		out.own[ref] = a // the latest stands
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.hubFrom, err = decisions.HubFrom(ctx, q); err != nil || out.hubFrom == "" {
		return out, err
	}
	rows, err = q.QueryContext(ctx, `SELECT l.ref, d.id, d.kind, d.shares, d.pot, d.applied_report_date IS NOT NULL,
		COALESCE(d.said_by, ''), COALESCE(d.note, '') FROM `+lines+` JOIN hub_sale_decisions d ON `+
		hub.SameSaleSQL("d.transaction_ref", "l.ref")+`
		WHERE d.state = 'active' ORDER BY d.id`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref, shares string
		var pot sql.NullString
		var applied bool
		a := &Agreement{Source: SourceHubDecision, Status: "open"}
		if err := rows.Scan(&ref, &a.ID, &a.Kind, &shares, &pot, &applied, &a.SaidBy, &a.Note); err != nil {
			return out, err
		}
		if applied {
			a.Status = "applied"
		}
		a.Shares, a.Pot = hubShares(shares), nullable(pot)
		out.hub[ref] = a
	}
	return out, rows.Err()
}

// hubShares renders the shares of Hub's decision the way a line's agreement carries ledger's: the person's code and the
// percent; a share of a login no person holds names the login.
func hubShares(raw string) json.RawMessage {
	var in []struct {
		Person  *string `json:"person"`
		UserID  int64   `json:"userId"`
		Percent int     `json:"percent"`
	}
	type share struct {
		Person    string `json:"person"`
		Percent   int    `json:"percent"`
		HubUserID *int64 `json:"hub_user_id,omitempty"`
	}
	out := []share{}
	if json.Unmarshal([]byte(raw), &in) == nil {
		for _, x := range in {
			s := share{Percent: x.Percent}
			if x.Person != nil && *x.Person != "" {
				s.Person = *x.Person
			} else {
				id := x.UserID
				s.HubUserID = &id
			}
			out = append(out, s)
		}
	}
	b, _ := json.Marshal(out)
	return b
}
