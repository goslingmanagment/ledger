package checks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// payTermsCheck is the name of PayTerms.
const payTermsCheck = "условия оплаты: реестр хаба и вкладки"

// payTermsStale is how old the last read of the registry's terms may be once they price the days: the job reads every 15 minutes.
const payTermsStale = 2 * time.Hour

// PayTerms compares the pay terms of Hub's people registry with the tabs' headers (payroll.CompareTerms). While
// settings.payroll_terms_hub_from is empty or later than today the headers price every day: the check never fails and
// lists where the two differ, a person in one source only the same way. From that day on the registry prices the days
// and the headers are a remnant it no longer reads: the check fails (warn) for every person whose day from it kept the
// header for lack of a registry term or who has a tab and no term for today, and when the registry is not read.
func PayTerms(ctx context.Context, e Env) Result {
	const name = payTermsCheck
	today := ts.Date(e.Now)
	d, err := payroll.CompareTerms(ctx, e.Q, today)
	if err != nil {
		return fail(name, "warn", err.Error(), nil)
	}
	on := d.From != "" && d.From <= today
	readProblem := ""
	switch st := d.Read; {
	case st == nil:
		readProblem = "условия реестра хаба ещё не читались: джоб " + payroll.HubTermsJob + " или ledger hub terms-sync"
	case st.NoTerms, st.Absent:
		readProblem = st.Error
	case !st.OK:
		readProblem = fmt.Sprintf("реестр людей хаба не прочитан %s: %s; последнее удачное чтение — %s", st.At, st.Error, orNever(st.LastOKAt))
	case on:
		if t, err := ts.Parse(st.LastOKAt); err != nil || e.Now.Sub(t) > payTermsStale {
			readProblem = "условия реестра не читались больше двух часов: последнее удачное чтение — " + orNever(st.LastOKAt)
		}
	}
	if !on {
		if d.Read == nil || d.Read.NoTerms || d.Read.Absent {
			return pass(name, readProblem)
		}
		lead := "в тени, условия начисляет шапка вкладки"
		if d.From != "" {
			lead = "условия начисляет шапка вкладки до " + d.From
		}
		if readProblem != "" {
			lead += "; " + readProblem
		}
		if d.Differ > 0 {
			return Result{Name: name, OK: true, Severity: "info", Details: d.Rows,
				Summary: fmt.Sprintf("%s: условия в реестре и во вкладке расходятся у %d (реестр ≠ вкладка): %s", lead, d.Differ, d.DifferText())}
		}
		return pass(name, fmt.Sprintf("%s: условия в реестре и во вкладках совпадают у %d", lead, d.Equal))
	}

	var bad []string
	for _, f := range d.Fallback {
		days := f.First
		if f.Last != f.First {
			days += "…" + f.Last
		}
		bad = append(bad, fmt.Sprintf("%s (%s) %s, дней %d — по шапке вкладки", f.Person, f.Model, days, f.Days))
	}
	for _, r := range d.Rows {
		if r.State == payroll.TermsTabOnly {
			bad = append(bad, fmt.Sprintf("%s (%s) на %s в реестре нет условия", r.Person, r.Model, today))
		}
	}
	if readProblem != "" {
		bad = append([]string{readProblem}, bad...)
	}
	if len(bad) > 0 {
		return fail(name, "warn", fmt.Sprintf("условия начисляет реестр хаба с %s, но: %s — условие на день ставит владелец в карточке человека (хаб, «График и условия»)",
			d.From, strings.Join(bad, "; ")), d)
	}
	tabs := 0
	for _, r := range d.Rows {
		if r.Tab != "" {
			tabs++
		}
	}
	return pass(name, fmt.Sprintf("условия начисляет реестр хаба с %s: у всех %d вкладок работающих людей есть условие на %s, шапки вкладок не читаются",
		d.From, tabs, today))
}

func orNever(at string) string {
	if at == "" {
		return "никогда"
	}
	return at
}
