package accruals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// A claim is the owner's word that a sale nobody's by the rule (no shift held the page, a PPV whose sender had no
// shift within the wait, every holder said «не моё») is a person's: they said it was theirs (the owner, 2026-11-09,
// decision 3 of docs/plans/2026-11-09-release-2.md). sale_claims keeps every claim and its removal; the shadow
// rebuild gives a claimed nobody's sale to its person whole, on the sale's Moscow day, at their rate of the page's
// model (split.ReasonOwnerClaim). A claim of a sale with any other home is refused, and one whose sale stops being
// nobody's (new facts) is not honoured: Report.ClaimsIgnored and v_sale_claims say so.

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("POST /v1/accruals/claim", auth.Require(s, auth.ScopeDecide, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in ClaimInput
			if !decode(w, r, &in) {
				return
			}
			out, err := Claim(r.Context(), s.Store, in, auth.KeyName(r.Context()), ts.Now())
			respond(s, w, out, err)
		})))
		s.Mux.Handle("POST /v1/accruals/unclaim", auth.Require(s, auth.ScopeDecide, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in ClaimInput
			if !decode(w, r, &in) {
				return
			}
			out, err := Unclaim(r.Context(), s.Store, in, auth.KeyName(r.Context()), ts.Now())
			respond(s, w, out, err)
		})))
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		server.WriteError(w, http.StatusBadRequest, "тело запроса: нужен JSON {transactionRef, person, note}")
		return false
	}
	return true
}

func respond(s *server.Server, w http.ResponseWriter, out ClaimResult, err error) {
	var r *Refusal
	switch {
	case errors.As(err, &r):
		server.WriteError(w, http.StatusUnprocessableEntity, r.Error())
	case err != nil:
		s.Log.Error("accruals claim", "err", err)
		server.WriteError(w, http.StatusInternalServerError, "заявка не записана: сбой сервиса, подробности в журнале")
	default:
		server.WriteJSON(w, http.StatusOK, out)
	}
}

// Refusal is a claim not written: the request is wrong, the sale is not nobody's, and so on.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

func refuse(format string, args ...any) *Refusal { return &Refusal{Msg: fmt.Sprintf(format, args...)} }

// ClaimInput is the owner's word: the sale (Hub's transactionRef) is the person's (people.slug). Unclaim reads Ref
// and Note.
type ClaimInput struct {
	Ref    string `json:"transactionRef"`
	Person string `json:"person,omitempty"`
	Note   string `json:"note"`
}

// ClaimRecord is a row of sale_claims.
type ClaimRecord struct {
	ID         int64  `json:"id"`
	Ref        string `json:"transactionRef"`
	Action     string `json:"action"`
	Person     string `json:"person,omitempty"`
	Note       string `json:"note"`
	RecordedBy string `json:"recordedBy"`
	RecordedAt string `json:"recordedAt"`
}

// ClaimResult is the row written and the sale's home after it, as the shadow tables now hold it.
type ClaimResult struct {
	OK    bool        `json:"ok"`
	Claim ClaimRecord `json:"claim"`
	// Sale is the sale's rows in fact_homes: its day, page, net, where it went and why; the claimed one carries the
	// amount at the person's rate.
	Sale []SaleLine `json:"sale"`
	Rate *float64   `json:"rate,omitempty"`
}

// claim is a sale's claim in force.
type claim struct {
	id                        int64
	ref, person, note, by, at string
}

// Claim writes the owner's word that a nobody's sale is the person's, rebuilds the shadow tables and returns the sale
// as they now hold it, in one transaction. A sale already claimed, not in the proven facts, or not nobody's is refused.
func Claim(ctx context.Context, store *db.Store, in ClaimInput, author string, now time.Time) (ClaimResult, error) {
	in.Ref, in.Person, in.Note = strings.TrimSpace(in.Ref), strings.TrimSpace(in.Person), strings.TrimSpace(in.Note)
	switch {
	case in.Ref == "":
		return ClaimResult{}, refuse("нужен номер продажи хаба (transactionRef)")
	case in.Person == "":
		return ClaimResult{}, refuse("нужен человек: --person <код карточки из people>")
	case in.Note == "":
		return ClaimResult{}, refuse("нужна заметка: чьё слово и почему (--note)")
	}
	return inTx(ctx, store, now, func(tx *sql.Tx) (ClaimResult, error) {
		var name string
		switch err := tx.QueryRowContext(ctx, `SELECT name FROM people WHERE slug = ?`, in.Person).Scan(&name); {
		case errors.Is(err, sql.ErrNoRows):
			return ClaimResult{}, refuse("нет человека «%s» в people: нужен код карточки (people.slug)", in.Person)
		case err != nil:
			return ClaimResult{}, err
		}
		cur, ok, err := activeClaim(ctx, tx, in.Ref)
		if err != nil {
			return ClaimResult{}, err
		}
		if ok {
			return ClaimResult{}, refuse("продажа %s уже заявлена: %s, записал %s %s. Сначала ledger accruals unclaim %s",
				in.Ref, cur.person, cur.by, cur.at, in.Ref)
		}
		if why, err := notInFacts(ctx, tx, in.Ref); err != nil || why != "" {
			if err != nil {
				return ClaimResult{}, err
			}
			return ClaimResult{}, refuse("продажа %s: %s", in.Ref, why)
		}
		rec := ClaimRecord{Ref: in.Ref, Action: "claim", Person: in.Person, Note: in.Note, RecordedBy: author, RecordedAt: ts.Format(now)}
		if rec.ID, err = insertClaim(ctx, tx, rec); err != nil {
			return ClaimResult{}, err
		}
		rep, err := rebuildTx(ctx, tx, now)
		if err != nil {
			return ClaimResult{}, err
		}
		for _, ig := range rep.ClaimsIgnored {
			if ig.Ref == in.Ref {
				return ClaimResult{}, refuse("продажа %s: %s", in.Ref, ig.Why)
			}
		}
		return result(ctx, tx, rec)
	})
}

// Unclaim writes that a sale's claim is removed (a new row: the claim stays in the history), rebuilds the shadow
// tables and returns the sale as the rule alone gives it.
func Unclaim(ctx context.Context, store *db.Store, in ClaimInput, author string, now time.Time) (ClaimResult, error) {
	in.Ref, in.Note = strings.TrimSpace(in.Ref), strings.TrimSpace(in.Note)
	switch {
	case in.Ref == "":
		return ClaimResult{}, refuse("нужен номер продажи хаба (transactionRef)")
	case in.Note == "":
		return ClaimResult{}, refuse("нужна заметка: чьё слово и почему (--note)")
	}
	return inTx(ctx, store, now, func(tx *sql.Tx) (ClaimResult, error) {
		if _, ok, err := activeClaim(ctx, tx, in.Ref); err != nil || !ok {
			if err != nil {
				return ClaimResult{}, err
			}
			return ClaimResult{}, refuse("у продажи %s нет действующей заявки: снимать нечего", in.Ref)
		}
		rec := ClaimRecord{Ref: in.Ref, Action: "unclaim", Note: in.Note, RecordedBy: author, RecordedAt: ts.Format(now)}
		var err error
		if rec.ID, err = insertClaim(ctx, tx, rec); err != nil {
			return ClaimResult{}, err
		}
		if _, err := rebuildTx(ctx, tx, now); err != nil {
			return ClaimResult{}, err
		}
		return result(ctx, tx, rec)
	})
}

func inTx(ctx context.Context, store *db.Store, now time.Time, f func(*sql.Tx) (ClaimResult, error)) (ClaimResult, error) {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, err
	}
	defer tx.Rollback()
	out, err := f(tx)
	if err != nil {
		return ClaimResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ClaimResult{}, err
	}
	return out, settle(ctx, store, now)
}

func insertClaim(ctx context.Context, tx *sql.Tx, r ClaimRecord) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO sale_claims(transaction_ref, action, person, note, recorded_by, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)`, r.Ref, r.Action, nullStr(r.Person), r.Note, r.RecordedBy, r.RecordedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// activeClaim is the sale's claim in force: its last row, if a claim.
func activeClaim(ctx context.Context, q ledger.Querier, ref string) (claim, bool, error) {
	var c claim
	var action string
	var person sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, action, person, note, recorded_by, recorded_at FROM sale_claims
		WHERE transaction_ref = ? ORDER BY id DESC LIMIT 1`, ref).Scan(&c.id, &action, &person, &c.note, &c.by, &c.at)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	c.ref, c.person = ref, person.String
	return c, action == "claim", nil
}

// loadClaims are the claims in force, by sale.
func loadClaims(ctx context.Context, q ledger.Querier) (map[string]claim, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.id, c.transaction_ref, c.person, c.note, c.recorded_by, c.recorded_at FROM sale_claims c
		WHERE c.action = 'claim' AND c.id = (SELECT MAX(x.id) FROM sale_claims x WHERE x.transaction_ref = c.transaction_ref)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]claim{}
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.id, &c.ref, &c.person, &c.note, &c.by, &c.at); err != nil {
			return nil, err
		}
		out[c.ref] = c
	}
	return out, rows.Err()
}

// notInFacts says why a sale is not in the proven facts; "" — it is.
func notInFacts(ctx context.Context, q ledger.Querier, ref string) (string, error) {
	var win, status string
	err := q.QueryRowContext(ctx, `SELECT s.window_from, COALESCE(t.status, '') FROM hub_sales s
		LEFT JOIN hub_team_sync t ON t.window_from = s.window_from WHERE s.transaction_ref = ?`, ref).Scan(&win, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "нет в фактах хаба (hub_sales): нужен transactionRef продажи хаба; свежие факты — ledger hub team-sync", nil
	case err != nil:
		return "", err
	case status != "ok":
		return "окно фактов " + win[:10] + " не доказано, чья продажа — неизвестно: ledger hub team-sync, затем снова", nil
	case win < ts.Format(ts.WeekStart(split.Default.Since)):
		return "продажа раньше правил дележа (" + ts.Date(split.Default.Since) + "): её платит таблица выплат", nil
	}
	return "", nil
}

// checkClaims counts the claims the rebuild honoured and says why it did not honour the rest.
func checkClaims(ctx context.Context, q ledger.Querier, claims map[string]claim, claimed map[string][]homeRow,
	models map[string]string, who *people, rep *Report) error {
	refs := make([]string, 0, len(claims))
	for ref := range claims {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	var names map[string]string
	for _, ref := range refs {
		c := claims[ref]
		rows := claimed[ref]
		var why string
		switch {
		case len(rows) == 0:
			w, err := notInFacts(ctx, q, ref)
			if err != nil {
				return err
			}
			if why = w; why == "" {
				why = "нет в доказанных фактах"
			}
		case rows[0].reason == split.ReasonOwnerClaim && rows[0].person == c.person:
			rep.Claims++
			continue
		case len(rows) == 1 && rows[0].home == split.Nobody:
			why = "у карточки «" + c.person + "» нет пользователя хаба для модели «" + models[rows[0].page] +
				"»: добавьте hub:<id> в aliases карточки"
		default:
			if names == nil {
				var err error
				if names, err = personNames(ctx, q); err != nil {
					return err
				}
			}
			why = "не ничья — " + describe(rows, names) + "; заявить можно только ничью продажу"
		}
		rep.ClaimsIgnored = append(rep.ClaimsIgnored, IgnoredClaim{Ref: ref, Person: c.person, Why: why})
	}
	return nil
}

// describe says where a sale's rows went and why.
func describe(rows []homeRow, names map[string]string) string {
	var parts []string
	seen := map[string]bool{}
	for _, h := range rows {
		p := whereTo(h, names) + " (" + ReasonText(h.reason, h.waitsUntil) + ")"
		if !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

// result reads the sale back from the shadow tables: its rows and, for the claimed one, the amount at the rate.
func result(ctx context.Context, q ledger.Querier, rec ClaimRecord) (ClaimResult, error) {
	out := ClaimResult{OK: true, Claim: rec, Sale: []SaleLine{}}
	homes, err := saleHomes(ctx, q, rec.Ref)
	if err != nil {
		return out, err
	}
	names, err := personNames(ctx, q)
	if err != nil {
		return out, err
	}
	for _, h := range homes {
		var amount *int64
		if h.reason == split.ReasonOwnerClaim {
			var rate sql.NullFloat64
			err := q.QueryRowContext(ctx, `SELECT rate FROM fact_accruals WHERE hub_user_id = ? AND model = ? AND page_label = ? AND day = ?`,
				h.user, h.model, h.page, h.day).Scan(&rate)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
			if rate.Valid {
				r := rate.Float64
				v := int64(money.FromFloat(float64(float64(h.share)/money.Scale) * r))
				out.Rate, amount = &r, &v
			}
		}
		out.Sale = append(out.Sale, line("", h, amount, whereTo(h, names)))
	}
	return out, nil
}

// userOf is the Hub user a person's claim on a sale of the model is credited to: the card's own hub:<id>, else a Hub
// user of the facts whose card on the model is the person (the lowest id); false — none.
func (p *people) userOf(person, model string, usernames map[int64]string) (int64, bool) {
	var ids []int64
	if card, ok := p.res.BySlug(person); ok {
		for _, a := range card.Aliases {
			if v, ok := strings.CutPrefix(strings.TrimSpace(a), "hub:"); ok {
				if id, err := strconv.ParseInt(v, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	known := make([]int64, 0, len(usernames))
	for id := range usernames {
		known = append(known, id)
	}
	sort.Slice(known, func(i, j int) bool { return known[i] < known[j] })
	for _, id := range append(ids, known...) {
		name := usernames[id]
		if name == "" {
			name = "hub:" + strconv.FormatInt(id, 10)
		}
		if got, _ := p.person(id, name, model); got == person {
			return id, true
		}
	}
	return 0, false
}
