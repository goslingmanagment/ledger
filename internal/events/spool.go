package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Spool keeps events until the API has taken them. One file per (source, source_id): a run that reads
// the same content again while the API is down replaces the file, so nothing piles up, and a resend is
// a repeat for ledger.AddEvent.
type Spool struct{ Dir string }

func (s Spool) path(e Event) string {
	sum := sha256.Sum256([]byte(e.Source + "\x00" + e.SourceID))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])[:24]+".json")
}

// spooled keeps the order the events were read in: the first run decides the order of the tabs in
// payroll_tabs, and the monthly card adds them up in that order, like the script reading the tables.
type spooled struct {
	Seq   int   `json:"seq"`
	Event Event `json:"event"`
}

// Put writes the events of a run, each atomically.
func (s Spool) Put(list []Event) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	for i, e := range list {
		raw, err := json.Marshal(spooled{Seq: i, Event: e})
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(s.Dir, ".put-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(raw); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), s.path(e)); err != nil {
			return err
		}
	}
	return nil
}

// Pending lists the waiting events, oldest run first, each run in reading order.
func (s Spool) Pending() ([]Event, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	list := make([]spooled, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var e spooled
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("очередь %s: %w", path, err)
		}
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Event.OccurredAt != list[j].Event.OccurredAt {
			return list[i].Event.OccurredAt < list[j].Event.OccurredAt
		}
		return list[i].Seq < list[j].Seq
	})
	out := make([]Event, len(list))
	for i, e := range list {
		out[i] = e.Event
	}
	return out, nil
}

// Sender hands a batch over: the API or, with --local-db, the database itself.
type Sender func(ctx context.Context, batch []Event) (Answer, error)

// FlushReport says what a flush did.
type FlushReport struct {
	Sent      int      `json:"sent"`
	Inserted  int      `json:"inserted"`
	Repeated  int      `json:"repeated"`
	Pending   int      `json:"pending"`
	Conflicts []string `json:"conflicts,omitempty"`
	Settle    []string `json:"settle,omitempty"`
}

// flushBatch keeps a request and its follow-up work small.
const flushBatch = 40

// Flush sends everything waiting, oldest first, and forgets what the receiver took. A failure leaves
// the rest in the spool and comes back as the error. A conflict stays in the spool and is reported on
// every run until somebody looks: the receiver holds another payload under the same id.
func (s Spool) Flush(ctx context.Context, send Sender) (FlushReport, error) {
	var rep FlushReport
	pending, err := s.Pending()
	if err != nil {
		return rep, err
	}
	rep.Pending = len(pending)
	for start := 0; start < len(pending); start += flushBatch {
		batch := pending[start:min(start+flushBatch, len(pending))]
		ans, err := send(ctx, batch)
		if err != nil {
			return rep, err
		}
		if len(ans.Results) != len(batch) {
			return rep, fmt.Errorf("приёмник ответил %d результатами на %d событий", len(ans.Results), len(batch))
		}
		rep.Sent += len(batch)
		rep.Inserted += ans.Inserted
		rep.Repeated += ans.Repeated
		rep.Settle = append(rep.Settle, ans.Settle...)
		for i, r := range ans.Results {
			if r.Conflict != "" {
				rep.Conflicts = append(rep.Conflicts, r.Conflict)
				continue
			}
			if err := os.Remove(s.path(batch[i])); err != nil && !os.IsNotExist(err) {
				return rep, err
			}
			rep.Pending--
		}
	}
	return rep, nil
}
