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
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/money"
)

func init() {
	Command(Cmd{Name: "decision add", Run: decisionAdd,
		Summary: "записать решение «чья продажа» (POST /v1/decisions, право reports или decide): --page … --fan-ref|--fan-name … --kind owner|split|pot|off_platform|exclude " +
			"[--share код:процент …] [--pot код] [--tx ref] [--net 12.50] [--at время] --said-by … --said-at … [--link …] [--note …] [--supersedes id]; или --json (JSON из stdin)"})
	Command(Cmd{Name: "decision list", Run: decisionList,
		Summary: "решения «чья продажа» (GET /v1/decisions): [--open] [--fan …] [--person код] [--from ГГГГ-ММ-ДД --to …] [--json] [--md — открытые списком, как файл агента]"})
	Command(Cmd{Name: "decision applied", Run: decisionApplied,
		Summary: "решение учтено в отчёте: ledger decision applied <id> --person <код> --date <ГГГГ-ММ-ДД>"})
	Command(Cmd{Name: "decision cancel", Run: decisionCancel,
		Summary: `отменить решение: ledger decision cancel <id> --note "…"`})
}

// shareFlags collects --share person:percent.
type shareFlags []decisions.Share

func (s *shareFlags) String() string { return fmt.Sprint(*s) }

func (s *shareFlags) Set(v string) error {
	person, pct, ok := strings.Cut(v, ":")
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(pct), "%"))
	if !ok || err != nil {
		return fmt.Errorf("доля: нужно код:процент (anna:50), а не %q", v)
	}
	*s = append(*s, decisions.Share{Person: strings.TrimSpace(person), Percent: n})
	return nil
}

// dollarsToMills reads dollars ("12.50") into mills; nil for "".
func dollarsToMills(name, v string) (*int64, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	m, err := money.Parse(v)
	if err != nil {
		return nil, Exit{Code: 2, Msg: name + ": нужна сумма в долларах (12.50), а не " + strconv.Quote(v)}
	}
	if int64(m)%1000 != 0 {
		return nil, Exit{Code: 2, Msg: name + ": не больше трёх знаков после точки"}
	}
	mills := int64(m) / 1000
	return &mills, nil
}

// refusedBy turns a refusal of the decisions or the pots into exit code 2.
func refusedBy(err error) error {
	var r *decisions.Refusal
	if errors.As(err, &r) {
		return Exit{Code: 2, Msg: "не записано: " + r.Error()}
	}
	return err
}

// direct runs f on LEDGER_DB.
func direct(ctx context.Context, f func(*db.Store) error) error {
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	return refusedBy(f(store))
}

func decisionAdd(ctx context.Context, args []string) error {
	fs := Flags("decision add")
	var in decisions.Input
	var shares shareFlags
	fromJSON := fs.Bool("json", false, "решение JSON-ом из stdin (поля POST /v1/decisions)")
	fs.StringVar(&in.PageLabel, "page", "", "страница хаба")
	fs.StringVar(&in.FanRef, "fan-ref", "", "фан: id хаба")
	fs.StringVar(&in.FanName, "fan-name", "", "фан: ник")
	fs.StringVar(&in.TransactionRef, "tx", "", "продажа хаба (transactionRef); пусто — продажи ещё нет")
	net := fs.String("net", "", "нетто продажи, доллары")
	fs.StringVar(&in.OccurredAt, "at", "", "когда продажа, RFC 3339")
	fs.StringVar(&in.Kind, "kind", "", "owner, split, pot, off_platform или exclude")
	fs.Var(&shares, "share", "доля код:процент, можно несколько")
	fs.StringVar(&in.Pot, "pot", "", "общак (у kind pot)")
	fs.StringVar(&in.SaidBy, "said-by", "", "кто решил")
	fs.StringVar(&in.SaidAt, "said-at", "", "когда сказано: ГГГГ-ММ-ДД или RFC 3339")
	fs.StringVar(&in.SourceLink, "link", "", "ссылка на сообщение")
	fs.StringVar(&in.Note, "note", "", "почему так")
	supersedes := fs.Int64("supersedes", 0, "какое решение заменяет")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return Exit{Code: 2, Msg: "лишние аргументы: " + strings.Join(pos, " ")}
	}
	if *fromJSON {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		in = decisions.Input{}
		if err := decisions.Decode(raw, &in); err != nil {
			return refusedBy(err)
		}
	} else {
		in.Shares = shares
		if in.NetMills, err = dollarsToMills("--net", *net); err != nil {
			return err
		}
		if *supersedes > 0 {
			in.Supersedes = supersedes
		}
	}
	var out decisions.Decision
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			out, err = decisions.Add(ctx, s, in, localAuthor(), time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/decisions", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

func decisionList(ctx context.Context, args []string) error {
	fs := Flags("decision list")
	open := fs.Bool("open", false, "только открытые")
	fan := fs.String("fan", "", "фан: id или ник, без регистра; от 3 знаков — и часть")
	person := fs.String("person", "", "человек в долях: код")
	from := fs.String("from", "", "с дня продажи (или когда сказано, пока продажи нет)")
	to := fs.String("to", "", "по день")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	md := fs.Bool("md", false, "открытые решения списком Markdown, как файл агента")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	f := decisions.Filter{Open: *open || *md, Fan: *fan, Person: *person, From: *from, To: *to}
	var list []decisions.Decision
	names := map[string]string{}
	if local() {
		err := direct(ctx, func(s *db.Store) error {
			var err error
			if list, err = decisions.List(ctx, s.RO, f); err != nil {
				return err
			}
			names, err = decisions.PotNames(ctx, s.RO)
			return err
		})
		if err != nil {
			return err
		}
	} else {
		q := url.Values{}
		if f.Open {
			q.Set("open", "1")
		}
		for k, v := range map[string]string{"fan": f.Fan, "person": f.Person, "from": f.From, "to": f.To} {
			if v != "" {
				q.Set(k, v)
			}
		}
		var ans decisions.ListAnswer
		if err := call(ctx, http.MethodGet, "/v1/decisions?"+q.Encode(), nil, &ans); err != nil {
			return err
		}
		list = ans.Decisions
		if *md {
			var pots struct {
				Pots []struct {
					Slug string `json:"slug"`
					Name string `json:"name"`
				} `json:"pots"`
			}
			if err := call(ctx, http.MethodGet, "/v1/pots", nil, &pots); err == nil {
				for _, p := range pots.Pots {
					names[p.Slug] = p.Name
				}
			}
		}
	}
	switch {
	case *asJSON:
		return printJSON(decisions.ListAnswer{Decisions: list})
	case *md:
		_, err := fmt.Print(decisions.Markdown(list, names))
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	rows := make([][]string, 0, len(list))
	for _, d := range list {
		var shares []string
		for _, s := range d.Shares {
			shares = append(shares, fmt.Sprintf("%s %d%%", s.Person, s.Percent))
		}
		if d.Pot != nil {
			shares = append(shares, "общак "+*d.Pot)
		}
		ref, net := "—", "—"
		if d.TransactionRef != nil {
			ref = *d.TransactionRef
		}
		if d.NetMicros != nil {
			net = usd(*d.NetMicros)
		}
		fan := d.FanName
		if d.FanRef != "" {
			fan = strings.TrimSpace(fan + " " + d.FanRef)
		}
		rows = append(rows, []string{strconv.FormatInt(d.ID, 10), d.Status, d.PageLabel, fan, ref, net, d.Kind, strings.Join(shares, ", "),
			d.SaidBy + ", " + d.SaidAt})
	}
	writeTable(out, []string{"id", "состояние", "страница", "фан", "продажа", "нетто", "вид", "кому", "кто сказал"}, rows,
		[]bool{true, false, false, false, false, true, false, false, false})
	return out.Flush()
}

func decisionID(pos []string, usage string) (int64, error) {
	if len(pos) != 1 {
		return 0, Exit{Code: 2, Msg: "нужен номер решения: " + usage}
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, Exit{Code: 2, Msg: "номер решения — целое число: " + usage}
	}
	return id, nil
}

func decisionApplied(ctx context.Context, args []string) error {
	fs := Flags("decision applied")
	var in decisions.AppliedInput
	fs.StringVar(&in.Person, "person", "", "в чьём отчёте: код")
	fs.StringVar(&in.ReportDate, "date", "", "дата отчёта ГГГГ-ММ-ДД")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := decisionID(pos, "ledger decision applied <id> --person <код> --date <ГГГГ-ММ-ДД>")
	if err != nil {
		return err
	}
	var out decisions.Decision
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			out, err = decisions.Applied(ctx, s, id, in, time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/decisions/"+strconv.FormatInt(id, 10)+"/applied", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

func decisionCancel(ctx context.Context, args []string) error {
	fs := Flags("decision cancel")
	var in decisions.CancelInput
	fs.StringVar(&in.Note, "note", "", "почему отменено")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := decisionID(pos, `ledger decision cancel <id> --note "…"`)
	if err != nil {
		return err
	}
	var out decisions.Decision
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			out, err = decisions.Cancel(ctx, s, id, in, time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/decisions/"+strconv.FormatInt(id, 10)+"/cancel", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

// jsonArg reads a JSON argument: a file, or «-» for stdin, or the JSON itself.
func jsonArg(arg string) ([]byte, error) {
	switch {
	case arg == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(strings.TrimSpace(arg), "{"):
		return []byte(arg), nil
	}
	return os.ReadFile(arg)
}
