package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The sheets mirror (R2): from settings.payroll_reports_from the payout sheets are a mirror of the shift reports, and
// `ledger mirror sheets` on the Mac writes the posted page amounts into their cells. The server gives it the base: the
// switch, which tab takes whose accruals, the rule that maps a row to a page, and the posted amounts. Nothing here writes
// a posting or a sheet.

func init() {
	server.Register(func(s *server.Server) {
		// The collector key of the Mac reads it, like it sends the sheets' events.
		s.Mux.Handle("GET /v1/sheets-mirror", auth.RequireAny(s, []string{auth.ScopeCollect, auth.ScopeRead},
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				from := strings.TrimSpace(r.URL.Query().Get("from"))
				if from != "" {
					if t, err := time.Parse(ts.DateLayout, from); err != nil || t.Format(ts.DateLayout) != from {
						server.WriteError(w, http.StatusBadRequest, "from: нужна дата ГГГГ-ММ-ДД, а не "+fmt.Sprintf("%q", from))
						return
					}
				}
				tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					s.Log.Error("sheets mirror", "err", err)
					server.WriteError(w, http.StatusInternalServerError, "база зеркала не прочитана: сбой сервиса, подробности в журнале")
					return
				}
				defer tx.Rollback()
				base, err := LoadMirrorBase(r.Context(), tx, from)
				if err != nil {
					s.Log.Error("sheets mirror", "key", auth.KeyName(r.Context()), "err", err)
					server.WriteError(w, http.StatusInternalServerError, "база зеркала не прочитана: сбой сервиса, подробности в журнале")
					return
				}
				server.WriteJSON(w, http.StatusOK, base)
			})))
	})
}

// MirrorTab is a tab of payroll_tabs as the mirror sees it. Skip says why the mirror never writes it: no person, or the
// person's accruals of the model go to another tab (the one planTabs picks); "" — the mirror writes it.
type MirrorTab struct {
	Model  string `json:"model"`
	Name   string `json:"name"`
	Person string `json:"person,omitempty"`
	Skip   string `json:"skip,omitempty"`
}

// MirrorPost is a posted page amount of the pay base (v_report_pages.posted_micros) for a person, model and report date.
type MirrorPost struct {
	Person string `json:"person"`
	Model  string `json:"model"`
	Date   string `json:"date"`
	Page   string `json:"page"`
	Micros int64  `json:"posted_micros"`
}

// MirrorBase is GET /v1/sheets-mirror: everything the writer needs from the database.
type MirrorBase struct {
	// ReportsFrom is settings.payroll_reports_from; "" — the reports do not accrue and nothing may be written.
	ReportsFrom string `json:"reports_from"`
	// From is the first date of Posts: the asked one, else ReportsFrom; "" — no posts.
	From       string            `json:"from"`
	Tabs       []MirrorTab       `json:"tabs"`
	SheetPages map[string]string `json:"sheet_pages"`
	Labels     []string          `json:"labels"`
	Posts      []MirrorPost      `json:"posts"`
}

// Resolver is the reader's rule for a row name (resolvePage) over the base's settings.
func (b MirrorBase) Resolver() func(string) (string, bool) {
	known := make(map[string]bool, len(b.Labels))
	for _, l := range b.Labels {
		known[l] = true
	}
	named := b.SheetPages
	if named == nil {
		named = map[string]string{}
	}
	return func(page string) (string, bool) { return resolvePage(named, known, page) }
}

// LoadMirrorBase reads the base from the switch day (or from, when given: a preview) on.
func LoadMirrorBase(ctx context.Context, q ledger.Querier, from string) (MirrorBase, error) {
	b := MirrorBase{Tabs: []MirrorTab{}, Labels: []string{}, Posts: []MirrorPost{}}
	var err error
	if b.ReportsFrom, err = ReportsFrom(ctx, q); err != nil {
		return b, err
	}
	b.From = from
	if b.From == "" {
		b.From = b.ReportsFrom
	}
	named, known, err := pageNames(ctx, q)
	if err != nil {
		return b, err
	}
	b.SheetPages = named
	for l := range known {
		b.Labels = append(b.Labels, l)
	}
	sort.Strings(b.Labels)
	_, tabOf, err := planTabs(ctx, q)
	if err != nil {
		return b, err
	}
	rows, err := q.QueryContext(ctx, `SELECT model, name, COALESCE(person, '') FROM payroll_tabs ORDER BY model, name`)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var t MirrorTab
		if err := rows.Scan(&t.Model, &t.Name, &t.Person); err != nil {
			rows.Close()
			return b, err
		}
		switch chosen, ok := tabOf[t.Person+"\x00"+ledger.Slug(t.Model)]; {
		case t.Person == "":
			t.Skip = "вкладка не сопоставлена с человеком"
		case !ok || chosen.name != t.Name || chosen.model != t.Model:
			t.Skip = fmt.Sprintf("начисления %s по модели %s идут на вкладку «%s»", t.Person, t.Model, chosen.name)
		}
		b.Tabs = append(b.Tabs, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return b, err
	}
	if b.From == "" {
		return b, nil
	}
	rows, err = q.QueryContext(ctx, `SELECT person, model, report_date, page, posted_micros FROM v_report_pages
		WHERE posted_micros IS NOT NULL AND report_date >= ? ORDER BY person, model, report_date, page`, b.From)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var p MirrorPost
		if err := rows.Scan(&p.Person, &p.Model, &p.Date, &p.Page, &p.Micros); err != nil {
			return b, err
		}
		b.Posts = append(b.Posts, p)
	}
	return b, rows.Err()
}

// MirrorLagText says what to do about the cells that lag: the health check and payables print it.
func MirrorLagText(cells int) string {
	return fmt.Sprintf("таблица отстаёт от отчётов: %d ячеек — `ledger mirror sheets`", cells)
}

// mirrorLagCells counts the cells the sheets as last read hold differently from the posted reports, from the switch day
// on: a posted page of a person's tab day whose row the tab has, and whose cell is empty or another amount. The writer
// sees more than payroll_days keeps (paid weeks, formulas), so this is what to run it for, not what it will write.
func mirrorLagCells(ctx context.Context, q ledger.Querier, from string) (int, error) {
	_, tabOf, err := planTabs(ctx, q)
	if err != nil {
		return 0, err
	}
	resolve, err := pageResolver(ctx, q)
	if err != nil {
		return 0, err
	}
	type cellKey struct{ model, name, date, label string }
	sheet := map[cellKey]*float64{}
	have := map[cellKey]bool{}
	rows, err := q.QueryContext(ctx, `SELECT model, name, date, pages FROM payroll_days WHERE person IS NOT NULL AND date >= ?`, from)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var model, name, date, pages string
		if err := rows.Scan(&model, &name, &date, &pages); err != nil {
			rows.Close()
			return 0, err
		}
		cells := map[string]*float64{}
		if err := json.Unmarshal([]byte(pages), &cells); err != nil {
			rows.Close()
			return 0, fmt.Errorf("payroll_days %s/%s %s: pages: %w", model, name, date, err)
		}
		for page, v := range cells {
			if l, ok := resolve(page); ok {
				k := cellKey{model, name, date, l}
				have[k] = true
				if v != nil {
					sheet[k] = v
				}
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows, err = q.QueryContext(ctx, `SELECT person, model, report_date, page, posted_micros FROM v_report_pages
		WHERE posted_micros IS NOT NULL AND report_date >= ?`, from)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var p MirrorPost
		if err := rows.Scan(&p.Person, &p.Model, &p.Date, &p.Page, &p.Micros); err != nil {
			return 0, err
		}
		t, ok := tabOf[p.Person+"\x00"+ledger.Slug(p.Model)]
		if !ok {
			continue
		}
		k := cellKey{t.model, t.name, p.Date, p.Page}
		if !have[k] {
			continue // no row or no column: the writer reports it, it cannot fix it
		}
		if v := sheet[k]; v == nil || money.FromFloat(*v) != money.Micros(p.Micros) {
			n++
		}
	}
	return n, rows.Err()
}
