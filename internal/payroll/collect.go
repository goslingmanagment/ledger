package payroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// SourceSheets is the events source of the payout sheets.
const SourceSheets = "sheets"

// parserVersion enters the hash of an event: a tab read by a changed parser is a new fact, not a
// conflict with the payload the old parser stored. Change it when ParseTab's output changes.
const parserVersion = "1"

// Lists are the tabs that name the chatters of a table.
var Lists = []string{"Итого", "Неактивные"}

// Table is the payout sheet of a model.
type Table struct {
	Model string
	Sheet string
}

// Config is ~/.config/ledger/sheets.json: never in the repository.
type Config struct {
	Account string
	Tables  []Table // in the file's order, like the Python's dict
}

// LoadConfig reads the collector's config; a file readable by others is refused like credentials.
func LoadConfig(path string) (Config, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Config{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("%s is readable by others: chmod 600 %s", path, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var f struct {
		Account string          `json:"account"`
		Tables  json.RawMessage `json:"tables"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg := Config{Account: f.Account}
	dec := json.NewDecoder(bytes.NewReader(f.Tables))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return Config{}, fmt.Errorf(`%s: нужен объект "tables": {"<модель>": "<id таблицы>"}`, path)
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
		var sheet string
		if err := dec.Decode(&sheet); err != nil {
			return Config{}, fmt.Errorf("%s: tables: %w", path, err)
		}
		cfg.Tables = append(cfg.Tables, Table{Model: k.(string), Sheet: sheet})
	}
	if cfg.Account == "" || len(cfg.Tables) == 0 {
		return Config{}, fmt.Errorf(`%s: нужны "account" и хотя бы одна таблица`, path)
	}
	return cfg, nil
}

// Reader returns the values of a range of a sheet.
type Reader func(ctx context.Context, sheet, rng string) ([][]Cell, error)

func tail(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

func head(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// decodeValues is the JSON side of payroll.py gog(): a missing "values" is an empty range.
func decodeValues(sheet, rng string, stdout []byte) ([][]Cell, error) {
	bad := failf("таблица %s %s: gog вернул не таблицу: %s", sheet, rng, head(strings.TrimSpace(string(stdout)), 200))
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.UseNumber()
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		return nil, bad
	}
	raw, ok := doc["values"]
	if !ok || string(raw) == "null" {
		return [][]Cell{}, nil
	}
	vd := json.NewDecoder(bytes.NewReader(raw))
	vd.UseNumber()
	var values [][]Cell
	if err := vd.Decode(&values); err != nil {
		return nil, bad
	}
	return values, nil
}

// Gog reads ranges through the gog CLI, read-only. A failed call is tried three times: a dropped
// connection should not turn a tab into an error for a whole hour.
func Gog(account string) Reader {
	return func(ctx context.Context, sheet, rng string) ([][]Cell, error) {
		var last error
		for attempt := range 3 {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
			cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(cctx, "gog", "sheets", "get", sheet, rng, "--render=UNFORMATTED_VALUE", "--json",
				"--readonly", "--account", account)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			timedOut := cctx.Err() != nil
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var exit *exec.ExitError
			switch {
			case err == nil:
				return decodeValues(sheet, rng, stdout.Bytes())
			case timedOut || !errors.As(err, &exit):
				last = failf("таблица %s %s: gog не запускается или не ответил за 120 с (%v)", sheet, rng, err)
			default:
				last = failf("таблица %s %s: gog rc=%d: %s", sheet, rng, exit.ExitCode(), tail(strings.TrimSpace(stderr.String()), 300))
			}
		}
		return nil, last
	}
}

// TabPayload is the payload of a kind 'tab' event: the cells as read, and what the strict parser made
// of them. Parsed and Error exclude each other; Values is absent only when the tab could not be read.
// The fetch time is the event's occurred_at, not part of the payload: AddEvent refuses a repeat with
// another payload, and an unchanged tab is fetched again every hour.
type TabPayload struct {
	Model  string   `json:"model"`
	Name   string   `json:"name"`
	Values [][]Cell `json:"values,omitempty"`
	Parsed *Tab     `json:"parsed,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// TablePayload is a kind 'table' event: the tabs a model's lists name, or why they were not read.
type TablePayload struct {
	Model string   `json:"model"`
	Tabs  []string `json:"tabs"`
	Error string   `json:"error,omitempty"`
}

// RunPayload is a kind 'run' event, one per run. Tabs repeat while nobody touches them, so this is what
// tells the freshness checks (the newest event of the source) that the collector is alive.
type RunPayload struct {
	Tables int `json:"tables"`
	Tabs   int `json:"tabs"`
	Errors int `json:"errors"`
}

func shortHash(parts ...any) string {
	h := sha256.New()
	h.Write([]byte(parserVersion))
	for _, p := range parts {
		b, _ := json.Marshal(p)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func event(kind, id string, payload any, now time.Time) (events.Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return events.Event{}, fmt.Errorf("%s: %w", id, err)
	}
	return events.Event{Source: SourceSheets, SourceID: id, Kind: kind, OccurredAt: ts.Format(now), Payload: b}, nil
}

// Collection is what one run read.
type Collection struct {
	Events []events.Event
	Errors []string // every tab's refusal, in reading order, like payroll.load
}

// Summary is the message of a run with errors: payroll.load's single refusal, or "".
func (c Collection) Summary() string {
	if len(c.Errors) == 0 {
		return ""
	}
	return "таблицы выплат:\n  " + strings.Join(c.Errors, "\n  ")
}

// Collect reads every table like payroll.load: the names from «Итого» and «Неактивные», then each tab,
// parsed here so that a refusal carries today's date of the machine that read it. It stops only when
// ctx does: a table or tab that fails becomes an event with the error.
func Collect(ctx context.Context, cfg Config, read Reader, now time.Time) (Collection, error) {
	var out Collection
	today := now.In(ts.MSK)
	for _, t := range cfg.Tables {
		names, seen, fail := []string{}, map[string]bool{}, ""
		for _, list := range Lists {
			rows, err := read(ctx, t.Sheet, "'"+list+"'!A1:A40")
			var got []string
			if err == nil {
				got, err = Names(rows, t.Model+"/"+list)
			}
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				fail = err.Error()
				break
			}
			for _, n := range got {
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
		if fail != "" {
			out.Errors = append(out.Errors, fail)
			ev, err := event("table", "table:"+t.Model+":"+shortHash(fail), TablePayload{Model: t.Model, Tabs: []string{}, Error: fail}, now)
			if err != nil {
				return out, err
			}
			out.Events = append(out.Events, ev)
			continue
		}
		ev, err := event("table", "table:"+t.Model+":"+shortHash(names), TablePayload{Model: t.Model, Tabs: names}, now)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, ev)
		for _, name := range names {
			where := t.Model + "/" + name
			p := TabPayload{Model: t.Model, Name: name}
			rows, err := read(ctx, t.Sheet, "'"+name+"'!A1:N")
			if err == nil {
				p.Values = rows
				p.Parsed, err = ParseTab(rows, where, today)
			}
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				p.Parsed, p.Error = nil, err.Error()
				out.Errors = append(out.Errors, p.Error)
			}
			id := "tab:" + where + ":" + shortHash(p.Values, p.Error)
			ev, err := event("tab", id, p, now)
			if err != nil { // a cell JSON cannot carry back (1e999): the tab is refused like any other
				p = TabPayload{Model: t.Model, Name: name, Error: where + ": " + err.Error()}
				out.Errors = append(out.Errors, p.Error)
				if ev, err = event("tab", "tab:"+where+":"+shortHash(p.Error), p, now); err != nil {
					return out, err
				}
			}
			out.Events = append(out.Events, ev)
		}
	}
	tabs := 0
	for _, e := range out.Events {
		if e.Kind == "tab" {
			tabs++
		}
	}
	ev, err := event("run", "run:"+ts.Format(now), RunPayload{Tables: len(cfg.Tables), Tabs: tabs, Errors: len(out.Errors)}, now)
	if err != nil {
		return out, err
	}
	out.Events = append(out.Events, ev)
	return out, nil
}

// Spool keeps events until the API has taken them. One file per (source, source_id): a run that reads
// the same content again while the API is down replaces the file, so nothing piles up, and a resend is
// a repeat for ledger.AddEvent.
type Spool struct{ Dir string }

func (s Spool) path(e events.Event) string {
	sum := sha256.Sum256([]byte(e.Source + "\x00" + e.SourceID))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])[:24]+".json")
}

// spooled keeps the order the events were read in: the first run decides the order of the tabs in
// payroll_tabs, and the monthly card adds them up in that order, like the script reading the tables.
type spooled struct {
	Seq   int          `json:"seq"`
	Event events.Event `json:"event"`
}

// Put writes the events of a run, each atomically.
func (s Spool) Put(list []events.Event) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	for i, e := range list {
		raw, err := json.Marshal(spooled{Seq: i, Event: e})
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(s.Dir, ".put-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(raw); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), s.path(e)); err != nil {
			return err
		}
	}
	return nil
}

// Pending lists the waiting events, oldest run first, each run in reading order.
func (s Spool) Pending() ([]events.Event, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	list := make([]spooled, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var e spooled
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("очередь %s: %w", path, err)
		}
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Event.OccurredAt != list[j].Event.OccurredAt {
			return list[i].Event.OccurredAt < list[j].Event.OccurredAt
		}
		return list[i].Seq < list[j].Seq
	})
	out := make([]events.Event, len(list))
	for i, e := range list {
		out[i] = e.Event
	}
	return out, nil
}

// Sender hands a batch over: the API or, with --local-db, the database itself.
type Sender func(ctx context.Context, batch []events.Event) (events.Answer, error)

// FlushReport says what a flush did.
type FlushReport struct {
	Sent      int      `json:"sent"`
	Inserted  int      `json:"inserted"`
	Repeated  int      `json:"repeated"`
	Pending   int      `json:"pending"`
	Conflicts []string `json:"conflicts,omitempty"`
	Settle    []string `json:"settle,omitempty"`
}

// flushBatch keeps a request and its follow-up work small.
const flushBatch = 40

// Flush sends everything waiting, oldest first, and forgets what the receiver took. A failure leaves
// the rest in the spool and comes back as the error. A conflict stays in the spool and is reported on
// every run until somebody looks: the receiver holds another payload under the same id.
func (s Spool) Flush(ctx context.Context, send Sender) (FlushReport, error) {
	var rep FlushReport
	pending, err := s.Pending()
	if err != nil {
		return rep, err
	}
	rep.Pending = len(pending)
	for start := 0; start < len(pending); start += flushBatch {
		batch := pending[start:min(start+flushBatch, len(pending))]
		ans, err := send(ctx, batch)
		if err != nil {
			return rep, err
		}
		if len(ans.Results) != len(batch) {
			return rep, fmt.Errorf("приёмник ответил %d результатами на %d событий", len(ans.Results), len(batch))
		}
		rep.Sent += len(batch)
		rep.Inserted += ans.Inserted
		rep.Repeated += ans.Repeated
		rep.Settle = append(rep.Settle, ans.Settle...)
		for i, r := range ans.Results {
			if r.Conflict != "" {
				rep.Conflicts = append(rep.Conflicts, r.Conflict)
				continue
			}
			if err := os.Remove(s.path(batch[i])); err != nil && !os.IsNotExist(err) {
				return rep, err
			}
			rep.Pending--
		}
	}
	return rep, nil
}

// Describe says in one line what an event would deliver, for --dry-run.
func Describe(e events.Event) string {
	switch e.Kind {
	case "run":
		var p RunPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err.Error()
		}
		return fmt.Sprintf("прогон: таблиц %d, вкладок %d, ошибок %d", p.Tables, p.Tabs, p.Errors)
	case "table":
		var p TablePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err.Error()
		}
		if p.Error != "" {
			return "таблица не прочитана: " + p.Error
		}
		return "вкладки: " + strings.Join(p.Tabs, ", ")
	case "tab":
		var p TabPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err.Error()
		}
		if p.Error != "" {
			return "ОТКАЗ: " + p.Error
		}
		return fmt.Sprintf("к выплате %.2f, выплачено %.2f, блоков на %.2f, дней %d, с записями %d, замечаний %d",
			p.Parsed.KV, p.Parsed.Paid, p.Parsed.Total, len(p.Parsed.Days), len(p.Parsed.Worked), len(p.Parsed.Notes))
	}
	return e.Kind
}
