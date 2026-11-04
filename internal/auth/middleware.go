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
// h finds the caller with FromContext or KeyName.
func Require(s *server.Server, scope string, h http.Handler) http.Handler {
	if !slices.Contains(Scopes, scope) {
		panic("auth.Require: unknown scope " + scope)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w, "нужен ключ: заголовок Authorization: Bearer <ключ>")
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
		if !k.Has(scope) {
			server.WriteError(w, http.StatusForbidden, fmt.Sprintf("у ключа %s нет права %s", k.Name, scope))
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
