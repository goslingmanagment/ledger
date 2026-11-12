package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/accruals"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The pay terms of Hub's people registry (ecosystem H3): read them now, compare them with the tabs' headers, and the
// day from which the accruals take them.

func init() {
	Command(Cmd{Name: "hub terms-sync", Run: hubTermsSync,
		Summary: "прочитать людей и условия оплаты из реестра хаба сейчас, как джоб hub-terms (рядом с базой)"})
	Command(Cmd{Name: "payroll terms", Run: payrollTerms,
		Summary: "diff — условия оплаты реестра хаба против шапок вкладок на день (рядом с базой): diff [--day ГГГГ-ММ-ДД] [--json]"})
	Command(Cmd{Name: "payroll terms-from", Run: payrollTermsFrom,
		Summary: "начисления берут условия из реестра хаба с дня (рядом с базой): [ГГГГ-ММ-ДД | --off]; без аргумента — показать; затем пересборка"})
}

func hubTermsSync(ctx context.Context, args []string) error {
	if _, err := ParseArgs(Flags("hub terms-sync"), args); err != nil {
		return err
	}
	cfg, err := hub.ConfigFromEnv()
	if err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	st, err := payroll.SyncHubTerms(ctx, store, hub.NewClient(cfg, nil), time.Now())
	if perr := printJSON(st); perr != nil {
		return perr
	}
	if err != nil {
		return Exit{Code: 3, Msg: "реестр людей хаба не прочитан: " + err.Error()}
	}
	return nil
}

// payrollTerms is `ledger payroll terms diff`: the comparison of the registry's terms with the tabs' headers.
func payrollTerms(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "diff" {
		return Exit{Code: 2, Msg: "ledger payroll terms diff [--day ГГГГ-ММ-ДД] [--json]"}
	}
	args = args[1:]
	fs := Flags("payroll terms diff")
	day := fs.String("day", ts.Date(ts.Now()), "день сравнения ГГГГ-ММ-ДД; по умолчанию сегодня (МСК)")
	asJSON := fs.Bool("json", false, "ответ как есть")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	if t, err := time.Parse(ts.DateLayout, *day); err != nil || t.Format(ts.DateLayout) != *day {
		return Exit{Code: 2, Msg: "--day: нужна ГГГГ-ММ-ДД"}
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	d, err := payroll.CompareTerms(ctx, store.RO, *day)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(d)
	}
	out := bufio.NewWriter(os.Stdout)
	printTermsDiff(out, d)
	return out.Flush()
}

func printTermsDiff(out *bufio.Writer, d payroll.TermsDiff) {
	from := "пусто — начисляет шапка вкладки"
	if d.From != "" {
		from = "с " + d.From + " начисляет реестр"
	}
	fmt.Fprintf(out, "Условия оплаты на %s: реестр хаба против шапок вкладок (%s %s)\n", d.Day, payroll.SettingTermsHubFrom, from)
	switch st := d.Read; {
	case st == nil:
		fmt.Fprintln(out, "Реестр ещё не читался: джоб hub-terms или ledger hub terms-sync.")
	case st.NoTerms || st.Absent || !st.OK:
		fmt.Fprintf(out, "Последнее чтение реестра %s: %s; удачное — %s.\n", st.At, st.Error, orDashText(st.LastOKAt))
	default:
		fmt.Fprintf(out, "Реестр прочитан %s: людей %d, условий %d.\n", st.LastOKAt, st.People, st.Terms)
	}
	states := map[string]string{payroll.TermsEqual: "совпадают", payroll.TermsDiffer: "расходятся",
		payroll.TermsTabOnly: "только вкладка", payroll.TermsHubOnly: "только реестр"}
	rows := [][]string{}
	for _, r := range d.Rows {
		hubSide, tabSide := "—", "—"
		if r.HubShareBP != nil {
			hubSide = fmt.Sprintf("%s, фикс %s (с %s)", payroll.BasisPercent(*r.HubShareBP), payroll.Dollars(*r.HubFixMicros), r.HubValidFrom)
		}
		if r.TabFixMicros != nil {
			share := "нет «Ставка %»"
			if r.TabRate != nil {
				share = payroll.RatePercent(*r.TabRate)
			}
			tabSide = fmt.Sprintf("%s, фикс %s", share, payroll.Dollars(*r.TabFixMicros))
		}
		rows = append(rows, []string{r.Person, r.Model, orDashText(r.Tab), hubSide, tabSide, states[r.State], strings.Join(r.Differences, ", ")})
	}
	if len(rows) > 0 {
		writeTable(indent{out}, []string{"человек", "модель", "вкладка", "реестр", "шапка вкладки", "итог", "разница"}, rows,
			[]bool{false, false, false, false, false, false, false})
	}
	if d.Differ > 0 {
		fmt.Fprintf(out, "Условия в реестре и во вкладке расходятся у %d (реестр ≠ вкладка): %s\n", d.Differ, d.DifferText())
	} else {
		fmt.Fprintf(out, "Условия в реестре и во вкладках совпадают у %d.\n", d.Equal)
	}
	if len(d.Newcomers) > 0 {
		fmt.Fprintln(out, "Из реестра без вкладки выплат — с даты начисляются по условиям реестра на вкладке книг, строка people — из реестра:")
		rows := [][]string{}
		for _, n := range d.Newcomers {
			people := "есть"
			if !n.InPeople {
				people = "нет — будет из реестра"
			}
			rows = append(rows, []string{n.Person, n.Model, n.Tab, people})
		}
		writeTable(indent{out}, []string{"человек", "модель", "вкладка книг", "строка people"}, rows, []bool{false, false, false, false})
	}
	if len(d.Fallback) > 0 {
		fmt.Fprintln(out, "Дни с "+d.From+", начисленные по шапке вкладки: в реестре нет условия на день:")
		rows := [][]string{}
		for _, f := range d.Fallback {
			rows = append(rows, []string{f.Person, f.Model, f.Tab, f.First, f.Last, fmt.Sprint(f.Days)})
		}
		writeTable(indent{out}, []string{"человек", "модель", "вкладка", "с", "по", "дней"}, rows, []bool{false, false, false, false, false, true})
	}
}

func orDashText(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// payrollTermsFrom sets or clears the day from which the accruals take the registry's terms, and rebuilds as
// reports-from does: the reports' accruals (and the facts', when they accrue), then the debts. --off gives back the
// headers' terms; a paid day waits for the owner either way.
func payrollTermsFrom(ctx context.Context, args []string) error {
	fs := Flags("payroll terms-from")
	off := fs.Bool("off", false, "выключить: условия снова из шапок вкладок (откат)")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 || (*off && len(pos) > 0) {
		return Exit{Code: 2, Msg: "ledger payroll terms-from [ГГГГ-ММ-ДД | --off]"}
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	if len(pos) == 0 && !*off {
		from, err := payroll.TermsHubFrom(ctx, store.RO)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "termsFrom": from})
	}
	day := ""
	if !*off {
		day = pos[0]
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := payroll.SetTermsHubFrom(ctx, tx, day, ts.Now()); err != nil {
		return Exit{Code: 2, Msg: "не записано: " + err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	out := map[string]any{"ok": true, "termsFrom": day}
	if _, source, err := payroll.AfterFrom(ctx, store.RO); err != nil {
		return err
	} else if source == payroll.SourceFacts {
		if out["facts"], err = accruals.Rebuild(ctx, store, ts.Now()); err != nil {
			return err
		}
	}
	if out["reports"], err = accruals.RebuildReports(ctx, store, ts.Now()); err != nil {
		return err
	}
	if out["report"], err = payroll.Rebuild(ctx, store, ts.Now()); err != nil {
		return err
	}
	d, err := payroll.CompareTerms(ctx, store.RO, ts.Date(ts.Now()))
	if err != nil {
		return err
	}
	out["fallback"] = d.Fallback
	return printJSON(out)
}
