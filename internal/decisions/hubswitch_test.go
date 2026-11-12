package decisions_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/accruals"
	"github.com/goslingmanagment/ledger/internal/decisions"
)

// From settings.sale_decisions_hub_from Hub is the only writer of «чья продажа»: every write of a decision, a question or
// the owner's word on a nobody's sale refuses here with one line naming Hub's command — over HTTP and beside the
// database — and what ledger has stays readable as history.
func TestFromTheSwitchOnlyHubWritesWhoseASaleIs(t *testing.T) {
	s, ctx, k := book(t)
	var d decisions.Decision
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", owner("tx-1"), &d); err != nil {
		t.Fatal(err)
	}
	q := addQuestion(t, k.agent, ask("tx-2"))
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-01", now); err != nil {
		t.Fatal(err)
	}
	id, qid := strconv.FormatInt(d.ID, 10), strconv.FormatInt(q.ID, 10)
	for _, c := range []struct {
		path, hub string
		body      any
	}{
		{"/v1/decisions", "hub sale decide", owner("tx-3")},
		{"/v1/decisions/" + id + "/applied", "hub sale decide", decisions.AppliedInput{Person: "anna", ReportDate: "2026-11-10"}},
		{"/v1/decisions/" + id + "/cancel", "hub sale decide", decisions.CancelInput{Note: "передумали"}},
		{"/v1/questions", "hub sale ask", ask("tx-4")},
		{"/v1/questions/" + qid + "/answer", "hub sale answer", decisions.AnswerInput{Answer: "всё Анне"}},
		{"/v1/questions/" + qid + "/drop", "hub sale ask", decisions.DropInput{Note: "отпал"}},
	} {
		err := k.owner.Do(ctx, http.MethodPost, c.path, c.body, nil)
		if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), c.hub) || !strings.Contains(message(err), "с 2026-11-01") {
			t.Errorf("%s: %d %q", c.path, status(err), message(err))
		}
	}
	// The owner's word on a nobody's sale, too.
	err := k.owner.Do(ctx, http.MethodPost, "/v1/accruals/claim", accruals.ClaimInput{Ref: "tx-5", Person: "anna", Note: "слово владельца"}, nil)
	if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), "hub sale decide") {
		t.Errorf("claim: %d %q", status(err), message(err))
	}
	// Beside the database (the CLI with LEDGER_DB): the same refusal.
	if _, err := decisions.Add(ctx, s, owner("tx-6"), "local", now); err == nil || !strings.Contains(err.Error(), "hub sale decide") {
		t.Errorf("direct add: %v", err)
	}
	// Nothing was written; the lists still read the history.
	if got := list(t, k.hub, ""); len(got) != 1 || got[0] != d.ID {
		t.Errorf("decisions after the switch: %v", got)
	}
	if got := questions(t, k.reader, ""); !strings.Contains(got, qid) {
		t.Errorf("questions after the switch: %s", got)
	}
	// Off: ledger writes again.
	if err := decisions.SetHubFrom(ctx, s.RW, "", now); err != nil {
		t.Fatal(err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", owner("tx-3"), nil); err != nil {
		t.Errorf("after off: %v", err)
	}
}
