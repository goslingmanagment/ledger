package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const PotDistributionsPath = "/api/v1/agent/pot-distributions/feed"
const potsCursor = "hub:pot_distributions"

type potChange struct {
	Seq          int64
	Change       string
	At           time.Time
	Distribution struct {
		ID      int64
		Pot     struct{ Code, Name, ModelSlug string }
		Day     string
		Formula struct {
			Kind   string
			Params json.RawMessage
		}
		BaseMills *int64
		Inflows   []struct {
			TransactionID int64
			PageLabel     string
			BaseMills     *int64
		}
		Lines []struct {
			PersonCode string `json:"personCode"`
			BaseMills  *int64 `json:"baseMills"`
			Basis      string `json:"basis,omitempty"`
		}
	}
}
type potFeedPage struct {
	Changes []potChange
	Held    []struct {
		Seq, DistributionID int64
		PageLabels          []string
		Reason              string
	}
	Next      string
	More      *bool
	ServerNow time.Time
}

func validPotMills(v *int64) bool { return v != nil && *v >= 0 && *v <= math.MaxInt64/1000 }

func (p potFeedPage) validate(since string) error {
	next, _ := strconv.ParseInt(p.Next, 10, 64)
	prev, _ := strconv.ParseInt(since, 10, 64)
	if p.Changes == nil || p.Held == nil || p.More == nil || p.ServerNow.IsZero() || !homesPosition.MatchString(p.Next) || next < prev || (*p.More && next == prev) {
		return errors.New("неверная страница ленты раздач Workboard")
	}
	for _, c := range p.Changes {
		d := c.Distribution
		_, dateErr := time.Parse(ts.DateLayout, d.Day)
		if c.Seq <= 0 || c.Seq > next || c.At.IsZero() || (c.Change != "posted" && c.Change != "reversed") {
			return fmt.Errorf("неверное изменение раздачи Workboard %d", d.ID)
		}
		if d.ID <= 0 || dateErr != nil || d.Pot.Code == "" || d.Pot.Name == "" || d.Pot.ModelSlug == "" ||
			!validPotMills(d.BaseMills) || len(d.Lines) == 0 || len(d.Inflows) == 0 {
			return fmt.Errorf("неполная раздача Workboard %d", d.ID)
		}
		if d.Formula.Kind == "" ||
			len(d.Formula.Params) == 0 || d.Formula.Params[0] != '{' {
			return fmt.Errorf("неверная формула раздачи %d", d.ID)
		}
		lineLeft, inflowLeft := *d.BaseMills, *d.BaseMills
		seen := map[string]bool{}
		for _, l := range d.Lines {
			if l.PersonCode == "" || seen[l.PersonCode] || !validPotMills(l.BaseMills) || *l.BaseMills > lineLeft {
				return fmt.Errorf("неверная доля раздачи %d", d.ID)
			}
			lineLeft -= *l.BaseMills
			seen[l.PersonCode] = true
		}
		for _, i := range d.Inflows {
			if i.TransactionID <= 0 || i.PageLabel == "" || !validPotMills(i.BaseMills) || *i.BaseMills > inflowLeft {
				return fmt.Errorf("неверное поступление раздачи %d", d.ID)
			}
			inflowLeft -= *i.BaseMills
		}
		if lineLeft != 0 || inflowLeft != 0 {
			return fmt.Errorf("суммы долей и поступлений не сходятся с базой раздачи %d", d.ID)
		}
	}
	for _, h := range p.Held {
		if h.Seq <= 0 || h.DistributionID <= 0 || len(h.PageLabels) == 0 || h.Reason != "page_outside_key" {
			return errors.New("неверное удержание раздачи Workboard")
		}
	}
	return nil
}

func (c *Client) potPage(ctx context.Context, since string) (potFeedPage, error) {
	var page potFeedPage
	q := url.Values{"limit": {"500"}}
	if since != "" {
		if !homesPosition.MatchString(since) {
			return page, fmt.Errorf("неверный курсор раздач %q", since)
		}
		q.Set("since", since)
	}
	raw, err := c.do(ctx, "pot-distributions", false, http.MethodGet, PotDistributionsPath+"?"+q.Encode(), nil)
	if err != nil {
		return page, err
	}
	if err = json.Unmarshal(raw, &page); err != nil {
		return page, err
	}
	return page, page.validate(since)
}

type PotStatus struct {
	From       string `json:"from"`
	Cursor     string `json:"cursor"`
	LastOK     string `json:"lastOk,omitempty"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	Dirty      bool   `json:"dirty"`
	Held       int    `json:"held"`
	generation int64
}

func ReadPotStatus(ctx context.Context, q ledger.Querier) (s PotStatus, err error) {
	err = q.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key='pot_shares_hub_from'),coalesce((SELECT value FROM cursors WHERE name=?),''),
 coalesce(last_ok,''),state,error,dirty,generation,(SELECT count(*) FROM hub_pot_distribution_held) FROM hub_pot_sync WHERE singleton=1`, potsCursor).Scan(&s.From, &s.Cursor, &s.LastOK, &s.State, &s.Error, &s.Dirty, &s.generation, &s.Held)
	return
}

func (s PotStatus) FeedProblem(now time.Time) string {
	switch {
	case s.Error != "":
		return "лента раздач Workboard: " + s.Error
	case s.State != "ok" || s.LastOK == "":
		return "нет полного успешного чтения раздач Workboard"
	}
	t, err := ts.Parse(s.LastOK)
	if err != nil || now.Sub(t) > time.Hour {
		return "лента раздач Workboard не читалась больше часа: " + s.LastOK
	}
	return ""
}

// AfterPots refreshes report accruals and the books after reaching the feed head.
var AfterPots func(context.Context, *db.Store, time.Time) error

func init() {
	server.Every("hub-pots", 5*time.Minute, func(ctx context.Context, s *server.Server) error {
		_, err := SyncConfiguredPots(ctx, s.Store, time.Now())
		return err
	})
}

func SyncConfiguredPots(ctx context.Context, store *db.Store, now time.Time) (PotStatus, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		_, saveErr := store.RW.ExecContext(ctx, `UPDATE hub_pot_sync SET state='error',error=?,generation=generation+1`, err.Error())
		return PotStatus{}, errors.Join(err, saveErr)
	}
	return SyncPotDistributions(ctx, store, NewClient(cfg, nil), now)
}

var errPotReader = errors.New("раздачи уже прочитаны другим процессом")

func SyncPotDistributions(ctx context.Context, store *db.Store, c *Client, now time.Time) (PotStatus, error) {
	for {
		st, err := ReadPotStatus(ctx, store.RO)
		if err != nil {
			return st, err
		}
		p, readErr := c.potPage(ctx, st.Cursor)
		state := "ok"
		switch {
		case IsAbsent(readErr) && st.From == "" && st.LastOK == "":
			state = "absent"
			readErr = nil
		case readErr != nil:
			state = "error"
		case *p.More:
			state = "reading"
		}
		message := ""
		if readErr != nil {
			message = readErr.Error()
		}
		err = writeTx(ctx, store, func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, `UPDATE hub_pot_sync SET generation=generation+1,state=?,error=?,last_ok=CASE WHEN ?='ok' THEN ? ELSE last_ok END
    WHERE singleton=1 AND generation=?`, state, message, state, ts.Format(now), st.generation)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return errPotReader
			}
			if state == "error" || state == "absent" {
				return nil
			}
			if err := savePotPage(ctx, tx, p, now); err != nil {
				return err
			}
			if st.From != "" && (len(p.Changes) > 0 || st.State != "ok" || st.Held > 0 || len(p.Held) > 0 || *p.More) {
				_, err = tx.ExecContext(ctx, `UPDATE hub_pot_sync SET dirty=1`)
			}
			return err
		})
		if err != nil {
			if !errors.Is(err, errPotReader) {
				// A scheduler timeout cancels the read context; still persist the failed attempt.
				recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, e := store.RW.ExecContext(recordCtx, `UPDATE hub_pot_sync SET generation=generation+1,state='error',error=? WHERE generation=?`, err.Error(), st.generation)
				cancel()
				err = errors.Join(err, e)
			}
			return st, err
		}
		if state == "reading" {
			continue
		}
		if state == "ok" && st.From != "" && AfterPots != nil {
			err = AfterPots(ctx, store, now)
			if err != nil {
				_, markErr := store.RW.ExecContext(ctx, `UPDATE hub_pot_sync SET dirty=1 WHERE generation=?`, st.generation+1)
				err = errors.Join(err, markErr)
			}
		}
		st, statusErr := ReadPotStatus(ctx, store.RO)
		return st, errors.Join(readErr, err, statusErr)
	}
}

func savePotPage(ctx context.Context, tx *sql.Tx, p potFeedPage, now time.Time) error {
	for _, c := range p.Changes {
		d := c.Distribution
		lines, _ := json.Marshal(d.Lines)
		r, err := tx.ExecContext(ctx, `INSERT INTO hub_pot_distributions(id,seq,state,pot_code,pot_name,model_slug,day,formula_kind,formula_params,base_mills,lines_json,at,updated_at)
  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET seq=excluded.seq,state=excluded.state,pot_code=excluded.pot_code,pot_name=excluded.pot_name,
  model_slug=excluded.model_slug,day=excluded.day,formula_kind=excluded.formula_kind,formula_params=excluded.formula_params,base_mills=excluded.base_mills,
  lines_json=excluded.lines_json,at=excluded.at,updated_at=excluded.updated_at WHERE excluded.seq>hub_pot_distributions.seq AND hub_pot_distributions.state<>'reversed'`,
			d.ID, c.Seq, c.Change, d.Pot.Code, d.Pot.Name, d.Pot.ModelSlug, d.Day, d.Formula.Kind, string(d.Formula.Params), *d.BaseMills, string(lines), ts.Format(c.At), ts.Format(now))
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM hub_pot_distribution_inflows WHERE distribution_id=?`, d.ID); err != nil {
			return err
		}
		for _, i := range d.Inflows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO hub_pot_distribution_inflows(distribution_id,transaction_id,page_label,base_mills) VALUES(?,?,?,?)`, d.ID, i.TransactionID, i.PageLabel, *i.BaseMills); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_pot_distribution_held`); err != nil {
		return err
	}
	for _, h := range p.Held {
		labels, _ := json.Marshal(h.PageLabels)
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_pot_distribution_held(seq,distribution_id,page_labels_json,reason) VALUES(?,?,?,?)`, h.Seq, h.DistributionID, string(labels), h.Reason); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO cursors(name,value,updated_at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, potsCursor, p.Next, ts.Format(now))
	return err
}
