package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/db"
)

func init() {
	Command(Cmd{Name: "brief", Summary: "бриф владельцу: открытый неотправленный или новый из открытых предложений [--text] [--new]; пусто — так и скажет", Run: briefCmd})
	Command(Cmd{Name: "brief-sent", Summary: "бриф дошёл до владельца: --brief N (считаются только отправленные)", Run: briefSent})
	Command(Cmd{Name: "pending", Summary: "открытые брифы, где ещё есть что решать [--json]", Run: pendingCmd})
	Command(Cmd{Name: "decide", Summary: "решения владельца одним набором: --brief N --set 1=confirm --set 2=personal --set 3=custom:{…} --set 4=defer --set 5=recorded:388,389", Run: decideCmd})
	Command(Cmd{Name: "propose", Summary: "своё предложение агента о деньгах мимо сборщиков: ledger propose <json|->", Run: proposeCmd})
	Command(Cmd{Name: "evidence add", Summary: "улика по движению: --key K [--source S] \"факт\"", Run: evidenceAdd})
	Command(Cmd{Name: "evidence show", Summary: "улики по движению: --key K", Run: evidenceShow})
	Command(Cmd{Name: "dispute open", Summary: "открыть спор: --subject … [--account счёт] [--amount N] [--evidence …]", Run: disputeOpen})
	Command(Cmd{Name: "dispute list", Summary: "споры: [--status open|resolved|rejected] [--json]", Run: disputeList})
	Command(Cmd{Name: "dispute resolve", Summary: "решить спор: ledger dispute resolve <id> --document <JSON строки или списка | @файл | -> | --reject [--note …]", Run: disputeResolve})
}

// writeLocal runs f in one write transaction on LEDGER_DB.
func writeLocal(ctx context.Context, f func(*sql.Tx) error) error {
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	return brief.InTx(ctx, store, f)
}

// readLocal runs f on the read pool of LEDGER_DB.
func readLocal(ctx context.Context, f func(*db.Store) error) error {
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	return f(store)
}

// localAuthor names who acts next to the database: the service knows the caller by the key.
func localAuthor() string {
	if a := os.Getenv("LEDGER_AUTHOR"); a != "" {
		return a
	}
	return "local"
}

// call goes to the API; a refused request is exit code 2.
func call(ctx context.Context, method, path string, in, out any) error {
	c, err := apiClient()
	if err != nil {
		return err
	}
	if err := c.Do(ctx, method, path, in, out); err != nil {
		return apiError(err)
	}
	return nil
}

// refused turns a refusal into exit code 2 with its reasons.
func refused(err error) error {
	if r, ok := err.(*brief.Refusal); ok {
		return Exit{Code: 2, Msg: "не применено: " + r.Error()}
	}
	return err
}

func briefCmd(ctx context.Context, args []string) error {
	fs := Flags("brief")
	text := fs.Bool("text", false, "текст по блокам вместо JSON")
	fresh := fs.Bool("new", false, "новый бриф, даже если есть открытый неотправленный")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var out brief.Answer
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			b, err := brief.Make(ctx, tx, brief.Options{New: *fresh, Author: localAuthor()}, time.Now())
			out = brief.Respond(b)
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, "/v1/brief", map[string]any{"new": *fresh}, &out); err != nil {
		return err
	}
	if *text || out.Empty {
		fmt.Println(out.Text)
		return nil
	}
	return printJSON(out)
}

func briefSent(ctx context.Context, args []string) error {
	fs := Flags("brief-sent")
	id := fs.Int64("brief", 0, "номер брифа")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 {
		return Exit{Code: 2, Msg: "нужен --brief N"}
	}
	out := map[string]any{}
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			at, err := brief.MarkSent(ctx, tx, *id, time.Now())
			out = map[string]any{"ok": true, "brief": *id, "sent_at": at}
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, fmt.Sprintf("/v1/brief/%d/sent", *id), nil, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func pendingCmd(ctx context.Context, args []string) error {
	fs := Flags("pending")
	asJSON := fs.Bool("json", false, "JSON вместо текста")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var list []brief.Answer
	if local() {
		err := readLocal(ctx, func(store *db.Store) error {
			briefs, err := brief.Pending(ctx, store.RO)
			for _, b := range briefs {
				list = append(list, brief.Respond(b))
			}
			return err
		})
		if err != nil {
			return err
		}
	} else if err := call(ctx, http.MethodGet, "/v1/pending", nil, &list); err != nil {
		return err
	}
	if *asJSON {
		if list == nil {
			list = []brief.Answer{}
		}
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("открытых брифов нет")
		return nil
	}
	texts := make([]string, len(list))
	for i, a := range list {
		texts[i] = a.Text
	}
	fmt.Println(strings.Join(texts, "\n\n"))
	return nil
}

func decideCmd(ctx context.Context, args []string) error {
	fs := Flags("decide")
	id := fs.Int64("brief", 0, "номер брифа")
	var sets multi
	fs.Var(&sets, "set", "<номер>=confirm|personal|custom:…|defer|recorded[:номера]; можно несколько")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 || len(sets) == 0 {
		return Exit{Code: 2, Msg: "нужны --brief N и хотя бы один --set"}
	}
	set := make([]brief.Decision, 0, len(sets))
	for _, spec := range sets {
		d, err := brief.ParseSet(spec)
		if err != nil {
			return Exit{Code: 2, Msg: err.Error()}
		}
		set = append(set, d)
	}
	var out *brief.Result
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			var err error
			out, err = brief.Decide(ctx, tx, *id, set, localAuthor(), time.Now())
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, fmt.Sprintf("/v1/brief/%d/decide", *id), map[string]any{"set": set}, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func proposeCmd(ctx context.Context, args []string) error {
	fs := Flags("propose")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Exit{Code: 2, Msg: `нужен JSON: файл, - или {"key", "summary", "documents": [{…}]}`}
	}
	raw := []byte(pos[0])
	if !strings.HasPrefix(strings.TrimSpace(pos[0]), "{") {
		if raw, err = readInput(pos[0]); err != nil {
			return err
		}
	}
	var in brief.AgentProposal
	if err := json.Unmarshal(raw, &in); err != nil {
		return Exit{Code: 2, Msg: "предложение: " + err.Error()}
	}
	out := map[string]any{}
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			p, change, err := brief.Propose(ctx, tx, in, localAuthor(), time.Now())
			out = map[string]any{"ok": true, "change": change, "proposal": p}
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, "/v1/proposals", in, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func evidenceAdd(ctx context.Context, args []string) error {
	fs := Flags("evidence add")
	key := fs.String("key", "", "ключ движения: withdrawal:N, card:N, card:N:delta, agent:…")
	source := fs.String("source", "agent", "откуда факт: tg, gmail, ledger…")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if *key == "" || len(pos) != 1 {
		return Exit{Code: 2, Msg: `нужны --key и текст улики: ledger evidence add --key K "факт без цитат"`}
	}
	e := brief.Evidence{Key: *key, Source: *source, Text: pos[0]}
	out := map[string]any{}
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			e.Author = localAuthor()
			added, err := brief.AddEvidence(ctx, tx, e, time.Now())
			out = map[string]any{"ok": true, "added": added}
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, "/v1/evidence", e, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func evidenceShow(ctx context.Context, args []string) error {
	fs := Flags("evidence show")
	key := fs.String("key", "", "ключ движения")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" {
		return Exit{Code: 2, Msg: "нужен --key"}
	}
	list := []brief.Evidence{}
	if local() {
		err := readLocal(ctx, func(store *db.Store) error {
			got, err := brief.EvidenceFor(ctx, store.RO, *key)
			if got != nil {
				list = got
			}
			return err
		})
		if err != nil {
			return err
		}
	} else if err := call(ctx, http.MethodGet, "/v1/evidence?key="+url.QueryEscape(*key), nil, &list); err != nil {
		return err
	}
	return printJSON(list)
}

func disputeOpen(ctx context.Context, args []string) error {
	fs := Flags("dispute open")
	subject := fs.String("subject", "", "что оспаривается")
	account := fs.String("account", "", "счёт")
	amount := fs.String("amount", "", "сумма")
	evidence := fs.String("evidence", "", "улики")
	if err := fs.Parse(args); err != nil {
		return err
	}
	in := brief.OpenInput{Subject: *subject, Account: *account, Amount: *amount, Evidence: *evidence}
	var out brief.Dispute
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			var err error
			out, err = brief.OpenDispute(ctx, tx, in, localAuthor(), time.Now())
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, "/v1/disputes", in, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func disputeList(ctx context.Context, args []string) error {
	fs := Flags("dispute list")
	status := fs.String("status", "", "open, resolved, rejected")
	asJSON := fs.Bool("json", false, "JSON вместо строк")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var list []brief.Dispute
	if local() {
		err := readLocal(ctx, func(store *db.Store) error {
			var err error
			list, err = brief.ListDisputes(ctx, store.RO, *status)
			return err
		})
		if err != nil {
			return err
		}
	} else if err := call(ctx, http.MethodGet, "/v1/disputes?status="+url.QueryEscape(*status), nil, &list); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(list)
	}
	for _, d := range list {
		line := fmt.Sprintf("%d  %-8s  %s  %s", d.ID, d.Status, d.OpenedAt, d.Subject)
		if d.Account != "" {
			line += "  счёт " + d.Account
		}
		if d.Amount != "" {
			line += "  сумма " + d.Amount
		}
		fmt.Println(line)
	}
	fmt.Printf("споров: %d\n", len(list))
	return nil
}

func disputeResolve(ctx context.Context, args []string) error {
	fs := Flags("dispute resolve")
	document := fs.String("document", "", "JSON исправленной строки или списка строк, @файл или -")
	reject := fs.Bool("reject", false, "отклонить спор без исправления")
	note := fs.String("note", "", "пояснение")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return Exit{Code: 2, Msg: fmt.Sprintf("«%s» не номер спора", pos[0])}
	}
	in := brief.ResolveInput{Reject: *reject, Note: *note}
	if *document != "" {
		text := *document
		if text == "-" || strings.HasPrefix(text, "@") {
			raw, err := readInput(strings.TrimPrefix(text, "@"))
			if err != nil {
				return err
			}
			text = string(raw)
		}
		if in.Documents, err = brief.ParseRowPatches(text); err != nil {
			return Exit{Code: 2, Msg: "--document: " + err.Error()}
		}
	}
	var out brief.Dispute
	if local() {
		err := writeLocal(ctx, func(tx *sql.Tx) error {
			var err error
			out, err = brief.ResolveDispute(ctx, tx, id, in, localAuthor(), time.Now())
			return err
		})
		if err != nil {
			return refused(err)
		}
	} else if err := call(ctx, http.MethodPost, fmt.Sprintf("/v1/disputes/%d/resolve", id), in, &out); err != nil {
		return err
	}
	return printJSON(out)
}
