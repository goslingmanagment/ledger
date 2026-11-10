package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/pots"
)

func init() {
	Command(Cmd{Name: "pot declare", Run: potDeclare,
		Summary: "объявить или изменить общак (слово владельца: право decide или рядом с базой): <файл|-|JSON>"})
	Command(Cmd{Name: "pot list", Run: potList, Summary: "общаки: [--json]"})
	Command(Cmd{Name: "pot show", Run: potShow, Summary: "общак: покупки и доля каждого: <код> [--json]"})
	Command(Cmd{Name: "pot add-sale", Run: potAddSale,
		Summary: "покупка в общак (право reports или decide): <код> --tx <transactionRef> | --from-line <отчёт>:<n> [--page … --net 12.50 --at время --fan-ref … --fan-name …] [--note …] [--force]"})
	Command(Cmd{Name: "pot remove-sale", Run: potRemoveSale, Summary: `снять покупку из общака (строка остаётся): <id> --note "…"`})
	Command(Cmd{Name: "pot candidates", Run: potCandidates,
		Summary: "строки отчётов, не вошедшие в итог с пометкой про общак и ещё не в общаке: [<код>] [--json]"})
}

// potWrite runs a write of the pots next to the database, then what follows it (the accruals).
func potWrite(ctx context.Context, f func(*db.Store) error) error {
	return direct(ctx, func(s *db.Store) error {
		if err := f(s); err != nil {
			return err
		}
		if err := pots.Changed(ctx, s, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "записано, но начисления не пересчитаны: "+err.Error())
		}
		return nil
	})
}

func potDeclare(ctx context.Context, args []string) error {
	fs := Flags("pot declare")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужно объявление: ledger pot declare <файл|-|JSON>"}
	}
	raw, err := jsonArg(pos[0])
	if err != nil {
		return err
	}
	var in pots.DeclareInput
	if err := decisions.Decode(raw, &in); err != nil {
		return refusedBy(err)
	}
	var out pots.Pot
	if local() {
		err = potWrite(ctx, func(s *db.Store) error {
			out, err = pots.Declare(ctx, s, in, localAuthor(), time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/pots", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

func potList(ctx context.Context, args []string) error {
	fs := Flags("pot list")
	asJSON := fs.Bool("json", false, "ответ как есть")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	var ans pots.ListAnswer
	if local() {
		if err := direct(ctx, func(s *db.Store) error {
			var err error
			ans.Pots, err = pots.List(ctx, s.RO)
			return err
		}); err != nil {
			return err
		}
	} else if err := call(ctx, http.MethodGet, "/v1/pots", nil, &ans); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	rows := make([][]string, 0, len(ans.Pots))
	for _, p := range ans.Pots {
		rows = append(rows, []string{p.Slug, p.Name, p.Model, strings.Join(p.Pages, ", "), potSpan(p), p.Status,
			strconv.Itoa(p.Sales), usd(p.NetMicros), participants(p)})
	}
	writeTable(out, []string{"код", "название", "модель", "страницы", "срок", "состояние", "покупок", "нетто", "участники"}, rows,
		[]bool{false, false, false, false, false, false, true, true, false})
	return out.Flush()
}

func potSpan(p pots.Pot) string {
	if p.EndsAt == nil {
		return "с " + p.StartsAt
	}
	return p.StartsAt + " — " + *p.EndsAt
}

func participants(p pots.Pot) string {
	parts := make([]string, len(p.Participants))
	for i, x := range p.Participants {
		to := "…"
		if x.To != nil {
			to = *x.To
		}
		parts[i] = fmt.Sprintf("%s %s—%s", x.Person, x.From, to)
	}
	return strings.Join(parts, ", ")
}

func potShow(ctx context.Context, args []string) error {
	fs := Flags("pot show")
	asJSON := fs.Bool("json", false, "ответ как есть")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен код общака: ledger pot show <код>"}
	}
	var sh pots.Shown
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			sh, err = pots.Show(ctx, s.RO, pos[0])
			if err == pots.ErrNotFound {
				return Exit{Code: 2, Msg: "общака " + pos[0] + " нет"}
			}
			return err
		})
	} else {
		err = call(ctx, http.MethodGet, "/v1/pots/"+url.PathEscape(pos[0]), nil, &sh)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(sh)
	}
	out := bufio.NewWriter(os.Stdout)
	p := sh.Pot
	fmt.Fprintf(out, "общак %s «%s»: модель %s, страницы %s, %s, %s\nучастники: %s\nобъявил %s, %s", p.Slug, p.Name, p.Model,
		strings.Join(p.Pages, ", "), potSpan(p), p.Status, participants(p), p.DeclaredBy, p.DeclaredAt)
	if p.DecisionRef != "" {
		fmt.Fprintf(out, " (%s)", p.DecisionRef)
	}
	fmt.Fprintln(out)
	if p.Note != "" {
		fmt.Fprintln(out, p.Note)
	}
	fmt.Fprintln(out)
	rows := make([][]string, 0, len(sh.Sales))
	for _, s := range sh.Sales {
		var shares []string
		for _, x := range s.Shares {
			shares = append(shares, x.Person+" "+usd(x.Micros))
		}
		if s.Flag != nil {
			shares = append(shares, "! "+*s.Flag)
		}
		ref := "—"
		if s.TransactionRef != nil {
			ref = *s.TransactionRef
		}
		rows = append(rows, []string{strconv.FormatInt(s.ID, 10), s.OccurredAt, s.PageLabel, strings.TrimSpace(s.FanName + " " + s.FanRef), ref,
			usd(s.NetMicros), s.Source, strings.Join(shares, ", ")})
	}
	writeTable(out, []string{"id", "когда", "страница", "фан", "продажа", "нетто", "откуда", "доли"}, rows,
		[]bool{true, false, false, false, false, true, false, false})
	fmt.Fprintln(out, "\nдоля каждого (идёт в базу продаж его вкладки модели, платится по его ставке):")
	totals := make([][]string, 0, len(sh.Totals))
	for _, t := range sh.Totals {
		totals = append(totals, []string{t.Person, strconv.Itoa(t.Sales), usd(t.Micros)})
	}
	writeTable(out, []string{"человек", "покупок", "доля"}, totals, []bool{false, true, true})
	if len(sh.Removed) > 0 {
		fmt.Fprintf(out, "\nснято из общака: %d\n", len(sh.Removed))
		for _, s := range sh.Removed {
			fmt.Fprintf(out, "  %d %s %s: %s\n", s.ID, s.OccurredAt, usd(s.NetMicros), deref(s.RemovedNote))
		}
	}
	return out.Flush()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func potAddSale(ctx context.Context, args []string) error {
	fs := Flags("pot add-sale")
	var in pots.SaleInput
	fs.StringVar(&in.TransactionRef, "tx", "", "продажа хаба (transactionRef)")
	line := fs.String("from-line", "", "строка сохранённого отчёта: <id отчёта>:<n>")
	fs.StringVar(&in.PageLabel, "page", "", "страница хаба, если продажи нет в зеркале хаба")
	net := fs.String("net", "", "нетто, доллары, если продажи нет в зеркале хаба")
	fs.StringVar(&in.OccurredAt, "at", "", "когда покупка, RFC 3339, если продажи нет в зеркале хаба")
	fs.StringVar(&in.FanRef, "fan-ref", "", "фан: id хаба")
	fs.StringVar(&in.FanName, "fan-name", "", "фан: ник")
	fs.StringVar(&in.Note, "note", "", "заметка")
	fs.BoolVar(&in.Force, "force", false, "добавить, хотя продажа учтена в отчёте чаттера")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен код общака: ledger pot add-sale <код> --tx … | --from-line <отчёт>:<n>"}
	}
	if *line != "" {
		a, b, ok := strings.Cut(*line, ":")
		id, err1 := strconv.ParseInt(a, 10, 64)
		n, err2 := strconv.ParseInt(b, 10, 64)
		if !ok || err1 != nil || err2 != nil || id <= 0 {
			return Exit{Code: 2, Msg: "--from-line: нужно <id отчёта>:<n>, а не " + strconv.Quote(*line)}
		}
		in.FromLine = &pots.LineRef{ReportID: id, N: n}
	}
	if in.NetMills, err = dollarsToMills("--net", *net); err != nil {
		return err
	}
	var out pots.SaleResult
	if local() {
		err = potWrite(ctx, func(s *db.Store) error {
			out, err = pots.AddSale(ctx, s, pos[0], in, pots.SourceOwner, localAuthor(), time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/pots/"+url.PathEscape(pos[0])+"/sales", in, &out)
	}
	if err != nil {
		return err
	}
	for _, w := range out.Warnings {
		fmt.Fprintln(os.Stderr, "внимание: "+w)
	}
	return printJSON(out.Sale)
}

func potRemoveSale(ctx context.Context, args []string) error {
	fs := Flags("pot remove-sale")
	var in pots.RemoveInput
	fs.StringVar(&in.Note, "note", "", "почему снята")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	usage := `ledger pot remove-sale <id> --note "…"`
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: "нужен номер покупки: " + usage}
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return Exit{Code: 2, Msg: "номер покупки — целое число: " + usage}
	}
	var out pots.Sale
	if local() {
		err = potWrite(ctx, func(s *db.Store) error {
			out, err = pots.RemoveSale(ctx, s, id, in, localAuthor(), time.Now())
			if err == pots.ErrNotFound {
				return Exit{Code: 2, Msg: "покупки " + pos[0] + " нет"}
			}
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/pot-sales/"+strconv.FormatInt(id, 10)+"/remove", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

func potCandidates(ctx context.Context, args []string) error {
	fs := Flags("pot candidates")
	asJSON := fs.Bool("json", false, "ответ как есть")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return Exit{Code: 2, Msg: "ledger pot candidates [<код>]"}
	}
	slug := ""
	if len(pos) == 1 {
		slug = pos[0]
	}
	var ans pots.CandidatesAnswer
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			ans.Candidates, err = pots.Candidates(ctx, s.RO, slug)
			if err == pots.ErrNotFound {
				return Exit{Code: 2, Msg: "общака " + slug + " нет"}
			}
			return err
		})
	} else {
		q := ""
		if slug != "" {
			q = "?pot=" + url.QueryEscape(slug)
		}
		err = call(ctx, http.MethodGet, "/v1/pots/candidates"+q, nil, &ans)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(ans)
	}
	out := bufio.NewWriter(os.Stdout)
	rows := make([][]string, 0, len(ans.Candidates))
	for _, c := range ans.Candidates {
		line := strconv.FormatInt(c.ReportID, 10)
		if c.N != nil {
			line += ":" + strconv.FormatInt(*c.N, 10)
		}
		who, at, ref := c.HubUsername, c.ReportDate, "—"
		if c.Person != nil {
			who = *c.Person
		}
		if c.OccurredAt != nil {
			at = *c.OccurredAt
		}
		if c.TransactionRef != nil {
			ref = *c.TransactionRef
		}
		rows = append(rows, []string{line, who, at, c.PageLabel, strings.TrimSpace(c.FanName + " " + c.FanRef), ref, usd(c.NetMicros),
			strings.Join(c.Flags, "; "), strings.Join(c.Pots, ", ")})
	}
	writeTable(out, []string{"строка", "чей отчёт", "когда", "страница", "фан", "продажа", "нетто", "пометки", "подходит в"}, rows,
		[]bool{false, false, false, false, false, false, true, false, false})
	fmt.Fprintln(out, "\nкаждую — по подтверждению: ledger pot add-sale <код> --from-line <строка>")
	return out.Flush()
}
