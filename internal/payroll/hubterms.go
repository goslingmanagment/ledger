package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The pay terms of the people registry (ecosystem H3): a person's share and fix per shift on a model live in Hub's
// people registry («Люди → карточка → График и условия»), each term valid from a day until the person's next term of the
// model. The job hub-terms reads Hub's feed (GET /api/v1/agent/people) every 15 minutes after a cursor by the people's
// updatedAt, with an overlap, using ledger's Hub key; the terms come only when the key has read:pay_terms
// (payTermsIncluded), and without it nothing is kept. Every person served is stored whole (hub_people) and so are their
// terms (hub_pay_terms): a term Hub no longer serves for a person it serves is marked gone, never deleted.
//
// Until settings.payroll_terms_hub_from the registry is a shadow: the tabs' headers price every day (Terms), and the
// registry only stands beside them in `ledger payroll terms diff` and the health check. From that day a day's accrual
// (the reports' and the facts') takes the person's term valid ON THAT DAY from the registry; a day with no registry term
// keeps the tab's header and the health check fails for that person. The tab still says where a day is posted.

// SettingTermsHubFrom is the first Moscow day whose accruals take the person's terms from the registry of Hub; empty or
// absent — the tabs' headers.
const SettingTermsHubFrom = "payroll_terms_hub_from"

// HubTermsJob is the scheduler's name of the job that reads the registry's feed.
const HubTermsJob = "hub-terms"

const (
	termsCursor       = "hub:people"
	termsStatusCursor = "hub:people:status"
	// termsOverlap is how far before the last updatedAt served the next read asks: a change committed late is not skipped.
	termsOverlap = 10 * time.Minute
)

func init() {
	server.Every(HubTermsJob, 15*time.Minute, func(ctx context.Context, s *server.Server) error {
		cfg, err := hub.ConfigFromEnv()
		if err != nil {
			return err
		}
		_, err = SyncHubTerms(ctx, s.Store, hub.NewClient(cfg, nil), time.Now())
		return err
	})
}

// TermsHubFrom is the first day whose accruals take the registry's terms, "" when the tabs' headers.
func TermsHubFrom(ctx context.Context, q ledger.Querier) (string, error) {
	return settingDay(ctx, q, SettingTermsHubFrom)
}

// SetTermsHubFrom sets the first day whose accruals take the registry's terms ("" turns it off).
func SetTermsHubFrom(ctx context.Context, q ledger.Querier, day string, now time.Time) error {
	if day != "" {
		t, err := time.Parse(ts.DateLayout, day)
		if err != nil || t.Format(ts.DateLayout) != day {
			return fmt.Errorf("дата: нужна ГГГГ-ММ-ДД, а не %q", day)
		}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingTermsHubFrom, day, termsHubFromNote, ts.Format(now))
	return err
}

const termsHubFromNote = "С какого дня (МСК) начисление дня берёт условия человека (доля, фикс за смену) из реестра людей хаба " +
	"(hub_pay_terms), действующие в этот день; нет условия на день — шапка вкладки, как прежде, и проверка падает. Пусто — " +
	"только шапки вкладок. Ставит и снимает ledger payroll terms-from; откат — --off и пересборка"

// HubTerm is a person's term on a model as the registry serves it.
type HubTerm struct {
	ID        int64  `json:"id"`
	Person    string `json:"person"`
	ModelSlug string `json:"modelSlug"` // Hub's
	Model     string `json:"model"`     // ledger's: settings.hub_model_slugs of the slug, or the slug
	ValidFrom string `json:"validFrom"`
	ValidTo   string `json:"validTo,omitempty"` // "" — it holds now
	ShareBP   int64  `json:"shareBasisPoints"`
	FixMicros int64  `json:"fixMicros"`
	Note      string `json:"note,omitempty"`
}

// Holds says the term holds on the day.
func (h HubTerm) Holds(day string) bool {
	return h.ValidFrom <= day && (h.ValidTo == "" || day <= h.ValidTo)
}

// Rate is the share as the tabs' «Ставка %» holds it: 2500 basis points are 0.25.
func (h HubTerm) Rate() float64 { return float64(h.ShareBP) / 10_000 }

// Terms are the term on a tab of the person: the tab says where the day is posted, the registry what it is paid.
func (h HubTerm) Terms(tab Terms) Terms {
	r := h.Rate()
	return Terms{Model: tab.Model, Name: tab.Name, Person: tab.Person, Rate: &r, ShiftFix: float64(h.FixMicros) / money.Scale}
}

// HubTerms are the registry's terms ledger keeps.
type HubTerms []HubTerm

// On is the person's term of the model holding on the day; nil — the registry has none.
func (hs HubTerms) On(person, model, day string) *HubTerm {
	var found *HubTerm
	for i := range hs {
		h := &hs[i]
		if h.Person == person && ledger.Slug(h.Model) == ledger.Slug(model) && h.Holds(day) && (found == nil || h.ValidFrom > found.ValidFrom) {
			found = h
		}
	}
	return found
}

// LoadHubTerms reads the terms the registry serves now (not gone), the model as ledger names it.
func LoadHubTerms(ctx context.Context, q ledger.Querier) (HubTerms, error) {
	slugs, err := optionalMap(ctx, q, "hub_model_slugs")
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT hub_id, person, model_slug, valid_from, COALESCE(valid_to, ''), share_bp, fix_micros,
		COALESCE(note, '') FROM hub_pay_terms WHERE gone_at IS NULL ORDER BY person, model_slug, valid_from, hub_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out HubTerms
	for rows.Next() {
		var h HubTerm
		if err := rows.Scan(&h.ID, &h.Person, &h.ModelSlug, &h.ValidFrom, &h.ValidTo, &h.ShareBP, &h.FixMicros, &h.Note); err != nil {
			return nil, err
		}
		h.Model = h.ModelSlug
		if m, ok := slugs[h.ModelSlug]; ok {
			h.Model = m
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TermsStatus is the last pass over the registry's feed (cursors hub:people:status).
type TermsStatus struct {
	At string `json:"at"`
	OK bool   `json:"ok"`
	// Absent: Hub has no people feed (404, an older Hub): nothing to read, not a failure.
	Absent bool `json:"absent,omitempty"`
	// NoTerms: the key lacks read:pay_terms (payTermsIncluded false): nothing is kept, not a failure.
	NoTerms bool   `json:"noTerms,omitempty"`
	Error   string `json:"error,omitempty"`
	Since   string `json:"since"` // what was asked; "" — everyone
	Next    string `json:"next"`  // the greatest updatedAt served so far
	People  int    `json:"people"`
	Terms   int    `json:"terms"`
	Written int    `json:"written"` // terms new or changed
	Gone    int    `json:"gone"`    // terms Hub no longer serves
	// LastOKAt is the last pass that read the terms; with NoTerms it stays as it was.
	LastOKAt string `json:"lastOkAt,omitempty"`
}

// ReadTermsStatus reads the last pass; nil — the feed was never read.
func ReadTermsStatus(ctx context.Context, q ledger.Querier) (*TermsStatus, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, termsStatusCursor).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st TermsStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("%s: %w", termsStatusCursor, err)
	}
	return &st, nil
}

func putTermsCursor(ctx context.Context, q ledger.Querier, name, value string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, value, ts.Format(now))
	return err
}

// SyncHubTerms reads the registry's people changed since the cursor (less the overlap) and stores them and their terms.
// A Hub without the feed (404) is «nothing yet», a key without read:pay_terms keeps nothing: the status says so and
// the pass succeeds. With settings.payroll_terms_hub_from set, a change of the terms rebuilds the books.
func SyncHubTerms(ctx context.Context, store *db.Store, c *hub.Client, now time.Time) (TermsStatus, error) {
	st := TermsStatus{At: ts.Format(now)}
	if prev, err := ReadTermsStatus(ctx, store.RW); err != nil {
		return st, err
	} else if prev != nil {
		st.LastOKAt = prev.LastOKAt
	}
	var cursor string
	if err := store.RW.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, termsCursor).Scan(&cursor); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.Next = cursor
	var since time.Time
	if cursor != "" {
		t, err := time.Parse(time.RFC3339Nano, cursor)
		if err != nil {
			return st, fmt.Errorf("%s: %q — не время: %w", termsCursor, cursor, err)
		}
		since = t.Add(-termsOverlap)
		st.Since = since.UTC().Format(time.RFC3339)
	}
	save := func() error {
		b, _ := json.Marshal(st)
		_, err := store.RW.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, termsStatusCursor, string(b), ts.Format(now))
		return err
	}
	ans, readErr := c.People(ctx, since)
	switch {
	case readErr == nil && !ans.PayTermsIncluded:
		st.OK, st.NoTerms = true, true
		st.Error = "ключу ledger в хабе не дано read:pay_terms — условия реестра не читаются"
		return st, save()
	case readErr == nil:
	case hub.IsAbsent(readErr):
		st.OK, st.Absent, st.LastOKAt = true, true, st.At
		st.Error = "хаб ещё не отдаёт реестр людей (" + hub.PeoplePath + " — 404)"
		return st, save()
	case hub.IsKeyRefused(readErr):
		st.Error = hub.KeyRefusalText(readErr)
	case hub.IsRefusal(readErr):
		st.Error = readErr.Error()
	default:
		return st, readErr // a cancelled pass: the status stays as it was
	}
	if readErr != nil {
		if err := save(); err != nil {
			return st, err
		}
		return st, errors.New(st.Error)
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback()
	next, err := storeRegistry(ctx, tx, ans, now, &st)
	if err != nil {
		return st, err
	}
	if next != "" {
		if err := putTermsCursor(ctx, tx, termsCursor, next, now); err != nil {
			return st, err
		}
		st.Next = next
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	st.OK, st.LastOKAt = true, st.At
	if err := save(); err != nil {
		return st, err
	}
	if st.Written+st.Gone == 0 {
		return st, nil
	}
	from, err := TermsHubFrom(ctx, store.RO)
	if err != nil {
		return st, err
	}
	if from != "" {
		if _, err := Rebuild(ctx, store, now); err != nil {
			return st, fmt.Errorf("долги после условий реестра: %w", err)
		}
	}
	return st, nil
}

// storeRegistry writes the people of an answer and their terms, and returns the greatest updatedAt served (or the
// cursor as it was, when nothing is newer).
func storeRegistry(ctx context.Context, tx *sql.Tx, ans *hub.PeopleAnswer, now time.Time, st *TermsStatus) (string, error) {
	at := ts.Format(now)
	next := st.Next
	var nextT time.Time
	if next != "" {
		nextT, _ = time.Parse(time.RFC3339Nano, next)
	}
	for _, p := range ans.People {
		st.People++
		if t, _ := time.Parse(time.RFC3339Nano, p.UpdatedAt); t.After(nextT) {
			nextT, next = t, p.UpdatedAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_people(hub_id, code, kind, role, state, left_on, updated_at, stored_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(hub_id) DO UPDATE SET code = excluded.code, kind = excluded.kind, role = excluded.role, state = excluded.state,
			  left_on = excluded.left_on, updated_at = excluded.updated_at, stored_at = excluded.stored_at
			WHERE hub_people.code IS NOT excluded.code OR hub_people.kind IS NOT excluded.kind OR hub_people.role IS NOT excluded.role
			  OR hub_people.state IS NOT excluded.state OR hub_people.left_on IS NOT excluded.left_on
			  OR hub_people.updated_at IS NOT excluded.updated_at`,
			p.ID, p.Code, p.Kind, p.Role, p.State, p.LeftOn, p.UpdatedAt, at); err != nil {
			return "", err
		}
		served := map[int64]bool{}
		for _, t := range p.PayTerms {
			st.Terms++
			served[t.ID] = true
			res, err := tx.ExecContext(ctx, `INSERT INTO hub_pay_terms(hub_id, hub_person_id, person, model_slug, model_name, valid_from,
				valid_to, share_bp, fix_micros, note, updated_at, stored_at, gone_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
				ON CONFLICT(hub_id) DO UPDATE SET hub_person_id = excluded.hub_person_id, person = excluded.person,
				  model_slug = excluded.model_slug, model_name = excluded.model_name, valid_from = excluded.valid_from,
				  valid_to = excluded.valid_to, share_bp = excluded.share_bp, fix_micros = excluded.fix_micros, note = excluded.note,
				  updated_at = excluded.updated_at, stored_at = excluded.stored_at, gone_at = NULL
				WHERE hub_pay_terms.hub_person_id IS NOT excluded.hub_person_id OR hub_pay_terms.person IS NOT excluded.person
				  OR hub_pay_terms.model_slug IS NOT excluded.model_slug OR hub_pay_terms.model_name IS NOT excluded.model_name
				  OR hub_pay_terms.valid_from IS NOT excluded.valid_from OR hub_pay_terms.valid_to IS NOT excluded.valid_to
				  OR hub_pay_terms.share_bp IS NOT excluded.share_bp OR hub_pay_terms.fix_micros IS NOT excluded.fix_micros
				  OR hub_pay_terms.note IS NOT excluded.note OR hub_pay_terms.updated_at IS NOT excluded.updated_at
				  OR hub_pay_terms.gone_at IS NOT NULL`,
				t.ID, p.ID, p.Code, t.ModelSlug, t.ModelName, t.ValidFrom, t.ValidTo, *t.ShareBasisPoints, *t.FixPerShiftMills*1000,
				t.Note, t.UpdatedAt, at)
			if err != nil {
				return "", err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				st.Written++
			}
		}
		// A term of this person Hub no longer serves: kept, marked gone.
		rows, err := tx.QueryContext(ctx, `SELECT hub_id FROM hub_pay_terms WHERE hub_person_id = ? AND gone_at IS NULL`, p.ID)
		if err != nil {
			return "", err
		}
		var gone []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", err
			}
			if !served[id] {
				gone = append(gone, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		sort.Slice(gone, func(i, j int) bool { return gone[i] < gone[j] })
		for _, id := range gone {
			if _, err := tx.ExecContext(ctx, `UPDATE hub_pay_terms SET gone_at = ? WHERE hub_id = ?`, at, id); err != nil {
				return "", err
			}
			st.Gone++
		}
	}
	return next, nil
}
