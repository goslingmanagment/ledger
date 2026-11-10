package decisions_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
)

// ask is a valid question to the owner about tx-1.
func ask(ref string) decisions.QuestionInput {
	return decisions.QuestionInput{PageLabel: "page-a", FanRef: "fan-100", FanName: "MoonFan", TransactionRef: ref, NetMills: mills(25_000),
		OccurredAt: "2026-11-10T21:15:00+03:00", Question: "80/20 или всё Анне?", AskedOf: "owner", AskedAt: "2026-11-11T10:00:00+03:00",
		SourceLink: "https://t.me/c/1/3", Note: "PPV отправил Борис, купили в смену Анны."}
}

func addQuestion(t *testing.T, c *client.Client, in decisions.QuestionInput) decisions.Question {
	t.Helper()
	var q decisions.Question
	if err := c.Do(context.Background(), http.MethodPost, "/v1/questions", in, &q); err != nil {
		t.Fatalf("%+v: %v", in, err)
	}
	return q
}

func questions(t *testing.T, c *client.Client, query string) string {
	t.Helper()
	var ans decisions.QuestionsAnswer
	if err := c.Do(context.Background(), http.MethodGet, "/v1/questions"+query, nil, &ans); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	var ids []string
	for _, q := range ans.Questions {
		ids = append(ids, strconv.FormatInt(q.ID, 10))
	}
	return strings.Join(ids, ",")
}

func TestAQuestionIsChecked(t *testing.T) {
	_, ctx, k := book(t)
	for _, c := range []struct {
		edit func(*decisions.QuestionInput)
		part string
	}{
		{func(in *decisions.QuestionInput) { in.PageLabel = " " }, "page_label"},
		{func(in *decisions.QuestionInput) { in.FanRef, in.FanName = "", "" }, "нужен фан"},
		{func(in *decisions.QuestionInput) { in.Question = "" }, "нужен вопрос"},
		{func(in *decisions.QuestionInput) { in.Question = strings.Repeat("я", decisions.MaxQuestion+1) }, "можно до 2000"},
		{func(in *decisions.QuestionInput) { in.AskedOf = "" }, "asked_of бывает"},
		{func(in *decisions.QuestionInput) { in.AskedOf = "model" }, "asked_of бывает"},
		{func(in *decisions.QuestionInput) { in.AskedAt = "" }, "когда спросили"},
		{func(in *decisions.QuestionInput) { in.AskedAt = "11.11" }, "asked_at"},
		{func(in *decisions.QuestionInput) { in.OccurredAt = "2026-11-10" }, "occurred_at"},
		{func(in *decisions.QuestionInput) { in.NetMills = mills(2_000_000_000_000) }, "вне разумного"},
		{func(in *decisions.QuestionInput) { in.Note = strings.Repeat("я", decisions.MaxNote+1) }, "можно до 4000"},
	} {
		in := ask("tx-1")
		c.edit(&in)
		err := k.agent.Do(ctx, http.MethodPost, "/v1/questions", in, nil)
		if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), c.part) {
			t.Errorf("want %q, got %v", c.part, err)
		}
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/questions", map[string]any{"page_label": "page-a", "whose": "anna"}, nil); status(err) != http.StatusBadRequest {
		t.Errorf("unknown field: %v", err)
	}
	q := addQuestion(t, k.agent, ask("tx-1"))
	if q.Status != "open" || q.KeyName != "agent" || *q.NetMicros != 25_000_000 || q.AskedAt != "2026-11-11T10:00:00+03:00" || q.Decision != nil {
		t.Fatalf("stored %+v", q)
	}
	// Unknown sale, words only, the chatters' own question.
	q = addQuestion(t, k.agent, decisions.QuestionInput{PageLabel: "page-a", FanName: "fan-x", Question: "решите между собой",
		AskedOf: "chatters", Who: "Анна и Борис", AskedAt: "2026-11-11"})
	if q.TransactionRef != nil || q.NetMicros != nil || q.OccurredAt != nil || q.Who != "Анна и Борис" {
		t.Fatalf("bare %+v", q)
	}
}

func TestTheQuestionFilters(t *testing.T) {
	_, ctx, k := book(t)
	a := ask("tx-1") // MoonFan / fan-100, owner, the 11th
	b := ask("")
	b.FanRef, b.FanName, b.AskedOf, b.AskedAt = "fan-200", "Котик", "chatters", "2026-11-05"
	c := ask("tx-3")
	c.FanRef, c.FanName, c.AskedAt = "fan-300", "ab", "2026-11-20T09:00:00+03:00"
	for _, in := range []decisions.QuestionInput{a, b, c} {
		addQuestion(t, k.agent, in)
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/questions/3/drop", decisions.DropInput{Note: "продажи не было"}, nil); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"":                               "1,2,3",
		"?open=1":                        "1,2",
		"?asked_of=owner":                "1,3",
		"?asked_of=chatters":             "2",
		"?asked_of=owner&open=1":         "1",
		"?fan=moon":                      "1",
		"?fan=MOONFAN":                   "1",
		"?fan=mo":                        "",
		"?fan=fan-200":                   "2",
		"?fan=кот":                       "2",
		"?fan=AB":                        "3",
		"?fan=fan-":                      "1,2,3",
		"?from=2026-11-06&to=2026-11-15": "1", // by when asked
		"?from=2026-11-20":               "3",
	} {
		if got := questions(t, k.hub, q); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	for _, q := range []string{"?from=10.11", "?asked_of=model"} {
		if err := k.reader.Do(ctx, http.MethodGet, "/v1/questions"+q, nil, nil); status(err) != http.StatusBadRequest {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestAnswerAndDrop(t *testing.T) {
	s, ctx, k := book(t)
	q1 := addQuestion(t, k.agent, ask("tx-1"))
	q2 := addQuestion(t, k.agent, ask("tx-2"))
	q3 := addQuestion(t, k.agent, ask(""))
	path := func(id int64, what string) string { return "/v1/questions/" + strconv.FormatInt(id, 10) + "/" + what }

	var d decisions.Decision
	if err := k.owner.Do(ctx, http.MethodPost, "/v1/decisions", owner("tx-1"), &d); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   decisions.AnswerInput
		code int
		part string
	}{
		{decisions.AnswerInput{}, http.StatusUnprocessableEntity, "нужен ответ"},
		{decisions.AnswerInput{DecisionID: mills(999)}, http.StatusUnprocessableEntity, "решения 999 нет"},
	} {
		if err := k.agent.Do(ctx, http.MethodPost, path(q1.ID, "answer"), c.in, nil); status(err) != c.code || !strings.Contains(message(err), c.part) {
			t.Errorf("want %q, got %v", c.part, err)
		}
	}
	if err := k.agent.Do(ctx, http.MethodPost, path(999, "answer"), decisions.AnswerInput{Answer: "x"}, nil); status(err) != http.StatusNotFound {
		t.Errorf("answer of none: %v", err)
	}
	// By a decision.
	var got decisions.Question
	if err := k.agent.Do(ctx, http.MethodPost, path(q1.ID, "answer"), decisions.AnswerInput{DecisionID: &d.ID}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "answered" || *got.DecisionID != d.ID || got.ClosedAt == nil || got.Answer != nil ||
		got.Decision == nil || got.Decision.Kind != "owner" || got.Decision.SaidBy != "владелец" || got.Decision.Shares[0].Person != "anna" {
		t.Fatalf("answered %+v %+v", got, got.Decision)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path(q1.ID, "answer"), decisions.AnswerInput{Answer: "ещё"}, nil); status(err) != http.StatusUnprocessableEntity ||
		!strings.Contains(message(err), "уже закрыт") {
		t.Errorf("answer twice: %v", err)
	}
	// In words.
	if err := k.agent.Do(ctx, http.MethodPost, path(q2.ID, "answer"), decisions.AnswerInput{Answer: "Анне, как обычно"}, &got); err != nil ||
		got.Status != "answered" || got.DecisionID != nil || *got.Answer != "Анне, как обычно" {
		t.Fatalf("in words %+v %v", got, err)
	}
	// Dropped.
	if err := k.agent.Do(ctx, http.MethodPost, path(q3.ID, "drop"), decisions.DropInput{}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("drop without a note: %v", err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path(q3.ID, "drop"), decisions.DropInput{Note: "продажи не было"}, &got); err != nil ||
		got.Status != "dropped" || *got.ClosedNote != "продажи не было" || got.ClosedAt == nil {
		t.Fatalf("dropped %+v %v", got, err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path(q3.ID, "drop"), decisions.DropInput{Note: "ещё"}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("drop twice: %v", err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path(999, "drop"), decisions.DropInput{Note: "x"}, nil); status(err) != http.StatusNotFound {
		t.Errorf("drop of none: %v", err)
	}
	// A cancelled decision answers nothing.
	q4 := addQuestion(t, k.agent, ask(""))
	if _, err := decisions.Cancel(ctx, s, d.ID, decisions.CancelInput{Note: "нет"}, now); err != nil {
		t.Fatal(err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path(q4.ID, "answer"), decisions.AnswerInput{DecisionID: &d.ID}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Errorf("by a cancelled decision: %v", err)
	}
}

func count(t *testing.T, s *db.Store) int {
	t.Helper()
	var n int
	if err := s.RO.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sale_decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestADecisionAnswersAQuestionInOneTransaction(t *testing.T) {
	s, ctx, k := book(t)
	q := addQuestion(t, k.agent, ask("tx-1"))
	in := owner("tx-1")
	in.Answers = mills(999)
	err := k.owner.Do(ctx, http.MethodPost, "/v1/decisions", in, nil)
	if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), "вопроса 999 нет") || count(t, s) != 0 {
		t.Fatalf("a missing question: %v, %d decisions", err, count(t, s))
	}
	in.Answers = &q.ID
	var d decisions.Decision
	if err := k.owner.Do(ctx, http.MethodPost, "/v1/decisions", in, &d); err != nil {
		t.Fatal(err)
	}
	got, err := decisions.GetQuestion(ctx, s.RO, q.ID)
	if err != nil || got.Status != "answered" || got.DecisionID == nil || *got.DecisionID != d.ID || got.Decision.ID != d.ID {
		t.Fatalf("closed %+v %v", got, err)
	}
	// A closed question refuses the decision; nothing is written.
	again := owner("tx-2")
	again.Answers = &q.ID
	err = k.agent.Do(ctx, http.MethodPost, "/v1/decisions", again, nil)
	if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), "уже закрыт") || count(t, s) != 1 {
		t.Fatalf("a closed question: %v, %d decisions", err, count(t, s))
	}
	dropped := addQuestion(t, k.agent, ask("tx-3"))
	if _, err := decisions.DropQuestion(ctx, s, dropped.ID, decisions.DropInput{Note: "отпал"}, now); err != nil {
		t.Fatal(err)
	}
	again.Answers = &dropped.ID
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", again, nil); status(err) != http.StatusUnprocessableEntity || count(t, s) != 1 {
		t.Fatalf("a dropped question: %v, %d decisions", err, count(t, s))
	}
}

func TestTheQuestionScopes(t *testing.T) {
	_, ctx, k := book(t)
	// The agent's key adds, reads, answers and drops.
	q1 := addQuestion(t, k.agent, ask("tx-1"))
	q2 := addQuestion(t, k.agent, ask("tx-2"))
	if questions(t, k.agent, "") != "1,2" {
		t.Fatal("agent reads")
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/questions/"+strconv.FormatInt(q1.ID, 10)+"/answer", decisions.AnswerInput{Answer: "Анне"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/questions/"+strconv.FormatInt(q2.ID, 10)+"/drop", decisions.DropInput{Note: "отпал"}, nil); err != nil {
		t.Fatal(err)
	}
	// The owner writes; Hub's and the reader's keys only read.
	addQuestion(t, k.owner, ask("tx-3"))
	if err := k.owner.Do(ctx, http.MethodGet, "/v1/questions", nil, nil); status(err) != http.StatusForbidden {
		t.Errorf("decide alone reads questions: %v", err)
	}
	for name, c := range map[string]*client.Client{"reader": k.reader, "hub": k.hub} {
		if got := questions(t, c, "?open=1"); got != "3" {
			t.Errorf("%s reads: %s", name, got)
		}
		for _, path := range []string{"/v1/questions", "/v1/questions/3/answer", "/v1/questions/3/drop"} {
			if err := c.Do(ctx, http.MethodPost, path, map[string]any{}, nil); status(err) != http.StatusForbidden {
				t.Errorf("%s POST %s: %v", name, path, err)
			}
		}
	}
}

func TestTheQuestionsMarkdown(t *testing.T) {
	s, ctx, k := book(t)
	addQuestion(t, k.agent, ask("tx-1"))
	addQuestion(t, k.agent, decisions.QuestionInput{PageLabel: "page-a", FanName: "fan-x", Question: "Кто из вас продал набор",
		AskedOf: "chatters", Who: "Анна и Борис", AskedAt: "2026-11-11", Note: "Купил\nпосле двух смен."})
	closed := addQuestion(t, k.agent, ask("tx-2"))
	if _, err := decisions.DropQuestion(ctx, s, closed.ID, decisions.DropInput{Note: "отпал"}, now); err != nil {
		t.Fatal(err)
	}
	list, err := decisions.ListQuestions(ctx, s.RO, decisions.QuestionFilter{Open: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "## Ждёт Диму\n\n" +
		"- **MoonFan / fan-100** (page-a). Нетто 25.00. 80/20 или всё Анне? PPV отправил Борис, купили в смену Анны. Спросили: 2026-11-11T10:00:00+03:00, https://t.me/c/1/3.\n" +
		"\n## Решают сами чаттеры\n\n" +
		"- **fan-x** (page-a). Кто из вас продал набор. Купил после двух смен. Договориться: Анна и Борис. Спросили: 2026-11-11.\n"
	if got := decisions.QuestionsMarkdown(list); got != want {
		t.Fatalf("markdown:\n%s\nwant\n%s", got, want)
	}
	if got := decisions.QuestionsMarkdown(list[:1]); strings.Contains(got, "чаттеры") {
		t.Fatalf("an empty group is shown: %s", got)
	}
}
