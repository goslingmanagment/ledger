package decisions_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/goslingmanagment/ledger/internal/accruals" // POST /v1/accruals/claim
	_ "github.com/goslingmanagment/ledger/internal/agentsql" // POST /v1/sql
	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/pots"
	"github.com/goslingmanagment/ledger/internal/reports"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

var (
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	now   = time.Date(2026, 11, 12, 12, 0, 0, 0, ts.MSK)
)

type keys struct {
	agent, owner, reader, hub *client.Client
}

// book is a store with two cards, a Hub page of model Alpha, a pot and the service with four keys; all synthetic.
func book(t *testing.T) (*db.Store, context.Context, keys) {
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
	}, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RW.ExecContext(ctx, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
		VALUES ('page-a', 'fansly', 'Alpha', 'Alpha', '2026-11-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pots.Declare(ctx, s, pots.DeclareInput{Slug: "set-one", Name: "Набор", Model: "Alpha", Pages: []string{"page-a"},
		StartsAt: "2026-11-01", DeclaredBy: "владелец", DeclaredAt: "2026-11-01",
		Participants: []pots.Participant{{Person: "anna"}, {Person: "boris"}}}, "owner", now); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	t.Cleanup(srv.Close)
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	return s, ctx, keys{agent: key("agent", auth.ScopeReports), owner: key("owner", auth.ScopeDecide),
		reader: key("reader", auth.ScopeRead), hub: key("hub", auth.ScopePayouts)}
}

func status(err error) int {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Status
	}
	if err == nil {
		return http.StatusOK
	}
	return -1
}

func message(err error) string {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return ""
}

func mills(v int64) *int64 { return &v }

// owner is a valid decision: the whole of tx-1 is anna's.
func owner(ref string) decisions.Input {
	return decisions.Input{PageLabel: "page-a", FanRef: "fan-100", FanName: "MoonFan", TransactionRef: ref, NetMills: mills(25_000),
		OccurredAt: "2026-11-10T21:15:00+03:00", Kind: "owner", Shares: []decisions.Share{{Person: "anna", Percent: 100}},
		SaidBy: "владелец", SaidAt: "2026-11-11", SourceLink: "https://t.me/c/1/2", Note: "PPV отправил Борис, но продала Анна в свою смену."}
}

func TestADecisionIsCheckedAgainstItsKind(t *testing.T) {
	_, ctx, k := book(t)
	for _, c := range []struct {
		edit func(*decisions.Input)
		part string
	}{
		{func(in *decisions.Input) {
			in.Kind = "split"
			in.Shares = []decisions.Share{{"anna", 50}, {"boris", 40}}
		}, "в сумме 90%"},
		{func(in *decisions.Input) { in.Shares = []decisions.Share{{"nobody", 100}} }, "человека \"nobody\" нет"},
		{func(in *decisions.Input) { in.Shares = []decisions.Share{{"anna", 50}, {"boris", 50}} }, "у kind owner одна доля"},
		{func(in *decisions.Input) {
			in.Kind = "split"
			in.Shares = []decisions.Share{{"anna", 50}, {"anna", 50}}
		}, "дважды"},
		{func(in *decisions.Input) { in.Kind = "pot"; in.Shares = nil }, "нужен общак"},
		{func(in *decisions.Input) { in.Kind = "pot"; in.Shares = nil; in.Pot = "nope" }, "общака \"nope\" нет"},
		{func(in *decisions.Input) { in.Kind = "exclude" }, "у kind exclude нет ни долей"},
		{func(in *decisions.Input) { in.Kind = "mine" }, "kind бывает"},
		{func(in *decisions.Input) { in.SaidBy = "" }, "кто решил"},
		{func(in *decisions.Input) { in.SaidAt = "11.11" }, "said_at"},
		{func(in *decisions.Input) { in.FanRef, in.FanName = "", "" }, "нужен фан"},
		{func(in *decisions.Input) { in.Note = strings.Repeat("я", decisions.MaxNote+1) }, "можно до 4000"},
	} {
		in := owner("tx-1")
		c.edit(&in)
		err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", in, nil)
		if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), c.part) {
			t.Errorf("want %q, got %v", c.part, err)
		}
	}
	// Unknown fields are refused too.
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", map[string]any{"page_label": "page-a", "whose": "anna"}, nil); status(err) != http.StatusBadRequest {
		t.Errorf("unknown field: %v", err)
	}
	for _, in := range []decisions.Input{
		owner("tx-1"),
		{PageLabel: "page-a", FanName: "fan-x", Kind: "pot", Pot: "set-one", SaidBy: "Анна и Борис", SaidAt: "2026-11-11T10:00:00Z"},
		{PageLabel: "page-a", FanName: "fan-y", Kind: "off_platform", Shares: []decisions.Share{{"boris", 100}}, SaidBy: "Борис", SaidAt: "2026-11-11"},
		{PageLabel: "page-a", FanName: "fan-z", TransactionRef: "tx-9", Kind: "exclude", SaidBy: "владелец", SaidAt: "2026-11-11"},
	} {
		var d decisions.Decision
		if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", in, &d); err != nil || d.Status != "open" || d.KeyName != "agent" {
			t.Fatalf("%+v: %+v %v", in, d, err)
		}
	}
}

func TestSupersedeAppliedAndCancel(t *testing.T) {
	s, ctx, k := book(t)
	var first, second decisions.Decision
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", owner("tx-1"), &first); err != nil {
		t.Fatal(err)
	}
	// A second word on the same sale must replace the first one by name.
	split := owner("tx-1")
	split.Kind, split.Shares = "split", []decisions.Share{{"anna", 60}, {"boris", 40}}
	err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", split, nil)
	if status(err) != http.StatusUnprocessableEntity || !strings.Contains(message(err), "supersedes: "+strconv.FormatInt(first.ID, 10)) {
		t.Fatalf("no supersede: %v", err)
	}
	split.Supersedes = &first.ID
	if err := k.owner.Do(ctx, http.MethodPost, "/v1/decisions", split, &second); err != nil {
		t.Fatal(err)
	}
	old, err := decisions.Get(ctx, s.RO, first.ID)
	if err != nil || old.Status != "cancelled" || *old.CancelNote != "заменено решением "+strconv.FormatInt(second.ID, 10) {
		t.Fatalf("the replaced one %+v %v", old, err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", split, nil); status(err) != http.StatusUnprocessableEntity {
		t.Fatalf("superseding a cancelled one: %v", err)
	}
	// Promises without a sale may stand side by side.
	promise := owner("")
	for i := 0; i < 2; i++ {
		if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", promise, nil); err != nil {
			t.Fatal(err)
		}
	}

	path := "/v1/decisions/" + strconv.FormatInt(second.ID, 10)
	if err := k.agent.Do(ctx, http.MethodPost, path+"/applied", decisions.AppliedInput{Person: "nobody", ReportDate: "2026-11-10"}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Fatalf("applied to nobody: %v", err)
	}
	var d decisions.Decision
	if err := k.agent.Do(ctx, http.MethodPost, path+"/applied", decisions.AppliedInput{Person: "anna", ReportDate: "2026-11-10"}, &d); err != nil ||
		d.Status != "applied" || *d.AppliedPerson != "anna" || *d.AppliedReportDate != "2026-11-10" {
		t.Fatalf("applied %+v %v", d, err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path+"/applied", decisions.AppliedInput{Person: "anna", ReportDate: "2026-11-10"}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Fatalf("applied twice: %v", err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path+"/cancel", decisions.CancelInput{}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Fatalf("cancel without a note: %v", err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path+"/cancel", decisions.CancelInput{Note: "владелец передумал"}, &d); err != nil || d.Status != "cancelled" {
		t.Fatalf("cancel %+v %v", d, err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, path+"/cancel", decisions.CancelInput{Note: "ещё"}, nil); status(err) != http.StatusUnprocessableEntity {
		t.Fatalf("cancel twice: %v", err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions/999/cancel", decisions.CancelInput{Note: "x"}, nil); status(err) != http.StatusNotFound {
		t.Fatalf("cancel of none: %v", err)
	}
}

func list(t *testing.T, c *client.Client, query string) []int64 {
	t.Helper()
	var ans decisions.ListAnswer
	if err := c.Do(context.Background(), http.MethodGet, "/v1/decisions"+query, nil, &ans); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	ids := []int64{}
	for _, d := range ans.Decisions {
		ids = append(ids, d.ID)
	}
	return ids
}

func TestTheFanSearchAndTheFilters(t *testing.T) {
	_, ctx, k := book(t)
	a := owner("tx-1") // MoonFan / fan-100, anna, the 10th
	b := owner("tx-2")
	b.FanRef, b.FanName, b.OccurredAt = "fan-200", "Котик", "2026-11-05T10:00:00+03:00"
	b.Kind, b.Shares = "split", []decisions.Share{{"anna", 50}, {"boris", 50}}
	c := owner("")
	c.FanRef, c.FanName, c.OccurredAt, c.NetMills, c.SaidAt = "fan-300", "ab", "", nil, "2026-11-20"
	c.Shares = []decisions.Share{{"boris", 100}}
	for _, in := range []decisions.Input{a, b, c} {
		if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", in, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions/1/applied", decisions.AppliedInput{Person: "anna", ReportDate: "2026-11-10"}, nil); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"":                               "1,2,3",
		"?open=1":                        "2,3",
		"?fan=moon":                      "1", // a part of the name, case aside
		"?fan=MOONFAN":                   "1", // the whole name
		"?fan=mo":                        "",  // two characters: only whole
		"?fan=fan-200":                   "2", // the ref
		"?fan=кот":                       "2", // Cyrillic, case aside
		"?fan=AB":                        "3", // short, but whole
		"?fan=fan-":                      "1,2,3",
		"?person=boris":                  "2,3", // anyone in the shares
		"?person=anna&open=1":            "2",
		"?from=2026-11-06&to=2026-11-15": "1", // by the sale's date
		"?from=2026-11-15":               "3", // no sale yet: by when it was said
	} {
		got := list(t, k.reader, q)
		var s []string
		for _, id := range got {
			s = append(s, strconv.FormatInt(id, 10))
		}
		if strings.Join(s, ",") != want {
			t.Errorf("%s: %v, want %s", q, got, want)
		}
	}
	if err := k.reader.Do(ctx, http.MethodGet, "/v1/decisions?from=10.11", nil, nil); status(err) != http.StatusBadRequest {
		t.Errorf("bad date: %v", err)
	}
}

func TestTheScopes(t *testing.T) {
	_, ctx, k := book(t)
	// The agent's key adds and reads decisions and pot purchases, and reads the pots.
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", owner("tx-1"), nil); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*client.Client{"agent": k.agent, "reader": k.reader, "hub": k.hub} {
		for _, path := range []string{"/v1/decisions", "/v1/pots", "/v1/pots/set-one", "/v1/pots/candidates"} {
			if err := c.Do(ctx, http.MethodGet, path, nil, nil); err != nil {
				t.Errorf("%s GET %s: %v", name, path, err)
			}
		}
	}
	if err := k.owner.Do(ctx, http.MethodGet, "/v1/decisions", nil, nil); status(err) != http.StatusForbidden {
		t.Errorf("decide alone reads decisions: %v", err)
	}
	sale := pots.SaleInput{TransactionRef: "tx-5", PageLabel: "page-a", OccurredAt: "2026-11-05T10:00:00+03:00", NetMills: mills(1000)}
	var added pots.SaleResult
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/pots/set-one/sales", sale, &added); err != nil || added.Sale.Source != "agent" {
		t.Fatalf("agent adds a purchase: %+v %v", added, err)
	}
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/pot-sales/"+strconv.FormatInt(added.Sale.ID, 10)+"/remove", pots.RemoveInput{Note: "ошибка"}, nil); err != nil {
		t.Fatal(err)
	}
	sale.TransactionRef = "tx-6"
	if err := k.owner.Do(ctx, http.MethodPost, "/v1/pots/set-one/sales", sale, &added); err != nil || added.Sale.Source != "owner" {
		t.Fatalf("owner adds a purchase: %+v %v", added, err)
	}
	// Nothing else new: the agent does not declare a pot, Hub's and the reader's keys write nothing.
	declare := pots.DeclareInput{Slug: "set-two", Name: "Другой", Model: "Alpha", Pages: []string{"page-a"}, StartsAt: "2026-11-01",
		DeclaredBy: "агент", DeclaredAt: "2026-11-01", Participants: []pots.Participant{{Person: "anna"}}}
	for name, c := range map[string]*client.Client{"agent": k.agent, "reader": k.reader, "hub": k.hub} {
		if err := c.Do(ctx, http.MethodPost, "/v1/pots", declare, nil); status(err) != http.StatusForbidden {
			t.Errorf("%s declares a pot: %v", name, err)
		}
	}
	for name, c := range map[string]*client.Client{"reader": k.reader, "hub": k.hub} {
		for _, path := range []string{"/v1/decisions", "/v1/decisions/1/cancel", "/v1/pots/set-one/sales", "/v1/pot-sales/1/remove"} {
			if err := c.Do(ctx, http.MethodPost, path, map[string]any{}, nil); status(err) != http.StatusForbidden {
				t.Errorf("%s POST %s: %v", name, path, err)
			}
		}
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/payouts?from=2026-11-01&to=2026-11-10"},
		{http.MethodGet, "/v1/payouts/prepare?person=anna"},
		{http.MethodPost, "/v1/accruals/claim"},
		{http.MethodPost, "/v1/sql"},
	} {
		if err := k.agent.Do(ctx, c.method, c.path, map[string]any{}, nil); status(err) != http.StatusForbidden {
			t.Errorf("agent %s %s: %v", c.method, c.path, err)
		}
	}
	if err := k.owner.Do(ctx, http.MethodPost, "/v1/pots", declare, nil); err != nil {
		t.Errorf("owner declares: %v", err)
	}
}

func TestReportLinesCarryTheAgreement(t *testing.T) {
	s, ctx, k := book(t)
	raw, _ := json.Marshal(map[string]any{
		"hub_username": "anna_hub", "report_date": "2026-11-10", "window_from": "2026-11-10T16:00:00+03:00",
		"window_to": "2026-11-11T00:00:00+03:00", "cut_at": "2026-11-10T23:50:00+03:00", "source": "agent", "total_mills": 25000,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": 25000}},
		"lines": []any{
			map[string]any{"n": 1, "transaction_ref": "tx-1", "page": "page-a", "net_mills": 25000, "counted": true, "decision": "include"},
			map[string]any{"n": 2, "transaction_ref": "tx-2", "page": "page-a", "net_mills": 1000, "counted": false, "decision": "auto"},
		},
	})
	saved, err := reports.Save(ctx, s, raw, "agent", now)
	if err != nil {
		t.Fatal(err)
	}
	var d decisions.Decision
	if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", owner("tx-1"), &d); err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := k.hub.Do(ctx, http.MethodGet, "/v1/reports/"+strconv.FormatInt(saved.ID, 10), nil, &r); err != nil {
		t.Fatal(err)
	}
	lines := r["lines"].([]any)
	first, second := lines[0].(map[string]any), lines[1].(map[string]any)
	if first["decision"] != "include" || second["decision"] != "auto" || second["agreement"] != nil {
		t.Fatalf("lines %+v", lines)
	}
	a := first["agreement"].(map[string]any)
	if a["id"] != json.Number(strconv.FormatInt(d.ID, 10)) || a["kind"] != "owner" || a["said_by"] != "владелец" || !strings.HasPrefix(a["note"].(string), "PPV отправил") {
		t.Fatalf("agreement %+v", a)
	}
	if sh := a["shares"].([]any); len(sh) != 1 || sh[0].(map[string]any)["person"] != "anna" {
		t.Fatalf("shares %+v", a["shares"])
	}
	// Applied it stays on the line; cancelled it is gone.
	if _, err := decisions.Applied(ctx, s, d.ID, decisions.AppliedInput{Person: "anna", ReportDate: "2026-11-10"}, now); err != nil {
		t.Fatal(err)
	}
	rep, err := reports.Get(ctx, s.RO, saved.ID)
	if err != nil || rep.Lines[0].Agreement == nil || rep.Lines[0].Agreement.Status != "applied" {
		t.Fatalf("applied %+v %v", rep.Lines, err)
	}
	if _, err := decisions.Cancel(ctx, s, d.ID, decisions.CancelInput{Note: "нет"}, now); err != nil {
		t.Fatal(err)
	}
	if list, err := reports.List(ctx, s.RO, reports.Filter{From: "2026-11-10", To: "2026-11-10"}); err != nil || list[0].Lines[0].Agreement != nil {
		t.Fatalf("cancelled %+v %v", list, err)
	}
}

func TestTheMarkdownIsTheAgentsFile(t *testing.T) {
	s, ctx, k := book(t)
	pot := decisions.Input{PageLabel: "page-a", FanName: "fan-x", Kind: "pot", Pot: "set-one", SaidBy: "Анна и Борис", SaidAt: "2026-11-11",
		Note: "Купил набор\nиз общака."}
	for _, in := range []decisions.Input{owner("tx-1"), pot} {
		if err := k.agent.Do(ctx, http.MethodPost, "/v1/decisions", in, nil); err != nil {
			t.Fatal(err)
		}
	}
	list, err := decisions.List(ctx, s.RO, decisions.Filter{Open: true})
	if err != nil {
		t.Fatal(err)
	}
	names, err := decisions.PotNames(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	want := "- **MoonFan / fan-100** (page-a). Продажа tx-1, нетто 25.00, 2026-11-10T21:15:00+03:00. Вся продажа — anna: PPV отправил Борис, но продала Анна в свою смену. " +
		"Источник: владелец, 2026-11-11, https://t.me/c/1/2. Статус: открыто, ждёт отчёта.\n" +
		"- **fan-x** (page-a). Продажи ещё нет. В общак Набор (set-one): Купил набор из общака. Источник: Анна и Борис, 2026-11-11. Статус: открыто, ждёт отчёта.\n"
	if got := decisions.Markdown(list, names); got != want {
		t.Fatalf("markdown:\n%s\nwant\n%s", got, want)
	}
}
