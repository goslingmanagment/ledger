package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/payroll"
)

// countingSheets is a spreadsheet that counts what the mirror asks of it; every tab is empty.
type countingSheets struct{ reads, writes int }

func (c *countingSheets) Values(context.Context, string, string) ([][]payroll.Cell, error) {
	c.reads++
	return nil, nil
}

func (c *countingSheets) Formulas(context.Context, string, string) ([][]payroll.Cell, error) {
	c.reads++
	return nil, nil
}

func (c *countingSheets) Write(context.Context, string, []payroll.RangeValue) error {
	c.writes++
	return nil
}

var mirrorCfg = payroll.Config{Account: "a@example.com", Tables: []payroll.Table{{Model: "alpha", Sheet: "id-alpha"}}}

func mirrorBase(reportsFrom, from string) payroll.MirrorBase {
	return payroll.MirrorBase{ReportsFrom: reportsFrom, From: from, Tabs: []payroll.MirrorTab{{Model: "alpha", Name: "Альфа", Person: "alfa"}},
		Posts: []payroll.MirrorPost{{Person: "alfa", Model: "alpha", Date: "2026-11-03", Page: "page-a", Micros: 1_000_000}}}
}

// With the switch unset the job exits quietly: one line, status 0, no sheet touched.
func TestMirrorSheetsRefusesWithoutTheSwitch(t *testing.T) {
	for _, o := range []mirrorOpts{{apply: true}, {}} {
		var out bytes.Buffer
		s := &countingSheets{}
		if err := runMirror(context.Background(), o, mirrorCfg, mirrorBase("", ""), s, &out, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out.String()); strings.Count(got, "\n") != 0 || !strings.Contains(got, "payroll_reports_from не задан") {
			t.Errorf("%+v: %q", o, got)
		}
		if s.reads+s.writes != 0 {
			t.Errorf("%+v: touched the sheets", o)
		}
	}
	// --from is a preview only.
	err := mirrorSheets(context.Background(), []string{"--apply", "--from", "2026-11-02"})
	var ex Exit
	if !errors.As(err, &ex) || ex.Code != 2 {
		t.Fatalf("%v", err)
	}
}

// A preview before the switch reads and plans, and writes nothing.
func TestMirrorSheetsPreview(t *testing.T) {
	var out bytes.Buffer
	s := &countingSheets{}
	o := mirrorOpts{from: "2026-11-02"}
	if err := runMirror(context.Background(), o, mirrorCfg, mirrorBase("", "2026-11-02"), s, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.reads == 0 || s.writes != 0 || !strings.Contains(out.String(), "Предпросмотр с 2026-11-02") || !strings.Contains(out.String(), "Итого:") {
		t.Fatalf("reads %d writes %d\n%s", s.reads, s.writes, out.String())
	}
}
