package bybit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The vectors come from ledger-sync's bybit_client.py with the same synthetic key and secret.
func TestSignKnownVector(t *testing.T) {
	c := NewClient(testKeys)
	if got := c.Sign("1700000000000", "accountType=FUND&coin=USDT"); got != "1a76c56033436704033487d09a6e0ae0b3e23b4840c2dbf31eaa1afe88d10af2" {
		t.Fatalf("GET signature %s", got)
	}
	if got := c.Sign("1700000000000", `{"beginTime":"1","endTime":"2","page":1,"size":30}`); got != "e4d157c98f2efe20a09395b1180c29bbde8c06a3ae150c0d4b3615633548fb80" {
		t.Fatalf("POST signature %s", got)
	}
}

func TestRequestHeadersAndFreshTimestamp(t *testing.T) {
	var stamps []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-BAPI-API-KEY") != testKeys.Key || r.Header.Get("X-BAPI-RECV-WINDOW") != recvWindow {
			t.Errorf("headers %v", r.Header)
		}
		stamps = append(stamps, r.Header.Get("X-BAPI-TIMESTAMP"))
		if len(stamps) == 1 {
			w.WriteHeader(502)
			return
		}
		w.Write([]byte(`{"retCode":0,"result":{"ok":1}}`))
	}))
	defer srv.Close()
	c := NewClient(testKeys)
	c.BaseURL = srv.URL
	clock := time.UnixMilli(1_700_000_000_000)
	c.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	res, err := c.Get(context.Background(), "/x", map[string]string{"a": "1", "empty": ""})
	if err != nil || !strings.Contains(string(res), `"ok":1`) {
		t.Fatal(string(res), err)
	}
	if len(stamps) != 2 || stamps[0] == stamps[1] {
		t.Fatalf("a retry must sign a fresh timestamp: %v", stamps)
	}
}

func TestRetCodesAndEnvelopes(t *testing.T) {
	var answers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(answers[0]))
		answers = answers[1:]
	}))
	defer srv.Close()
	c := NewClient(testKeys)
	c.BaseURL = srv.URL
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()

	answers = []string{`{"retCode":10003,"retMsg":"bad key"}`}
	_, err := c.Get(ctx, "/x", nil)
	var api *APIError
	if !errors.As(err, &api) || api.Code != 10003 {
		t.Fatalf("want APIError 10003, got %v", err)
	}
	answers = []string{`{"ret_code":0,"ret_msg":"SUCCESS","result":{"items":[]}}`}
	if res, err := c.Post(ctx, "/p2p", map[string]any{"page": 1}); err != nil || string(res) != `{"items":[]}` {
		t.Fatal(string(res), err)
	}
	answers = []string{`{"ret_code":912,"ret_msg":"nope"}`}
	if _, err := c.Post(ctx, "/p2p", map[string]any{"page": 1}); !errors.As(err, &api) || api.Code != 912 {
		t.Fatalf("old envelope error: %v", err)
	}
	answers = []string{`{"retCode":10006,"retMsg":"too many"}`, `{"retCode":10006,"retMsg":"too many"}`, `{"retCode":0,"result":{}}`}
	if _, err := c.Get(ctx, "/x", nil); err != nil {
		t.Fatalf("rate limit is retried: %v", err)
	}
	answers = []string{`{"retCode":10006}`, `{"retCode":10006}`, `{"retCode":10006}`}
	if _, err := c.Get(ctx, "/x", nil); err == nil || !strings.Contains(err.Error(), "после 3 попыток") {
		t.Fatalf("three failures: %v", err)
	}
	answers = []string{`not json`}
	if _, err := c.Get(ctx, "/x", nil); err == nil {
		t.Fatal("not JSON accepted")
	}
	answers = []string{`{"result":{}}`}
	if _, err := c.Get(ctx, "/x", nil); err == nil {
		t.Fatal("answer without retCode accepted")
	}
}

func TestErrorsCarryNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	c := NewClient(testKeys)
	c.BaseURL = srv.URL
	_, err := c.Get(context.Background(), "/x", map[string]string{"a": "1"})
	if err == nil || strings.Contains(err.Error(), testKeys.Secret) || strings.Contains(err.Error(), testKeys.Key) {
		t.Fatalf("error: %v", err)
	}
}
