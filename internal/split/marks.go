package split

// decide: several shifts hold the sale and none of their people wrote last to the fan. The owner's split wins; else
// the chatters' confirmed marks, when they agree (Hub's `judgeMarks` and `dueCreditsOf`); else it waits for a
// decision. `holders` — every work holding it, shifts first; `shifts` — the shifts among them.
func decide(sale Sale, holders, shifts []*Work, ix index) Home {
	net := sale.NetMills
	if len(sale.Resolution) > 0 {
		shares := ownerShares(net, sale.Resolution, func(user int64) string { return workRefOf(user, holders) })
		return sharesHome(shares, ReasonOwnerSplit, ix)
	}
	settled := judgeMarks(sale.Marks)
	switch settled.kind {
	case MarkMine:
		u := settled.users[0]
		m := markOf(sale.Marks, u)
		if !m.Confirmed {
			break
		}
		mills := net // «моё» claims the net, or the chatter's own amount, never above it
		if m.OwnAmountMills != nil && *m.OwnAmountMills < net {
			mills = *m.OwnAmountMills
		}
		return sharesHome([]Share{{WorkRef: workRefOf(u, holders), UserID: u, Percent: percentOfNet(mills, net), Mills: mills}}, ReasonMarks, ix)
	case MarkSplit:
		a, b := settled.users[0], settled.users[1]
		if !markOf(sale.Marks, a).Confirmed && !markOf(sale.Marks, b).Confirmed {
			break
		}
		low, high := a, b
		if high < low {
			low, high = high, low
		}
		first := percentOf(net, 50) // the odd mill to the lower user id
		return sharesHome([]Share{
			{WorkRef: workRefOf(low, holders), UserID: low, Percent: 50, Mills: first},
			{WorkRef: workRefOf(high, holders), UserID: high, Percent: 50, Mills: net - first},
		}, ReasonMarks, ix)
	case MarkNotMine:
		if len(sale.Marks) > 0 && everyoneSaysNotMine(sale.Marks, shifts) {
			return Home{Kind: Nobody, Reason: ReasonMarks}
		}
	default:
		return Home{Kind: Decide, Reason: ReasonMarksDisagree}
	}
	return Home{Kind: Decide, Reason: ReasonNoLastWriter}
}

// ownerShares is the owner's split as shares of the net (Hub's `splitShares`): in the order given, the last share takes
// the rounding, so the shares sum to the net exactly. workRefOf names each person's work.
func ownerShares(net int64, resolution []OwnerShare, workRefOf func(user int64) string) []Share {
	shares := make([]Share, len(resolution))
	var given int64
	for i, s := range resolution {
		mills := percentOf(net, s.Percent)
		if i == len(resolution)-1 {
			mills = net - given
		}
		given += mills
		shares[i] = Share{WorkRef: workRefOf(s.UserID), UserID: s.UserID, Percent: s.Percent, Mills: mills}
	}
	return shares
}

// settlement is what the marks agree on: MarkMine (one user), MarkSplit (two), MarkNotMine (nobody claims it), or ""
// — a dispute.
type settlement struct {
	kind  string
	users []int64
}

// judgeMarks is Hub's: the marks agree only as one «моё» or as two «делим» naming each other («не моё» beside them is
// fine); no claim at all — nobody's; anything else is a dispute the owner splits.
func judgeMarks(marks []Mark) settlement {
	var claims []Mark
	for _, m := range marks {
		if m.Mark != MarkNotMine {
			claims = append(claims, m)
		}
	}
	if len(claims) == 0 {
		return settlement{kind: MarkNotMine}
	}
	if len(claims) == 1 && claims[0].Mark == MarkMine {
		return settlement{kind: MarkMine, users: []int64{claims[0].UserID}}
	}
	if len(claims) == 2 {
		a, b := claims[0], claims[1]
		if a.Mark == MarkSplit && b.Mark == MarkSplit && a.ShareWith != nil && *a.ShareWith == b.UserID &&
			b.ShareWith != nil && *b.ShareWith == a.UserID {
			return settlement{kind: MarkSplit, users: []int64{a.UserID, b.UserID}}
		}
	}
	return settlement{}
}

func markOf(marks []Mark, user int64) Mark {
	for _, m := range marks {
		if m.UserID == user {
			return m
		}
	}
	return Mark{}
}

// everyoneSaysNotMine: every person whose shift holds the sale confirmed «не моё».
func everyoneSaysNotMine(marks []Mark, shifts []*Work) bool {
	for _, w := range shifts {
		m := markOf(marks, w.UserID)
		if m.UserID != w.UserID || m.Mark != MarkNotMine || !m.Confirmed {
			return false
		}
	}
	return true
}

// workRefOf is the user's work holding the sale (a shift before a look-in), or "" — none holds it.
func workRefOf(user int64, holders []*Work) string {
	for _, w := range holders {
		if w.UserID == user {
			return w.Ref
		}
	}
	return ""
}

// sharesHome is a home of shares: Visit when every share is a look-in's, else Shift.
func sharesHome(shares []Share, reason string, ix index) Home {
	kind := Visit
	for _, s := range shares {
		i, ok := ix.byRef[s.WorkRef]
		if !ok || ix.works[i].Kind != Visit {
			kind = Shift
		}
	}
	return Home{Kind: kind, Shares: shares, Reason: reason}
}

// percentOf is Hub's `millsPercentOf`: the percent of an amount, rounded half away from zero.
func percentOf(v int64, percent int) int64 {
	x := v * int64(percent)
	if x < 0 {
		return -((-x + 50) / 100)
	}
	return (x + 50) / 100
}

// percentOfNet is the whole percent of the net an amount is, rounded down; all of it — 100.
func percentOfNet(mills, net int64) int {
	if net == 0 || mills == net {
		return 100
	}
	return int(mills * 100 / net)
}
