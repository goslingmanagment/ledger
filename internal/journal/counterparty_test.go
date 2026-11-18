package journal

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// counterpartyDoc is a journal row version written straight into documents.
type counterpartyDoc struct {
	num, version int
	date, typ    string
	counterparty string
	micros       int64
	status, note string
}

func putCounterpartyDocs(t *testing.T, s *db.Store, docs ...counterpartyDoc) {
	t.Helper()
	tx, err := s.RW.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, d := range docs {
		if _, err := tx.Exec(`INSERT INTO documents(num, version, date, type, category, counterparty, model, amount, amount_micros,
			currency, method, status, note, recorded_at, author, origin) VALUES (?, ?, ?, ?, 'Трафик', ?, 'Alfa', 'x', ?, 'USDT',
			'Internal Transfer (Bybit)', ?, ?, '2026-11-17T12:00:00+03:00', 'test', 'test')`,
			d.num, d.version, d.date, d.typ, d.counterparty, d.micros, d.status, d.note); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type counterpartyEnv struct {
	t      *testing.T
	ctx    context.Context
	url    string
	key    func(name string, scopes ...string) *client.Client
	status func(err error) int
}

func newCounterpartyEnv(t *testing.T, s *db.Store, ctx context.Context) counterpartyEnv {
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	t.Cleanup(srv.Close)
	return counterpartyEnv{t: t, ctx: ctx, url: srv.URL,
		key: func(name string, scopes ...string) *client.Client {
			token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
			if err != nil {
				t.Fatal(err)
			}
			return client.New(config.Client{URL: srv.URL, Key: token})
		},
		status: func(err error) int {
			t.Helper()
			if err == nil {
				return http.StatusOK
			}
			e, ok := err.(*client.Error)
			if !ok {
				t.Fatalf("not an HTTP answer: %v", err)
			}
			return e.Status
		}}
}

func counterpartyPath(v url.Values) string { return "/v1/payouts/counterparty-payments?" + v.Encode() }

func nums(ps []CounterpartyPayment) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.Num
	}
	return out
}

func sameNums(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// GET /v1/payouts/counterparty-payments opens to the payouts scope only and gives the latest version of each expense or
// income row, paid or pending, of the named counterparties in any case, newest first, within the dates inclusive.
func TestCounterpartyPaymentsForHub(t *testing.T) {
	s, ctx := openDB(t)
	longNote := strings.Repeat("ж", 600)
	putCounterpartyDocs(t, s,
		counterpartyDoc{num: 1, version: 1, date: "2026-11-01", typ: "expense", counterparty: "Vendor One", micros: 100_000_000, status: "paid", note: "a"},
		counterpartyDoc{num: 2, version: 1, date: "2026-11-03", typ: "expense", counterparty: "  vendor ONE ", micros: 20_000_000, status: "pending", note: "b"},
		// Row 3 changed: only its latest version counts.
		counterpartyDoc{num: 3, version: 1, date: "2026-11-05", typ: "expense", counterparty: "Студия Север", micros: 50_000_000, status: "paid", note: "old"},
		counterpartyDoc{num: 3, version: 2, date: "2026-11-05", typ: "expense", counterparty: "СТУДИЯ СЕВЕР", micros: 70_000_000, status: "paid", note: "new"},
		counterpartyDoc{num: 4, version: 1, date: "2026-11-05", typ: "income", counterparty: "студия север", micros: 5_000_000, status: "paid", note: "refund"},
		counterpartyDoc{num: 5, version: 1, date: "2026-11-06", typ: "expense", counterparty: "Vendor One", micros: 1_000_000, status: "cancelled", note: "c"},
		counterpartyDoc{num: 6, version: 1, date: "2026-11-07", typ: "transfer", counterparty: "Vendor One", micros: 1_000_000, status: "paid", note: "d"},
		counterpartyDoc{num: 7, version: 1, date: "2026-11-08", typ: "expense", counterparty: "Other Vendor", micros: 1_000_000, status: "paid", note: "e"},
		// Row 8 was paid, then cancelled: its latest version leaves it out.
		counterpartyDoc{num: 8, version: 1, date: "2026-11-09", typ: "expense", counterparty: "Vendor One", micros: 1_000_000, status: "paid", note: "f"},
		counterpartyDoc{num: 8, version: 2, date: "2026-11-09", typ: "expense", counterparty: "Vendor One", micros: 1_000_000, status: "cancelled", note: "f"},
		counterpartyDoc{num: 9, version: 1, date: "2026-11-10", typ: "expense", counterparty: "Vendor One", micros: 145_000_000, status: "paid", note: longNote},
	)
	env := newCounterpartyEnv(t, s, ctx)
	hub := env.key("hub", auth.ScopePayouts)
	names := url.Values{"name": {"Vendor One", "  студия СЕВЕР  "}}

	var got CounterpartyPayments
	if err := hub.Do(ctx, http.MethodGet, counterpartyPath(names), nil, &got); err != nil {
		t.Fatal(err)
	}
	if !sameNums(nums(got.Payments), []int{9, 4, 3, 2, 1}) || got.Truncated {
		t.Fatalf("payments %v truncated %v", nums(got.Payments), got.Truncated)
	}
	if at, err := time.Parse(time.RFC3339, got.GeneratedAt); err != nil || at.Location() != time.UTC {
		t.Errorf("generatedAt %q", got.GeneratedAt)
	}
	by := map[int]CounterpartyPayment{}
	for _, p := range got.Payments {
		by[p.Num] = p
	}
	if p := by[3]; p.AmountMicros != 70_000_000 || p.Note != "new" || p.Counterparty != "СТУДИЯ СЕВЕР" || p.Type != "expense" {
		t.Errorf("row 3 %+v", p)
	}
	if p := by[4]; p.Type != "income" || p.AmountMicros != 5_000_000 || p.Status != "paid" {
		t.Errorf("row 4 %+v", p)
	}
	if p := by[2]; p.Status != "pending" || p.Counterparty != "  vendor ONE " {
		t.Errorf("row 2 %+v", p)
	}
	if p := by[9]; utf8.RuneCountInString(p.Note) != MaxCounterpartyNoteLen || !strings.HasPrefix(longNote, p.Note) ||
		p.Date != "2026-11-10" || p.Model != "Alfa" || p.Currency != "USDT" || p.Category != "Трафик" {
		t.Errorf("row 9 %+v", p)
	}

	// The dates are inclusive at both ends; either may be left out.
	window := func(from, to string) []int {
		t.Helper()
		v := url.Values{"name": names["name"]}
		if from != "" {
			v.Set("from", from)
		}
		if to != "" {
			v.Set("to", to)
		}
		var a CounterpartyPayments
		if err := hub.Do(ctx, http.MethodGet, counterpartyPath(v), nil, &a); err != nil {
			t.Fatal(err)
		}
		return nums(a.Payments)
	}
	if w := window("2026-11-03", "2026-11-05"); !sameNums(w, []int{4, 3, 2}) {
		t.Errorf("window 03..05: %v", w)
	}
	if w := window("2026-11-05", ""); !sameNums(w, []int{9, 4, 3}) {
		t.Errorf("from 05: %v", w)
	}
	if w := window("", "2026-11-01"); !sameNums(w, []int{1}) {
		t.Errorf("to 01: %v", w)
	}

	// Only the payouts scope opens it.
	for _, sc := range []string{auth.ScopeRead, auth.ScopePropose, auth.ScopeDecide, auth.ScopeCollect, auth.ScopeImport, auth.ScopeReports} {
		if code := env.status(env.key("k-"+sc, sc).Do(ctx, http.MethodGet, counterpartyPath(names), nil, nil)); code != http.StatusForbidden {
			t.Errorf("%s key: %d, want 403", sc, code)
		}
	}
	if code := env.status(client.New(config.Client{URL: env.url}).Do(ctx, http.MethodGet, counterpartyPath(names), nil, nil)); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}

	// Bad input is 400; a name of 200 Cyrillic letters is not too long.
	many := make([]string, MaxCounterpartyNames+1)
	for i := range many {
		many[i] = "Vendor One"
	}
	for label, v := range map[string]url.Values{
		"no name":       {},
		"blank name":    {"name": {"Vendor One", "   "}},
		"long name":     {"name": {strings.Repeat("x", MaxCounterpartyNameLen+1)}},
		"many names":    {"name": many},
		"bad from":      {"name": {"Vendor One"}, "from": {"2026-13-01"}},
		"short to":      {"name": {"Vendor One"}, "to": {"2026-11-5"}},
		"from after to": {"name": {"Vendor One"}, "from": {"2026-11-06"}, "to": {"2026-11-05"}},
	} {
		if code := env.status(hub.Do(ctx, http.MethodGet, counterpartyPath(v), nil, nil)); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", label, code)
		}
	}
	for _, v := range []url.Values{{"name": {strings.Repeat("ж", MaxCounterpartyNameLen)}}, {"name": many[:MaxCounterpartyNames]}} {
		if code := env.status(hub.Do(ctx, http.MethodGet, counterpartyPath(v), nil, nil)); code != http.StatusOK {
			t.Errorf("%d names of %d letters: %d, want 200", len(v["name"]), utf8.RuneCountInString(v["name"][0]), code)
		}
	}
}

// More matching rows than the limit: the newest MaxCounterpartyPayments come back and truncated says so.
func TestCounterpartyPaymentsTruncate(t *testing.T) {
	s, ctx := openDB(t)
	docs := []counterpartyDoc{{num: 1, version: 1, date: "2026-09-30", typ: "expense", counterparty: "Vendor Two", micros: 1, status: "paid"}}
	for n := 2; n <= MaxCounterpartyPayments+1; n++ {
		docs = append(docs, counterpartyDoc{num: n, version: 1, date: "2026-10-01", typ: "expense", counterparty: "Vendor Two", micros: 1, status: "paid"})
	}
	putCounterpartyDocs(t, s, docs...)
	hub := newCounterpartyEnv(t, s, ctx).key("hub", auth.ScopePayouts)

	var all CounterpartyPayments
	if err := hub.Do(ctx, http.MethodGet, counterpartyPath(url.Values{"name": {"Vendor Two"}}), nil, &all); err != nil {
		t.Fatal(err)
	}
	if !all.Truncated || len(all.Payments) != MaxCounterpartyPayments || all.Payments[0].Num != MaxCounterpartyPayments+1 ||
		all.Payments[len(all.Payments)-1].Num != 2 {
		t.Fatalf("truncated %v, %d rows, first %d", all.Truncated, len(all.Payments), all.Payments[0].Num)
	}
	// Exactly the limit is not truncated.
	var some CounterpartyPayments
	if err := hub.Do(ctx, http.MethodGet, counterpartyPath(url.Values{"name": {"Vendor Two"}, "from": {"2026-10-01"}}), nil, &some); err != nil {
		t.Fatal(err)
	}
	if some.Truncated || len(some.Payments) != MaxCounterpartyPayments {
		t.Fatalf("truncated %v, %d rows", some.Truncated, len(some.Payments))
	}
}
