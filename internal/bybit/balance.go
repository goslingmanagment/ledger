package bybit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

const (
	balanceObservation = "bybit_balance"
	balanceSubject     = "bybit"
	balanceSource      = "bybit_api"
)

var (
	stableCoins = ledger.StableCoins
	dustLimit   = mustRat("0.01")
)

// Balance is what the wallets hold: FUND + UNIFIED, coin by coin. A stable coin under a cent is dust; any
// other coin is kept at any size, a small quantity of BTC is dollars. AllCoins marks readings made so.
type Balance struct {
	Wallets     map[string]map[string]string `json:"wallets"`
	StableTotal string                       `json:"stable_total"`
	AllCoins    bool                         `json:"all_coins,omitempty"`
	Micros      money.Micros                 `json:"-"`
}

type walletCoin struct {
	Coin          string `json:"coin"`
	WalletBalance string `json:"walletBalance"`
}

// ReadBalance reads both wallets. A partial total would raise a false alarm in the cash check: any
// failed wallet is an error and no balance.
func ReadBalance(ctx context.Context, c *Client) (Balance, error) {
	b := Balance{Wallets: map[string]map[string]string{}}
	var total money.Micros
	add := func(wallet string, coins []walletCoin) error {
		for _, coin := range coins {
			v, ok := rat(coin.WalletBalance)
			if !ok || v.Sign() <= 0 || (stableCoins[coin.Coin] && v.Cmp(dustLimit) <= 0) {
				continue
			}
			if b.Wallets[wallet] == nil {
				b.Wallets[wallet] = map[string]string{}
			}
			b.Wallets[wallet][coin.Coin] = coin.WalletBalance
			if stableCoins[coin.Coin] {
				m, err := toMicros(coin.WalletBalance)
				if err != nil {
					return err
				}
				total += m
			}
		}
		return nil
	}
	res, err := c.Get(ctx, "/v5/account/wallet-balance", map[string]string{"accountType": "UNIFIED"})
	if err != nil {
		return b, fmt.Errorf("UNIFIED: %w", err)
	}
	var unified struct {
		List []struct {
			Coin []walletCoin `json:"coin"`
		} `json:"list"`
	}
	if err := json.Unmarshal(res, &unified); err != nil {
		return b, fmt.Errorf("UNIFIED: %v", err)
	}
	for _, acct := range unified.List {
		if err := add("UNIFIED", acct.Coin); err != nil {
			return b, err
		}
	}
	res, err = c.Get(ctx, "/v5/asset/transfer/query-account-coins-balance", map[string]string{"accountType": "FUND"})
	if err != nil {
		return b, fmt.Errorf("FUND: %w", err)
	}
	var fund struct {
		Balance []walletCoin `json:"balance"`
	}
	if err := json.Unmarshal(res, &fund); err != nil {
		return b, fmt.Errorf("FUND: %v", err)
	}
	if err := add("FUND", fund.Balance); err != nil {
		return b, err
	}
	b.Micros, b.StableTotal, b.AllCoins = total, total.String(), true
	return b, nil
}

// BalanceStatus is the cursor of the hourly read.
type BalanceStatus struct {
	At          string `json:"at"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	StableTotal string `json:"stable_total,omitempty"`
	LastOKAt    string `json:"last_ok_at,omitempty"`
}

// RecordBalance stores the reading as an observation: value is the stable total in micros.
func RecordBalance(ctx context.Context, q ledger.Querier, b Balance, now time.Time) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO observations(kind, subject, value, observed_at, source, payload)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, balanceObservation, balanceSubject, int64(b.Micros), ts.Format(now), balanceSource, string(raw))
	return err
}

// CollectBalance reads the balance, stores the observation and the status cursor.
func CollectBalance(ctx context.Context, store *db.Store, c *Client, now time.Time) (Balance, error) {
	b, readErr := ReadBalance(ctx, c)
	st := BalanceStatus{At: ts.Format(now), OK: readErr == nil, StableTotal: b.StableTotal}
	if readErr != nil {
		st.Error, st.StableTotal = readErr.Error(), ""
	}
	prev, err := getCursor(ctx, store.RW, cursorBalance)
	if err != nil {
		return b, errors.Join(readErr, err)
	}
	if prev != "" {
		var p BalanceStatus
		if json.Unmarshal([]byte(prev), &p) == nil {
			st.LastOKAt = p.LastOKAt
		}
	}
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return b, errors.Join(readErr, err)
	}
	defer tx.Rollback()
	if readErr == nil {
		st.LastOKAt = st.At
		if err := RecordBalance(ctx, tx, b, now); err != nil {
			return b, err
		}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return b, errors.Join(readErr, err)
	}
	if err := setCursor(ctx, tx, cursorBalance, string(raw), now); err != nil {
		return b, errors.Join(readErr, err)
	}
	if err := tx.Commit(); err != nil {
		return b, errors.Join(readErr, err)
	}
	return b, readErr
}

// LatestBalance is the last reading of the wallets and when it was taken; ok is false before the first one.
func LatestBalance(ctx context.Context, q ledger.Querier) (b Balance, at string, ok bool, err error) {
	var payload string
	err = q.QueryRowContext(ctx, `SELECT payload, observed_at FROM observations WHERE kind = ? AND subject = ?
		ORDER BY observed_at DESC, id DESC LIMIT 1`, balanceObservation, balanceSubject).Scan(&payload, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return b, "", false, nil
	}
	if err != nil {
		return b, "", false, err
	}
	if err := json.Unmarshal([]byte(payload), &b); err != nil {
		return b, at, false, fmt.Errorf("баланс Bybit на %s: %w", at, err)
	}
	return b, at, true, nil
}

// CoinsWithoutAccount lists the coins of a reading that no account of the books holds: a stable coin is cash
// on `bybit`, any other coin needs its own account bybit:<coin>. Each is «COIN amount (wallet)».
func CoinsWithoutAccount(ctx context.Context, q ledger.Querier, b Balance) ([]string, error) {
	var out []string
	for wallet, coins := range b.Wallets {
		for coin, amount := range coins {
			if stableCoins[coin] {
				continue
			}
			var known int
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE code = ?`, "bybit:"+lower(coin)).Scan(&known); err != nil {
				return nil, err
			}
			if known == 0 {
				out = append(out, fmt.Sprintf("%s %s (%s)", coin, amount, wallet))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
