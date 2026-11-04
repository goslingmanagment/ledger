package cards

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	journal.AfterImport = func(ctx context.Context, q ledger.Querier, now time.Time) error {
		_, err := Process(ctx, q, now)
		return err
	}
	server.Every("cards", 15*time.Minute, func(ctx context.Context, s *server.Server) error {
		return brief.InTx(ctx, s.Store, func(tx *sql.Tx) error {
			rep, err := Process(ctx, tx, time.Now())
			for _, p := range rep.Problems {
				s.Log.Warn("cards", "problem", p)
			}
			return err
		})
	})
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/cards/balances", auth.Require(s, auth.ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			list, err := Balances(r.Context(), s.Store.RO)
			if err != nil {
				s.Log.Error("cards", "err", err)
				server.WriteError(w, http.StatusInternalServerError, "сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, list)
		})))
		s.Mux.Handle("POST /v1/cards/link", auth.Require(s, auth.ScopeDecide, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				MsgID      int64  `json:"msg_id"`
				Withdrawal string `json:"withdrawal"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
				server.WriteError(w, http.StatusBadRequest, "тело запроса: нужен JSON {msg_id, withdrawal}")
				return
			}
			var rep Report
			err := brief.InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
				var err error
				rep, err = LinkDeposit(r.Context(), tx, in.MsgID, in.Withdrawal, ts.Now())
				return err
			})
			var refusal *brief.Refusal
			if errors.As(err, &refusal) {
				server.WriteError(w, http.StatusUnprocessableEntity, refusal.Error())
				return
			}
			if err != nil {
				s.Log.Error("cards", "err", err)
				server.WriteError(w, http.StatusInternalServerError, "сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, rep)
		})))
	})
}
