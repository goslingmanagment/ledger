package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	stableCoins = map[string]bool{"USDT": true, "USDC": true, "USD": true, "DAI": true}
	dustLimit   = mustRat("0.01")
)

// Balance is what the wallets hold: FUND + UNIFIED, coin by coin.
type Balance struct {
	Wallets     map[string]map[string]string `json:"wallets"`
	StableTotal string                       `json:"stable_total"`
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
			if !ok || v.Cmp(dustLimit) <= 0 {
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
	b.Micros, b.StableTotal = total, total.String()
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
