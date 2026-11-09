// Package hub reads Agency Hub through its Agent Read Plane by the hub's honesty rules and keeps
// mirrors of what the hub proved: transactions, Fansly payouts, OnlyFans payout requests and page
// balances. It ports the Hub class of /OS scripts/month.py: a response with unknown blockers, gaps,
// source errors or an unapplied filter is a refusal, a transaction list must equal the hub's own
// summary of the same window, a payout list must be proven complete. A refused read leaves the page
// «неизвестно», never zero.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/config"
)

// DefaultBaseURL is production Hub.
const DefaultBaseURL = "https://hub.gosling-agency.ru"

// Config is how to reach the hub. The key is a secret: it never goes to logs or errors.
type Config struct {
	BaseURL string
	Key     string
}

// ConfigFromEnv reads LEDGER_HUB_AGENT_KEY and LEDGER_HUB_BASE_URL, falling back for the key to
// ~/.config/ledger/credentials (chmod 600) on agents' machines.
func ConfigFromEnv() (Config, error) {
	c := Config{BaseURL: strings.TrimRight(os.Getenv("LEDGER_HUB_BASE_URL"), "/"), Key: os.Getenv("LEDGER_HUB_AGENT_KEY")}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Key != "" {
		return c, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".config", "ledger", "credentials")
		vals, err := config.ReadKV(path)
		if err != nil && !os.IsNotExist(err) {
			return c, err
		}
		c.Key = vals["LEDGER_HUB_AGENT_KEY"]
	}
	if c.Key == "" {
		return c, errors.New("нет LEDGER_HUB_AGENT_KEY (env или ~/.config/ledger/credentials): хаб не читается")
	}
	return c, nil
}

// Refusal is a hub answer the reading rules do not accept as data, or no answer at all. The page's
// money stays «неизвестно». Status is the HTTP status of a refused call (0 — not an HTTP refusal).
type Refusal struct {
	Msg    string
	Status int
}

func (r *Refusal) Error() string { return r.Msg }

func refusef(format string, args ...any) error { return &Refusal{Msg: fmt.Sprintf(format, args...)} }

// IsRefusal tells a refusal from an operational failure (database, cancelled context).
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// Client calls the hub strictly one request at a time.
type Client struct {
	base     string
	key      string
	HTTP     *http.Client
	Sleep    func(context.Context, time.Duration) error // retry pauses; tests replace it
	Throttle *Throttle
	Calls    int // requests sent, retries included
}

// NewClient builds a client; th limits dataset requests per rolling minute.
func NewClient(cfg Config, th *Throttle) *Client {
	return &Client{base: strings.TrimRight(cfg.BaseURL, "/"), key: cfg.Key, HTTP: &http.Client{Timeout: 180 * time.Second},
		Sleep: sleepCtx, Throttle: th}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// callError is a failed call: the HTTP status and the hub's error code, or a network failure.
type callError struct {
	status  int    // 0 — no HTTP answer
	code    string // the hub's `error`, network_error, timeout
	message string
}

func (e *callError) String() string {
	st, code, msg := "None", e.code, e.message
	if e.status != 0 {
		st = fmt.Sprint(e.status)
	}
	if code == "" {
		code = "None"
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return st + " " + code + " " + msg
}

// retryable: 429, 502, 503, 504 and network errors, never a spent budget or a disabled plane.
func (e *callError) retryable() bool {
	if e.code == "agent_budget_exhausted" || e.code == "agent_plane_disabled" {
		return false
	}
	return e.status == 429 || e.status == 502 || e.status == 503 || e.status == 504 || e.code == "network_error"
}

func (c *Client) call(ctx context.Context, method, path string, body any) ([]byte, *callError) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, &callError{code: "unexpected", message: err.Error()}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, &callError{code: "unexpected", message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ne net.Error
		code := "network_error"
		if errors.As(err, &ne) && ne.Timeout() {
			code = "timeout"
		}
		if ctx.Err() != nil {
			code = "aborted"
		}
		// The URL is not secret but carries nothing useful either; the key is never in it.
		msg := err.Error()
		var ue *url.Error
		if errors.As(err, &ue) {
			msg = ue.Err.Error()
		}
		return nil, &callError{code: code, message: "Network error calling " + method + " " + path + ": " + msg}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &callError{status: resp.StatusCode, code: "network_error", message: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &callError{status: resp.StatusCode, message: fmt.Sprintf("%s %s failed with %d", method, path, resp.StatusCode)}
		var env struct {
			Error   any `json:"error"`
			Message any `json:"message"`
		}
		if json.Unmarshal(raw, &env) == nil {
			if s, ok := env.Error.(string); ok {
				e.code = s
			}
			if s, ok := env.Message.(string); ok {
				e.message = s
			}
		}
		return nil, e
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &callError{status: resp.StatusCode, code: "undeclared_status",
			message: fmt.Sprintf("%s %s returned undeclared status %d", method, path, resp.StatusCode)}
	}
	return raw, nil
}

// do sends one operation with month.py's retry discipline: up to 4 attempts, 30 s × attempt apart.
func (c *Client) do(ctx context.Context, what string, dataset bool, method, path string, body any) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if dataset && c.Throttle != nil {
			if err := c.Throttle.Wait(ctx); err != nil {
				return nil, err
			}
		}
		raw, cerr := c.call(ctx, method, path, body)
		c.Calls++
		if cerr == nil {
			return raw, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt < 3 && cerr.retryable() {
			if err := c.Sleep(ctx, time.Duration(30*(attempt+1))*time.Second); err != nil {
				return nil, err
			}
			continue
		}
		return nil, &Refusal{Msg: fmt.Sprintf("хаб %s: %s", what, cerr), Status: cerr.status}
	}
}

// ---- the envelope ----

type captureFloor struct {
	At   *string `json:"at"`
	Kind string  `json:"kind"`
}

type plane struct {
	Plane        string        `json:"plane"`
	State        string        `json:"state"`
	CaptureFloor *captureFloor `json:"captureFloor"`
	Reason       string        `json:"reason"`
}

type gap struct {
	Kind  string  `json:"kind"`
	From  *string `json:"from"`
	To    *string `json:"to"`
	Plane string  `json:"plane"`
}

type item struct {
	DatasetRef string                     `json:"datasetRef"`
	Key        string                     `json:"key"`
	OccurredAt *string                    `json:"occurredAt"`
	Fields     map[string]json.RawMessage `json:"fields"`
}

type summaryGroup struct {
	Currency         *string `json:"currency"`
	TransactionCount *int64  `json:"transactionCount"`
	NetMills         *int64  `json:"netMills"`
}

type datasetResponse struct {
	DatasetRef string `json:"datasetRef"`
	PageLabel  string `json:"pageLabel"`
	Items      []item `json:"items"`
	Summary    *struct {
		Basis  string         `json:"basis"`
		Groups []summaryGroup `json:"groups"`
	} `json:"summary"`
	Predicates []struct {
		Name      string `json:"name"`
		Requested *bool  `json:"requested"`
		Applied   *bool  `json:"applied"`
		Reason    string `json:"reason"`
	} `json:"predicates"`
	Delivery *struct {
		Returned       *int `json:"returned"`
		MatchedInScope *struct {
			Value *int  `json:"value"`
			Exact *bool `json:"exact"`
		} `json:"matchedInScope"`
		CappedBy          *string `json:"cappedBy"`
		NextCursor        *string `json:"nextCursor"`
		SnapshotExhausted *bool   `json:"snapshotExhausted"`
	} `json:"delivery"`
	Capture *struct {
		Planes       []plane           `json:"planes"`
		Gaps         []gap             `json:"gaps"`
		SourceErrors []json.RawMessage `json:"sourceErrors"`
	} `json:"capture"`
	Conclusion *struct {
		Blockers []string `json:"blockers"`
	} `json:"conclusion"`
}

// contract checks the parts of the response contract the reading rules stand on.
func (r *datasetResponse) contract(dataset string) error {
	switch {
	case r.Delivery == nil || r.Delivery.Returned == nil || r.Delivery.MatchedInScope == nil ||
		r.Delivery.MatchedInScope.Value == nil || r.Delivery.MatchedInScope.Exact == nil || r.Delivery.SnapshotExhausted == nil:
		return errors.New("нет delivery")
	case r.Capture == nil || r.Capture.Planes == nil || r.Capture.Gaps == nil || r.Capture.SourceErrors == nil:
		return errors.New("нет capture")
	case r.Conclusion == nil || r.Conclusion.Blockers == nil:
		return errors.New("нет conclusion.blockers")
	case r.Items == nil || r.Predicates == nil:
		return errors.New("нет items или predicates")
	case r.DatasetRef != dataset:
		return fmt.Errorf("datasetRef %q", r.DatasetRef)
	}
	n := len(r.Items)
	if r.Summary != nil {
		if r.Summary.Groups == nil {
			return errors.New("нет summary.groups")
		}
		if len(r.Items) > 0 {
			return errors.New("summary вместе со строками")
		}
		n = len(r.Summary.Groups)
		for _, g := range r.Summary.Groups {
			if g.TransactionCount == nil || g.NetMills == nil {
				return errors.New("группа summary без transactionCount или netMills")
			}
		}
	}
	d := r.Delivery
	switch {
	case *d.Returned != n:
		return fmt.Errorf("returned %d при %d записях", *d.Returned, n)
	case *d.SnapshotExhausted && d.NextCursor != nil:
		return errors.New("исчерпанный снимок с курсором")
	case d.NextCursor != nil && d.CappedBy == nil:
		return errors.New("курсор без cappedBy")
	}
	for _, p := range r.Predicates {
		if p.Requested == nil || p.Applied == nil {
			return errors.New("предикат без requested/applied")
		}
	}
	for i, it := range r.Items {
		if it.DatasetRef != dataset || it.Key == "" || it.Fields == nil {
			return fmt.Errorf("строка %d без datasetRef/key/fields", i)
		}
	}
	return nil
}

// floorOf is the capture floor of one plane, when the plane was read and has one.
func (r *datasetResponse) floorOf(name string) (time.Time, bool) {
	for _, p := range r.Capture.Planes {
		if p.Plane == name && p.State == "read" && p.CaptureFloor != nil && p.CaptureFloor.At != nil {
			if t, err := parseHubTime(*p.CaptureFloor.At); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

type filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

type sortKey struct {
	Field string `json:"field"`
	Dir   string `json:"dir"`
}

// query is the body of agentDatasetQuery, built the way `hub dataset` builds it: only the flags
// given, and a cursor alone on a continuation.
type query struct {
	From, To string
	Summary  bool
	Filters  []filter
	Sort     []sortKey
	Limit    int
	Cursor   string
	Claim    []string
}

func (q query) body() map[string]any {
	b := map[string]any{}
	if q.From != "" {
		b["from"] = q.From
	}
	if q.To != "" {
		b["to"] = q.To
	}
	if q.Summary {
		b["summary"] = true
	}
	if q.Filters != nil {
		b["filters"] = q.Filters
	}
	if q.Sort != nil {
		b["sort"] = q.Sort
	}
	if q.Limit > 0 {
		b["limit"] = q.Limit
	}
	if q.Cursor != "" {
		b["cursor"] = q.Cursor
	}
	if len(q.Claim) > 0 {
		b["claim"] = map[string]any{"fields": q.Claim, "targets": "all_in_scope"}
	}
	return b
}

// dataset runs one query and applies the rules every answer is held to: the contract, every
// requested predicate applied, no source errors. It returns the conclusion's blockers.
func (c *Client) dataset(ctx context.Context, what, page, ds string, q query) (*datasetResponse, map[string]bool, error) {
	path := "/api/v1/agent/pages/" + url.PathEscape(page) + "/datasets/" + url.PathEscape(ds) + "/query"
	raw, err := c.do(ctx, what, true, http.MethodPost, path, q.body())
	if err != nil {
		return nil, nil, err
	}
	var r datasetResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	if err := r.contract(ds); err != nil {
		return nil, nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	for _, p := range r.Predicates {
		if *p.Requested && !*p.Applied {
			b, _ := json.Marshal(r.Predicates)
			return nil, nil, refusef("хаб %s: фильтр не применён %s", what, b)
		}
	}
	if len(r.Capture.SourceErrors) > 0 {
		b, _ := json.Marshal(r.Capture.SourceErrors)
		return nil, nil, refusef("хаб %s: sourceErrors %s", what, b)
	}
	bl := map[string]bool{}
	for _, b := range r.Conclusion.Blockers {
		bl[b] = true
	}
	return &r, bl, nil
}

// GrantPage is one page of the key's grant.
type GrantPage struct {
	PageLabel string `json:"pageLabel"`
	Platform  string `json:"platform"`
	ModelSlug string `json:"modelSlug"`
	ModelName string `json:"modelName"`
}

// Capabilities reads the key's grant (not throttled: it is not a dataset request).
func (c *Client) Capabilities(ctx context.Context) ([]GrantPage, error) {
	raw, err := c.do(ctx, "capabilities", false, http.MethodGet, "/api/v1/agent/capabilities", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Grant *struct {
			Pages []GrantPage `json:"pages"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Grant == nil || r.Grant.Pages == nil {
		return nil, refusef("хаб capabilities: ответ не по контракту (%v)", err)
	}
	for _, p := range r.Grant.Pages {
		if p.PageLabel == "" || p.Platform == "" {
			return nil, refusef("хаб capabilities: ответ не по контракту (страница без pageLabel/platform)")
		}
	}
	return r.Grant.Pages, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseHubTime reads an RFC 3339 instant of the hub ("2026-09-30T21:33:14.000Z").
func parseHubTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// Refusef makes a refusal for callers outside this package: the data is not proven, the answer is «неизвестно».
func Refusef(format string, args ...any) error { return &Refusal{Msg: fmt.Sprintf(format, args...)} }
