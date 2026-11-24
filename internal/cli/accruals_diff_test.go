package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/goslingmanagment/ledger/internal/accruals"
)

func TestAccrualsDiffRendersGroupedSaleReason(t *testing.T) {
	var b bytes.Buffer
	out := bufio.NewWriter(&b)
	printDiff(out, accruals.Diff{Pot: accruals.Group{Sales: []accruals.SaleLine{{Page: "fixture-page", Reason: "fixture reason"}}}})
	if err := out.Flush(); err != nil || !strings.Contains(b.String(), "fixture reason") {
		t.Fatal(b.String(), err)
	}
}
