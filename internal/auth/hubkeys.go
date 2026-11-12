package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// An agent's hub key (ecosystem plan, M2): an agent may call ledger with its Agency Hub key instead of a ledger
// key. ledger asks the hub who the key is — it sends only the key's sha256, as the bearer of its OWN hub key
// holding introspect:agent_keys, never the agent's key — and gives the agent's code the rights of its own table
// hub_agents. Hub's exchange: docs/agent-doors.md of the hub, «Кто этот ключ».

// HubTokenPrefix starts every hub agent key; any other bearer is a ledger key, checked as before.
const HubTokenPrefix = "agency_hub_agent_"

// HubScopes are the only scopes a hub key can ever get, whatever hub_agents says: reading. decide and every scope
// that records or changes money stay with ledger's own keys.
var HubScopes = []string{ScopeRead, ScopePayouts}

const (
	// DefaultHubBaseURL is production Hub, as for the hub reader (LEDGER_HUB_BASE_URL overrides both).
	DefaultHubBaseURL = "https://hub.gosling-agency.ru"
	hubFresh          = 60 * time.Second // an answer is used as it is this long (the hub's cacheSeconds, at most)
	hubHold           = time.Hour        // an ACTIVE answer is held this long after it was fetched while the hub cannot be reached
	hubRetry          = 15 * time.Second // while holding, the hub is asked again at most this often
	hubTimeout        = 5 * time.Second  // one introspection; the agent waits for it
	hubMaxEntries     = 4096
	hubAsksPerMinute  = 120 // cache misses sent to the hub per minute, whoever asks: made-up keys must not spend its limit
)

// Errors of a hub key that is not answered: the hub cannot be reached and no active answer is held, or the hub
// refused ledger's own key or the question.
var (
	ErrHubUnreachable = errors.New("hub unreachable")
	ErrHubRefused     = errors.New("hub refused")
)

// HubAnswer is what ledger knows about a hub key now.
type HubAnswer struct {
	Active bool
	Code   string // the agent's code at the hub, when active
	Held   bool   // the hub did not answer: the last active answer is held
}

type hubEntry struct {
	active    bool
	code      string
	expiresAt time.Time
	fetched   time.Time
	fresh     time.Duration
	hold      time.Duration
	tried     time.Time // the last failed refresh while holding
}

// holdable: an active answer still inside its hold window and the key's own life.
func (e *hubEntry) holdable(now time.Time) bool {
	return e.active && now.Sub(e.fetched) < e.hold && now.Before(e.expiresAt)
}

func (e *hubEntry) answer(now time.Time, held bool) HubAnswer {
	if !e.active || !now.Before(e.expiresAt) {
		return HubAnswer{}
	}
	return HubAnswer{Active: true, Code: e.code, Held: held}
}

type hubCall struct {
	done chan struct{}
	ans  HubAnswer
	err  error
}

// HubKeys answers «who is this hub key» with a cache: 60 s per answer; while the hub cannot be reached (network,
// timeout, 5xx) the last ACTIVE answer is held up to an hour after it was fetched, an inactive one never; a 4xx or a
// malformed answer from the hub is an answer: refused, nothing held. Keys and digests are never logged; a digest
// lives only as the cache key.
type HubKeys struct {
	base, key string
	HTTP      *http.Client
	Now       func() time.Time

	mu      sync.Mutex
	entries map[string]*hubEntry
	calls   map[string]*hubCall
	asks    []time.Time
}

// NewHubKeys asks the hub at base with serviceKey, ledger's own hub key holding introspect:agent_keys.
func NewHubKeys(base, serviceKey string) *HubKeys {
	return &HubKeys{
		base: strings.TrimRight(base, "/"), key: serviceKey,
		HTTP: &http.Client{Timeout: hubTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		Now:     time.Now,
		entries: map[string]*hubEntry{}, calls: map[string]*hubCall{},
	}
}

// HubKeysFromEnv reads LEDGER_HUB_INTROSPECT_KEY and LEDGER_HUB_BASE_URL; without the key hub keys are not
// accepted (nil) and nothing else changes.
func HubKeysFromEnv() *HubKeys {
	key := strings.TrimSpace(os.Getenv("LEDGER_HUB_INTROSPECT_KEY"))
	if key == "" {
		return nil
	}
	base := strings.TrimSpace(os.Getenv("LEDGER_HUB_BASE_URL"))
	if base == "" {
		base = DefaultHubBaseURL
	}
	return NewHubKeys(base, key)
}

var hubKeys struct {
	sync.RWMutex
	h *HubKeys
}

// UseHubKeys makes the routes accept hub keys through h; nil — they are refused (the default).
func UseHubKeys(h *HubKeys) {
	hubKeys.Lock()
	hubKeys.h = h
	hubKeys.Unlock()
}

func currentHubKeys() *HubKeys {
	hubKeys.RLock()
	defer hubKeys.RUnlock()
	return hubKeys.h
}

// Who tells who the hub key token is.
func (h *HubKeys) Who(ctx context.Context, token string) (HubAnswer, error) {
	digest := Hash(token)
	now := h.Now()
	h.mu.Lock()
	e := h.entries[digest]
	if e != nil && now.Sub(e.fetched) < e.fresh {
		h.mu.Unlock()
		return e.answer(now, false), nil
	}
	if e != nil && e.holdable(now) && !e.tried.IsZero() && now.Sub(e.tried) < hubRetry {
		h.mu.Unlock()
		return e.answer(now, true), nil
	}
	if c := h.calls[digest]; c != nil {
		h.mu.Unlock()
		select {
		case <-c.done:
			return c.ans, c.err
		case <-ctx.Done():
			return HubAnswer{}, ctx.Err()
		}
	}
	if !h.mayAsk(now) {
		defer h.mu.Unlock()
		if e != nil && e.holdable(now) {
			return e.answer(now, true), nil
		}
		return HubAnswer{}, fmt.Errorf("%w: too many hub keys to check this minute", ErrHubUnreachable)
	}
	c := &hubCall{done: make(chan struct{})}
	h.calls[digest] = c
	h.mu.Unlock()

	c.ans, c.err = h.refresh(digest)
	close(c.done)
	return c.ans, c.err
}

// mayAsk counts a question to the hub; the caller holds mu.
func (h *HubKeys) mayAsk(now time.Time) bool {
	kept := h.asks[:0]
	for _, at := range h.asks {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	h.asks = kept
	if len(h.asks) >= hubAsksPerMinute {
		return false
	}
	h.asks = append(h.asks, now)
	return true
}

// refresh asks the hub about digest and settles the cache. The call is detached from the request: an agent that
// hangs up does not cancel an answer other requests wait for.
func (h *HubKeys) refresh(digest string) (HubAnswer, error) {
	ans, err := h.introspect(context.Background(), digest)
	now := h.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.calls, digest)
	switch {
	case err == nil:
		h.store(digest, ans, now)
		return ans.answer(now, false), nil
	case errors.Is(err, ErrHubRefused):
		delete(h.entries, digest) // a refusal is an answer: nothing is held after it
		return HubAnswer{}, err
	default:
		if e := h.entries[digest]; e != nil && e.holdable(now) {
			e.tried = now
			return e.answer(now, true), nil
		}
		delete(h.entries, digest)
		return HubAnswer{}, err
	}
}

// store keeps e under digest, dropping what can no longer be used when the cache is full; the caller holds mu.
func (h *HubKeys) store(digest string, e *hubEntry, now time.Time) {
	if _, ok := h.entries[digest]; !ok && len(h.entries) >= hubMaxEntries {
		for d, old := range h.entries {
			if now.Sub(old.fetched) >= old.fresh && !old.holdable(now) {
				delete(h.entries, d)
			}
		}
		for d := range h.entries {
			if len(h.entries) < hubMaxEntries {
				break
			}
			delete(h.entries, d)
		}
	}
	h.entries[digest] = e
}

type introspectAnswer struct {
	Active *bool `json:"active"`
	Agent  *struct {
		Code string `json:"code"`
	} `json:"agent"`
	ExpiresAt    string `json:"expiresAt"`
	CacheSeconds int    `json:"cacheSeconds"`
	StaleSeconds int    `json:"staleSeconds"`
}

func clampSeconds(s int, limit time.Duration) time.Duration {
	d := time.Duration(s) * time.Second
	if d <= 0 || d > limit {
		return limit
	}
	return d
}

// introspect is one POST /api/v1/agent/keys/introspect: ErrHubRefused for a 4xx or a malformed answer,
// ErrHubUnreachable for no answer (network, timeout, a redirect, 5xx).
func (h *HubKeys) introspect(ctx context.Context, digest string) (*hubEntry, error) {
	body, _ := json.Marshal(map[string]string{"keyDigest": digest})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/api/v1/agent/keys/introspect", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHubRefused, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	fetched := h.Now()
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: no answer", ErrHubUnreachable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, fmt.Errorf("%w: HTTP %d", ErrHubRefused, resp.StatusCode)
	case resp.StatusCode != http.StatusOK || err != nil:
		return nil, fmt.Errorf("%w: HTTP %d", ErrHubUnreachable, resp.StatusCode)
	}
	var a introspectAnswer
	if err := json.Unmarshal(raw, &a); err != nil || a.Active == nil {
		return nil, fmt.Errorf("%w: not an answer about the key", ErrHubRefused)
	}
	e := &hubEntry{fetched: fetched, fresh: clampSeconds(a.CacheSeconds, hubFresh), hold: clampSeconds(a.StaleSeconds, hubHold)}
	if !*a.Active {
		return e, nil
	}
	expires, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
	if a.Agent == nil || !hubCodeRe.MatchString(a.Agent.Code) || err != nil {
		return nil, fmt.Errorf("%w: an active answer without the agent's code or expiry", ErrHubRefused)
	}
	e.active, e.code, e.expiresAt = true, a.Agent.Code, expires
	return e, nil
}
