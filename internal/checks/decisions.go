package checks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/pots"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// hubDecisionsCheck is the name of HubDecisions.
const hubDecisionsCheck = "решения хаба о продажах доходят"

// hubDecisionsStale is how old the last good read of Hub's feed of decisions may be: the job reads it every 5 minutes.
const hubDecisionsStale = time.Hour

// HubDecisions: Hub's decisions about whose a sale is reach ledger and what follows them is done. While
// settings.sale_decisions_hub_from is empty nothing reads the decisions: the check never fails and says what the mirror
// holds. From the day it is set, the check fails (warn) when the feed is not read (the job hub-decisions), while Hub
// holds a change back from ledger's key (a decision on a page outside the key's pages: ledger does not see it), and for
// every decision of kind pot that cannot become a purchase — an unknown or closed pot, a page or a day outside the pot,
// a sale a report still counts in a person's page, a sale already in another pot by hand, a sale that never reached
// ledger's mirror of Hub's sales.
func HubDecisions(ctx context.Context, e Env) Result {
	const name = hubDecisionsCheck
	from, err := decisions.HubFrom(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	st, err := decisions.ReadHubStatus(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	held, err := decisions.HubHeld(ctx, e.Q)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	plan, err := pots.PlanHub(ctx, e.Q, e.Now)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	// The read of the feed: what is wrong with it ("" — nothing), and how it stands.
	readProblem, read := "", ""
	switch {
	case st == nil:
		readProblem = "лента решений хаба ещё не читалась: джоб " + decisions.HubJob + " или ledger hub decisions-sync"
	case !st.OK:
		readProblem = fmt.Sprintf("лента решений хаба не прочитана %s: %s; последнее удачное чтение — %s", st.At, st.Error, orNever(st.LastOKAt))
	case st.Absent:
		readProblem = st.Error
	default:
		t, err := ts.Parse(st.LastOKAt)
		if err != nil || e.Now.Sub(t) > hubDecisionsStale {
			readProblem = "лента решений хаба не читалась больше часа: последнее удачное чтение — " + orNever(st.LastOKAt)
			break
		}
		cursor := st.Next
		if cursor == "" {
			cursor = "с начала"
		}
		read = fmt.Sprintf("лента решений хаба прочитана %s (%d мин назад), курсор %s", st.LastOKAt, int(e.Now.Sub(t).Minutes()), cursor)
	}
	var heldParts []string
	for _, h := range held {
		page := "страница не названа"
		if h.PageLabel != nil && *h.PageLabel != "" {
			page = *h.PageLabel
		}
		heldParts = append(heldParts, fmt.Sprintf("решение %d (%s)", h.DecisionID, page))
	}
	if from == "" {
		// In shadow: nothing reads the decisions, so nothing of this is a failure.
		line := "в тени: решения хаба на учёт не влияют (settings.sale_decisions_hub_from пуст, ledger decision hub-from)"
		if readProblem != "" {
			return pass(name, line+"; "+readProblem)
		}
		line += "; " + read
		if len(heldParts) > 0 {
			line += fmt.Sprintf("; хаб придержал %d изменений — страницы вне ключа ledger: %s", len(heldParts), strings.Join(heldParts, ", "))
		}
		return pass(name, line)
	}
	var failures []string
	details := map[string]any{}
	if readProblem != "" {
		failures = append(failures, readProblem)
		details["read"] = st
	}
	if len(heldParts) > 0 {
		failures = append(failures, fmt.Sprintf("%d изменений решений хаб придержал: у ключа ledger в хабе нет их страниц, ledger этих решений "+
			"не видит — %s; дайте ключу эти страницы в хабе (решение без страницы привяжите к продаже), изменения придут сами",
			len(heldParts), strings.Join(heldParts, ", ")))
		details["held"] = held
	}
	if len(plan.Problems) > 0 {
		parts := make([]string, 0, len(plan.Problems))
		for _, p := range plan.Problems {
			parts = append(parts, fmt.Sprintf("решение %d: %s", p.DecisionID, p.Why))
		}
		failures = append(failures, fmt.Sprintf("%d решений хаба «в общак» не стали покупками общака: %s", len(plan.Problems), strings.Join(parts, " | ")))
		details["problems"] = plan.Problems
	}
	if len(failures) > 0 {
		return fail(name, "warn", fmt.Sprintf("решения хаба читаются с %s, но: %s", from, strings.Join(failures, " || ")), details)
	}
	line := fmt.Sprintf("решения хаба читаются у продаж с %s: %s; решений «в общак» с покупкой — %d", from, read, plan.Kept)
	if n := len(plan.Add) + len(plan.Remove); n > 0 {
		line += fmt.Sprintf(", ждут прохода джоба — %d", n)
	}
	if len(plan.Waiting) > 0 {
		line += fmt.Sprintf(", ждут фактов продажи — %d", len(plan.Waiting))
	}
	return pass(name, line)
}
