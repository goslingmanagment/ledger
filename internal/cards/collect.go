package cards

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

	"github.com/goslingmanagment/ledger/internal/ts"
)

// Limits of one read of the bot's chat.
const (
	DefaultLimit  = 400
	firstLookback = 7 * 24 * time.Hour
	overlap       = 24 * time.Hour
)

// Config is ~/.config/ledger/cards.json on the owner's Mac: never in the repository.
type Config struct {
	Peer    json.Number `json:"peer"`    // the bot's peer in tg
	Account string      `json:"account"` // the tg account that sees it
	TgDir   string      `json:"tg_dir"`  // the tg-inbox checkout `uv run tg` runs in
	Since   string      `json:"since,omitempty"`
}

// LoadConfig reads the collector's config; a file readable by others is refused like credentials.
func LoadConfig(path string) (Config, error) {
	var cfg Config
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
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Peer == "" || cfg.Account == "" || cfg.TgDir == "" {
		return cfg, fmt.Errorf(`%s: нужны "peer", "account" и "tg_dir"`, path)
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

// Reader returns the bot's history since a date, with the arguments of tg history.
type Reader func(ctx context.Context, peer, account, since string, limit int) ([]byte, error)

// TgReader runs `uv run tg history` in the tg-inbox checkout: a read-only command of the tg CLI.
func TgReader(dir string) Reader {
	return func(ctx context.Context, peer, account, since string, limit int) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, "uv", "run", "tg", "history", peer, "--account", account, "--since", since, "--limit", strconv.Itoa(limit))
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

// Collected is what one read of the chat gave.
type Collected struct {
	Since    string
	Messages int
	Events   []Parsed
	Last     time.Time // the newest message read
}

// Collect reads and parses the bot's chat since a date.
func Collect(ctx context.Context, cfg Config, read Reader, since string, limit int) (Collected, error) {
	got := Collected{Since: since}
	raw, err := read(ctx, cfg.Peer.String(), cfg.Account, since, limit)
	if err != nil {
		return got, err
	}
	var h History
	if err := json.Unmarshal(raw, &h); err != nil {
		return got, fmt.Errorf("tg history: ответ не JSON: %w", err)
	}
	got.Messages = len(h.Messages)
	for _, m := range h.Messages {
		if t, err := ts.Parse(m.Date); err == nil && t.After(got.Last) {
			got.Last = t
		}
		p, err := ParseMessage(m)
		if err != nil {
			return got, err
		}
		if p != nil {
			got.Events = append(got.Events, *p)
		}
	}
	return got, nil
}

// state is what the collector remembers between runs: where to read from next.
type state struct {
	LastMessageAt string `json:"last_message_at"`
}

// SinceFrom picks the date to read from: the newest message seen minus a day, so a message a sleeping
// Mac missed is read again; the first run goes by the config or a week back.
func SinceFrom(statePath string, cfg Config, now time.Time) string {
	raw, err := os.ReadFile(statePath)
	if err == nil {
		var st state
		if json.Unmarshal(raw, &st) == nil {
			if t, err := ts.Parse(st.LastMessageAt); err == nil {
				return ts.Date(t.Add(-overlap))
			}
		}
	}
	if cfg.Since != "" {
		return cfg.Since
	}
	return ts.Date(now.Add(-firstLookback))
}

// SaveState remembers the newest message read, atomically.
func SaveState(statePath string, last time.Time) error {
	if last.IsZero() {
		return nil
	}
	raw, err := json.Marshal(state{LastMessageAt: ts.Format(last)})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return err
	}
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, statePath); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}
