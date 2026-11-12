package split

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// Synthetic team: chatters 101, 102 and 103, a team lead 201; pages page-a and page-b; times are Moscow, November 2026.

func msk(s string) time.Time {
	t, err := ts.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

func work(kind string, user int64, from, to string, pages ...string) Work {
	start, end := msk(from), msk(to)
	w := Work{
		Ref:    fmt.Sprintf("%d:%s", user, start.UTC().Format("2006-01-02T15:04:05.000Z07:00")),
		UserID: user, Username: fmt.Sprintf("user%d", user), Kind: kind, StartedAt: start, EndedAt: end, Role: RoleChatter,
	}
	for _, p := range pages {
		w.Pages = append(w.Pages, WorkPage{PageLabel: p, From: start})
	}
	return w
}

func shift(user int64, from, to string, pages ...string) Work {
	return work(Shift, user, from, to, pages...)
}
func visit(user int64, from, to string, pages ...string) Work {
	return work(Visit, user, from, to, pages...)
}

func sale(kind, page, at string, net int64, holders ...Work) Sale {
	s := Sale{TransactionRef: "t-" + at, PageLabel: page, Platform: "fansly", FanRef: "fan-1", Kind: kind, NetMills: net, OccurredAt: msk(at)}
	for _, h := range holders {
		s.Holders = append(s.Holders, h.Ref)
	}
	return s
}

func ppvBy(s Sale, user int64, role string) Sale {
	s.PPVSender = &Person{UserID: user, Username: fmt.Sprintf("user%d", user), Role: role}
	return s
}

func ref(v int64) *int64 { return &v }

func whole100(w Work, net int64) []Share {
	return []Share{{WorkRef: w.Ref, UserID: w.UserID, Percent: 100, Mills: net}}
}

func expect(t *testing.T, got Home, kind, reason string, shares []Share) {
	t.Helper()
	if got.Kind != kind || got.Reason != reason || !reflect.DeepEqual(got.Shares, shares) {
		t.Errorf("home = %+v, want kind %s, reason %s, shares %+v", got, kind, reason, shares)
	}
}

// A week after the sales: every PPV's wait is over unless a test says otherwise.
var later = msk("2026-11-20 12:00")

func TestPPVBoughtInAColleaguesShiftGoesToTheSendersShiftOnAnotherPage(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	elsewhere := shift(102, "2026-11-05 12:00", "2026-11-05 18:00", "page-b")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 14:00", 24_000, holder), 102, RoleChatter)
	expect(t, HomeOf(ppv, []Work{holder, elsewhere}, later), Shift, ReasonPPVSenderOtherPage, whole100(elsewhere, 24_000))
}

func TestPPVBoughtBetweenTheSendersShiftsWaitsAndLandsInTheirNextShiftWithinSevenDays(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 16:44", 30_000, holder), 102, RoleChatter)
	now := msk("2026-11-06 12:00")
	got := HomeOf(ppv, []Work{holder}, now)
	expect(t, got, Waiting, ReasonPPVSenderWaiting, nil)
	if !got.WaitsUntil.Equal(msk("2026-11-12 16:44")) {
		t.Errorf("waits until %v", got.WaitsUntil)
	}
	// The sender's shifts: one after the purchase within the week, a later one; the first takes it.
	night := shift(102, "2026-11-06 03:39", "2026-11-06 11:00", "page-a")
	weekLater := shift(102, "2026-11-11 10:00", "2026-11-11 18:00", "page-b")
	expect(t, HomeOf(ppv, []Work{weekLater, holder, night}, now), Shift, ReasonPPVSenderNextShift, whole100(night, 30_000))
	// A shift that begins exactly 7 days after the purchase is still within the wait.
	edge := shift(102, "2026-11-12 16:44", "2026-11-12 22:00", "page-b")
	expect(t, HomeOf(ppv, []Work{holder, edge}, later), Shift, ReasonPPVSenderNextShift, whole100(edge, 30_000))
}

func TestPPVWhoseSendersNextShiftIsNotWithinSevenDaysIsNobodys(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 16:44", 30_000, holder), 102, RoleChatter)
	tooLate := shift(102, "2026-11-12 16:45", "2026-11-12 22:00", "page-a")
	expect(t, HomeOf(ppv, []Work{holder, tooLate}, msk("2026-11-12 16:44")), Nobody, ReasonPPVSenderNoShift, nil)
	expect(t, HomeOf(ppv, []Work{holder}, msk("2026-11-12 16:43")), Waiting, ReasonPPVSenderWaiting, nil)
}

func TestTheSendersOwnShiftHoldingTheirPPVKeepsIt(t *testing.T) {
	mine := shift(102, "2026-11-05 10:00", "2026-11-05 18:00", "page-a")
	colleague := shift(101, "2026-11-05 12:00", "2026-11-05 20:00", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 14:00", 20_000, colleague, mine), 102, RoleChatter)
	ppv.LastWriter = ref(101)
	expect(t, HomeOf(ppv, []Work{colleague, mine}, later), Shift, ReasonPPVSender, whole100(mine, 20_000))
}

func TestTheSendersOwnLookInKeepsTheirPPV(t *testing.T) {
	look := visit(102, "2026-11-05 12:00", "2026-11-05 12:50", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 12:30", 50_000, look), 102, RoleChatter)
	// Their shift later that day does not take it: the look-in holding it does.
	afternoon := shift(102, "2026-11-05 15:00", "2026-11-05 22:00", "page-a")
	expect(t, HomeOf(ppv, []Work{look, afternoon}, later), Visit, ReasonPPVSender, whole100(look, 50_000))
}

func TestPPVOfATeamLeadOrWithoutAuthorStaysWithTheShiftThatHoldsIt(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	lead := shift(201, "2026-11-05 13:00", "2026-11-05 20:00", "page-b")
	byLead := ppvBy(sale(SalePPV, "page-a", "2026-11-05 14:00", 20_000, holder), 201, "team_lead")
	expect(t, HomeOf(byLead, []Work{holder, lead}, later), Shift, ReasonHolder, whole100(holder, 20_000))
	noAuthor := sale(SalePPV, "page-a", "2026-11-05 14:10", 20_000, holder)
	expect(t, HomeOf(noAuthor, []Work{holder, lead}, later), Shift, ReasonHolder, whole100(holder, 20_000))
}

func TestPostTipAndCampaignGoToThePotWhoeverHeldThePage(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	postTip := sale(SaleTip, "page-a", "2026-11-05 14:00", 300_000, holder)
	postTip.PostTip = true
	campaign := sale(SaleTip, "page-a", "2026-11-05 15:00", 100_000, holder)
	campaign.Platform, campaign.Campaign = "onlyfans", true
	homes := HomesOf([]Sale{postTip, campaign}, []Work{holder}, later)
	want := []Home{{Kind: Pot, Pot: PotObshak, Reason: ReasonPostTip}, {Kind: Pot, Pot: PotObshak, Reason: ReasonCampaign}}
	if !reflect.DeepEqual(homes, want) {
		t.Errorf("homes = %+v, want %+v", homes, want)
	}
}

func TestARenewalAndAChatTipStayWithTheOneShiftThatHoldsThemWhoeverWroteLast(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	renewal := sale(SaleSubscription, "page-a", "2026-11-05 14:00", 40_000, holder)
	renewal.LastWriter = ref(102)
	tip := sale(SaleTip, "page-a", "2026-11-05 15:00", 10_000, holder)
	homes := HomesOf([]Sale{renewal, tip}, []Work{holder}, later)
	expect(t, homes[0], Shift, ReasonHolder, whole100(holder, 40_000))
	expect(t, homes[1], Shift, ReasonHolder, whole100(holder, 10_000))
}

func TestTwoHoldersGiveTheSaleToTheLastWriter(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 30_000, a, b)
	s.LastWriter = ref(102)
	expect(t, HomeOf(s, []Work{a, b}, later), Shift, ReasonLastWriter, whole100(b, 30_000))
}

func TestTwoHoldersNoneOfThemLastWaitForADecision(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 30_000, a, b)
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonNoLastWriter, nil)
	s.LastWriter = ref(103) // a third chatter wrote last: still neither of theirs
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonNoLastWriter, nil)
}

func TestADecisionTheOwnerSplitGoesByHisPercentsTheLastTakingTheRounding(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 1_001, a, b)
	s.Resolution = []OwnerShare{{UserID: 102, Percent: 33}, {UserID: 101, Percent: 67}}
	// The owner's split wins over the marks.
	s.Marks = []Mark{{UserID: 101, Mark: MarkMine, Confirmed: true}}
	expect(t, HomeOf(s, []Work{a, b}, later), Shift, ReasonOwnerSplit, []Share{
		{WorkRef: b.Ref, UserID: 102, Percent: 33, Mills: 330},
		{WorkRef: a.Ref, UserID: 101, Percent: 67, Mills: 671},
	})
}

func TestADecisionOfTwoMutualSplitMarksIsHalvesTheOddMillToTheLowerUser(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 30_001, a, b)
	s.Marks = []Mark{
		{UserID: 102, Mark: MarkSplit, ShareWith: ref(101), Confirmed: true},
		{UserID: 101, Mark: MarkSplit, ShareWith: ref(102)},
	}
	expect(t, HomeOf(s, []Work{a, b}, later), Shift, ReasonMarks, []Share{
		{WorkRef: a.Ref, UserID: 101, Percent: 50, Mills: 15_001},
		{WorkRef: b.Ref, UserID: 102, Percent: 50, Mills: 15_000},
	})
	// Neither report confirmed: the marks are a draft, it still waits.
	s.Marks[0].Confirmed = false
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonNoLastWriter, nil)
}

func TestADecisionOfOneMineMarkIsThatChattersOrOnlyTheirOwnAmount(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 40_000, a, b)
	s.Marks = []Mark{{UserID: 101, Mark: MarkNotMine, Confirmed: true}, {UserID: 102, Mark: MarkMine, Confirmed: true}}
	expect(t, HomeOf(s, []Work{a, b}, later), Shift, ReasonMarks, whole100(b, 40_000))
	// An own amount below the net keeps only that; the rest is nobody's.
	s.Marks[1].OwnAmountMills = ref(10_000)
	expect(t, HomeOf(s, []Work{a, b}, later), Shift, ReasonMarks, []Share{{WorkRef: b.Ref, UserID: 102, Percent: 25, Mills: 10_000}})
	// One above the net claims the net, never more.
	s.Marks[1].OwnAmountMills = ref(90_000)
	expect(t, HomeOf(s, []Work{a, b}, later), Shift, ReasonMarks, whole100(b, 40_000))
	// A «моё» of a report not confirmed decides nothing yet.
	s.Marks[1].Confirmed = false
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonNoLastWriter, nil)
}

func TestMarksThatDisagreeWaitForTheOwner(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 40_000, a, b)
	s.Marks = []Mark{{UserID: 101, Mark: MarkMine, Confirmed: true}, {UserID: 102, Mark: MarkMine, Confirmed: true}}
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonMarksDisagree, nil)
	// A «делим» the colleague did not answer with theirs.
	s.Marks = []Mark{{UserID: 101, Mark: MarkSplit, ShareWith: ref(102), Confirmed: true}, {UserID: 102, Mark: MarkNotMine, Confirmed: true}}
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonMarksDisagree, nil)
}

func TestEveryHolderConfirmingNotMineMakesTheSaleNobodys(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 40_000, a, b)
	s.Marks = []Mark{{UserID: 101, Mark: MarkNotMine, Confirmed: true}}
	expect(t, HomeOf(s, []Work{a, b}, later), Decide, ReasonNoLastWriter, nil)
	s.Marks = append(s.Marks, Mark{UserID: 102, Mark: MarkNotMine, Confirmed: true})
	expect(t, HomeOf(s, []Work{a, b}, later), Nobody, ReasonMarks, nil)
}

func TestNoShiftHoldingTheSaleGoesToTheLookInWhosePersonLastWroteElseNobody(t *testing.T) {
	look := visit(102, "2026-11-05 11:00", "2026-11-05 12:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:30", 40_000, look)
	s.LastWriter = ref(102)
	expect(t, HomeOf(s, []Work{look}, later), Visit, ReasonLookInLastWriter, whole100(look, 40_000))
	s.LastWriter = ref(101)
	expect(t, HomeOf(s, []Work{look}, later), Nobody, ReasonNoHolder, nil)
	// A look-in beside a shift holding the sale takes nothing.
	holder := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	s = sale(SaleTip, "page-a", "2026-11-05 11:30", 40_000, holder, look)
	s.LastWriter = ref(102)
	expect(t, HomeOf(s, []Work{holder, look}, later), Shift, ReasonHolder, whole100(holder, 40_000))
}

func TestASaleBefore2NovemberIsBeforeTheRule(t *testing.T) {
	holder := shift(101, "2026-11-01 11:00", "2026-11-01 16:30", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-01 16:00", 30_000, holder), 102, RoleChatter)
	expect(t, HomeOf(ppv, []Work{holder}, later), BeforeRule, ReasonSaleBeforeRule, nil)
	tip := sale(SaleTip, "page-a", "2026-11-01 23:59", 5_000)
	tip.PostTip = true
	expect(t, HomeOf(tip, nil, later), BeforeRule, ReasonSaleBeforeRule, nil)
}

func TestTheNightShiftOf1NovemberKeepsTheOldRulePastMidnight(t *testing.T) {
	// 19:00 1 Nov – 01:30 2 Nov: a shift of 1 November. Its sales after midnight stay the old rule's, a PPV too.
	night := shift(101, "2026-11-01 19:00", "2026-11-02 01:30", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-02 00:20", 12_000, night), 102, RoleChatter)
	sender := shift(102, "2026-11-02 10:00", "2026-11-02 18:00", "page-a")
	expect(t, HomeOf(ppv, []Work{night, sender}, later), BeforeRule, ReasonWorkBeforeRule, nil)
	// Beside a shift of 2 November whose person wrote last, the rule settles it.
	morning := shift(103, "2026-11-02 00:00", "2026-11-02 08:00", "page-a")
	tip := sale(SaleTip, "page-a", "2026-11-02 00:30", 8_000, night, morning)
	tip.LastWriter = ref(103)
	expect(t, HomeOf(tip, []Work{night, morning}, later), Shift, ReasonLastWriter, whole100(morning, 8_000))
	tip.LastWriter = nil
	expect(t, HomeOf(tip, []Work{night, morning}, later), BeforeRule, ReasonWorkBeforeRule, nil)
}

func TestTheSendersShiftOf1NovemberTakesNoPPVOfTheRule(t *testing.T) {
	// The sender's night of 1 November is going at the purchase on page-a; the PPV on page-b waits for their next shift.
	night := shift(102, "2026-11-01 19:00", "2026-11-02 01:30", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-b", "2026-11-02 00:20", 12_000), 102, RoleChatter)
	next := shift(102, "2026-11-03 10:00", "2026-11-03 18:00", "page-a")
	expect(t, HomeOf(ppv, []Work{night, next}, later), Shift, ReasonPPVSenderNextShift, whole100(next, 12_000))
}

func TestAHolderMissingFromTheWorksIsUnknown(t *testing.T) {
	holder := shift(101, "2026-11-05 13:00", "2026-11-05 20:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 14:00", 10_000, holder)
	expect(t, HomeOf(s, nil, later), Unknown, ReasonHolderNotInWorks, nil)
}

// Hub's decision of a sale (Sale.Decision) comes before the rule, as Hub's own rule puts the owner's decision first.

func TestHubsNobodyAndPotDecisionsComeBeforeTheRule(t *testing.T) {
	holder := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-05 11:00", 30_000, holder)
	expect(t, HomeOf(s, []Work{holder}, later), Shift, ReasonHolder, whole100(holder, 30_000))
	s.Decision = &Decision{Kind: DecisionNobody}
	expect(t, HomeOf(s, []Work{holder}, later), Nobody, ReasonOwnerNobody, nil)
	s.Decision = &Decision{Kind: DecisionPot, Pot: "spring"}
	got := HomeOf(s, []Work{holder}, later)
	expect(t, got, Pot, ReasonOwnerPot, nil)
	if got.Pot != "spring" {
		t.Errorf("pot %q, want spring", got.Pot)
	}
	// A pot decision that names no pot gives no home: the rule's stands.
	s.Decision = &Decision{Kind: DecisionPot}
	expect(t, HomeOf(s, []Work{holder}, later), Shift, ReasonHolder, whole100(holder, 30_000))
	// «Никому» wins over a claim; a PPV waiting for its sender's shift is decided too.
	s.Decision, s.ClaimedBy = &Decision{Kind: DecisionNobody}, ref(102)
	expect(t, HomeOf(s, []Work{holder}, later), Nobody, ReasonOwnerNobody, nil)
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 20:00", 12_000), 102, RoleChatter)
	ppv.Decision = &Decision{Kind: DecisionPot, Pot: "spring"}
	expect(t, HomeOf(ppv, nil, msk("2026-11-06 12:00")), Pot, ReasonOwnerPot, nil)
}

func TestHubsDecisionGivesTheSaleToPeopleByItsShares(t *testing.T) {
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 09:00", "2026-11-05 13:00", "page-b") // works, but does not hold page-a
	look := visit(103, "2026-11-05 10:00", "2026-11-05 10:20", "page-a")
	works := []Work{a, b, look}
	s := sale(SaleTip, "page-a", "2026-11-05 10:10", 1_001, a, look)
	s.Decision = &Decision{Kind: DecisionSplit}
	// The whole sale to a person who held nothing: on no work.
	s.Resolution = []OwnerShare{{UserID: 102, Percent: 100}}
	expect(t, HomeOf(s, works, later), Shift, ReasonOwnerSplit, []Share{{UserID: 102, Percent: 100, Mills: 1_001}})
	// Shares: the rule's own person on the work the rule names, a look-in's person on the look-in, the last takes the rounding.
	s.Resolution = []OwnerShare{{UserID: 103, Percent: 33}, {UserID: 101, Percent: 67}}
	expect(t, HomeOf(s, works, later), Shift, ReasonOwnerSplit, []Share{
		{WorkRef: look.Ref, UserID: 103, Percent: 33, Mills: 330},
		{WorkRef: a.Ref, UserID: 101, Percent: 67, Mills: 671},
	})
	// Every share a look-in's: a look-in's home.
	s.Resolution = []OwnerShare{{UserID: 103, Percent: 100}}
	expect(t, HomeOf(s, works, later), Visit, ReasonOwnerSplit, whole100(look, 1_001))
	// A PPV its sender's next shift takes by the rule, given to the sender: that shift.
	next := shift(102, "2026-11-06 09:00", "2026-11-06 13:00", "page-a")
	ppv := ppvBy(sale(SalePPV, "page-a", "2026-11-05 20:00", 12_000), 102, RoleChatter)
	ppv.Decision, ppv.Resolution = &Decision{Kind: DecisionSplit}, []OwnerShare{{UserID: 102, Percent: 100}}
	expect(t, HomeOf(ppv, []Work{next}, later), Shift, ReasonOwnerSplit, whole100(next, 12_000))
	// A decision with no shares gives no home.
	s.Resolution = nil
	expect(t, HomeOf(s, works, later), Shift, ReasonHolder, whole100(a, 1_001))
}

func TestANobodysSaleHubGivesToAPersonIsTheirsAsAClaim(t *testing.T) {
	s := sale(SaleTip, "page-a", "2026-11-05 03:00", 50_000)
	expect(t, HomeOf(s, nil, later), Nobody, ReasonNoHolder, nil)
	s.Decision, s.Resolution = &Decision{Kind: DecisionSplit}, []OwnerShare{{UserID: 102, Percent: 100}}
	expect(t, HomeOf(s, nil, later), Shift, ReasonOwnerClaim, []Share{{UserID: 102, Percent: 100, Mills: 50_000}})
	// Shares of a nobody's sale are the owner's split.
	s.Resolution = []OwnerShare{{UserID: 101, Percent: 50}, {UserID: 102, Percent: 50}}
	expect(t, HomeOf(s, nil, later), Shift, ReasonOwnerSplit, []Share{{UserID: 101, Percent: 50, Mills: 25_000}, {UserID: 102, Percent: 50, Mills: 25_000}})
}

func TestHubsDecisionLeavesASaleBeforeTheRuleAndAJunctionAlreadySplit(t *testing.T) {
	// Before the rule the payout sheets pay: no decision moves it.
	old := shift(101, "2026-11-01 11:00", "2026-11-01 16:30", "page-a")
	s := sale(SaleTip, "page-a", "2026-11-01 16:00", 30_000, old)
	s.Decision = &Decision{Kind: DecisionNobody}
	expect(t, HomeOf(s, []Work{old}, later), BeforeRule, ReasonSaleBeforeRule, nil)
	// A junction the shares already split is the same home with or without the decision beside them.
	a := shift(101, "2026-11-05 09:00", "2026-11-05 13:00", "page-a")
	b := shift(102, "2026-11-05 10:30", "2026-11-05 14:00", "page-a")
	j := sale(SaleTip, "page-a", "2026-11-05 11:00", 1_001, a, b)
	j.Resolution = []OwnerShare{{UserID: 102, Percent: 33}, {UserID: 101, Percent: 67}}
	want := []Share{{WorkRef: b.Ref, UserID: 102, Percent: 33, Mills: 330}, {WorkRef: a.Ref, UserID: 101, Percent: 67, Mills: 671}}
	expect(t, HomeOf(j, []Work{a, b}, later), Shift, ReasonOwnerSplit, want)
	j.Decision = &Decision{Kind: DecisionSplit}
	expect(t, HomeOf(j, []Work{a, b}, later), Shift, ReasonOwnerSplit, want)
}
