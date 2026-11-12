package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const serviceKey = "agency_hub_agent_service-key-of-ledger"

// fakeHub answers introspection by the digests it knows; down makes it answer 503, refuse 403.
type fakeHub struct {
	mu      sync.Mutex
	t       *testing.T
	active  map[string]string // digest → agent code
	expires time.Time
	down    bool
	refuse  bool
	asked   int
	srv     *httptest.Server
}

func newFakeHub(t *testing.T) *fakeHub {
	f := &fakeHub{t: t, active: map[string]string{}, expires: time.Now().Add(24 * time.Hour)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asked++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agent/keys/introspect" {
			t.Errorf("hub got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+serviceKey {
			t.Errorf("hub got another bearer than ledger's own key")
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		if err := json.Unmarshal(raw, &body); err != nil || len(body) != 1 || len(body["keyDigest"]) != 64 {
			t.Errorf("hub got %s: only the digest may be sent", raw)
		}
		if strings.Contains(string(raw), HubTokenPrefix) {
			t.Error("the agent's key reached the hub")
		}
		switch {
		case f.down:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case f.refuse:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":"agent_capability_missing"}`)
			return
		}
		code, ok := f.active[body["keyDigest"]]
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			io.WriteString(w, `{"active":false,"cacheSeconds":60,"staleSeconds":3600}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"active": true, "agent": map[string]string{"code": code, "displayName": "Agent"},
			"expiresAt": f.expires.UTC().Format(time.RFC3339Nano), "cacheSeconds": 60, "staleSeconds": 3600})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) set(fn func(f *fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeHub) questions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

type hubStand struct {
	st    *db.Store
	srv   *httptest.Server
	hub   *fakeHub
	clock time.Time
}

func (s *hubStand) advance(d time.Duration) { s.clock = s.clock.Add(d) }

// setupHub serves routes of every kind of scope; a hub key is checked at a fake hub with a clock the test moves.
func setupHub(t *testing.T) *hubStand {
	t.Helper()
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := &server.Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Mux: http.NewServeMux()}
	who := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, KeyName(r.Context())) })
	s.Mux.Handle("GET /read", Require(s, ScopeRead, who))
	s.Mux.Handle("GET /payouts", Require(s, ScopePayouts, who))
	s.Mux.Handle("GET /either", RequireAny(s, []string{ScopePayouts, ScopeRead}, who))
	for _, sc := range []string{ScopeDecide, ScopePropose, ScopeReports, ScopeCollect, ScopeImport} {
		s.Mux.Handle("POST /"+sc, Require(s, sc, who))
	}
	stand := &hubStand{st: st, hub: newFakeHub(t), clock: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	hk := NewHubKeys(stand.hub.srv.URL, serviceKey)
	hk.Now = func() time.Time { return stand.clock }
	UseHubKeys(hk)
	t.Cleanup(func() { UseHubKeys(nil) })
	stand.srv = httptest.NewServer(s.Mux)
	t.Cleanup(stand.srv.Close)
	return stand
}

func call(t *testing.T, srv *httptest.Server, method, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type auditRow struct {
	code, method, path string
	status             int
	held               bool
}

func audited(t *testing.T, st *db.Store) []auditRow {
	t.Helper()
	rows, err := st.RO.Query(`SELECT agent_code, method, path, status, held FROM hub_key_calls ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		rows.Scan(&r.code, &r.method, &r.path, &r.status, &r.held)
		out = append(out, r)
	}
	return out
}

func TestHubKeyGetsOnlyTheRightsOfItsRow(t *testing.T) {
	s := setupHub(t)
	ctx := context.Background()
	reader, payer, stranger, off := HubTokenPrefix+"reader", HubTokenPrefix+"payer", HubTokenPrefix+"stranger", HubTokenPrefix+"off"
	s.hub.set(func(f *fakeHub) {
		f.active[Hash(reader)], f.active[Hash(payer)], f.active[Hash(stranger)], f.active[Hash(off)] = "gengar", "hub-server", "unknown-agent", "gone-agent"
	})
	for code, scopes := range map[string][]string{"gengar": {ScopeRead}, "hub-server": {ScopePayouts}, "gone-agent": {ScopeRead, ScopePayouts}} {
		if _, err := SetHubAgent(ctx, s.st.RW, code, scopes, "", ts.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := DisableHubAgent(ctx, s.st.RW, "gone-agent", ts.Now()); err != nil {
		t.Fatal(err)
	}
	local, _, err := Create(ctx, s.st.RW, "local", []string{ScopeRead, ScopeDecide}, false, ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path, token string
		code                int
		body                string
	}{
		{"GET", "/read", reader, 200, "hub:gengar"},
		{"GET", "/either", reader, 200, "hub:gengar"},
		{"GET", "/payouts", reader, 403, "нет права payouts"},
		{"GET", "/payouts", payer, 200, "hub:hub-server"},
		{"GET", "/read", payer, 403, "нет права read"},
		{"POST", "/decide", reader, 403, "нет права decide"},
		{"POST", "/propose", reader, 403, "нет права"},
		{"POST", "/reports", payer, 403, "нет права"},
		{"POST", "/collect", reader, 403, "нет права"},
		{"POST", "/import", reader, 403, "нет права"},
		// A code the table does not hold, or holds switched off, gets nothing: never a default.
		{"GET", "/read", stranger, 403, "не открыт"},
		{"GET", "/read", off, 403, "не открыт"},
		// A key the hub does not know is no key.
		{"GET", "/read", HubTokenPrefix + "made-up", 401, "не действует"},
		// Local keys work exactly as before.
		{"POST", "/decide", local, 200, "local"},
	} {
		if code, body := call(t, s.srv, c.method, c.path, c.token); code != c.code || !strings.Contains(body, c.body) {
			t.Errorf("%s %s %s: %d %s, want %d %s", c.method, c.path, c.token[len(HubTokenPrefix):], code, body, c.code, c.body)
		}
	}
	got := audited(t, s.st)
	want := []auditRow{
		{"gengar", "GET", "/read", 200, false}, {"gengar", "GET", "/either", 200, false}, {"gengar", "GET", "/payouts", 403, false},
		{"hub-server", "GET", "/payouts", 200, false}, {"hub-server", "GET", "/read", 403, false},
		{"gengar", "POST", "/decide", 403, false}, {"gengar", "POST", "/propose", 403, false}, {"hub-server", "POST", "/reports", 403, false},
		{"gengar", "POST", "/collect", 403, false}, {"gengar", "POST", "/import", 403, false},
		{"unknown-agent", "GET", "/read", 403, false}, {"gone-agent", "GET", "/read", 403, false},
	}
	if len(got) != len(want) {
		t.Fatalf("audit: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	var leaked int
	s.st.RO.QueryRow(`SELECT count(*) FROM hub_key_calls WHERE path LIKE '%' || ? || '%' OR path LIKE '%' || ? || '%'`,
		HubTokenPrefix, Hash(reader)).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("the audit holds a key or a digest")
	}
}

func TestHubKeyScopesAreReadingOnly(t *testing.T) {
	s := setupHub(t)
	ctx := context.Background()
	for _, scopes := range [][]string{{ScopeDecide}, {ScopeRead, ScopePropose}, {ScopeReports}, {ScopeCollect}, {ScopeImport}} {
		if _, err := SetHubAgent(ctx, s.st.RW, "gengar", scopes, "", ts.Now()); err == nil {
			t.Errorf("scopes %v accepted for a hub key", scopes)
		}
	}
	for _, bad := range []string{"decide", "read,decide", "reports", "propose", "collect", "import", ""} {
		if _, err := ParseHubScopes(bad); err == nil {
			t.Errorf("ParseHubScopes(%q) accepted", bad)
		}
	}
	// Even a row written past the code cannot hold a writing scope, and one that did would not count.
	if _, err := s.st.RW.Exec(`INSERT INTO hub_agents(code, scopes, created_at, updated_at) VALUES ('gengar', 'read,decide', 'x', 'x')`); err == nil {
		t.Fatal("the table took decide")
	}
	for _, code := range []string{"Gengar", "-x", "a_b", "a b", strings.Repeat("a", 64)} {
		if _, err := SetHubAgent(ctx, s.st.RW, code, []string{ScopeRead}, "", ts.Now()); err == nil {
			t.Errorf("code %q accepted", code)
		}
	}
	if got, err := ParseHubScopes("payouts, read"); err != nil || strings.Join(got, ",") != "read,payouts" {
		t.Errorf("ParseHubScopes: %v %v", got, err)
	}
}

func TestHubKeyCacheRevocationAndOutage(t *testing.T) {
	s := setupHub(t)
	ctx := context.Background()
	token := HubTokenPrefix + "gengar"
	s.hub.set(func(f *fakeHub) { f.active[Hash(token)] = "gengar" })
	if _, err := SetHubAgent(ctx, s.st.RW, "gengar", []string{ScopeRead}, "", ts.Now()); err != nil {
		t.Fatal(err)
	}
	expect := func(step string, code int, body string) {
		t.Helper()
		if got, b := call(t, s.srv, "GET", "/read", token); got != code || !strings.Contains(b, body) {
			t.Fatalf("%s: %d %s, want %d %s", step, got, b, code, body)
		}
	}
	expect("first call", 200, "hub:gengar")
	expect("cached", 200, "hub:gengar")
	if n := s.hub.questions(); n != 1 {
		t.Fatalf("the hub was asked %d times inside a minute", n)
	}

	// Revoked at the hub: seen within the cache time.
	s.hub.set(func(f *fakeHub) { delete(f.active, Hash(token)) })
	s.advance(59 * time.Second)
	expect("revoked, still cached", 200, "hub:gengar")
	s.advance(2 * time.Second)
	expect("revoked, past the cache", 401, "не действует")

	// An inactive answer is never extended while the hub is down.
	s.hub.set(func(f *fakeHub) { f.down = true })
	expect("inactive cached", 401, "не действует")
	s.advance(61 * time.Second)
	expect("inactive, hub down", 503, "хаб не отвечает")

	// The hub comes back with the key active again, then goes down: the active answer is held up to an hour.
	s.hub.set(func(f *fakeHub) { f.down, f.active[Hash(token)] = false, "gengar" })
	s.advance(61 * time.Second)
	expect("active again", 200, "hub:gengar")
	s.hub.set(func(f *fakeHub) { f.down = true })
	s.advance(10 * time.Minute)
	expect("hub down, held", 200, "hub:gengar")
	asked := s.hub.questions()
	s.advance(5 * time.Second)
	expect("hub down, held, no new question yet", 200, "hub:gengar")
	if s.hub.questions() != asked {
		t.Fatal("the hub was asked again inside the retry pause")
	}
	s.advance(49*time.Minute + 50*time.Second)
	expect("hub down, 59m55s after the answer", 200, "hub:gengar")
	s.advance(10 * time.Second)
	expect("hub down, past the hour", 503, "хаб не отвечает")
	rows := audited(t, s.st)
	held := 0
	for _, r := range rows {
		if r.held {
			held++
		}
	}
	if held != 3 {
		t.Fatalf("held calls audited: %d, want 3: %+v", held, rows)
	}

	// A 4xx is an answer: nothing is held after it.
	s.hub.set(func(f *fakeHub) { f.down = false })
	s.advance(time.Minute)
	expect("back", 200, "hub:gengar")
	s.hub.set(func(f *fakeHub) { f.refuse = true })
	s.advance(61 * time.Second)
	expect("hub refuses ledger's key", 503, "хаб отказал")
	s.hub.set(func(f *fakeHub) { f.refuse, f.down = false, true })
	expect("after a refusal nothing is held", 503, "хаб не отвечает")
}

func TestHubKeyExpiryAndNoServiceKey(t *testing.T) {
	s := setupHub(t)
	ctx := context.Background()
	token := HubTokenPrefix + "gengar"
	s.hub.set(func(f *fakeHub) { f.active[Hash(token)], f.expires = "gengar", s.clock.Add(30*time.Second) })
	if _, err := SetHubAgent(ctx, s.st.RW, "gengar", []string{ScopeRead}, "", ts.Now()); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, s.srv, "GET", "/read", token); code != 200 {
		t.Fatalf("before expiry: %d", code)
	}
	s.advance(31 * time.Second)
	if code, _ := call(t, s.srv, "GET", "/read", token); code != 401 {
		t.Fatalf("a cached answer past the key's expiry: %d", code)
	}
	UseHubKeys(nil)
	if code, body := call(t, s.srv, "GET", "/read", token); code != 401 || !strings.Contains(body, "не принимается") {
		t.Fatalf("without ledger's own hub key: %d %s", code, body)
	}
}
