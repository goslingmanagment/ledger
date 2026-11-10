package cli

import (
	"bufio"
	"context"
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
)

func init() {
	Command(Cmd{Name: "question add", Run: questionAdd,
		Summary: "записать открытый вопрос «чья продажа» (POST /v1/questions, право reports или decide): --page … --fan-ref|--fan-name … --question \"…\" " +
			"--asked-of owner|chatters --asked-at … [--who …] [--tx ref] [--net 12.50] [--at время] [--link …] [--note …]; или --json (JSON из stdin)"})
	Command(Cmd{Name: "question list", Run: questionList,
		Summary: "вопросы «чья продажа» (GET /v1/questions): [--open] [--asked-of owner|chatters] [--fan …] [--from ГГГГ-ММ-ДД --to …] [--json] [--md — открытые списком по группам]"})
	Command(Cmd{Name: "question answer", Run: questionAnswer,
		Summary: `ответ на вопрос: ledger question answer <id> [--decision <id>] [--answer "…"]`})
	Command(Cmd{Name: "question drop", Run: questionDrop,
		Summary: `вопрос отпал: ledger question drop <id> --note "…"`})
}

func questionAdd(ctx context.Context, args []string) error {
	fs := Flags("question add")
	var in decisions.QuestionInput
	fromJSON := fs.Bool("json", false, "вопрос JSON-ом из stdin (поля POST /v1/questions)")
	fs.StringVar(&in.PageLabel, "page", "", "страница хаба")
	fs.StringVar(&in.FanRef, "fan-ref", "", "фан: id хаба")
	fs.StringVar(&in.FanName, "fan-name", "", "фан: ник")
	fs.StringVar(&in.TransactionRef, "tx", "", "продажа хаба (transactionRef); пусто — продажи ещё нет или неизвестна")
	net := fs.String("net", "", "нетто продажи, доллары")
	fs.StringVar(&in.OccurredAt, "at", "", "когда продажа, RFC 3339")
	fs.StringVar(&in.Question, "question", "", "вопрос словами")
	fs.StringVar(&in.AskedOf, "asked-of", "", "owner — ждёт владельца, chatters — решают сами чаттеры")
	fs.StringVar(&in.Who, "who", "", "какие чаттеры должны договориться")
	fs.StringVar(&in.AskedAt, "asked-at", "", "когда спросили: ГГГГ-ММ-ДД или RFC 3339")
	fs.StringVar(&in.SourceLink, "link", "", "ссылка на сообщение")
	fs.StringVar(&in.Note, "note", "", "факты")
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
		in = decisions.QuestionInput{}
		if err := decisions.Decode(raw, &in); err != nil {
			return refusedBy(err)
		}
	} else if in.NetMills, err = dollarsToMills("--net", *net); err != nil {
		return err
	}
	var out decisions.Question
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			out, err = decisions.AddQuestion(ctx, s, in, localAuthor(), time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/questions", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

func questionList(ctx context.Context, args []string) error {
	fs := Flags("question list")
	open := fs.Bool("open", false, "только открытые")
	askedOf := fs.String("asked-of", "", "owner или chatters")
	fan := fs.String("fan", "", "фан: id или ник, без регистра; от 3 знаков — и часть")
	from := fs.String("from", "", "спросили с дня")
	to := fs.String("to", "", "по день")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	md := fs.Bool("md", false, "открытые вопросы списком Markdown: «Ждёт Диму» и «Решают сами чаттеры»")
	if _, err := ParseArgs(fs, args); err != nil {
		return err
	}
	f := decisions.QuestionFilter{Open: *open || *md, AskedOf: *askedOf, Fan: *fan, From: *from, To: *to}
	var list []decisions.Question
	if local() {
		err := direct(ctx, func(s *db.Store) error {
			var err error
			list, err = decisions.ListQuestions(ctx, s.RO, f)
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
		for k, v := range map[string]string{"asked_of": f.AskedOf, "fan": f.Fan, "from": f.From, "to": f.To} {
			if v != "" {
				q.Set(k, v)
			}
		}
		var ans decisions.QuestionsAnswer
		if err := call(ctx, http.MethodGet, "/v1/questions?"+q.Encode(), nil, &ans); err != nil {
			return err
		}
		list = ans.Questions
	}
	switch {
	case *asJSON:
		return printJSON(decisions.QuestionsAnswer{Questions: list})
	case *md:
		_, err := fmt.Print(decisions.QuestionsMarkdown(list))
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	rows := make([][]string, 0, len(list))
	for _, q := range list {
		fan := q.FanName
		if q.FanRef != "" {
			fan = strings.TrimSpace(fan + " " + q.FanRef)
		}
		net := "—"
		if q.NetMicros != nil {
			net = usd(*q.NetMicros)
		}
		closed := ""
		switch {
		case q.DecisionID != nil:
			closed = "решение " + strconv.FormatInt(*q.DecisionID, 10)
		case q.Answer != nil:
			closed = *q.Answer
		case q.ClosedNote != nil:
			closed = *q.ClosedNote
		}
		rows = append(rows, []string{strconv.FormatInt(q.ID, 10), q.Status, q.AskedOf, q.PageLabel, fan, net, q.Question, q.AskedAt, closed})
	}
	writeTable(out, []string{"id", "состояние", "кого", "страница", "фан", "нетто", "вопрос", "спросили", "ответ"}, rows,
		[]bool{true, false, false, false, false, true, false, false, false})
	return out.Flush()
}

func questionID(pos []string, usage string) (int64, error) {
	if len(pos) != 1 {
		return 0, Exit{Code: 2, Msg: "нужен номер вопроса: " + usage}
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, Exit{Code: 2, Msg: "номер вопроса — целое число: " + usage}
	}
	return id, nil
}

func questionAnswer(ctx context.Context, args []string) error {
	fs := Flags("question answer")
	var in decisions.AnswerInput
	decision := fs.Int64("decision", 0, "решение, которым ответили")
	fs.StringVar(&in.Answer, "answer", "", "ответ словами")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := questionID(pos, `ledger question answer <id> [--decision <id>] [--answer "…"]`)
	if err != nil {
		return err
	}
	if *decision > 0 {
		in.DecisionID = decision
	}
	var out decisions.Question
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			out, err = decisions.AnswerQuestion(ctx, s, id, in, time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/questions/"+strconv.FormatInt(id, 10)+"/answer", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}

func questionDrop(ctx context.Context, args []string) error {
	fs := Flags("question drop")
	var in decisions.DropInput
	fs.StringVar(&in.Note, "note", "", "почему вопрос отпал")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := questionID(pos, `ledger question drop <id> --note "…"`)
	if err != nil {
		return err
	}
	var out decisions.Question
	if local() {
		err = direct(ctx, func(s *db.Store) error {
			out, err = decisions.DropQuestion(ctx, s, id, in, time.Now())
			return err
		})
	} else {
		err = call(ctx, http.MethodPost, "/v1/questions/"+strconv.FormatInt(id, 10)+"/drop", in, &out)
	}
	if err != nil {
		return err
	}
	return printJSON(out)
}
