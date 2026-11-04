package cli

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/agentsql"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
)

func init() {
	Command(Cmd{Name: "sql", Summary: `запрос через API: ledger sql "SELECT …" [--json|--csv]; - — запрос со stdin`, Run: sqlCmd})
	Command(Cmd{Name: "schema", Summary: "таблицы и колонки с описаниями: ledger schema [таблица…] [--json]", Run: schemaCmd})
}

// apiClient is the agent's client: LEDGER_URL and LEDGER_KEY or ~/.config/ledger/credentials.
func apiClient() (*client.Client, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, Exit{Code: 2, Msg: err.Error()}
	}
	return client.New(cfg), nil
}

// apiError turns a failed call into an exit code: 2 — the request is wrong (4xx), 4 — no
// answer (network, 5xx).
func apiError(err error) error {
	var e *client.Error
	if errors.As(err, &e) {
		code := 2
		if e.Status >= 500 {
			code = 4
		}
		return Exit{Code: code, Msg: fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)}
	}
	return Exit{Code: 4, Msg: err.Error()}
}

// parseArgs parses flags placed before, between or after the positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

func sqlCmd(ctx context.Context, args []string) error {
	fs := Flags("sql")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	asCSV := fs.Bool("csv", false, "CSV с заголовком")
	words, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(words, " "))
	if query == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		query = strings.TrimSpace(string(b))
	}
	if query == "" {
		return fmt.Errorf(`нужен запрос: ledger sql "SELECT …" или ledger sql - с запросом на stdin`)
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	var resp agentsql.SQLResponse
	if err := c.Do(ctx, http.MethodPost, "/v1/sql", agentsql.SQLRequest{SQL: query}, &resp); err != nil {
		return apiError(err)
	}
	switch {
	case *asJSON:
		return printJSON(resp)
	case *asCSV:
		w := csv.NewWriter(os.Stdout)
		w.Write(resp.Columns)
		for _, r := range resp.Rows {
			rec := make([]string, len(r))
			for i, v := range r {
				if v != nil {
					rec[i] = cell(v)
				}
			}
			w.Write(rec)
		}
		w.Flush()
		if resp.Truncated {
			fmt.Fprintln(os.Stderr, resp.Note)
		}
		return w.Error()
	}
	cells := make([][]string, len(resp.Rows))
	right := make([]bool, len(resp.Columns))
	for i := range right {
		right[i] = len(resp.Rows) > 0
	}
	for i, r := range resp.Rows {
		cells[i] = make([]string, len(r))
		for j, v := range r {
			cells[i][j] = cell(v)
			if _, num := v.(json.Number); !num && v != nil {
				right[j] = false
			}
		}
	}
	out := bufio.NewWriter(os.Stdout)
	writeTable(out, resp.Columns, cells, right)
	if resp.Truncated {
		fmt.Fprintf(out, "(%s; %d мс)\n", resp.Note, resp.DurationMs)
	} else {
		fmt.Fprintf(out, "(%s, %d мс)\n", plural(len(resp.Rows), "строка", "строки", "строк"), resp.DurationMs)
	}
	return out.Flush()
}

var escapes = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return escapes.Replace(x)
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// writeTable prints rows under the header in aligned columns; right marks number columns.
func writeTable(w io.Writer, header []string, rows [][]string, right []bool) {
	width := make([]int, len(header))
	for i, h := range header {
		width[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			width[i] = max(width[i], utf8.RuneCountInString(c))
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, c := range cells {
			pad := strings.Repeat(" ", width[i]-utf8.RuneCountInString(c))
			if i > 0 {
				b.WriteString("  ")
			}
			if right != nil && right[i] {
				b.WriteString(pad + c)
			} else {
				b.WriteString(c + pad)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(header)
	dashes := make([]string, len(header))
	for i := range dashes {
		dashes[i] = strings.Repeat("-", width[i])
	}
	line(dashes)
	for _, r := range rows {
		line(r)
	}
}

func plural(n int, one, few, many string) string {
	word := many
	switch {
	case n%10 == 1 && n%100 != 11:
		word = one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		word = few
	}
	return fmt.Sprintf("%d %s", n, word)
}

func schemaCmd(ctx context.Context, args []string) error {
	fs := Flags("schema")
	asJSON := fs.Bool("json", false, "ответ API как есть")
	names, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	var resp agentsql.SchemaResponse
	if err := c.Do(ctx, http.MethodGet, "/v1/schema", nil, &resp); err != nil {
		return apiError(err)
	}
	tables := resp.Tables
	if len(names) > 0 {
		byName := map[string]agentsql.Table{}
		for _, t := range resp.Tables {
			byName[t.Name] = t
		}
		tables = nil
		for _, n := range names {
			t, ok := byName[n]
			if !ok {
				return Exit{Code: 2, Msg: fmt.Sprintf("нет таблицы или представления %q", n)}
			}
			tables = append(tables, t)
		}
	}
	if *asJSON {
		return printJSON(agentsql.SchemaResponse{OK: true, Tables: tables})
	}
	out := bufio.NewWriter(os.Stdout)
	for i, t := range tables {
		if i > 0 {
			fmt.Fprintln(out)
		}
		title := t.Name
		if t.Kind == "view" {
			title += " (представление)"
		}
		if t.Description != "" {
			title += " — " + t.Description
		}
		fmt.Fprintln(out, title)
		nameW, typeW := 0, 0
		for _, col := range t.Columns {
			nameW = max(nameW, utf8.RuneCountInString(col.Name))
			typeW = max(typeW, utf8.RuneCountInString(col.Type))
		}
		for _, col := range t.Columns {
			var notes []string
			if col.PrimaryKey {
				notes = append(notes, "PK")
			}
			if col.References != "" {
				notes = append(notes, "→ "+col.References)
			}
			if col.Description != "" {
				notes = append(notes, col.Description)
			}
			l := fmt.Sprintf("  %-*s  %-*s  %s", nameW, col.Name, typeW, col.Type, strings.Join(notes, "; "))
			fmt.Fprintln(out, strings.TrimRight(l, " "))
		}
	}
	return out.Flush()
}
