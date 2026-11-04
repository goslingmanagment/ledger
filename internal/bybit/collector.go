package bybit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const (
	overlap          = 72 * time.Hour      // every read starts this far behind the watermark
	fundingLookback  = 10 * 24 * time.Hour // the Funding check covers these days
	keyCheckInterval = 24 * time.Hour
)

var noKeysOnce sync.Once

func logNoKeys(log *slog.Logger) {
	noKeysOnce.Do(func() {
		log.Warn("bybit: нет LEDGER_BYBIT_API_KEY / LEDGER_BYBIT_API_SECRET, сбор Bybit не работает")
	})
}

func init() {
	server.Every("bybit", 15*time.Minute, collectJob)
	server.Every("bybit-balance", time.Hour, balanceJob)
}

var jobCollector *Collector // one per process: it remembers the key check between runs

func collectJob(ctx context.Context, s *server.Server) error {
	keys, ok := KeysFromEnv()
	if !ok {
		logNoKeys(s.Log)
		return nil
	}
	if jobCollector == nil || jobCollector.Store != s.Store {
		jobCollector = NewCollector(s.Store, NewClient(keys))
	}
	_, err := jobCollector.Run(ctx, RunOptions{})
	return err
}

func balanceJob(ctx context.Context, s *server.Server) error {
	keys, ok := KeysFromEnv()
	if !ok {
		logNoKeys(s.Log)
		return nil
	}
	_, err := CollectBalance(ctx, s.Store, NewClient(keys), time.Now())
	return err
}

// Collector runs passes over the Bybit account.
type Collector struct {
	Store  *db.Store
	Client *Client
	Now    func() time.Time

	key        *KeyInfo
	keyChecked time.Time
}

// NewCollector builds a collector.
func NewCollector(store *db.Store, c *Client) *Collector {
	return &Collector{Store: store, Client: c, Now: time.Now}
}

// RunOptions: with Since set the pass reads that window once and leaves the watermarks and the status alone.
type RunOptions struct {
	Since time.Time
	Until time.Time
}

// PendingInfo is a hanging withdrawal in the status.
type PendingInfo struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Since  string `json:"since"`
	Amount string `json:"amount"`
	Coin   string `json:"coin"`
	Error  string `json:"error,omitempty"`
}

// StreamWindow is what a stream read in this pass.
type StreamWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Status is the result of a pass, kept in cursors (bybit:status) for the hourly checks.
type Status struct {
	At              string                  `json:"at"`
	OK              bool                    `json:"ok"`
	LastOKAt        string                  `json:"last_ok_at,omitempty"`
	Mode            string                  `json:"mode"`
	LiveUnwired     bool                    `json:"live_unwired,omitempty"` // mode live, but nothing writes documents yet
	Backfill        bool                    `json:"backfill,omitempty"`
	Requests        int                     `json:"requests"`
	Windows         map[string]StreamWindow `json:"windows"`
	Errors          map[string]string       `json:"errors,omitempty"`
	Fetched         int                     `json:"fetched"`
	NewEvents       int                     `json:"new_events"`
	KnownEvents     int                     `json:"known_events"`
	FundingFetched  int                     `json:"funding_fetched"`
	NewFunding      int                     `json:"new_funding"`
	SkippedStatus   int                     `json:"skipped_status"`
	Mismatches      []string                `json:"mismatches,omitempty"` // a stored movement whose money fields changed at Bybit
	Pending         []PendingInfo           `json:"pending_withdrawals"`
	ResolvedPending int                     `json:"resolved_pending"`
	Proposals       ClassifyReport          `json:"proposals"`
	NoteLinks       NoteLinks               `json:"note_links"`
	Superseded      int                     `json:"proposals_superseded"`
	Funding         *FundingReport          `json:"funding,omitempty"`
	Key             *KeyInfo                `json:"key,omitempty"`
}

func (c *Collector) checkKey(ctx context.Context, now time.Time, st *Status) error {
	if c.key == nil || now.Sub(c.keyChecked) >= keyCheckInterval {
		info, err := c.Client.CheckKey(ctx)
		if err != nil {
			st.Errors["key"] = err.Error() // the check failing is not a reason to stop collecting
			return nil
		}
		c.key, c.keyChecked = &info, now
	}
	st.Key = c.key
	if !c.key.ReadOnly {
		return errors.New("ключ Bybit не только на чтение: сбор остановлен, пересоздать ключ")
	}
	return nil
}

func (c *Collector) since(ctx context.Context, q ledger.Querier, cursor string, start time.Time, opt RunOptions) (time.Time, error) {
	if !opt.Since.IsZero() {
		return opt.Since, nil
	}
	wm, ok, err := watermark(ctx, q, cursor)
	if err != nil || !ok {
		return start, err
	}
	if s := wm.Add(-overlap); s.After(start) {
		return s, nil
	}
	return start, nil
}

// Run does one pass: reads the streams from the network, then stores everything in one transaction.
// What a failed stream did not read comes with the next pass: its watermark stays put.
func (c *Collector) Run(ctx context.Context, opt RunOptions) (Status, error) {
	now := c.Now()
	until := opt.Until
	if until.IsZero() {
		until = now
	}
	backfill := !opt.Since.IsZero()
	q := c.Store.RW
	calls := c.Client.Calls
	start, err := Start(ctx, q)
	if err != nil {
		return Status{}, err
	}
	mode, err := Mode(ctx, q)
	if err != nil {
		return Status{}, err
	}
	st := Status{At: ts.Format(now), Mode: mode, Backfill: backfill, Windows: map[string]StreamWindow{}, Errors: map[string]string{},
		Pending: []PendingInfo{}}
	st.LiveUnwired = mode == "live" && LiveHook == nil

	if err := c.checkKey(ctx, now, &st); err != nil {
		st.Errors["key"] = err.Error()
		if !backfill {
			if werr := c.saveStatus(ctx, q, &st, now); werr != nil {
				return st, errors.Join(err, werr)
			}
		}
		return st, err
	}

	sinceM, err := c.since(ctx, q, cursorMovements, start, opt)
	if err != nil {
		return st, err
	}
	sinceP, err := c.since(ctx, q, cursorP2P, start, opt)
	if err != nil {
		return st, err
	}
	sinceF, err := c.since(ctx, q, cursorFunding, start, opt)
	if err != nil {
		return st, err
	}
	st.Windows["movements"] = StreamWindow{ts.FormatMilli(sinceM), ts.FormatMilli(until)}
	st.Windows["p2p"] = StreamWindow{ts.FormatMilli(sinceP), ts.FormatMilli(until)}
	st.Windows["funding"] = StreamWindow{ts.FormatMilli(sinceF), ts.FormatMilli(until)}

	fetched, errM := FetchMovements(ctx, c.Client, sinceM, until)
	if errM != nil {
		st.Errors["movements"] = errM.Error()
	}
	p2p, errP := FetchP2P(ctx, c.Client, sinceP, until)
	if errP != nil {
		st.Errors["p2p"] = errP.Error()
	}
	funding, errF := FetchFunding(ctx, c.Client, sinceF, until)
	if errF != nil {
		st.Errors["funding"] = errF.Error()
	}
	rechecked, err := c.recheckPending(ctx, q, fetched)
	if err != nil {
		return st, err
	}

	tx, err := c.Store.RW.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	movements := append(append([]Movement{}, fetched.Movements...), p2p...)
	for _, r := range rechecked {
		if r.status == withdrawOK && r.movement != nil {
			movements = append(movements, *r.movement)
		}
	}
	st.Fetched, st.SkippedStatus = len(movements), fetched.Skipped
	for _, m := range movements {
		ins, mismatch, err := storeMovement(ctx, tx, m, now)
		if err != nil {
			return st, err
		}
		if ins {
			st.NewEvents++
		} else {
			st.KnownEvents++
		}
		if mismatch != "" {
			st.Mismatches = append(st.Mismatches, mismatch)
		}
	}
	if err := applyPending(ctx, tx, fetched, rechecked, &st, now); err != nil {
		return st, err
	}
	st.Requests = c.Client.Calls - calls
	st.FundingFetched = len(funding)
	for _, r := range funding {
		_, ins, err := addEventOnce(ctx, tx, r.Event(), now)
		if err != nil {
			return st, err
		}
		if ins {
			st.NewFunding++
		}
	}
	if st.NoteLinks, err = LinkNotes(ctx, tx, now); err != nil {
		return st, err
	}
	if st.Superseded, err = supersedeLinked(ctx, tx, proposalAuthor, now); err != nil {
		return st, err
	}
	if st.Proposals, err = ClassifyEvents(ctx, tx, mode, start, now); err != nil {
		return st, err
	}
	if _, err := PostConverts(ctx, tx, now); err != nil {
		return st, err
	}
	report, err := fundingReport(ctx, tx, until)
	if err != nil {
		return st, err
	}
	st.Funding = &report
	if !backfill {
		for _, w := range []struct {
			name string
			err  error
		}{{cursorMovements, errM}, {cursorP2P, errP}, {cursorFunding, errF}} {
			if w.err == nil {
				if err := setCursor(ctx, tx, w.name, ts.FormatMilli(until), now); err != nil {
					return st, err
				}
			}
		}
		if err := c.saveStatus(ctx, tx, &st, now); err != nil {
			return st, err
		}
	} else {
		st.OK = len(st.Errors) == 0
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, errors.Join(errM, errP, errF)
}

// saveStatus writes the status cursor, keeping the time of the last clean pass.
func (c *Collector) saveStatus(ctx context.Context, q ledger.Querier, st *Status, now time.Time) error {
	st.OK = len(st.Errors) == 0
	prev, err := getCursor(ctx, q, cursorStatus)
	if err != nil {
		return err
	}
	if prev != "" {
		var p Status
		if json.Unmarshal([]byte(prev), &p) == nil {
			st.LastOKAt = p.LastOKAt
		}
	}
	if st.OK {
		st.LastOKAt = st.At
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return setCursor(ctx, q, cursorStatus, string(raw), now)
}

type recheck struct {
	row      pendingRow
	status   string
	movement *Movement
	err      error
}

// recheckPending asks Bybit by id about hanging withdrawals this pass did not meet in its window.
func (c *Collector) recheckPending(ctx context.Context, q ledger.Querier, f Fetched) ([]recheck, error) {
	open, err := openPending(ctx, q)
	if err != nil {
		return nil, err
	}
	met := map[string]bool{}
	for _, m := range f.Movements {
		met[m.Key] = true
	}
	for _, p := range f.Pending {
		met[p.Movement.Key] = true
	}
	var out []recheck
	for _, p := range open {
		if met[p.Key] {
			continue
		}
		r := recheck{row: p}
		r.status, r.movement, r.err = FetchWithdrawal(ctx, c.Client, strings.TrimPrefix(p.Key, "withdrawal:"))
		out = append(out, r)
	}
	return out, nil
}

// applyPending updates the table of hanging withdrawals. A success becomes an event by the normal
// path (the caller stored it); a terminal failure closes without money.
func applyPending(ctx context.Context, q ledger.Querier, f Fetched, rechecked []recheck, st *Status, now time.Time) error {
	succeeded := map[string]bool{}
	for _, m := range f.Movements {
		succeeded[m.Key] = true
	}
	for _, p := range f.Pending {
		if !succeeded[p.Movement.Key] {
			if err := upsertPending(ctx, q, p.Movement, p.Status, "", now); err != nil {
				return err
			}
		}
	}
	for _, r := range rechecked {
		switch {
		case r.err != nil:
			if err := upsertPending(ctx, q, r.row.Movement, r.row.LastStatus, r.err.Error(), now); err != nil {
				return err
			}
		case r.status == withdrawOK:
			if err := resolvePending(ctx, q, r.row.Key, withdrawOK, now); err != nil {
				return err
			}
			st.ResolvedPending++
		case withdrawFailed[r.status]:
			if err := resolvePending(ctx, q, r.row.Key, r.status, now); err != nil {
				return err
			}
			st.ResolvedPending++
		case r.status != "":
			m := r.row.Movement
			if r.movement != nil {
				m = *r.movement
			}
			if err := upsertPending(ctx, q, m, r.status, "", now); err != nil {
				return err
			}
		default: // Bybit does not know the id any more
			if err := upsertPending(ctx, q, r.row.Movement, r.row.LastStatus, "Bybit не вернул вывод по withdrawID", now); err != nil {
				return err
			}
		}
	}
	// Withdrawals tracked earlier that this pass found succeeded in its window.
	open, err := openPending(ctx, q)
	if err != nil {
		return err
	}
	for _, p := range open {
		if succeeded[p.Key] {
			if err := resolvePending(ctx, q, p.Key, withdrawOK, now); err != nil {
				return err
			}
			st.ResolvedPending++
			continue
		}
		info := PendingInfo{Key: p.Key, Status: p.LastStatus, Since: p.FirstSeen, Amount: p.Movement.Amount, Coin: p.Movement.Coin}
		var e string
		if err := q.QueryRowContext(ctx, `SELECT error FROM bybit_pending WHERE key = ?`, p.Key).Scan(&e); err != nil && err != sql.ErrNoRows {
			return err
		}
		info.Error = e
		st.Pending = append(st.Pending, info)
	}
	return nil
}

// fundingReport checks the Funding rows of the last days against the stored movements.
func fundingReport(ctx context.Context, q ledger.Querier, until time.Time) (FundingReport, error) {
	since := until.Add(-fundingLookback)
	rows, err := q.QueryContext(ctx, `SELECT payload FROM events WHERE source = 'bybit' AND kind = 'funding'
		AND occurred_at >= ? ORDER BY occurred_at, id`, ts.Format(since))
	if err != nil {
		return FundingReport{}, err
	}
	var list []FundingRow
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return FundingReport{}, err
		}
		var r FundingRow
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			rows.Close()
			return FundingReport{}, fmt.Errorf("event payload: %v", err)
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return FundingReport{}, err
	}
	opRows, err := q.QueryContext(ctx, `SELECT payload FROM events WHERE source = 'bybit'
		AND kind IN ('deposit', 'internal_deposit', 'withdrawal', 'p2p') AND occurred_at >= ? ORDER BY occurred_at, id`,
		ts.Format(since.Add(-matchWindowMs*time.Millisecond)))
	if err != nil {
		return FundingReport{}, err
	}
	var ops []Op
	for opRows.Next() {
		var raw string
		if err := opRows.Scan(&raw); err != nil {
			opRows.Close()
			return FundingReport{}, err
		}
		var m Movement
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			opRows.Close()
			return FundingReport{}, fmt.Errorf("event payload: %v", err)
		}
		ops = append(ops, Op{Key: m.Key, Direction: m.Direction, Coin: m.Coin, Amount: m.Amount, Fee: m.Fee, TimeMs: m.TimeMs})
	}
	opRows.Close()
	if err := opRows.Err(); err != nil {
		return FundingReport{}, err
	}
	rep := Reconcile(list, ops, since.UnixMilli())
	rep.From, rep.To = ts.Format(since), ts.Format(until)
	return rep, nil
}
