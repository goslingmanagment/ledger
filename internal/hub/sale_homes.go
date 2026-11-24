package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const SaleHomesPath = "/api/v1/agent/sale-homes/feed"
const homesCursor = "hub:sale_homes"

var homesPosition = regexp.MustCompile(`^[0-9]{1,18}$`)

// SaleHome is Hub's computed home at a feed position. A retract has only its identity and position.
type SaleHome struct {
	Seq           int64           `json:"seq"`
	At            time.Time       `json:"at"`
	Op            string          `json:"op"`
	TransactionID int64           `json:"transactionId"`
	PageLabel     string          `json:"pageLabel"`
	OccurredAt    time.Time       `json:"occurredAt"`
	NetMills      *int64          `json:"netMills"`
	SaleKind      string          `json:"saleKind"`
	Home          string          `json:"home"`
	Reason        string          `json:"reason"`
	Pot           string          `json:"pot"`
	WaitsUntil    *time.Time      `json:"waitsUntil"`
	RuleVersion   string          `json:"ruleVersion"`
	Shares        []SaleHomeShare `json:"shares"`
}

type SaleHomeShare struct {
	PersonCode string `json:"personCode"`
	UserID     int64  `json:"userId"`
	ShiftID    *int64 `json:"shiftId"`
	WorkRef    string `json:"workRef"`
	ReportDate string `json:"reportDate"`
	DaySource  string `json:"daySource"`
	Percent    *int   `json:"percent"`
	Mills      *int64 `json:"mills"`
}

// DecodeSaleHome checks the fields the ledger needs before acknowledging or pricing a home.
func DecodeSaleHome(raw []byte) (SaleHome, error) {
	var h SaleHome
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, err
	}
	if h.Seq <= 0 || h.TransactionID <= 0 || h.At.IsZero() || h.Shares == nil {
		return h, fmt.Errorf("неверное изменение дома продажи %d", h.TransactionID)
	}
	switch h.Op {
	case "retract":
		if len(h.Shares) != 0 || h.NetMills != nil || !h.OccurredAt.IsZero() || h.Home != "" || h.Reason != "" || h.Pot != "" || h.WaitsUntil != nil || h.RuleVersion != "" || h.SaleKind != "" {
			return h, fmt.Errorf("неверный отзыв дома продажи %d", h.TransactionID)
		}
		return h, nil
	case "upsert":
		if h.NetMills == nil || h.OccurredAt.IsZero() || h.Reason == "" || h.RuleVersion == "" {
			return h, fmt.Errorf("неполный дом продажи %d", h.TransactionID)
		}
	default:
		return h, fmt.Errorf("неизвестная операция дома %q", h.Op)
	}
	switch h.Home {
	case "shift", "visit", "pot", "waiting", "decide", "nobody", "before_rule", "unknown":
	default:
		return h, fmt.Errorf("неизвестный дом продажи %d: %q", h.TransactionID, h.Home)
	}
	for _, sh := range h.Shares {
		if sh.UserID <= 0 || sh.Percent == nil || *sh.Percent < 0 || *sh.Percent > 100 || sh.Mills == nil || sh.DaySource == "" {
			return h, fmt.Errorf("неверная доля продажи %d", h.TransactionID)
		}
		if sh.ReportDate != "" {
			if _, err := time.Parse(ts.DateLayout, sh.ReportDate); err != nil {
				return h, fmt.Errorf("дата доли продажи %d: %w", h.TransactionID, err)
			}
		}
	}
	return h, nil
}

type homesPage struct {
	Homes []json.RawMessage `json:"homes"`
	Held  []struct {
		Seq           int64
		TransactionID int64
		Reason        string
	} `json:"held"`
	Next      string    `json:"next"`
	More      *bool     `json:"more"`
	ServerNow time.Time `json:"serverNow"`
}

func (c *Client) saleHomes(ctx context.Context, since string) (homesPage, error) {
	var p homesPage
	q := url.Values{"limit": {"500"}}
	if since != "" {
		if !homesPosition.MatchString(since) {
			return p, fmt.Errorf("неверный курсор домов %q", since)
		}
		q.Set("since", since)
	}
	raw, err := c.do(ctx, "sale-homes", false, http.MethodGet, SaleHomesPath+"?"+q.Encode(), nil)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Homes == nil || p.Held == nil || p.More == nil || p.ServerNow.IsZero() || !homesPosition.MatchString(p.Next) {
		return p, refusef("хаб: страница домов не по контракту")
	}
	next, _ := strconv.ParseInt(p.Next, 10, 64)
	prev, _ := strconv.ParseInt(since, 10, 64)
	if next < prev || (*p.More && next == prev) {
		return p, refusef("хаб: курсор домов %s после %s", p.Next, since)
	}
	return p, nil
}

type HomesStatus struct {
	Cursor     string `json:"cursor"`
	LastOK     string `json:"lastOk,omitempty"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	Held       int    `json:"held"`
	generation int64
}

func ReadHomesStatus(ctx context.Context, q ledger.Querier) (HomesStatus, error) {
	var st HomesStatus
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM cursors WHERE name=?), ''),
		COALESCE(last_ok,''), state, error, generation, (SELECT count(*) FROM hub_sale_homes_held)
		FROM hub_sale_homes_sync WHERE singleton=1`, homesCursor).Scan(&st.Cursor, &st.LastOK, &st.State, &st.Error, &st.generation, &st.Held)
	return st, err
}

// Problem is shared by the facts and health. Only health excuses an initial 404.
func (s HomesStatus) Problem(now time.Time) string {
	switch {
	case s.Held > 0:
		return fmt.Sprintf("хаб придержал изменений домов продаж: %d", s.Held)
	case s.LastOK == "":
		return "лента домов хаба ещё не читалась до конца"
	case s.State == "reading":
		return "лента домов хаба прочитана не до конца"
	}
	last, err := ts.Parse(s.LastOK)
	if err != nil || now.Sub(last) > time.Hour {
		return "лента домов хаба не читалась больше часа: " + s.LastOK
	}
	return ""
}

func init() {
	server.Every("hub-homes", 5*time.Minute, func(ctx context.Context, s *server.Server) error {
		cfg, err := ConfigFromEnv()
		if err != nil {
			return err
		}
		_, err = SyncSaleHomes(ctx, s.Store, NewClient(cfg, nil), time.Now())
		return err
	})
}

// SyncSaleHomes publishes each page, its held snapshot and its cursor together. Only the final page
// proves freshness. AfterTeam also runs after a failed read so the facts can age out.
func SyncSaleHomes(ctx context.Context, store *db.Store, c *Client, now time.Time) (HomesStatus, error) {
	st, err := syncHomes(ctx, store, c, now)
	if AfterTeam != nil {
		err = errors.Join(err, AfterTeam(ctx, store, now))
	}
	return st, err
}

func syncHomes(ctx context.Context, store *db.Store, c *Client, now time.Time) (HomesStatus, error) {
	for {
		st, err := ReadHomesStatus(ctx, store.RO)
		if err != nil {
			return st, err
		}
		page, readErr := c.saleHomes(ctx, st.Cursor)
		state, message := "ok", ""
		switch {
		case IsAbsent(readErr) && st.LastOK == "":
			state, readErr = "absent", nil
		case readErr != nil:
			state, message = "error", readErr.Error()
			if st.State == "reading" {
				state = "reading"
			}
		case *page.More:
			state = "reading"
		}
		err = writeTx(ctx, store, func(tx *sql.Tx) error {
			// A slower response must never replace a newer held set, including two idle reads at one cursor.
			r, err := tx.ExecContext(ctx, `UPDATE hub_sale_homes_sync SET generation=generation+1, state=?, error=?,
				last_ok=CASE WHEN ?='ok' THEN ? ELSE last_ok END WHERE singleton=1 AND generation=?`,
				state, message, state, ts.Format(now), st.generation)
			if err != nil {
				return err
			}
			if n, err := r.RowsAffected(); err != nil || n != 1 {
				return errors.New("дома продаж уже прочитаны другим процессом")
			}
			if readErr != nil || state == "absent" {
				return nil
			}
			return saveHomesPage(ctx, tx, page, now)
		})
		if err != nil {
			return st, err
		}
		if state != "reading" || readErr != nil {
			st, err = ReadHomesStatus(ctx, store.RO)
			return st, errors.Join(readErr, err)
		}
	}
}

func saveHomesPage(ctx context.Context, tx *sql.Tx, p homesPage, now time.Time) error {
	next, _ := strconv.ParseInt(p.Next, 10, 64)
	for _, raw := range p.Homes {
		h, err := DecodeSaleHome(raw)
		if err != nil {
			return err
		}
		if h.Seq > next {
			return fmt.Errorf("место дома %d после курсора %d", h.Seq, next)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_sale_homes(transaction_id, seq, retracted, payload) VALUES (?,?,?,?)
			ON CONFLICT(transaction_id) DO UPDATE SET seq=excluded.seq, retracted=excluded.retracted, payload=excluded.payload
			WHERE excluded.seq > hub_sale_homes.seq`, h.TransactionID, h.Seq, h.Op == "retract", string(raw)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hub_sale_homes_held`); err != nil {
		return err
	}
	for _, h := range p.Held {
		if h.Seq <= 0 || h.TransactionID <= 0 || h.Reason != "page_outside_key" {
			return fmt.Errorf("неверное придержанное изменение дома продажи %d", h.TransactionID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hub_sale_homes_held(seq,transaction_id,reason) VALUES (?,?,?)`, h.Seq, h.TransactionID, h.Reason); err != nil {
			return err
		}
	}
	// A crash between this commit and the rebuild must not leave yesterday's derived rows payable.
	if len(p.Homes) != 0 || len(p.Held) != 0 || *p.More {
		if _, err := tx.ExecContext(ctx, `UPDATE fact_weeks SET status='unknown', reasons='["ожидается пересчёт по ленте домов"]' WHERE status='ok'`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO cursors(name,value,updated_at) VALUES (?,?,?)
		ON CONFLICT(name) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, homesCursor, p.Next, ts.Format(now))
	return err
}
