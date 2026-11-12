// Package split decides whose each Hub sale is: a shift's (or shares of shifts), a look-in's, the общак's, waiting for
// its PPV sender's next shift, waiting for a decision, nobody's, or before the rule (the payout sheets pay it).
//
// A port of Hub's sale-rule.ts (branch fix/shift-review-ppv-sender, closed hub#1085) and the marks of shift-report.ts
// (`judgeMarks`, `halvesOf`, `dueCreditsOf`), with the owner's decisions of 2026-11-08 and 2026-11-09
// (docs/plans/2026-11-09-release-2.md, «Правила дележа»). From Rules.Since (2026-11-02 00:00 MSK):
//
//  1. A PPV a chatter sent is the sender's whoever's shift it was bought in: in their own shift or look-in that holds
//     it; else in their shift going at the purchase, on another page; else in their next shift, if it begins within
//     Rules.Wait (till then it is Waiting, later Nobody). A PPV of a team lead or the owner has no sender here.
//  2. A tip under a wall post and an OnlyFans Campaign contribution are the общак (Pot), no shift's.
//  3. The rest by the shifts that hold the sale: one — its; several — the one whose person last wrote to the fan
//     (48 h, Hub's fact); none of them — Decide, until the owner splits it or the chatters' marks agree. No shift holds
//     it: the look-in whose person last wrote, else Nobody.
//  4. A sale before Rules.Since, or one that by the old rule is the money of a shift of a Moscow day before it (the
//     night of 1 November past midnight), is BeforeRule: the weeks before the rule do not move by a cent.
//  5. A Nobody's sale someone claimed, by the owner's word (Sale.ClaimedBy, ledger's sale_claims), is the claimer's
//     whole, on no work (the owner, 2026-11-09, decision 3). A claim moves no other home.
//  6. Hub's decision of the sale (Sale.Decision, hub#1279; Hub's `ownerOrAgreed`) comes before all of it but 4: «никому»
//     is Nobody's, a pot decision is that pot's, a decision that gives the sale to people goes by its shares
//     (Sale.Resolution) — a whole sale given where the rule finds nobody is the claim of 5. The caller sets Decision only
//     for the sales Hub's word rules (settings.sale_decisions_hub_from): without it Resolution splits a Decide alone.
//
// Pure: no database and no clock, `now` is an argument. Money is Hub's mills (1/1000 USD), split as Hub splits it.
package split

import (
	"sort"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// The kinds of a piece of work and of a home.
const (
	Shift      = "shift"       // a shift (a piece of work), or a home in shifts
	Visit      = "visit"       // a look-in: work too short to be a shift
	Pot        = "pot"         // the общак: Home.Pot names which
	Waiting    = "waiting"     // a PPV waits for its sender's next shift, till Home.WaitsUntil
	Decide     = "decide"      // several shifts held it, none of their people wrote last: the owner or the marks decide
	Nobody     = "nobody"      // nobody's money
	BeforeRule = "before_rule" // the old rule's: the payout sheets pay it, the facts do not
	Unknown    = "unknown"     // the facts are not whole (a holder is missing from the works): not counted
)

// PotObshak is the one pot of the rule: tips under wall posts and Campaign contributions.
const PotObshak = "obshak"

// The sale kinds the team's rule reads (Hub's `salesTheRuleReads`: a purchased PPV message or a tip).
const (
	SalePPV          = "ppv"
	SaleTip          = "tip"
	SaleSubscription = "subscription"
	SalePost         = "post"
)

// RoleChatter is the only role whose PPV goes to its sender.
const RoleChatter = "chatter"

// The marks of a chatter's shift report («моё», «делим», «не моё»).
const (
	MarkMine    = "mine"
	MarkSplit   = "split"
	MarkNotMine = "not_mine"
)

// Why a sale's home is what it is.
const (
	ReasonSaleBeforeRule     = "sale_before_rule"      // the sale is before Rules.Since
	ReasonWorkBeforeRule     = "work_before_rule"      // the old rule gives it to work of a day before the rule
	ReasonHolderNotInWorks   = "holder_not_in_works"   // Sale.Holders names work the works do not have
	ReasonPPVSender          = "ppv_sender"            // the sender's own shift or look-in holds it
	ReasonPPVSenderOtherPage = "ppv_sender_other_page" // the sender's shift going at the purchase, not holding it
	ReasonPPVSenderNextShift = "ppv_sender_next_shift" // the sender's next shift within Rules.Wait
	ReasonPPVSenderWaiting   = "ppv_sender_waiting"    // no such shift yet, Rules.Wait not over
	ReasonPPVSenderNoShift   = "ppv_sender_no_shift"   // no shift of the sender within Rules.Wait
	ReasonPostTip            = "post_tip"
	ReasonCampaign           = "campaign"
	ReasonHolder             = "holder"              // the one shift that holds it
	ReasonLastWriter         = "last_writer"         // of the shifts holding it, the one whose person last wrote
	ReasonLookInLastWriter   = "look_in_last_writer" // no shift holds it; a look-in whose person last wrote does
	ReasonNoHolder           = "no_holder"
	ReasonNoLastWriter       = "no_last_writer" // several shifts, none of their people last: no decision yet
	ReasonMarksDisagree      = "marks_disagree" // the marks are a dispute (`judgeMarks`): the owner splits it
	ReasonMarks              = "marks"          // the chatters' confirmed marks agree
	ReasonOwnerSplit         = "owner_split"    // the owner's split of a disputed sale
	ReasonOwnerClaim         = "owner_claim"    // a nobody's sale the owner gave the person who claimed it
	ReasonOwnerNobody        = "owner_nobody"   // Hub's decision: the sale is nobody's («никому»)
	ReasonOwnerPot           = "owner_pot"      // Hub's decision: the sale is a pot's (Home.Pot)
)

// The kinds of Hub's decision of a sale, as its team facts carry them (`decision.kind`).
const (
	DecisionSplit  = "split"  // the sale is given to people: Sale.Resolution has the shares (one of 100 — a whole sale)
	DecisionNobody = "nobody" // «никому»
	DecisionPot    = "pot"    // a pot's: Decision.Pot
)

// Rules are the rule's dates and wait; Default is the seeded split_rules rows'.
type Rules struct {
	// Since is the first instant the team's rule settles; its Moscow date is the first day of work it pays.
	Since time.Time
	// Wait is how long a chatter's PPV waits for their next shift.
	Wait time.Duration
}

// Default: from 2026-11-02 00:00 MSK, a PPV waits 7 days (the owner, 2026-11-08).
var Default = Rules{Since: time.Date(2026, 11, 2, 0, 0, 0, 0, ts.MSK), Wait: 7 * 24 * time.Hour}

// Work is a person's shift or look-in, as Hub computes it.
type Work struct {
	Ref       string     `json:"ref"` // "<userId>:<startedAt ISO>"
	UserID    int64      `json:"userId"`
	Username  string     `json:"username"`
	Kind      string     `json:"kind"` // Shift or Visit
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   time.Time  `json:"endedAt"`
	Ongoing   bool       `json:"ongoing"`
	Role      string     `json:"role"` // chatter, team_lead, owner
	Pages     []WorkPage `json:"pages"`
}

// WorkPage is a page the work held sales of, from the moment it began working there.
type WorkPage struct {
	PageLabel string    `json:"pageLabel"`
	From      time.Time `json:"from"`
}

// Sale is one Hub sale with the facts the rule reads.
type Sale struct {
	TransactionRef string    `json:"transactionRef"`
	PageLabel      string    `json:"pageLabel"`
	Platform       string    `json:"platform"`
	FanRef         string    `json:"fanRef"`
	Kind           string    `json:"kind"` // SalePPV, SaleTip, SaleSubscription, SalePost
	NetMills       int64     `json:"netMills"`
	OccurredAt     time.Time `json:"occurredAt"`
	// Holders are the refs of every work (shifts and look-ins) holding the sale's page at the sale (Hub's
	// `workHoldsSale`: from the work's start on the page to its end).
	Holders   []string   `json:"holders"`
	PPVSender *Person    `json:"ppvSender"` // nil: not a PPV, or its message has no author
	PPVSentAt *time.Time `json:"ppvSentAt"`
	PostTip   bool       `json:"postTip"`
	Campaign  bool       `json:"campaign"`
	// LastWriter is the user who last wrote to the fan on the page within 48 hours before the sale; nil — nobody known.
	LastWriter *int64 `json:"lastWriter"`
	// Marks are the chatters' marks of the sale in their shift reports.
	Marks []Mark `json:"marks"`
	// Resolution is the owner's split of a disputed sale; nil — none. With Decision it is the shares Hub's decision gives.
	Resolution []OwnerShare `json:"resolution"`
	// Decision is Hub's active decision of the sale, which comes before the rule; nil — none, or not to be read.
	Decision *Decision `json:"decision"`
	// ClaimedBy is the user the owner gives the sale if it is nobody's (ledger's sale_claims, not Hub's); nil — none.
	ClaimedBy *int64 `json:"-"`
}

// Decision is Hub's decision of a sale: its kind and, for a pot, which.
type Decision struct {
	Kind string `json:"kind"` // DecisionSplit, DecisionNobody, DecisionPot
	Pot  string `json:"pot"`
}

// Person is the PPV's sender with their role now.
type Person struct {
	UserID   int64  `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Mark is a chatter's mark of a sale.
type Mark struct {
	UserID int64  `json:"userId"`
	Mark   string `json:"mark"` // MarkMine, MarkSplit, MarkNotMine
	// ShareWith is the colleague a «делим» names; two «делим» agree only naming each other.
	ShareWith *int64 `json:"shareWith"`
	// OwnAmountMills is the chatter's own amount of a «моё» (never above the net); nil — the whole net.
	OwnAmountMills *int64 `json:"ownAmountMills"`
	// Confirmed: the chatter's confirmed shift report holds the sale (Hub's report line). A draft mark decides nothing.
	Confirmed bool `json:"confirmed"`
}

// OwnerShare is one person's percent of the owner's split.
type OwnerShare struct {
	UserID  int64 `json:"userId"`
	Percent int   `json:"percent"`
}

// Home is where a sale's money goes.
type Home struct {
	Kind string `json:"kind"`
	// Shares of a Shift or Visit home: whose work gets how much. Mills sum to the net, except a «моё» with an own
	// amount below it: the rest is nobody's.
	Shares     []Share   `json:"shares,omitempty"`
	Pot        string    `json:"pot,omitempty"`
	Reason     string    `json:"reason"`
	WaitsUntil time.Time `json:"waitsUntil,omitzero"` // a Waiting PPV's last moment for its sender's shift
}

// Share is one work's part of a sale. WorkRef is empty for a person the owner or a «делим» gave a share who has no
// work holding the sale: date it by the sale.
type Share struct {
	WorkRef string `json:"workRef"`
	UserID  int64  `json:"userId"`
	// Percent of the net: 100, the halves' 50, the owner's; an own amount's, rounded down. Count Mills.
	Percent int   `json:"percent"`
	Mills   int64 `json:"mills"`
}

// HomeOf is a sale's home by the Default rules. `works` — everyone's around it (Rules.Wait and a day more on each
// side, so a PPV finds its sender's shifts), any order.
func HomeOf(sale Sale, works []Work, now time.Time) Home { return Default.HomeOf(sale, works, now) }

// HomesOf is every sale's home by the Default rules, in the order given.
func HomesOf(sales []Sale, works []Work, now time.Time) []Home {
	return Default.HomesOf(sales, works, now)
}

// HomeOf is a sale's home (see the package's head).
func (r Rules) HomeOf(sale Sale, works []Work, now time.Time) Home {
	return r.home(sale, indexOf(works), now)
}

// HomesOf is every sale's home, in the order given.
func (r Rules) HomesOf(sales []Sale, works []Work, now time.Time) []Home {
	ix := indexOf(works)
	homes := make([]Home, len(sales))
	for i, sale := range sales {
		homes[i] = r.home(sale, ix, now)
	}
	return homes
}

type index struct {
	works []Work
	byRef map[string]int
}

func indexOf(works []Work) index {
	ix := index{works: works, byRef: make(map[string]int, len(works))}
	for i, w := range works {
		if _, ok := ix.byRef[w.Ref]; !ok {
			ix.byRef[w.Ref] = i
		}
	}
	return ix
}

func (r Rules) home(sale Sale, ix index, now time.Time) Home {
	h := r.ruleHome(sale, ix, now)
	if h.Kind != BeforeRule {
		if d, ok := decidedHome(sale, h, ix); ok {
			return d
		}
	}
	if h.Kind == Nobody && sale.ClaimedBy != nil {
		return claimed(*sale.ClaimedBy, sale.NetMills)
	}
	return h
}

// claimed is a nobody's sale given whole to a person, on no work.
func claimed(user, net int64) Home {
	return Home{Kind: Shift, Reason: ReasonOwnerClaim, Shares: []Share{{UserID: user, Percent: 100, Mills: net}}}
}

// decidedHome is the home Hub's decision gives the sale, before the rule's (`found`): «никому» is nobody's, a pot
// decision with a pot named is that pot's, a decision that gives the sale to people goes by its shares — a whole sale
// given where the rule finds nobody is the claim's form. false — no decision, or nothing in it to go by: the rule's home
// stands (a Decide the shares already split is the same home).
func decidedHome(sale Sale, found Home, ix index) (Home, bool) {
	if sale.Decision == nil {
		return Home{}, false
	}
	switch sale.Decision.Kind {
	case DecisionNobody:
		return Home{Kind: Nobody, Reason: ReasonOwnerNobody}, true
	case DecisionPot:
		if sale.Decision.Pot != "" {
			return Home{Kind: Pot, Pot: sale.Decision.Pot, Reason: ReasonOwnerPot}, true
		}
	case DecisionSplit:
		res := sale.Resolution
		switch {
		case len(res) == 0 || found.Reason == ReasonOwnerSplit:
			return Home{}, false
		case found.Kind == Nobody && len(res) == 1 && res[0].Percent == 100:
			return claimed(res[0].UserID, sale.NetMills), true
		}
		shares := ownerShares(sale.NetMills, res, func(user int64) string { return workRefFor(user, sale, found, ix) })
		return sharesHome(shares, ReasonOwnerSplit, ix), true
	}
	return Home{}, false
}

// workRefFor is a person's work for a share of a decided sale: the one the rule's home already names for them, else
// their work holding the sale (a shift before a look-in); "" — none, the share is dated by the sale.
func workRefFor(user int64, sale Sale, found Home, ix index) string {
	for _, s := range found.Shares {
		if s.UserID == user && s.WorkRef != "" {
			return s.WorkRef
		}
	}
	visit := ""
	for _, ref := range sale.Holders {
		i, ok := ix.byRef[ref]
		if !ok || ix.works[i].UserID != user {
			continue
		}
		if ix.works[i].Kind != Visit {
			return ref
		}
		if visit == "" {
			visit = ref
		}
	}
	return visit
}

// ruleHome is the home by the rule alone, the claims aside.
func (r Rules) ruleHome(sale Sale, ix index, now time.Time) Home {
	at := sale.OccurredAt
	if at.Before(r.Since) {
		return Home{Kind: BeforeRule, Reason: ReasonSaleBeforeRule}
	}
	var shifts, visits []*Work
	seen := map[string]bool{}
	for _, ref := range sale.Holders {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		i, ok := ix.byRef[ref]
		if !ok {
			return Home{Kind: Unknown, Reason: ReasonHolderNotInWorks}
		}
		if w := &ix.works[i]; w.Kind == Visit {
			visits = append(visits, w)
		} else {
			shifts = append(shifts, w)
		}
	}
	// Hub's old rule: the shifts holding it that `settleSale` does not call a colleague's.
	old := oldHolders(shifts, sale.LastWriter)
	for _, w := range old {
		if r.beforeRule(*w) {
			return Home{Kind: BeforeRule, Reason: ReasonWorkBeforeRule}
		}
	}
	if sale.Kind == SalePPV || sale.Kind == SaleTip {
		switch {
		case sale.PostTip:
			return Home{Kind: Pot, Pot: PotObshak, Reason: ReasonPostTip}
		case sale.Campaign:
			return Home{Kind: Pot, Pot: PotObshak, Reason: ReasonCampaign}
		case sale.PPVSender != nil && sale.PPVSender.Role == RoleChatter:
			return r.homeOfSender(sale, sale.PPVSender.UserID, append(append([]*Work{}, shifts...), visits...), ix, now)
		}
	}
	switch {
	case len(old) == 0:
		for _, w := range visits {
			if sale.LastWriter != nil && w.UserID == *sale.LastWriter {
				if r.beforeRule(*w) {
					return Home{Kind: BeforeRule, Reason: ReasonWorkBeforeRule}
				}
				return whole(w, sale.NetMills, ReasonLookInLastWriter)
			}
		}
		return Home{Kind: Nobody, Reason: ReasonNoHolder}
	case oneUser(old) && oneUser(shifts):
		return whole(old[0], sale.NetMills, ReasonHolder)
	case oneUser(old):
		return whole(old[0], sale.NetMills, ReasonLastWriter)
	}
	return decide(sale, append(append([]*Work{}, shifts...), visits...), shifts, ix)
}

// oldHolders are the shifts Hub's `settleSale` gives the sale to: each that is alone or whose person wrote last; when
// the last writer is a colleague among them, only theirs; when none of them is, all of them.
func oldHolders(shifts []*Work, lastWriter *int64) []*Work {
	var old []*Work
	for _, w := range shifts {
		others := false
		writerAmong := false
		for _, o := range shifts {
			if o.UserID != w.UserID {
				others = true
				if lastWriter != nil && o.UserID == *lastWriter {
					writerAmong = true
				}
			}
		}
		mine := !others || (lastWriter != nil && *lastWriter == w.UserID)
		if mine || !writerAmong {
			old = append(old, w)
		}
	}
	return old
}

func oneUser(works []*Work) bool {
	for _, w := range works {
		if w.UserID != works[0].UserID {
			return false
		}
	}
	return len(works) > 0
}

// homeOfSender: a chatter's PPV goes to their own work holding it, their shift going then, their next one within
// the wait; till the wait is over it waits. Their work of a day before the rule takes nothing.
func (r Rules) homeOfSender(sale Sale, sender int64, holders []*Work, ix index, now time.Time) Home {
	at := sale.OccurredAt
	theirs := func(w *Work) bool { return w.UserID == sender && !r.beforeRule(*w) }
	for _, w := range holders {
		if theirs(w) {
			return whole(w, sale.NetMills, ReasonPPVSender)
		}
	}
	var next []*Work
	for i := range ix.works {
		w := &ix.works[i]
		if w.Kind == Visit || !theirs(w) {
			continue
		}
		if !w.StartedAt.After(at) && at.Before(w.EndedAt) {
			return whole(w, sale.NetMills, ReasonPPVSenderOtherPage)
		}
		if w.StartedAt.After(at) && !w.StartedAt.After(at.Add(r.Wait)) {
			next = append(next, w)
		}
	}
	if len(next) > 0 {
		sort.SliceStable(next, func(a, b int) bool { return next[a].StartedAt.Before(next[b].StartedAt) })
		return whole(next[0], sale.NetMills, ReasonPPVSenderNextShift)
	}
	until := at.Add(r.Wait)
	if until.After(now) {
		return Home{Kind: Waiting, Reason: ReasonPPVSenderWaiting, WaitsUntil: until}
	}
	return Home{Kind: Nobody, Reason: ReasonPPVSenderNoShift}
}

// kindOf is the home kind of a work: Visit for a look-in, else Shift.
func kindOf(w *Work) string {
	if w.Kind == Visit {
		return Visit
	}
	return Shift
}

// beforeRule: work of a Moscow day before the rule's first day keeps what the old rule gives it, takes nothing new.
func (r Rules) beforeRule(w Work) bool { return dayOfWork(w) < ts.Date(r.Since) }

// DayOfWork is the Moscow day a piece of work is paid on, Hub's `dayOfWork` (chatter-shifts.ts): the day holding most
// of it, the day of its middle instant (a tie: the day it began), as the payout sheet dates it.
func DayOfWork(w Work) string { return dayOfWork(w) }

// dayOfWork is the Moscow day holding most of the work: the day of its middle instant (a tie: the day it began).
func dayOfWork(w Work) string {
	middle := (w.StartedAt.UnixMilli() + w.EndedAt.UnixMilli() - 1) / 2
	return ts.Date(time.UnixMilli(middle))
}

func whole(w *Work, net int64, reason string) Home {
	return Home{Kind: kindOf(w), Reason: reason, Shares: []Share{{WorkRef: w.Ref, UserID: w.UserID, Percent: 100, Mills: net}}}
}
