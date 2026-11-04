package checks

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// urgentEvery repeats a failure that holds.
const urgentEvery = 6 * time.Hour

func envOf(s *server.Server, now time.Time) Env {
	dir := filepath.Dir(config.LoadServer().DB)
	if _, err := os.Stat(filepath.Join(dir, "replica")); err != nil {
		dir = ""
	}
	return Env{Q: s.Store.RO, Now: now, DataDir: dir}
}

func init() {
	server.Every("checks", time.Hour, func(ctx context.Context, s *server.Server) error {
		start := time.Now()
		rs := Run(ctx, envOf(s, start))
		return Save(ctx, s.Store.RW, rs, start, time.Now())
	})
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/health/checks", auth.Require(s, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var rs []Result
			var at string
			var err error
			if r.URL.Query().Get("now") == "1" {
				rs, at = Run(r.Context(), envOf(s, time.Now())), ts.Format(time.Now())
			} else if rs, at, err = Latest(r.Context(), s.Store.RO); err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			} else {
				rs = WithStale(rs, at, time.Now())
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "at": at, "checks": rs})
		})))
		s.Mux.Handle("POST /v1/health/urgent", auth.Require(s, "decide", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rs, at, err := Latest(r.Context(), s.Store.RO)
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out, err := Urgent(r.Context(), s.Store.RW, rs, time.Now(), urgentEvery)
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if out == nil {
				out = []Result{}
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "at": at, "alerts": out})
		})))
	})
}
