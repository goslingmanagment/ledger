package month

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/payouts/month/{month} is the month card as Hub's owner screen «Деньги → Месяц» reads it with Hub's ledger key
// (scope payouts, which opens no SQL and not the full card of GET /v1/month/{month}). It serves the hourly row of
// month_cards as it is — nothing is computed per request — and only the money of the pages: the pages' revenue
// («не согласовано» included), «вне платформ», what is earned and where it is (arrived, riding, hold, on the page,
// stuck), by model, and the card's notes. Never the expenses, the decision number, the owner's draws, the requests,
// the transfers or anyone's pay. Money is integer micros, as the other payouts reads (/v1/payouts/person).

// Statuses of the answer.
const (
	MonthProven  = "proven"  // the hourly card is computed and its checks pass
	MonthUnknown = "unknown" // not computed, not proven, or a check failed: reason says why
)

// offChannel is the line of the money past the platforms.
const offChannel = "вне платформ"

// PayoutsMonth is the answer of GET /v1/payouts/month/{month}. A money field is null when the card has no figures.
type PayoutsMonth struct {
	OK         bool    `json:"ok"`
	Month      string  `json:"month"`
	AsOf       *string `json:"asOf"`       // the cut of the card; null — never computed
	ComputedAt *string `json:"computedAt"` // when the hourly job computed it
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
	// PagesNet is the net of every page line («не согласовано» included); OffPlatform what «вне платформ» received;
	// Turnover their sum.
	PagesNet    *int64 `json:"pagesNet"`
	OffPlatform *int64 `json:"offPlatform"`
	Turnover    *int64 `json:"turnover"`
	// The card's totals (its «Итого»): «Заработано», «Пришло», «Едет», «Холд», «Застряло», «На странице».
	Earned  *int64              `json:"earned"`
	Arrived *int64              `json:"arrived"`
	Riding  *int64              `json:"riding"`
	Hold    *int64              `json:"hold"`
	Stuck   *int64              `json:"stuck"`
	OnPage  *int64              `json:"onPage"`
	Models  []PayoutsMonthModel `json:"models"`
	Notes   []string            `json:"notes"`
}

// PayoutsMonthModel is a model's lines of the card added up.
type PayoutsMonthModel struct {
	Model       string `json:"model"`
	PagesNet    int64  `json:"pagesNet"`
	OffPlatform int64  `json:"offPlatform"`
	Earned      int64  `json:"earned"`
	Arrived     int64  `json:"arrived"`
	Riding      int64  `json:"riding"`
}

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payouts/month/{month}", auth.Require(s, auth.ScopePayouts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			month := r.PathValue("month")
			if _, _, err := ts.MonthStart(month); err != nil || len(month) != 7 {
				server.WriteError(w, http.StatusBadRequest, "месяц: нужен ГГГГ-ММ, а не «"+month+"»")
				return
			}
			ans, err := ReadPayoutsMonth(r.Context(), s.Store.RO, month)
			if err != nil {
				s.Log.Error("payouts month", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "карточка месяца не прочитана: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
	})
}

// storedCard is what ReadPayoutsMonth takes of a month_cards row's card (Card.Snapshot, the checks and the notes).
type storedCard struct {
	Totals *struct {
		Net     float64 `json:"net"`
		Earned  float64 `json:"earned"`
		Arrived float64 `json:"arrived"`
		Riding  float64 `json:"riding"`
		Page    float64 `json:"page"`
		Hold    float64 `json:"hold"`
		Stuck   float64 `json:"stuck"`
	} `json:"totals"`
	Lines []struct {
		Model   string  `json:"model"`
		Channel string  `json:"channel"`
		Net     float64 `json:"net"`
		Earned  float64 `json:"earned"`
		Arrived float64 `json:"arrived"`
		Riding  float64 `json:"riding"`
	} `json:"lines"`
	Checks []struct {
		N  int  `json:"n"`
		OK bool `json:"ok"`
	} `json:"checks"`
	Notes []string `json:"notes"`
}

// ReadPayoutsMonth reads the month's hourly card. A month never computed is unknown, with the reason, not an error.
func ReadPayoutsMonth(ctx context.Context, q ledger.Querier, month string) (PayoutsMonth, error) {
	ans := PayoutsMonth{OK: true, Month: month, Status: MonthUnknown, Models: []PayoutsMonthModel{}, Notes: []string{}}
	var asOf, computedAt, status, reason, raw string
	err := q.QueryRowContext(ctx, `SELECT as_of, computed_at, status, reason, card FROM month_cards WHERE month = ?`, month).
		Scan(&asOf, &computedAt, &status, &reason, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		why := "карточка месяца " + month + " не посчитана: джоб month-cards считает месяцы с первого полного по текущий раз в час"
		ans.Reason = &why
		return ans, nil
	}
	if err != nil {
		return ans, err
	}
	ans.AsOf, ans.ComputedAt = &asOf, &computedAt
	var c storedCard
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return ans, fmt.Errorf("month_cards %s: %w", month, err)
	}
	var why []string
	if reason != "" {
		why = append(why, reason)
	}
	var failed []string
	for _, ch := range c.Checks {
		if !ch.OK {
			failed = append(failed, fmt.Sprint(ch.N))
		}
	}
	if len(failed) > 0 {
		why = append(why, "проверки карточки не пройдены (№ "+strings.Join(failed, ", ")+"): цифры не для решений — ledger month show")
	}
	if c.Totals == nil {
		if status == "ok" {
			why = append(why, "в карточке нет итогов")
		}
		status = "unknown"
	}
	if status == "ok" && len(failed) == 0 {
		ans.Status = MonthProven
	}
	if len(why) > 0 {
		r := strings.Join(why, "; ")
		ans.Reason = &r
	}
	if c.Notes != nil {
		ans.Notes = c.Notes
	}
	if c.Totals == nil {
		return ans, nil
	}
	m := func(x float64) int64 { return int64(money.FromFloat(x)) }
	ptr := func(v int64) *int64 { return &v }
	ans.Earned, ans.Arrived, ans.Riding = ptr(m(c.Totals.Earned)), ptr(m(c.Totals.Arrived)), ptr(m(c.Totals.Riding))
	ans.Hold, ans.Stuck, ans.OnPage = ptr(m(c.Totals.Hold)), ptr(m(c.Totals.Stuck)), ptr(m(c.Totals.Page))
	// The top figures are the card's own «Итого»: its «Оборот» (totals.net) leaves «не согласовано» out, so it is added back
	// here. The models are their lines added up, rounded line by line as the card stores them: a cent from the total.
	var unagreed, off int64
	byModel := map[string]int{}
	for _, l := range c.Lines {
		i, ok := byModel[l.Model]
		if !ok {
			i = len(ans.Models)
			byModel[l.Model] = i
			ans.Models = append(ans.Models, PayoutsMonthModel{Model: l.Model})
		}
		x := &ans.Models[i]
		if l.Channel == offChannel {
			x.OffPlatform += m(l.Net)
			off += m(l.Net)
		} else {
			x.PagesNet += m(l.Net)
			if strings.HasSuffix(l.Channel, "не согласовано") {
				unagreed += m(l.Net)
			}
		}
		x.Earned += m(l.Earned)
		x.Arrived += m(l.Arrived)
		x.Riding += m(l.Riding)
	}
	turnover := m(c.Totals.Net) + unagreed
	ans.PagesNet, ans.OffPlatform, ans.Turnover = ptr(turnover-off), ptr(off), ptr(turnover)
	return ans, nil
}
