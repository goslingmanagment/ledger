package bybit

import (
	"encoding/json"
	"github.com/goslingmanagment/ledger/internal/ts"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const syntheticCounterparties = `# UID/адрес -> контрагент
- match: {uid: "1001"}
  slug: alpha
  name: "Альфа"
  direction: expense
  category: "Чаттер зарплата"
  model: "Модель-1"
  note: "заметка про альфу"
- match: {address: "TaddrBeta"}
  slug: null
  name: "Бета"
  direction: income
  category: "Custom"
  model: "—"
- match: {address_prefix: "Trot", address_suffix: "end"}
  slug: gamma
  name: "Гамма"
  direction: expense
  category: "Подрядчик"
  model: "—"
  extra_field: 5
- match: {uid: 1001}
  slug: alpha2
  name: "Альфа-дубль"
  direction: income
  category: "Custom"
  model: "—"
- slug: broken
  name: "Без match"
  direction: expense
  category: "Подрядчик"
  model: "—"
- match: {phone: "1"}
  name: "Чужой ключ"
  direction: expense
  category: "Подрядчик"
`

const syntheticNotes = `# Заметки атрибуции — память паттернов

Описание файла.

- Каждая заметка несёт статус: [подтверждено YYYY-MM-DD] — это шапка, не заметка.

## Заметки

- [подтверждено 2026-08-03] Сервис Икс платится
  с копейками, адрес меняется.
- [гипотеза 2026-09-01; нужен чек] Возможно, это Игрек.

- [опровергнуто 2026-07-20] Зет не платит напрямую.
- [owner 2026-10-02, бриф #44] Сказано владельцем.
  - вложенный пункт
- Заметка без маркера.
`

func writeConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestImportRules(t *testing.T) {
	s, ctx := openDB(t)
	now := at("2026-10-01T10:00:00+03:00")
	dir := writeConfig(t, map[string]string{
		"counterparties.yaml":  syntheticCounterparties,
		"ignore.yaml":          "# личное\n- match: {uid: \"9009\"}\n  note: \"личное\"\n- match: {address: \"TaddrBeta\"}\n",
		"merchants.yaml":       "- match: \"synthetic.*shop\"\n  name: \"Магазин\"\n  category: \"Инструменты\"\n- match: \"(bad\"\n  name: \"Плохой\"\n  category: \"Прочее\"\n",
		"merchant-bots.yaml":   "- name: \"Бот\"\n  peer_id: 777000\n  account: personal\n",
		"attribution-notes.md": syntheticNotes,
	})
	rep, err := ImportRules(ctx, s.RW, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rules["counterparty"] != 6 || rep.Rules["ignore"] != 2 || rep.Rules["merchant"] != 2 || rep.Rules["merchant_bot"] != 1 {
		t.Fatalf("rules %v", rep.Rules)
	}
	if rep.Notes["confirmed"] != 1 || rep.Notes["hypothesis"] != 1 || rep.Notes["refuted"] != 1 || rep.Notes["info"] != 2 {
		t.Fatalf("notes %v", rep.Notes)
	}
	warnings := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{"дубль идентификатора", "counterparties[4]: нет match", "неизвестные ключи match: [phone]",
		"конфликт ignore и counterparties", "нет ключа slug", "не компилируется"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("no warning %q in:\n%s", want, warnings)
		}
	}

	var uid, direction, slug, name, category, model, note, source string
	if err := s.RW.QueryRow(`SELECT match_uid, direction, slug, name, category, model, note, source FROM rules WHERE slug = 'alpha'`).
		Scan(&uid, &direction, &slug, &name, &category, &model, &note, &source); err != nil {
		t.Fatal(err)
	}
	if uid != "1001" || direction != "expense" || name != "Альфа" || category != "Чаттер зарплата" || model != "Модель-1" || note != "заметка про альфу" ||
		source != "ledger-sync:counterparties.yaml" {
		t.Fatal(uid, direction, name, category, model, note, source)
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE name = 'Бета' AND slug IS NULL AND match_address = 'TaddrBeta'`); n != 1 {
		t.Fatal("null slug and address")
	}
	var params string
	if err := s.RW.QueryRow(`SELECT params FROM rules WHERE match_prefix = 'Trot' AND match_suffix = 'end'`).Scan(&params); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(params, `"extra_field":5`) {
		t.Fatalf("params %s", params)
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE match_uid = '1001' AND slug = 'alpha2'`); n != 1 {
		t.Fatal("a numeric uid is stored as text")
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE kind = 'ignore' AND match_uid = '9009' AND name = 'личное'`); n != 1 {
		t.Fatal("ignore without a name takes its note")
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE kind = 'ignore' AND match_address = 'TaddrBeta' AND name = 'игнор'`); n != 1 {
		t.Fatal("ignore without name and note")
	}
	if err := s.RW.QueryRow(`SELECT params FROM rules WHERE kind = 'merchant_bot'`).Scan(&params); err != nil {
		t.Fatal(err)
	}
	var bot map[string]any
	if err := json.Unmarshal([]byte(params), &bot); err != nil || bot["account"] != "personal" || bot["peer_id"].(float64) != 777000 {
		t.Fatal(params, err)
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE kind = 'merchant' AND match_merchant = 'synthetic.*shop' AND category = 'Инструменты'`); n != 1 {
		t.Fatal("merchant")
	}
	var text, status, dated string
	if err := s.RW.QueryRow(`SELECT text, status, dated FROM notes WHERE status = 'confirmed'`).Scan(&text, &status, &dated); err != nil {
		t.Fatal(err)
	}
	if text != "[подтверждено 2026-08-03] Сервис Икс платится с копейками, адрес меняется." || dated != "2026-08-03" {
		t.Fatalf("note %q %q", text, dated)
	}
	if err := s.RW.QueryRow(`SELECT text, dated FROM notes WHERE text LIKE '[owner%'`).Scan(&text, &dated); err != nil {
		t.Fatal(err)
	}
	if text != "[owner 2026-10-02, бриф #44] Сказано владельцем.\n- вложенный пункт" || dated != "2026-10-02" {
		t.Fatalf("nested note %q %q", text, dated)
	}
	if n := count(t, s, `SELECT count(*) FROM notes WHERE text LIKE '%шапка%'`); n != 0 {
		t.Fatal("the file header became a note")
	}

	// Re-import: same counts, no duplicates. A file that shrank replaces its rows; an absent one stays.
	if _, err := ImportRules(ctx, s.RW, dir, now); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM rules`); n != 11 {
		t.Fatalf("rules after re-import: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM notes`); n != 5 {
		t.Fatalf("notes after re-import: %d", n)
	}
	smaller := writeConfig(t, map[string]string{"counterparties.yaml": "- match: {uid: \"1\"}\n  slug: one\n  name: \"Один\"\n  direction: expense\n  category: \"Подрядчик\"\n  model: \"—\"\n",
		"ignore.yaml": "# только комментарий\n"})
	rep, err = ImportRules(ctx, s.RW, smaller, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Missing, ",") != "merchants.yaml,merchant-bots.yaml,attribution-notes.md" {
		t.Fatalf("missing %v", rep.Missing)
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE kind = 'counterparty'`); n != 1 {
		t.Fatalf("counterparties %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE kind = 'ignore'`); n != 0 {
		t.Fatalf("an emptied file keeps %d rows", n)
	}
	if n := count(t, s, `SELECT count(*) FROM rules WHERE kind = 'merchant'`); n != 2 {
		t.Fatalf("merchants of an absent file: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM notes`); n != 5 {
		t.Fatalf("notes of an absent file: %d", n)
	}
}

func TestImportRulesRefusesBadInput(t *testing.T) {
	s, ctx := openDB(t)
	now := at("2026-10-01T10:00:00+03:00")
	bad := writeConfig(t, map[string]string{"counterparties.yaml": "- match: {uid: \"1\"}\n  name: \"Х\"\n  direction: sideways\n  category: \"К\"\n"})
	if _, err := ImportRules(ctx, s.RW, bad, now); err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Fatalf("bad direction: %v", err)
	}
	notList := writeConfig(t, map[string]string{"ignore.yaml": "a: 1\n"})
	if _, err := ImportRules(ctx, s.RW, notList, now); err == nil {
		t.Fatal("a mapping accepted as a list")
	}
	broken := writeConfig(t, map[string]string{"merchants.yaml": "- match: [\n"})
	if _, err := ImportRules(ctx, s.RW, broken, now); err == nil {
		t.Fatal("broken YAML accepted")
	}
}

func TestImportedRulesClassify(t *testing.T) {
	s, ctx := openDB(t)
	dir := writeConfig(t, map[string]string{"counterparties.yaml": syntheticCounterparties})
	if _, err := ImportRules(ctx, s.RW, dir, at("2026-10-01")); err != nil {
		t.Fatal(err)
	}
	addEvent(t, s, ctx, movement("withdrawal", "1", "expense", "uid", "1001", "10", "", "2026-09-03T10:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("deposit", "2", "income", "address", "TaddrBeta", "20", "", "2026-09-03T11:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "3", "expense", "address", "TrotXXXend", "30", "", "2026-09-03T12:00:00.000+03:00"))
	addEvent(t, s, ctx, movement("withdrawal", "4", "expense", "address", "TrotXXX", "40", "", "2026-09-03T13:00:00.000+03:00"))
	rep := classifyAll(t, s, ctx, "shadow")
	if rep.Auto != 3 || rep.Unknown != 1 {
		t.Fatalf("report %+v", rep)
	}
	if _, _, _, c := proposalOf(t, s, "withdrawal:3"); c.Rule.Name != "Гамма" {
		t.Fatalf("prefix + suffix: %+v", c.Rule)
	}
}

func TestAddRuleFromAnAnswer(t *testing.T) {
	s, ctx := openDB(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, ts.MSK)
	in := RuleInput{Kind: "counterparty", Address: "TaddrNew", Direction: "expense", Name: "Сервис", Category: "Инструменты"}
	if _, err := AddRule(ctx, s.RW, in, "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := AddRule(ctx, s.RW, in, "main", now); err == nil {
		t.Fatal("a repeat must be refused")
	}
	rules, err := LoadRules(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	v := Classify(Movement{Kind: "withdrawal", Coin: "USDT", Direction: "expense", CounterpartyKind: "address", Counterparty: "TaddrNew", Date: "2026-11-04"}, rules)
	if v.Block != "auto" || v.Rule == nil || v.Rule.Name != "Сервис" || v.Rule.Model != "—" {
		t.Fatalf("verdict %+v", v)
	}
	for name, bad := range map[string]RuleInput{
		"no identity":  {Kind: "counterparty", Direction: "expense", Name: "x", Category: "y"},
		"two ids":      {Kind: "counterparty", UID: "1", Address: "T", Direction: "expense", Name: "x", Category: "y"},
		"half address": {Kind: "counterparty", Prefix: "T", Direction: "expense", Name: "x", Category: "y"},
		"no direction": {Kind: "counterparty", UID: "1", Name: "x", Category: "y"},
		"bad pattern":  {Kind: "merchant", Merchant: "(", Name: "x", Category: "y"},
		"bad kind":     {Kind: "merchant_bot", UID: "1", Name: "x"},
	} {
		if _, err := AddRule(ctx, s.RW, bad, "main", now); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
