package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// The shift reports handed in in Hub (ecosystem F2; Hub's side is hub#1225): `agentShiftReportsFeed` serves every
// version of every report in the order they were made, after a cursor that is the version's own id. A version is served
// once it is 10 seconds old, so one that commits late is never skipped past. ledger reads it with its key (read:team and
// read:money) and keeps the versions in internal/reports; this file is only the wire.

// ShiftReportsPath is Hub's feed of the handed-in shift reports.
const ShiftReportsPath = "/api/v1/agent/shift-reports"

// ShiftReport is one version in the feed. Amounts are integer mills; a page's amount is its lines' rounded to the cent.
type ShiftReport struct {
	ID                 int64                `json:"id"`
	Person             *string              `json:"person"` // the ledger's people.slug; null — a login no person holds
	HubUserID          int64                `json:"hubUserId"`
	ShiftID            int64                `json:"shiftId"`
	ReportDate         string               `json:"reportDate"`
	Version            int                  `json:"version"`
	SubmittedAt        string               `json:"submittedAt"`
	Door               string               `json:"door"`
	TelegramMessageRef *string              `json:"telegramMessageRef"`
	TotalMills         *int64               `json:"totalMills"`
	Pages              []ShiftReportPage    `json:"pages"`
	Lines              []ShiftReportLine    `json:"lines"`
	OffPlatform        []ShiftReportOffSale `json:"offPlatform"`
	Comment            string               `json:"comment"`
	DayPaid            *bool                `json:"dayPaid"`
	RuleVersion        string               `json:"ruleVersion"`
}

// ShiftReportPage is a page of a version and its amount.
type ShiftReportPage struct {
	PageLabel   string `json:"pageLabel"`
	AmountMills *int64 `json:"amountMills"`
}

// ShiftReportLine is a sale of a version: what the rule says, what the person says, what it counts.
type ShiftReportLine struct {
	TransactionID   int64   `json:"transactionId"`
	PageLabel       string  `json:"pageLabel"`
	Group           string  `json:"group"` // mine, near
	NetMills        *int64  `json:"netMills"`
	RuleShareMills  *int64  `json:"ruleShareMills"`
	RuleHome        string  `json:"ruleHome"`
	RuleReason      string  `json:"ruleReason"`
	Statement       *string `json:"statement"` // mine, not_mine, share; null — the rule stands
	ShareWithUserID *int64  `json:"shareWithUserId"`
	Note            *string `json:"note"`
	AmountMills     *int64  `json:"amountMills"`
	Added           bool    `json:"added"`
	Disputed        bool    `json:"disputed"`
}

// ShiftReportOffSale is money «мимо площадки»: never in the version's total until the owner confirms it; a confirmed one
// counts in the sum of its page (hub#1252) — a version made on the owner's word includes it there.
type ShiftReportOffSale struct {
	ID          int64  `json:"id"`
	AmountMills *int64 `json:"amountMills"`
	FromWhom    string `json:"fromWhom"`
	WhatFor     string `json:"whatFor"`
	State       string `json:"state"` // pending, confirmed, rejected
	// PageLabel is a confirmed amount's page; nil — none (an older Hub, or not confirmed).
	PageLabel *string `json:"pageLabel"`
}

// CountsOnPage is the page a confirmed amount counts on; "" — it counts nowhere (not confirmed, or no page).
func (o ShiftReportOffSale) CountsOnPage() string {
	if o.State != "confirmed" || o.PageLabel == nil {
		return ""
	}
	return *o.PageLabel
}

// ShiftReportsAnswer is one page of the feed, each version with its raw JSON.
type ShiftReportsAnswer struct {
	Reports   []ShiftReport
	Raw       []json.RawMessage
	Next      string
	ServerNow string
}

// ShiftReports reads the versions after since ("" — from the first), at most limit. A Hub without the route answers 404:
// IsAbsent tells it.
func (c *Client) ShiftReports(ctx context.Context, since string, limit int) (*ShiftReportsAnswer, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if since != "" {
		q.Set("since", since)
	}
	what := "shift-reports since " + since
	if since == "" {
		what = "shift-reports from the first"
	}
	raw, err := c.do(ctx, what, false, http.MethodGet, ShiftReportsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Reports   []json.RawMessage `json:"reports"`
		Next      *string           `json:"next"`
		ServerNow *string           `json:"serverNow"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Reports == nil || env.Next == nil || env.ServerNow == nil {
		if err == nil {
			err = errors.New("нет reports, next или serverNow")
		}
		return nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	a := &ShiftReportsAnswer{Raw: env.Reports, Next: *env.Next, ServerNow: *env.ServerNow}
	if _, err := strconv.ParseInt(a.Next, 10, 64); err != nil && a.Next != "" {
		return nil, refusef("хаб %s: ответ не по контракту (next %q — не номер версии)", what, a.Next)
	}
	for i, r := range env.Reports {
		var s ShiftReport
		if err := json.Unmarshal(r, &s); err != nil {
			return nil, refusef("хаб %s: версия %d ответа не по контракту (%v)", what, i+1, err)
		}
		// What a version is known by: without it nothing can be stored, superseded or read again safely.
		if s.ID <= 0 || s.ShiftID <= 0 || s.Version < 1 {
			return nil, refusef("хаб %s: версия %d ответа без id, shiftId или version", what, i+1)
		}
		a.Reports = append(a.Reports, s)
	}
	return a, nil
}

// IsAbsent says the hub has no such route yet (404): an older Hub, nothing to read, not a failure.
func IsAbsent(err error) bool {
	var r *Refusal
	return errors.As(err, &r) && r.Status == http.StatusNotFound
}

// IsKeyRefused says the hub refused the key (401, 403); KeyRefusalText says what the owner does about it.
func IsKeyRefused(err error) bool {
	var r *Refusal
	return errors.As(err, &r) && (r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden)
}

// KeyRefusalText says what the owner does about a key the hub refuses.
func KeyRefusalText(err error) string {
	var r *Refusal
	if !errors.As(err, &r) {
		return err.Error()
	}
	return keyRefusal(r)
}

// String is the version as logs and problems name it.
func (s ShiftReport) String() string {
	return fmt.Sprintf("версия %d (смена %d, v%d, %s)", s.ID, s.ShiftID, s.Version, s.ReportDate)
}
