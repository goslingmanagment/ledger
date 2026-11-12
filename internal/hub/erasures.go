package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// Hub's feed of completed fan erasures (hub#1271, `agentErasuresFeed`): every fan whose erasure ran to the end, in the
// order of the erasures, after a cursor. It names the platform and the fan's reference only — never the fan's sales, so
// as not to tie back what the erasure cut: ledger finds the fan's sales by the fan reference in its own hub_sales. The
// erasure itself is internal/erasure; this file is only the wire.

// ErasuresPath is Hub's feed of completed fan erasures.
const ErasuresPath = "/api/v1/agent/erasures"

// Erasure is one completed fan erasure.
type Erasure struct {
	ID       int64  `json:"id"`
	Platform string `json:"platform"`
	FanRef   string `json:"fanRef"`
	ErasedAt string `json:"erasedAt"`
}

// ErasuresAnswer is one page of the feed.
type ErasuresAnswer struct {
	Erasures  []Erasure
	Next      string
	ServerNow string
}

// Erasures reads the erasures after since ("" — from the first), at most limit. A Hub without the route answers 404:
// IsAbsent tells it.
func (c *Client) Erasures(ctx context.Context, since string, limit int) (*ErasuresAnswer, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if since != "" {
		q.Set("since", since)
	}
	what := "erasures since " + since
	if since == "" {
		what = "erasures from the first"
	}
	raw, err := c.do(ctx, what, false, http.MethodGet, ErasuresPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Erasures  []Erasure `json:"erasures"`
		Next      *string   `json:"next"`
		ServerNow *string   `json:"serverNow"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Erasures == nil || env.Next == nil || env.ServerNow == nil {
		if err == nil {
			err = errors.New("нет erasures, next или serverNow")
		}
		return nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	if _, err := strconv.ParseInt(*env.Next, 10, 64); err != nil && *env.Next != "" {
		return nil, refusef("хаб %s: ответ не по контракту (next %q — не номер)", what, *env.Next)
	}
	for i, e := range env.Erasures {
		if e.ID <= 0 || e.Platform == "" || e.FanRef == "" {
			return nil, refusef("хаб %s: стирание %d ответа без id, platform или fanRef", what, i+1)
		}
	}
	return &ErasuresAnswer{Erasures: env.Erasures, Next: *env.Next, ServerNow: *env.ServerNow}, nil
}
