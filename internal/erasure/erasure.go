// Package erasure carries Hub's fan erasures into ledger's own copies (ecosystem review R5в; Hub's side is hub#1271).
// The job hub-erasures reads Hub's feed of completed fan erasures (platform + the fan's reference, never the fan's
// sales) after a cursor, finds the fan's sales by the fan reference in ledger's hub_sales, and clears the words about
// the fan that ledger keeps of those sales and of that fan — money, ids, states and who of the team stay:
//
//   - Hub's handed-in versions: a line's note (hub_shift_report_lines and the version's stored payload);
//   - the agent's drafts: a line's fan reference, fan name and notes (shift_report_lines and the draft's payload);
//   - decisions, questions and pot purchases on those sales or naming that fan on a page of the platform — by his id,
//     by "u" + the id or by a login ledger holds for him (namesOf): fan reference, fan name, the note, the question and
//     answer texts;
//   - the mirror of Hub's decisions (hub_sale_decisions): the note of a decision on those sales;
//   - hub_sales: the fan reference in the payload, which also makes a second pass find nothing (idempotent).
//
// What no fan reference ties to the fan stays: «мимо площадки» (who paid, what for), the shift's comment, the posts of
// «Отчётность» (their texts are in the immutable events), a decision or question that names the fan only by a login no
// other row of ledger ties to his id (Hub's feed names the id alone), and the owner's word on a nobody's sale
// (sale_claims, immutable).
package erasure

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Job is the scheduler's name of the job.
const Job = "hub-erasures"

const (
	cursorName = "hub:erasures"
	statusName = "hub:erasures:status"
	maxPages   = 25
)

// limit is how many erasures one request asks (tests ask fewer).
var limit = 200

// Erased is the text the job leaves where a required text was about the fan.
const Erased = "фан стёрт в хабе"

func init() {
	server.Every(Job, 15*time.Minute, func(ctx context.Context, s *server.Server) error {
		cfg, err := hub.ConfigFromEnv()
		if err != nil {
			return err
		}
		_, err = Sync(ctx, s.Store, hub.NewClient(cfg, nil), time.Now())
		return err
	})
}

// Status is the last pass (cursors hub:erasures:status).
type Status struct {
	At       string         `json:"at"`
	OK       bool           `json:"ok"`
	Absent   bool           `json:"absent,omitempty"` // Hub has no feed yet (404, never read before)
	Error    string         `json:"error,omitempty"`
	Since    string         `json:"since"`
	Next     string         `json:"next"`
	Read     int            `json:"read"`
	Applied  int            `json:"applied"` // erasures that changed a row
	Rows     map[string]int `json:"rows"`    // rows changed, by table
	LastOKAt string         `json:"lastOkAt,omitempty"`
}

// ReadStatus reads the last pass; nil — never.
func ReadStatus(ctx context.Context, q ledger.Querier) (*Status, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, statusName).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("%s: %w", statusName, err)
	}
	return &st, nil
}

func putCursor(ctx context.Context, q ledger.Querier, name, value string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, value, ts.Format(now))
	return err
}

// Sync reads the erasures after the cursor and applies each, a request's erasures and the cursor after them in one
// transaction. A Hub without the feed (404) before it was ever read is «nothing yet»; after, a failure.
func Sync(ctx context.Context, store *db.Store, c *hub.Client, now time.Time) (Status, error) {
	st := Status{At: ts.Format(now), Rows: map[string]int{}}
	if prev, err := ReadStatus(ctx, store.RW); err != nil {
		return st, err
	} else if prev != nil {
		st.LastOKAt = prev.LastOKAt
	}
	var since string
	if err := store.RW.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, cursorName).Scan(&since); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.Since, st.Next = since, since
	var readErr error
	for i := 0; i < maxPages; i++ {
		ans, err := c.Erasures(ctx, st.Next, limit)
		if err != nil {
			readErr = err
			break
		}
		st.Read += len(ans.Erasures)
		err = inTx(ctx, store, func(tx *sql.Tx) error {
			for _, e := range ans.Erasures {
				rows, err := Apply(ctx, tx, e, now)
				if err != nil {
					return fmt.Errorf("стирание %d: %w", e.ID, err)
				}
				changed := 0
				for k, n := range rows {
					st.Rows[k] += n
					changed += n
				}
				if changed > 0 {
					st.Applied++
				}
			}
			return putCursor(ctx, tx, cursorName, ans.Next, now)
		})
		if err != nil {
			return st, err
		}
		// The feed is read to its end when it placed nothing after the place asked from. A page with fewer erasures than
		// asked is not the end: Hub counts `limit` in erasures of every key and leaves out those of pages outside this one's.
		from, _ := strconv.ParseInt(st.Next, 10, 64)
		next, _ := strconv.ParseInt(ans.Next, 10, 64)
		st.Next = ans.Next
		if next <= from {
			break
		}
	}
	switch {
	case readErr == nil:
		st.OK, st.LastOKAt = true, st.At
	case hub.IsAbsent(readErr) && (since == "" || since == "0"):
		st.OK, st.Absent, st.LastOKAt = true, true, st.At
		st.Error = "хаб ещё не отдаёт стирания фанов (" + hub.ErasuresPath + " — 404)"
		readErr = nil
	case hub.IsAbsent(readErr):
		st.Error = "лента стираний ответила 404 (" + hub.ErasuresPath + "), хотя уже читалась: курсор " + since + " — адрес хаба или его выкатка"
	case hub.IsKeyRefused(readErr):
		st.Error = hub.KeyRefusalText(readErr)
	case hub.IsRefusal(readErr):
		st.Error = readErr.Error()
	default:
		return st, readErr
	}
	b, _ := json.Marshal(st)
	if err := inTx(ctx, store, func(tx *sql.Tx) error { return putCursor(ctx, tx, statusName, string(b), now) }); err != nil {
		return st, err
	}
	if readErr != nil {
		return st, errors.New(st.Error)
	}
	return st, nil
}

func inTx(ctx context.Context, store *db.Store, fn func(*sql.Tx) error) error {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Apply clears the words about one erased fan and records the erasure (hub_erasures: no fan reference). It returns the
// rows changed by table; a second run changes nothing.
func Apply(ctx context.Context, tx *sql.Tx, e hub.Erasure, now time.Time) (map[string]int, error) {
	out := map[string]int{}
	refs, err := salesOf(ctx, tx, e.Platform, e.FanRef)
	if err != nil {
		return nil, err
	}
	pages, err := pagesOf(ctx, tx, e.Platform)
	if err != nil {
		return nil, err
	}
	// How a row may name the fan (namesOf), read before anything is cleared.
	names, err := namesOf(ctx, tx, e.FanRef, refs, pages)
	if err != nil {
		return nil, err
	}
	exec := func(table, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		out[table] += int(n)
		return nil
	}
	refList, refArgs := inList(refs)
	pageList, pageArgs := inList(pages)
	nameList, nameArgs := inList(names)
	// The rows of the fan's sales, or naming the fan — in any of his shapes — on a page of the platform.
	whose := func(refCol, fanCol, pageCol string) (string, []any) {
		where := "(" + refCol + " IN " + refList + " OR (" + fanCol + " <> '' AND lower(" + fanCol + ") IN " + nameList + " AND " + pageCol + " IN " + pageList + "))"
		args := append(append(append([]any{}, refArgs...), nameArgs...), pageArgs...)
		return where, args
	}

	inRefs := map[string]bool{}
	for _, r := range refs {
		inRefs[r] = true
	}
	isFan := map[string]bool{}
	for _, n := range names {
		isFan[n] = true
	}
	onPlatform := map[string]bool{}
	for _, p := range pages {
		onPlatform[p] = true
	}
	// Hub's handed-in versions: the line's note, in the stored payload and in the table.
	if len(refs) > 0 {
		n, err := rewritePayloads(ctx, tx, `SELECT DISTINCT r.id, r.payload FROM hub_shift_reports r JOIN hub_shift_report_lines l ON l.report_id = r.id
			WHERE l.transaction_ref IN `+refList, refArgs, `UPDATE hub_shift_reports SET payload = ? WHERE id = ?`,
			func(m map[string]any) bool { return inRefs[refString(m["transactionId"])] }, map[string]any{"note": nil})
		if err != nil {
			return nil, fmt.Errorf("hub_shift_reports: %w", err)
		}
		out["hub_shift_reports"] += n
		if err := exec("hub_shift_report_lines", `UPDATE hub_shift_report_lines SET note = NULL WHERE note IS NOT NULL AND transaction_ref IN `+refList,
			refArgs...); err != nil {
			return nil, err
		}
	}
	// The agent's drafts: the line's fan and notes, in the stored payload (its content hash stays, so the same draft sent
	// again makes no new version) and in the table.
	where, args := whose("l.transaction_ref", "l.fan_ref", "l.page_label")
	n, err := rewritePayloads(ctx, tx, `SELECT DISTINCT r.id, r.payload FROM shift_reports r JOIN shift_report_lines l ON l.report_id = r.id
		WHERE `+where, args, `UPDATE shift_reports SET payload = ? WHERE id = ?`,
		func(m map[string]any) bool {
			ref, fan, page := refString(m["transaction_ref"]), refString(m["fan_ref"]), refString(m["page"])
			return inRefs[ref] || (fan != "" && isFan[strings.ToLower(fan)] && onPlatform[page])
		}, map[string]any{"fan_ref": "", "fan_name": "", "notes": []any{}})
	if err != nil {
		return nil, fmt.Errorf("shift_reports: %w", err)
	}
	out["shift_reports"] += n
	where, args = whose("transaction_ref", "fan_ref", "page_label")
	if err := exec("shift_report_lines", `UPDATE shift_report_lines SET fan_ref = '', fan_name = '', notes = '[]'
		WHERE (fan_ref <> '' OR fan_name <> '' OR notes <> '[]') AND `+where, args...); err != nil {
		return nil, err
	}
	// Decisions, questions, pot purchases.
	if err := exec("sale_decisions", `UPDATE sale_decisions SET fan_ref = '', fan_name = '', note = ''
		WHERE (fan_ref <> '' OR fan_name <> '' OR note <> '') AND `+where, args...); err != nil {
		return nil, err
	}
	if err := exec("sale_questions", `UPDATE sale_questions SET fan_ref = '', fan_name = '', question = ?, note = '',
		answer = CASE WHEN answer IS NULL THEN NULL ELSE ? END
		WHERE (fan_ref <> '' OR fan_name <> '' OR question <> ? OR note <> '') AND `+where,
		append([]any{Erased, Erased, Erased}, args...)...); err != nil {
		return nil, err
	}
	if err := exec("pot_sales", `UPDATE pot_sales SET fan_ref = '', fan_name = '', note = ''
		WHERE (fan_ref <> '' OR fan_name <> '' OR note <> '') AND `+where, args...); err != nil {
		return nil, err
	}
	// Hub's decisions as ledger mirrors them (hub_sale_decisions): the note of a decision on the fan's sale. Hub clears
	// its own and its feed brings that; the copy here does not wait for the feed.
	if len(refs) > 0 {
		if err := exec("hub_sale_decisions", `UPDATE hub_sale_decisions SET note = NULL WHERE note IS NOT NULL AND transaction_ref IN `+refList,
			refArgs...); err != nil {
			return nil, err
		}
	}
	// Last, the link itself: a second pass finds no sale of the fan.
	if len(refs) > 0 {
		if err := exec("hub_sales", `UPDATE hub_sales SET payload = json_set(payload, '$.fanRef', json('null'))
			WHERE transaction_ref IN `+refList, refArgs...); err != nil {
			return nil, err
		}
	}
	counts, _ := json.Marshal(out)
	if _, err := tx.ExecContext(ctx, `INSERT INTO hub_erasures(id, platform, erased_at, applied_at, rows) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`, e.ID, e.Platform, e.ErasedAt, ts.Format(now), string(counts)); err != nil {
		return nil, err
	}
	return out, nil
}

// salesOf are the fan's sales on the platform, by the fan reference ledger's mirror of Hub's sales carries.
func salesOf(ctx context.Context, q ledger.Querier, platform, fanRef string) ([]string, error) {
	return strings1(ctx, q, `SELECT transaction_ref FROM hub_sales WHERE platform = ? AND json_extract(payload, '$.fanRef') = ?
		ORDER BY transaction_ref`, platform, fanRef)
}

// loginRe is the shape of a login: what a fan name must look like to stand for the fan's login.
var loginRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// namesOf are the ways ledger's rows may name the fan, lower-cased — the three Hub's carry-over of ledger's decisions
// matched a fan by: his platform id, "u" + the id (the login of an account that never chose one), and his login. Hub's
// feed carries no login, so the logins are the ones ledger itself holds for him: the fan name of a row with his id or on
// one of his sales, when it has the shape of a login. A login no row of ledger ties to his id is not known to be his.
func namesOf(ctx context.Context, q ledger.Querier, fanRef string, refs, pages []string) ([]string, error) {
	id := strings.ToLower(fanRef)
	out := []string{id, "u" + id}
	seen := map[string]bool{id: true, "u" + id: true}
	refList, refArgs := inList(refs)
	pageList, pageArgs := inList(pages)
	args := append(append(append([]any{}, refArgs...), id, "u"+id), pageArgs...)
	for _, table := range []string{"shift_report_lines", "sale_decisions", "sale_questions", "pot_sales"} {
		found, err := strings1(ctx, q, `SELECT DISTINCT lower(fan_name) FROM `+table+` WHERE fan_name <> '' AND (transaction_ref IN `+refList+`
			OR (lower(fan_ref) IN (?, ?) AND page_label IN `+pageList+`)) ORDER BY 1`, args...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
		for _, n := range found {
			if loginRe.MatchString(n) && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// pagesOf are the platform's pages ledger knows.
func pagesOf(ctx context.Context, q ledger.Querier, platform string) ([]string, error) {
	return strings1(ctx, q, `SELECT page_label FROM hub_pages WHERE platform = ? UNION SELECT DISTINCT page_label FROM hub_sales
		WHERE platform = ? ORDER BY 1`, platform, platform)
}

func strings1(ctx context.Context, q ledger.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// inList is "(?, ?, …)" and its arguments; an empty list is "(”)", which matches no reference.
func inList(xs []string) (string, []any) {
	if len(xs) == 0 {
		return "('')", nil
	}
	args := make([]any, len(xs))
	for i, x := range xs {
		args[i] = x
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?, ", len(xs)), ", ") + ")", args
}

// rewritePayloads sets fields of the payload's lines that match and writes back the payloads that changed.
func rewritePayloads(ctx context.Context, tx *sql.Tx, query string, args []any, update string, match func(map[string]any) bool,
	set map[string]any) (int, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	type doc struct {
		id      int64
		payload string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.id, &d.payload); err != nil {
			rows.Close()
			return 0, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	changed := 0
	for _, d := range docs {
		var p map[string]any
		dec := json.NewDecoder(strings.NewReader(d.payload))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			return changed, fmt.Errorf("%d: %w", d.id, err)
		}
		lines, _ := p["lines"].([]any)
		touched := false
		for _, l := range lines {
			m, ok := l.(map[string]any)
			if !ok || !match(m) {
				continue
			}
			for k, v := range set {
				if cur, has := m[k]; !has || !sameJSON(cur, v) {
					m[k] = v
					touched = true
				}
			}
		}
		if !touched {
			continue
		}
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(p); err != nil {
			return changed, err
		}
		if _, err := tx.ExecContext(ctx, update, strings.TrimSpace(b.String()), d.id); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

func refString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
