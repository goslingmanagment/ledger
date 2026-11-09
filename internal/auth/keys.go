// Package auth issues the agents' API keys and checks them on every request.
//
// A key is "ldg_<name>_<52 base32 characters>": 32 random bytes behind a readable prefix. The
// database keeps its SHA-256 and the first characters for display; the key itself is printed
// once, by `ledger keys create` on the server.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// Scopes of a key. None implies another.
const (
	ScopeRead    = "read"    // SQL and schema
	ScopePropose = "propose" // proposals and disputes
	ScopeDecide  = "decide"  // the owner's decisions from the brief: openclaw-main only
	ScopeCollect = "collect" // collectors on the Mac send events
	ScopeImport  = "import"  // one-off imports
	// ScopePayouts opens GET /v1/payouts and GET /v1/payouts/prepare and nothing else: Hub's server reads
	// the chatters' pay with it and sees no owner draws, no other expenses, no cash and no SQL.
	ScopePayouts = "payouts"
)

// Scopes lists every scope in canonical order.
var Scopes = []string{ScopeRead, ScopePropose, ScopeDecide, ScopeCollect, ScopeImport, ScopePayouts}

// Errors callers branch on.
var (
	ErrUnknownKey = errors.New("unknown key")
	ErrRevoked    = errors.New("key is revoked")
	ErrExists     = errors.New("key name is taken")
	ErrNoKey      = errors.New("no key with this name")
)

const tokenPrefix = "ldg_"

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	b32    = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// Key is a stored key without its secret.
type Key struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"createdAt"`
	RevokedAt  string   `json:"revokedAt,omitempty"`
	LastUsedAt string   `json:"lastUsedAt,omitempty"`
}

// Has reports whether the key carries scope.
func (k Key) Has(scope string) bool { return slices.Contains(k.Scopes, scope) }

// ParseScopes reads "read,propose" into canonical order. An unknown scope or none is an error.
func ParseScopes(s string) ([]string, error) {
	want := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !slices.Contains(Scopes, p) {
			return nil, fmt.Errorf("unknown scope %q, known: %s", p, strings.Join(Scopes, ", "))
		}
		want[p] = true
	}
	var out []string
	for _, sc := range Scopes {
		if want[sc] {
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scope given, known: %s", strings.Join(Scopes, ", "))
	}
	return out, nil
}

// Hash is the stored form of a key: SHA-256, hex.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create issues a key for name and returns it; this is the only time it is seen. A taken name
// is ErrExists unless replace: then the old key is revoked and kept for the record as
// "<name>~<id>", and the new key takes the name.
func Create(ctx context.Context, db *sql.DB, name string, scopes []string, replace bool, now time.Time) (string, Key, error) {
	if !nameRe.MatchString(name) {
		return "", Key{}, fmt.Errorf("key name %q: lowercase latin letters, digits and '-', up to 32", name)
	}
	if len(scopes) == 0 {
		return "", Key{}, fmt.Errorf("key %s needs at least one scope", name)
	}
	for _, sc := range scopes {
		if !slices.Contains(Scopes, sc) {
			return "", Key{}, fmt.Errorf("unknown scope %q", sc)
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Key{}, err
	}
	secret := strings.ToLower(b32.EncodeToString(raw[:]))
	token := tokenPrefix + name + "_" + secret
	k := Key{Name: name, Prefix: tokenPrefix + name + "_" + secret[:4], Scopes: scopes, CreatedAt: ts.Format(now)}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", Key{}, err
	}
	defer tx.Rollback()
	var old int64
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM keys WHERE name = ?`, name).Scan(&old); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", Key{}, err
	case !replace:
		return "", Key{}, fmt.Errorf("%w: %s", ErrExists, name)
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE keys SET name = name || '~' || id,
			revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, k.CreatedAt, old); err != nil {
			return "", Key{}, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO keys(name, prefix, hash, scopes, created_at) VALUES (?, ?, ?, ?, ?)`,
		k.Name, k.Prefix, Hash(token), strings.Join(scopes, ","), k.CreatedAt)
	if err != nil {
		return "", Key{}, err
	}
	if k.ID, err = res.LastInsertId(); err != nil {
		return "", Key{}, err
	}
	return token, k, tx.Commit()
}

const keyColumns = `id, name, prefix, scopes, created_at, revoked_at, last_used_at`

func scanKey(row interface{ Scan(...any) error }) (Key, error) {
	var k Key
	var scopes string
	var revoked, used sql.NullString
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &scopes, &k.CreatedAt, &revoked, &used); err != nil {
		return Key{}, err
	}
	for _, sc := range strings.Split(scopes, ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			k.Scopes = append(k.Scopes, sc)
		}
	}
	k.RevokedAt, k.LastUsedAt = revoked.String, used.String
	return k, nil
}

// List returns every key: active ones first, by name.
func List(ctx context.Context, db *sql.DB) ([]Key, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+keyColumns+` FROM keys ORDER BY revoked_at IS NOT NULL, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Revoke stops the key of name at once; a second revoke keeps the first time.
func Revoke(ctx context.Context, db *sql.DB, name string, now time.Time) (Key, error) {
	if _, err := db.ExecContext(ctx, `UPDATE keys SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`,
		ts.Format(now), name); err != nil {
		return Key{}, err
	}
	k, err := scanKey(db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM keys WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, fmt.Errorf("%w: %s", ErrNoKey, name)
	}
	return k, err
}

// Lookup finds the key of token: ErrUnknownKey when no key has its hash, ErrRevoked (with the
// key) when it is revoked.
func Lookup(ctx context.Context, db *sql.DB, token string) (Key, error) {
	if !strings.HasPrefix(token, tokenPrefix) || len(token) > 128 {
		return Key{}, ErrUnknownKey
	}
	k, err := scanKey(db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM keys WHERE hash = ?`, Hash(token)))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Key{}, ErrUnknownKey
	case err != nil:
		return Key{}, err
	case k.RevokedAt != "":
		return k, ErrRevoked
	}
	return k, nil
}

// Touch records that k was used at now, at most once a minute: the condition keeps
// concurrent requests from writing twice.
func Touch(ctx context.Context, db *sql.DB, k Key, now time.Time) error {
	if last, err := ts.Parse(k.LastUsedAt); err == nil && now.Sub(last) < time.Minute {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE keys SET last_used_at = ?
		WHERE id = ? AND (last_used_at IS NULL OR last_used_at <= ?)`,
		ts.Format(now), k.ID, ts.Format(now.Add(-time.Minute)))
	return err
}
