package erasure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// An invented agency: page-a (fansly) and page-b (onlyfans); fan 777 bought sale 1001 on page-a, fan 888 sale 1002;
// on onlyfans another fan has the same reference 777 and bought sale 1003.

var now = time.Date(2026, 11, 13, 12, 0, 0, 0, ts.MSK)

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
	for _, q := range []string{
		`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-a', 'fansly', 'm', 'M', '2026-11-01'),
			('page-b', 'onlyfans', 'm', 'M', '2026-11-01')`,
		`INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at, post_tip, campaign, payload,
			first_seen_at, read_at) VALUES
			('1001', '2026-11-09', 'page-a', 'fansly', 'ppv', 20000, '2026-11-10T20:00:00+03:00', 0, 0, '{"transactionRef":"1001","fanRef":"777"}', 'x', 'x'),
			('1002', '2026-11-09', 'page-a', 'fansly', 'tip', 5000, '2026-11-10T21:00:00+03:00', 0, 0, '{"transactionRef":"1002","fanRef":"888"}', 'x', 'x'),
			('1003', '2026-11-09', 'page-b', 'onlyfans', 'tip', 7000, '2026-11-10T22:00:00+03:00', 0, 0, '{"transactionRef":"1003","fanRef":"777"}', 'x', 'x')`,
		// Hub's handed-in version with the two fansly sales and a person's notes on them.
		`INSERT INTO hub_shift_reports(id, shift_id, version, person, report_date, submitted_at, total_micros, comment, current, payload, received_at, stored_at)
			VALUES (501, 51, 1, 'anna', '2026-11-10', '2026-11-10T23:00:00+03:00', 25000000, 'смена спокойная', 1,
			'{"id":501,"lines":[{"transactionId":1001,"note":"фан Ромео просил скидку","amountMills":20000},{"transactionId":1002,"note":"другой фан","amountMills":5000}],"comment":"смена спокойная"}',
			'x', 'x')`,
		`INSERT INTO hub_shift_report_lines(report_id, seq, transaction_ref, page_label, grp, net_micros, rule_share_micros, amount_micros, note, counted, added, disputed)
			VALUES (501, 1, '1001', 'page-a', 'mine', 20000000, 20000000, 20000000, 'фан Ромео просил скидку', 1, 0, 0),
			(501, 2, '1002', 'page-a', 'mine', 5000000, 5000000, 5000000, 'другой фан', 1, 0, 0)`,
		`INSERT INTO hub_shift_report_adds(report_id, seq, hub_id, amount_micros, from_whom, what_for, state, counted)
			VALUES (501, 1, 9, 15000000, 'Ромео', 'кастом', 'pending', 0)`,
		// The agent's draft: a line on sale 1001, and a line naming the fan by reference only.
		`INSERT INTO shift_reports(id, person, hub_username, report_date, window_from, window_to, cut_at, version, total_micros, source, status,
			content_hash, payload, created_at, key_name) VALUES (7, 'anna', 'anna_hub', '2026-11-10', 'w', 'w', 'w', 1, 20000000, 'agent', 'posted', 'h',
			'{"lines":[{"transaction_ref":"1001","page":"page-a","fan_ref":"777","fan_name":"Romeo","notes":["пишет каждый день"]},{"page":"page-a","fan_ref":"777","fan_name":"Romeo","notes":["обещал вернуться"]},{"transaction_ref":"1002","page":"page-a","fan_ref":"888","fan_name":"Other","notes":[]}]}',
			'x', 'agent')`,
		`INSERT INTO shift_report_lines(report_id, seq, n, transaction_ref, page_label, fan_ref, fan_name, net_micros, counted, added, notes)
			VALUES (7, 1, 1, '1001', 'page-a', '777', 'Romeo', 20000000, 1, 0, '["пишет каждый день"]'),
			(7, 2, 2, NULL, 'page-a', '777', 'Romeo', 1000000, 0, 0, '["обещал вернуться"]'),
			(7, 3, 3, '1002', 'page-a', '888', 'Other', 5000000, 1, 0, '[]')`,
		// Decisions: on the sale; a promise naming the fan on the platform; the same reference on another platform.
		`INSERT INTO sale_decisions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, kind, shares, said_by, said_at, note, created_at, key_name)
			VALUES (1, 'page-a', '777', 'Romeo', '1001', 20000000, 'owner', '[{"person":"anna","percent":100}]', 'владелец', '2026-11-11', 'Ромео — её фан', 'x', 'agent'),
			(2, 'page-a', '777', 'Romeo', NULL, NULL, 'owner', '[{"person":"anna","percent":100}]', 'владелец', '2026-11-11', 'обещание', 'x', 'agent'),
			(3, 'page-b', '777', 'Other OF', NULL, NULL, 'owner', '[{"person":"anna","percent":100}]', 'владелец', '2026-11-11', 'не тот фан', 'x', 'agent')`,
		`INSERT INTO sale_questions(id, page_label, fan_ref, fan_name, transaction_ref, question, asked_of, asked_at, note, status, answer, created_at, key_name)
			VALUES (1, 'page-a', '777', 'Romeo', '1001', 'Ромео чей?', 'owner', '2026-11-11', 'спросили', 'answered', 'Ромео — Анны', 'x', 'agent')`,
		`INSERT INTO pot_sales(id, pot, transaction_ref, page_label, fan_ref, fan_name, net_micros, occurred_at, day, source, note, added_at, key_name)
			VALUES (1, 'set-one', '1001', 'page-a', '777', 'Romeo', 20000000, '2026-11-10T20:00:00+03:00', '2026-11-10', 'agent', 'набор', 'x', 'agent')`,
	} {
		if _, err := s.RW.ExecContext(ctx, q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	return s, ctx
}

func rows(t *testing.T, s *db.Store, q string) string {
	t.Helper()
	r, err := s.RO.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer r.Close()
	cols, _ := r.Columns()
	var out []string
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				v = "∅"
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return strings.Join(out, "\n")
}

// feed serves Hub's erasures after since by the documented shape; status other than 200 answers it.
func feed(t *testing.T, items []hub.Erasure, status *int) *hub.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hub.ErasuresPath {
			http.NotFound(w, r)
			return
		}
		if *status != http.StatusOK {
			w.WriteHeader(*status)
			_, _ = w.Write([]byte(`{"error":"x","message":"stub"}`))
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		out, next := []hub.Erasure{}, since
		for _, e := range items {
			if e.ID > since {
				out, next = append(out, e), e.ID
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"erasures": out, "next": strconv.FormatInt(next, 10), "serverNow": "2026-11-13T09:00:00.000Z"})
	}))
	t.Cleanup(srv.Close)
	return hub.NewClient(hub.Config{BaseURL: srv.URL, Key: "k"}, nil)
}

// Fan 777 of fansly is erased in Hub: the words about him go from every copy ledger keeps of his sales and of his
// reference on fansly's pages; the money, the other fan and the onlyfans fan with the same reference stay; a second pass
// changes nothing; what no fan reference ties to him stays and is said.
func TestAnErasedFansWordsGoAndTheMoneyStays(t *testing.T) {
	s, ctx := book(t)
	ok := http.StatusOK
	c := feed(t, []hub.Erasure{{ID: 41, Platform: "fansly", FanRef: "777", ErasedAt: "2026-11-13T08:00:00.000Z"}}, &ok)
	st, err := Sync(ctx, s, c, now)
	if err != nil || !st.OK || st.Read != 1 || st.Applied != 1 || st.Next != "41" {
		t.Fatalf("pass %+v %v", st, err)
	}
	for _, c := range []struct{ query, want string }{
		{`SELECT transaction_ref, COALESCE(note, '∅'), amount_micros FROM hub_shift_report_lines ORDER BY seq`, "1001|∅|20000000\n1002|другой фан|5000000"},
		{`SELECT json_extract(payload, '$.lines[0].note'), json_extract(payload, '$.lines[0].amountMills'), json_extract(payload, '$.lines[1].note'),
			comment FROM hub_shift_reports`, "∅|20000|другой фан|смена спокойная"},
		{`SELECT from_whom, what_for, amount_micros FROM hub_shift_report_adds`, "Ромео|кастом|15000000"}, // no fan reference ties it
		{`SELECT seq, COALESCE(transaction_ref, '∅'), fan_ref, fan_name, notes, net_micros FROM shift_report_lines ORDER BY seq`,
			"1|1001|||[]|20000000\n2|∅|||[]|1000000\n3|1002|888|Other|[]|5000000"},
		{`SELECT json_extract(payload, '$.lines[0].fan_name'), json_extract(payload, '$.lines[1].fan_ref'), json_extract(payload, '$.lines[2].fan_name'),
			content_hash FROM shift_reports`, "||Other|h"},
		{`SELECT id, fan_ref, fan_name, note, kind, shares FROM sale_decisions ORDER BY id`,
			"1||||owner|[{\"person\":\"anna\",\"percent\":100}]\n2||||owner|[{\"person\":\"anna\",\"percent\":100}]\n3|777|Other OF|не тот фан|owner|[{\"person\":\"anna\",\"percent\":100}]"},
		{`SELECT fan_ref, fan_name, question, note, answer, status FROM sale_questions`, "||" + Erased + "||" + Erased + "|answered"},
		{`SELECT fan_ref, fan_name, note, net_micros, pot FROM pot_sales`, "|||20000000|set-one"},
		{`SELECT transaction_ref, COALESCE(json_extract(payload, '$.fanRef'), '∅'), net_mills FROM hub_sales ORDER BY transaction_ref`,
			"1001|∅|20000\n1002|888|5000\n1003|777|7000"},
		{`SELECT id, platform, erased_at FROM hub_erasures`, "41|fansly|2026-11-13T08:00:00.000Z"},
	} {
		if got := rows(t, s, c.query); got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.query, got, c.want)
		}
	}
	// Idempotent: the same erasure again changes no row.
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	again, err := Apply(ctx, tx, hub.Erasure{ID: 41, Platform: "fansly", FanRef: "777", ErasedAt: "2026-11-13T08:00:00.000Z"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for table, n := range again {
		if n != 0 {
			t.Errorf("a second pass changed %d rows of %s", n, table)
		}
	}
}

// An older Hub has no feed: 404 before it was ever read is «nothing yet»; after, a failure.
func TestErasuresFeedAbsentAndGone(t *testing.T) {
	s, ctx := book(t)
	status := http.StatusNotFound
	c := feed(t, []hub.Erasure{{ID: 5, Platform: "fansly", FanRef: "888", ErasedAt: "2026-11-13T08:00:00.000Z"}}, &status)
	if st, err := Sync(ctx, s, c, now); err != nil || !st.OK || !st.Absent {
		t.Fatalf("404 first %+v %v", st, err)
	}
	status = http.StatusOK
	if st, err := Sync(ctx, s, c, now); err != nil || st.Next != "5" {
		t.Fatalf("read %+v %v", st, err)
	}
	status = http.StatusNotFound
	if st, err := Sync(ctx, s, c, now); err == nil || st.OK || !strings.Contains(st.Error, "404") {
		t.Fatalf("404 after reading %+v %v", st, err)
	}
}
