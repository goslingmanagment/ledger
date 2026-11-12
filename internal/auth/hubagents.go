package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// hubCodeRe is the hub's person code (PERSON_CODE_PATTERN of its contracts).
var hubCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// HubAgent is a row of hub_agents: what an agent of the hub may do in ledger with its hub key.
type HubAgent struct {
	Code       string   `json:"code"`
	Scopes     []string `json:"scopes"`
	Note       string   `json:"note,omitempty"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
	DisabledAt string   `json:"disabledAt,omitempty"`
}

// ErrNoHubAgent: no row for the code.
var ErrNoHubAgent = errors.New("no hub agent with this code")

// ParseHubScopes reads "read,payouts" into canonical order: only HubScopes, at least one.
func ParseHubScopes(s string) ([]string, error) {
	scopes, err := ParseScopes(s)
	if err != nil {
		return nil, err
	}
	for _, sc := range scopes {
		if !slices.Contains(HubScopes, sc) {
			return nil, fmt.Errorf("ключу хаба право %s не даётся: только %s", sc, strings.Join(HubScopes, ", "))
		}
	}
	return scopes, nil
}

// SetHubAgent opens ledger to the hub agent code with scopes (only HubScopes): a new row, or the row's scopes and
// note replaced and a disabled row switched on again.
func SetHubAgent(ctx context.Context, db *sql.DB, code string, scopes []string, note string, now time.Time) (HubAgent, error) {
	if !hubCodeRe.MatchString(code) {
		return HubAgent{}, fmt.Errorf("код агента хаба %q: строчные латинские буквы, цифры и '-', до 63", code)
	}
	if len(scopes) == 0 {
		return HubAgent{}, fmt.Errorf("агенту %s нужно хотя бы одно право", code)
	}
	for _, sc := range scopes {
		if !slices.Contains(HubScopes, sc) {
			return HubAgent{}, fmt.Errorf("ключу хаба право %s не даётся", sc)
		}
	}
	at := ts.Format(now)
	if _, err := db.ExecContext(ctx, `INSERT INTO hub_agents(code, scopes, note, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(code) DO UPDATE SET scopes = excluded.scopes, note = excluded.note, updated_at = excluded.updated_at,
		disabled_at = NULL`, code, strings.Join(scopes, ","), note, at, at); err != nil {
		return HubAgent{}, err
	}
	return GetHubAgent(ctx, db, code)
}

// DisableHubAgent closes ledger to the code's hub key at once; the row stays for the record.
func DisableHubAgent(ctx context.Context, db *sql.DB, code string, now time.Time) (HubAgent, error) {
	if _, err := db.ExecContext(ctx, `UPDATE hub_agents SET disabled_at = ?, updated_at = ? WHERE code = ? AND disabled_at IS NULL`,
		ts.Format(now), ts.Format(now), code); err != nil {
		return HubAgent{}, err
	}
	return GetHubAgent(ctx, db, code)
}

const hubAgentColumns = `code, scopes, note, created_at, updated_at, disabled_at`

func scanHubAgent(row interface{ Scan(...any) error }) (HubAgent, error) {
	var a HubAgent
	var scopes string
	var disabled sql.NullString
	if err := row.Scan(&a.Code, &scopes, &a.Note, &a.CreatedAt, &a.UpdatedAt, &disabled); err != nil {
		return HubAgent{}, err
	}
	for _, sc := range strings.Split(scopes, ",") {
		// Only HubScopes count, whatever the row holds: the table cannot widen a hub key.
		if sc = strings.TrimSpace(sc); slices.Contains(HubScopes, sc) {
			a.Scopes = append(a.Scopes, sc)
		}
	}
	a.DisabledAt = disabled.String
	return a, nil
}

// GetHubAgent is the row of code: ErrNoHubAgent without one.
func GetHubAgent(ctx context.Context, db *sql.DB, code string) (HubAgent, error) {
	a, err := scanHubAgent(db.QueryRowContext(ctx, `SELECT `+hubAgentColumns+` FROM hub_agents WHERE code = ?`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return HubAgent{}, fmt.Errorf("%w: %s", ErrNoHubAgent, code)
	}
	return a, err
}

// ListHubAgents returns every row: switched on first, by code.
func ListHubAgents(ctx context.Context, db *sql.DB) ([]HubAgent, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+hubAgentColumns+` FROM hub_agents ORDER BY disabled_at IS NOT NULL, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HubAgent
	for rows.Next() {
		a, err := scanHubAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// hubKeyName is how a hub key's caller is named wherever ledger records who did something (query_log, key_name):
// not a ledger key's name, which has no colon.
func hubKeyName(code string) string { return "hub:" + code }

// auditHubCall writes one call of a hub key under the agent's code.
func auditHubCall(ctx context.Context, db *sql.DB, code, method, path string, status int, held bool, now time.Time) error {
	if len(path) > 512 {
		path = strings.ToValidUTF8(path[:512], "")
	}
	h := 0
	if held {
		h = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO hub_key_calls(at, agent_code, method, path, status, held) VALUES (?, ?, ?, ?, ?, ?)`,
		ts.Format(now), code, method, path, status, h)
	return err
}
