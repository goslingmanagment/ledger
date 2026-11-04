package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// All data in these tests is synthetic.
var testKeys = Keys{Key: "synthetic-key", Secret: "synthetic-secret"}

func openDB(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func at(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func ms(s string) string { return strconv.FormatInt(at(s).UnixMilli(), 10) }

func secs(s string) string { return strconv.FormatInt(at(s).Unix(), 10) }

type fakeCall struct {
	Path   string
	Params map[string]string
}

// fakeBybit answers like the v5 API and enforces its limits: a signature on every request, windows
// under 30 days (7 for Funding), P2P size at most 30.
type fakeBybit struct {
	t           *testing.T
	mu          sync.Mutex
	calls       []fakeCall
	deposits    []map[string]any
	internals   []map[string]any
	withdrawals []map[string]any
	p2p         []map[string]any
	funding     []map[string]any
	unified     []map[string]any
	fund        []map[string]any
	readOnly    int
	failCode    map[string]int // path → retCode
	pageSize    int
}

func newFake(t *testing.T) (*fakeBybit, *Client) {
	t.Helper()
	f := &fakeBybit{t: t, readOnly: 1, failCode: map[string]int{}, pageSize: 2}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := NewClient(testKeys)
	c.BaseURL = srv.URL
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return f, c
}

func (f *fakeBybit) reply(w http.ResponseWriter, result any) {
	json.NewEncoder(w).Encode(map[string]any{"retCode": 0, "retMsg": "OK", "result": result})
}

func (f *fakeBybit) refuse(w http.ResponseWriter, code int, msg string) {
	json.NewEncoder(w).Encode(map[string]any{"retCode": code, "retMsg": msg, "result": map[string]any{}})
}

func (f *fakeBybit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	payload := r.URL.RawQuery
	if r.Method == http.MethodPost {
		payload = string(body)
	}
	check := &Client{keys: testKeys}
	if got := r.Header.Get("X-BAPI-SIGN"); got != check.Sign(r.Header.Get("X-BAPI-TIMESTAMP"), payload) {
		f.t.Errorf("%s: bad signature", r.URL.Path)
		f.refuse(w, 10004, "error sign")
		return
	}
	params := map[string]string{}
	for k, v := range r.URL.Query() {
		params[k] = v[0]
	}
	f.calls = append(f.calls, fakeCall{Path: r.URL.Path, Params: params})
	if code := f.failCode[r.URL.Path]; code != 0 {
		f.refuse(w, code, "synthetic failure")
		return
	}
	switch r.URL.Path {
	case depositPath:
		f.list(w, params, f.deposits, "successAt", 1)
	case internalDepositPath:
		f.list(w, params, f.internals, "createdTime", 1000)
	case withdrawPath:
		if id := params["withdrawID"]; id != "" {
			var rows []map[string]any
			for _, x := range f.withdrawals {
				if fmt.Sprint(x["withdrawId"]) == id {
					rows = append(rows, x)
				}
			}
			f.reply(w, map[string]any{"rows": rows, "nextPageCursor": ""})
			return
		}
		if params["withdrawType"] != "2" {
			f.t.Errorf("withdrawals asked without withdrawType 2: %v", params)
		}
		f.list(w, params, f.withdrawals, "createTime", 1)
	case p2pPath:
		f.p2pList(w, body)
	case fundingPath:
		f.fundingList(w, params)
	case "/v5/user/query-api":
		f.reply(w, map[string]any{"readOnly": f.readOnly, "expiredAt": "2027-01-01T00:00:00Z", "ips": []string{"*"},
			"permissions": map[string]any{"Wallet": []string{}}})
	case "/v5/account/wallet-balance":
		f.reply(w, map[string]any{"list": []map[string]any{{"coin": f.unified}}})
	case "/v5/asset/transfer/query-account-coins-balance":
		f.reply(w, map[string]any{"balance": f.fund})
	default:
		f.t.Errorf("unexpected endpoint %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *fakeBybit) list(w http.ResponseWriter, params map[string]string, all []map[string]any, timeField string, toMs int64) {
	start, _ := strconv.ParseInt(params["startTime"], 10, 64)
	end, _ := strconv.ParseInt(params["endTime"], 10, 64)
	if end-start >= 30*24*3600*1000 {
		f.refuse(w, 131001, "window over 30 days")
		return
	}
	var rows []map[string]any
	for _, x := range all {
		v, _ := strconv.ParseInt(fmt.Sprint(x[timeField]), 10, 64)
		if v*toMs >= start && v*toMs <= end { // both ends included, as Bybit does: a row can come twice
			rows = append(rows, x)
		}
	}
	off, _ := strconv.Atoi(params["cursor"])
	size := f.pageSize
	page, next := rows[min(off, len(rows)):], ""
	if len(page) > size {
		page, next = page[:size], strconv.Itoa(off+size)
	}
	if page == nil {
		page = []map[string]any{}
	}
	f.reply(w, map[string]any{"rows": page, "nextPageCursor": next})
}

func (f *fakeBybit) p2pList(w http.ResponseWriter, body []byte) {
	var q struct {
		Page      int    `json:"page"`
		Size      int    `json:"size"`
		BeginTime string `json:"beginTime"`
		EndTime   string `json:"endTime"`
	}
	json.Unmarshal(body, &q)
	if q.Size > 30 {
		f.refuse(w, 40001, "size over 30")
		return
	}
	f.calls[len(f.calls)-1].Params = map[string]string{"page": strconv.Itoa(q.Page), "size": strconv.Itoa(q.Size), "beginTime": q.BeginTime, "endTime": q.EndTime}
	begin, _ := strconv.ParseInt(q.BeginTime, 10, 64)
	end, _ := strconv.ParseInt(q.EndTime, 10, 64)
	var rows []map[string]any
	for _, x := range f.p2p {
		v, _ := strconv.ParseInt(fmt.Sprint(x["createDate"]), 10, 64)
		if v >= begin && v <= end {
			rows = append(rows, x)
		}
	}
	lo := (q.Page - 1) * q.Size
	page := rows[min(lo, len(rows)):]
	if len(page) > q.Size {
		page = page[:q.Size]
	}
	if page == nil {
		page = []map[string]any{}
	}
	json.NewEncoder(w).Encode(map[string]any{"ret_code": 0, "ret_msg": "SUCCESS", "result": map[string]any{"count": len(rows), "items": page}})
}

func (f *fakeBybit) fundingList(w http.ResponseWriter, params map[string]string) {
	from, _ := strconv.ParseInt(params["createTimeFrom"], 10, 64)
	to, _ := strconv.ParseInt(params["createTimeTo"], 10, 64)
	if to-from > 7*24*3600 {
		f.refuse(w, 10001, "window over 7 days")
		return
	}
	var rows []map[string]any
	for _, x := range f.funding {
		v, _ := strconv.ParseInt(fmt.Sprint(x["createTime"]), 10, 64)
		if v >= from && v <= to {
			rows = append(rows, x)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return fmt.Sprint(rows[i]["createTime"]) < fmt.Sprint(rows[j]["createTime"]) })
	off, _ := strconv.Atoi(params["cursor"])
	size := f.pageSize
	page, next := rows[min(off, len(rows)):], ""
	if len(page) > size {
		page, next = page[:size], strconv.Itoa(off+size)
	}
	if page == nil {
		page = []map[string]any{}
	}
	f.reply(w, map[string]any{"list": page, "nextPageCursor": next})
}

func (f *fakeBybit) callsTo(path string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeBybit) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func deposit(id, when, amount, from string) map[string]any {
	return map[string]any{"id": id, "coin": "USDT", "chain": "TRX", "amount": amount, "txID": "tx" + id, "status": 3,
		"toAddress": "Tself", "depositFee": "", "successAt": ms(when), "fromAddress": from, "confirmations": "60"}
}

func internalDeposit(id, when, amount, from string) map[string]any {
	return map[string]any{"id": id, "coin": "USDT", "amount": amount, "status": 2, "createdTime": secs(when), "txID": "", "fromMemberId": from}
}

func withdrawal(id, when, amount, fee, to, status string) map[string]any {
	return map[string]any{"withdrawId": id, "txID": "tx" + id, "withdrawType": 0, "coin": "USDT", "chain": "TRX", "amount": amount,
		"withdrawFee": fee, "status": status, "toAddress": to, "createTime": ms(when)}
}
