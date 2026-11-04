// Package bybit collects the agency's Bybit account read-only through the v5 API: movements
// (deposits, internal deposits, withdrawals, P2P), the Funding journal and balances. Each movement is
// an event; an exact rule or the owner's answer decides how it lands in the journal. It ports
// ledger-sync (bybit_client.py, fetcher.py, funding.py, classify.py, enrich.py, store.py).
package bybit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// DefaultBaseURL is the production API.
const DefaultBaseURL = "https://api.bybit.com"

const recvWindow = "20000"

// Keys is the API key pair. The secret never goes to logs, errors or output.
type Keys struct{ Key, Secret string }

// KeysFromEnv reads LEDGER_BYBIT_API_KEY and LEDGER_BYBIT_API_SECRET; ok is false without either.
func KeysFromEnv() (Keys, bool) {
	k := Keys{Key: strings.TrimSpace(os.Getenv("LEDGER_BYBIT_API_KEY")), Secret: strings.TrimSpace(os.Getenv("LEDGER_BYBIT_API_SECRET"))}
	return k, k.Key != "" && k.Secret != ""
}

// APIError is an answer with retCode ≠ 0.
type APIError struct {
	Path string
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: retCode=%d retMsg=%s", e.Path, e.Code, e.Msg)
}

// Retried codes: too many visits, server error, request outside the receive window.
var retryCodes = map[int]bool{10006: true, 10016: true, 10002: true}

// Client signs every request (HMAC-SHA256 of timestamp + key + recv window + query or body) and
// calls only read endpoints.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error // pauses between attempts; tests replace it
	Calls   int                                        // requests sent, retries included
	keys    Keys
}

// NewClient builds a client of the production API (LEDGER_BYBIT_URL overrides the address).
func NewClient(k Keys) *Client {
	base := strings.TrimRight(os.Getenv("LEDGER_BYBIT_URL"), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{BaseURL: base, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now, Sleep: sleepCtx, keys: k}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Sign is the v5 signature of a request made at timestamp (ms) with query string or raw body.
func (c *Client) Sign(timestamp, payload string) string {
	mac := hmac.New(sha256.New, []byte(c.keys.Secret))
	mac.Write([]byte(timestamp + c.keys.Key + recvWindow + payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Get calls a signed GET; empty parameters are dropped. It returns the `result` object.
func (c *Client) Get(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	return c.do(ctx, http.MethodGet, path, q.Encode(), nil)
}

// Post calls a signed POST with a JSON body (the P2P endpoints are POST but read nothing else).
func (c *Client) Post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, path, "", raw)
}

func (c *Client) do(ctx context.Context, method, path, query string, body []byte) (json.RawMessage, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := c.Sleep(ctx, time.Duration(2*attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		// The signature lives seconds: a fresh timestamp for every attempt.
		stamp := strconv.FormatInt(c.Now().UnixMilli(), 10)
		target, payload := c.BaseURL+path, string(body)
		if method == http.MethodGet {
			target, payload = target+"?"+query, query
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-BAPI-API-KEY", c.keys.Key)
		req.Header.Set("X-BAPI-TIMESTAMP", stamp)
		req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)
		req.Header.Set("X-BAPI-SIGN", c.Sign(stamp, payload))
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		c.Calls++
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // without the address: the error may reach logs
			}
			last = fmt.Errorf("%s: сеть: %v", path, err)
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			last = fmt.Errorf("%s: ответ оборван: %v", path, err)
			continue
		}
		if resp.StatusCode >= 500 {
			last = fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, snippet(raw))
		}
		result, err := unwrap(path, raw)
		var api *APIError
		if errors.As(err, &api) && retryCodes[api.Code] {
			last = err
			continue
		}
		return result, err
	}
	return nil, fmt.Errorf("после 3 попыток: %w", last)
}

// unwrap checks the envelope: v5 answers retCode/retMsg, the P2P endpoints the older ret_code/ret_msg.
func unwrap(path string, raw []byte) (json.RawMessage, error) {
	var env struct {
		RetCode  *int            `json:"retCode"`
		RetCode2 *int            `json:"ret_code"`
		RetMsg   string          `json:"retMsg"`
		RetMsg2  string          `json:"ret_msg"`
		Result   json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: ответ не JSON: %s", path, snippet(raw))
	}
	code, msg := env.RetCode, env.RetMsg
	if code == nil {
		code, msg = env.RetCode2, env.RetMsg2
	}
	if code == nil {
		return nil, fmt.Errorf("%s: в ответе нет retCode: %s", path, snippet(raw))
	}
	if *code != 0 {
		return nil, &APIError{Path: path, Code: *code, Msg: msg}
	}
	return env.Result, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// KeyInfo is what /v5/user/query-api says about the key (never the key itself).
type KeyInfo struct {
	ReadOnly    bool            `json:"read_only"`
	ExpiredAt   string          `json:"expired_at,omitempty"`
	IPBound     bool            `json:"ip_bound"`
	Permissions json.RawMessage `json:"permissions,omitempty"`
	CheckedAt   string          `json:"checked_at"`
}

// CheckKey reads the key's rights. ledger refuses to collect with a key that can trade or withdraw.
func (c *Client) CheckKey(ctx context.Context) (KeyInfo, error) {
	res, err := c.Get(ctx, "/v5/user/query-api", nil)
	if err != nil {
		return KeyInfo{}, err
	}
	var r struct {
		ReadOnly    json.Number     `json:"readOnly"`
		ExpiredAt   string          `json:"expiredAt"`
		IPs         []string        `json:"ips"`
		Permissions json.RawMessage `json:"permissions"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return KeyInfo{}, fmt.Errorf("/v5/user/query-api: %v", err)
	}
	bound := false
	for _, ip := range r.IPs {
		if ip != "" && ip != "*" {
			bound = true
		}
	}
	return KeyInfo{ReadOnly: r.ReadOnly.String() == "1", ExpiredAt: r.ExpiredAt, IPBound: bound, Permissions: r.Permissions,
		CheckedAt: ts.Format(c.Now())}, nil
}
