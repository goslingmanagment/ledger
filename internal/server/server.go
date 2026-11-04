// Package server is the HTTP API and the in-process scheduler of the ledger service.
//
// Features plug in from their own packages: Register adds routes, Every adds a periodic
// job. Both are called from init() and applied by New, so a new feature touches no
// shared file except the import list of internal/cli.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/version"
)

// Server carries what handlers and jobs need.
type Server struct {
	Store      *db.Store
	Log        *slog.Logger
	Mux        *http.ServeMux
	Deployment string
	started    time.Time
	jobs       []job
}

type job struct {
	name     string
	interval time.Duration
	run      func(context.Context, *Server) error
}

var (
	regMu     sync.Mutex
	routes    []func(*Server)
	schedules []job
)

// Register adds an installer of routes; it runs once per New.
func Register(install func(*Server)) {
	regMu.Lock()
	defer regMu.Unlock()
	routes = append(routes, install)
}

// Every adds a periodic job. The first run starts shortly after the service starts.
func Every(name string, interval time.Duration, run func(context.Context, *Server) error) {
	regMu.Lock()
	defer regMu.Unlock()
	schedules = append(schedules, job{name: name, interval: interval, run: run})
}

// New builds the server with every registered route.
func New(store *db.Store, log *slog.Logger, deployment string) *Server {
	s := &Server{Store: store, Log: log, Mux: http.NewServeMux(), Deployment: deployment, started: time.Now()}
	s.Mux.HandleFunc("GET /health", s.health)
	regMu.Lock()
	installers := append([]func(*Server){}, routes...)
	s.jobs = append(s.jobs, schedules...)
	regMu.Unlock()
	for _, install := range installers {
		install(s)
	}
	return s
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out := map[string]any{"ok": true, "service": "ledger", "build": version.Get(), "deployment": s.Deployment,
		"uptimeSeconds": int(time.Since(s.started).Seconds())}
	v, err := db.SchemaVersion(ctx, s.Store.RO)
	if err != nil {
		out["ok"], out["error"] = false, "database: "+err.Error()
		WriteJSON(w, http.StatusServiceUnavailable, out)
		return
	}
	out["schemaVersion"] = v
	WriteJSON(w, http.StatusOK, out)
}

// Run serves HTTP on addr and, when scheduler is true, runs the jobs until ctx ends.
func (s *Server) Run(ctx context.Context, addr string, scheduler bool) error {
	srv := &http.Server{Addr: addr, Handler: s.Mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	var wg sync.WaitGroup
	if scheduler {
		for i, j := range s.jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.loop(ctx, j, time.Duration(10+5*i)*time.Second)
			}()
		}
	}
	s.Log.Info("listening", "addr", addr, "build", version.Get(), "jobs", len(s.jobs), "scheduler", scheduler)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdown)
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func (s *Server) loop(ctx context.Context, j job, first time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = errors.New("panic: " + slog.AnyValue(p).String())
				}
			}()
			jctx, cancel := context.WithTimeout(ctx, j.interval)
			defer cancel()
			return j.run(jctx, s)
		}()
		if err != nil {
			s.Log.Error("job failed", "job", j.name, "err", err, "ms", time.Since(start).Milliseconds())
		} else {
			s.Log.Info("job done", "job", j.name, "ms", time.Since(start).Milliseconds())
		}
		t.Reset(j.interval)
	}
}

// WriteJSON writes v with the status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// WriteError writes {"ok":false,"error":msg}.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"ok": false, "error": msg})
}
