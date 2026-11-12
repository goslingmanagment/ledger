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
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ts"
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
	book       *jobBook // the last outcome of each job, in memory: a restart forgets it
}

// jobBook keeps the last outcome of each job by its index in jobs.
type jobBook struct {
	mu        sync.Mutex
	scheduler bool // Run started the jobs
	states    []jobState
}

type jobState struct {
	started, done, failed time.Time
	lastErr               string
	inRow                 int
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
	s.book = &jobBook{states: make([]jobState, len(s.jobs))}
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
	if s.book == nil {
		s.book = &jobBook{states: make([]jobState, len(s.jobs))}
	}
	s.book.mu.Lock()
	s.book.scheduler = scheduler
	s.book.mu.Unlock()
	srv := &http.Server{Addr: addr, Handler: s.Mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	var wg sync.WaitGroup
	if scheduler {
		for i, j := range s.jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.loop(ctx, i, j, time.Duration(10+5*i)*time.Second)
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

func (s *Server) loop(ctx context.Context, i int, j job, first time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		s.book.begin(i, start)
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
		s.book.end(i, time.Now(), err)
		if err != nil {
			s.Log.Error("job failed", "job", j.name, "err", err, "ms", time.Since(start).Milliseconds())
		} else {
			s.Log.Info("job done", "job", j.name, "ms", time.Since(start).Milliseconds())
		}
		t.Reset(j.interval)
	}
}

func (b *jobBook) begin(i int, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states[i].started = at
}

func (b *jobBook) end(i int, at time.Time, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := &b.states[i]
	if err == nil {
		st.done, st.inRow = at, 0
		return
	}
	st.failed, st.lastErr = at, jobError(err.Error())
	st.inRow++
}

// maxJobError is how much of a job's error the jobs answer keeps.
const maxJobError = 200

// secretRe finds what may be a secret in an error text: a ledger key, a bearer token, a key, token, secret or signature
// parameter.
var secretRe = regexp.MustCompile(`(?i)(ldg_)[a-z0-9_-]+|(bearer\s+)\S+|((?:api[_-]?key|key|token|secret|sign|signature|password|passwd)=)[^&\s"']+`)

// jobError is an error text as the jobs answer gives it: secrets masked, at most maxJobError characters.
func jobError(text string) string {
	text = secretRe.ReplaceAllString(text, "${1}${2}${3}…")
	if utf8.RuneCountInString(text) > maxJobError {
		r := []rune(text)
		text = string(r[:maxJobError-1]) + "…"
	}
	return text
}

// JobStatus is the last outcome of one scheduled job in this process.
type JobStatus struct {
	Job             string  `json:"job"`
	IntervalSeconds int64   `json:"intervalSeconds"`
	LastStartedAt   *string `json:"lastStartedAt"` // null — not run since the process started
	LastDoneAt      *string `json:"lastDoneAt"`
	LastFailedAt    *string `json:"lastFailedAt"`
	LastError       *string `json:"lastError"`   // of the last failure, secrets masked, at most 200 characters
	FailedInRow     int     `json:"failedInRow"` // failures since the last success
}

// JobsAnswer is every job the scheduler registers with its last outcome since the process started.
type JobsAnswer struct {
	OK        bool        `json:"ok"`
	At        string      `json:"at"`
	StartedAt string      `json:"startedAt"` // the process start: no outcome is older
	Scheduler bool        `json:"scheduler"` // false — the jobs do not run in this process (LEDGER_SCHEDULER=off)
	Jobs      []JobStatus `json:"jobs"`
}

// Jobs reads the last outcome of every job, in the order they were registered.
func (s *Server) Jobs(now time.Time) JobsAnswer {
	ans := JobsAnswer{OK: true, At: ts.Format(now), StartedAt: ts.Format(s.started), Jobs: []JobStatus{}}
	var states []jobState
	if s.book != nil {
		s.book.mu.Lock()
		ans.Scheduler = s.book.scheduler
		states = append(states, s.book.states...)
		s.book.mu.Unlock()
	}
	at := func(t time.Time) *string {
		if t.IsZero() {
			return nil
		}
		v := ts.Format(t)
		return &v
	}
	for i, j := range s.jobs {
		js := JobStatus{Job: j.name, IntervalSeconds: int64(j.interval / time.Second)}
		if i < len(states) {
			st := states[i]
			js.LastStartedAt, js.LastDoneAt, js.LastFailedAt, js.FailedInRow = at(st.started), at(st.done), at(st.failed), st.inRow
			if st.lastErr != "" {
				e := st.lastErr
				js.LastError = &e
			}
		}
		ans.Jobs = append(ans.Jobs, js)
	}
	return ans
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
