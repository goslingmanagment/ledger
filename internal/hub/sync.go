package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	server.Every("hub", time.Hour, func(ctx context.Context, s *server.Server) error {
		cfg, err := ConfigFromEnv()
		if err != nil {
			return err
		}
		sy := NewSyncer(s.Store, cfg)
		sy.Log = func(msg string) { s.Log.Info("hub", "msg", msg) }
		rep, err := sy.Run(ctx, nil)
		if err != nil {
			return err
		}
		if n := rep.Unknown(); n > 0 {
			return fmt.Errorf("хаб не доказал %d выгрузок из %d: ledger hub status", n, len(rep.Results))
		}
		return nil
	})
}

// Kept fields of each mirrored dataset: what month.py caches of a transaction, what it reads of a
// payout request, the balance and its moment.
var keptFields = map[string][]string{
	"transactions":          {"occurredAt", "netMills", "currency", "transactionType", "transactionRef"},
	"payouts":               {"payoutRef", "requestedAt", "amountMills", "statusLabel", "methodProviderLabel", "updatedAtPlatform"},
	"ofapi_payout_requests": {"payoutRef", "requestedAt", "amountMills", "currency", "state"},
	"page_profile_daily":    {"day", "earningsBalanceMills", "capturedAt"},
	"pending_balance":       {"pendingMills", "firstSeenAt", "lastSeenAt"},
}

// balanceLookback is the read window of the daily balance datasets: a few of their newest rows.
const balanceLookback = 7 * 24 * time.Hour

// Syncer keeps the mirrors of every granted page.
type Syncer struct {
	Store  *db.Store
	Client *Client
	Now    func() time.Time
	Log    func(string)

	stepCalls int // Client.Calls when the current step began
}

// NewSyncer builds a syncer whose throttle state lives in the database.
func NewSyncer(store *db.Store, cfg Config) *Syncer {
	return &Syncer{Store: store, Client: NewClient(cfg, &Throttle{DB: store.RW}), Now: time.Now}
}

// Result is one (page, dataset) of a run.
type Result struct {
	Page     string   `json:"page"`
	Dataset  string   `json:"dataset"`
	Status   string   `json:"status"`
	Reasons  []string `json:"reasons,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Rows     int      `json:"rows"`
	Events   int      `json:"events"`
	Removed  int      `json:"removed,omitempty"`
	Requests int      `json:"requests"`
}

// Report is one run.
type Report struct {
	At       string   `json:"at"`
	Requests int      `json:"requests"`
	Events   int      `json:"events"`
	Results  []Result `json:"results"`
}

// Unknown counts what the hub did not prove.
func (r Report) Unknown() int {
	n := 0
	for _, x := range r.Results {
		if x.Status != "ok" {
			n++
		}
	}
	return n
}

func (s *Syncer) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

type runCtx struct {
	now       time.Time
	start     time.Time
	tail      time.Duration
	fullEvery time.Duration
	ex        Exceptions
}

// Run syncs pages (nil — the whole grant): transactions, then payout requests, then balances of each
// page, one request at a time. A refused read marks its (page, dataset) unknown and the run goes on.
func (s *Syncer) Run(ctx context.Context, pages []string) (rep Report, err error) {
	now := s.Now()
	rep = Report{At: ts.Format(now)}
	release, err := s.lease(ctx, now)
	if err != nil {
		return rep, err
	}
	defer release()
	set, err := LoadSettings(ctx, s.Store.RW)
	if err != nil {
		return rep, err
	}
	rc := runCtx{now: now}
	if rc.start, err = set.Time("hub_window_start"); err != nil {
		return rep, err
	}
	if rc.tail, err = set.Days("hub_tail_days"); err != nil {
		return rep, err
	}
	if rc.fullEvery, err = set.Days("hub_full_every_days"); err != nil {
		return rep, err
	}
	rpm, err := set.Int("hub_rpm")
	if err != nil {
		return rep, err
	}
	if s.Client.Throttle != nil {
		s.Client.Throttle.RPM = rpm
	}
	if rc.ex, err = LoadExceptions(ctx, s.Store.RW); err != nil {
		return rep, err
	}
	calls0 := s.Client.Calls
	defer func() { rep.Requests = s.Client.Calls - calls0 }()

	grant, err := s.Client.Capabilities(ctx)
	capRes := Result{Page: "*", Dataset: "capabilities", Status: "ok", Rows: len(grant), Requests: s.Client.Calls - calls0}
	if err != nil {
		if !IsRefusal(err) {
			return rep, err
		}
		capRes.Status, capRes.Reasons = "unknown", []string{err.Error()}
		if werr := s.write(ctx, func(tx *sql.Tx) error {
			return failStatus(ctx, tx, "*", "capabilities", capRes.Reasons, nil, nil, capRes.Requests, now)
		}); werr != nil {
			return rep, werr
		}
		rep.Results = append(rep.Results, capRes)
		return rep, nil
	}
	if err := s.write(ctx, func(tx *sql.Tx) error { return saveGrant(ctx, tx, grant, now, capRes.Requests) }); err != nil {
		return rep, err
	}
	rep.Results = append(rep.Results, capRes)

	byLabel := map[string]GrantPage{}
	for _, p := range grant {
		byLabel[p.PageLabel] = p
	}
	todo := append([]string(nil), pages...)
	if len(todo) == 0 {
		for _, p := range grant {
			todo = append(todo, p.PageLabel)
		}
	}
	sort.Strings(todo)
	for _, label := range todo {
		p, ok := byLabel[label]
		if !ok {
			return rep, fmt.Errorf("страницы %s нет в гранте ключа хаба", label)
		}
		steps := []string{"transactions"}
		switch p.Platform {
		case "fansly":
			steps = append(steps, "payouts", "page_profile_daily", "pending_balance")
		case "onlyfans":
			steps = append(steps, "ofapi_payout_requests")
		}
		for _, ds := range steps {
			res, err := s.step(ctx, rc, label, ds)
			if err != nil {
				return rep, err
			}
			rep.Events += res.Events
			rep.Results = append(rep.Results, res)
			if res.Status == "ok" {
				s.logf("%s %s: ok, %d строк, событий %d, запросов %d", label, ds, res.Rows, res.Events, res.Requests)
			} else {
				s.logf("%s %s: неизвестно — %v", label, ds, res.Reasons)
			}
		}
	}
	return rep, nil
}

// leaseFor outlives any sync: a run that died gives way after it.
const leaseFor = time.Hour

// lease lets one sync run at a time, across processes: a sync reads the mirror before its transaction, so
// a slower run applying its answer after a newer one would write older figures over newer ones.
func (s *Syncer) lease(ctx context.Context, now time.Time) (func(), error) {
	const name = "hub:sync:lease"
	err := s.write(ctx, func(tx *sql.Tx) error {
		var held string
		switch err := tx.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, name).Scan(&held); {
		case err == sql.ErrNoRows:
		case err != nil:
			return err
		default:
			if t, err := ts.Parse(held); err == nil && now.Before(t.Add(leaseFor)) {
				return refusef("идёт другая выгрузка хаба, начата %s", held)
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO cursors(name, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, name, ts.Format(now), ts.Format(now))
		return err
	})
	if err != nil {
		return nil, err
	}
	return func() {
		// The job's context may be gone by now; the lease must still go.
		lctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.write(lctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(lctx, `DELETE FROM cursors WHERE name = ?`, name)
			return err
		})
	}, nil
}

func (s *Syncer) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.Store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// step runs one dataset of one page and records its status; only operational errors come back.
func (s *Syncer) step(ctx context.Context, rc runCtx, page, ds string) (Result, error) {
	calls0 := s.Client.Calls
	s.stepCalls = calls0
	var res Result
	var from, to time.Time
	var err error
	switch ds {
	case "transactions":
		res, from, to, err = s.transactions(ctx, rc, page)
	case "payouts", "ofapi_payout_requests":
		res, from, to, err = s.payouts(ctx, rc, page, ds)
	default:
		res, from, to, err = s.balances(ctx, rc, page, ds)
	}
	res.Page, res.Dataset, res.Requests = page, ds, s.Client.Calls-calls0
	if err == nil {
		res.Status = "ok"
		return res, nil
	}
	if !IsRefusal(err) {
		return res, err
	}
	res.Status, res.Reasons = "unknown", []string{err.Error()}
	werr := s.write(ctx, func(tx *sql.Tx) error {
		return failStatus(ctx, tx, page, ds, res.Reasons, &from, &to, res.Requests, rc.now)
	})
	return res, werr
}

// ---- rows of the hub ----

// hubRow is one item reduced to its kept fields.
type hubRow struct {
	key    string
	at     time.Time // the dataset's own moment: occurredAt, requestedAt, capturedAt, lastSeenAt
	fields map[string]json.RawMessage
	canon  []byte
	hash   string
}

func (r hubRow) int(name string) (*int64, error) {
	v, ok := r.fields[name]
	if !ok || string(v) == "null" {
		return nil, nil
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("поле %s не целое: %s", name, v)
	}
	return &n, nil
}

func (r hubRow) str(name string) *string {
	v, ok := r.fields[name]
	if !ok || string(v) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return &s
	}
	var n json.Number // a numeric ref is read as Python's str() of it
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	if dec.Decode(&n) == nil {
		x := n.String()
		return &x
	}
	x := string(v)
	return &x
}

func (r hubRow) time(name string) (*time.Time, error) {
	s := r.str(name)
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := parseHubTime(*s)
	if err != nil {
		return nil, fmt.Errorf("поле %s не время: %q", name, *s)
	}
	return &t, nil
}

// keep reduces an item to the dataset's kept fields (missing = null) and hashes them.
func keep(ds string, it item, atField string) (hubRow, error) {
	r := hubRow{key: it.Key, fields: map[string]json.RawMessage{}}
	for _, f := range keptFields[ds] {
		v, ok := it.Fields[f]
		if !ok {
			v = json.RawMessage("null")
		}
		var x any
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		if err := dec.Decode(&x); err != nil {
			return r, fmt.Errorf("строка %s: поле %s не JSON", it.Key, f)
		}
		b, _ := json.Marshal(x)
		r.fields[f] = b
	}
	r.canon, _ = json.Marshal(r.fields) // keys sorted
	sum := sha256.Sum256(r.canon)
	r.hash = hex.EncodeToString(sum[:])[:12]
	t, err := r.time(atField)
	if err != nil || t == nil {
		return r, fmt.Errorf("строка %s без %s (%v)", it.Key, atField, err)
	}
	r.at = *t
	return r, nil
}

func keepAll(what, ds string, items []item, atField string) ([]hubRow, error) {
	out := make([]hubRow, 0, len(items))
	for _, it := range items {
		r, err := keep(ds, it, atField)
		if err != nil {
			return nil, refusef("хаб %s: %v — ответ не по контракту", what, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (r hubRow) event(page, ds string) ledger.Event {
	return ledger.Event{Source: "hub", SourceID: page + "/" + ds + "/" + r.key + "/" + r.hash, Kind: ds,
		OccurredAt: ts.FormatMilli(r.at),
		Payload:    map[string]any{"page": page, "dataset": ds, "key": r.key, "fields": json.RawMessage(r.canon)}}
}

// ---- mirrors ----

type mirrorRow struct {
	at    time.Time
	net   *int64
	hash  string
	first string
}

var mirrorTable = map[string]string{
	"transactions": "hub_transactions", "payouts": "hub_payouts", "ofapi_payout_requests": "hub_of_payouts",
}

func loadMirror(ctx context.Context, q ledger.Querier, ds, page string) (map[string]mirrorRow, error) {
	col, amount := "occurred_at", "net_mills"
	if ds != "transactions" {
		col, amount = "requested_at", "amount_mills"
	}
	rows, err := q.QueryContext(ctx, `SELECT key, `+col+`, `+amount+`, hash, first_seen_at FROM `+mirrorTable[ds]+
		` WHERE page_label = ?`, page)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]mirrorRow{}
	for rows.Next() {
		var k, at string
		var net sql.NullInt64
		var m mirrorRow
		if err := rows.Scan(&k, &at, &net, &m.hash, &m.first); err != nil {
			return nil, err
		}
		if m.at, err = ts.Parse(at); err != nil {
			return nil, fmt.Errorf("%s %s/%s: %w", mirrorTable[ds], page, k, err)
		}
		if net.Valid {
			v := net.Int64
			m.net = &v
		}
		out[k] = m
	}
	return out, rows.Err()
}

// saveMirror writes the new and changed versions (each also an event), deletes the rows that left
// the hub and returns (new events, removed rows).
func saveMirror(ctx context.Context, tx *sql.Tx, page, ds string, mirror map[string]mirrorRow, upd []hubRow,
	drop []string, now time.Time) (int, int, error) {
	events := 0
	at := ts.Format(now)
	for _, r := range upd {
		if old, ok := mirror[r.key]; ok && old.hash == r.hash {
			continue
		}
		id, ins, err := ledger.AddEvent(ctx, tx, r.event(page, ds), now)
		if err != nil {
			return 0, 0, err
		}
		if ins {
			events++
		}
		first := at
		if old, ok := mirror[r.key]; ok {
			first = old.first
		}
		if err := upsertRow(ctx, tx, page, ds, r, id, first, at); err != nil {
			return 0, 0, err
		}
	}
	for _, k := range drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+mirrorTable[ds]+` WHERE page_label = ? AND key = ?`, page, k); err != nil {
			return 0, 0, err
		}
	}
	return events, len(drop), nil
}

func upsertRow(ctx context.Context, tx *sql.Tx, page, ds string, r hubRow, eventID int64, first, at string) error {
	ms := ts.FormatMilli(r.at)
	var err error
	switch ds {
	case "transactions":
		net, _ := r.int("netMills")
		_, err = tx.ExecContext(ctx, `INSERT INTO hub_transactions(page_label, key, occurred_at, net_mills, currency, type, ref,
			fields, hash, event_id, first_seen_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(page_label, key) DO UPDATE SET occurred_at = excluded.occurred_at, net_mills = excluded.net_mills,
			currency = excluded.currency, type = excluded.type, ref = excluded.ref, fields = excluded.fields,
			hash = excluded.hash, event_id = excluded.event_id, updated_at = excluded.updated_at`,
			page, r.key, ms, nullInt(net), nullStr(r.str("currency")), nullStr(r.str("transactionType")),
			nullStr(r.str("transactionRef")), string(r.canon), r.hash, eventID, first, at)
	case "payouts":
		amount, _ := r.int("amountMills")
		var upd any
		if t, _ := r.time("updatedAtPlatform"); t != nil {
			upd = ts.FormatMilli(*t)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO hub_payouts(page_label, key, requested_at, amount_mills, status, method,
			updated_at_platform, payout_ref, fields, hash, event_id, first_seen_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(page_label, key) DO UPDATE SET requested_at = excluded.requested_at,
			amount_mills = excluded.amount_mills, status = excluded.status, method = excluded.method,
			updated_at_platform = excluded.updated_at_platform, payout_ref = excluded.payout_ref, fields = excluded.fields,
			hash = excluded.hash, event_id = excluded.event_id, updated_at = excluded.updated_at`,
			page, r.key, ms, nullInt(amount), nullStr(r.str("statusLabel")), nullStr(r.str("methodProviderLabel")), upd,
			nullStr(r.str("payoutRef")), string(r.canon), r.hash, eventID, first, at)
	case "ofapi_payout_requests":
		amount, _ := r.int("amountMills")
		_, err = tx.ExecContext(ctx, `INSERT INTO hub_of_payouts(page_label, key, requested_at, amount_mills, currency, state,
			payout_ref, fields, hash, event_id, first_seen_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(page_label, key) DO UPDATE SET requested_at = excluded.requested_at,
			amount_mills = excluded.amount_mills, currency = excluded.currency, state = excluded.state,
			payout_ref = excluded.payout_ref, fields = excluded.fields, hash = excluded.hash, event_id = excluded.event_id,
			updated_at = excluded.updated_at`,
			page, r.key, ms, nullInt(amount), nullStr(r.str("currency")), nullStr(r.str("state")), nullStr(r.str("payoutRef")),
			string(r.canon), r.hash, eventID, first, at)
	}
	return err
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullStr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func netOf(m mirrorRow) int64 {
	if m.net == nil {
		return 0
	}
	return *m.net
}

func hubNet(r hubRow) int64 {
	v, _ := r.int("netMills")
	if v == nil {
		return 0
	}
	return *v
}

// ---- transactions ----

// transactions keeps the page's transactions over [hub_window_start (or the page's start), now)
// equal to the hub (month.py Hub.items): with a mirror younger than hub_full_every_days that covers
// the window, only the tail of hub_tail_days before its newest row is fetched again; the window's
// summary must then match the mirror in count and net. One retry, then a full fetch (one retry),
// then a refusal.
func (s *Syncer) transactions(ctx context.Context, rc runCtx, page string) (Result, time.Time, time.Time, error) {
	a, b := rc.start, rc.now.Truncate(time.Second)
	if st, ok := rc.ex.Starts[page]; ok && st.After(a) {
		a = st
	}
	res := Result{}
	if !a.Before(b) {
		return res, a, b, s.write(ctx, func(tx *sql.Tx) error {
			return okStatus(ctx, tx, statusRow{page: page, ds: "transactions", from: a, to: b, now: rc.now})
		})
	}
	st, err := loadStatus(ctx, s.Store.RW, page, "transactions")
	if err != nil {
		return res, a, b, err
	}
	mirror, err := loadMirror(ctx, s.Store.RW, "transactions", page)
	if err != nil {
		return res, a, b, err
	}
	what := fmt.Sprintf("%s transactions %s..%s", page, iso(a), iso(b))
	spec := func(from time.Time) listSpec {
		return listSpec{page: page, dataset: "transactions", from: from, to: b, claim: "netMills", sort: "occurredAt",
			filters: notPayoutReversal}
	}
	var upd []hubRow
	var drop []string
	var n, net int64
	var floor *time.Time
	blockers := map[string]bool{}
	lo, full := a, rc.now
	done := false
	if st != nil && st.mirrorFrom != nil && st.fullAt != nil && !a.Before(*st.mirrorFrom) && rc.now.Sub(*st.fullAt) < rc.fullEvery {
		latest := a
		found := false
		for _, m := range mirror {
			if m.at.Before(b) && (!found || m.at.After(latest)) {
				latest, found = m.at, true
			}
		}
		t := latest.Add(-rc.tail)
		if t.Before(a) {
			t = a
		}
		if t.After(b) {
			t = b
		}
		for attempt := 0; attempt < 2 && !done; attempt++ {
			var tail []hubRow
			if t.Before(b) {
				lr, err := s.Client.listing(ctx, spec(t))
				if err != nil {
					return res, a, b, err
				}
				if tail, err = keepAll(what, "transactions", lr.items, "occurredAt"); err != nil {
					return res, a, b, err
				}
				for k := range lr.blockers {
					blockers[k] = true
				}
			}
			inTail := map[string]bool{}
			var count, sum int64
			for _, r := range tail {
				inTail[r.key] = true
				count, sum = count+1, sum+hubNet(r)
			}
			for k, m := range mirror {
				if !inTail[k] && !m.at.Before(a) && m.at.Before(t) {
					count, sum = count+1, sum+netOf(m)
				}
			}
			if n, net, floor, err = s.Client.summary(ctx, page, a, b); err != nil {
				return res, a, b, err
			}
			if n == count && net == sum {
				upd, done, res.Mode, lo, full = tail, true, "tail", *st.mirrorFrom, *st.fullAt
				for k, m := range mirror {
					if !inTail[k] && !m.at.Before(t) && m.at.Before(b) {
						drop = append(drop, k)
					}
				}
			}
		}
	}
	var items []hubRow
	for attempt := 0; attempt < 2 && !done; attempt++ {
		lr, err := s.Client.listing(ctx, spec(a))
		if err != nil {
			return res, a, b, err
		}
		if items, err = keepAll(what, "transactions", lr.items, "occurredAt"); err != nil {
			return res, a, b, err
		}
		for k := range lr.blockers {
			blockers[k] = true
		}
		if n, net, floor, err = s.Client.summary(ctx, page, a, b); err != nil {
			return res, a, b, err
		}
		var sum int64
		in := map[string]bool{}
		for _, r := range items {
			in[r.key], sum = true, sum+hubNet(r)
		}
		if n == int64(len(items)) && net == sum {
			upd, done, res.Mode, full = items, true, "full", rc.now
			if st != nil && st.mirrorFrom != nil && st.mirrorFrom.Before(a) {
				lo = *st.mirrorFrom
			}
			for k, m := range mirror {
				if !in[k] && !m.at.Before(a) && m.at.Before(b) {
					drop = append(drop, k)
				}
			}
		}
	}
	if !done {
		return res, a, b, refusef("хаб %s: список строк (%d) не сходится с --summary (%d, %.2f)", page, len(items), n,
			float64(net)/1000)
	}
	sort.Strings(drop)
	err = s.write(ctx, func(tx *sql.Tx) error {
		ev, rm, err := saveMirror(ctx, tx, page, "transactions", mirror, upd, drop, rc.now)
		if err != nil {
			return err
		}
		res.Events, res.Removed, res.Rows = ev, rm, int(n)
		return okStatus(ctx, tx, statusRow{page: page, ds: "transactions", from: a, to: b, rows: n, net: net, floor: floor,
			mirrorFrom: &lo, fullAt: &full, blockers: blockers, requests: s.Client.Calls - s.stepCalls, now: rc.now})
	})
	return res, a, b, err
}

// ---- payout requests ----

// payouts mirrors the page's payout requests over [hub_window_start, now): Fansly's `payouts` must be
// proven complete (one page, exact count, nothing capped), a page whose request journal begins with
// its first request (hub-exceptions floor) tolerates exactly that before_capture_floor gap.
func (s *Syncer) payouts(ctx context.Context, rc runCtx, page, ds string) (Result, time.Time, time.Time, error) {
	a, b := rc.start, rc.now.Truncate(time.Second)
	res := Result{}
	spec := listSpec{page: page, dataset: ds, from: a, to: b, claim: "ofapiPayoutRequest", sort: "requestedAt"}
	if ds == "payouts" {
		spec.claim, spec.floor, spec.complete = "payoutRequest", rc.ex.Floors[page], true
	}
	mirror, err := loadMirror(ctx, s.Store.RW, ds, page)
	if err != nil {
		return res, a, b, err
	}
	lr, err := s.Client.listing(ctx, spec)
	if err != nil {
		return res, a, b, err
	}
	what := fmt.Sprintf("%s %s %s..%s", page, ds, iso(a), iso(b))
	rows, err := keepAll(what, ds, lr.items, "requestedAt")
	if err != nil {
		return res, a, b, err
	}
	in := map[string]bool{}
	var sum int64
	for _, r := range rows {
		in[r.key] = true
		if v, _ := r.int("amountMills"); v != nil {
			sum += *v
		}
	}
	var drop []string
	for k, m := range mirror {
		if !in[k] && !m.at.Before(a) && m.at.Before(b) {
			drop = append(drop, k)
		}
	}
	sort.Strings(drop)
	lo, full := a, rc.now
	err = s.write(ctx, func(tx *sql.Tx) error {
		ev, rm, err := saveMirror(ctx, tx, page, ds, mirror, rows, drop, rc.now)
		if err != nil {
			return err
		}
		res.Events, res.Removed, res.Rows, res.Mode = ev, rm, len(rows), "full"
		return okStatus(ctx, tx, statusRow{page: page, ds: ds, from: a, to: b, rows: int64(len(rows)), net: sum,
			floor: lr.floor, mirrorFrom: &lo, fullAt: &full, blockers: lr.blockers, requests: s.Client.Calls - s.stepCalls, now: rc.now})
	})
	return res, a, b, err
}

// ---- balances ----

var balanceSpec = map[string]struct{ claim, sort, at, value, kind string }{
	"page_profile_daily": {"pageProfileDay", "day", "capturedAt", "earningsBalanceMills", "hub_page_balance"},
	"pending_balance":    {"pendingBalance", "lastSeenAt", "lastSeenAt", "pendingMills", "hub_pending"},
}

// balances turns the newest rows of a Fansly balance dataset into observations of the page's
// balance (micro-dollars) at the moment the hub saw it. These are point facts, so a hole before the
// plane's floor (the settings journal keeps only its latest versions) does not refuse them.
func (s *Syncer) balances(ctx context.Context, rc runCtx, page, ds string) (Result, time.Time, time.Time, error) {
	b := rc.now.Truncate(time.Second)
	a := b.Add(-balanceLookback)
	spec := balanceSpec[ds]
	res := Result{}
	lr, err := s.Client.listing(ctx, listSpec{page: page, dataset: ds, from: a, to: b, claim: spec.claim, sort: spec.sort,
		point: true})
	if err != nil {
		return res, a, b, err
	}
	what := fmt.Sprintf("%s %s %s..%s", page, ds, iso(a), iso(b))
	var rows []hubRow
	for _, it := range lr.items {
		if v, ok := it.Fields[spec.value]; !ok || string(v) == "null" {
			continue // the day was not captured: no observation, not a zero
		}
		r, err := keep(ds, it, spec.at)
		if err != nil {
			return res, a, b, refusef("хаб %s: %v — ответ не по контракту", what, err)
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	var last int64
	err = s.write(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			v, err := r.int(spec.value)
			if err != nil {
				return refusef("хаб %s: %v", what, err)
			}
			id, ins, err := ledger.AddEvent(ctx, tx, r.event(page, ds), rc.now)
			if err != nil {
				return err
			}
			if ins {
				res.Events++
			}
			last = *v
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO observations(kind, subject, value, observed_at, source,
				event_id, payload) VALUES (?, ?, ?, ?, 'hub', ?, ?)`, spec.kind, page, *v*1000, ts.Format(r.at), id,
				string(r.canon)); err != nil {
				return err
			}
		}
		res.Rows, res.Mode = len(rows), "point"
		return okStatus(ctx, tx, statusRow{page: page, ds: ds, from: a, to: b, rows: int64(len(rows)), net: last,
			floor: lr.floor, blockers: lr.blockers, requests: s.Client.Calls - s.stepCalls, now: rc.now})
	})
	if err != nil && IsRefusal(err) {
		res.Events = 0
	}
	return res, a, b, err
}

// ---- statuses ----

type statusRow struct {
	page, ds   string
	from, to   time.Time
	rows, net  int64
	floor      *time.Time
	mirrorFrom *time.Time
	fullAt     *time.Time
	blockers   map[string]bool
	requests   int
	now        time.Time
}

func optTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts.FormatMilli(*t)
}

func okStatus(ctx context.Context, tx *sql.Tx, s statusRow) error {
	bl, _ := json.Marshal(sortedKeys(s.blockers))
	at := ts.Format(s.now)
	_, err := tx.ExecContext(ctx, `INSERT INTO hub_sync(page_label, dataset, status, reasons, blockers, window_from, window_to,
		rows, net_mills, capture_floor, mirror_from, full_at, requests, checked_at, ok_at)
		VALUES (?, ?, 'ok', '[]', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(page_label, dataset) DO UPDATE SET status = 'ok', reasons = '[]', blockers = excluded.blockers,
		window_from = excluded.window_from, window_to = excluded.window_to, rows = excluded.rows,
		net_mills = excluded.net_mills, capture_floor = excluded.capture_floor, mirror_from = excluded.mirror_from,
		full_at = excluded.full_at, requests = excluded.requests, checked_at = excluded.checked_at, ok_at = excluded.ok_at`,
		s.page, s.ds, string(bl), ts.Format(s.from), ts.Format(s.to), s.rows, s.net, optTime(s.floor), optTime(s.mirrorFrom),
		optTime(s.fullAt), s.requests, at, at)
	return err
}

// failStatus marks the dataset unknown. What the mirror proved before stays, but reads refuse it
// until a run proves the data again.
func failStatus(ctx context.Context, tx *sql.Tx, page, ds string, reasons []string, from, to *time.Time, requests int,
	now time.Time) error {
	rs, _ := json.Marshal(reasons)
	var f, t any
	if from != nil && !from.IsZero() {
		f = ts.Format(*from)
	}
	if to != nil && !to.IsZero() {
		t = ts.Format(*to)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO hub_sync(page_label, dataset, status, reasons, window_from, window_to, requests,
		checked_at) VALUES (?, ?, 'unknown', ?, ?, ?, ?, ?)
		ON CONFLICT(page_label, dataset) DO UPDATE SET status = 'unknown', reasons = excluded.reasons,
		window_from = excluded.window_from, window_to = excluded.window_to, requests = excluded.requests,
		checked_at = excluded.checked_at`, page, ds, string(rs), f, t, requests, ts.Format(now))
	return err
}

// saveGrant replaces the grant; datasets of a page that left it become unknown.
func saveGrant(ctx context.Context, tx *sql.Tx, grant []GrantPage, now time.Time, requests int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_pages`); err != nil {
		return err
	}
	in := map[string]bool{"*": true}
	for _, p := range grant {
		in[p.PageLabel] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_pages(page_label, platform, model_slug, model_name, seen_at)
			VALUES (?, ?, ?, ?, ?)`, p.PageLabel, p.Platform, p.ModelSlug, p.ModelName, ts.Format(now)); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT page_label FROM hub_sync`)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		if !in[p] {
			gone = append(gone, p)
		}
	}
	rows.Close()
	for _, p := range gone {
		rs, _ := json.Marshal([]string{"страницы " + p + " нет в гранте ключа хаба"})
		if _, err := tx.ExecContext(ctx, `UPDATE hub_sync SET status = 'unknown', reasons = ?, checked_at = ? WHERE page_label = ?`,
			string(rs), ts.Format(now), p); err != nil {
			return err
		}
	}
	return okStatus(ctx, tx, statusRow{page: "*", ds: "capabilities", from: now, to: now, rows: int64(len(grant)),
		requests: requests, now: now})
}

// SyncStatus is one row of hub_sync.
type SyncStatus struct {
	Page         string     `json:"page"`
	Dataset      string     `json:"dataset"`
	Status       string     `json:"status"`
	Reasons      []string   `json:"reasons,omitempty"`
	Blockers     []string   `json:"blockers,omitempty"`
	From         *time.Time `json:"-"`
	To           *time.Time `json:"-"`
	WindowFrom   string     `json:"windowFrom,omitempty"`
	WindowTo     string     `json:"windowTo,omitempty"`
	Rows         *int64     `json:"rows,omitempty"`
	NetMills     *int64     `json:"netMills,omitempty"`
	CaptureFloor string     `json:"captureFloor,omitempty"`
	FullAt       string     `json:"fullAt,omitempty"`
	CheckedAt    string     `json:"checkedAt"`
	OkAt         string     `json:"okAt,omitempty"`
	mirrorFrom   *time.Time
	fullAt       *time.Time
	floor        *time.Time
}

const statusColumns = `page_label, dataset, status, reasons, blockers, COALESCE(window_from, ''), COALESCE(window_to, ''),
	rows, net_mills, COALESCE(capture_floor, ''), COALESCE(mirror_from, ''), COALESCE(full_at, ''), checked_at,
	COALESCE(ok_at, '')`

func scanStatus(sc interface{ Scan(...any) error }) (SyncStatus, error) {
	var s SyncStatus
	var reasons, blockers, mirrorFrom string
	var rows, net sql.NullInt64
	if err := sc.Scan(&s.Page, &s.Dataset, &s.Status, &reasons, &blockers, &s.WindowFrom, &s.WindowTo, &rows, &net,
		&s.CaptureFloor, &mirrorFrom, &s.FullAt, &s.CheckedAt, &s.OkAt); err != nil {
		return s, err
	}
	_ = json.Unmarshal([]byte(reasons), &s.Reasons)
	_ = json.Unmarshal([]byte(blockers), &s.Blockers)
	if rows.Valid {
		s.Rows = &rows.Int64
	}
	if net.Valid {
		s.NetMills = &net.Int64
	}
	parse := func(v string) *time.Time {
		if v == "" {
			return nil
		}
		t, err := ts.Parse(v)
		if err != nil {
			return nil
		}
		return &t
	}
	s.From, s.To, s.mirrorFrom, s.fullAt, s.floor = parse(s.WindowFrom), parse(s.WindowTo), parse(mirrorFrom), parse(s.FullAt),
		parse(s.CaptureFloor)
	return s, nil
}

func loadStatus(ctx context.Context, q ledger.Querier, page, ds string) (*SyncStatus, error) {
	s, err := scanStatus(q.QueryRowContext(ctx, `SELECT `+statusColumns+` FROM hub_sync WHERE page_label = ? AND dataset = ?`,
		page, ds))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Statuses lists every (page, dataset) the hub was asked about.
func Statuses(ctx context.Context, q ledger.Querier) ([]SyncStatus, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+statusColumns+` FROM hub_sync ORDER BY page_label, dataset`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncStatus
	for rows.Next() {
		s, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
