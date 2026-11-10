package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/events"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The collector of the posts (`ledger collect reports`, on the owner's Mac): reads the «Отчётность» topics with
// `tg history`, read-only, and sends every message that looks like a report as a tg_reports event. The parsing is
// on the server.

// Limits of one read of a topic.
const (
	DefaultLimit     = 200
	DefaultSinceDays = 3
	maxPages         = 50
)

// PeerID is a chat id written in the config as a number or a string.
type PeerID string

// UnmarshalJSON takes -1001234 or "-1001234".
func (p *PeerID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*p = PeerID(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("peer: нужен id чата числом или строкой")
	}
	*p = PeerID(n.String())
	return nil
}

// CollectTopic is a topic to read.
type CollectTopic struct {
	Peer  PeerID `json:"peer"`
	Topic int64  `json:"topic"`
}

// CollectConfig is ~/.config/ledger/reports.json on the owner's Mac: where to read, never in the repository.
type CollectConfig struct {
	Topics    []CollectTopic `json:"topics"`
	SinceDays int            `json:"since_days"`
	Account   string         `json:"account,omitempty"` // the tg account that sees the groups; "" — tg's main one
	TgDir     string         `json:"tg_dir"`            // the tg-inbox checkout `uv run tg` runs in
}

// LoadCollectConfig reads the collector's config; a file readable by others is refused like credentials.
func LoadCollectConfig(path string) (CollectConfig, error) {
	var cfg CollectConfig
	st, err := os.Stat(path)
	if err != nil {
		return cfg, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return cfg, fmt.Errorf("%s is readable by others: chmod 600 %s", path, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Topics) == 0 || cfg.TgDir == "" {
		return cfg, fmt.Errorf(`%s: нужны "topics" [{"peer", "topic"}] и "tg_dir"`, path)
	}
	for i, t := range cfg.Topics {
		if t.Peer == "" || t.Topic <= 0 {
			return cfg, fmt.Errorf("%s: топик %d: нужны peer и topic > 0", path, i+1)
		}
	}
	if cfg.SinceDays <= 0 {
		cfg.SinceDays = DefaultSinceDays
	}
	if strings.HasPrefix(cfg.TgDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, err
		}
		cfg.TgDir = filepath.Join(home, cfg.TgDir[2:])
	}
	return cfg, nil
}

// TopicReader returns a topic's history since a time, the answer of tg history.
type TopicReader func(ctx context.Context, peer string, topic int64, account, since string, limit int) ([]byte, error)

// TgTopicReader runs `uv run tg history … --topic` in the tg-inbox checkout: a read-only command of the tg CLI. The
// peer goes after «--», so a negative chat id is never read as a flag.
func TgTopicReader(dir string) TopicReader {
	return func(ctx context.Context, peer string, topic int64, account, since string, limit int) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		args := []string{"run", "tg", "history", "--topic", strconv.FormatInt(topic, 10), "--since", since, "--limit", strconv.Itoa(limit)}
		if account != "" {
			args = append(args, "--account", account)
		}
		cmd := exec.CommandContext(cctx, "uv", append(args, "--", peer)...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if r := []rune(msg); len(r) > 300 {
				msg = string(r[len(r)-300:])
			}
			return nil, fmt.Errorf("tg history: %w: %s", err, msg)
		}
		return stdout.Bytes(), nil
	}
}

type tgMessage struct {
	ID       int64        `json:"id"`
	Date     string       `json:"date"`
	Sender   *string      `json:"sender"`
	SenderID *json.Number `json:"sender_id"`
	Text     *string      `json:"text"`
}

type tgHistory struct {
	Messages []tgMessage `json:"messages"`
}

// LooksLikeReport is the collector's filter: a message with «Общий доход». The server decides what is a report.
func LooksLikeReport(text string) bool { return totalRe.MatchString(text) }

// Collected is what one read of the topics gave.
type Collected struct {
	Since    string
	Messages int
	Events   []events.Event
}

// CollectPosts reads every topic since a date and makes an event of each message that looks like a report. tg
// history gives the oldest messages first and stops at the limit, so a full page means there is more.
func CollectPosts(ctx context.Context, cfg CollectConfig, read TopicReader, since string, limit int) (Collected, error) {
	got := Collected{Since: since}
	for _, t := range cfg.Topics {
		peer := string(t.Peer)
		seen := map[int64]bool{}
		from, lim := since, limit
		var last time.Time
		done := false
		for page := 0; page < maxPages && !done; page++ {
			raw, err := read(ctx, peer, t.Topic, cfg.Account, from, lim)
			if err != nil {
				return got, err
			}
			var h tgHistory
			if err := json.Unmarshal(raw, &h); err != nil {
				return got, fmt.Errorf("tg history %s/%d: ответ не JSON: %w", peer, t.Topic, err)
			}
			before, fresh := last, 0
			for _, m := range h.Messages {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				fresh++
				got.Messages++
				at, err := ts.Parse(m.Date)
				if err != nil {
					continue
				}
				if at.After(last) {
					last = at
				}
				if m.Text == nil || !LooksLikeReport(*m.Text) {
					continue
				}
				e := PostEvent{Peer: peer, Topic: t.Topic, MsgID: m.ID, Date: ts.Format(at), Text: *m.Text}
				if m.Sender != nil {
					e.Sender = *m.Sender
				}
				if m.SenderID != nil {
					e.SenderID = m.SenderID.String()
				}
				payload, err := json.Marshal(e)
				if err != nil {
					return got, err
				}
				got.Events = append(got.Events, events.Event{Source: PostSource, SourceID: PostSourceID(peer, t.Topic, m.ID, *m.Text),
					Kind: "post", OccurredAt: e.Date, Payload: payload})
			}
			switch {
			case len(h.Messages) < lim:
				done = true
			case fresh == 0 || !last.After(before):
				lim *= 2 // one second holds a whole page: a bigger page gets past it
			default:
				from = ts.Format(last)
			}
		}
		if !done {
			return got, fmt.Errorf("tg history %s/%d: за %d страниц история не кончилась", peer, t.Topic, maxPages)
		}
	}
	return got, nil
}

// Sent remembers which events the server already took, so an unchanged post is not sent again: source_id → the
// message's time, forgotten once it is older than the read window.
type Sent map[string]string

// LoadSent reads the file; a missing or unreadable one is empty.
func LoadSent(path string) Sent {
	s := Sent{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// Fresh is the events not sent yet.
func (s Sent) Fresh(list []events.Event) []events.Event {
	out := []events.Event{}
	for _, e := range list {
		if _, ok := s[e.SourceID]; !ok {
			out = append(out, e)
		}
	}
	return out
}

// Save adds the events, forgets those older than since, and writes the file atomically.
func (s Sent) Save(path string, list []events.Event, since string) error {
	for _, e := range list {
		s[e.SourceID] = e.OccurredAt
	}
	for id, at := range s {
		if at < since {
			delete(s, id)
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}
