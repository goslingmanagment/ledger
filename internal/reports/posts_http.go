package reports

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
)

func init() {
	server.Register(func(s *server.Server) {
		// The pay base: Hub's server reads it with its payouts key, for one chatter at a time.
		s.Mux.Handle("GET /v1/report-days", auth.RequireAny(s, readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			from, to, err := payroll.PayoutWindow(q.Get("from"), q.Get("to"))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			f := Filter{From: from, To: to, Person: strings.TrimSpace(q.Get("person")), HubUsername: strings.TrimSpace(q.Get("hub_username"))}
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
				s.Log.Error("report days", "err", err)
				server.WriteError(w, http.StatusInternalServerError, "база оплаты не прочитана: сбой сервиса, подробности в журнале")
				return
			}
			defer tx.Rollback()
			days, err := Days(r.Context(), tx, f)
			if err != nil {
				s.Log.Error("report days", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "база оплаты не прочитана: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, DaysAnswer{From: from, To: to, Days: days})
		})))
		// The posts hold their text: agents with read only, not Hub's payouts key.
		s.Mux.Handle("GET /v1/report-posts", auth.Require(s, auth.ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			from, to, err := payroll.PayoutWindow(q.Get("from"), q.Get("to"))
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				s.Log.Error("report posts", "err", err)
				server.WriteError(w, http.StatusInternalServerError, "посты не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			defer tx.Rollback()
			list, err := Posts(r.Context(), tx, from, to, q.Get("flagged") == "1")
			if err != nil {
				s.Log.Error("report posts", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "посты не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, PostsAnswer{From: from, To: to, Posts: list})
		})))
		// Parse now, the way the job does: the collector's key may ask, it only re-reads what it sent.
		s.Mux.Handle("POST /v1/report-posts/parse", auth.Require(s, auth.ScopeCollect, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var rep ParseReport
			err := brief.InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
				var err error
				rep, err = ParsePosts(r.Context(), tx, time.Now())
				return err
			})
			if err == nil {
				err = Changed(r.Context(), s.Store, time.Now())
			}
			if err != nil {
				s.Log.Error("report parse", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "посты не разобраны: "+err.Error())
				return
			}
			server.WriteJSON(w, http.StatusOK, rep)
		})))
	})
}
