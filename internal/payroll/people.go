package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Person is a card of /OS people/<slug>.md.
type Person struct {
	Slug     string
	Name     string
	Aliases  []string
	Status   string
	Position string
	Pages    string
	Schedule string // "" — no schedule
	Workdays []string
	Rate     string
	Models   []string
}

var slugRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// fmValue is a frontmatter value: a scalar or a list; null and empty are the empty scalar.
type fmValue struct {
	Text string
	List []string
	Seq  bool
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if json.Unmarshal([]byte(s), &out) == nil {
			return out
		}
		return s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// flowList splits the inside of [a, "b, c", d] on commas outside quotes.
func flowList(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if v := unquote(cur.String()); v != "" {
			out = append(out, v)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ',':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// plain drops a trailing comment: after a quoted scalar anything past the closing quote, after an
// unquoted one a « #».
func plain(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if q := s[0]; q == '"' || q == '\'' {
		for i := 1; i < len(s); i++ {
			switch {
			case q == '"' && s[i] == '\\':
				i++
			case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
				i++
			case s[i] == q:
				return s[:i+1]
			}
		}
		return s
	}
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// Frontmatter reads the part of YAML the cards use: top-level scalars, quoted strings, null, flow lists
// and block lists. The second result is false when the file has no frontmatter.
func Frontmatter(text string) (map[string]fmValue, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, false
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false
	}
	out := map[string]fmValue{}
	last := ""
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if t := strings.TrimSpace(line); (line[0] == ' ' || line[0] == '\t' || line[0] == '-') && strings.HasPrefix(t, "- ") && last != "" {
			v := out[last]
			v.Seq, v.List = true, append(v.List, unquote(plain(t[2:])))
			out[last] = v
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue // a nested mapping: the cards keep nothing the ledger reads there
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), plain(v)
		last = k
		switch {
		case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"):
			out[k] = fmValue{Seq: true, List: flowList(v[1 : len(v)-1])}
		case v == "" || v == "null" || v == "~" || v == "Null" || v == "NULL":
			out[k] = fmValue{}
		default:
			out[k] = fmValue{Text: unquote(v)}
		}
	}
	return out, true
}

func (v fmValue) list() []string {
	if v.Seq {
		if v.List == nil {
			return []string{}
		}
		return v.List
	}
	if v.Text != "" {
		return []string{v.Text}
	}
	return []string{}
}

// ReadPeople reads the cards of dir. Files without frontmatter or with another `type` are named in
// the second result, a card with a bad slug or without a title stops the import.
func ReadPeople(dir string) ([]Person, []string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, nil, err
	}
	var out []Person
	var skipped []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		slug := strings.TrimSuffix(filepath.Base(path), ".md")
		fm, ok := Frontmatter(string(raw))
		if !ok || (fm["type"].Text != "" && fm["type"].Text != "person") {
			skipped = append(skipped, slug)
			continue
		}
		if !slugRe.MatchString(slug) {
			return nil, nil, fmt.Errorf("карточка %s: имя файла не годится для счёта payable:<slug>", filepath.Base(path))
		}
		if fm["title"].Text == "" {
			return nil, nil, fmt.Errorf("карточка %s: нет title", filepath.Base(path))
		}
		out = append(out, Person{Slug: slug, Name: fm["title"].Text, Aliases: fm["aliases"].list(), Status: fm["status"].Text,
			Position: fm["position"].Text, Pages: fm["account"].Text, Schedule: fm["schedule"].Text,
			Workdays: fm["workdays"].list(), Rate: fm["rate"].Text, Models: fm["models"].list()})
	}
	if len(out) == 0 {
		return nil, skipped, fmt.Errorf("в %s нет карточек людей", dir)
	}
	return out, skipped, nil
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// PutPeople upserts the cards. A card that is gone from the folder stays: its accounts still exist.
func PutPeople(ctx context.Context, q ledger.Querier, people []Person, source string, now time.Time) error {
	for _, p := range people {
		var schedule any
		if p.Schedule != "" {
			schedule = p.Schedule
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO people(slug, name, aliases, status, position, pages, schedule, workdays, rate,
			models, source, imported_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(slug) DO UPDATE SET name = excluded.name, aliases = excluded.aliases, status = excluded.status,
			position = excluded.position, pages = excluded.pages, schedule = excluded.schedule, workdays = excluded.workdays,
			rate = excluded.rate, models = excluded.models, source = excluded.source, imported_at = excluded.imported_at`,
			p.Slug, p.Name, jsonText(p.Aliases), p.Status, p.Position, p.Pages, schedule, jsonText(p.Workdays), p.Rate,
			jsonText(p.Models), source, ts.Format(now)); err != nil {
			return fmt.Errorf("people %s: %w", p.Slug, err)
		}
	}
	return nil
}

// LoadPeople reads the people table by slug.
func LoadPeople(ctx context.Context, q ledger.Querier) (map[string]Person, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug, name, aliases, status, position, pages, COALESCE(schedule, ''), workdays, rate, models FROM people`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Person{}
	for rows.Next() {
		var p Person
		var aliases, workdays, models string
		if err := rows.Scan(&p.Slug, &p.Name, &aliases, &p.Status, &p.Position, &p.Pages, &p.Schedule, &workdays, &p.Rate, &models); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(aliases), &p.Aliases)
		_ = json.Unmarshal([]byte(workdays), &p.Workdays)
		_ = json.Unmarshal([]byte(models), &p.Models)
		out[p.Slug] = p
	}
	return out, rows.Err()
}

// optionalMap reads a JSON-object setting; no row is an empty map.
func optionalMap(ctx context.Context, q ledger.Querier, key string) (map[string]string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if strings.TrimSpace(v) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("настройка %s: нужен JSON-объект строк: %v", key, err)
	}
	return out, nil
}

// Resolver finds the person of a tab or of a journal row. It only ever answers with one person it can
// prove: an unknown or ambiguous name is an error for the report, never a guess.
type Resolver struct {
	people  []Person
	aliases map[string]string // settings.tab_aliases: name in the journal → tab
}

// NewResolver loads the people and settings.tab_aliases.
func NewResolver(ctx context.Context, q ledger.Querier) (*Resolver, error) {
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	aliases, err := optionalMap(ctx, q, "tab_aliases")
	if err != nil {
		return nil, err
	}
	r := &Resolver{aliases: aliases}
	for _, p := range people {
		r.people = append(r.people, p)
	}
	sort.Slice(r.people, func(i, j int) bool { return r.people[i].Slug < r.people[j].Slug })
	return r, nil
}

// BySlug is the card with this code, if any.
func (r *Resolver) BySlug(slug string) (Person, bool) {
	for _, p := range r.people {
		if p.Slug == slug {
			return p, true
		}
	}
	return Person{}, false
}

// HubPerson is the card of a Hub user: one whose aliases name `hub:<id>`, else their Hub username (Resolve: title,
// alias, tab_aliases; the model decides between two cards of one name), else the card whose code is the username,
// case aside (Hub's «Grisha» is the card grisha: the agency's logins are the cards' codes). The error is the
// username's.
func (r *Resolver) HubPerson(model string, user int64, username string) (string, error) {
	if slug, err := r.Resolve(model, "hub:"+strconv.FormatInt(user, 10)); err == nil {
		return slug, nil
	}
	slug, err := r.Resolve(model, username)
	if err == nil {
		return slug, nil
	}
	if card, ok := r.BySlug(strings.ToLower(strings.TrimSpace(username))); ok {
		return card.Slug, nil
	}
	return "", err
}

var parenthetical = regexp.MustCompile(`\s*\([^()]*\)\s*$`)

// Resolve maps (model, name) to a slug. A name matches a card by its title, the title without a trailing
// remark in parentheses, or an alias; settings.tab_aliases joins the name in the journal and the name of
// the tab. When two cards share a name, the card's models decide; the model of a journal row may be «—».
func (r *Resolver) Resolve(model, name string) (string, error) {
	name = strings.TrimSpace(name)
	names := map[string]bool{name: true}
	if tab, ok := r.aliases[name]; ok {
		names[tab] = true
	}
	for journal, tab := range r.aliases {
		if tab == name {
			names[journal] = true
		}
	}
	var found []Person
	for _, p := range r.people {
		hit := names[p.Name] || names[parenthetical.ReplaceAllString(p.Name, "")]
		for _, a := range p.Aliases {
			hit = hit || names[a]
		}
		if hit {
			found = append(found, p)
		}
	}
	if len(found) > 1 {
		var narrowed []Person
		m := ledger.Slug(model)
		for _, p := range found {
			for _, pm := range p.Models {
				if ledger.Slug(pm) == m {
					narrowed = append(narrowed, p)
					break
				}
			}
		}
		if len(narrowed) == 1 {
			return narrowed[0].Slug, nil
		}
		slugs := make([]string, len(found))
		for i, p := range found {
			slugs[i] = p.Slug
		}
		return "", fmt.Errorf("«%s» (%s) подходит нескольким: %s", name, model, strings.Join(slugs, ", "))
	}
	if len(found) == 0 {
		return "", fmt.Errorf("«%s» (%s) — такого человека нет в people и tab_aliases", name, model)
	}
	return found[0].Slug, nil
}
