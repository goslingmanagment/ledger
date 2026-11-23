package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/payroll"
)

func TestPrintBriefRendersRoundingLine(t *testing.T) {
	for _, tc := range []struct {
		name        string
		shownMicros int64
		text        string
		wantCol     string
	}{
		{"positive", 10_000, "округление долей цента: +0.01", "0.01"},
		{"negative", -10_000, "округление долей цента: -0.01", "-0.01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			out := bufio.NewWriter(&buf)
			b := payroll.PayoutBrief{
				OK:           true,
				Slug:         "alfa",
				Name:         "Альфа",
				AmountMicros: 47_835_000,
				Period:       &payroll.PreparePeriod{From: "2026-09-07", To: "2026-09-14"},
				Lines: []payroll.PrepareLine{
					{Kind: payroll.LineWeek, Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-07", To: "2026-09-07",
						PayMicros: 30_862_500, ShownMicros: 30_860_000, Text: "07.09: 123.45 × 25% = 30.86"},
					{Kind: payroll.LineRounding, PayMicros: 0, ShownMicros: tc.shownMicros, Text: tc.text},
				},
				Blocks: []payroll.PrepareBlock{
					{Model: "Alpha", Tab: "Альфа", Week: "2026-09-07", From: "2026-09-07", To: "2026-09-07", WeekOver: true},
				},
				Placeholder: payroll.TransferPlaceholder,
				Message:     "закинул зп, {перевод} usdt\nпериод 07–14.09\n\nalpha\n07.09: 123.45 × 25% = 30.86\n" + tc.text + "\nитого 47.84 → {перевод}\n\nчекни что дошло",
			}
			printBrief(out, b)
			if err := out.Flush(); err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			if !strings.Contains(got, tc.text) {
				t.Fatalf("missing rounding text %q in:\n%s", tc.text, got)
			}
			// Find the table row for the rounding line: blank tab column, rounding text, right-aligned amount.
			found := false
			for _, line := range strings.Split(got, "\n") {
				if strings.Contains(line, tc.text) && strings.HasSuffix(line, " "+tc.wantCol) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("rounding table row with amount %q not found in:\n%s", tc.wantCol, got)
			}
		})
	}
}
