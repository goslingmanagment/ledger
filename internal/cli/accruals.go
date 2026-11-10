package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/accruals"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "accruals diff", Run: accrualsDiff,
		Summary: "было — стало за неделю: таблица выплат, отчёты смен и факты хаба рядом (тень): [--week понедельник] [--json]"})
	Command(Cmd{Name: "accruals rebuild", Summary: "пересчитать начисления из фактов хаба сейчас (рядом с базой)", Run: accrualsRebuild})
	Command(Cmd{Name: "accruals claim", Run: accrualsClaim,
		Summary: `ничья продажа — человеку по слову владельца: ledger accruals claim <transactionRef> --person <код> --note "…"`})
	Command(Cmd{Name: "accruals unclaim", Run: accrualsUnclaim,
		Summary: `снять заявку на продажу, она снова по правилу: ledger accruals unclaim <transactionRef> --note "…"`})
}

func accrualsClaim(ctx context.Context, args []string) error {
	fs := Flags("accruals claim")
	person := fs.String("person", "", "кому: код карточки (people.slug)")
	note := fs.String("note", "", "слово владельца: кто заявил и почему")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: `нужен номер продажи: ledger accruals claim <transactionRef> --person <код> --note "…"`}
	}
	return claimCall(ctx, "/v1/accruals/claim", accruals.ClaimInput{Ref: pos[0], Person: *person, Note: *note}, accruals.Claim)
}

func accrualsUnclaim(ctx context.Context, args []string) error {
	fs := Flags("accruals unclaim")
	note := fs.String("note", "", "слово владельца: почему заявка снята")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: `нужен номер продажи: ledger accruals unclaim <transactionRef> --note "…"`}
	}
	return claimCall(ctx, "/v1/accruals/unclaim", accruals.ClaimInput{Ref: pos[0], Note: *note}, accruals.Unclaim)
}

// claimCall writes a claim beside the database, or through the API with the decide right.
func claimCall(ctx context.Context, path string, in accruals.ClaimInput,
	write func(context.Context, *db.Store, accruals.ClaimInput, string, time.Time) (accruals.ClaimResult, error)) error {
	var out accruals.ClaimResult
	if local() {
		store, err := openDirect(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		if out, err = write(ctx, store, in, localAuthor(), time.Now()); err != nil {
			var r *accruals.Refusal
			if errors.As(err, &r) {
				return Exit{Code: 2, Msg: "не записано: " + r.Error()}
			}
			return err
		}
	} else if err := call(ctx, http.MethodPost, path, in, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func accrualsDiff(ctx context.Context, args []string) error {
	fs := Flags("accruals diff")
	lastWeek := ts.Date(ts.WeekStart(ts.Now()).AddDate(0, 0, -7))
	week := fs.String("week", lastWeek, "понедельник недели ГГГГ-ММ-ДД; по умолчанию прошлая неделя")
	asJSON := fs.Bool("json", false, "ответ как есть")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := accruals.Monday(*week); err != nil {
		return err
	}
	var d accruals.Diff
	if local() { // beside the database: no API needed
		store, err := OpenStore(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		if d, err = accruals.WeekDiff(ctx, store.RO, *week); err != nil {
			return err
		}
	} else {
		c, err := apiClient()
		if err != nil {
			return err
		}
		if err := c.Do(ctx, http.MethodGet, "/v1/accruals/diff?week="+url.QueryEscape(*week), nil, &d); err != nil {
			return apiError(err)
		}
	}
	if *asJSON {
		return printJSON(d)
	}
	out := bufio.NewWriter(os.Stdout)
	printDiff(out, d)
	return out.Flush()
}

func accrualsRebuild(ctx context.Context, args []string) error {
	if err := Flags("accruals rebuild").Parse(args); err != nil {
		return err
	}
	store, err := OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	rep, err := accruals.Rebuild(ctx, store, time.Now())
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "report": rep})
}

func usd(v int64) string { return money.Micros(v).Cents() }

func signed(v int64) string {
	if s := usd(v); v > 0 && s != "0.00" {
		return "+" + s
	}
	return usd(v)
}

func orZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func orDash(v *int64) string {
	if v == nil {
		return "—"
	}
	return usd(*v)
}

// reportsDiffer: a day of the person where the sheet and the reports differ, or is on one side only.
func reportsDiffer(p accruals.PersonDiff) bool {
	for _, x := range p.Days {
		if x.Presence != "" || len(x.Pages) > 0 {
			return true
		}
	}
	return false
}

func orUnknown(v *int64, f func(int64) string) string {
	if v == nil {
		return "неизвестно"
	}
	return f(*v)
}

// printDiff renders the week: the people, each one's days and the sales of the days that differ, then the общак,
// what waits, what nobody's, and the Hub users no card names.
func printDiff(out *bufio.Writer, d accruals.Diff) {
	fmt.Fprintf(out, "Неделя %s — %s: таблица выплат, отчёты смен и факты хаба. Тень: долги и начисления не меняются.\n",
		d.Week, d.Until)
	if d.ReportsFrom != "" {
		fmt.Fprintf(out, "С %s начисляют отчёты смен: для этих дней таблица — зеркало.\n", d.ReportsFrom)
	}
	if d.Facts.Status == "ok" {
		fmt.Fprintf(out, "Факты недели доказаны, прочитаны %s.\n\n", d.Facts.ReadAt)
	} else {
		fmt.Fprintln(out, "Факты недели «неизвестно» — это не ноль:")
		for _, r := range d.Facts.Reasons {
			fmt.Fprintf(out, "  %s\n", r)
		}
		fmt.Fprintln(out)
	}
	rows := make([][]string, 0, len(d.People))
	var sheet, facts, reps, counted int64
	for _, p := range d.People {
		sheet += p.SheetMicros
		reps += p.ReportsMicros
		counted += p.SheetReportsCountedMicros
		if p.FactsMicros != nil {
			facts += *p.FactsMicros
		}
		unpriced := "—"
		if n := p.ReportsUnpricedMicros + orZero(p.UnpricedMicros); n != 0 {
			unpriced = usd(n)
		}
		rows = append(rows, []string{p.Name + " (" + p.Person + ")", usd(p.SheetMicros), usd(p.ReportsMicros), orUnknown(p.FactsMicros, usd),
			signed(p.SheetReportsCountedMicros), orUnknown(p.ReportsFactsDiffMicros, signed), orUnknown(p.DiffMicros, signed), unpriced})
	}
	if len(rows) > 0 {
		factsTotal, diffTotal, rfTotal := usd(facts), signed(facts-sheet), signed(facts-reps)
		if d.Facts.Status != "ok" {
			factsTotal, diffTotal, rfTotal = "неизвестно", "неизвестно", "неизвестно"
		}
		rows = append(rows, []string{"Итого", usd(sheet), usd(reps), factsTotal, signed(counted), rfTotal, diffTotal, ""})
	}
	writeTable(out, []string{"человек", "таблица", "отчёты", "факты", "отчёты − табл.", "факты − отчёты", "факты − табл.", "без ставки"}, rows,
		[]bool{false, true, true, true, true, true, true, true})
	fmt.Fprintln(out, "«отчёты − табл.» без округления таблицы: разница меньше доллара на странице, где в таблице целое число, не считается.")
	for _, p := range d.People {
		if len(p.Notes) == 0 && len(p.Sales) == 0 && len(p.Claimed) == 0 && !reportsDiffer(p) {
			continue
		}
		fmt.Fprintf(out, "\n%s (%s): таблица %s, отчёты %s, факты %s; отчёты − таблица %s, факты − таблица %s\n", p.Name, p.Person,
			usd(p.SheetMicros), usd(p.ReportsMicros), orUnknown(p.FactsMicros, usd), signed(p.SheetReportsCountedMicros), orUnknown(p.DiffMicros, signed))
		for _, n := range p.Notes {
			fmt.Fprintf(out, "  ! %s\n", n)
		}
		days := [][]string{}
		for _, x := range p.Days {
			days = append(days, []string{x.Day, fmt.Sprintf("%.2f", x.SheetRevenue), usd(x.ReportRevenueMicros), usd(x.FactsNetMicros),
				usd(x.SheetMicros), usd(x.ReportsMicros), usd(x.FactsMicros), signed(x.SheetReportsDiffMicros),
				orUnknown(x.ReportsFactsDiffMicros, signed), x.Presence})
		}
		writeTable(indent{out}, []string{"день", "выручка табл.", "выручка отч.", "net фактов", "таблица", "отчёты", "факты",
			"отч. − табл.", "факты − отч.", ""}, days, []bool{false, true, true, true, true, true, true, true, true, false})
		for _, x := range p.Days {
			if len(x.Pages) == 0 {
				continue
			}
			fmt.Fprintf(out, "  %s по страницам:\n", x.Day)
			for _, pg := range x.Pages {
				line := fmt.Sprintf("    %s: таблица %s, отчёт %s", pg.Page, orDash(pg.SheetMicros), orDash(pg.ReportMicros))
				if pg.Rounding {
					line += " — округление таблицы"
				}
				fmt.Fprintln(out, line)
			}
		}
		if len(p.Sales) > 0 {
			fmt.Fprintln(out, "  Продажи, из которых разница:")
			writeTable(indent{out}, saleHeader, saleRows(p.Sales), saleRight)
		}
		if len(p.Claimed) > 0 {
			fmt.Fprintln(out, "  Ничьи продажи по заявке (слово владельца):")
			rows := [][]string{}
			for i, r := range saleRows(p.Claimed) {
				rows = append(rows, []string{r[1], r[2], r[3], r[4], r[6], p.Claimed[i].Note})
			}
			writeTable(indent{out}, []string{"время", "страница", "вид", "net", "начислено", "заметка"}, rows,
				[]bool{false, false, false, true, true, false})
		}
	}
	group(out, "Общак за неделю (ничей смены, делит владелец)", d.Pot)
	group(out, "Ждёт решения (владелец или согласные метки чаттеров)", d.Decide)
	group(out, "PPV ждёт смены отправителя", d.Waiting)
	group(out, "Ничей", d.Nobody)
	group(out, "Неизвестно (факты продажи не полны)", d.Unknown)
	group(out, "По-старому (до правила, платит таблица)", d.BeforeRule)
	if len(d.Unmapped) > 0 {
		fmt.Fprintln(out, "\nПользователи хаба без карточки в people — их деньги не начислены (имя хаба или hub:<id> в aliases):")
		rows := [][]string{}
		for _, u := range d.Unmapped {
			rows = append(rows, []string{u.Username, fmt.Sprint(u.UserID), u.Model, usd(u.NetMicros), fmt.Sprint(u.Days)})
		}
		writeTable(indent{out}, []string{"пользователь", "hub id", "модель", "net", "дней"}, rows, []bool{false, true, false, true, true})
	}
}

var (
	saleHeader = []string{"", "время", "страница", "вид", "net", "доля", "начислено", "куда", "почему"}
	saleRight  = []bool{false, false, false, false, true, true, true, false, false}
)

func saleRows(sales []accruals.SaleLine) [][]string {
	rows := make([][]string, 0, len(sales))
	for _, s := range sales {
		amount := "—"
		if s.AmountMicros != nil {
			amount = usd(*s.AmountMicros)
		}
		at := strings.Replace(s.At, "T", " ", 1)
		if len(at) > 16 {
			at = at[:16]
		}
		rows = append(rows, []string{s.Sign, at, s.Page, s.Kind, usd(s.NetMicros), usd(s.ShareMicros), amount, s.To, s.Reason})
	}
	return rows
}

func group(out *bufio.Writer, title string, g accruals.Group) {
	if len(g.Sales) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s: %s, продаж %d\n", title, usd(g.Micros), len(g.Sales))
	writeTable(indent{out}, []string{"время", "страница", "вид", "net", "сумма", "почему"}, trim(saleRows(g.Sales)),
		[]bool{false, false, false, true, true, false})
}

// trim keeps the columns time … share and the reason of a group's sales.
func trim(rows [][]string) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		out[i] = append(r[1:6:6], r[8])
	}
	return out
}

// indent writes every line two spaces in.
type indent struct{ w io.Writer }

func (i indent) Write(p []byte) (int, error) {
	lines := strings.SplitAfter(string(p), "\n")
	for _, l := range lines {
		if l == "" {
			continue
		}
		if _, err := io.WriteString(i.w, "  "+l); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
