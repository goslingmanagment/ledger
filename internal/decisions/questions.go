package decisions

// Open questions about whose a sale is: the team's chat agent cannot tell and asks the owner («80/20 или всё
// одному?») or the chatters themselves («решите между собой»). Until someone answers, the question is a record here, so
// Hub can show the owner what waits for his word; the answer closes it with a decision or in words. Nothing here posts
// or accrues.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Whom a question is asked of.
const (
	AskedOwner    = "owner"    // waits for the owner's word
	AskedChatters = "chatters" // the chatters settle it between themselves
)

// Statuses of a question.
const (
	QuestionOpen     = "open"
	QuestionAnswered = "answered"
	QuestionDropped  = "dropped"
)

// MaxQuestion bounds the question in words, in characters.
const MaxQuestion = 2000

// ErrNoQuestion is a question id with no row.
var ErrNoQuestion = errors.New("no such question")

// QuestionInput is the body of POST /v1/questions. The amount is integer mills, as in POST /v1/decisions.
type QuestionInput struct {
	PageLabel      string `json:"page_label"`
	FanRef         string `json:"fan_ref,omitempty"`
	FanName        string `json:"fan_name,omitempty"`
	TransactionRef string `json:"transaction_ref,omitempty"`
	NetMills       *int64 `json:"net_mills,omitempty"`
	OccurredAt     string `json:"occurred_at,omitempty"`
	Question       string `json:"question"`
	AskedOf        string `json:"asked_of"`
	Who            string `json:"who,omitempty"`
	AskedAt        string `json:"asked_at"`
	SourceLink     string `json:"source_link,omitempty"`
	Note           string `json:"note,omitempty"`
}

// QuestionDecision is the short form of the decision that answered a question.
type QuestionDecision struct {
	ID     int64   `json:"id"`
	Kind   string  `json:"kind"`
	Shares []Share `json:"shares"`
	SaidBy string  `json:"said_by"`
}

// Question is a stored question as the API serves it.
type Question struct {
	ID             int64             `json:"id"`
	PageLabel      string            `json:"page_label"`
	FanRef         string            `json:"fan_ref"`
	FanName        string            `json:"fan_name"`
	TransactionRef *string           `json:"transaction_ref"`
	NetMicros      *int64            `json:"net_micros"`
	OccurredAt     *string           `json:"occurred_at"`
	Question       string            `json:"question"`
	AskedOf        string            `json:"asked_of"`
	Who            string            `json:"who"`
	AskedAt        string            `json:"asked_at"`
	SourceLink     string            `json:"source_link"`
	Note           string            `json:"note"`
	Status         string            `json:"status"`
	DecisionID     *int64            `json:"decision_id"`
	Answer         *string           `json:"answer"`
	ClosedAt       *string           `json:"closed_at"`
	ClosedNote     *string           `json:"closed_note"`
	CreatedAt      string            `json:"created_at"`
	KeyName        string            `json:"key_name"`
	Decision       *QuestionDecision `json:"decision,omitempty"`
}

func normalizeQuestion(in QuestionInput) (QuestionInput, error) {
	in.PageLabel, in.FanRef, in.FanName = strings.TrimSpace(in.PageLabel), strings.TrimSpace(in.FanRef), strings.TrimSpace(in.FanName)
	in.TransactionRef, in.Question, in.AskedOf = strings.TrimSpace(in.TransactionRef), strings.TrimSpace(in.Question), strings.TrimSpace(in.AskedOf)
	in.Who, in.SourceLink, in.Note = strings.TrimSpace(in.Who), strings.TrimSpace(in.SourceLink), strings.TrimSpace(in.Note)
	if in.PageLabel == "" {
		return in, refuse("нужна страница хаба (page_label)")
	}
	if in.FanRef == "" && in.FanName == "" {
		return in, refuse("нужен фан: fan_ref или fan_name")
	}
	if in.NetMills != nil {
		if _, err := CheckMills("net_mills", *in.NetMills); err != nil {
			return in, err
		}
	}
	var err error
	if in.OccurredAt != "" {
		if in.OccurredAt, err = NormTime("occurred_at", in.OccurredAt); err != nil {
			return in, err
		}
	}
	if in.Question == "" {
		return in, refuse("нужен вопрос словами (question)")
	}
	if n := utf8.RuneCountInString(in.Question); n > MaxQuestion {
		return in, refuse("question: %d знаков, можно до %d", n, MaxQuestion)
	}
	if in.AskedOf != AskedOwner && in.AskedOf != AskedChatters {
		return in, refuse("asked_of бывает owner (ждёт владельца) или chatters (решают сами чаттеры), а не %q", in.AskedOf)
	}
	if strings.TrimSpace(in.AskedAt) == "" {
		return in, refuse("нужно, когда спросили (asked_at): ГГГГ-ММ-ДД или время")
	}
	if in.AskedAt, err = NormDateOrTime("asked_at", in.AskedAt); err != nil {
		return in, err
	}
	if n := utf8.RuneCountInString(in.Note); n > MaxNote {
		return in, refuse("note: %d знаков, можно до %d", n, MaxNote)
	}
	return in, nil
}

// AddQuestion checks a question and writes it open.
func AddQuestion(ctx context.Context, store *db.Store, in QuestionInput, keyName string, now time.Time) (Question, error) {
	in, err := normalizeQuestion(in)
	if err != nil {
		return Question{}, err
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, err
	}
	defer tx.Rollback()
	var net any
	if in.NetMills != nil {
		net = int64(money.FromMills(*in.NetMills))
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO sale_questions(page_label, fan_ref, fan_name, transaction_ref, net_micros, occurred_at,
		question, asked_of, who, asked_at, source_link, note, status, created_at, key_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.PageLabel, in.FanRef, in.FanName, nullStr(in.TransactionRef), net, nullStr(in.OccurredAt), in.Question, in.AskedOf, in.Who,
		in.AskedAt, in.SourceLink, in.Note, QuestionOpen, ts.Format(now), keyName)
	if err != nil {
		return Question{}, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return Question{}, err
	}
	q, err := getQuestion(ctx, tx, id)
	if err != nil {
		return Question{}, err
	}
	return q, tx.Commit()
}

// AnswerInput is the body of POST /v1/questions/{id}/answer: the decision that answered, or the answer in words, or both.
type AnswerInput struct {
	DecisionID *int64 `json:"decision_id,omitempty"`
	Answer     string `json:"answer,omitempty"`
}

// AnswerQuestion closes an open question as answered.
func AnswerQuestion(ctx context.Context, store *db.Store, id int64, in AnswerInput, now time.Time) (Question, error) {
	in.Answer = strings.TrimSpace(in.Answer)
	if in.DecisionID == nil && in.Answer == "" {
		return Question{}, refuse("нужен ответ: decision_id (решение) или answer (словами)")
	}
	if n := utf8.RuneCountInString(in.Answer); n > MaxNote {
		return Question{}, refuse("answer: %d знаков, можно до %d", n, MaxNote)
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, err
	}
	defer tx.Rollback()
	if _, err := getQuestion(ctx, tx, id); err != nil {
		return Question{}, err
	}
	if in.DecisionID != nil {
		d, err := get(ctx, tx, *in.DecisionID)
		if errors.Is(err, ErrNotFound) {
			return Question{}, refuse("decision_id: решения %d нет", *in.DecisionID)
		}
		if err != nil {
			return Question{}, err
		}
		if d.Status == StatusCancelled {
			return Question{}, refuse("decision_id: решение %d отменено", d.ID)
		}
	}
	if err := closeQuestion(ctx, tx, id, in.DecisionID, in.Answer, now); err != nil {
		return Question{}, err
	}
	q, err := getQuestion(ctx, tx, id)
	if err != nil {
		return Question{}, err
	}
	return q, tx.Commit()
}

// closeQuestion marks an open question answered; a missing or closed one is a refusal.
func closeQuestion(ctx context.Context, tx *sql.Tx, id int64, decision *int64, answer string, now time.Time) error {
	q, err := getQuestion(ctx, tx, id)
	if errors.Is(err, ErrNoQuestion) {
		return refuse("вопроса %d нет", id)
	}
	if err != nil {
		return err
	}
	if q.Status != QuestionOpen {
		return refuse("вопрос %d уже закрыт (%s)", id, q.Status)
	}
	_, err = tx.ExecContext(ctx, `UPDATE sale_questions SET status = ?, decision_id = ?, answer = ?, closed_at = ? WHERE id = ?`,
		QuestionAnswered, decision, nullStr(answer), ts.Format(now), id)
	return err
}

// DropInput is the body of POST /v1/questions/{id}/drop.
type DropInput struct {
	Note string `json:"note"`
}

// DropQuestion closes an open question that went away: the sale never happened, the chatters settled it and nothing
// needs recording.
func DropQuestion(ctx context.Context, store *db.Store, id int64, in DropInput, now time.Time) (Question, error) {
	note := strings.TrimSpace(in.Note)
	if note == "" {
		return Question{}, refuse("нужна заметка, почему вопрос отпал (note)")
	}
	if n := utf8.RuneCountInString(note); n > MaxNote {
		return Question{}, refuse("note: %d знаков, можно до %d", n, MaxNote)
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, err
	}
	defer tx.Rollback()
	q, err := getQuestion(ctx, tx, id)
	if err != nil {
		return Question{}, err
	}
	if q.Status != QuestionOpen {
		return Question{}, refuse("вопрос %d уже закрыт (%s)", id, q.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sale_questions SET status = ?, closed_at = ?, closed_note = ? WHERE id = ?`,
		QuestionDropped, ts.Format(now), note, id); err != nil {
		return Question{}, err
	}
	if q, err = getQuestion(ctx, tx, id); err != nil {
		return Question{}, err
	}
	return q, tx.Commit()
}

const questionColumns = `id, page_label, fan_ref, fan_name, transaction_ref, net_micros, occurred_at, question, asked_of, who, asked_at,
	source_link, note, status, decision_id, answer, closed_at, closed_note, created_at, key_name`

func scanQuestion(row interface{ Scan(...any) error }) (Question, error) {
	var q Question
	var ref, at, ans, cat, cn sql.NullString
	var net, dec sql.NullInt64
	if err := row.Scan(&q.ID, &q.PageLabel, &q.FanRef, &q.FanName, &ref, &net, &at, &q.Question, &q.AskedOf, &q.Who, &q.AskedAt,
		&q.SourceLink, &q.Note, &q.Status, &dec, &ans, &cat, &cn, &q.CreatedAt, &q.KeyName); err != nil {
		return Question{}, err
	}
	q.TransactionRef, q.OccurredAt, q.Answer, q.ClosedAt, q.ClosedNote = ptr(ref), ptr(at), ptr(ans), ptr(cat), ptr(cn)
	if net.Valid {
		q.NetMicros = &net.Int64
	}
	if dec.Valid {
		q.DecisionID = &dec.Int64
	}
	return q, nil
}

// withDecision fills the short decision of an answered question.
func withDecision(ctx context.Context, dbq ledger.Querier, q *Question) error {
	if q.DecisionID == nil {
		return nil
	}
	d, err := get(ctx, dbq, *q.DecisionID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	q.Decision = &QuestionDecision{ID: d.ID, Kind: d.Kind, Shares: d.Shares, SaidBy: d.SaidBy}
	return nil
}

func getQuestion(ctx context.Context, dbq ledger.Querier, id int64) (Question, error) {
	q, err := scanQuestion(dbq.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM sale_questions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, ErrNoQuestion
	}
	if err != nil {
		return Question{}, err
	}
	return q, withDecision(ctx, dbq, &q)
}

// GetQuestion reads one question.
func GetQuestion(ctx context.Context, dbq ledger.Querier, id int64) (Question, error) {
	return getQuestion(ctx, dbq, id)
}

// QuestionFilter narrows ListQuestions.
type QuestionFilter struct {
	Open     bool   // status open only
	AskedOf  string // owner or chatters; empty — both
	Fan      string // as Filter.Fan
	From, To string // the date asked; inclusive
}

// ListQuestions reads the questions by the filter, the oldest first.
func ListQuestions(ctx context.Context, dbq ledger.Querier, f QuestionFilter) ([]Question, error) {
	where := []string{"1 = 1"}
	var args []any
	if f.Open {
		where = append(where, "status = ?")
		args = append(args, QuestionOpen)
	}
	if f.AskedOf != "" {
		where = append(where, "asked_of = ?")
		args = append(args, f.AskedOf)
	}
	if f.From != "" {
		where = append(where, "substr(asked_at, 1, 10) >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "substr(asked_at, 1, 10) <= ?")
		args = append(args, f.To)
	}
	rows, err := dbq.QueryContext(ctx, `SELECT `+questionColumns+` FROM sale_questions WHERE `+strings.Join(where, " AND ")+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	out := []Question{}
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if MatchFan(f.Fan, q.FanRef, q.FanName) {
			out = append(out, q)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := withDecision(ctx, dbq, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// QuestionsMarkdown renders the open questions as the agent's memory file has them, grouped by whom they wait for:
//
//	## Ждёт Диму
//	- **<fan name> / <fan ref>** (<page>). <net when known>. <question> <note>. Спросили: <asked_at>, <link>.
//
//	## Решают сами чаттеры
//	- … Договориться: <who>. Спросили: …
func QuestionsMarkdown(list []Question) string {
	var b strings.Builder
	for _, g := range []struct{ of, title string }{{AskedOwner, "Ждёт Диму"}, {AskedChatters, "Решают сами чаттеры"}} {
		var lines []string
		for _, q := range list {
			if q.AskedOf == g.of && q.Status == QuestionOpen {
				lines = append(lines, questionLine(q))
			}
		}
		if len(lines) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## %s\n\n", g.title)
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func questionLine(q Question) string {
	fan := q.FanName
	switch {
	case fan == "":
		fan = q.FanRef
	case q.FanRef != "":
		fan += " / " + q.FanRef
	}
	parts := []string{fmt.Sprintf("- **%s** (%s).", fan, q.PageLabel)}
	if q.NetMicros != nil {
		parts = append(parts, "Нетто "+money.Micros(*q.NetMicros).Cents()+".")
	}
	parts = append(parts, sentence(q.Question))
	if q.Note != "" {
		parts = append(parts, sentence(q.Note))
	}
	if q.Who != "" {
		parts = append(parts, sentence("Договориться: "+q.Who))
	}
	asked := []string{q.AskedAt}
	if q.SourceLink != "" {
		asked = append(asked, q.SourceLink)
	}
	parts = append(parts, "Спросили: "+strings.Join(asked, ", ")+".")
	return strings.Join(parts, " ")
}

// sentence folds the text into one line and ends it with a stop unless it already ends with one.
func sentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, stop := range []string{".", "?", "!", "…"} {
		if s == "" || strings.HasSuffix(s, stop) {
			return s
		}
	}
	return s + "."
}
