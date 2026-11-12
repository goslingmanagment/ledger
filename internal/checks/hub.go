package checks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/health/jobs (below) is the last outcome of each scheduled job of this process, from memory (server.Jobs).
//
// GET /v1/payouts/checks is the last run of the health checks for Hub's server, which raises ledger's own findings as
// matters for the owner: each check with a stable key, its sentence as ledger words it, the severity, ok or not and
// since when it has failed without a passing run between, plus the tabs of the week-columns notice. Only the payouts
// scope opens it; it reads what `ledger health` shows, never runs the checks, and gives no other details, no SQL.

// Keys are the stable machine keys of the checks, by name: the names are Russian sentences and may be reworded.
var Keys = map[string]string{
	"проводки сходятся":                         "balanced",
	cashCheck:                                   "bybit_cash",
	"монеты Bybit на счетах":                    "bybit_coins",
	"монеты Bybit по цене покупки":              "bybit_coins_at_cost",
	"карты против бота":                         "card_anchors",
	"карты покрывают списания":                  "card_cover",
	"движения Bybit разобраны":                  "bybit_unlinked",
	"заявки равны покрытому net":                "requests",
	"k переводов в норме":                       "transfer_k",
	"переводы не просрочены":                    "transfers_overdue",
	"депозиты сходятся с заявками":              "deposits",
	"долги равны «К выплате»":                   "payables",
	"дрейф закрытых месяцев":                    "month_drift",
	"копии базы":                                "copies",
	"источники живы":                            "sources",
	"отчёты разобраны":                          "report_posts",
	"у смен есть отчёты":                        "shift_reports",
	hubReportsCheck:                             "hub_reports",
	hubDecisionsCheck:                           "hub_decisions",
	"изменения за выплаченные дни ждут решения": "pending_changes",
	weekColumnsCheck:                            "week_columns",
	"ставки вкладок как в карточках":            "tab_rates",
	payTermsCheck: "pay_terms",
	checksRunning: "checks_running",
}

// KeyOf is the key of a check; a name without one keys as itself.
func KeyOf(name string) string {
	if k, ok := Keys[name]; ok {
		return k
	}
	return name
}

// HubCheck is one check as Hub reads it.
type HubCheck struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"` // of a failure: info, warn, urgent; a passing check is info
	Summary  string `json:"summary"`
	// At is when this result was recorded; "" — the check never ran.
	At string `json:"at"`
	// FailingSince is the first run of the failure that holds now, with no passing run after it; null — the check passes.
	FailingSince *string `json:"failingSince"`
}

// HubWeekColumns is the week-columns notice: the tabs someone works on with no column for tomorrow.
type HubWeekColumns struct {
	Tabs []TabColumn `json:"tabs"`
}

// HubChecks is the reply of GET /v1/payouts/checks.
type HubChecks struct {
	OK          bool           `json:"ok"`
	At          string         `json:"at"` // the last run of the checks; "" — never
	GeneratedAt string         `json:"generatedAt"`
	Checks      []HubCheck     `json:"checks"`
	WeekColumns HubWeekColumns `json:"weekColumns"`
}

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payouts/checks", auth.Require(s, auth.ScopePayouts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ans, err := ReadHubChecks(r.Context(), s.Store.RO, time.Now())
			if err != nil {
				s.Log.Error("hub checks", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "проверки не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
		// The scheduler's own jobs for Hub's «Состояние»: the last outcome of each since the process started, from memory.
		s.Mux.Handle("GET /v1/health/jobs", auth.Require(s, auth.ScopePayouts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			server.WriteJSON(w, http.StatusOK, s.Jobs(time.Now()))
		})))
	})
}

// ReadHubChecks reads the last result of every check (v_checks), since when each failing one fails, and the stopped-job
// result of WithStale when the last run is old.
func ReadHubChecks(ctx context.Context, q ledger.Querier, now time.Time) (HubChecks, error) {
	ans := HubChecks{OK: true, GeneratedAt: ts.Format(now), Checks: []HubCheck{}, WeekColumns: HubWeekColumns{Tabs: []TabColumn{}}}
	rows, err := q.QueryContext(ctx, `SELECT c.name, c.ok, c.severity, c.summary, c.details, c.finished_at,
		CASE WHEN c.ok = 0 THEN (SELECT MIN(x.finished_at) FROM checks x WHERE x.name = c.name AND x.ok = 0
			AND x.id > COALESCE((SELECT MAX(y.id) FROM checks y WHERE y.name = c.name AND y.ok = 1), 0)) END
		FROM v_checks c ORDER BY c.name`)
	if err != nil {
		return ans, err
	}
	defer rows.Close()
	var results []Result
	for rows.Next() {
		var c HubCheck
		var details string
		var since sql.NullString
		if err := rows.Scan(&c.Name, &c.OK, &c.Severity, &c.Summary, &details, &c.At, &since); err != nil {
			return ans, err
		}
		c.Key = KeyOf(c.Name)
		if since.Valid {
			c.FailingSince = &since.String
		}
		if c.At > ans.At {
			ans.At = c.At
		}
		if c.Name == weekColumnsCheck && details != "{}" && details != "null" {
			var tabs []TabColumn
			if err := json.Unmarshal([]byte(details), &tabs); err != nil {
				return ans, errors.New("проверка «" + weekColumnsCheck + "»: details: " + err.Error())
			}
			ans.WeekColumns.Tabs = append(ans.WeekColumns.Tabs, tabs...)
		}
		ans.Checks = append(ans.Checks, c)
		results = append(results, Result{Name: c.Name, OK: c.OK})
	}
	if err := rows.Err(); err != nil {
		return ans, err
	}
	// The job stopped: its old green must not read as green.
	if all := WithStale(results, ans.At, now); len(all) > len(results) {
		r := all[len(all)-1]
		c := HubCheck{Key: KeyOf(r.Name), Name: r.Name, Severity: r.Severity, Summary: r.Summary}
		if ans.At != "" {
			at := ans.At
			c.FailingSince = &at
		}
		ans.Checks = append(ans.Checks, c)
	}
	return ans, nil
}
