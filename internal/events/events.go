// Package events is the door for collectors that do not live on the server: POST /v1/events takes raw
// facts, one or a batch, and stores each once through ledger.AddEvent. A source is known only after a
// feature registers it, so a collector key cannot write facts of a source nobody reads, and each
// source decides in its own hooks what a fact changes.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// maxBody bounds one request: a batch of sheet tabs is a few megabytes.
const maxBody = 64 << 20

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("POST /v1/events", auth.Require(s, auth.ScopeCollect, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handle(s, w, r)
		})))
	})
}

// Event is one fact as a collector sends it. OccurredAt is when the collector saw or the source
// dated it; a repeat keeps the stored row, and the source's Arrive hook still gets the new time.
type Event struct {
	Source     string          `json:"source"`
	SourceID   string          `json:"source_id"`
	Kind       string          `json:"kind"`
	OccurredAt string          `json:"occurred_at,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// Arrival is a stored event handed to the source's hook, new or repeated.
type Arrival struct {
	Event
	ID       int64
	Inserted bool
	Now      time.Time
}

// Source is what collectors may push. Arrive runs inside the batch's transaction for every event of
// the source and says whether derived state changed; its error rolls the whole batch back, so a fact
// the source cannot read is not stored (a *BadRequest says the fact is at fault). Settle runs after the commit when something changed: its
// failure is reported but leaves the facts in place, the source's hourly job catches up.
type Source struct {
	Name   string
	Arrive func(ctx context.Context, tx *sql.Tx, a Arrival) (changed bool, err error)
	Settle func(ctx context.Context, store *db.Store, now time.Time) error
}

var (
	mu      sync.Mutex
	sources = map[string]Source{}
)

// Register makes a source known; features call it from init().
func Register(s Source) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := sources[s.Name]; dup {
		panic("duplicate event source " + s.Name)
	}
	sources[s.Name] = s
}

func lookup(name string) (Source, bool) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := sources[name]
	return s, ok
}

func known() string {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Result is what happened to one event of a batch.
type Result struct {
	SourceID string `json:"source_id"`
	ID       int64  `json:"id,omitempty"`
	Inserted bool   `json:"inserted"`
	Conflict string `json:"conflict,omitempty"` // the id is taken by another payload; nothing was written
}

// Answer is the reply to a batch.
type Answer struct {
	OK       bool     `json:"ok"`
	Inserted int      `json:"inserted"`
	Repeated int      `json:"repeated"`
	Results  []Result `json:"results"`
	Settle   []string `json:"settle,omitempty"` // failures of the sources' follow-up work
}

// BadRequest is an event refused before anything was stored.
type BadRequest struct{ Msg string }

func (e *BadRequest) Error() string { return e.Msg }

// Accept stores the batch in one transaction, then lets the sources that changed settle. A repeat
// with another payload does not stop the batch: it is reported in its Result.
func Accept(ctx context.Context, store *db.Store, batch []Event, now time.Time) (Answer, error) {
	ans := Answer{OK: true, Results: make([]Result, 0, len(batch))}
	if len(batch) == 0 {
		return ans, &BadRequest{"нет событий"}
	}
	for i := range batch {
		e := &batch[i]
		if _, ok := lookup(e.Source); !ok {
			return ans, &BadRequest{fmt.Sprintf("неизвестный источник «%s»; известны: %s", e.Source, known())}
		}
		if e.SourceID == "" || e.Kind == "" {
			return ans, &BadRequest{fmt.Sprintf("событие %d: нужны source_id и kind", i)}
		}
		if len(e.Payload) == 0 || !json.Valid(e.Payload) {
			return ans, &BadRequest{fmt.Sprintf("событие %s: payload не JSON", e.SourceID)}
		}
		if e.OccurredAt != "" {
			t, err := ts.Parse(e.OccurredAt)
			if err != nil {
				return ans, &BadRequest{fmt.Sprintf("событие %s: occurred_at: %v", e.SourceID, err)}
			}
			e.OccurredAt = ts.Format(t)
		}
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return ans, err
	}
	defer tx.Rollback()
	changed := map[string]bool{}
	for _, e := range batch {
		id, inserted, err := ledger.AddEvent(ctx, tx, ledger.Event{Source: e.Source, SourceID: e.SourceID, Kind: e.Kind,
			OccurredAt: e.OccurredAt, Payload: e.Payload}, now)
		if errors.Is(err, ledger.ErrEventConflict) {
			ans.OK = false
			ans.Results = append(ans.Results, Result{SourceID: e.SourceID, ID: id, Conflict: err.Error()})
			continue
		}
		if err != nil {
			return ans, err
		}
		if inserted {
			ans.Inserted++
		} else {
			ans.Repeated++
		}
		ans.Results = append(ans.Results, Result{SourceID: e.SourceID, ID: id, Inserted: inserted})
		src, _ := lookup(e.Source)
		if src.Arrive == nil {
			continue
		}
		ch, err := src.Arrive(ctx, tx, Arrival{Event: e, ID: id, Inserted: inserted, Now: now})
		if err != nil {
			var bad *BadRequest
			if errors.As(err, &bad) {
				return ans, &BadRequest{fmt.Sprintf("событие %s: %s", e.SourceID, bad.Msg)}
			}
			return ans, fmt.Errorf("событие %s: %w", e.SourceID, err)
		}
		if ch {
			changed[e.Source] = true
		}
	}
	if err := tx.Commit(); err != nil {
		return ans, err
	}
	names := make([]string, 0, len(changed))
	for n := range changed {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		src, _ := lookup(n)
		if src.Settle == nil {
			continue
		}
		// The facts are in; the caller going away must not leave their consequences half done.
		if err := src.Settle(context.WithoutCancel(ctx), store, now); err != nil {
			ans.Settle = append(ans.Settle, n+": "+err.Error())
		}
	}
	return ans, nil
}

// request is {"events": […]} or one event as the whole body.
type request struct {
	Events []Event `json:"events"`
	Event
}

func handle(s *server.Server, w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		server.WriteError(w, http.StatusBadRequest, `тело запроса: нужен JSON {"events": [{"source", "source_id", "kind", "occurred_at", "payload"}]}`)
		return
	}
	batch := req.Events
	if batch == nil && req.Source != "" {
		batch = []Event{req.Event}
	}
	ans, err := Accept(r.Context(), s.Store, batch, ts.Now())
	var bad *BadRequest
	switch {
	case errors.As(err, &bad):
		server.WriteError(w, http.StatusBadRequest, bad.Msg)
		return
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		s.Log.Error("events", "key", auth.KeyName(r.Context()), "err", err)
		server.WriteError(w, http.StatusInternalServerError, "события не записаны: сбой сервиса, подробности в журнале")
		return
	}
	for _, msg := range ans.Settle {
		s.Log.Error("events settle", "key", auth.KeyName(r.Context()), "err", msg)
	}
	server.WriteJSON(w, http.StatusOK, ans)
}
