package erasure

import (
	"net/http"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
)

// seed runs the statements on top of the book.
func seed(t *testing.T, s *db.Store, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := s.RW.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
}

// sale puts a sale of a fan on fansly's page-a into both of Hub's mirrors: the fact under Hub's id, with the fan's
// reference (hub_sales), and the transaction that holds both references of the sale — Hub's id and the platform's
// (hub_transactions).
func sale(t *testing.T, s *db.Store, id, platformRef, fanRef string) {
	t.Helper()
	const at = "2026-11-10T20:00:00.000+03:00"
	if _, err := s.RW.Exec(`INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at, post_tip,
		campaign, payload, first_seen_at, read_at) VALUES (?, '2026-11-09', 'page-a', 'fansly', 'ppv', 20000, ?, 0, 0, json_object('fanRef', ?), 'x', 'x')`,
		id, at, fanRef); err != nil {
		t.Fatal(err)
	}
	res, err := s.RW.Exec(`INSERT INTO events(source, source_id, kind, occurred_at, received_at, payload)
		VALUES ('hub', ?, 'transactions', ?, ?, '{}')`, "page-a/transactions/"+id, at, at)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := res.LastInsertId()
	if _, err := s.RW.Exec(`INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref, fields, hash,
		event_id, first_seen_at, updated_at) VALUES ('page-a', ?, ?, 20000, 'USD', 'message_purchase', ?, '{}', 'h', ?, ?, ?)`,
		id, at, platformRef, event, at, at); err != nil {
		t.Fatal(err)
	}
}

// erase runs one pass over a feed with the erasures given.
func erase(t *testing.T, s *db.Store, items ...hub.Erasure) {
	t.Helper()
	ok := http.StatusOK
	if st, err := Sync(t.Context(), s, feed(t, items, &ok), now); err != nil || !st.OK || st.Read != len(items) {
		t.Fatalf("pass %+v %v", st, err)
	}
}

// check compares what the queries read with what is wanted.
func check(t *testing.T, s *db.Store, cases [][2]string) {
	t.Helper()
	for _, c := range cases {
		if got := rows(t, s, c[0]); got != c[1] {
			t.Errorf("%s:\n%s\nwant\n%s", c[0], got, c[1])
		}
	}
}

// Defect 6. hub_sales names a fan's sales by Hub's id; the agent's draft, and a decision, a question or a pot purchase
// written from it, name a sale by the platform's reference. A row that only its sale ties to the fan — no fan id on
// it, a name that is no login — is his all the same; the other fan's rows, under another sale's reference, stay.
func TestTheFansRowsUnderThePlatformsReferenceAreHisByHisSale(t *testing.T) {
	s, _ := book(t)
	sale(t, s, "2001", "pf-2001", "555")
	sale(t, s, "2002", "pf-2002", "556")
	seed(t, s,
		`INSERT INTO shift_reports(id, person, hub_username, report_date, window_from, window_to, cut_at, version, total_micros, source, status,
			content_hash, payload, created_at, key_name) VALUES (8, 'boris', 'boris_hub', '2026-11-10', 'w', 'w', 'w', 1, 40000000, 'agent', 'posted', 'h8',
			'{"lines":[{"transaction_ref":"pf-2001","page":"page-a","fan_ref":"","fan_name":"Big Spender","notes":["просил скидку"]},{"transaction_ref":"pf-2002","page":"page-a","fan_ref":"","fan_name":"Quiet One","notes":["другой фан"]}]}',
			'x', 'agent')`,
		`INSERT INTO shift_report_lines(report_id, seq, n, transaction_ref, page_label, fan_ref, fan_name, net_micros, counted, added, notes)
			VALUES (8, 1, 1, 'pf-2001', 'page-a', '', 'Big Spender', 20000000, 1, 0, '["просил скидку"]'),
			(8, 2, 2, 'pf-2002', 'page-a', '', 'Quiet One', 20000000, 1, 0, '["другой фан"]')`,
		`INSERT INTO sale_decisions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, kind, shares, said_by, said_at, note, created_at, key_name)
			VALUES (20, 'page-a', '', 'Big Spender', 'pf-2001', 20000000, 'exclude', '[]', 'владелец', '2026-11-11', 'возврат обещан', 'x', 'agent'),
			(21, 'page-a', '', 'Quiet One', 'pf-2002', 20000000, 'exclude', '[]', 'владелец', '2026-11-11', 'чужое решение', 'x', 'agent')`,
		`INSERT INTO sale_questions(id, page_label, fan_ref, fan_name, transaction_ref, question, asked_of, asked_at, note, status, created_at, key_name)
			VALUES (20, 'page-a', '', 'Big Spender', 'pf-2001', 'чей Big Spender?', 'owner', '2026-11-11', 'спросили', 'open', 'x', 'agent'),
			(21, 'page-a', '', 'Quiet One', 'pf-2002', 'чей Quiet One?', 'owner', '2026-11-11', 'спросили', 'open', 'x', 'agent')`,
		`INSERT INTO pot_sales(id, pot, transaction_ref, page_label, fan_ref, fan_name, net_micros, occurred_at, day, source, note, added_at, key_name)
			VALUES (2, 'set-one', 'pf-2001', 'page-a', '', 'Big Spender', 20000000, '2026-11-10T20:00:00+03:00', '2026-11-10', 'report_line', 'набор для него', 'x', 'agent'),
			(3, 'set-one', 'pf-2002', 'page-a', '', 'Quiet One', 20000000, '2026-11-10T20:00:00+03:00', '2026-11-10', 'report_line', 'набор для другого', 'x', 'agent')`)
	erase(t, s, hub.Erasure{ID: 51, Platform: "fansly", FanRef: "555", ErasedAt: "2026-11-13T08:00:00.000Z"})
	check(t, s, [][2]string{
		{`SELECT seq, transaction_ref, fan_name, notes, net_micros FROM shift_report_lines WHERE report_id = 8 ORDER BY seq`,
			"1|pf-2001||[]|20000000\n2|pf-2002|Quiet One|[\"другой фан\"]|20000000"},
		{`SELECT json_extract(payload, '$.lines[0].fan_name'), json_extract(payload, '$.lines[0].notes'), json_extract(payload, '$.lines[1].fan_name'),
			json_extract(payload, '$.lines[1].notes'), content_hash FROM shift_reports WHERE id = 8`, "|[]|Quiet One|[\"другой фан\"]|h8"},
		{`SELECT id, fan_name, note, transaction_ref, net_micros FROM sale_decisions WHERE id >= 20 ORDER BY id`,
			"20|||pf-2001|20000000\n21|Quiet One|чужое решение|pf-2002|20000000"},
		{`SELECT id, fan_name, question, note FROM sale_questions WHERE id >= 20 ORDER BY id`,
			"20||" + Erased + "|\n21|Quiet One|чей Quiet One?|спросили"},
		{`SELECT id, fan_name, note, transaction_ref, net_micros FROM pot_sales WHERE id >= 2 ORDER BY id`,
			"2|||pf-2001|20000000\n3|Quiet One|набор для другого|pf-2002|20000000"},
		{`SELECT transaction_ref, COALESCE(json_extract(payload, '$.fanRef'), '∅') FROM hub_sales WHERE transaction_ref IN ('2001', '2002') ORDER BY 1`,
			"2001|∅\n2002|556"},
	})
}

// Defect 2. A fan name is not a login: the display name of the erased fan may read as another fan's login. A name
// ledger's rows tie to another fan too is not used to find the erased fan's rows — whose a row with that word alone is,
// is not known —, and the erasure's stored counts say how many names were left out. What his id and his own sales tie
// to the erased fan goes as before.
func TestANameAnotherFanCarriesTooErasesNothingOfTheOtherFan(t *testing.T) {
	s, _ := book(t)
	// Fan 100 shows as «Alex»; fan 200's login is «alex». Fan 101 shows as «Kim»; another fan, 201, is called «kim» in
	// rows about his own sale. Fan 102's name reads as another account's id: «u202».
	sale(t, s, "3001", "pf-3001", "100")
	sale(t, s, "3002", "pf-3002", "200")
	sale(t, s, "3003", "pf-3003", "101")
	sale(t, s, "3004", "pf-3004", "201")
	sale(t, s, "3005", "pf-3005", "102")
	seed(t, s,
		`INSERT INTO shift_reports(id, person, hub_username, report_date, window_from, window_to, cut_at, version, total_micros, source, status,
			content_hash, payload, created_at, key_name) VALUES (9, 'boris', 'boris_hub', '2026-11-10', 'w', 'w', 'w', 1, 80000000, 'agent', 'posted', 'h9',
			'{"lines":[{"transaction_ref":"pf-3001","page":"page-a","fan_ref":"100","fan_name":"Alex","notes":["пишет по ночам"]},{"transaction_ref":"pf-3002","page":"page-a","fan_ref":"200","fan_name":"alex","notes":["любит кастомы"]}]}',
			'x', 'agent')`,
		`INSERT INTO shift_report_lines(report_id, seq, n, transaction_ref, page_label, fan_ref, fan_name, net_micros, counted, added, notes)
			VALUES (9, 1, 1, 'pf-3001', 'page-a', '100', 'Alex', 20000000, 1, 0, '["пишет по ночам"]'),
			(9, 2, 2, 'pf-3002', 'page-a', '200', 'alex', 20000000, 1, 0, '["любит кастомы"]'),
			(9, 3, 3, 'pf-3003', 'page-a', '101', 'Kim', 20000000, 1, 0, '["новый"]'),
			(9, 4, 4, 'pf-3005', 'page-a', '102', 'u202', 20000000, 1, 0, '["без логина"]')`,
		// Rows that name a fan by the word alone: by ledger's rows «alex» is fan 200 as much as fan 100.
		`INSERT INTO sale_decisions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, kind, shares, said_by, said_at, note, created_at, key_name)
			VALUES (30, 'page-a', 'alex', '', NULL, NULL, 'owner', '[]', 'владелец', '2026-11-11', 'alex обещал купить набор', 'x', 'agent'),
			(31, 'page-a', 'kim', '', '3004', 20000000, 'owner', '[]', 'владелец', '2026-11-11', 'kim — фан Бориса', 'x', 'agent'),
			(32, 'page-a', 'u202', '', NULL, NULL, 'owner', '[]', 'владелец', '2026-11-11', 'фан без логина', 'x', 'agent')`,
		`INSERT INTO sale_questions(id, page_label, fan_ref, fan_name, transaction_ref, question, asked_of, asked_at, note, status, created_at, key_name)
			VALUES (30, 'page-a', 'Alex', '', NULL, 'alex чей?', 'chatters', '2026-11-11', '', 'open', 'x', 'agent'),
			(31, 'page-a', 'Kim', '', 'pf-3004', 'kim чей?', 'chatters', '2026-11-11', '', 'open', 'x', 'agent')`,
		`INSERT INTO pot_sales(id, pot, transaction_ref, page_label, fan_ref, fan_name, net_micros, occurred_at, day, source, note, added_at, key_name)
			VALUES (4, 'set-one', NULL, 'page-a', 'alex', '', 5000000, '2026-11-10T20:00:00+03:00', '2026-11-10', 'agent', 'перевод от alex', 'x', 'agent')`)
	erase(t, s, hub.Erasure{ID: 61, Platform: "fansly", FanRef: "100", ErasedAt: "2026-11-13T08:00:00.000Z"},
		hub.Erasure{ID: 62, Platform: "fansly", FanRef: "101", ErasedAt: "2026-11-13T08:01:00.000Z"},
		hub.Erasure{ID: 63, Platform: "fansly", FanRef: "102", ErasedAt: "2026-11-13T08:02:00.000Z"})
	check(t, s, [][2]string{
		// The erased fans' own lines are cleared — their ids and their sales tie them; fan 200's line keeps its fan and notes.
		{`SELECT seq, fan_ref, fan_name, notes FROM shift_report_lines WHERE report_id = 9 ORDER BY seq`,
			"1|||[]\n2|200|alex|[\"любит кастомы\"]\n3|||[]\n4|||[]"},
		{`SELECT json_extract(payload, '$.lines[0].fan_name'), json_extract(payload, '$.lines[1].fan_ref'), json_extract(payload, '$.lines[1].fan_name'),
			json_extract(payload, '$.lines[1].notes') FROM shift_reports WHERE id = 9`, "|200|alex|[\"любит кастомы\"]"},
		// The rows that name a fan by the word alone stay: «alex» — a row gives the name to fan 200's id; «kim» — a row
		// with the name is about fan 201's sale, by either of the sale's references; «u202» is another account's id itself.
		{`SELECT id, fan_ref, note FROM sale_decisions WHERE id >= 30 ORDER BY id`,
			"30|alex|alex обещал купить набор\n31|kim|kim — фан Бориса\n32|u202|фан без логина"},
		{`SELECT id, fan_ref, question FROM sale_questions WHERE id >= 30 ORDER BY id`, "30|Alex|alex чей?\n31|Kim|kim чей?"},
		{`SELECT id, fan_ref, note FROM pot_sales WHERE id = 4`, "4|alex|перевод от alex"},
		// What was left out is counted: one name of each of the three fans.
		{`SELECT id, json_extract(rows, '$.ambiguous_names'), json_extract(rows, '$.shift_report_lines') FROM hub_erasures WHERE id >= 61 ORDER BY id`,
			"61|1|1\n62|1|1\n63|1|1"},
		{`SELECT json_extract(value, '$.ambiguousNames') FROM cursors WHERE name = 'hub:erasures:status'`, "3"},
	})
}

// The other side of the rule: a name only the erased fan carries still finds the rows that name him by the word alone.
// A row that carries the name beside a reference that is no id — the agent wrote how the fan shows — gives the name to
// nobody else.
func TestANameOnlyTheErasedFanCarriesStillFindsHisRows(t *testing.T) {
	s, _ := book(t)
	sale(t, s, "3101", "pf-3101", "300")
	seed(t, s,
		`INSERT INTO shift_reports(id, person, hub_username, report_date, window_from, window_to, cut_at, version, total_micros, source, status,
			content_hash, payload, created_at, key_name) VALUES (10, 'boris', 'boris_hub', '2026-11-10', 'w', 'w', 'w', 1, 20000000, 'agent', 'posted', 'h10',
			'{"lines":[{"transaction_ref":"pf-3101","page":"page-a","fan_ref":"300","fan_name":"Solo","notes":[]}]}', 'x', 'agent')`,
		`INSERT INTO shift_report_lines(report_id, seq, n, transaction_ref, page_label, fan_ref, fan_name, net_micros, counted, added, notes)
			VALUES (10, 1, 1, 'pf-3101', 'page-a', '300', 'Solo', 20000000, 1, 0, '[]')`,
		`INSERT INTO sale_decisions(id, page_label, fan_ref, fan_name, transaction_ref, net_micros, kind, shares, said_by, said_at, note, created_at, key_name)
			VALUES (40, 'page-a', 'solo', '', NULL, NULL, 'owner', '[]', 'владелец', '2026-11-11', 'solo обещал вернуться', 'x', 'agent'),
			(41, 'page-b', 'solo', '', NULL, NULL, 'owner', '[]', 'владелец', '2026-11-11', 'solo другой площадки', 'x', 'agent')`,
		`INSERT INTO sale_questions(id, page_label, fan_ref, fan_name, transaction_ref, question, asked_of, asked_at, note, status, created_at, key_name)
			VALUES (40, 'page-a', 'SOLO', '', NULL, 'solo чей?', 'chatters', '2026-11-11', '', 'open', 'x', 'agent'),
			(41, 'page-a', 'Solo The Great', 'solo', NULL, 'как его звать?', 'chatters', '2026-11-11', '', 'open', 'x', 'agent')`)
	erase(t, s, hub.Erasure{ID: 71, Platform: "fansly", FanRef: "300", ErasedAt: "2026-11-13T08:00:00.000Z"})
	check(t, s, [][2]string{
		{`SELECT id, fan_ref, note FROM sale_decisions WHERE id >= 40 ORDER BY id`, "40||\n41|solo|solo другой площадки"},
		{`SELECT id, fan_ref, question FROM sale_questions WHERE id = 40`, "40||" + Erased},
		{`SELECT json_extract(rows, '$.ambiguous_names'), json_extract(rows, '$.sale_decisions') FROM hub_erasures WHERE id = 71`, "0|1"},
	})
}
