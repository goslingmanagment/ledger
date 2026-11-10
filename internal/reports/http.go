package reports

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// maxBody bounds a report: a shift's breakdown is a few hundred lines.
const maxBody = 4 << 20

// readers are the scopes that read reports: agents (read) and Hub's server (payouts).
var readers = []string{auth.ScopeRead, auth.ScopePayouts}

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("POST /v1/reports", auth.Require(s, auth.ScopeReports, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, "тело запроса не прочитано или больше 4 МБ")
				return
			}
			out, err := Save(r.Context(), s.Store, raw, auth.KeyName(r.Context()), ts.Now())
			var ref *Refusal
			switch {
			case errors.As(err, &ref):
				server.WriteError(w, http.StatusBadRequest, ref.Error())
			case err != nil:
				s.Log.Error("reports save", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "отчёт не сохранён: сбой сервиса, подробности в журнале")
			default:
				// The draft is saved; the accruals from the reports follow, and a failure there is the hourly job's to retry.
				if out.Created {
					if err := Changed(r.Context(), s.Store, ts.Now()); err != nil {
						s.Log.Error("reports save: accruals", "key", auth.KeyName(r.Context()), "err", err)
					}
				}
				server.WriteJSON(w, http.StatusOK, out)
			}
		})))
		s.Mux.Handle("GET /v1/reports", auth.RequireAny(s, readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			from, to, err := payroll.PayoutWindow(q.Get("from"), q.Get("to"))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			f := Filter{From: from, To: to, Person: strings.TrimSpace(q.Get("person")),
				HubUsername: strings.TrimSpace(q.Get("hub_username")), AllVersions: q.Get("all_versions") == "1"}
			if v := strings.TrimSpace(q.Get("hub_user_id")); v != "" {
				id, err := strconv.ParseInt(v, 10, 64)
				if err != nil || id <= 0 {
					server.WriteError(w, http.StatusBadRequest, "hub_user_id: нужен id пользователя хаба, а не "+strconv.Quote(v))
					return
				}
				f.HubUserID = &id
			}
			tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				s.Log.Error("reports list", "err", err)
				server.WriteError(w, http.StatusInternalServerError, "отчёты не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			defer tx.Rollback()
			list, err := List(r.Context(), tx, f)
			if err != nil {
				s.Log.Error("reports list", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "отчёты не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ListAnswer{From: from, To: to, Reports: list})
		})))
		s.Mux.Handle("GET /v1/reports/{id}", auth.RequireAny(s, readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id <= 0 {
				server.WriteError(w, http.StatusBadRequest, "нужен номер отчёта: /v1/reports/<id>")
				return
			}
			tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				s.Log.Error("reports get", "err", err)
				server.WriteError(w, http.StatusInternalServerError, "отчёт не прочитан: сбой сервиса, подробности в журнале")
				return
			}
			defer tx.Rollback()
			rep, err := Get(r.Context(), tx, id)
			switch {
			case errors.Is(err, ErrNotFound):
				server.WriteError(w, http.StatusNotFound, "нет отчёта "+strconv.FormatInt(id, 10))
			case err != nil:
				s.Log.Error("reports get", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "отчёт не прочитан: сбой сервиса, подробности в журнале")
			default:
				server.WriteJSON(w, http.StatusOK, rep)
			}
		})))
	})
}
