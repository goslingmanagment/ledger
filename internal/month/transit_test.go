package month

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/journal"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
)

func loadGolden(t *testing.T) (*golden, []journal.Row) {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	var rows []journal.Row
	for _, c := range g.Ledger {
		n, _ := strconv.Atoi(c[0])
		rows = append(rows, journal.Row{Num: n, Date: c[1], Type: c[2], Category: c[3], Counterparty: c[4], Model: c[5], Amount: c[6],
			Currency: c[7], Method: c[8], Status: c[9], Note: c[10]})
	}
	return &g, journal.SortedByDate(rows)
}

// The entries of money in transit must carry the card: income of a month = platform earnings of the
// card, money at the model = what is «едет», and a re-run changes nothing.
func TestTransitEntriesCarryTheCard(t *testing.T) {
	g, rows := loadGolden(t)
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cv, err := BuildCover(goldenConfig(g), fakeSource{g}, rows, parseT(g.AsOf))
	if err != nil {
		t.Fatal(err)
	}
	apply := func(cv *Cover) RefreshReport {
		t.Helper()
		want, err := DesiredEntries(cv, "2026-09-01")
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.RW.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var rep RefreshReport
		if err := reconcileEntries(ctx, tx, want, time.Now(), &rep); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	first := apply(cv)
	if first.Posted == 0 || first.Reversed != 0 {
		t.Fatalf("first run %+v", first)
	}
	var unbalanced int
	s.RO.QueryRow(`SELECT count(*) FROM v_unbalanced`).Scan(&unbalanced)
	if unbalanced != 0 {
		t.Fatal("unbalanced entries")
	}
	again := apply(cv)
	if again.Posted != 0 || again.Reversed != 0 || again.Same != first.Posted {
		t.Fatalf("second run changed something: %+v", again)
	}
	for _, month := range []string{"2026-09", "2026-10"} {
		card, err := BuildCard(cv, month, g.Tabs)
		if err != nil {
			t.Fatal(err)
		}
		earned := 0.0
		for _, l := range card.Lines {
			if l.Channel == "Fansly" || l.Channel == "OF" {
				earned += l.Vals.Vals["earned"]
			}
		}
		now, _, err := ledger.MonthTotals(ctx, s.RO, month)
		if err != nil {
			t.Fatal(err)
		}
		var income money.Micros
		for acct, v := range now {
			// income:platforms:<model> without a dash is the part of a transfer earned before the accrual date.
			if (strings.HasPrefix(acct, "income:platforms:") && strings.Contains(acct, "-")) || strings.HasPrefix(acct, "income:true_up:") {
				income -= v
			}
		}
		if math.Abs(income.Float()-earned) > 0.05 {
			t.Errorf("%s income by entries %.4f, earned on the card %.4f", month, income.Float(), earned)
		}
	}
	// Everything requested and not transferred yet sits at the model.
	riding := 0.0
	for _, p := range cv.PageOrder {
		for _, r := range cv.Rows[p] {
			if r.T.Before(parseT("2026-09-01T00:00:00+03:00")) {
				continue
			}
			for _, part := range r.Parts {
				if part.Req != nil && !part.Req.Pre && (part.Req.Transfer == nil || len(part.Req.Transfer.Bad) > 0) {
					riding += part.Amt * cv.Cfg.RateOf(p, part.Req) * cv.Cfg.Share
				}
			}
		}
	}
	var due int64
	s.RO.QueryRow(`SELECT COALESCE(SUM(balance_micros), 0) FROM v_balances WHERE account LIKE 'model:%'`).Scan(&due)
	if math.Abs(float64(due)/1e6-riding) > 0.05 {
		t.Errorf("at the models %.4f, riding %.4f", float64(due)/1e6, riding)
	}
	// An earlier cut changes state: what is no longer true is reversed, nothing is edited.
	earlier, err := BuildCover(goldenConfig(g), fakeSource{g}, rows, parseT("2026-10-20T12:00:00+03:00"))
	if err != nil {
		t.Fatal(err)
	}
	rep := apply(earlier)
	if rep.Reversed == 0 {
		t.Fatalf("no reversals on a changed coverage: %+v", rep)
	}
	var keys int
	s.RO.QueryRow(`SELECT count(*) FROM entries WHERE source_key LIKE 'transit:%:r2'`).Scan(&keys)
	if keys == 0 {
		t.Fatal("changed entries were not reposted under a new revision")
	}
	s.RO.QueryRow(`SELECT count(*) FROM v_unbalanced`).Scan(&unbalanced)
	if unbalanced != 0 {
		t.Fatal("unbalanced entries after the second coverage")
	}
}
