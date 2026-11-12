package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// Hub's feed of the decisions about whose a sale is (hub#1280, `agentSaleDecisionsFeed`): every change of a decision,
// from every writer — the owner's dispute, the agent's door, the carry-over from ledger, a link, a cancel — in the order
// the feed placed it. A change is placed once it is committed, after every place given before, so one committed late
// comes after the cursor. Each change carries the decision as it is now: applied in order and kept by id, the last one
// read is the decision. No fan's name: the sale is Hub's id and its page. A change of a decision on a page outside the
// key's pages is held back: `held` names it on every read until the key may read it; then Hub gives it a new place at
// the end of the feed and it comes in `changes` by the ordinary cursor. `limit` counts places, held ones too. ledger
// keeps the decisions in internal/decisions; this file is only the wire.

// SaleDecisionsPath is Hub's feed of the decisions.
const SaleDecisionsPath = "/api/v1/agent/sale-decisions/feed"

// The kinds and states of a decision in the feed.
const (
	SaleDecisionOwner       = "owner"
	SaleDecisionSplit       = "split"
	SaleDecisionPot         = "pot"
	SaleDecisionOffPlatform = "off_platform"
	SaleDecisionExclude     = "exclude"

	SaleDecisionActive     = "active"
	SaleDecisionSuperseded = "superseded"
	SaleDecisionCancelled  = "cancelled"
)

var (
	saleDecisionKinds = map[string]bool{SaleDecisionOwner: true, SaleDecisionSplit: true, SaleDecisionPot: true,
		SaleDecisionOffPlatform: true, SaleDecisionExclude: true}
	saleDecisionStates = map[string]bool{SaleDecisionActive: true, SaleDecisionSuperseded: true, SaleDecisionCancelled: true}
)

// SaleDecisionShare is a person's part of a decided sale.
type SaleDecisionShare struct {
	Person  *string `json:"person"` // the ledger's people.slug; null — a login no person holds
	UserID  int64   `json:"userId"`
	Percent int     `json:"percent"`
}

// SaleDecisionApplied says in whose post-based report of what date a decision was taken.
type SaleDecisionApplied struct {
	Person     *string `json:"person"`
	ReportDate string  `json:"reportDate"`
}

// SaleDecision is a decision as it is now.
type SaleDecision struct {
	ID               int64                `json:"id"`
	TransactionID    *int64               `json:"transactionId"` // Hub's sale id; null — no sale, or not tied yet
	PageLabel        *string              `json:"pageLabel"`
	Kind             string               `json:"kind"`
	Shares           []SaleDecisionShare  `json:"shares"`
	Pot              *string              `json:"pot"` // kind pot: the pot's name is the ledger's pot slug
	Outcome          string               `json:"outcome"`
	RuleHome         *string              `json:"ruleHome"`
	State            string               `json:"state"`
	Source           string               `json:"source"`
	SaidBy           *string              `json:"saidBy"`
	SaidAt           *string              `json:"saidAt"`
	DecidedAt        string               `json:"decidedAt"`
	Note             *string              `json:"note"`
	Applied          *SaleDecisionApplied `json:"applied"`
	LedgerDecisionID *int64               `json:"ledgerDecisionId"`
}

// SaleDecisionChange is one change in the feed.
type SaleDecisionChange struct {
	Seq      int64        `json:"seq"`
	Change   string       `json:"change"` // made, superseded, cancelled, linked, applied, changed
	At       string       `json:"at"`
	Decision SaleDecision `json:"decision"`
}

// HeldSaleDecision is a change the feed holds back from the key.
type HeldSaleDecision struct {
	Seq        int64   `json:"seq"`
	DecisionID int64   `json:"decisionId"`
	PageLabel  *string `json:"pageLabel"`
	Reason     string  `json:"reason"` // page_outside_key
}

// SaleDecisionsAnswer is one page of the feed.
type SaleDecisionsAnswer struct {
	Changes []SaleDecisionChange
	// Held are all the changes Hub holds back from the key now, whatever the cursor; nil — the answer has no such list.
	Held      []HeldSaleDecision
	Next      string
	ServerNow string
}

// SaleDecisions reads the changes after since ("" — from the first), at most limit. A Hub without the route answers
// 404: IsAbsent tells it.
func (c *Client) SaleDecisions(ctx context.Context, since string, limit int) (*SaleDecisionsAnswer, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if since != "" {
		q.Set("since", since)
	}
	what := "sale-decisions since " + since
	if since == "" {
		what = "sale-decisions from the first"
	}
	raw, err := c.do(ctx, what, false, http.MethodGet, SaleDecisionsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Changes   []SaleDecisionChange `json:"changes"`
		Held      []HeldSaleDecision   `json:"held"`
		Next      *string              `json:"next"`
		ServerNow *string              `json:"serverNow"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Changes == nil || env.Next == nil || env.ServerNow == nil {
		if err == nil {
			err = errors.New("нет changes, next или serverNow")
		}
		return nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	if _, err := strconv.ParseInt(*env.Next, 10, 64); err != nil && *env.Next != "" {
		return nil, refusef("хаб %s: ответ не по контракту (next %q — не номер)", what, *env.Next)
	}
	for i, ch := range env.Changes {
		d := ch.Decision
		switch {
		case ch.Seq <= 0 || d.ID <= 0:
			return nil, refusef("хаб %s: изменение %d ответа без seq или id решения", what, i+1)
		case !saleDecisionKinds[d.Kind] || !saleDecisionStates[d.State]:
			return nil, refusef("хаб %s: решение %d: kind %q или state %q не по контракту", what, d.ID, d.Kind, d.State)
		case d.TransactionID != nil && *d.TransactionID <= 0:
			return nil, refusef("хаб %s: решение %d: transactionId не номер продажи", what, d.ID)
		}
		for _, s := range d.Shares {
			if s.UserID <= 0 || s.Percent < 0 || s.Percent > 100 {
				return nil, refusef("хаб %s: решение %d: доля не по контракту", what, d.ID)
			}
		}
	}
	for i, h := range env.Held {
		if h.Seq <= 0 || h.DecisionID <= 0 {
			return nil, refusef("хаб %s: придержанное изменение %d ответа без seq или decisionId", what, i+1)
		}
	}
	return &SaleDecisionsAnswer{Changes: env.Changes, Held: env.Held, Next: *env.Next, ServerNow: *env.ServerNow}, nil
}
