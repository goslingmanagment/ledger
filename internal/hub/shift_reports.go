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
// version of every report after a cursor, and `next` is the cursor to ask from. Since hub#1266 the feed's order is the
// place Hub gives a version once it is committed (`seq`), no longer the version's id: a version committed late is placed
// after the cursor. ledger follows `next` and also reads a window behind it again (internal/reports, hubOverlap), so a
// version an older Hub served late is not lost either. Since hub#1279 a version with a line on a page outside the key's
// pages is held back, not dropped: `held` names it on every read until the key may read it; then Hub gives it a new
// place at the end of the feed and it comes in `reports` by the ordinary cursor. `limit` counts places, held ones too,
// and `next` is the last place read, held or not. ledger reads with its key (read:team and read:money) and keeps the
// versions in internal/reports, by their id whatever their place; this file is only the wire.

// ShiftReportsPath is Hub's feed of the handed-in shift reports.
const ShiftReportsPath = "/api/v1/agent/shift-reports"

// ShiftReport is one version in the feed. Amounts are integer mills; a page's amount is its lines' rounded to the cent.
type ShiftReport struct {
	ID int64 `json:"id"`
	// Seq is the version's place in the feed (hub#1266); 0 — an older Hub, whose order was the id.
	Seq                int64                `json:"seq"`
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
	// ClaimedMills is what a disputed «моё» claims (hub#1279): such a line counts nothing (AmountMills 0) until the
	// dispute is settled, and says here the net it claims. nil — not disputed, or a Hub before hub#1279, whose disputed
	// line counted its whole net.
	ClaimedMills *int64 `json:"claimedMills"`
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

// HeldVersion is a version the feed holds back from the key (hub#1279): a line of it is on a page outside the key's
// pages. No sums and no lines: who, when, and the pages outside the key.
type HeldVersion struct {
	ID         int64    `json:"id"`
	Seq        int64    `json:"seq"`
	ShiftID    int64    `json:"shiftId"`
	Person     *string  `json:"person"` // the ledger's people.slug; null — a login no person holds
	ReportDate string   `json:"reportDate"`
	Reason     string   `json:"reason"` // page_outside_key
	PageLabels []string `json:"pageLabels"`
}

// ShiftReportsAnswer is one page of the feed, each version with its raw JSON.
type ShiftReportsAnswer struct {
	Reports []ShiftReport
	Raw     []json.RawMessage
	// Held are all the versions Hub holds back from the key now, whatever the cursor; nil — the answer has no such list
	// (a Hub before hub#1279), which says nothing about what is held.
	Held      []HeldVersion
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
		Held      []HeldVersion     `json:"held"`
		Next      *string           `json:"next"`
		ServerNow *string           `json:"serverNow"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Reports == nil || env.Next == nil || env.ServerNow == nil {
		if err == nil {
			err = errors.New("нет reports, next или serverNow")
		}
		return nil, refusef("хаб %s: ответ не по контракту (%v)", what, err)
	}
	a := &ShiftReportsAnswer{Raw: env.Reports, Held: env.Held, Next: *env.Next, ServerNow: *env.ServerNow}
	if _, err := strconv.ParseInt(a.Next, 10, 64); err != nil && a.Next != "" {
		return nil, refusef("хаб %s: ответ не по контракту (next %q — не номер версии)", what, a.Next)
	}
	for i, h := range a.Held {
		if h.ID <= 0 {
			return nil, refusef("хаб %s: придержанная версия %d ответа без id", what, i+1)
		}
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
