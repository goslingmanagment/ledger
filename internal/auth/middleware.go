package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

type ctxKey struct{}

// Require serves h only to a request with "Authorization: Bearer <key>" of an active key that
// carries scope: 401 without a key or with an unknown or revoked one, 403 without the scope.
// h finds the caller with FromContext or KeyName. A hub agent key (HubTokenPrefix) is checked at the
// hub and gets the rights of its agent's row in hub_agents, reading only; its KeyName is "hub:<code>".
func Require(s *server.Server, scope string, h http.Handler) http.Handler {
	return RequireAny(s, []string{scope}, h)
}

// RequireAny is Require for a route open to several scopes: a key with any one of them passes.
func RequireAny(s *server.Server, scopes []string, h http.Handler) http.Handler {
	if len(scopes) == 0 {
		panic("auth.Require: no scope")
	}
	for _, scope := range scopes {
		if !slices.Contains(Scopes, scope) {
			panic("auth.Require: unknown scope " + scope)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w, "нужен ключ: заголовок Authorization: Bearer <ключ>")
			return
		}
		if strings.HasPrefix(token, HubTokenPrefix) {
			serveHubKey(s, scopes, h, w, r, token)
			return
		}
		k, err := Lookup(r.Context(), s.Store.RO, token)
		switch {
		case errors.Is(err, ErrUnknownKey):
			unauthorized(w, "ключ не подходит")
			return
		case errors.Is(err, ErrRevoked):
			unauthorized(w, "ключ "+k.Name+" отозван")
			return
		case err != nil:
			s.Log.Error("key lookup", "err", err)
			server.WriteError(w, http.StatusServiceUnavailable, "ключ не проверить: база недоступна")
			return
		}
		if !slices.ContainsFunc(scopes, k.Has) {
			server.WriteError(w, http.StatusForbidden, fmt.Sprintf("у ключа %s нет права %s", k.Name, strings.Join(scopes, " или ")))
			return
		}
		tctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		if err := Touch(tctx, s.Store.RW, k, ts.Now()); err != nil {
			s.Log.Warn("key last_used_at", "key", k.Name, "err", err)
		}
		cancel()
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, k)))
	})
}

// serveHubKey is Require for an agent's hub key: the hub says whose it is, hub_agents says what that agent may do
// here (only HubScopes), and every call that reaches the table is written to hub_key_calls under the agent's code.
func serveHubKey(s *server.Server, scopes []string, h http.Handler, w http.ResponseWriter, r *http.Request, token string) {
	hk := currentHubKeys()
	if hk == nil || len(token) > 256 {
		unauthorized(w, "ключ хаба здесь не принимается: у ledger нет своего ключа хаба (LEDGER_HUB_INTROSPECT_KEY)")
		return
	}
	ans, err := hk.Who(r.Context(), token)
	switch {
	case errors.Is(err, ErrHubUnreachable):
		s.Log.Warn("hub key: the hub did not answer", "err", err)
		server.WriteError(w, http.StatusServiceUnavailable, "ключ хаба не проверить: хаб не отвечает, а последнего ответа о ключе нет или ему больше часа")
		return
	case errors.Is(err, ErrHubRefused):
		s.Log.Error("hub key: the hub refused the question", "err", err)
		server.WriteError(w, http.StatusServiceUnavailable, "ключ хаба не проверить: хаб отказал ledger в ответе")
		return
	case err != nil:
		server.WriteError(w, http.StatusServiceUnavailable, "ключ хаба не проверить")
		return
	case !ans.Active:
		unauthorized(w, "ключ хаба не действует: отозван, истёк или не принадлежит агенту")
		return
	}
	now := ts.Now()
	audit := func(status int) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		if err := auditHubCall(actx, s.Store.RW, ans.Code, r.Method, r.URL.Path, status, ans.Held, now); err != nil {
			s.Log.Error("hub_key_calls", "agent", ans.Code, "err", err)
		}
	}
	a, err := GetHubAgent(r.Context(), s.Store.RO, ans.Code)
	switch {
	case errors.Is(err, ErrNoHubAgent) || (err == nil && a.DisabledAt != ""):
		audit(http.StatusForbidden)
		server.WriteError(w, http.StatusForbidden, "агенту хаба "+ans.Code+" ledger не открыт (ledger keys hub-agent)")
		return
	case err != nil:
		s.Log.Error("hub agent lookup", "agent", ans.Code, "err", err)
		server.WriteError(w, http.StatusServiceUnavailable, "ключ не проверить: база недоступна")
		return
	}
	if !slices.ContainsFunc(scopes, func(sc string) bool { return slices.Contains(a.Scopes, sc) }) {
		audit(http.StatusForbidden)
		server.WriteError(w, http.StatusForbidden, fmt.Sprintf("у агента хаба %s нет права %s", ans.Code, strings.Join(scopes, " или ")))
		return
	}
	k := Key{Name: hubKeyName(ans.Code), Scopes: a.Scopes}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxKey{}, k)))
	audit(rec.status)
}

// statusRecorder remembers the status a handler answered with.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ledger"`)
	server.WriteError(w, http.StatusUnauthorized, msg)
}

// FromContext returns the key that authenticated the request.
func FromContext(ctx context.Context) (Key, bool) {
	k, ok := ctx.Value(ctxKey{}).(Key)
	return k, ok
}

// KeyName is the name of the request's key, "" without one.
func KeyName(ctx context.Context) string {
	k, _ := FromContext(ctx)
	return k.Name
}
