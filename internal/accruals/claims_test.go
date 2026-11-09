package accruals

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/client"
	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/hub"
	"github.com/goslingmanagment/ledger/internal/payroll"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/split"
	"github.com/goslingmanagment/ledger/internal/ts"
)

var claimNow = at("2026-11-17T12:00:00+03:00")

// putSale adds a sale to a proven window of the mirror.
func putSale(t *testing.T, s *db.Store, window string, x hub.TeamSale) {
	t.Helper()
	b, _ := json.Marshal(x)
	exec(t, s, `INSERT INTO hub_sales(transaction_ref, window_from, page_label, platform, kind, net_mills, occurred_at, post_tip,
		campaign, payload, first_seen_at, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, '2026-11-24', '2026-11-24')`,
		x.TransactionRef, ts.Format(at(window)), x.PageLabel, x.Platform, x.Kind, *x.NetMills, ts.FormatMilli(x.OccurredAt), string(b))
}

// nobodysWeek is the week of 9 November with two sales nobody's by the rule: «6», a tip on page-a at 03:00 of the
// 11th that no shift held; «7», Альфа's PPV of the 14th with no shift of hers within the 7 days after.
func nobodysWeek(t *testing.T) (*db.Store, context.Context) {
	t.Helper()
	s, ctx := open(t)
	week(t, s)
	putSale(t, s, "2026-11-09", sale("6", split.SaleTip, "page-a", "2026-11-11 03:00", 50_000))
	ppv := sale("7", split.SalePPV, "page-b", "2026-11-14 12:00", 8_000)
	ppv.PPVSender = &hub.TeamPerson{UserID: 101, Username: "Alfa", Role: split.RoleChatter}
	putSale(t, s, "2026-11-09", ppv)
	sheets(t, s, ctx, map[string][][]payroll.Cell{
		"Альфа": tab("2026-11-09", 0.25, 0, [7]any{nil, 100, nil, nil, nil, nil, nil}),
		"Браво": tab("2026-11-09", 0.2, 10, [7]any{nil, 0, nil, 40, nil, nil, nil}),
	})
	rebuild(t, s, ctx)
	for ref, reason := range map[string]string{"6": split.ReasonNoHolder, "7": split.ReasonPPVSenderNoShift} {
		if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = ? AND home = 'nobody' AND reason = ?`, ref, reason); n != 1 {
			t.Fatalf("sale %s is not nobody's by %s", ref, reason)
		}
	}
	return s, ctx
}

func TestAClaimGivesANobodysSaleToThePersonOnItsDayAtTheirRate(t *testing.T) {
	s, ctx := nobodysWeek(t)
	res, err := Claim(ctx, s, ClaimInput{Ref: "6", Person: "bravo", Note: "Браво писала фану, по слову владельца"}, "test", claimNow)
	if err != nil {
		t.Fatal(err)
	}
	// Браво (hub:102) gets the whole $50 on the sale's Moscow day, the 11th — not her shift's — at the 20% of her tab.
	if res.Rate == nil || *res.Rate != 0.2 || len(res.Sale) != 1 || *res.Sale[0].AmountMicros != 10_000_000 || res.Sale[0].To != "Браво" ||
		res.Sale[0].Day != "2026-11-11" {
		t.Fatalf("result %+v", res)
	}
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '6' AND home = 'shift' AND reason = 'owner_claim'
		AND person = 'bravo' AND hub_user_id = 102 AND work_ref IS NULL AND day = '2026-11-11' AND share_micros = 50000000`); n != 1 {
		t.Fatal("the claimed sale's row")
	}
	if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE person = 'bravo' AND day = '2026-11-11' AND page_label = 'page-a'
		AND kind = 'sales' AND net_micros = 50000000 AND rate = 0.2 AND amount_micros = 10000000`); n != 1 {
		t.Fatal("Браво's accrual of the 11th")
	}
	// The diff lists it under Браво with the owner's word, not under «Ничей».
	d, err := WeekDiff(ctx, s.RO, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	bravo := d.People[1]
	if bravo.Person != "bravo" || *bravo.FactsMicros != 38_000_000 || len(bravo.Claimed) != 1 || bravo.Claimed[0].Ref != "6" ||
		*bravo.Claimed[0].AmountMicros != 10_000_000 || !strings.Contains(bravo.Claimed[0].Note, "по слову владельца") ||
		bravo.Claimed[0].Reason != ReasonText(split.ReasonOwnerClaim, "") {
		t.Fatalf("Браво %+v", bravo)
	}
	if len(bravo.Sales) != 1 || bravo.Sales[0].Ref != "6" || bravo.Sales[0].Sign != "+" {
		t.Fatalf("the claimed sale makes Браво's difference: %+v", bravo.Sales)
	}
	for _, l := range d.Nobody.Sales {
		if l.Ref == "6" {
			t.Fatal("the claimed sale is still nobody's")
		}
	}
	if d.Nobody.Micros != 8_000_000 {
		t.Fatalf("nobody's %d, want the PPV alone", d.Nobody.Micros)
	}
	// A rebuild over the same facts and claims changes nothing.
	if again := rebuild(t, s, ctx); again.Rewritten != 0 || again.Claims != 1 || len(again.ClaimsIgnored) != 0 {
		t.Fatalf("rebuild after the claim %+v", again)
	}

	// A PPV whose sender had no shift in the wait can be claimed too; Альфа is found by her login, at her 25%.
	if res, err = Claim(ctx, s, ClaimInput{Ref: "7", Person: "alfa", Note: "по слову владельца"}, "test", claimNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE person = 'alfa' AND hub_user_id = 101 AND day = '2026-11-14'
		AND page_label = 'page-b' AND amount_micros = 2000000`); n != 1 || *res.Sale[0].AmountMicros != 2_000_000 {
		t.Fatal("Альфа's claimed PPV")
	}
}

func TestUnclaimReturnsTheSaleToNobodyAndKeepsTheHistory(t *testing.T) {
	s, ctx := nobodysWeek(t)
	if _, err := Claim(ctx, s, ClaimInput{Ref: "6", Person: "bravo", Note: "по слову владельца"}, "test", claimNow); err != nil {
		t.Fatal(err)
	}
	res, err := Unclaim(ctx, s, ClaimInput{Ref: "6", Note: "владелец передумал"}, "test", claimNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sale) != 1 || res.Sale[0].To != "ничей" || res.Rate != nil {
		t.Fatalf("result %+v", res)
	}
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '6' AND home = 'nobody' AND reason = 'no_holder'
		AND person IS NULL AND day = '2026-11-11'`); n != 1 {
		t.Fatal("the sale is not nobody's again")
	}
	if n := count(t, s, `SELECT count(*) FROM fact_accruals WHERE person = 'bravo' AND day = '2026-11-11'`); n != 0 {
		t.Fatal("Браво keeps the unclaimed sale")
	}
	if n := count(t, s, `SELECT count(*) FROM sale_claims WHERE transaction_ref = '6'`); n != 2 {
		t.Fatalf("history of %d rows, want the claim and its removal", n)
	}
	if n := count(t, s, `SELECT count(*) FROM v_sale_claims`); n != 0 {
		t.Fatal("a removed claim is in force")
	}
	if _, err := s.RW.Exec(`DELETE FROM sale_claims`); err == nil {
		t.Fatal("claims can be deleted")
	}
	var r *Refusal
	if _, err := Unclaim(ctx, s, ClaimInput{Ref: "6", Note: "ещё раз"}, "test", claimNow); !errors.As(err, &r) {
		t.Fatalf("unclaim without a claim: %v", err)
	}
	// It can be claimed again, by someone else.
	if _, err := Claim(ctx, s, ClaimInput{Ref: "6", Person: "alfa", Note: "по слову владельца"}, "test", claimNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM v_sale_claims WHERE transaction_ref = '6' AND person = 'alfa' AND reason = 'owner_claim'`); n != 1 {
		t.Fatal("the new claim")
	}
}

func TestOnlyANobodysSaleCanBeClaimed(t *testing.T) {
	s, ctx := nobodysWeek(t)
	homes := func() int64 { return count(t, s, `SELECT count(*) FROM fact_homes WHERE reason <> 'owner_claim'`) }
	before := homes()
	for _, c := range []struct{ ref, person, note, want string }{
		{"1", "bravo", "по слову владельца", "не ничья — Альфа (страницу держала одна смена)"},
		{"2", "alfa", "по слову владельца", "не ничья — Браво (PPV отправителя"},
		{"3", "bravo", "по слову владельца", "не ничья — общак"},
		{"4", "bravo", "по слову владельца", "не ничья — ждёт решения"},
		{"no-such-sale", "bravo", "по слову владельца", "нет в фактах хаба"},
		{"6", "nobody-card", "по слову владельца", "нет человека «nobody-card»"},
		{"6", "bravo", " ", "нужна заметка"},
	} {
		_, err := Claim(ctx, s, ClaimInput{Ref: c.ref, Person: c.person, Note: c.note}, "test", claimNow)
		var r *Refusal
		if !errors.As(err, &r) || !strings.Contains(r.Msg, c.want) {
			t.Fatalf("claim of %s: %v, want «%s»", c.ref, err, c.want)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM sale_claims`); n != 0 || homes() != before {
		t.Fatal("a refused claim left rows")
	}
	if _, err := Claim(ctx, s, ClaimInput{Ref: "6", Person: "bravo", Note: "по слову владельца"}, "test", claimNow); err != nil {
		t.Fatal(err)
	}
	var r *Refusal
	if _, err := Claim(ctx, s, ClaimInput{Ref: "6", Person: "alfa", Note: "по слову владельца"}, "test", claimNow); !errors.As(err, &r) ||
		!strings.Contains(r.Msg, "уже заявлена: bravo") {
		t.Fatalf("a second claim: %v", err)
	}
	// New facts say Альфа's shift held the sale after all: it is hers by the rule, the claim is not honoured and says why.
	exec(t, s, `UPDATE hub_sales SET payload = json_set(payload, '$.holders', json_array((SELECT ref FROM hub_works WHERE user_id = 101)))
		WHERE transaction_ref = '6'`)
	rep := rebuild(t, s, ctx)
	if rep.Claims != 0 || len(rep.ClaimsIgnored) != 1 || !strings.Contains(rep.ClaimsIgnored[0].Why, "не ничья — Альфа") {
		t.Fatalf("report %+v", rep)
	}
	if n := count(t, s, `SELECT count(*) FROM fact_homes WHERE transaction_ref = '6' AND person = 'alfa' AND reason = 'holder'`); n != 1 {
		t.Fatal("the claim moved a held sale")
	}
}

func TestTheClaimRoutesNeedTheDecideScope(t *testing.T) {
	s, ctx := nobodysWeek(t)
	srv := httptest.NewServer(server.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test").Mux)
	defer srv.Close()
	token := func(name, scope string) string {
		tok, _, err := auth.Create(ctx, s.RW, name, []string{scope}, false, ts.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	in := ClaimInput{Ref: "6", Person: "bravo", Note: "по слову владельца"}
	var out ClaimResult
	var e *client.Error
	proposer := client.New(config.Client{URL: srv.URL, Key: token("agent", auth.ScopePropose)})
	if err := proposer.Do(ctx, http.MethodPost, "/v1/accruals/claim", in, &out); !asStatus(err, &e, 403) {
		t.Fatalf("a key without decide: %v", err)
	}
	owner := client.New(config.Client{URL: srv.URL, Key: token("owner", auth.ScopeDecide)})
	if err := owner.Do(ctx, http.MethodPost, "/v1/accruals/claim", in, &out); err != nil || !out.OK || out.Claim.RecordedBy != "owner" {
		t.Fatalf("claim %+v %v", out, err)
	}
	if err := owner.Do(ctx, http.MethodPost, "/v1/accruals/claim", ClaimInput{Ref: "1", Person: "bravo", Note: "x"}, &out); !asStatus(err, &e, 422) ||
		!strings.Contains(e.Message, "не ничья") {
		t.Fatalf("a held sale: %v", err)
	}
	if err := owner.Do(ctx, http.MethodPost, "/v1/accruals/unclaim", ClaimInput{Ref: "6", Note: "снято"}, &out); err != nil ||
		out.Claim.Action != "unclaim" {
		t.Fatalf("unclaim %+v %v", out, err)
	}
}
