package accruals

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/decisions"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/split"
)

// Hub's team facts carry its active decision about whose a sale is (hub#1279): `resolution` — the shares of a decision
// that gives the sale to people, a nobody's sale given to a person included — and `decision` {kind, pot}: split, nobody
// («никому») or pot. The facts rule reads them for the sales of settings.sale_decisions_hub_from and later; a sale of a
// day before it, and every sale while the setting is empty, goes by ledger's own rows as before.

// decided marks a sale of the mirror with Hub's decision, as Hub's answer carries it.
func decided(t *testing.T, s *db.Store, ref, kind string, pot any, shares ...hub.TeamShare) {
	t.Helper()
	resolution := "null"
	if len(shares) > 0 {
		var parts []string
		for _, sh := range shares {
			parts = append(parts, `{"userId": `+itoa(sh.UserID)+`, "percent": `+itoa(int64(sh.Percent))+`}`)
		}
		resolution = "[" + strings.Join(parts, ", ") + "]"
	}
	p := "null"
	if pot != nil {
		p = `"` + pot.(string) + `"`
	}
	exec(t, s, `UPDATE hub_sales SET payload = json_set(payload, '$.resolution', json(?), '$.decision', json(?)) WHERE transaction_ref = ?`,
		resolution, `{"kind": "`+kind+`", "pot": `+p+`}`, ref)
	if n := count(t, s, `SELECT count(*) FROM hub_sales WHERE transaction_ref = ? AND json_extract(payload, '$.decision.kind') = ?`, ref, kind); n != 1 {
		t.Fatalf("sale %s is not in the mirror", ref)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// homes renders fact_homes of the sales named: the sale, the share's number, where it went and why, whose, how much, the day.
func homes(t *testing.T, s *db.Store, refs ...string) string {
	t.Helper()
	list := "'" + strings.Join(refs, "', '") + "'"
	return cols(t, s, `SELECT transaction_ref, seq, home, reason, COALESCE(pot, '-'), COALESCE(person, username, '-'), share_micros, day
		FROM fact_homes WHERE transaction_ref IN (`+list+`) ORDER BY CAST(transaction_ref AS INTEGER), seq`)
}

// decisionWeek is the week of 9 November: Альфа held page-a on the 10th and the 12th, Браво page-b on the 12th, user 103
// page-a on the 10th beside Альфа. The sales of the 10th are before the switch day of the tests (the 11th), those of the
// 12th after it.
func decisionWeek(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := open(t)
	tenth := work(101, split.Shift, "2026-11-10 10:00", "2026-11-10 18:00", "page-a")
	beside := work(103, split.Shift, "2026-11-10 14:00", "2026-11-10 22:00", "page-a")
	twelfth := work(101, split.Shift, "2026-11-12 10:00", "2026-11-12 18:00", "page-a")
	bravo := work(102, split.Shift, "2026-11-12 10:00", "2026-11-12 18:00", "page-b")
	old := sale("18", split.SaleTip, "page-a", "2026-11-10 16:00", 10_000, tenth, beside)
	old.Resolution = []hub.TeamShare{{UserID: 101, Percent: 100}} // the old shift-report table's split: no decision beside it
	putWindow(t, s, "2026-11-02", nil, nil)
	putWindow(t, s, "2026-11-09", []hub.TeamWork{tenth, beside, twelfth, bravo}, []hub.TeamSale{
		sale("10", split.SaleTip, "page-a", "2026-11-10 11:00", 100_000, tenth),
		sale("16", split.SaleTip, "page-a", "2026-11-10 23:30", 20_000),
		sale("17", split.SaleTip, "page-a", "2026-11-10 15:00", 16_000, tenth, beside),
		old,
		sale("11", split.SaleTip, "page-a", "2026-11-12 11:00", 50_000, twelfth),
		sale("12", split.SaleTip, "page-a", "2026-11-12 12:00", 60_000, twelfth),
		sale("13", split.SaleTip, "page-a", "2026-11-12 13:00", 40_000, twelfth),
		sale("14", split.SaleTip, "page-a", "2026-11-12 19:00", 30_000),
		sale("15", split.SaleTip, "page-a", "2026-11-12 14:00", 70_001, twelfth),
		sale("19", split.SaleTip, "page-a", "2026-11-12 15:00", 5_000, twelfth),
	})
	putWindow(t, s, "2026-11-16", nil, nil)
	// Hub's decisions, on both sides of the switch day.
	decided(t, s, "10", "nobody", nil)                                          // before the day: «никому»
	decided(t, s, "16", "split", nil, hub.TeamShare{UserID: 102, Percent: 100}) // before the day: a nobody's sale given to Браво
	decided(t, s, "17", "split", nil, hub.TeamShare{UserID: 103, Percent: 100}) // before the day: a junction decided
	decided(t, s, "11", "nobody", nil)
	decided(t, s, "12", "pot", "spring")
	decided(t, s, "13", "split", nil, hub.TeamShare{UserID: 102, Percent: 100}) // Альфа's by the rule, the owner gives it to Браво
	decided(t, s, "14", "split", nil, hub.TeamShare{UserID: 102, Percent: 100}) // nobody's, given to Браво
	decided(t, s, "15", "split", nil, hub.TeamShare{UserID: 101, Percent: 60}, hub.TeamShare{UserID: 102, Percent: 40})
	return s, ctx
}

// byTheRule is what the rule and ledger's own rows give the sales of decisionWeek: Hub's decisions change nothing.
const byTheRule = `10 0 shift holder - alfa 100000000 2026-11-10
11 0 shift holder - alfa 50000000 2026-11-12
12 0 shift holder - alfa 60000000 2026-11-12
13 0 shift holder - alfa 40000000 2026-11-12
14 0 nobody no_holder - - 30000000 2026-11-12
15 0 shift holder - alfa 70001000 2026-11-12
16 0 nobody no_holder - - 20000000 2026-11-10
17 0 decide no_last_writer - - 16000000 2026-11-10
18 0 shift owner_split - alfa 10000000 2026-11-10
19 0 shift holder - alfa 5000000 2026-11-12`

var decidedRefs = []string{"10", "11", "12", "13", "14", "15", "16", "17", "18", "19"}

func TestHubsDecisionsChangeNoFactWhileTheSettingIsEmpty(t *testing.T) {
	s, ctx := decisionWeek(t)
	rebuild(t, s, ctx)
	if got := homes(t, s, decidedRefs...); got != byTheRule {
		t.Fatalf("Hub's decisions moved the facts with the setting empty:\n%s\nwant\n%s", got, byTheRule)
	}
}

func TestFromTheSwitchDayHubsDecisionRulesASalesHome(t *testing.T) {
	s, ctx := decisionWeek(t)
	rebuild(t, s, ctx)
	// Ledger's own word of before the switch: the nobody's sale 16 is Альфа's.
	if _, err := Claim(ctx, s, ClaimInput{Ref: "16", Person: "alfa", Note: "слово владельца"}, "test", claimNow); err != nil {
		t.Fatal(err)
	}
	if err := decisions.SetHubFrom(ctx, s.RW, "2026-11-11", claimNow); err != nil {
		t.Fatal(err)
	}
	rep := rebuild(t, s, ctx)
	want := strings.Join([]string{
		// Before the day ledger's own rows rule as they did: Hub's «никому» and its shares are not read, the claim stands.
		"10 0 shift holder - alfa 100000000 2026-11-10",
		// From the day: «никому» is nobody's, a pot decision is that pot's.
		"11 0 nobody owner_nobody - - 50000000 2026-11-12",
		"12 0 pot owner_pot spring - 60000000 2026-11-12",
		// The owner's decision gives Альфа's sale to Браво, who held another page: on no work, the sale's day.
		"13 0 shift owner_split - bravo 40000000 2026-11-12",
		// A nobody's sale given to a person, as a claim was.
		"14 0 shift owner_claim - bravo 30000000 2026-11-12",
		// Shares: by the percents, the last one takes the rounding; Альфа's on her shift.
		"15 0 shift owner_split - alfa 42001000 2026-11-12",
		"15 1 shift owner_split - bravo 28000000 2026-11-12",
		"16 0 shift owner_claim - alfa 20000000 2026-11-10",
		"17 0 decide no_last_writer - - 16000000 2026-11-10",
		"18 0 shift owner_split - alfa 10000000 2026-11-10",
		"19 0 shift holder - alfa 5000000 2026-11-12",
	}, "\n")
	if got := homes(t, s, decidedRefs...); got != want {
		t.Fatalf("the facts from the switch day:\n%s\nwant\n%s", got, want)
	}
	if rep.Claims != 1 || len(rep.ClaimsIgnored) != 0 {
		t.Fatalf("ledger's claim of before the day: %+v", rep)
	}
	// Браво's shares are sales of his day on the page: 40 + 30 + 28.
	if got := cols(t, s, `SELECT person, page_label, day, sales, net_micros FROM fact_accruals WHERE hub_user_id = 102 AND kind = 'sales'`); got != "bravo page-a 2026-11-12 3 98000000" {
		t.Fatalf("Браво's day: %s", got)
	}
	// Off again: the rule and ledger's rows alone.
	if err := decisions.SetHubFrom(ctx, s.RW, "", claimNow); err != nil {
		t.Fatal(err)
	}
	rebuild(t, s, ctx)
	back := strings.Replace(byTheRule, "16 0 nobody no_holder - - 20000000 2026-11-10", "16 0 shift owner_claim - alfa 20000000 2026-11-10", 1)
	if got := homes(t, s, decidedRefs...); got != back {
		t.Fatalf("after the switch is off:\n%s\nwant\n%s", got, back)
	}
}
