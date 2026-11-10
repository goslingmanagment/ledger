package pots_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/pots"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/ts"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, ts.MSK)

// book is a store with three cards (anna, boris, carl) and two Hub pages of model Alpha (page-a, page-b) and one of
// Beta (page-c), all synthetic.
func book(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := payroll.PutPeople(ctx, s.RW, []payroll.Person{
		{Slug: "anna", Name: "Анна", Aliases: []string{"anna_hub"}, Status: "active", Workdays: []string{}},
		{Slug: "boris", Name: "Борис", Aliases: []string{}, Status: "active", Workdays: []string{}},
		{Slug: "carl", Name: "Карл", Aliases: []string{}, Status: "active", Workdays: []string{}},
	}, "test", now); err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"page-a", "Alpha"}, {"page-b", "Alpha"}, {"page-c", "Beta"}} {
		if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
			VALUES (?, 'fansly', ?, ?, '2026-11-01')`, p[0], p[1], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	return s, ctx
}

func str(s string) *string { return &s }

// pot is a pot of page-a in November: anna and boris all month, carl from the 10th.
func pot() pots.DeclareInput {
	return pots.DeclareInput{Slug: "set-one", Name: "Набор", Model: "Alpha", Pages: []string{"page-a"}, StartsAt: "2026-11-01",
		EndsAt: str("2026-11-30"), DeclaredBy: "владелец", DeclaredAt: "2026-11-01T10:00:00+03:00", DecisionRef: "чат, 01.11",
		Recognise:    json.RawMessage(`{"words": ["набор"]}`),
		Participants: []pots.Participant{{Person: "anna"}, {Person: "boris"}, {Person: "carl", From: "2026-11-10"}}}
}

func refusal(t *testing.T, err error, part string) {
	t.Helper()
	var r *decisions.Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Msg, part) {
		t.Fatalf("want a refusal with %q, got %v", part, err)
	}
}

func mills(v int64) *int64 { return &v }

func add(t *testing.T, s *db.Store, ctx context.Context, in pots.SaleInput) (pots.SaleResult, error) {
	t.Helper()
	return pots.AddSale(ctx, s, "set-one", in, pots.SourceAgent, "agent", now)
}

func TestDeclareRefusesWhatIsNotThePotsWord(t *testing.T) {
	s, ctx := book(t)
	for _, c := range []struct {
		edit func(*pots.DeclareInput)
		part string
	}{
		{func(in *pots.DeclareInput) { in.Participants[1].Person = "nobody" }, "не человек"},
		{func(in *pots.DeclareInput) { in.Pages = []string{"page-c"} }, "модели Beta, а общак модели Alpha"},
		{func(in *pots.DeclareInput) { in.EndsAt = str("2026-10-01") }, "конец раньше начала"},
		{func(in *pots.DeclareInput) {
			in.Participants = append(in.Participants, pots.Participant{Person: "anna", From: "2026-11-20"})
		}, "сроки участия пересекаются"},
		{func(in *pots.DeclareInput) { in.Slug = "Set One" }, "код общака"},
		{func(in *pots.DeclareInput) { in.Participants = nil }, "хотя бы один участник"},
		{func(in *pots.DeclareInput) { in.DeclaredBy = "" }, "кто объявил"},
	} {
		in := pot()
		c.edit(&in)
		_, err := pots.Declare(ctx, s, in, "owner", now)
		refusal(t, err, c.part)
	}
	p, err := pots.Declare(ctx, s, pot(), "owner", now)
	if err != nil || p.Status != "open" || len(p.Participants) != 3 || p.Participants[0].From != "2026-11-01" || p.Participants[2].To != nil {
		t.Fatalf("declare %+v %v", p, err)
	}
	// The same slug again changes the pot, not a second one.
	in := pot()
	in.Name = "Набор (уточнено)"
	if p, err = pots.Declare(ctx, s, in, "owner", now); err != nil || p.Name != "Набор (уточнено)" {
		t.Fatalf("redeclare %+v %v", p, err)
	}
	if list, err := pots.List(ctx, s.RO); err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestSharesFollowTheParticipantsAndAddUpExactly(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	// Out of bounds: before, after, another page.
	_, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-x", PageLabel: "page-a", OccurredAt: "2026-10-31T23:59:00+03:00", NetMills: mills(1000)})
	refusal(t, err, "вне срока")
	// Moscow time decides: 21:30 UTC on the 30th is the 1st of December in Moscow.
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-x", PageLabel: "page-a", OccurredAt: "2026-11-30T21:30:00Z", NetMills: mills(1000)})
	refusal(t, err, "вне срока")
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-x", PageLabel: "page-b", OccurredAt: "2026-11-05T12:00:00+03:00", NetMills: mills(1000)})
	refusal(t, err, "не в общаке")
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-x"})
	refusal(t, err, "нужны page_label, net_mills, occurred_at")

	// Two people on the 5th, three from the 10th; nets that do not divide.
	for _, in := range []pots.SaleInput{
		{TransactionRef: "tx-1", PageLabel: "page-a", OccurredAt: "2026-11-05T12:00:00+03:00", NetMills: mills(10_001)},
		{TransactionRef: "tx-2", PageLabel: "page-a", OccurredAt: "2026-11-12T12:00:00+03:00", NetMills: mills(10_000)},
		{TransactionRef: "tx-3", PageLabel: "page-a", OccurredAt: "2026-11-30T23:59:00+03:00", NetMills: mills(1)},
		{PageLabel: "page-a", OccurredAt: "2026-11-01T00:00:00+03:00", NetMills: mills(5_000), FanName: "fan-x", Note: "мимо хаба"},
	} {
		if _, err := add(t, s, ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-2", PageLabel: "page-a", OccurredAt: "2026-11-12T12:00:00+03:00", NetMills: mills(10_000)})
	refusal(t, err, "уже в общаке set-one")

	sh, err := pots.Show(ctx, s.RO, "set-one")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sale := range sh.Sales {
		var sum int64
		var parts []string
		for _, x := range sale.Shares {
			sum += x.Micros
			parts = append(parts, x.Person+"="+itoa(x.Micros))
		}
		if sum != sale.NetMicros {
			t.Fatalf("sale %d: shares %d, net %d", sale.ID, sum, sale.NetMicros)
		}
		got[sale.Day+" "+itoa(sale.NetMicros)] = strings.Join(parts, " ")
	}
	for k, v := range map[string]string{
		"2026-11-05 10001000": "anna=5000500 boris=5000500",
		"2026-11-12 10000000": "anna=3333334 boris=3333333 carl=3333333",
		"2026-11-30 1000":     "anna=334 boris=333 carl=333",
		"2026-11-01 5000000":  "anna=2500000 boris=2500000",
	} {
		if got[k] != v {
			t.Fatalf("%s: %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	totals := map[string]int64{}
	for _, x := range sh.Totals {
		totals[x.Person] = x.Micros
	}
	if totals["anna"] != 5000500+3333334+334+2500000 || totals["carl"] != 3333333+333 {
		t.Fatalf("totals %+v", sh.Totals)
	}

	// Taken out, it stays a row; the same sale may come back.
	id := sh.Sales[2].ID
	if _, err := pots.RemoveSale(ctx, s, id, pots.RemoveInput{}, "agent", now); err == nil {
		t.Fatal("removed without a note")
	}
	if _, err := pots.RemoveSale(ctx, s, id, pots.RemoveInput{Note: "чаттер взял себе"}, "agent", now); err != nil {
		t.Fatal(err)
	}
	_, err = pots.RemoveSale(ctx, s, id, pots.RemoveInput{Note: "ещё раз"}, "agent", now)
	refusal(t, err, "уже снята")
	if sh, err = pots.Show(ctx, s.RO, "set-one"); err != nil || len(sh.Sales) != 3 || len(sh.Removed) != 1 || *sh.Removed[0].RemovedNote != "чаттер взял себе" {
		t.Fatalf("after remove %+v %v", sh, err)
	}
	if _, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-2", PageLabel: "page-a", OccurredAt: "2026-11-12T12:00:00+03:00",
		NetMills: mills(10_000)}); err != nil {
		t.Fatalf("re-add after remove: %v", err)
	}

	// A change of the pot that would leave a purchase outside is refused.
	in := pot()
	in.StartsAt = "2026-11-06"
	_, err = pots.Declare(ctx, s, in, "owner", now)
	refusal(t, err, "выпадает из нового объявления")

	// Closed: no new purchases, the shares stay.
	in = pot()
	in.Status = "closed"
	if _, err := pots.Declare(ctx, s, in, "owner", now); err != nil {
		t.Fatal(err)
	}
	_, err = add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-9", PageLabel: "page-a", OccurredAt: "2026-11-15T12:00:00+03:00", NetMills: mills(1000)})
	refusal(t, err, "закрыт")
}

func TestAPurchaseOnADayWithoutParticipantsHasNoShare(t *testing.T) {
	s, ctx := book(t)
	in := pot()
	in.Participants = []pots.Participant{{Person: "anna", From: "2026-11-10"}}
	if _, err := pots.Declare(ctx, s, in, "owner", now); err != nil {
		t.Fatal(err)
	}
	if _, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-1", PageLabel: "page-a", OccurredAt: "2026-11-02T12:00:00+03:00", NetMills: mills(3000)}); err != nil {
		t.Fatal(err)
	}
	sh, err := pots.Show(ctx, s.RO, "set-one")
	if err != nil || len(sh.Sales) != 1 || sh.Sales[0].Flag == nil || len(sh.Sales[0].Shares) != 0 || len(sh.Totals) != 0 {
		t.Fatalf("show %+v %v", sh, err)
	}
	var n int
	if err := s.RO.QueryRow(`SELECT COUNT(*) FROM v_pot_shares WHERE person IS NULL AND share_micros IS NULL AND flag <> ''`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("v_pot_shares flagged rows: %d %v", n, err)
	}
}

func TestThePurchaseFromHubsMirrorAndFromAReportLine(t *testing.T) {
	s, ctx := book(t)
	if _, err := pots.Declare(ctx, s, pot(), "owner", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at,
		post_tip, campaign, payload, first_seen_at, read_at) VALUES ('tx-h', '2026-11-09T00:00:00+03:00', 'page-a', 'fansly', 'message_purchase',
		12000, '2026-11-11T09:00:00Z', 0, 0, '{"fanRef": "fan-7"}', '2026-11-11', '2026-11-11')`); err != nil {
		t.Fatal(err)
	}
	// What the mirror says it takes; what contradicts it is refused.
	_, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-h", NetMills: mills(11_000)})
	refusal(t, err, "net_mills")
	out, err := add(t, s, ctx, pots.SaleInput{TransactionRef: "tx-h", FanName: "nick-7"})
	if err != nil {
		t.Fatal(err)
	}
	if v := out.Sale; v.PageLabel != "page-a" || v.NetMicros != 12_000_000 || v.OccurredAt != "2026-11-11T12:00:00+03:00" || v.FanRef != "fan-7" ||
		v.FanName != "nick-7" || v.Source != "agent" {
		t.Fatalf("from the mirror: %+v", v)
	}

	// A report: line 1 held out as a pot purchase, line 2 counted in the chatter's page, line 3 held out for another reason.
	raw, _ := json.Marshal(map[string]any{
		"hub_username": "anna_hub", "report_date": "2026-11-12", "window_from": "2026-11-12T16:00:00+03:00",
		"window_to": "2026-11-13T00:00:00+03:00", "cut_at": "2026-11-12T23:50:00+03:00", "source": "agent", "total_mills": 7000,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 7000}},
		"lines": []any{
			map[string]any{"n": 1, "transaction_ref": "tx-c1", "page": "page-a", "fan_ref": "fan-1", "fan_name": "nick-1",
				"occurred_at": "2026-11-12T18:00:00+03:00", "net_mills": 4000, "counted": false, "flags": []any{"Общак: набор, не в итоге"}},
			map[string]any{"n": 2, "transaction_ref": "tx-c2", "page": "page-a", "occurred_at": "2026-11-12T19:00:00+03:00",
				"net_mills": 7000, "counted": true},
			map[string]any{"n": 3, "transaction_ref": "tx-c3", "page": "page-a", "net_mills": 2000, "counted": false, "flags": []any{"чужой PPV"}},
		},
	})
	saved, err := reports.Save(ctx, s, raw, "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pots.Candidates(ctx, s.RO, "")
	if err != nil || len(c) != 1 || *c[0].TransactionRef != "tx-c1" || *c[0].N != 1 || strings.Join(c[0].Pots, ",") != "set-one" {
		t.Fatalf("candidates %+v %v", c, err)
	}
	if c, err := pots.Candidates(ctx, s.RO, "nope"); !errors.Is(err, pots.ErrNotFound) {
		t.Fatalf("candidates of no pot: %+v %v", c, err)
	}
	_, err = add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved.ID, N: 9}})
	refusal(t, err, "нет продажи с номером 9")
	out, err = add(t, s, ctx, pots.SaleInput{FromLine: &pots.LineRef{ReportID: saved.ID, N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if v := out.Sale; v.Source != "report_line" || *v.TransactionRef != "tx-c1" || v.NetMicros != 4_000_000 || v.FanName != "nick-1" || *v.ReportN != 1 {
		t.Fatalf("from the line: %+v", v)
	}
	if c, err := pots.Candidates(ctx, s.RO, "set-one"); err != nil || len(c) != 0 {
		t.Fatalf("candidates after the add: %+v %v", c, err)
	}

	// A sale the chatter counted in her own page stays hers: refused, accepted with force and a warning naming the report.
	in := pots.SaleInput{TransactionRef: "tx-c2", PageLabel: "page-a", OccurredAt: "2026-11-12T19:00:00+03:00", NetMills: mills(7000)}
	_, err = add(t, s, ctx, in)
	refusal(t, err, "учтена в отчёте")
	in.Force = true
	out, err = add(t, s, ctx, in)
	if err != nil || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "отчёте "+itoa(saved.ID)+" (anna, 2026-11-12) строкой 2") {
		t.Fatalf("forced %+v %v", out, err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
