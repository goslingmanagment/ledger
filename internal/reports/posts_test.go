package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const peer = "-1001000000001"

// postBook is book with a second Hub page and the settings of one «Отчётность» topic: «vip» in a post is page-b,
// anna writes from Telegram id 501, boris is known by his tag only.
func postBook(t *testing.T) *db.Store {
	t.Helper()
	s := book(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.RW.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at) VALUES ('page-b', 'onlyfans', 'model-x', 'Model X', ?)`, ts.Format(ts.Now()))
	setSetting(t, s, "report_topics", `[{"peer": "`+peer+`", "topic": 4, "model": "model-x", "pages": {"VIP": "page-b"}}]`)
	setSetting(t, s, "report_authors", `{"tg:501": "anna", "#Boris": "boris"}`)
	return s
}

func setSetting(t *testing.T, s *db.Store, key, value string) {
	t.Helper()
	if _, err := s.RW.Exec(`INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, 'test', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value, ts.Format(ts.Now())); err != nil {
		t.Fatal(err)
	}
}

// send posts a message as the collector would, through events.Accept: the source parses it on arrival.
func send(t *testing.T, s *db.Store, msgID int64, sender, at, text string) {
	t.Helper()
	e := PostEvent{Peer: peer, Topic: 4, MsgID: msgID, Date: at, Sender: "tg user", SenderID: sender, Text: text}
	payload, _ := json.Marshal(e)
	ans, err := events.Accept(context.Background(), s, []events.Event{{Source: PostSource, SourceID: PostSourceID(peer, 4, msgID, text),
		Kind: "post", OccurredAt: at, Payload: payload}}, ts.Now())
	if err != nil || len(ans.Settle) > 0 {
		t.Fatalf("accept: %v %v", err, ans.Settle)
	}
}

func report(date, tag string, total string, lines ...string) string {
	return "🗓 Дата: " + date + "\n👤 Чаттер: " + tag + "\n\n📌 Аккаунты:\n" + strings.Join(lines, "\n") + "\n\n💰Общий доход: " + total +
		"\n\n📝 Комментарий:"
}

// days renders the pay base as «person date page posted/draft state».
func days(t *testing.T, s *db.Store) []string {
	t.Helper()
	list, err := Days(context.Background(), s.RO, Filter{From: "2026-11-01", To: "2026-11-30"})
	if err != nil {
		t.Fatal(err)
	}
	return renderDays(list)
}

func renderDays(list []Day) []string {
	opt := func(v *int64) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprint(*v / 10_000)
	}
	out := []string{}
	for _, d := range list {
		out = append(out, fmt.Sprintf("%s %s %s %s/%s %s", d.Person, d.ReportDate, d.Page, opt(d.PostedMicros), opt(d.DraftMicros), d.State))
	}
	return out
}

func flagsOf(t *testing.T, s *db.Store, msgID int64) string {
	t.Helper()
	var f string
	if err := s.RO.QueryRow(`SELECT flags FROM shift_report_posts WHERE msg_id = ? AND current = 1`, msgID).Scan(&f); err != nil {
		t.Fatalf("post %d: %v", msgID, err)
	}
	return f
}

func statusOf(t *testing.T, s *db.Store, id int64) string {
	t.Helper()
	var st string
	if err := s.RO.QueryRow(`SELECT status FROM shift_reports WHERE id = ?`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func want(t *testing.T, got []string, exp ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(exp, "\n") {
		t.Errorf("pay base:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(exp, "\n"))
	}
}

func TestPostConfirmsDraftAndEditFollows(t *testing.T) {
	s := postBook(t)
	ctx := context.Background()
	draft, err := Save(ctx, s, raw(t, sample("anna_hub")), "agent", ts.Now()) // anna 2026-11-08: page-a 31.52, page-b 181.60
	if err != nil || draft.Person == nil || *draft.Person != "anna" {
		t.Fatalf("draft: %v %+v", err, draft)
	}
	want(t, days(t, s), "anna 2026-11-08 page-a -/3152 draft_only", "anna 2026-11-08 page-b -/18160 draft_only")

	const at = "2026-11-08T20:30:00Z" // 23:30 MSK
	first := report("08.11.2026", "#Anna", "$213,12", "Fansly", "• Page-A - $31,52", "OF", "• VIP - $181,60", "• Page-C - не в смене")
	send(t, s, 10, "501", at, first)
	want(t, days(t, s), "anna 2026-11-08 page-a 3152/3152 posted_equal", "anna 2026-11-08 page-b 18160/18160 posted_equal")
	if st := statusOf(t, s, draft.ID); st != StatusPosted {
		t.Errorf("draft after an equal post: %s", st)
	}
	if f := flagsOf(t, s, 10); f != "[]" {
		t.Errorf("flags of a clean post: %s", f)
	}
	// The same text again is a repeat: nothing new.
	send(t, s, 10, "501", at, first)
	if n := count(t, s, "shift_report_posts"); n != 1 {
		t.Errorf("a repeat made %d posts", n)
	}

	// The chatter edits the post: a new event, the old version stays but is no longer current.
	send(t, s, 10, "501", at, report("08.11.2026", "#Anna", "$216,60", "• Page-A - $35,00", "• VIP - $181,60"))
	want(t, days(t, s), "anna 2026-11-08 page-a 3500/3152 posted_changed", "anna 2026-11-08 page-b 18160/18160 posted_equal")
	var versions, current int
	if err := s.RO.QueryRow(`SELECT COUNT(*), SUM(current) FROM shift_report_posts WHERE msg_id = 10`).Scan(&versions, &current); err != nil ||
		versions != 2 || current != 1 {
		t.Errorf("versions %d, current %d: %v", versions, current, err)
	}
	if st := statusOf(t, s, draft.ID); st != StatusDraft {
		t.Errorf("draft after a differing edit: %s, want draft", st)
	}
	var d []Day
	if d, err = Days(ctx, s.RO, Filter{From: "2026-11-08", To: "2026-11-08", Person: "anna"}); err != nil || *d[0].DiffMicros != 3_480_000 {
		t.Errorf("difference: %v %+v", err, d)
	}

	// The agent saves a new version matching the post: posted at once; the old version stays superseded.
	m := sample("anna_hub")
	m["total_mills"] = 216600
	m["pages"].([]any)[0].(map[string]any)["amount_mills"] = 35000
	m["adds"].([]any)[0].(map[string]any)["amount_mills"] = 7000
	v2, err := Save(ctx, s, raw(t, m), "agent", ts.Now())
	if err != nil || v2.Version != 2 {
		t.Fatalf("version 2: %v %+v", err, v2)
	}
	if st := statusOf(t, s, v2.ID); st != StatusPosted {
		t.Errorf("new version equal to the post: %s", st)
	}
	if st := statusOf(t, s, draft.ID); st != StatusSuperseded {
		t.Errorf("old version: %s", st)
	}
	want(t, days(t, s), "anna 2026-11-08 page-a 3500/3500 posted_equal", "anna 2026-11-08 page-b 18160/18160 posted_equal")

	// GET /v1/reports shows the post on each page.
	list, err := List(ctx, s.RO, Filter{From: "2026-11-08", To: "2026-11-08", AllVersions: true})
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	old, cur := list[0], list[1]
	if cur.PostedAt == nil || *cur.PostedAt != "2026-11-08T23:30:00+03:00" || cur.Pages[0].PostState != StatePostedEqual ||
		*cur.Pages[0].PostedMicros != 35_000_000 {
		t.Errorf("current version: %+v", cur)
	}
	if old.Pages[0].PostState != StatePostedChanged || old.Pages[1].PostState != StatePostedEqual {
		t.Errorf("old version against the post: %+v", old.Pages)
	}
}

func TestAuthorsPagesAndFlags(t *testing.T) {
	s := postBook(t)
	ctx := context.Background()
	at := "2026-11-05T19:00:00Z"
	// boris by his tag (the sender is unknown), the bare platform word: model-x has one Fansly page.
	send(t, s, 20, "777", at, report("05.11.2026", "#boris", "$204,00", "-Fansly - $204,00"))
	// Nobody knows this author.
	send(t, s, 21, "999", at, report("05.11.2026", "#nobody", "$5", "• Page-A - $5"))
	// anna's sender id with boris's tag: the sender wins.
	send(t, s, 22, "501", at, report("05.11.2026", "#Boris", "$7", "• Page-A - $7"))
	// A page nobody mapped: the other page still counts.
	send(t, s, 23, "501", "2026-11-04T19:00:00Z", report("04.11.2026", "#Anna", "$12", "• Page-A - $4", "• Page-Z - $8"))
	// Totals that do not add up, a date far from the day of posting, a date that does not read.
	send(t, s, 24, "501", "2026-11-03T19:00:00Z", report("03.11.2026", "#Anna", "**$140**", "-VIP: $8", "-Page-A: $92"))
	send(t, s, 25, "501", "2026-11-06T19:00:00Z", report("01.11.2026", "#Anna", "$3", "• Page-A - $3"))
	send(t, s, 26, "501", "2026-11-06T19:00:00Z", report("позавчера", "#Anna", "$3", "• Page-A - $3"))
	// Talk in the topic is not a post.
	send(t, s, 27, "501", at, "Общий доход за неделю хороший, без даты")

	for id, exp := range map[int64]string{
		20: `[]`, 21: `["unknown_author"]`, 22: `["tag_mismatch"]`, 23: `["unknown_page"]`,
		24: `["sum_mismatch"]`, 25: `["date_mismatch"]`, 26: `["no_date"]`,
	} {
		if f := flagsOf(t, s, id); f != exp {
			t.Errorf("post %d flags %s, want %s", id, f, exp)
		}
	}
	if n := count(t, s, "shift_report_posts"); n != 7 {
		t.Errorf("%d posts, want 7: talk is not a post", n)
	}
	// 22 is anna's (sender), and 23… are blocked except the known page of 23.
	want(t, days(t, s),
		"anna 2026-11-04 page-a 400/- post_only",
		"anna 2026-11-05 page-a 700/- post_only",
		"boris 2026-11-05 page-a 20400/- post_only")

	// The owner maps the missing name: the post becomes whole without collecting again.
	setSetting(t, s, "report_topics", `[{"peer": "`+peer+`", "topic": 4, "model": "model-x", "pages": {"vip": "page-b", "page-z": "page-b"}}]`)
	if _, err := ParsePostsNow(ctx, s); err != nil {
		t.Fatal(err)
	}
	if f := flagsOf(t, s, 23); f != `[]` {
		t.Errorf("after the mapping: %s", f)
	}
	want(t, days(t, s)[:2], "anna 2026-11-04 page-a 400/- post_only", "anna 2026-11-04 page-b 800/- post_only")

	// The check lists what the pay base cannot use; it names who, the date and the flag in words.
	posts, err := Posts(ctx, s.RO, "2026-11-01", "2026-11-30", true)
	if err != nil {
		t.Fatal(err)
	}
	var blocked []string
	for _, p := range posts {
		if p.Blocked {
			blocked = append(blocked, fmt.Sprint(p.MsgID))
		}
	}
	sort.Strings(blocked)
	if strings.Join(blocked, ",") != "21,24,25,26" {
		t.Errorf("blocked posts: %v", blocked)
	}
}

// ParsePostsNow runs a parsing pass in its own transaction.
func ParsePostsNow(ctx context.Context, s *db.Store) (ParseReport, error) {
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		return ParseReport{}, err
	}
	defer tx.Rollback()
	rep, err := ParsePosts(ctx, tx, time.Now())
	if err != nil {
		return rep, err
	}
	return rep, tx.Commit()
}

func TestDuplicateMessagesLatestWins(t *testing.T) {
	s := postBook(t)
	send(t, s, 30, "501", "2026-11-07T18:00:00Z", report("07.11.2026", "#Anna", "$10", "• Page-A - $10"))
	send(t, s, 31, "501", "2026-11-07T19:00:00Z", report("07.11.2026", "#Anna", "$12", "• Page-A - $12"))
	for _, id := range []int64{30, 31} {
		if f := flagsOf(t, s, id); f != `["duplicate"]` {
			t.Errorf("post %d: %s", id, f)
		}
	}
	want(t, days(t, s), "anna 2026-11-07 page-a 1200/- post_only")
	// Parsing again changes nothing.
	rep, err := ParsePostsNow(context.Background(), s)
	if err != nil || rep.Written != 0 || rep.Statuses != 0 {
		t.Errorf("second pass: %v %+v", err, rep)
	}
}

func TestReportDaysFilterAndScopes(t *testing.T) {
	s := postBook(t)
	ctx := t.Context()
	if _, err := Save(ctx, s, raw(t, sample("anna_hub")), "agent", ts.Now()); err != nil {
		t.Fatal(err)
	}
	m := sample("boris")
	m["hub_user_id"] = 42
	if _, err := Save(ctx, s, raw(t, m), "agent", ts.Now()); err != nil {
		t.Fatal(err)
	}
	send(t, s, 40, "777", "2026-11-08T20:00:00Z", report("08.11.2026", "#boris", "$1", "• Page-A - $1"))
	srv := httptest.NewServer(server.New(s, quiet, "test").Mux)
	defer srv.Close()
	key := func(name string, scopes ...string) *client.Client {
		token, _, err := auth.Create(ctx, s.RW, name, scopes, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return client.New(config.Client{URL: srv.URL, Key: token})
	}
	hubKey, reader, agent := key("hub", auth.ScopePayouts), key("reader", auth.ScopeRead), key("agent", auth.ScopeReports)
	status := func(err error) int {
		if err == nil {
			return http.StatusOK
		}
		return err.(*client.Error).Status
	}
	const window = "/v1/report-days?from=2026-11-01&to=2026-11-10"
	rows := func(c *client.Client, q string) []string {
		t.Helper()
		var ans DaysAnswer
		if err := c.Do(ctx, http.MethodGet, window+q, nil, &ans); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return renderDays(ans.Days)
	}
	all := rows(reader, "")
	if len(all) != 4 {
		t.Errorf("all rows: %v", all)
	}
	want(t, rows(hubKey, "&hub_username=anna_hub"), "anna 2026-11-08 page-a -/3152 draft_only", "anna 2026-11-08 page-b -/18160 draft_only")
	want(t, rows(hubKey, "&hub_user_id=42"), "boris 2026-11-08 page-a 100/3152 posted_changed", "boris 2026-11-08 page-b -/18160 draft_only")
	// Never another person's rows: an unknown user, or a person param that is not the user, gets nothing.
	want(t, rows(hubKey, "&hub_username=someone"))
	want(t, rows(hubKey, "&hub_user_id=5"))
	want(t, rows(hubKey, "&hub_user_id=42&person=anna"))
	want(t, rows(hubKey, "&person=anna"), "anna 2026-11-08 page-a -/3152 draft_only", "anna 2026-11-08 page-b -/18160 draft_only")

	// payouts reads the pay base and the list of posts without their text; read reads both with the text; reports neither.
	var bare struct {
		Posts []map[string]any `json:"posts"`
	}
	if err := hubKey.Do(ctx, http.MethodGet, "/v1/report-posts?from=2026-11-01&to=2026-11-10", nil, &bare); err != nil || len(bare.Posts) != 1 {
		t.Errorf("payouts on report-posts: %v %+v", err, bare)
	} else if _, has := bare.Posts[0]["text"]; has {
		t.Errorf("payouts got the text of a post: %+v", bare.Posts[0])
	}
	var posts PostsAnswer
	if err := reader.Do(ctx, http.MethodGet, "/v1/report-posts?from=2026-11-01&to=2026-11-10", nil, &posts); err != nil || len(posts.Posts) != 1 ||
		posts.Posts[0].Pages[0].Page == nil || *posts.Posts[0].Pages[0].Page != "page-a" || posts.Posts[0].Text == "" {
		t.Errorf("read on report-posts: %v %+v", err, posts)
	}
	for _, path := range []string{window, "/v1/report-posts?from=2026-11-01&to=2026-11-10"} {
		if code := status(agent.Do(ctx, http.MethodGet, path, nil, nil)); code != http.StatusForbidden {
			t.Errorf("reports key on %s: %d", path, code)
		}
	}
	if code := status(hubKey.Do(ctx, http.MethodPost, "/v1/report-posts/parse", map[string]any{}, nil)); code != http.StatusForbidden {
		t.Errorf("payouts on parse: %d", code)
	}
	if code := status(hubKey.Do(ctx, http.MethodGet, "/v1/report-days?from=2026-09-01&to=2026-11-10", nil, nil)); code != http.StatusBadRequest {
		t.Errorf("window over 62 days: %d", code)
	}
}

func TestSourceIDFollowsTheText(t *testing.T) {
	a := PostSourceID(peer, 4, 10, "текст")
	if a != PostSourceID(peer, 4, 10, "текст") {
		t.Error("the same text gives another id")
	}
	if a == PostSourceID(peer, 4, 10, "текст, правка") {
		t.Error("an edit keeps the id")
	}
	if !strings.HasPrefix(a, peer+":4:10:") || len(a) != len(peer+":4:10:")+12 {
		t.Errorf("id %s", a)
	}
}

func TestCollectPosts(t *testing.T) {
	var calls []string
	read := func(_ context.Context, p string, topic int64, account, since string, limit int) ([]byte, error) {
		calls = append(calls, fmt.Sprintf("%s/%d %s %d", p, topic, since, limit))
		return []byte(`{"messages": [
			{"id": 1, "date": "2026-11-08T20:00:00+00:00", "sender": "a", "sender_id": 501, "text": "` + strings.ReplaceAll(report("08.11.2026", "#Anna", "$1", "• Page-A - $1"), "\n", `\n`) + `"},
			{"id": 2, "date": "2026-11-08T20:01:00+00:00", "sender": "a", "sender_id": 501, "text": "кто в смене?"},
			{"id": 3, "date": "2026-11-08T20:02:00+00:00", "sender": null, "sender_id": null, "text": null}]}`), nil
	}
	cfg := CollectConfig{Topics: []CollectTopic{{Peer: PeerID(peer), Topic: 4}}}
	got, err := CollectPosts(context.Background(), cfg, read, "2026-11-05", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages != 3 || len(got.Events) != 1 || calls[0] != peer+"/4 2026-11-05 10" {
		t.Fatalf("collected %+v, calls %v", got, calls)
	}
	var e PostEvent
	if err := json.Unmarshal(got.Events[0].Payload, &e); err != nil || e.SenderID != "501" || e.Date != "2026-11-08T23:00:00+03:00" {
		t.Errorf("payload %+v: %v", e, err)
	}
	// What the server took is not sent again; an edit is.
	path := t.TempDir() + "/sent.json"
	sent := LoadSent(path)
	if len(sent.Fresh(got.Events)) != 1 {
		t.Fatal("nothing fresh at first")
	}
	if err := sent.Save(path, got.Events, "2026-11-05"); err != nil {
		t.Fatal(err)
	}
	if n := len(LoadSent(path).Fresh(got.Events)); n != 0 {
		t.Errorf("%d sent again", n)
	}
	edited := got.Events[0]
	edited.SourceID = PostSourceID(peer, 4, 1, "другой текст")
	if n := len(LoadSent(path).Fresh([]events.Event{edited})); n != 1 {
		t.Error("an edit is not fresh")
	}
}
