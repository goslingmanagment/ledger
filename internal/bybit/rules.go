package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Files of a ledger-sync config directory and the rule kind each one fills.
const (
	fileCounterparties = "counterparties.yaml"
	fileIgnore         = "ignore.yaml"
	fileMerchants      = "merchants.yaml"
	fileMerchantBots   = "merchant-bots.yaml"
	fileNotes          = "attribution-notes.md"
	sourcePrefix       = "ledger-sync:"
)

// RulesReport is what `ledger rules import` did.
type RulesReport struct {
	Rules    map[string]int `json:"rules"`
	Notes    map[string]int `json:"notes"`
	Missing  []string       `json:"missing_files,omitempty"`
	Warnings []string       `json:"warnings"`
}

// ruleRow is one row of `rules` as imported.
type ruleRow struct {
	kind, uid, address, prefix, suffix, merchant, direction, slug, name, category, model, note string
	params                                                                                     map[string]any
}

// loadYAMLList reads a YAML list; missing is true when the file is absent (its rows stay as they are).
func loadYAMLList(path string) (list []map[string]any, missing bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	if doc == nil { // only comments: an empty list
		return nil, false, nil
	}
	items, ok := doc.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%s: ожидался список", filepath.Base(path))
	}
	for _, it := range items {
		m, _ := it.(map[string]any) // an entry that is not a mapping keeps its position: lint names it
		if m == nil {
			m = map[string]any{}
		}
		list = append(list, m)
	}
	return list, false, nil
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

var (
	validMatchKeys  = map[string]bool{"uid": true, "address": true, "address_prefix": true, "address_suffix": true}
	validDirections = map[string]bool{"expense": true, "income": true, "any": true}
)

// identity of a rule for the duplicate check.
func identity(entry map[string]any) string {
	m, _ := entry["match"].(map[string]any)
	if uid, ok := m["uid"]; ok {
		return "uid " + str(uid)
	}
	if a, ok := m["address"]; ok {
		return "address " + str(a)
	}
	if p, ok := m["address_prefix"]; ok {
		return "prefix " + str(p) + "…" + str(m["address_suffix"])
	}
	return ""
}

// Lint is map_lint.py: the schema of the entries and duplicate identities. Warnings are material for
// review, not a blocker.
func Lint(counterparties, ignores []map[string]any) []string {
	warns := []string{}
	seen := map[string]string{}
	for i, r := range counterparties {
		where := fmt.Sprintf("counterparties[%d]", i)
		m, ok := r["match"].(map[string]any)
		if !ok || len(m) == 0 {
			warns = append(warns, where+": нет match")
			continue
		}
		name := str(r["name"])
		if name == "" {
			name = "?"
		}
		var bad []string
		for k := range m {
			if !validMatchKeys[k] {
				bad = append(bad, k)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			warns = append(warns, fmt.Sprintf("%s (%s): неизвестные ключи match: %v", where, name, bad))
		}
		for _, f := range []string{"name", "direction", "category"} {
			if str(r[f]) == "" {
				warns = append(warns, fmt.Sprintf("%s (%s): нет поля %s", where, name, f))
			}
		}
		if d := str(r["direction"]); d != "" && !validDirections[d] {
			warns = append(warns, fmt.Sprintf("%s (%s): direction %q вне any, expense, income", where, name, d))
		}
		for _, f := range []string{"slug", "model"} {
			if _, ok := r[f]; !ok {
				warns = append(warns, fmt.Sprintf("%s (%s): нет ключа %s (схема разошлась; slug может быть null)", where, name, f))
			}
		}
		if id := identity(r); id != "" {
			if first, dup := seen[id]; dup {
				warns = append(warns, fmt.Sprintf("%s (%s): дубль идентификатора с %s", where, name, first))
			} else {
				seen[id] = fmt.Sprintf("%s (%s)", where, name)
			}
		}
	}
	for i, r := range ignores {
		where := fmt.Sprintf("ignore[%d]", i)
		if m, ok := r["match"].(map[string]any); !ok || len(m) == 0 {
			warns = append(warns, where+": нет match")
			continue
		}
		if id := identity(r); id != "" {
			if first, dup := seen[id]; dup {
				warns = append(warns, fmt.Sprintf("%s: идентификатор уже в %s — конфликт ignore и counterparties", where, first))
			}
		}
	}
	return warns
}

func extras(entry map[string]any, known ...string) map[string]any {
	k := map[string]bool{}
	for _, n := range known {
		k[n] = true
	}
	out := map[string]any{}
	for name, v := range entry {
		if !k[name] {
			out[name] = v
		}
	}
	return out
}

// bybitRow maps a counterparties or ignore entry.
func bybitRow(kind string, entry map[string]any, where string) (ruleRow, error) {
	m, _ := entry["match"].(map[string]any)
	r := ruleRow{kind: kind, uid: str(m["uid"]), address: str(m["address"]), prefix: str(m["address_prefix"]),
		suffix: str(m["address_suffix"]), direction: str(entry["direction"]), slug: str(entry["slug"]), name: str(entry["name"]),
		category: str(entry["category"]), model: str(entry["model"]), note: str(entry["note"])}
	if r.direction != "" && !validDirections[r.direction] {
		return r, fmt.Errorf("%s (%s): direction %q вне any, expense, income", where, r.name, r.direction)
	}
	if kind == "ignore" && r.name == "" {
		if r.name = r.note; r.name == "" {
			r.name = "игнор"
		}
	}
	params := map[string]any{}
	if e := extras(entry, "match", "slug", "name", "direction", "category", "model", "note"); len(e) > 0 {
		params["extra"] = e
	}
	if e := extras(m, "uid", "address", "address_prefix", "address_suffix"); len(e) > 0 {
		params["match_extra"] = e
	}
	r.params = params
	return r, nil
}

func replaceRules(ctx context.Context, q ledger.Querier, source string, rows []ruleRow, now time.Time) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM rules WHERE source = ?`, source); err != nil {
		return err
	}
	for _, r := range rows {
		params, err := json.Marshal(r.params)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO rules(kind, match_uid, match_address, match_prefix, match_suffix,
			match_merchant, direction, slug, name, category, model, note, params, created_at, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.kind, nullable(r.uid), nullable(r.address), nullable(r.prefix), nullable(r.suffix), nullable(r.merchant),
			nullable(r.direction), nullable(r.slug), r.name, nullable(r.category), nullable(r.model), r.note, string(params),
			ts.Format(now), source); err != nil {
			return fmt.Errorf("правило %s «%s»: %w", r.kind, r.name, err)
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ImportRules loads a ledger-sync config directory: counterparties, ignore, merchants and merchant
// bots into `rules`, attribution notes into `notes`. A re-import replaces the rows of the same file;
// an absent file leaves its rows untouched. Real values stay in the database.
func ImportRules(ctx context.Context, q ledger.Querier, dir string, now time.Time) (RulesReport, error) {
	rep := RulesReport{Rules: map[string]int{}, Notes: map[string]int{}, Warnings: []string{}}
	counterparties, missC, err := loadYAMLList(filepath.Join(dir, fileCounterparties))
	if err != nil {
		return rep, err
	}
	ignores, missI, err := loadYAMLList(filepath.Join(dir, fileIgnore))
	if err != nil {
		return rep, err
	}
	merchants, missM, err := loadYAMLList(filepath.Join(dir, fileMerchants))
	if err != nil {
		return rep, err
	}
	bots, missB, err := loadYAMLList(filepath.Join(dir, fileMerchantBots))
	if err != nil {
		return rep, err
	}
	rep.Warnings = append(rep.Warnings, Lint(counterparties, ignores)...)

	load := func(file, kind string, missing bool, entries []map[string]any, build func(int, map[string]any) (ruleRow, error)) error {
		if missing {
			rep.Missing = append(rep.Missing, file)
			return nil
		}
		rows := make([]ruleRow, 0, len(entries))
		for i, e := range entries {
			r, err := build(i, e)
			if err != nil {
				return err
			}
			rows = append(rows, r)
		}
		rep.Rules[kind] = len(rows)
		return replaceRules(ctx, q, sourcePrefix+file, rows, now)
	}
	if err := load(fileCounterparties, "counterparty", missC, counterparties, func(i int, e map[string]any) (ruleRow, error) {
		return bybitRow("counterparty", e, fmt.Sprintf("counterparties[%d]", i))
	}); err != nil {
		return rep, err
	}
	if err := load(fileIgnore, "ignore", missI, ignores, func(i int, e map[string]any) (ruleRow, error) {
		return bybitRow("ignore", e, fmt.Sprintf("ignore[%d]", i))
	}); err != nil {
		return rep, err
	}
	if err := load(fileMerchants, "merchant", missM, merchants, func(i int, e map[string]any) (ruleRow, error) {
		pattern := str(e["match"])
		where := fmt.Sprintf("merchants[%d] (%s)", i, str(e["name"]))
		if pattern == "" {
			rep.Warnings = append(rep.Warnings, where+": нет match")
		} else if _, err := regexp.Compile("(?i)" + pattern); err != nil {
			rep.Warnings = append(rep.Warnings, where+": match не компилируется в Go: "+err.Error())
		}
		if str(e["name"]) == "" || str(e["category"]) == "" {
			rep.Warnings = append(rep.Warnings, where+": нет name или category")
		}
		params := map[string]any{"match": "regex, без учёта регистра"}
		if x := extras(e, "match", "name", "category", "note"); len(x) > 0 {
			params["extra"] = x
		}
		return ruleRow{kind: "merchant", merchant: pattern, name: str(e["name"]), category: str(e["category"]), note: str(e["note"]), params: params}, nil
	}); err != nil {
		return rep, err
	}
	if err := load(fileMerchantBots, "merchant_bot", missB, bots, func(i int, e map[string]any) (ruleRow, error) {
		where := fmt.Sprintf("merchant-bots[%d] (%s)", i, str(e["name"]))
		if str(e["name"]) == "" || str(e["peer_id"]) == "" || str(e["account"]) == "" {
			rep.Warnings = append(rep.Warnings, where+": нет name, peer_id или account")
		}
		params := extras(e, "name", "note")
		return ruleRow{kind: "merchant_bot", name: str(e["name"]), note: str(e["note"]), params: params}, nil
	}); err != nil {
		return rep, err
	}

	raw, err := os.ReadFile(filepath.Join(dir, fileNotes))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		rep.Missing = append(rep.Missing, fileNotes)
	case err != nil:
		return rep, err
	default:
		notes := ParseNotes(string(raw))
		source := sourcePrefix + fileNotes
		if _, err := q.ExecContext(ctx, `DELETE FROM notes WHERE source = ?`, source); err != nil {
			return rep, err
		}
		for _, n := range notes {
			if _, err := q.ExecContext(ctx, `INSERT INTO notes(text, status, dated, created_at, source) VALUES (?, ?, ?, ?, ?)`,
				n.Text, n.Status, nullable(n.Dated), ts.Format(now), source); err != nil {
				return rep, err
			}
			rep.Notes[n.Status]++
		}
	}
	return rep, nil
}

// Note is one attribution note.
type Note struct {
	Text   string
	Status string // confirmed | hypothesis | refuted | info
	Dated  string
}

var (
	noteMarker = regexp.MustCompile(`^\[([^\]\s,;]+)([^\]]*)\]`)
	noteDate   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	noteStatus = map[string]string{"подтверждено": "confirmed", "гипотеза": "hypothesis", "опровергнуто": "refuted"}
)

// ParseNotes reads attribution-notes.md: top-level bullets under «## Заметки» (or the whole file
// without that heading), wrapped lines joined. A bullet opening with [подтверждено …], [гипотеза …] or
// [опровергнуто …] takes its status and date; any other bullet is info.
func ParseNotes(text string) []Note {
	lines := strings.Split(text, "\n")
	start := 0
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "## Заметки") {
			start = i + 1
			break
		}
	}
	var notes []Note
	var cur strings.Builder
	flush := func() {
		body := strings.TrimSpace(cur.String())
		cur.Reset()
		if body == "" {
			return
		}
		n := Note{Text: body, Status: "info"}
		if m := noteMarker.FindStringSubmatch(body); m != nil {
			if st, ok := noteStatus[strings.ToLower(m[1])]; ok {
				n.Status = st
			}
			n.Dated = noteDate.FindString(m[0])
		}
		notes = append(notes, n)
	}
	for _, l := range lines[start:] {
		switch {
		case strings.HasPrefix(l, "- "):
			flush()
			cur.WriteString(strings.TrimSpace(l[2:]))
		case strings.HasPrefix(l, "#"):
			flush()
		case strings.TrimSpace(l) == "":
			// a blank line inside a note is only spacing
		case l[0] == ' ' || l[0] == '\t':
			if cur.Len() == 0 {
				continue
			}
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
				cur.WriteString("\n" + t) // a nested list keeps its own line
			} else {
				cur.WriteString(" " + t) // a wrapped line continues the paragraph
			}
		default:
			flush()
		}
	}
	flush()
	return notes
}
