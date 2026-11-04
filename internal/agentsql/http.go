package agentsql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	server.Register(func(s *server.Server) { Mount(s, Open(s.Store.Path)) })
}

// SQLRequest is the body of POST /v1/sql.
type SQLRequest struct {
	SQL string `json:"sql"`
}

// SQLResponse is the answer of POST /v1/sql.
type SQLResponse struct {
	OK         bool     `json:"ok"`
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	Truncated  bool     `json:"truncated"`
	Note       string   `json:"note,omitempty"`
	DurationMs int64    `json:"durationMs"`
}

// SchemaResponse is the answer of GET /v1/schema.
type SchemaResponse struct {
	OK     bool    `json:"ok"`
	Tables []Table `json:"tables"`
}

// Mount adds POST /v1/sql and GET /v1/schema for keys with the read scope.
func Mount(s *server.Server, eng *Engine) {
	h := &handler{s: s, eng: eng}
	s.Mux.Handle("POST /v1/sql", auth.Require(s, auth.ScopeRead, http.HandlerFunc(h.sql)))
	s.Mux.Handle("GET /v1/schema", auth.Require(s, auth.ScopeRead, http.HandlerFunc(h.schema)))
}

type handler struct {
	s   *server.Server
	eng *Engine
}

var statusOf = map[Kind]int{Invalid: http.StatusBadRequest, Denied: http.StatusForbidden,
	Timeout: http.StatusBadRequest, Failed: http.StatusBadRequest}

func (h *handler) sql(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req SQLRequest
	var res *Result
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*MaxSQL)).Decode(&req)
	if err != nil {
		err = &Error{Kind: Invalid, Msg: `тело запроса: нужен JSON {"sql": "SELECT …"}`}
	} else {
		res, err = h.eng.Run(r.Context(), req.SQL)
	}
	elapsed := time.Since(start)
	h.log(r.Context(), start, req.SQL, elapsed, res, err)
	if err != nil {
		var qe *Error
		switch {
		case errors.As(err, &qe):
			server.WriteError(w, statusOf[qe.Kind], qe.Msg)
		case errors.Is(err, context.Canceled): // the caller has gone
		default:
			h.s.Log.Error("agent sql", "key", auth.KeyName(r.Context()), "err", err)
			server.WriteError(w, http.StatusInternalServerError, "запрос не выполнен: сбой сервиса, подробности в журнале")
		}
		return
	}
	out := SQLResponse{OK: true, Columns: res.Columns, Rows: res.Rows, Truncated: res.Truncated,
		DurationMs: elapsed.Milliseconds()}
	if res.Truncated {
		out.Note = fmt.Sprintf("показаны первые %d строк, остальные отброшены: сузьте запрос (WHERE, LIMIT, GROUP BY)", len(res.Rows))
	}
	server.WriteJSON(w, http.StatusOK, out)
}

// log writes the call into query_log, successful or not. The request may be gone by now, the
// row is still written.
func (h *handler) log(ctx context.Context, at time.Time, query string, d time.Duration, res *Result, err error) {
	var rows int
	var msg any // NULL: success
	switch {
	case err == nil:
		rows = len(res.Rows)
	case errors.Is(err, context.Canceled):
		msg = "отменён: клиент закрыл соединение"
	default:
		msg = err.Error()
	}
	if len(query) > MaxSQL {
		query = strings.ToValidUTF8(query[:MaxSQL], "")
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := h.s.Store.RW.ExecContext(lctx, `INSERT INTO query_log(key_name, at, sql, duration_ms, rows, error)
		VALUES (?, ?, ?, ?, ?, ?)`, auth.KeyName(ctx), ts.Format(at), query, d.Milliseconds(), rows, msg); err != nil {
		h.s.Log.Error("query_log", "key", auth.KeyName(ctx), "err", err)
	}
}

func (h *handler) schema(w http.ResponseWriter, r *http.Request) {
	tables, err := ReadSchema(r.Context(), h.s.Store.RO)
	if err != nil {
		h.s.Log.Error("schema", "err", err)
		server.WriteError(w, http.StatusInternalServerError, "схема не прочитана: "+err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, SchemaResponse{OK: true, Tables: tables})
}
