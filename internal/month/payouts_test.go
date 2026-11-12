package month

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// GET /v1/payouts/month/{month}: the hourly card of a month for Hub's owner screen, scope payouts only — the money of the
// pages and where it is, never the expenses, the decision, the requests or the transfers.

func monthDB(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func putCard(t *testing.T, s *db.Store, month, status, reason string, card any) {
	t.Helper()
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.Exec(`INSERT INTO month_cards(month, as_of, computed_at, status, reason, card) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(month) DO UPDATE SET status = excluded.status, reason = excluded.reason, card = excluded.card`,
		month, "2026-11-12T15:41:00+03:00", "2026-11-12T16:38:20+03:00", status, reason, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// inventedCard is a card the way Card.Snapshot stores it, of an invented agency: model Alfa on two platforms and a page
// whose terms are not agreed, model Bravo with money past the platforms; the rest of the card is there to stay out.
func inventedCard(checksOK bool) map[string]any {
	return map[string]any{
		"month": "2026-11", "as_of": "2026-11-12T15:41:00+03:00", "ledger_n": 420,
		"totals": map[string]any{"net": 1500.5, "earned": 900.25, "arrived": 300.1, "riding": 50.15, "page": 400, "hold": 150, "stuck": 0,
			"transit": 600.15, "decision": 123.45},
		"lines": []any{
			map[string]any{"model": "Alfa", "channel": "Fansly", "net": 1000.5, "earned": 600.25, "arrived": 300.1, "riding": 50.15, "page": 100,
				"hold": 150, "stuck": 0},
			map[string]any{"model": "Alfa", "channel": "alfa-x: не согласовано", "net": 77.7},
			map[string]any{"model": "Alfa", "channel": "OF", "net": 300, "earned": 200, "arrived": 0, "riding": 0, "page": 200, "hold": 0, "stuck": 0},
			map[string]any{"model": "Bravo", "channel": "вне платформ", "net": 200, "earned": 100, "arrived": 100},
		},
		"decision":  123.45,
		"expenses":  map[string]any{"total": 777.0, "chat": map[string]any{"Alfa": 500.0}, "extra": []any{[]any{1, 50.0, "Альфа"}}},
		"requests":  []any{map[string]any{"page": "alfa-x", "amount": 10.0}},
		"transfers": []any{map[string]any{"n": 7, "amount": 1000.0}},
		"pages":     map[string]any{"alfa-x 2026-11-01": 10.0},
		"checks":    []any{map[string]any{"n": 1, "ok": true, "msg": "части платежей = net"}, map[string]any{"n": 2, "ok": checksOK, "msg": "перевод #7 …"}},
		"notes":     []any{"alfa-x: страница без строки в таблице маршрутов — условия не согласованы, в цифру не входит"},
	}
}

func TestThePayoutsMonthIsTheHourlyCardsPagesMoney(t *testing.T) {
	s := monthDB(t)
	ctx := context.Background()
	putCard(t, s, "2026-11", "ok", "", inventedCard(true))
	got, err := ReadPayoutsMonth(ctx, s.RO, "2026-11")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	want := `{"ok":true,"month":"2026-11","asOf":"2026-11-12T15:41:00+03:00","computedAt":"2026-11-12T16:38:20+03:00","status":"proven",` +
		`"reason":null,"pagesNet":1378200000,"offPlatform":200000000,"turnover":1578200000,"earned":900250000,"arrived":300100000,` +
		`"riding":50150000,"hold":150000000,"stuck":0,"onPage":400000000,"models":[` +
		`{"model":"Alfa","pagesNet":1378200000,"offPlatform":0,"earned":800250000,"arrived":300100000,"riding":50150000},` +
		`{"model":"Bravo","pagesNet":0,"offPlatform":200000000,"earned":100000000,"arrived":100000000,"riding":0}],` +
		`"notes":["alfa-x: страница без строки в таблице маршрутов — условия не согласованы, в цифру не входит"]}`
	if string(raw) != want {
		t.Fatalf("answer\n%s\nwant\n%s", raw, want)
	}
	for _, never := range []string{"decision", "expenses", "requests", "transfers", "777", "123450000", "перевод"} {
		if strings.Contains(string(raw), never) {
			t.Fatalf("%q in the answer: %s", never, raw)
		}
	}

	// A failed check: the figures stay, the month is not proven.
	putCard(t, s, "2026-11", "ok", "", inventedCard(false))
	if got, _ := ReadPayoutsMonth(ctx, s.RO, "2026-11"); got.Status != MonthUnknown || got.Reason == nil ||
		*got.Reason != "проверки карточки не пройдены (№ 2): цифры не для решений — ledger month show" || got.Earned == nil {
		t.Fatalf("failed check: %+v", got)
	}
	// The job could not prove the cut: unknown with its reason, the last figures kept.
	putCard(t, s, "2026-11", "unknown", "хаб: срез не доказан", inventedCard(true))
	if got, _ := ReadPayoutsMonth(ctx, s.RO, "2026-11"); got.Status != MonthUnknown || *got.Reason != "хаб: срез не доказан" || *got.Turnover != 1578200000 {
		t.Fatalf("unknown cut: %+v", got)
	}
	// Not computable: no figures at all.
	putCard(t, s, "2026-11", "unknown", "таблица маршрутов: …", map[string]any{})
	if got, _ := ReadPayoutsMonth(ctx, s.RO, "2026-11"); got.Status != MonthUnknown || got.Earned != nil || got.PagesNet != nil || len(got.Models) != 0 {
		t.Fatalf("no figures: %+v", got)
	}
	// Never computed: unknown, said why.
	if got, err := ReadPayoutsMonth(ctx, s.RO, "2026-12"); err != nil || got.Status != MonthUnknown || got.AsOf != nil || got.Reason == nil ||
		!strings.Contains(*got.Reason, "не посчитана") {
		t.Fatalf("never computed: %+v %v", got, err)
	}
}

// The card the port computes, stored as the hourly job stores it: the reader takes its totals and lines by their keys.
func TestThePayoutsMonthReadsTheSnapshotAsTheCardWritesIt(t *testing.T) {
	g, rows := loadGolden(t)
	cv, err := BuildCover(goldenConfig(g), fakeSource{g}, rows, parseT(g.AsOf))
	if err != nil {
		t.Fatal(err)
	}
	card, err := BuildCard(cv, "2026-09", g.Tabs)
	if err != nil {
		t.Fatal(err)
	}
	snap := card.Snapshot()
	snap["checks"] = card.Checks("")
	snap["notes"] = card.Notes
	s := monthDB(t)
	putCard(t, s, "2026-09", "ok", "", snap)
	got, err := ReadPayoutsMonth(context.Background(), s.RO, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	tot := card.Totals()
	m := func(x float64) int64 { return int64(money.FromFloat(money.Round2(x))) }
	if *got.Earned != m(tot.Earned) || *got.Arrived != m(tot.Arrived) || *got.Riding != m(tot.Riding) || *got.Hold != m(tot.Hold) ||
		*got.Stuck != m(tot.Stuck) || *got.OnPage != m(tot.Page) {
		t.Fatalf("totals %+v, card %+v", got, tot)
	}
	// The turnover is the card's «Оборот» with «не согласовано» added back; the models add up to it within a cent a line.
	var unagreed, off, models int64
	for _, l := range card.Lines {
		switch {
		case l.Channel == offChannel:
			off += m(l.Vals.Vals["net"])
		case strings.HasSuffix(l.Channel, "не согласовано"):
			unagreed += m(l.Vals.Vals["net"])
		}
	}
	for _, x := range got.Models {
		models += x.PagesNet + x.OffPlatform
	}
	turnover := m(tot.Net) + unagreed
	if *got.Turnover != turnover || *got.OffPlatform != off || *got.PagesNet != turnover-off || unagreed == 0 ||
		models-turnover > int64(len(card.Lines))*10_000 || turnover-models > int64(len(card.Lines))*10_000 {
		t.Fatalf("turnover %d (models %d) off %d unagreed %d: %+v", turnover, models, off, unagreed, got)
	}
}

// Only the payouts scope opens it; GET /v1/month/{month} stays read's, closed to payouts.
func TestThePayoutsMonthOpensOnlyToThePayoutsScope(t *testing.T) {
	s := monthDB(t)
	ctx := context.Background()
	putCard(t, s, "2026-11", "ok", "", inventedCard(true))
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
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
	const path = "/v1/payouts/month/2026-11"
	hub := key("hub", auth.ScopePayouts)
	var got PayoutsMonth
	if err := hub.Do(ctx, http.MethodGet, path, nil, &got); err != nil || got.Status != MonthProven || *got.Turnover != 1578200000 {
		t.Fatalf("payouts key: %+v %v", got, err)
	}
	if code := status(hub.Do(ctx, http.MethodGet, "/v1/payouts/month/2026-13", nil, nil)); code != http.StatusBadRequest {
		t.Errorf("a month that is not one: %d", code)
	}
	for _, sc := range []string{auth.ScopeRead, auth.ScopePropose, auth.ScopeDecide, auth.ScopeCollect, auth.ScopeImport, auth.ScopeReports} {
		if code := status(key("k-"+sc, sc).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusForbidden {
			t.Errorf("%s key: %d, want 403", sc, code)
		}
	}
	if code := status(client.New(config.Client{URL: srv.URL}).Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}
	// The full card stays where it was: read opens it (here the book has no hub data, so it cannot compute: 422, not 403),
	// payouts does not.
	if code := status(key("reader", auth.ScopeRead).Do(ctx, http.MethodGet, "/v1/month/2026-11", nil, nil)); code == http.StatusForbidden ||
		code == http.StatusUnauthorized {
		t.Errorf("read key on the full card: %d", code)
	}
	if code := status(hub.Do(ctx, http.MethodGet, "/v1/month/2026-11", nil, nil)); code != http.StatusForbidden {
		t.Errorf("payouts key on the full card: %d, want 403", code)
	}
}
