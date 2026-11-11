package budgets

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const maxBody = 1 << 17

// GET /v1/budgets is the budget by month for Hub's console: Hub's server reads it with a `budgets` key on the
// owner's behalf (the screen is the owner's only). POST /v1/budgets/plans sets plans: the owner from the console
// (`budgets`) or his agent session (`decide`).
func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/budgets", auth.RequireAny(s, []string{auth.ScopeBudgets, auth.ScopeRead}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, "бюджеты не прочитаны: сбой сервиса")
				return
			}
			defer tx.Rollback()
			ans, err := Read(r.Context(), tx, r.URL.Query().Get("from"), r.URL.Query().Get("to"), ts.Now())
			if err != nil {
				if IsFail(err) {
					server.WriteError(w, http.StatusBadRequest, err.Error())
					return
				}
				s.Log.Error("budgets", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "бюджеты не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
		s.Mux.Handle("POST /v1/budgets/plans", auth.RequireAny(s, []string{auth.ScopeBudgets, auth.ScopeDecide}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in SetInput
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				server.WriteError(w, http.StatusBadRequest, "тело запроса: "+err.Error())
				return
			}
			tx, err := s.Store.RW.BeginTx(r.Context(), nil)
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, "план не записан: сбой сервиса")
				return
			}
			defer tx.Rollback()
			ans, err := SetPlans(r.Context(), tx, in, auth.KeyName(r.Context()), ts.Now())
			if err == nil {
				err = tx.Commit()
			}
			if err != nil {
				if IsFail(err) {
					server.WriteError(w, http.StatusBadRequest, err.Error())
					return
				}
				s.Log.Error("budget plans", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "план не записан: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
	})
}
