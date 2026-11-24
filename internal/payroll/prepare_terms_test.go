package payroll

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
)

// All names, IDs, pages, pots, and amounts below are synthetic.

func sheetWithTerms(kv, paid, rate, fix float64, weeks ...[][]Cell) [][]Cell {
	h := []Cell{"К выплате:", "", "Заработано:", "", "Выплачено:", "", "Отраб. дней:", "", "Средн./смена:", "", "Лучший день:", "", "Ставка %", "Фикс/см $"}
	v := []Cell{n(kv), "", n(0), "", n(paid), "", n(0), "", n(0), "", n(0), "", n(rate), n(fix)}
	rows := [][]Cell{h, v, {}}
	for _, w := range weeks {
		rows = append(rows, w...)
	}
	return rows
}

func weekWithRate(start string, rate, fix float64, pages map[string][7]any) [][]Cell {
	s := serialOf(start)
	hdr := []Cell{"Работяга", "Страница"}
	for k := range 7 {
		hdr = append(hdr, n(s+k))
	}
	rows := [][]Cell{append(hdr, "Итого за неделю", "З/П", "Выплачено", "Фикс")}
	total := 0.0
	names := make([]string, 0, len(pages))
	for p := range pages {
		names = append(names, p)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for idx, p := range names {
		row := []Cell{"Кто-то", p}
		rev := 0.0
		for _, c := range pages[p] {
			if c == nil {
				row = append(row, "")
				continue
			}
			row = append(row, n(c))
			if f, ok := asNum(n(c)); ok {
				rev += f.v
			}
		}
		row = append(row, n(rev), n(rev*rate))
		if idx == 0 && fix != 0 {
			row = append(row, "нет", n(fix))
		}
		total += rev * rate
		rows = append(rows, row)
	}
	total += fix
	sum := []Cell{"", "Сумма", n(0), n(0), n(0), n(0), n(0), n(0), n(0), n(0), n(total)}
	return append(rows, sum, []Cell{})
}

func putHubTerm(t *testing.T, s *db.Store, hubID, personID int64, person, modelSlug, validFrom, validTo string, shareBP, fixMicros int64) {
	t.Helper()
	var to any
	if validTo != "" {
		to = validTo
	}
	execSQL(t, s, `INSERT INTO hub_pay_terms(hub_id, hub_person_id, person, model_slug, model_name, valid_from, valid_to,
		share_bp, fix_micros, updated_at, stored_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '2026-11-01T00:00:00+03:00', '2026-11-01T00:00:00+03:00')`,
		hubID, personID, person, modelSlug, modelSlug, validFrom, to, shareBP, fixMicros)
}

func putReportAccrual(t *testing.T, s *db.Store, person, model, tabName, day string, pages []PayoutPage, pots []PayoutPot, rate float64, fixMicros, amountMicros int64) {
	t.Helper()
	type rp struct {
		Page   string `json:"page"`
		Micros int64  `json:"micros"`
	}
	type pp struct {
		Pot    string `json:"pot"`
		Name   string `json:"name"`
		Micros int64  `json:"micros"`
	}
	var net, potNet int64
	rps := make([]rp, 0, len(pages))
	for _, p := range pages {
		rps = append(rps, rp{Page: p.Page, Micros: p.RevenueMicros})
		net += p.RevenueMicros
	}
	pps := make([]pp, 0, len(pots))
	for _, p := range pots {
		pps = append(pps, pp{Pot: p.Pot, Name: p.Name, Micros: p.RevenueMicros})
		potNet += p.RevenueMicros
		net += p.RevenueMicros
	}
	pb, _ := json.Marshal(rps)
	potb, _ := json.Marshal(pps)
	execSQL(t, s, `INSERT INTO report_accruals(person, model, day, week, pages, posts, net_micros, tab_model, tab_name,
		rate, fix_micros, amount_micros, pot_micros, pots, computed_at)
		VALUES (?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, ?, ?, ?, '2026-11-16T10:00:00+03:00')`,
		person, model, day, weekOfDate(day), string(pb), net, model, tabName, rate, fixMicros, amountMicros, potNet, string(potb))
}

// Mid-week rate and shift-fix changes under Hub terms split the week block into clean lines with exact
// formulas (ignoring stale sheet header terms) and raise no false formula mismatch flag.
func TestPrepareHubTermsMidWeekRateAndFixSplit(t *testing.T) {
	s, ctx := openStore(t)
	execSQL(t, s, `INSERT INTO settings(key, value, updated_at) VALUES ('hub_model_slugs', '{"alpha-hub":"Alpha"}', '2026-11-01')`)
	// Sheet header has stale 20% and $10 fix; Hub terms override from 2026-11-09.
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{
		"Альфа": sheetWithTerms(0, 0, 0.20, 10, weekWithRate("2026-11-09", 0.20, 0, map[string][7]any{"page-1": {nil, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-16T10:00:00+03:00")
	if err := SetReportsFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetTermsHubFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	// Three Hub terms in the same week: 25%/0 -> 30%/0 -> 30%/15.
	putHubTerm(t, s, 1, 10, "alfa", "alpha-hub", "2026-11-02", "2026-11-10", 2500, 0)
	putHubTerm(t, s, 2, 10, "alfa", "alpha-hub", "2026-11-11", "2026-11-12", 3000, 0)
	putHubTerm(t, s, 3, 10, "alfa", "alpha-hub", "2026-11-13", "", 3000, m("15"))

	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-09", []PayoutPage{{"page-1", m("100")}}, nil, 0.25, 0, m("25"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-10", []PayoutPage{{"page-1", m("40")}}, nil, 0.25, 0, m("10"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-11", []PayoutPage{{"page-1", m("80")}}, nil, 0.30, 0, m("24"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-12", []PayoutPage{{"page-1", m("20")}}, nil, 0.30, 0, m("6"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-13", []PayoutPage{{"page-1", m("50")}}, nil, 0.30, m("15"), m("30"))
	rebuild(t, s, ctx, "2026-11-16T10:10:00+03:00")

	b := prepare(t, s, "alfa", "2026-11-16T10:30:00+03:00")
	if b.AmountMicros != m("95") || len(b.Lines) != 3 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	addsUp(t, b)
	wantTexts := []string{
		"09–10.11: 100+40 = 140 × 25% = 35",
		"11–12.11: 80+20 = 100 × 30% = 30",
		"13.11: 50 × 30% + фикс 15 = 30",
	}
	wantRates := []float64{0.25, 0.30, 0.30}
	wantExtra := []int64{0, 0, m("15")}
	for i, l := range b.Lines {
		if l.Kind != LineWeek || l.Text != wantTexts[i] || l.Rate == nil || *l.Rate != wantRates[i] || l.ExtraMicros != wantExtra[i] {
			t.Fatalf("line %d: got %+v, want text=%q rate=%v extra=%d", i, l, wantTexts[i], wantRates[i], wantExtra[i])
		}
	}
	wantBlocks := []PrepareBlock{{Model: "Alpha", Tab: "Альфа", Week: "2026-11-09", From: "2026-11-09", To: "2026-11-13", WeekOver: true}}
	if !reflect.DeepEqual(b.Blocks, wantBlocks) {
		t.Fatalf("blocks %+v, want %+v", b.Blocks, wantBlocks)
	}
	if got := flagsOfKind(b, "formula"); len(got) != 0 {
		t.Fatalf("unexpected formula flags: %+v", got)
	}
	if got := flagsOfKind(b, "no_rate"); len(got) != 0 {
		t.Fatalf("unexpected no_rate flags: %+v", got)
	}
}

// Days before payroll_terms_hub_from keep the sheet header's rate and fix; days on or after it take Hub's
// terms even when the sheet header has a different/stale rate and fix. A held sheet day stays on the header.
func TestPrepareHubTermsCutoffRespectsTermsFrom(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{
		// 2026-11-13 has a sheet cell and no report, so it is held as a sheet day (50 * 20% + 10 = 20).
		"Альфа": sheetWithTerms(20, 0, 0.20, 10, weekWithRate("2026-11-09", 0.20, 10, map[string][7]any{"page-1": {nil, nil, nil, nil, 50, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-16T10:00:00+03:00")
	if err := SetReportsFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetTermsHubFrom(ctx, s.RW, "2026-11-11", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	// Hub term valid from 2026-11-01 (before terms_hub_from): must NOT apply to 09.11 or 10.11.
	putHubTerm(t, s, 1, 10, "alfa", "alpha", "2026-11-01", "", 3000, 0)

	// 09–10.11 (< termsFrom): accrued at sheet header (20% + $10 fix).
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-09", []PayoutPage{{"page-1", m("50")}}, nil, 0.20, m("10"), m("20"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-10", []PayoutPage{{"page-1", m("100")}}, nil, 0.20, m("10"), m("30"))
	// 11–12.11 (>= termsFrom): accrued at Hub terms (30%, no fix).
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-11", []PayoutPage{{"page-1", m("60")}}, nil, 0.30, 0, m("18"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-12", []PayoutPage{{"page-1", m("40")}}, nil, 0.30, 0, m("12"))
	rebuild(t, s, ctx, "2026-11-16T10:10:00+03:00")

	b := prepare(t, s, "alfa", "2026-11-16T10:30:00+03:00")
	if b.AmountMicros != m("100") || len(b.Lines) != 3 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	addsUp(t, b)
	wantTexts := []string{
		"09–10.11: 50+100 = 150 × 20% + фикс 20 = 50",
		"11–12.11: 60+40 = 100 × 30% = 30",
		"13.11: 50 × 20% + фикс 10 = 20",
	}
	for i, l := range b.Lines {
		if l.Text != wantTexts[i] {
			t.Fatalf("line %d: got %q, want %q", i, l.Text, wantTexts[i])
		}
	}
	if got := flagsOfKind(b, "formula"); len(got) != 0 {
		t.Fatalf("unexpected formula flags: %+v", got)
	}

	// With payroll_terms_hub_from unset, all days use the sheet header (20% + $10 fix) even though Hub has 30%/0.
	if err := SetTermsHubFrom(ctx, s.RW, "", t0("2026-11-16T10:20:00+03:00")); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, `UPDATE report_accruals SET rate = 0.20, fix_micros = ?, amount_micros = ? WHERE person = 'alfa' AND day = '2026-11-11'`, m("10"), m("22"))
	execSQL(t, s, `UPDATE report_accruals SET rate = 0.20, fix_micros = ?, amount_micros = ? WHERE person = 'alfa' AND day = '2026-11-12'`, m("10"), m("18"))
	rebuild(t, s, ctx, "2026-11-16T10:25:00+03:00")

	bOff := prepare(t, s, "alfa", "2026-11-16T10:30:00+03:00")
	addsUp(t, bOff)
	if len(bOff.Lines) != 2 || bOff.Lines[0].Text != "09–12.11: 50+100+60+40 = 250 × 20% + фикс 40 = 90" ||
		bOff.Lines[1].Text != "13.11: 50 × 20% + фикс 10 = 20" {
		t.Fatalf("terms-from off lines: %+v", bOff.Lines)
	}
	if got := flagsOfKind(bOff, "formula"); len(got) != 0 {
		t.Fatalf("unexpected formula flags with terms-from off: %+v", got)
	}
}

// A day with only a pot share (no shift/report pages) gets no shift fix, both alone and alongside a
// worked shift day in the same week; a real mismatch against expected shift fixes still raises formula.
func TestPreparePotOnlyDayGetsNoShiftFix(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа", "Браво"}, map[string][][]Cell{
		"Альфа": sheetWithTerms(0, 0, 0.25, 0, weekWithRate("2026-11-09", 0.25, 0, map[string][7]any{"page-1": {nil, nil, nil, nil, nil, nil, nil}})),
		"Браво": sheetWithTerms(0, 0, 0.25, 0, weekWithRate("2026-11-09", 0.25, 0, map[string][7]any{"page-1": {nil, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-16T10:00:00+03:00")
	if err := SetReportsFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetTermsHubFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	putHubTerm(t, s, 1, 10, "alfa", "alpha", "2026-11-01", "", 2000, m("10"))
	putHubTerm(t, s, 2, 11, "bravo", "alpha", "2026-11-01", "", 2000, m("10"))

	// Alfa: pot-only day alone in the week (4 * 20% = 0.80, no fix).
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-11", nil, []PayoutPot{{Pot: "pot-one", Name: "Набор один", RevenueMicros: m("4")}}, 0.20, 0, m("0.80"))
	// Bravo: worked day with page + pot (50 + 10) * 20% + 10 fix = 22, plus pot-only day 4 * 20% = 0.80 (no fix).
	putReportAccrual(t, s, "bravo", "Alpha", "Браво", "2026-11-10",
		[]PayoutPage{{"page-1", m("50")}}, []PayoutPot{{Pot: "pot-one", Name: "Набор один", RevenueMicros: m("10")}}, 0.20, m("10"), m("22"))
	putReportAccrual(t, s, "bravo", "Alpha", "Браво", "2026-11-11",
		nil, []PayoutPot{{Pot: "pot-one", Name: "Набор один", RevenueMicros: m("4")}}, 0.20, 0, m("0.80"))
	rebuild(t, s, ctx, "2026-11-16T10:10:00+03:00")

	ba := prepare(t, s, "alfa", "2026-11-16T10:30:00+03:00")
	addsUp(t, ba)
	if len(ba.Lines) != 1 || ba.Lines[0].ExtraMicros != 0 ||
		ba.Lines[0].Text != "11.11: 4 × 20% = 0.80 (в т.ч. общак Набор один: база 4)" {
		t.Fatalf("alfa lines: %+v", ba.Lines)
	}
	if got := flagsOfKind(ba, "formula"); len(got) != 0 {
		t.Fatalf("alfa pot-only day triggered false formula flag: %+v", got)
	}

	bb := prepare(t, s, "bravo", "2026-11-16T10:30:00+03:00")
	addsUp(t, bb)
	if len(bb.Lines) != 1 || bb.Lines[0].ExtraMicros != m("10") ||
		bb.Lines[0].Text != "10–11.11: 60+4 = 64 × 20% + фикс 10 = 22.80 (в т.ч. общак Набор один 10.11: база 10; общак Набор один 11.11: база 4)" {
		t.Fatalf("bravo lines: %+v", bb.Lines)
	}
	if got := flagsOfKind(bb, "formula"); len(got) != 0 {
		t.Fatalf("bravo worked+pot-only week triggered false formula flag: %+v", got)
	}

	// Real mismatch 1: if the pot-only day erroneously carried a shift fix (+$10), formula MUST flag it.
	execSQL(t, s, `UPDATE report_accruals SET fix_micros = ?, amount_micros = ? WHERE person = 'alfa' AND day = '2026-11-11'`, m("10"), m("10.80"))
	rebuild(t, s, ctx, "2026-11-16T10:15:00+03:00")
	baBad := prepare(t, s, "alfa", "2026-11-16T10:30:00+03:00")
	if got := flagsOfKind(baBad, "formula"); len(got) != 1 || !strings.Contains(got[0].Text, "+10") {
		t.Fatalf("expected formula mismatch flag for wrongly fixed pot-only day, got %+v", got)
	}

	// Real mismatch 2: if a worked day is missing its expected shift fix (-$10), formula MUST flag it.
	execSQL(t, s, `UPDATE report_accruals SET fix_micros = 0, amount_micros = ? WHERE person = 'bravo' AND day = '2026-11-10'`, m("12"))
	rebuild(t, s, ctx, "2026-11-16T10:20:00+03:00")
	bbBad := prepare(t, s, "bravo", "2026-11-16T10:30:00+03:00")
	if got := flagsOfKind(bbBad, "formula"); len(got) != 1 || !strings.Contains(got[0].Text, "+ фикс 10 = 22.80") || !strings.Contains(got[0].Text, "-10") {
		t.Fatalf("expected formula mismatch flag for missing shift fix, got %+v", got)
	}
}

// A person on a model with no Google Sheet tab (registry-only tab «реестр <код>») gets clean rate and
// fix formulas from HubTerms.On, with no no_rate or formula flags.
func TestPrepareRegistryOnlyTabUsesHubTerms(t *testing.T) {
	s, ctx := openStore(t)
	if err := SetReportsFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetTermsHubFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, `INSERT INTO hub_people(hub_id, code, kind, role, state, display_name, logins, updated_at, stored_at)
		VALUES (15, 'delta', 'human', 'chatter', 'working', 'Дельта', '[105]', '2026-11-01T00:00:00+03:00', '2026-11-01T00:00:00+03:00')`)
	putHubTerm(t, s, 20, 15, "delta", "alpha", "2026-11-02", "2026-11-11", 2500, m("10"))
	putHubTerm(t, s, 21, 15, "delta", "alpha", "2026-11-12", "", 3000, 0)

	tab := RegistryTabName("delta")
	putReportAccrual(t, s, "delta", "alpha", tab, "2026-11-10", []PayoutPage{{"page-1", m("60")}}, nil, 0.25, m("10"), m("25"))
	putReportAccrual(t, s, "delta", "alpha", tab, "2026-11-12", []PayoutPage{{"page-1", m("40")}}, nil, 0.30, 0, m("12"))
	rebuild(t, s, ctx, "2026-11-16T10:10:00+03:00")

	b := prepare(t, s, "delta", "2026-11-16T10:30:00+03:00")
	if b.AmountMicros != m("37") || len(b.Lines) != 2 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	addsUp(t, b)
	if b.Lines[0].Tab != tab || b.Lines[0].Text != "10.11: 60 × 25% + фикс 10 = 25" || b.Lines[0].ExtraMicros != m("10") {
		t.Fatalf("line 0: %+v", b.Lines[0])
	}
	if b.Lines[1].Tab != tab || b.Lines[1].Text != "12.11: 40 × 30% = 12" || b.Lines[1].ExtraMicros != 0 {
		t.Fatalf("line 1: %+v", b.Lines[1])
	}
	if got := flagsOfKind(b, "no_rate"); len(got) != 0 {
		t.Fatalf("registry-only tab triggered no_rate: %+v", got)
	}
	if got := flagsOfKind(b, "formula"); len(got) != 0 {
		t.Fatalf("registry-only tab triggered formula: %+v", got)
	}
}

// Mid-week rate split with fractional cents: each split line rounds honestly to its own cents and the
// rounding line carries the remainder so the shown lines still add up to the shown total to the cent.
func TestPrepareHubTermsSplitAddsUpWithRoundingLine(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{
		"Альфа": sheetWithTerms(0, 0, 0.20, 0, weekWithRate("2026-11-09", 0.20, 0, map[string][7]any{"page-1": {nil, nil, nil, nil, nil, nil, nil}})),
	})
	f.run(t, s, ctx, "2026-11-16T10:00:00+03:00")
	if err := SetReportsFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := SetTermsHubFrom(ctx, s.RW, "2026-11-09", t0("2026-11-16T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	putHubTerm(t, s, 1, 10, "alfa", "alpha", "2026-11-02", "2026-11-10", 2500, 0)
	putHubTerm(t, s, 2, 10, "alfa", "alpha", "2026-11-11", "", 3000, 0)

	// 10.01 * 25% = 2.5025 (shown 2.50) + 10.01 * 30% = 3.003 (shown 3.00) = 5.5055 (total shown 5.51 -> rounding +0.01).
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-09", []PayoutPage{{"page-1", m("10.01")}}, nil, 0.25, 0, m("2.5025"))
	putReportAccrual(t, s, "alfa", "Alpha", "Альфа", "2026-11-11", []PayoutPage{{"page-1", m("10.01")}}, nil, 0.30, 0, m("3.003"))
	rebuild(t, s, ctx, "2026-11-16T10:10:00+03:00")

	b := prepare(t, s, "alfa", "2026-11-16T10:30:00+03:00")
	if b.AmountMicros != m("5.5055") || len(b.Lines) != 3 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	addsUp(t, b)
	if b.Lines[0].Text != "09.11: 10.01 × 25% = 2.50" || b.Lines[0].ShownMicros != m("2.50") || b.Lines[0].ExtraMicros != 0 {
		t.Fatalf("line 0: %+v", b.Lines[0])
	}
	if b.Lines[1].Text != "11.11: 10.01 × 30% = 3" || b.Lines[1].ShownMicros != m("3") || b.Lines[1].ExtraMicros != 0 {
		t.Fatalf("line 1: %+v", b.Lines[1])
	}
	wantRound := PrepareLine{Kind: LineRounding, PayMicros: 0, ShownMicros: m("0.01"), Text: "округление долей цента: +0.01"}
	if !reflect.DeepEqual(b.Lines[2], wantRound) {
		t.Fatalf("rounding line\n got %+v\nwant %+v", b.Lines[2], wantRound)
	}
	if got := flagsOfKind(b, "formula"); len(got) != 0 {
		t.Fatalf("unexpected formula flags: %+v", got)
	}
	wantBlocks := []PrepareBlock{{Model: "Alpha", Tab: "Альфа", Week: "2026-11-09", From: "2026-11-09", To: "2026-11-11", WeekOver: true}}
	if !reflect.DeepEqual(b.Blocks, wantBlocks) {
		t.Fatalf("blocks %+v, want %+v", b.Blocks, wantBlocks)
	}
}
