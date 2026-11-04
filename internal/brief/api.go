package brief

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/bybit"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// SweepReport says what a sweep closed.
type SweepReport struct {
	Notes      bybit.NoteLinks `json:"note_links"`
	Superseded int             `json:"proposals_superseded"`
}

// Sweep is the post-import step: journal rows naming a movement become links, and open proposals of
// linked movements close, so nothing surfaces twice.
func Sweep(ctx context.Context, q ledger.Querier, by string, now time.Time) (SweepReport, error) {
	var rep SweepReport
	var err error
	if rep.Notes, err = bybit.LinkNotes(ctx, q, now); err != nil {
		return rep, err
	}
	if rep.Superseded, err = bybit.SupersedeLinked(ctx, q, by, now); err != nil {
		return rep, err
	}
	return rep, closeResolved(ctx, q, now)
}

// InTx runs f in one write transaction, committing when f returns nil.
func InTx(ctx context.Context, store *db.Store, f func(*sql.Tx) error) error {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func init() {
	server.Every("brief-sweep", 15*time.Minute, func(ctx context.Context, s *server.Server) error {
		return InTx(ctx, s.Store, func(tx *sql.Tx) error {
			_, err := Sweep(ctx, tx, "job:brief-sweep", time.Now())
			return err
		})
	})
	server.Register(install)
}

// fail writes an error: a refusal is the caller's request, the rest is the service's.
func fail(s *server.Server, w http.ResponseWriter, err error) {
	var r *Refusal
	if errors.As(err, &r) {
		server.WriteError(w, http.StatusUnprocessableEntity, r.Error())
		return
	}
	s.Log.Error("brief", "err", err)
	server.WriteError(w, http.StatusInternalServerError, "сбой сервиса, подробности в журнале")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		server.WriteError(w, http.StatusBadRequest, "тело запроса: нужен JSON")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		server.WriteError(w, http.StatusBadRequest, "нужен номер")
		return 0, false
	}
	return id, true
}

func install(s *server.Server) {
	handle := func(pattern, scope string, h func(http.ResponseWriter, *http.Request)) {
		s.Mux.Handle(pattern, auth.Require(s, scope, http.HandlerFunc(h)))
	}
	handle("POST /v1/brief", auth.ScopePropose, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			New bool `json:"new"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		var out Answer
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			b, err := Make(r.Context(), tx, Options{New: in.New, Author: auth.KeyName(r.Context())}, ts.Now())
			out = Respond(b)
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, out)
	})
	handle("GET /v1/brief/{id}", auth.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		b, err := Get(r.Context(), s.Store.RO, id)
		if errors.Is(err, sql.ErrNoRows) {
			server.WriteError(w, http.StatusNotFound, "брифа нет")
			return
		}
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, Respond(b))
	})
	handle("POST /v1/brief/{id}/sent", auth.ScopePropose, func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var at string
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			var err error
			at, err = MarkSent(r.Context(), tx, id, ts.Now())
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "brief": id, "sent_at": at})
	})
	handle("POST /v1/brief/{id}/decide", auth.ScopeDecide, func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Set []Decision `json:"set"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		var out *Result
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			var err error
			out, err = Decide(r.Context(), tx, id, in.Set, auth.KeyName(r.Context()), ts.Now())
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, out)
	})
	handle("GET /v1/pending", auth.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		list, err := Pending(r.Context(), s.Store.RO)
		if err != nil {
			fail(s, w, err)
			return
		}
		out := make([]Answer, 0, len(list))
		for _, b := range list {
			out = append(out, Respond(b))
		}
		server.WriteJSON(w, http.StatusOK, out)
	})
	handle("POST /v1/proposals", auth.ScopePropose, func(w http.ResponseWriter, r *http.Request) {
		var in AgentProposal
		if !decodeBody(w, r, &in) {
			return
		}
		var out map[string]any
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			p, change, err := Propose(r.Context(), tx, in, auth.KeyName(r.Context()), ts.Now())
			out = map[string]any{"ok": true, "change": change, "proposal": p}
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, out)
	})
	handle("POST /v1/evidence", auth.ScopePropose, func(w http.ResponseWriter, r *http.Request) {
		var in Evidence
		if !decodeBody(w, r, &in) {
			return
		}
		in.CreatedAt, in.Author = "", auth.KeyName(r.Context())
		var added bool
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			var err error
			added, err = AddEvidence(r.Context(), tx, in, ts.Now())
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added})
	})
	handle("GET /v1/evidence", auth.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		list, err := EvidenceFor(r.Context(), s.Store.RO, r.URL.Query().Get("key"))
		if err != nil {
			fail(s, w, err)
			return
		}
		if list == nil {
			list = []Evidence{}
		}
		server.WriteJSON(w, http.StatusOK, list)
	})
	handle("POST /v1/disputes", auth.ScopePropose, func(w http.ResponseWriter, r *http.Request) {
		var in OpenInput
		if !decodeBody(w, r, &in) {
			return
		}
		var out Dispute
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			var err error
			out, err = OpenDispute(r.Context(), tx, in, auth.KeyName(r.Context()), ts.Now())
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, out)
	})
	handle("GET /v1/disputes", auth.ScopeRead, func(w http.ResponseWriter, r *http.Request) {
		list, err := ListDisputes(r.Context(), s.Store.RO, r.URL.Query().Get("status"))
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, list)
	})
	handle("POST /v1/disputes/{id}/resolve", auth.ScopeDecide, func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in ResolveInput
		if !decodeBody(w, r, &in) {
			return
		}
		var out Dispute
		err := InTx(r.Context(), s.Store, func(tx *sql.Tx) error {
			var err error
			out, err = ResolveDispute(r.Context(), tx, id, in, auth.KeyName(r.Context()), ts.Now())
			return err
		})
		if err != nil {
			fail(s, w, err)
			return
		}
		server.WriteJSON(w, http.StatusOK, out)
	})
}
