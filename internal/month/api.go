package month

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Inputs of a computation.
type Inputs struct {
	AsOf     time.Time // zero: the latest proven cut
	DocsAsOf string    // "": current journal rows
	Tabs     []Tab     // nil: from the database
	TabsErr  string
}

// PrepareCover builds the coverage for a request.
func PrepareCover(ctx context.Context, q ledger.Querier, in Inputs) (*Cover, []Tab, string, error) {
	cfg, err := LoadConfig(ctx, q)
	if err != nil {
		return nil, nil, "", err
	}
	if in.AsOf.IsZero() {
		if in.AsOf, err = LiveAsOf(ctx, q, cfg); err != nil {
			return nil, nil, "", err
		}
	}
	rows, err := JournalRows(ctx, q, in.DocsAsOf)
	if err != nil {
		return nil, nil, "", err
	}
	cv, err := BuildCover(cfg, DBSource{Ctx: ctx, Q: q}, rows, in.AsOf)
	if err != nil {
		return nil, nil, "", err
	}
	tabs, tabsErr := in.Tabs, in.TabsErr
	if tabs == nil {
		if tabs, tabsErr, err = LoadTabs(ctx, q); err != nil {
			return nil, nil, "", err
		}
		if tabs == nil {
			tabs = []Tab{}
		}
	}
	return cv, tabs, tabsErr, nil
}

// Show computes a month's card and its text.
func Show(ctx context.Context, q ledger.Querier, month string, in Inputs, ceoRate *float64) (map[string]any, error) {
	cv, tabs, tabsErr, err := PrepareCover(ctx, q, in)
	if err != nil {
		return nil, err
	}
	card, err := BuildCard(cv, month, tabs)
	if err != nil {
		return nil, err
	}
	res := card.Checks(tabsErr)
	if ceoRate != nil {
		if card.RaiseFrom, err = raiseFrom(ctx, q, month, *ceoRate+cv.Cfg.GuardStep); err != nil {
			return nil, err
		}
	}
	snap := card.Snapshot()
	snap["checks"] = res
	snap["notes"] = card.Notes
	snap["text"] = card.Render(ceoRate, res, nil)
	now, atClose, err := ledger.MonthTotals(ctx, q, month)
	if err != nil {
		return nil, err
	}
	_ = now
	if atClose != nil {
		if p, err := ledger.GetPeriod(ctx, q, month); err == nil && p.Decision != nil {
			snap["decision_at_close"] = p.Decision.Float()
			snap["verdict_at_close"] = p.Verdict
			snap["locked_at"] = p.LockedAt
		}
	}
	return snap, nil
}

func httpFail(w http.ResponseWriter, err error) {
	var f Fail
	if errors.As(err, &f) || hub.IsRefusal(err) {
		server.WriteError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	server.WriteError(w, http.StatusInternalServerError, err.Error())
}

func floatParam(r *http.Request, name string) (*float64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// rateParam is the owner's salary rate a month is judged with: the parameter, otherwise the setting
// owner_salary_rate in force on the month's first day.
func rateParam(r *http.Request, q ledger.Querier, month string) (*float64, error) {
	rate, err := floatParam(r, "ceo_rate")
	if err != nil || rate != nil {
		return rate, err
	}
	return settingRate(r.Context(), q, month)
}

func settingRate(ctx context.Context, q ledger.Querier, month string) (*float64, error) {
	v, _, ok, err := ledger.OwnerRate(ctx, q, month+"-01")
	if err != nil || !ok {
		return nil, err
	}
	return &v, nil
}

// prevMonth is the month before month ("ГГГГ-ММ").
func prevMonth(month string) string {
	m0, _, _ := ts.MonthStart(month)
	return ts.Month(m0.AddDate(0, 0, -1))
}

// raiseFrom says from which day the raise that month's safeguard tests is already in force: the owner
// accepted it before the close. "" when it is not.
func raiseFrom(ctx context.Context, q ledger.Querier, month string, candidate float64) (string, error) {
	_, m1, err := ts.MonthStart(month)
	if err != nil {
		return "", err
	}
	next, from, ok, err := ledger.OwnerRate(ctx, q, ts.Date(m1))
	if err != nil || !ok || math.Abs(next-candidate) >= 0.005 {
		return "", err
	}
	if from == "" {
		from = ts.Date(m1)
	}
	return from, nil
}

func init() {
	server.Every("month-cards", time.Hour, func(ctx context.Context, s *server.Server) error {
		tx, err := s.Store.RW.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now := time.Now()
		cfg, err := LoadConfig(ctx, tx)
		if err != nil {
			return err
		}
		asOf, err := LiveAsOf(ctx, tx, cfg)
		if err != nil {
			if !hub.IsRefusal(err) {
				return err
			}
			// The figures stay for reading, but they are no longer proven.
			if _, err := tx.ExecContext(ctx, `UPDATE month_cards SET status = 'unknown', reason = ?, computed_at = ?`, err.Error(), ts.Format(now)); err != nil {
				return err
			}
			return tx.Commit()
		}
		if err := RefreshCards(ctx, tx, asOf, now); err != nil {
			return err
		}
		return tx.Commit()
	})
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/month/{month}", auth.Require(s, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in Inputs
			if v := r.URL.Query().Get("as_of"); v != "" {
				t, err := ts.Parse(v)
				if err != nil {
					server.WriteError(w, http.StatusBadRequest, err.Error())
					return
				}
				in.AsOf = t
			}
			rate, err := rateParam(r, s.Store.RO, r.PathValue("month"))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, "ceo_rate: "+err.Error())
				return
			}
			out, err := Show(r.Context(), s.Store.RO, r.PathValue("month"), in, rate)
			if err != nil {
				httpFail(w, err)
				return
			}
			out["ok"] = true
			server.WriteJSON(w, http.StatusOK, out)
		})))
		s.Mux.Handle("GET /v1/months", auth.Require(s, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cv, tabs, _, err := PrepareCover(r.Context(), s.Store.RO, Inputs{})
			if err != nil {
				httpFail(w, err)
				return
			}
			rows, err := Drift(r.Context(), s.Store.RO, cv, tabs)
			if err != nil {
				httpFail(w, err)
				return
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "months": rows})
		})))
		s.Mux.Handle("POST /v1/months/accept", auth.Require(s, "decide", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			cv, tabs, _, err := PrepareCover(ctx, s.Store.RO, Inputs{})
			if err != nil {
				httpFail(w, err)
				return
			}
			rows, err := Drift(ctx, s.Store.RO, cv, tabs)
			if err != nil {
				httpFail(w, err)
				return
			}
			tx, err := s.Store.RW.BeginTx(ctx, nil)
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			defer tx.Rollback()
			if err = Accept(ctx, tx, rows, time.Now()); err == nil {
				err = tx.Commit()
			}
			if err != nil {
				httpFail(w, err)
				return
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": len(rows)})
		})))
		s.Mux.Handle("GET /v1/month-brief", auth.Require(s, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			rate, err := floatParam(r, "ceo_rate")
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, "ceo_rate: "+err.Error())
				return
			}
			out, flagged, err := BriefAt(ctx, s.Store.RO, time.Now(), rate, true)
			if err != nil {
				httpFail(w, err)
				return
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "text": strings.Join(out, "\n"), "flagged": len(flagged)})
		})))
		s.Mux.Handle("POST /v1/month/{month}/close", auth.Require(s, "decide", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			rate, err := rateParam(r, s.Store.RO, r.PathValue("month"))
			if err != nil || rate == nil {
				server.WriteError(w, http.StatusBadRequest, "нужен ceo_rate или настройка owner_salary_rate")
				return
			}
			out, err := CloseNow(ctx, s, r.PathValue("month"), *rate)
			if err != nil {
				httpFail(w, err)
				return
			}
			server.WriteJSON(w, http.StatusOK, out)
		})))
	})
}

// BriefAt computes the Monday message: the cut is the end of yesterday (or the proven cut if earlier).
// Without a rate, fromSetting takes the one in force in the month the message judges: the previous one.
func BriefAt(ctx context.Context, q ledger.Querier, now time.Time, rate *float64, fromSetting bool) ([]string, []DriftRow, error) {
	n := now.In(ts.MSK)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, ts.MSK)
	cv, tabs, _, err := PrepareCover(ctx, q, Inputs{AsOf: midnight.Add(-time.Second)})
	if err != nil {
		return nil, nil, err
	}
	if rate == nil && fromSetting {
		if rate, err = settingRate(ctx, q, prevMonth(ts.Month(cv.AsOf))); err != nil {
			return nil, nil, err
		}
	}
	return Brief(ctx, q, cv, tabs, rate)
}

// CloseNow closes a month at the latest proven cut; the answer says what stopped it, if anything.
func CloseNow(ctx context.Context, s *server.Server, month string, rate float64) (map[string]any, error) {
	tx, err := s.Store.RW.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cfg, err := LoadConfig(ctx, tx)
	if err != nil {
		return nil, err
	}
	asOf, err := LiveAsOf(ctx, tx, cfg)
	if err != nil {
		return nil, err
	}
	card, stops, err := Close(ctx, tx, month, asOf, rate, time.Now(), "ledger month close")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "closed": len(stops) == 0, "stops": stops}
	if card != nil {
		if card.RaiseFrom, err = raiseFrom(ctx, tx, month, rate+cfg.GuardStep); err != nil {
			return nil, err
		}
		out["text"] = card.Render(&rate, nil, nil)
	}
	if len(stops) > 0 {
		return out, nil
	}
	return out, tx.Commit()
}
