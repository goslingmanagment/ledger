package payroll

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func m(s string) int64 { return int64(money.MustParse(s)) }

func prepare(t *testing.T, s *db.Store, slug, at string) PayoutBrief {
	t.Helper()
	b, err := PreparePayout(context.Background(), s.RO, slug, t0(at))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// balanceOf is the person's debt in v_payables.
func balanceOf(t *testing.T, s *db.Store, slug string) int64 {
	t.Helper()
	list, err := Payables(context.Background(), s.RO, slug)
	if err != nil || len(list) != 1 {
		t.Fatalf("v_payables %s: %+v %v", slug, list, err)
	}
	return list[0].BalanceMicros
}

// addsUp checks the lines against the amount: the pays to the micro, the shown cents to the shown total.
func addsUp(t *testing.T, b PayoutBrief) {
	t.Helper()
	var pay, shown int64
	for _, l := range b.Lines {
		pay += l.PayMicros
		shown += l.ShownMicros
		if l.ShownMicros%10_000 != 0 {
			t.Errorf("line %q shown %d: not whole cents", l.Text, l.ShownMicros)
		}
	}
	if pay != b.AmountMicros {
		t.Errorf("lines pay %d, amount %d", pay, b.AmountMicros)
	}
	if total := int64(money.RoundCents(money.Micros(b.AmountMicros))); shown != total {
		t.Errorf("lines shown %d, amount shown %d", shown, total)
	}
	if !strings.Contains(b.Message, "итого "+amt(int64(money.RoundCents(money.Micros(b.AmountMicros))))+" → "+TransferPlaceholder) {
		t.Errorf("message total:\n%s", b.Message)
	}
}

func flagKinds(b PayoutBrief) map[string]bool {
	out := map[string]bool{}
	for _, f := range b.Flags {
		out[f.Kind] = true
	}
	return out
}

// The payouts book: Альфа's 90 by the table paid 09-07…09-08 of both tabs and 5 of Beta's 09-10, so 15 of
// that day and Alpha's 09-11 are owed, 45 in all, while her tabs say 60.
func TestPrepareOwesTheBooksDaysWithAPartlyPaidDay(t *testing.T) {
	s, f := payoutsBook(t)
	ctx := t.Context()
	f.run(t, s, ctx, "2026-09-15T10:20:00+03:00") // the sheets fresh: no stale flag
	b := prepare(t, s, "alfa", "2026-09-15T10:30:00+03:00")
	if b.AmountMicros != balanceOf(t, s, "alfa") || b.AmountMicros != m("45") || b.Nothing != "" {
		t.Fatalf("amount %d, nothing %q", b.AmountMicros, b.Nothing)
	}
	if b.Period == nil || *b.Period != (PreparePeriod{From: "2026-09-10", To: "2026-09-11"}) {
		t.Fatalf("period %+v", b.Period)
	}
	base, rate := m("120"), 0.25
	want := []PrepareLine{
		{Kind: LineWeek, Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-11", To: "2026-09-11",
			Days:       []PrepareDay{{Date: "2026-09-11", Pages: []PayoutPage{{"page-1", m("120")}}, ValueMicros: m("120"), PayMicros: m("30")}},
			BaseMicros: &base, Rate: &rate, PayMicros: m("30"), ShownMicros: m("30"), Text: "11.09: 120 × 25% = 30"},
		{Kind: LineRemainder, Model: "Beta", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-10", To: "2026-09-10",
			Days:      []PrepareDay{{Date: "2026-09-10", Pages: []PayoutPage{{"page-2", m("80")}}, ValueMicros: m("80"), PayMicros: m("15")}},
			PayMicros: m("15"), ShownMicros: m("15"), Text: "остаток за 10.09: 15"},
	}
	if !reflect.DeepEqual(b.Lines, want) {
		t.Fatalf("lines\n got %+v\nwant %+v", b.Lines, want)
	}
	addsUp(t, b)
	wantBlocks := []PrepareBlock{
		{Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-11", To: "2026-09-11", WeekOver: true},
		{Model: "Beta", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-10", To: "2026-09-10", WeekOver: true},
	}
	if !reflect.DeepEqual(b.Blocks, wantBlocks) {
		t.Fatalf("blocks %+v", b.Blocks)
	}
	wantMsg := "закинул зп, {перевод} usdt\nпериод 10–11.09\n\nalpha\n11.09: 120 × 25% = 30\n\nbeta\nостаток за 10.09: 15\n\n" +
		"итого 45 → {перевод}\n\nчекни что дошло"
	if b.Message != wantMsg {
		t.Fatalf("message\n%s\nwant\n%s", b.Message, wantMsg)
	}
	if b.Suggestion == nil || b.Suggestion.TransferMicros != m("45") || b.Suggestion.AboveTableMicros != 0 {
		t.Fatalf("suggestion %+v", b.Suggestion)
	}
	// The debt is 15 below «К выплате» of the tabs: a blocker, and not in the message.
	kinds := flagKinds(b)
	if !kinds["to_pay"] || kinds["stale"] || b.Blockers == 0 || !b.Flags[0].Blocker {
		t.Fatalf("flags %+v", b.Flags)
	}
	if strings.Contains(b.Message, "60") {
		t.Fatalf("flag numbers in the message:\n%s", b.Message)
	}
}

// An opening debt is a line of its own; «не занесено» after the period is a flag and stays out of the message.
func TestPrepareOpeningAndUnentered(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(105, 0,
		week("2026-08-31", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}}),
		week("2026-09-07", map[string][7]any{"page-1": {200, 120, nil, nil, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	plan, from, err := PlanOpenings(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PostOpenings(ctx, tx, plan, from, t0("2026-09-14T10:10:00+03:00")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rebuild(t, s, ctx, "2026-09-14T10:20:00+03:00")
	if _, err := s.RW.ExecContext(ctx, `DELETE FROM payroll_unentered`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO payroll_unentered(model, name, person, date, page, window_from, window_to, status, reason, net_mills, share, checked_at)
		VALUES ('Alpha', 'Альфа', 'alfa', '2026-09-09', 'page-1', '2026-09-09T16:00:00+03:00', '2026-09-10T00:00:00+03:00', 'ok', NULL, 64000, 16.0, '2026-09-14T10:20:00+03:00'),
		       ('Alpha', 'Альфа', 'alfa', '2026-09-10', 'page-1', '2026-09-10T16:00:00+03:00', '2026-09-11T00:00:00+03:00', 'unknown', 'хаб не доказал', NULL, NULL, '2026-09-14T10:20:00+03:00')`); err != nil {
		t.Fatal(err)
	}
	b := prepare(t, s, "alfa", "2026-09-14T11:00:00+03:00")
	if b.AmountMicros != balanceOf(t, s, "alfa") || b.AmountMicros != m("105") {
		t.Fatalf("amount %d", b.AmountMicros)
	}
	if b.Period == nil || *b.Period != (PreparePeriod{From: "2026-08-31", To: "2026-09-08"}) {
		t.Fatalf("period %+v", b.Period)
	}
	var texts []string
	for _, l := range b.Lines {
		texts = append(texts, l.Kind+": "+l.Text)
	}
	if want := []string{"week: 07–08.09: 200+120 = 320 × 25% = 80", "opening: старый долг: +25 — дни до 01.09"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("lines %q", texts)
	}
	addsUp(t, b)
	wantMsg := "закинул зп, {перевод} usdt\nпериод 07–08.09\n\nalpha\n07–08.09: 200+120 = 320 × 25% = 80\nстарый долг: +25 — дни до 01.09\n" +
		"итого 105 → {перевод}\n\nчекни что дошло"
	if b.Message != wantMsg {
		t.Fatalf("message\n%s\nwant\n%s", b.Message, wantMsg)
	}
	var unentered []PrepareFlag
	for _, fl := range b.Flags {
		if strings.HasPrefix(fl.Kind, "unentered") {
			unentered = append(unentered, fl)
		}
	}
	if len(unentered) != 2 || !unentered[0].Blocker || !strings.Contains(unentered[0].Text, "09.09") || !strings.Contains(unentered[0].Text, "64.00") ||
		unentered[1].Blocker || !strings.Contains(unentered[1].Text, "10.09") {
		t.Fatalf("unentered %+v", unentered)
	}
	if b.Blockers != 1 || len(b.Flags) != 2 {
		t.Fatalf("flags %+v", b.Flags)
	}
	for _, x := range []string{"09.09", "10.09", "64", "16"} {
		if strings.Contains(b.Message, x) {
			t.Fatalf("message has %q:\n%s", x, b.Message)
		}
	}
}

// Quarter cents in two weeks: each week rounds honestly to its own cents, and the rounding line carries the
// positive remainder so the shown lines add up to the shown total without altering week formulas, FIFO, blocks
// or the message period.
func TestPrepareLinesAddUpToTheCent(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(47.835, 0,
		week("2026-09-07", map[string][7]any{"page-1": {123.45, nil, nil, nil, nil, nil, nil}}),
		week("2026-09-14", map[string][7]any{"page-1": {67.89, nil, nil, nil, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-21T10:00:00+03:00")
	b := prepare(t, s, "alfa", "2026-09-21T10:30:00+03:00")
	if b.AmountMicros != balanceOf(t, s, "alfa") || b.AmountMicros != m("47.835") || len(b.Lines) != 3 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	addsUp(t, b)
	if b.Lines[0].PayMicros != m("30.8625") || b.Lines[0].ShownMicros != m("30.86") || b.Lines[0].ExtraMicros != 0 ||
		b.Lines[0].Text != "07.09: 123.45 × 25% = 30.86" {
		t.Fatalf("line 0: %+v", b.Lines[0])
	}
	if b.Lines[1].PayMicros != m("16.9725") || b.Lines[1].ShownMicros != m("16.97") || b.Lines[1].ExtraMicros != 0 ||
		b.Lines[1].Text != "14.09: 67.89 × 25% = 16.97" {
		t.Fatalf("line 1: %+v", b.Lines[1])
	}
	wantRound := PrepareLine{Kind: LineRounding, PayMicros: 0, ShownMicros: m("0.01"), Text: "округление долей цента: +0.01"}
	if !reflect.DeepEqual(b.Lines[2], wantRound) {
		t.Fatalf("rounding line\n got %+v\nwant %+v", b.Lines[2], wantRound)
	}
	if b.Period == nil || *b.Period != (PreparePeriod{From: "2026-09-07", To: "2026-09-14"}) {
		t.Fatalf("period %+v", b.Period)
	}
	wantBlocks := []PrepareBlock{
		{Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-07", To: "2026-09-07", WeekOver: true},
		{Model: "Alpha", Tab: "Альфа", Week: "2026-09-14", From: "2026-09-14", To: "2026-09-14", WeekOver: true},
	}
	if !reflect.DeepEqual(b.Blocks, wantBlocks) {
		t.Fatalf("blocks %+v", b.Blocks)
	}
	wantMsg := "закинул зп, {перевод} usdt\nпериод 07–14.09\n\nalpha\n07.09: 123.45 × 25% = 30.86\n14.09: 67.89 × 25% = 16.97\n" +
		"округление долей цента: +0.01\nитого 47.84 → {перевод}\n\nчекни что дошло"
	if b.Message != wantMsg {
		t.Fatalf("message\n%s\nwant\n%s", b.Message, wantMsg)
	}
	if b.Suggestion == nil || b.Suggestion.TransferMicros != m("48") || b.Suggestion.AboveTableMicros != m("0.165") {
		t.Fatalf("suggestion %+v", b.Suggestion)
	}
	var fifoRows int
	if err := s.RO.QueryRowContext(ctx, `SELECT count(*) FROM payable_days WHERE person = 'alfa' AND unpaid > 0`).Scan(&fifoRows); err != nil {
		t.Fatal(err)
	}
	if fifoRows != 2 {
		t.Fatalf("payable_days rows %d, want 2", fifoRows)
	}
}

// Three weeks each rounding up by a quarter cent: their shown sum exceeds the rounded total by a cent,
// so the rounding line is negative (-0.01) while each week keeps its exact formula.
func TestPrepareNegativeRoundingRemainder(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(17.5225, 0,
		week("2026-09-07", map[string][7]any{"page-1": {40.03, nil, nil, nil, nil, nil, nil}}),
		week("2026-09-14", map[string][7]any{"page-1": {20.03, nil, nil, nil, nil, nil, nil}}),
		week("2026-09-21", map[string][7]any{"page-1": {10.03, nil, nil, nil, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-28T10:00:00+03:00")
	b := prepare(t, s, "alfa", "2026-09-28T10:30:00+03:00")
	if b.AmountMicros != balanceOf(t, s, "alfa") || b.AmountMicros != m("17.5225") || len(b.Lines) != 4 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	addsUp(t, b)
	if b.Lines[0].ShownMicros != m("10.01") || b.Lines[1].ShownMicros != m("5.01") || b.Lines[2].ShownMicros != m("2.51") {
		t.Fatalf("week showns %+v", b.Lines[:3])
	}
	wantRound := PrepareLine{Kind: LineRounding, PayMicros: 0, ShownMicros: m("-0.01"), Text: "округление долей цента: -0.01"}
	if !reflect.DeepEqual(b.Lines[3], wantRound) {
		t.Fatalf("rounding line\n got %+v\nwant %+v", b.Lines[3], wantRound)
	}
	if len(b.Blocks) != 3 {
		t.Fatalf("blocks %+v", b.Blocks)
	}
	wantMsg := "закинул зп, {перевод} usdt\nпериод 07–21.09\n\nalpha\n07.09: 40.03 × 25% = 10.01\n14.09: 20.03 × 25% = 5.01\n" +
		"21.09: 10.03 × 25% = 2.51\nокругление долей цента: -0.01\nитого 17.52 → {перевод}\n\nчекни что дошло"
	if b.Message != wantMsg {
		t.Fatalf("message\n%s\nwant\n%s", b.Message, wantMsg)
	}
}

// Fractional cents that cancel out across weeks: shown lines already equal the rounded total, so no rounding line appears.
func TestPrepareNoRoundingLineWhenLinesMatchTotal(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(5.01, 0,
		week("2026-09-07", map[string][7]any{"page-1": {10.01, nil, nil, nil, nil, nil, nil}}),   // 2.5025 -> 2.50
		week("2026-09-14", map[string][7]any{"page-1": {10.03, nil, nil, nil, nil, nil, nil}}))}) // 2.5075 -> 2.51
	f.run(t, s, ctx, "2026-09-21T10:00:00+03:00")
	b := prepare(t, s, "alfa", "2026-09-21T10:30:00+03:00")
	if b.AmountMicros != m("5.01") || len(b.Lines) != 2 {
		t.Fatalf("amount %d, lines %+v", b.AmountMicros, b.Lines)
	}
	for _, l := range b.Lines {
		if l.Kind == LineRounding {
			t.Fatalf("unexpected rounding line: %+v", l)
		}
	}
	addsUp(t, b)
	if strings.Contains(b.Message, "округление") || !strings.Contains(b.Message, "период 07–14.09\n") {
		t.Fatalf("message:\n%s", b.Message)
	}
}

// prepareMessage keeps the period intact when a rounding line has empty From/To, and prepareBlocks ignores rounding lines.
func TestPrepareMessageAndBlocksIgnoreRoundingLine(t *testing.T) {
	lines := []PrepareLine{
		{Kind: LineWeek, Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-07", To: "2026-09-08",
			PayMicros: m("30.8625"), ShownMicros: m("30.86"), Text: "07–08.09: 123.45 × 25% = 30.86"},
		{Kind: LineWeek, Model: "Beta", Tab: "Альфа", Week: "2026-09-14", From: "2026-09-14", To: "2026-09-14",
			PayMicros: m("16.9725"), ShownMicros: m("16.97"), Text: "14.09: 67.89 × 25% = 16.97"},
		{Kind: LineRounding, PayMicros: 0, ShownMicros: m("0.01"), Text: "округление долей цента: +0.01"},
	}
	msg := prepareMessage(lines, m("47.835"))
	wantMsg := "закинул зп, {перевод} usdt\nпериод 07–14.09\n\nalpha\n07–08.09: 123.45 × 25% = 30.86\n\nbeta\n14.09: 67.89 × 25% = 16.97\n\n" +
		"округление долей цента: +0.01\nитого 47.84 → {перевод}\n\nчекни что дошло"
	if msg != wantMsg {
		t.Fatalf("message\n%s\nwant\n%s", msg, wantMsg)
	}

	// Even if a rounding line carries a Week value, prepareBlocks must ignore it.
	withWeek := append(append([]PrepareLine{}, lines[:2]...), PrepareLine{
		Kind: LineRounding, Model: "Alpha", Tab: "Альфа", Week: "2026-09-21", From: "2026-09-21", To: "2026-09-21",
		ShownMicros: m("0.01"), Text: "округление долей цента: +0.01",
	})
	wantBlocks := []PrepareBlock{
		{Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-07", To: "2026-09-08", WeekOver: true},
		{Model: "Beta", Tab: "Альфа", Week: "2026-09-14", From: "2026-09-14", To: "2026-09-14", WeekOver: true},
	}
	if got := prepareBlocks(withWeek, "2026-09-28"); !reflect.DeepEqual(got, wantBlocks) {
		t.Fatalf("blocks\n got %+v\nwant %+v", got, wantBlocks)
	}
}

// Paid ahead: nothing to pay, no lines and no message.
func TestPrepareNegativeBalanceIsNothingToPay(t *testing.T) {
	s, ctx := openStore(t)
	f := newSheets("Alpha")
	f.set("Alpha", []string{"Альфа"}, map[string][][]Cell{"Альфа": sheet(25, 0, week("2026-09-07", map[string][7]any{"page-1": {100, nil, nil, nil, nil, nil, nil}}))})
	f.run(t, s, ctx, "2026-09-14T10:00:00+03:00")
	importJournal(t, s, ctx, "2026-09-15T09:00:00+03:00", payout(1, "2026-09-15", "Альфа", "Alpha", "40.00", "аванс"))
	rebuild(t, s, ctx, "2026-09-15T10:00:00+03:00")
	b := prepare(t, s, "alfa", "2026-09-15T10:30:00+03:00")
	if b.AmountMicros != balanceOf(t, s, "alfa") || b.AmountMicros != m("-15") {
		t.Fatalf("amount %d", b.AmountMicros)
	}
	if !strings.Contains(b.Nothing, "выплачено вперёд 15.00") || len(b.Lines) != 0 || len(b.Blocks) != 0 || b.Message != "" ||
		b.Period != nil || b.Suggestion != nil {
		t.Fatalf("brief %+v", b)
	}
}

func TestPreparePayoutRoute(t *testing.T) {
	s, _ := payoutsBook(t)
	ctx := t.Context()
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	key := func(name, scope string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, []string{scope}, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	hub, agent, mac := key("hub", auth.ScopePayouts), key("agent", auth.ScopeRead), key("mac", auth.ScopeCollect)
	status := func(err error) int {
		t.Helper()
		if err == nil {
			return http.StatusOK
		}
		e, ok := err.(*client.Error)
		if !ok {
			t.Fatalf("not an HTTP answer: %v", err)
		}
		return e.Status
	}
	want := prepare(t, s, "alfa", "2026-09-15T10:30:00+03:00")
	for name, c := range map[string]*client.Client{"payouts": hub, "read": agent} {
		var got PayoutBrief
		if err := c.Do(ctx, http.MethodGet, "/v1/payouts/prepare?person=alfa", nil, &got); err != nil {
			t.Fatalf("%s key: %v", name, err)
		}
		if !got.OK || got.Slug != "alfa" || got.AmountMicros != want.AmountMicros || !reflect.DeepEqual(got.Lines, want.Lines) || got.Message != want.Message {
			t.Fatalf("%s key: %+v", name, got)
		}
	}
	if code := status(mac.Do(ctx, http.MethodGet, "/v1/payouts/prepare?person=alfa", nil, nil)); code != http.StatusForbidden {
		t.Fatalf("collect key: %d", code)
	}
	for q, code := range map[string]int{
		"":                  http.StatusBadRequest,
		"?person=":          http.StatusBadRequest,
		"?person=Alfa":      http.StatusBadRequest,
		"?person=alfa%20x":  http.StatusBadRequest,
		"?person=nobody":    http.StatusNotFound,
		"?person=bravo":     http.StatusNotFound, // a card without a tab
		"?person=boss":      http.StatusNotFound, // the owner's debt: no tab
		"?person=alpha":     http.StatusNotFound, // a model's share of a deal
		"?person=alfa&x=1":  http.StatusOK,
		"?person=alfa&to=0": http.StatusOK,
	} {
		if got := status(hub.Do(ctx, http.MethodGet, "/v1/payouts/prepare"+q, nil, nil)); got != code {
			t.Errorf("%q: %d, want %d", q, got, code)
		}
	}
	// The payouts key still opens nothing else.
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sql"}, {http.MethodGet, "/v1/schema"}, {http.MethodGet, "/v1/payables"}, {http.MethodGet, "/v1/free"},
	} {
		var body any
		if r.method == http.MethodPost {
			body = map[string]any{"sql": "SELECT * FROM v_balances"}
		}
		if code := status(hub.Do(ctx, r.method, r.path, body, nil)); code != http.StatusForbidden {
			t.Errorf("payouts key on %s %s: %d, want 403", r.method, r.path, code)
		}
	}
}
