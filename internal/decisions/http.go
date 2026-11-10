package decisions

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Writers are the scopes that write decisions: the team's chat agent (reports) and the owner (decide).
var Writers = []string{auth.ScopeReports, auth.ScopeDecide}

// Readers are the scopes that read them: agents (read), Hub's server (payouts) and the chat agent (reports).
var Readers = []string{auth.ScopeRead, auth.ScopePayouts, auth.ScopeReports}

const maxBody = 1 << 16

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("POST /v1/decisions", auth.RequireAny(s, Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in Input
			if !read(w, r, &in) {
				return
			}
			d, err := Add(r.Context(), s.Store, in, auth.KeyName(r.Context()), ts.Now())
			answer(s, w, d, err)
		})))
		s.Mux.Handle("POST /v1/decisions/{id}/applied", auth.RequireAny(s, Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := pathID(w, r)
			if !ok {
				return
			}
			var in AppliedInput
			if !read(w, r, &in) {
				return
			}
			d, err := Applied(r.Context(), s.Store, id, in, ts.Now())
			answer(s, w, d, err)
		})))
		s.Mux.Handle("POST /v1/decisions/{id}/cancel", auth.RequireAny(s, Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := pathID(w, r)
			if !ok {
				return
			}
			var in CancelInput
			if !read(w, r, &in) {
				return
			}
			d, err := Cancel(r.Context(), s.Store, id, in, ts.Now())
			answer(s, w, d, err)
		})))
		s.Mux.Handle("GET /v1/decisions", auth.RequireAny(s, Readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			f := Filter{Open: q.Get("open") == "1", Fan: strings.TrimSpace(q.Get("fan")), Person: strings.TrimSpace(q.Get("person"))}
			for name, dst := range map[string]*string{"from": &f.From, "to": &f.To} {
				if v := strings.TrimSpace(q.Get(name)); v != "" {
					d, err := NormDate(name, v)
					if err != nil {
						server.WriteError(w, http.StatusBadRequest, err.Error())
						return
					}
					*dst = d
				}
			}
			list, err := List(r.Context(), s.Store.RO, f)
			if err != nil {
				s.Log.Error("decisions list", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "решения не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ListAnswer{Decisions: list})
		})))
	})
}

// ListAnswer is the reply of GET /v1/decisions.
type ListAnswer struct {
	Decisions []Decision `json:"decisions"`
}

func read(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, "тело запроса не прочитано или больше 64 КБ")
		return false
	}
	if err := Decode(raw, v); err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		server.WriteError(w, http.StatusBadRequest, "нужен номер решения: /v1/decisions/<id>/…")
		return 0, false
	}
	return id, true
}

func answer(s *server.Server, w http.ResponseWriter, d Decision, err error) {
	var ref *Refusal
	switch {
	case errors.As(err, &ref):
		server.WriteError(w, http.StatusUnprocessableEntity, ref.Error())
	case errors.Is(err, ErrNotFound):
		server.WriteError(w, http.StatusNotFound, "нет такого решения")
	case err != nil:
		s.Log.Error("decisions", "err", err)
		server.WriteError(w, http.StatusInternalServerError, "решение не записано: сбой сервиса, подробности в журнале")
	default:
		server.WriteJSON(w, http.StatusOK, d)
	}
}
