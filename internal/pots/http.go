package pots

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const maxBody = 1 << 16

func init() {
	server.Register(func(s *server.Server) {
		// Only the owner's word declares or changes a pot.
		s.Mux.Handle("POST /v1/pots", auth.Require(s, auth.ScopeDecide, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in DeclareInput
			if !read(w, r, &in) {
				return
			}
			p, err := Declare(r.Context(), s.Store, in, auth.KeyName(r.Context()), ts.Now())
			if answer(s, w, p, err, "общак не записан") {
				changed(s, r)
			}
		})))
		s.Mux.Handle("GET /v1/pots", auth.RequireAny(s, decisions.Readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			list, err := List(r.Context(), s.Store.RO)
			answer(s, w, ListAnswer{Pots: list}, err, "общаки не прочитаны")
		})))
		s.Mux.Handle("GET /v1/pots/candidates", auth.RequireAny(s, decisions.Readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug := strings.TrimSpace(r.URL.Query().Get("pot"))
			tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				answer(s, w, nil, err, "кандидаты не прочитаны")
				return
			}
			defer tx.Rollback()
			list, err := Candidates(r.Context(), tx, slug)
			answer(s, w, CandidatesAnswer{Candidates: list}, err, "кандидаты не прочитаны")
		})))
		s.Mux.Handle("GET /v1/pots/{slug}", auth.RequireAny(s, decisions.Readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tx, err := s.Store.RO.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				answer(s, w, nil, err, "общак не прочитан")
				return
			}
			defer tx.Rollback()
			out, err := Show(r.Context(), tx, r.PathValue("slug"))
			answer(s, w, out, err, "общак не прочитан")
		})))
		s.Mux.Handle("POST /v1/pots/{slug}/sales", auth.RequireAny(s, decisions.Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in SaleInput
			if !read(w, r, &in) {
				return
			}
			source := SourceAgent
			if k, ok := auth.FromContext(r.Context()); ok && k.Has(auth.ScopeDecide) {
				source = SourceOwner
			}
			out, err := AddSale(r.Context(), s.Store, r.PathValue("slug"), in, source, auth.KeyName(r.Context()), ts.Now())
			if answer(s, w, out, err, "покупка не записана") {
				changed(s, r)
			}
		})))
		s.Mux.Handle("POST /v1/pot-sales/{id}/remove", auth.RequireAny(s, decisions.Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id <= 0 {
				server.WriteError(w, http.StatusBadRequest, "нужен номер покупки: /v1/pot-sales/<id>/remove")
				return
			}
			var in RemoveInput
			if !read(w, r, &in) {
				return
			}
			out, err := RemoveSale(r.Context(), s.Store, id, in, auth.KeyName(r.Context()), ts.Now())
			if answer(s, w, out, err, "покупка не снята") {
				changed(s, r)
			}
		})))
	})
}

// ListAnswer is the reply of GET /v1/pots.
type ListAnswer struct {
	Pots []Pot `json:"pots"`
}

// CandidatesAnswer is the reply of GET /v1/pots/candidates.
type CandidatesAnswer struct {
	Candidates []Candidate `json:"candidates"`
}

// changed recomputes the accruals after a write; a failure is the hourly job's to retry.
func changed(s *server.Server, r *http.Request) {
	if err := Changed(r.Context(), s.Store, ts.Now()); err != nil {
		s.Log.Error("pots: accruals", "key", auth.KeyName(r.Context()), "err", err)
	}
}

func read(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, "тело запроса не прочитано или больше 64 КБ")
		return false
	}
	if err := decisions.Decode(raw, v); err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// answer writes the result or the error; true — written OK.
func answer(s *server.Server, w http.ResponseWriter, out any, err error, failed string) bool {
	var ref *Refusal
	switch {
	case errors.As(err, &ref):
		server.WriteError(w, http.StatusUnprocessableEntity, ref.Error())
	case errors.Is(err, ErrNotFound):
		server.WriteError(w, http.StatusNotFound, "нет такого общака или покупки")
	case err != nil:
		s.Log.Error("pots", "err", err)
		server.WriteError(w, http.StatusInternalServerError, failed+": сбой сервиса, подробности в журнале")
	default:
		server.WriteJSON(w, http.StatusOK, out)
		return true
	}
	return false
}
