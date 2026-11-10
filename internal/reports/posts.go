package reports

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/brief"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// PostSource is the event source of the chatters' posts in «Отчётность», sent by `ledger collect reports`.
const PostSource = "tg_reports"

// Flags of a post: stable machine codes.
const (
	FlagUnknownAuthor = "unknown_author"
	FlagTagMismatch   = "tag_mismatch"
	FlagUnknownPage   = "unknown_page"
	FlagSumMismatch   = "sum_mismatch"
	FlagDateMismatch  = "date_mismatch"
	FlagNoDate        = "no_date"
	FlagDuplicate     = "duplicate"
)

// flagOrder is the order flags are stored and shown in.
var flagOrder = []string{FlagUnknownAuthor, FlagTagMismatch, FlagNoDate, FlagDateMismatch, FlagSumMismatch, FlagUnknownPage, FlagDuplicate}

// FlagText says a flag in Russian.
var FlagText = map[string]string{
	FlagUnknownAuthor: "автор не узнан: нет tg:<id> отправителя и #тега в report_authors",
	FlagTagMismatch:   "тег поста указывает на другого человека, чем отправитель; верен отправитель",
	FlagUnknownPage:   "имя страницы не сопоставлено со страницей хаба: добавить в pages топика (report_topics)",
	FlagSumMismatch:   "строки аккаунтов не сходятся с «Общим доходом»",
	FlagDateMismatch:  "дата отчёта дальше суток от дня отправки",
	FlagNoDate:        "дату отчёта не прочитать",
	FlagDuplicate:     "та же страница того же дня в другом сообщении: считается последнее",
}

// Blocking says whether a flag keeps the whole post out of the pay base (unknown_page keeps out only its page).
func Blocking(flag string) bool {
	switch flag {
	case FlagUnknownAuthor, FlagNoDate, FlagSumMismatch, FlagDateMismatch:
		return true
	}
	return false
}

// PostEvent is the payload of a tg_reports event: one Telegram message as read.
type PostEvent struct {
	Peer     string `json:"peer"`
	Topic    int64  `json:"topic"`
	MsgID    int64  `json:"msg_id"`
	Date     string `json:"date"`
	Sender   string `json:"sender"`
	SenderID string `json:"sender_id"`
	Text     string `json:"text"`
}

func (e PostEvent) key() string {
	return e.Peer + ":" + strconv.FormatInt(e.Topic, 10) + ":" + strconv.FormatInt(e.MsgID, 10)
}

// PostSourceID is the event id of a message: an edited text is a new event, the same text the same one.
func PostSourceID(peer string, topic, msgID int64, text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%s:%d:%d:%s", peer, topic, msgID, hex.EncodeToString(sum[:])[:12])
}

func init() {
	events.Register(events.Source{Name: PostSource,
		Arrive: func(_ context.Context, _ *sql.Tx, a events.Arrival) (bool, error) {
			var e PostEvent
			if err := json.Unmarshal(a.Payload, &e); err != nil || e.Peer == "" || e.MsgID <= 0 {
				return false, &events.BadRequest{Msg: "пост: нужен payload {peer, topic, msg_id, date, sender, sender_id, text}"}
			}
			if _, err := ts.Parse(e.Date); err != nil {
				return false, &events.BadRequest{Msg: "пост: date: " + err.Error()}
			}
			return a.Inserted, nil
		},
		Settle: func(ctx context.Context, store *db.Store, now time.Time) error {
			return brief.InTx(ctx, store, func(tx *sql.Tx) error {
				_, err := ParsePosts(ctx, tx, now)
				return err
			})
		}})
	server.Every("report-posts", 15*time.Minute, func(ctx context.Context, s *server.Server) error {
		return brief.InTx(ctx, s.Store, func(tx *sql.Tx) error {
			_, err := ParsePosts(ctx, tx, time.Now())
			return err
		})
	})
}

// ParseReport says what a parsing pass did.
type ParseReport struct {
	Events   int `json:"events"`
	Posts    int `json:"posts"`    // events that are reports
	Written  int `json:"written"`  // posts inserted or re-parsed with another result
	Removed  int `json:"removed"`  // stored posts whose event is no longer a report
	Flagged  int `json:"flagged"`  // current posts with a blocking flag
	Statuses int `json:"statuses"` // draft versions whose status changed
}

type postPage struct {
	Name    string  `json:"name"`
	Label   *string `json:"label"`
	InShift bool    `json:"in_shift"`
	Micros  int64   `json:"micros"`
}

// post is a stored version of a post as a parse makes it.
type post struct {
	EventID     int64      `json:"event_id"`
	Peer        string     `json:"peer"`
	Topic       int64      `json:"topic"`
	MsgID       int64      `json:"msg_id"`
	PostedAt    string     `json:"posted_at"`
	SenderID    string     `json:"sender_id"`
	Sender      string     `json:"sender"`
	Tag         string     `json:"tag"`
	Person      *string    `json:"person"`
	Model       string     `json:"model"`
	ReportDate  *string    `json:"report_date"`
	TotalMicros int64      `json:"total_micros"`
	PagesMicros int64      `json:"pages_micros"`
	Flags       []string   `json:"flags"`
	Text        string     `json:"text"`
	Current     bool       `json:"current"`
	Pages       []postPage `json:"pages"`
	key         string
}

func (p *post) blocked() bool {
	for _, f := range p.Flags {
		if Blocking(f) {
			return true
		}
	}
	return false
}

func (p *post) flag(f string) {
	for _, x := range p.Flags {
		if x == f {
			return
		}
	}
	p.Flags = append(p.Flags, f)
}

func (p *post) digest() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mapping is what turns names in a post into people and pages: the settings and ledger's mirror of Hub's pages.
type mapping struct {
	topics   map[string]hub.ReportTopic
	authors  map[string]string
	labels   map[string]string   // lowercased label → label
	platform map[string][]string // model + "\x00" + platform → labels
}

func loadMapping(ctx context.Context, q ledger.Querier) (mapping, error) {
	m := mapping{topics: map[string]hub.ReportTopic{}, labels: map[string]string{}, platform: map[string][]string{}}
	var topicsRaw, authorsRaw sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key = ?), (SELECT value FROM settings WHERE key = ?)`,
		hub.SettingReportTopics, hub.SettingReportAuthors).Scan(&topicsRaw, &authorsRaw); err != nil {
		return m, err
	}
	topics, err := hub.ParseReportTopics(topicsRaw.String)
	if err != nil {
		return m, err
	}
	for _, t := range topics {
		m.topics[t.Peer+":"+strconv.FormatInt(t.Topic, 10)] = t
	}
	if m.authors, err = hub.ParseReportAuthors(authorsRaw.String); err != nil {
		return m, err
	}
	models, err := payroll.HubPageModels(ctx, q)
	if err != nil {
		return m, err
	}
	rows, err := q.QueryContext(ctx, `SELECT page_label, platform FROM hub_pages ORDER BY page_label`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var label, platform string
		if err := rows.Scan(&label, &platform); err != nil {
			return m, err
		}
		m.labels[strings.ToLower(label)] = label
		k := models[label] + "\x00" + strings.ToLower(platform)
		m.platform[k] = append(m.platform[k], label)
	}
	return m, rows.Err()
}

// page is the Hub page a name of a post stands for: the topic's pages first, then a Hub label of that name, then a
// bare platform word when the topic's model has one page of that platform. "" — unknown.
func (m mapping) page(t hub.ReportTopic, name string) string {
	n := NormName(name)
	if l, ok := t.Pages[n]; ok {
		return l
	}
	if l, ok := m.labels[n]; ok {
		return l
	}
	if pl := PlatformOf(name); pl != "" && t.Model != "" {
		if list := m.platform[t.Model+"\x00"+pl]; len(list) == 1 {
			return list[0]
		}
	}
	return ""
}

// author is the person of a post: the sender id first, then the tag; mismatch when both name different people.
func (m mapping) author(senderID, tag string) (person string, mismatch bool) {
	bySender := ""
	if senderID != "" {
		bySender = m.authors["tg:"+senderID]
	}
	byTag := ""
	if tag != "" {
		byTag = m.authors[strings.ToLower(tag)]
	}
	switch {
	case bySender != "":
		return bySender, byTag != "" && byTag != bySender
	default:
		return byTag, false
	}
}

// build is the stored form of an event, or nil when the message is not a report.
func (m mapping) build(eventID int64, e PostEvent) *post {
	parsed, ok := ParsePost(e.Text)
	if !ok {
		return nil
	}
	at, err := ts.Parse(e.Date)
	if err != nil {
		return nil
	}
	topic := m.topics[e.Peer+":"+strconv.FormatInt(e.Topic, 10)]
	p := &post{EventID: eventID, Peer: e.Peer, Topic: e.Topic, MsgID: e.MsgID, PostedAt: ts.Format(at), SenderID: e.SenderID,
		Sender: e.Sender, Tag: parsed.Tag, Model: topic.Model, TotalMicros: parsed.TotalMicros, PagesMicros: parsed.PagesMicros(),
		Flags: []string{}, Text: e.Text, Pages: []postPage{}, key: e.key()}
	person, mismatch := m.author(e.SenderID, parsed.Tag)
	if person == "" {
		p.flag(FlagUnknownAuthor)
	} else {
		p.Person = &person
	}
	if mismatch {
		p.flag(FlagTagMismatch)
	}
	if d := parsed.ReportDate(at); d == "" {
		p.flag(FlagNoDate)
	} else {
		p.ReportDate = &d
		day, _ := time.ParseInLocation(ts.DateLayout, d, ts.MSK)
		postedDay, _ := time.ParseInLocation(ts.DateLayout, ts.Date(at), ts.MSK)
		if gap := day.Sub(postedDay); gap > 24*time.Hour || gap < -24*time.Hour {
			p.flag(FlagDateMismatch)
		}
	}
	if p.PagesMicros != p.TotalMicros {
		p.flag(FlagSumMismatch)
	}
	for _, pg := range parsed.Pages {
		row := postPage{Name: pg.Name, InShift: pg.InShift, Micros: pg.Micros}
		if l := m.page(topic, pg.Name); l != "" {
			row.Label = &l
		} else if pg.InShift {
			p.flag(FlagUnknownPage)
		}
		p.Pages = append(p.Pages, row)
	}
	return p
}

// ParsePosts parses every tg_reports event again and writes what changed: the posts, their pages, and the status of
// the agent's current drafts. Idempotent; a changed setting (a page name, an author) shows on the next pass without
// collecting again. It writes no posting and changes no balance.
func ParsePosts(ctx context.Context, tx *sql.Tx, now time.Time) (ParseReport, error) {
	var rep ParseReport
	m, err := loadMapping(ctx, tx)
	if err != nil {
		return rep, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, payload FROM events WHERE source = ? ORDER BY id`, PostSource)
	if err != nil {
		return rep, err
	}
	var list []*post
	latest := map[string]int64{} // message → its newest event
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return rep, err
		}
		rep.Events++
		var e PostEvent
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue // Arrive refused such payloads: an old one stays unread, not fatal
		}
		latest[e.key()] = id
		if p := m.build(id, e); p != nil {
			list = append(list, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	rep.Posts = len(list)
	for _, p := range list {
		p.Current = latest[p.key] == p.EventID
	}
	markDuplicates(list)
	for _, p := range list {
		sort.Slice(p.Flags, func(i, j int) bool { return flagRank(p.Flags[i]) < flagRank(p.Flags[j]) })
		if p.Current && p.blocked() {
			rep.Flagged++
		}
	}

	stored := map[int64]struct {
		id     int64
		digest string
	}{}
	srows, err := tx.QueryContext(ctx, `SELECT id, event_id, digest FROM shift_report_posts`)
	if err != nil {
		return rep, err
	}
	for srows.Next() {
		var id, ev int64
		var d string
		if err := srows.Scan(&id, &ev, &d); err != nil {
			srows.Close()
			return rep, err
		}
		stored[ev] = struct {
			id     int64
			digest string
		}{id, d}
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return rep, err
	}
	at := ts.Format(now)
	keep := map[int64]bool{}
	for _, p := range list {
		keep[p.EventID] = true
		d := p.digest()
		old, ok := stored[p.EventID]
		if ok && old.digest == d {
			continue
		}
		rep.Written++
		flags, _ := json.Marshal(p.Flags)
		var id int64
		if ok {
			id = old.id
			if _, err := tx.ExecContext(ctx, `DELETE FROM shift_report_post_pages WHERE post_id = ?`, id); err != nil {
				return rep, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE shift_report_posts SET posted_at = ?, sender_id = ?, sender = ?, tag = ?, person = ?,
				model = ?, report_date = ?, total_micros = ?, pages_micros = ?, flags = ?, text = ?, current = ?, digest = ?, parsed_at = ?
				WHERE id = ?`, p.PostedAt, p.SenderID, p.Sender, p.Tag, p.Person, p.Model, p.ReportDate, p.TotalMicros, p.PagesMicros,
				string(flags), p.Text, boolInt(p.Current), d, at, id); err != nil {
				return rep, err
			}
		} else {
			r, err := tx.ExecContext(ctx, `INSERT INTO shift_report_posts(event_id, peer, topic, msg_id, posted_at, sender_id, sender, tag,
				person, model, report_date, total_micros, pages_micros, flags, text, current, digest, parsed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.EventID, p.Peer, p.Topic, p.MsgID, p.PostedAt, p.SenderID,
				p.Sender, p.Tag, p.Person, p.Model, p.ReportDate, p.TotalMicros, p.PagesMicros, string(flags), p.Text, boolInt(p.Current), d, at)
			if err != nil {
				return rep, err
			}
			if id, err = r.LastInsertId(); err != nil {
				return rep, err
			}
		}
		for i, pg := range p.Pages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO shift_report_post_pages(post_id, seq, name, page_label, in_shift, amount_micros)
				VALUES (?, ?, ?, ?, ?, ?)`, id, i+1, pg.Name, pg.Label, boolInt(pg.InShift), pg.Micros); err != nil {
				return rep, err
			}
		}
	}
	// A stored post whose event no longer reads as a report (the parser learned better) is derived data: it goes.
	for ev, old := range stored {
		if keep[ev] {
			continue
		}
		rep.Removed++
		if _, err := tx.ExecContext(ctx, `DELETE FROM shift_report_post_pages WHERE post_id = ?`, old.id); err != nil {
			return rep, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shift_report_posts WHERE id = ?`, old.id); err != nil {
			return rep, err
		}
	}
	n, err := SyncStatuses(ctx, tx)
	rep.Statuses = n
	return rep, err
}

func flagRank(f string) int {
	for i, x := range flagOrder {
		if x == f {
			return i
		}
	}
	return len(flagOrder)
}

// markDuplicates flags the current, usable posts of one person and date that list the same page from different
// messages: the latest wins in the pay base, both carry the flag.
func markDuplicates(list []*post) {
	type dayPage struct{ person, date, page string }
	byPage := map[dayPage]map[string][]*post{}
	for _, p := range list {
		if !p.Current || p.Person == nil || p.ReportDate == nil || p.blocked() {
			continue
		}
		for _, pg := range p.Pages {
			if pg.Label == nil || !pg.InShift {
				continue
			}
			k := dayPage{*p.Person, *p.ReportDate, *pg.Label}
			if byPage[k] == nil {
				byPage[k] = map[string][]*post{}
			}
			byPage[k][p.key] = append(byPage[k][p.key], p)
		}
	}
	for _, msgs := range byPage {
		if len(msgs) < 2 {
			continue
		}
		for _, ps := range msgs {
			for _, p := range ps {
				p.flag(FlagDuplicate)
			}
		}
	}
}

// SyncStatuses sets the status of every current agent version from the pay base: posted when at least one of its
// pages is posted and every posted page of that person and day equals the draft, draft otherwise. Superseded versions
// are never touched. It returns how many changed.
func SyncStatuses(ctx context.Context, tx *sql.Tx) (int, error) {
	type day struct{ person, date string }
	anyPosted := map[int64]bool{}
	changed := map[day]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT person, report_date, report_id, state, posted_micros IS NOT NULL FROM v_report_pages`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var d day
		var report sql.NullInt64
		var state string
		var posted bool
		if err := rows.Scan(&d.person, &d.date, &report, &state, &posted); err != nil {
			rows.Close()
			return 0, err
		}
		if posted && report.Valid {
			anyPosted[report.Int64] = true
		}
		if posted && state != StatePostedEqual {
			changed[d] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id, COALESCE(person, ''), report_date, status FROM shift_reports
		WHERE source = ? AND status IN (?, ?)`, SourceAgent, StatusDraft, StatusPosted)
	if err != nil {
		return 0, err
	}
	type upd struct {
		id     int64
		status string
	}
	var todo []upd
	for rows.Next() {
		var id int64
		var d day
		var status string
		if err := rows.Scan(&id, &d.person, &d.date, &status); err != nil {
			rows.Close()
			return 0, err
		}
		want := StatusDraft
		if d.person != "" && anyPosted[id] && !changed[d] {
			want = StatusPosted
		}
		if want != status {
			todo = append(todo, upd{id, want})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, u := range todo {
		if _, err := tx.ExecContext(ctx, `UPDATE shift_reports SET status = ? WHERE id = ? AND status <> ?`, u.status, u.id, StatusSuperseded); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}
