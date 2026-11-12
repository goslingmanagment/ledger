package payroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
)

// A newcomer from Hub's people registry (ecosystem review R5). From settings.payroll_terms_hub_from a person the registry
// has with pay terms needs no hand entry here: their people row is the registry's projection (source hub-registry: the
// name, hub:<id> of every login, the state, the models of the terms; a row from the /OS cards stays theirs), their posts
// are known by the Telegram id and the report tag the registry holds when report_authors does not name them, and a
// model they have terms on but no payouts tab of is accrued on a tab of the books alone, «реестр <код>», at the
// registry's terms. With the setting empty none of this happens: `ledger payroll terms diff` lists who would.

// RegistrySource is the people.source of a row projected from Hub's registry.
const RegistrySource = "hub-registry"

// RegistryTabName is the tab of the books a registry person is accrued on for a model with no payouts tab.
func RegistryTabName(person string) string { return "реестр " + person }

// registryPerson is a human of the registry with pay terms, as ledger keeps them.
type registryPerson struct {
	code, name, state, role string
	logins                  []int64
	telegram                *int64
	tag                     string
	models                  []string // ledger's, of the terms not gone
}

// loadRegistryPeople reads the registry's humans who have pay terms.
func loadRegistryPeople(ctx context.Context, q ledger.Querier) ([]registryPerson, error) {
	terms, err := LoadHubTerms(ctx, q)
	if err != nil {
		return nil, err
	}
	models := map[string][]string{}
	for _, t := range terms {
		seen := false
		for _, m := range models[t.Person] {
			seen = seen || ledger.Slug(m) == ledger.Slug(t.Model)
		}
		if !seen {
			models[t.Person] = append(models[t.Person], t.Model)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT code, COALESCE(display_name, ''), state, role, COALESCE(logins, '[]'), telegram_user_id,
		COALESCE(report_tag, '') FROM hub_people WHERE kind = 'human' ORDER BY code, hub_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []registryPerson
	for rows.Next() {
		var p registryPerson
		var logins string
		var tg sql.NullInt64
		if err := rows.Scan(&p.code, &p.name, &p.state, &p.role, &logins, &tg, &p.tag); err != nil {
			return nil, err
		}
		if len(models[p.code]) == 0 {
			continue
		}
		_ = json.Unmarshal([]byte(logins), &p.logins)
		if tg.Valid {
			v := tg.Int64
			p.telegram = &v
		}
		p.models = models[p.code]
		out = append(out, p)
	}
	return out, rows.Err()
}

// registryOn says the registry's terms price the days (settings.payroll_terms_hub_from is set).
func registryOn(ctx context.Context, q ledger.Querier) (bool, error) {
	from, err := TermsHubFrom(ctx, q)
	return from != "", err
}

// ProjectRegistryPeople writes the people rows of the registry's humans with pay terms: a code people does not have gets
// one, a row projected before follows the registry; a row from the /OS cards is left as it is. Nothing while
// settings.payroll_terms_hub_from is empty. It returns how many rows it wrote.
func ProjectRegistryPeople(ctx context.Context, q ledger.Querier, now time.Time) (int, error) {
	if on, err := registryOn(ctx, q); err != nil || !on {
		return 0, err
	}
	list, err := loadRegistryPeople(ctx, q)
	if err != nil {
		return 0, err
	}
	have, err := LoadPeople(ctx, q)
	if err != nil {
		return 0, err
	}
	sources := map[string]string{}
	rows, err := q.QueryContext(ctx, `SELECT slug, source FROM people`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var slug, source string
		if err := rows.Scan(&slug, &source); err != nil {
			rows.Close()
			return 0, err
		}
		sources[slug] = source
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var put []Person
	for _, r := range list {
		if src, ok := sources[r.code]; ok && src != RegistrySource {
			continue // a card of /OS: its own
		}
		if !slugRe.MatchString(r.code) {
			return 0, fmt.Errorf("код реестра %q не годится для счёта payable:<код>", r.code)
		}
		p := Person{Slug: r.code, Name: r.name, Aliases: []string{}, Status: registryStatus(r.state), Position: r.role,
			Workdays: []string{}, Models: r.models}
		if p.Name == "" {
			p.Name = r.code
		}
		for _, id := range r.logins {
			p.Aliases = append(p.Aliases, "hub:"+strconv.FormatInt(id, 10))
		}
		if old, ok := have[r.code]; ok && samePerson(old, p) {
			continue
		}
		put = append(put, p)
	}
	if len(put) == 0 {
		return 0, nil
	}
	return len(put), PutPeople(ctx, q, put, RegistrySource, now)
}

// registryStatus is the people.status of a registry state: left is departed, the rest work.
func registryStatus(state string) string {
	if state == "left" {
		return "departed"
	}
	return "active"
}

func samePerson(a, b Person) bool {
	return a.Name == b.Name && a.Status == b.Status && a.Position == b.Position && strings.Join(a.Aliases, ",") == strings.Join(b.Aliases, ",") &&
		strings.Join(a.Models, ",") == strings.Join(b.Models, ",")
}

// RegistryAuthors are the authors of the posts the registry knows: "tg:<id>" and the lowercased #tag of every human with
// pay terms who has not left. Empty while settings.payroll_terms_hub_from is empty; report_authors stays first.
func RegistryAuthors(ctx context.Context, q ledger.Querier) (map[string]string, error) {
	out := map[string]string{}
	if on, err := registryOn(ctx, q); err != nil || !on {
		return out, err
	}
	list, err := loadRegistryPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		if r.state == "left" {
			continue
		}
		if r.telegram != nil {
			out["tg:"+strconv.FormatInt(*r.telegram, 10)] = r.code
		}
		if tag := strings.ToLower(strings.TrimSpace(r.tag)); tag != "" {
			out[tag] = r.code
		}
	}
	return out, nil
}

// registryTabs are the tabs of the books of the registry's people for the models they have terms on and no payouts tab
// of; none while settings.payroll_terms_hub_from is empty. The person must have a people row (the projection).
func registryTabs(ctx context.Context, q ledger.Querier, real []planTab) ([]planTab, error) {
	if on, err := registryOn(ctx, q); err != nil || !on {
		return nil, err
	}
	list, err := loadRegistryPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	has := map[string]bool{}
	for _, t := range real {
		has[t.person+"\x00"+ledger.Slug(t.model)] = true
	}
	var out []planTab
	for _, r := range list {
		if _, ok := people[r.code]; !ok {
			continue
		}
		for _, m := range r.models {
			if !has[r.code+"\x00"+ledger.Slug(m)] {
				out = append(out, planTab{model: m, name: RegistryTabName(r.code), person: r.code, listed: true})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].model != out[j].model {
			return out[i].model < out[j].model
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

// Newcomer is a registry person with pay terms the books do not have yet: no people row, or no payouts tab of a model.
type Newcomer struct {
	Person string `json:"person"`
	Model  string `json:"model"`
	// InPeople: the person has a people row already; false — the projection would write it.
	InPeople bool `json:"inPeople"`
	// Tab is the tab of the books the person's days of the model are (or would be) accrued on.
	Tab string `json:"tab"`
}

// Newcomers lists the registry's people with terms on a model with no payouts tab of theirs, whatever the setting.
func Newcomers(ctx context.Context, q ledger.Querier) ([]Newcomer, error) {
	list, err := loadRegistryPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	people, err := LoadPeople(ctx, q)
	if err != nil {
		return nil, err
	}
	tabs, err := LoadTerms(ctx, q)
	if err != nil {
		return nil, err
	}
	has := map[string]bool{}
	for _, t := range tabs {
		has[t.Person+"\x00"+ledger.Slug(t.Model)] = true
	}
	out := []Newcomer{}
	for _, r := range list {
		if r.state == "left" {
			continue
		}
		_, in := people[r.code]
		for _, m := range r.models {
			if !has[r.code+"\x00"+ledger.Slug(m)] {
				out = append(out, Newcomer{Person: r.code, Model: m, InPeople: in, Tab: RegistryTabName(r.code)})
			}
		}
	}
	return out, nil
}

// projectPeople runs ProjectRegistryPeople in a transaction of its own.
func projectPeople(ctx context.Context, store *db.Store, now time.Time) error {
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := ProjectRegistryPeople(ctx, tx, now); err != nil {
		return fmt.Errorf("люди из реестра: %w", err)
	}
	return tx.Commit()
}

// registryPayee is the SQL condition, on people p, of a registry person paid from the registry's terms: from
// settings.payroll_terms_hub_from a person with terms not gone counts as having a tab for the payouts reads.
const registryPayee = `(EXISTS (SELECT 1 FROM settings s WHERE s.key = 'payroll_terms_hub_from' AND trim(s.value) <> '')
	AND EXISTS (SELECT 1 FROM hub_pay_terms h WHERE h.person = p.slug AND h.gone_at IS NULL))`
