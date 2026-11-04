package month

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
)

func TestBlockers(t *testing.T) {
	ok := []Check{{1, true, ""}, {3, true, ""}}
	m1 := parseT("2026-10-01T00:00:00+03:00")
	clear := Blockers(ok, Totals{}, parseT("2026-10-14T12:00:00+03:00"), m1, nil, true, nil, true)
	if len(clear) != 0 {
		t.Fatalf("nothing should block: %v", clear)
	}
	all := Blockers([]Check{{3, false, "k вне нормы"}}, Totals{Transit: 500}, parseT("2026-10-05T12:00:00+03:00"), m1,
		[]string{"п.1"}, true, []string{"приход"}, true)
	for _, want := range []string{"проверка 3", "до 8-го", "в пути без", "ночной бриф", "приходы Bybit"} {
		if !strings.Contains(strings.Join(all, "|"), want) {
			t.Errorf("no blocker %q in %v", want, all)
		}
	}
	unread := Blockers(ok, Totals{}, parseT("2026-10-14T12:00:00+03:00"), m1, nil, false, nil, false)
	if len(unread) != 2 {
		t.Fatalf("unreadable sources must block: %v", unread)
	}
}

func TestVerdict(t *testing.T) {
	if v := Verdict(6000, 3500, nil); v != "проходит" {
		t.Fatal(v)
	}
	if v := Verdict(5000, 3500, nil); v != "не проходит" {
		t.Fatal(v)
	}
	tr := 200.0
	if v := Verdict(5900, 3500, &tr); v != "на грани" { // запас 66.67 < max(100, 200)
		t.Fatal(v)
	}
}

// The snapshot of a card compares equal to itself; any changed number is named; sheet cells changed
// after the close are overlaid and listed.
func TestCompareAndOverlay(t *testing.T) {
	g, rows := loadGolden(t)
	cv, err := BuildCover(goldenConfig(g), fakeSource{g}, rows, parseT(g.AsOf))
	if err != nil {
		t.Fatal(err)
	}
	card, err := BuildCard(cv, "2026-09", g.Tabs)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(card.Snapshot())
	diffs, err := CompareSnapshot(card, raw)
	if err != nil || len(diffs) != 0 {
		t.Fatalf("self compare: %v %v", diffs, err)
	}
	var s map[string]any
	json.Unmarshal(raw, &s)
	s["totals"].(map[string]any)["earned"] = 1.0
	s["decision"] = 2.0
	broken, _ := json.Marshal(s)
	diffs, err = CompareSnapshot(card, broken)
	if err != nil || len(diffs) != 2 {
		t.Fatalf("broken compare: %v %v", diffs, err)
	}
	tabs := []Tab{{Model: "Alfa", Name: "Пётр", Days: map[string]float64{"2026-09-03": 10, "2026-09-04": 12.5, "2026-08-31": 7}},
		{Model: "Beta", Name: "Новый", Days: map[string]float64{"2026-09-09": 5}}}
	over, changed := OverlayPayroll(tabs, map[string]map[string]float64{"Alfa/Пётр": {"2026-09-03": 10, "2026-09-04": 0, "2026-09-05": 3}}, "2026-09")
	if over[0].Days["2026-09-04"] != 0 || over[0].Days["2026-09-05"] != 3 || over[0].Days["2026-08-31"] != 7 || len(over[1].Days) != 0 || len(changed) != 3 {
		t.Fatalf("overlay %+v %v", over, changed)
	}
}

func TestDriftFlagsAndAccept(t *testing.T) {
	g, rows := loadGolden(t)
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cv, err := BuildCover(goldenConfig(g), fakeSource{g}, rows, parseT(g.AsOf))
	if err != nil {
		t.Fatal(err)
	}
	card, _ := BuildCard(cv, "2026-09", g.Tabs)
	dec := *card.Totals().Decision
	snap, _ := json.Marshal(map[string]any{"month": "2026-09", "closed_at": "2026-10-14T19:00:00+03:00", "as_of": "2026-10-14T19:00:00+03:00",
		"decision": dec - 150, "verdict": "проходит", "candidate": 100.0, "source": "test", "totals": map[string]any{"stuck": 0.0},
		"lines": []any{map[string]any{"model": "Alfa", "channel": "Fansly", "earned": 1.0}}})
	tx, _ := s.RW.BeginTx(ctx, nil)
	if _, err := ImportClose(ctx, tx, snap); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	rows2, err := Drift(ctx, s.RO, cv, g.Tabs)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("%v %v", rows2, err)
	}
	if !rows2[0].Flag || rows2[0].Delta < 149 || rows2[0].Reason == "" {
		t.Fatalf("a $150 drift must be flagged: %+v", rows2[0])
	}
	if err := Accept(ctx, s.RW, rows2, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows3, err := Drift(ctx, s.RO, cv, g.Tabs)
	if err != nil || rows3[0].Delta != 0 || rows3[0].Reported != rows3[0].Now {
		t.Fatalf("accepted figure still differs: %+v %v", rows3, err)
	}
	if _, err := s.RW.Exec(`UPDATE periods SET decision = 1`); err == nil {
		t.Fatal("closed month changed")
	}
}

func TestParsePlans(t *testing.T) {
	text := "# x\n### План оборота 2026-11\n\nтекст\n\n| Страница | $ в день | Было |\n|---|---:|---:|\n| alfa-1 | 470 | 439 |\n| beta-of | 12.5 | 1 |\n\nдальше\n"
	rows, err := ParsePlans(text)
	if err != nil || len(rows) != 2 || rows[1].Page != "beta-of" || rows[1].PerDay != 12.5 {
		t.Fatalf("%+v %v", rows, err)
	}
	if _, err := ParsePlans("### План оборота 2026-11\n| alfa-1 | много |\n"); err == nil {
		t.Fatal("broken plan accepted")
	}
	if _, err := ParsePlans("### План оборота 2026-11\n\nнет таблицы\n### Другое\n"); err == nil {
		t.Fatal("plan without rows accepted")
	}
}
