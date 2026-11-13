package reports

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// An amount added by hand may name the sale it pays (adds[].transaction_ref): most adds are sales of Hub paid late.
// The reference is either of the sale's two — the platform's, as the lines carry ("pf-7001"), or Hub's id ("7001") —
// and the mirror of Hub's transactions holds both.

// savedBefore is a report as the agent saved them before an add could name its sale — every field the format has,
// the adds bare —, and savedBeforeHash its content hash as the code of that time gave it (computed with that code).
const (
	savedBefore = `{"hub_username":"Ann_Hub","hub_user_id":7,"chatter":"k1","report_date":"2026-11-08",
		"window_from":"2026-11-08T13:00:00Z","window_to":"2026-11-09T00:00:00+03:00","cut_at":"2026-11-08T23:53:00+03:00",
		"source":"agent","total_mills":36520,"pages":[{"page":" page-a ","amount_mills":36520}],
		"lines":[{"n":1,"transaction_ref":"tx-1","page":"page-a","fan_ref":"fan-1","fan_name":"nick","kind":"message_purchase",
		"occurred_at":"2026-11-08T17:28:00+03:00","net_mills":28000,"gross_mills":35000,"counted":true,"decision":"auto","flags":[],
		"notes":["text"],"ppv_author":"ann","ppv_sent_at":"08.11 17:26"},
		{"n":2,"transaction_ref":"tx-2","page":"page-a","net_mills":5000,"counted":false,"decision":"exclude","flags":["чужой PPV"]}],
		"adds":[{"page":"page-a","amount_mills":3520,"what":" вне окна "},{"page":"page-a","amount_mills":5000,"what":"по слову чаттера"}]}`
	savedBeforeHash = "39a2c3245a17f581f22b9eaca273ba37b9e5a06ca08d924c085c3d8670386c50"
)

// sold is a sale of a report of page-a: a line of the window (n > 0) or an amount added by hand (n == 0).
type sold struct {
	n       int64
	ref     string
	mills   int64
	counted bool
}

// reportOf is anna's report of a day on page-a: the lines and the adds given, the page's amount what they count.
func reportOf(sales ...sold) map[string]any {
	var total int64
	lines, adds := []any{}, []any{}
	for _, x := range sales {
		if x.n == 0 {
			total += x.mills
			add := map[string]any{"page": "page-a", "amount_mills": x.mills, "what": "долёт"}
			if x.ref != "" {
				add["transaction_ref"] = x.ref
			}
			adds = append(adds, add)
			continue
		}
		if x.counted {
			total += x.mills
		}
		lines = append(lines, map[string]any{"n": x.n, "transaction_ref": x.ref, "page": "page-a", "net_mills": x.mills, "counted": x.counted})
	}
	return map[string]any{"hub_username": "anna_hub", "report_date": "2026-11-12", "window_from": "2026-11-12T16:00:00+03:00",
		"window_to": "2026-11-13T00:00:00+03:00", "cut_at": "2026-11-12T23:50:00+03:00", "source": "agent", "total_mills": total,
		"pages": []any{map[string]any{"page": "page-a", "amount_mills": total}}, "lines": lines, "adds": adds}
}

// mirrored puts a sale into the mirror of Hub's transactions, the row that holds both of its references.
func mirrored(t *testing.T, s *db.Store, id, platformRef string) {
	t.Helper()
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
			VALUES ('hub', ?, 'transactions', '2026-11-12', '2026-11-12', '{}')`, []any{"page-a/transactions/" + id}},
		{`INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash, event_id,
			first_seen_at, updated_at) VALUES ('page-a', ?, '2026-11-12T10:00:00.000+03:00', 1000, 'USD', 'message_purchase', ?, '{}', 'h',
			last_insert_rowid(), '2026-11-12', '2026-11-12')`, []any{id, platformRef}},
	} {
		if _, err := s.RW.Exec(q.query, q.args...); err != nil {
			t.Fatalf("%v\n%s", err, q.query)
		}
	}
}

// refused saves a report and wants a refusal that says every part.
func refused(t *testing.T, s *db.Store, name string, m map[string]any, parts ...string) {
	t.Helper()
	_, err := Save(t.Context(), s, raw(t, m), "agent", ts.Now())
	r, ok := err.(*Refusal)
	if !ok {
		t.Fatalf("%s: %v, want a refusal", name, err)
	}
	for _, p := range parts {
		if !strings.Contains(r.Msg, p) {
			t.Fatalf("%s: %q, want it to say %q", name, r.Msg, p)
		}
	}
}

func TestAnAddsSaleIsStoredAndReadBack(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	// The 3.52 of page-a: 2.00 a sale paid after the report was composed, 1.52 money Hub does not see.
	m := sample("anna_hub")
	m["adds"] = []any{
		map[string]any{"page": "page-a", "amount_mills": 2000, "what": "долёт: PPV куплен после отчёта", "transaction_ref": " tx-late "},
		map[string]any{"page": "page-a", "amount_mills": 1520, "what": "вне окна"},
	}
	saved, err := Save(ctx, s, raw(t, m), "agent", ts.Now())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.RO.Query(`SELECT seq, COALESCE(transaction_ref, '∅'), n IS NULL, counted, net_micros, what FROM shift_report_lines
		WHERE report_id = ? AND added = 1 ORDER BY seq`, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var seq, net int64
		var ref, what string
		var noN, counted bool
		if err := rows.Scan(&seq, &ref, &noN, &counted, &net, &what); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, strings.Join([]string{itoa(seq), ref, itoa(net), what}, " "))
		if !noN || !counted {
			t.Fatalf("add %d: n is null %v, counted %v", seq, noN, counted)
		}
	}
	if got, want := strings.Join(stored, "|"), "4 tx-late 2000000 долёт: PPV куплен после отчёта|5 ∅ 1520000 вне окна"; got != want {
		t.Fatalf("stored adds: %s\nwant %s", got, want)
	}
	one, err := Get(ctx, s.RO, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := List(ctx, s.RO, Filter{From: "2026-11-08", To: "2026-11-08"})
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	for name, r := range map[string]Report{"one": one, "list": list[0]} {
		if len(r.Lines) != 5 || r.TotalMicros != 213_120_000 || r.Pages[0].AmountMicros != 31_520_000 {
			t.Fatalf("%s: %+v", name, r)
		}
		late, bare := r.Lines[3], r.Lines[4]
		if !late.Added || !late.Counted || late.N != nil || late.TransactionRef == nil || *late.TransactionRef != "tx-late" ||
			late.NetMicros != 2_000_000 || late.What != "долёт: PPV куплен после отчёта" {
			t.Fatalf("%s: the add that names its sale: %+v", name, late)
		}
		if !bare.Added || !bare.Counted || bare.TransactionRef != nil || bare.NetMicros != 1_520_000 || bare.What != "вне окна" {
			t.Fatalf("%s: the add that names none: %+v", name, bare)
		}
	}
	var lines, adds int
	if err := s.RO.QueryRow(`SELECT lines, adds FROM v_shift_reports`).Scan(&lines, &adds); err != nil || lines != 3 || adds != 2 {
		t.Fatalf("v_shift_reports: %d lines, %d adds, %v", lines, adds, err)
	}
}

func TestASaleIsPaidOnceInAReport(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	// Two adds name one sale; an add names the sale of a line that is in the total. Nothing is written.
	refused(t, s, "two adds, one sale", reportOf(sold{n: 1, ref: "tx-1", mills: 28_000, counted: true}, sold{ref: "tx-9", mills: 2_000},
		sold{ref: "tx-9", mills: 1_000}), "добавка 2 (страница page-a): продажу tx-9 уже называет добавка 1", "оплачена дважды")
	refused(t, s, "an add and a counted line", reportOf(sold{n: 1, ref: "tx-1", mills: 28_000, counted: true}, sold{n: 2, ref: "tx-2", mills: 5_000},
		sold{ref: "tx-1", mills: 2_000}), "добавка 1 (страница page-a): продажа tx-1 уже в итоге отчёта — строка n=1", "оплачена дважды")
	refused(t, s, "a reference too long", reportOf(sold{ref: strings.Repeat("я", 201), mills: 2_000}), "transaction_ref — 201 знаков, можно до 200")
	for _, tbl := range []string{"shift_reports", "shift_report_pages", "shift_report_lines"} {
		if n := count(t, s, tbl); n != 0 {
			t.Fatalf("%s: %d rows after refusals", tbl, n)
		}
	}
	// The sale of a line held out of the total may be named: the script excluded it, the chatter's word added it.
	saved, err := Save(ctx, s, raw(t, reportOf(sold{n: 1, ref: "tx-1", mills: 28_000, counted: true}, sold{n: 2, ref: "tx-2", mills: 5_000},
		sold{ref: "tx-2", mills: 5_000}, sold{ref: strings.Repeat("я", 200), mills: 1_000})), "agent", ts.Now())
	if err != nil || !saved.Created {
		t.Fatalf("an add that names the sale of a held-out line: %+v %v", saved, err)
	}
	r, err := Get(ctx, s.RO, saved.ID)
	if err != nil || r.TotalMicros != 34_000_000 || r.Lines[1].Counted || *r.Lines[1].TransactionRef != "tx-2" || *r.Lines[2].TransactionRef != "tx-2" ||
		!r.Lines[2].Added {
		t.Fatalf("the report: %+v %v", r, err)
	}
}

// An add names its sale by either of the sale's two references — the agent's script finds a sale in Hub's facts, by
// Hub's id, and may not know the platform's —, while the lines carry the platform's: «the same sale» is asked of the
// mirror of Hub's transactions, not of the strings.
func TestASaleIsPaidOnceInAReportUnderEitherReference(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	mirrored(t, s, "7001", "pf-7001")
	mirrored(t, s, "7002", "pf-7002")
	refused(t, s, "Hub's id of a counted line's sale", reportOf(sold{n: 1, ref: "pf-7001", mills: 28_000, counted: true}, sold{ref: "7001", mills: 28_000}),
		"добавка 1 (страница page-a): продажа 7001 уже в итоге отчёта — строка n=1 (под другой ссылкой той же продажи — pf-7001)", "оплачена дважды")
	refused(t, s, "two adds, the two references of one sale", reportOf(sold{ref: "7002", mills: 2_000}, sold{ref: "pf-7002", mills: 2_000}),
		"добавка 2 (страница page-a): продажу pf-7002 уже называет добавка 1 (под другой ссылкой той же продажи — 7002)")
	if n := count(t, s, "shift_reports"); n != 0 {
		t.Fatalf("%d reports after refusals", n)
	}
	// Hub's id of the sale of a held-out line, and of a sale no line has: saved.
	if _, err := Save(ctx, s, raw(t, reportOf(sold{n: 1, ref: "pf-7001", mills: 28_000, counted: true}, sold{n: 2, ref: "pf-7002", mills: 5_000},
		sold{ref: "7002", mills: 5_000})), "agent", ts.Now()); err != nil {
		t.Fatalf("an add that names the sale of a held-out line by Hub's id: %v", err)
	}
	// A sale the mirror does not have yet is known by its reference alone: the two strings are not known to be one sale,
	// and the report is saved. The hourly read brings the sale, and the same report is refused — not «unchanged».
	twice := reportOf(sold{n: 1, ref: "pf-7009", mills: 28_000, counted: true}, sold{ref: "7009", mills: 28_000})
	first, err := Save(ctx, s, raw(t, twice), "agent", ts.Now())
	if err != nil || !first.Created {
		t.Fatalf("while the mirror does not have the sale: %+v %v", first, err)
	}
	mirrored(t, s, "7009", "pf-7009")
	refused(t, s, "once the mirror has the sale", twice, "продажа 7009 уже в итоге отчёта — строка n=1 (под другой ссылкой той же продажи — pf-7009)")
	if n := count(t, s, "shift_reports"); n != 2 {
		t.Fatalf("%d versions: the refused report wrote one", n)
	}
}

// The reference is content: the same report with a sale named is a new version, and without one it is the version it
// was — its hash is what it was before an add could name a sale, so the reports saved before are not saved anew.
func TestTheSaleAnAddNamesIsPartOfTheVersion(t *testing.T) {
	s := book(t)
	ctx := t.Context()
	// save saves savedBefore, its first add naming the sale given (nil — as it was sent before).
	save := func(ref any) SaveResult {
		t.Helper()
		body := []byte(savedBefore)
		if ref != nil {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			m["adds"].([]any)[0].(map[string]any)["transaction_ref"] = ref
			body = raw(t, m)
		}
		out, err := Save(ctx, s, body, "agent", ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	hash := func(id int64) string {
		t.Helper()
		var h string
		if err := s.RO.QueryRow(`SELECT content_hash FROM shift_reports WHERE id = ?`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	bare := save(nil)
	if !bare.Created || bare.Version != 1 || hash(bare.ID) != savedBeforeHash {
		t.Fatalf("a report whose adds name no sale: %+v, hash %s, want the hash it had before the field, %s", bare, hash(bare.ID), savedBeforeHash)
	}
	// An empty reference is none.
	if out := save("  "); out.Created || out.ID != bare.ID {
		t.Fatalf("an empty reference made a version: %+v", out)
	}
	named := save("tx-late")
	if !named.Created || named.Version != 2 || hash(named.ID) == savedBeforeHash {
		t.Fatalf("the same report with its add's sale named: %+v", named)
	}
	if out := save(" tx-late "); out.Created || out.ID != named.ID || out.Version != 2 {
		t.Fatalf("the same reference again: %+v", out)
	}
	if out := save("tx-other"); !out.Created || out.Version != 3 {
		t.Fatalf("another sale named: %+v", out)
	}
	back := save(nil)
	if !back.Created || back.Version != 4 || hash(back.ID) != savedBeforeHash {
		t.Fatalf("the reference taken off again: %+v, hash %s", back, hash(back.ID))
	}
	var statuses string
	if err := s.RO.QueryRow(`SELECT group_concat(status, ' ') FROM (SELECT status FROM shift_reports ORDER BY version)`).Scan(&statuses); err != nil ||
		statuses != "superseded superseded superseded draft" {
		t.Fatalf("statuses %q %v", statuses, err)
	}
}
