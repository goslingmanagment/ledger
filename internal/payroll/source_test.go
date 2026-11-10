package payroll

import (
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// The report date of a shift: the Moscow day it starts, unless it starts in the last two hours before midnight and
// mostly lies in the next day.
func TestReportDateOfAShift(t *testing.T) {
	msk := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, ts.MSK)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ from, to, want string }{
		{"2026-11-10 10:00", "2026-11-10 18:00", "2026-11-10"}, // a day shift
		{"2026-11-10 23:00", "2026-11-11 07:00", "2026-11-11"}, // an hour before midnight, seven after: the next day
		{"2026-11-10 22:00", "2026-11-11 02:30", "2026-11-11"}, // two hours before, two and a half after
		{"2026-11-10 22:00", "2026-11-11 02:00", "2026-11-10"}, // half and half: the day it starts
		{"2026-11-10 22:30", "2026-11-11 01:00", "2026-11-10"}, // mostly before midnight
		{"2026-11-10 21:00", "2026-11-11 05:00", "2026-11-10"}, // three hours before midnight: too early for the exception
		{"2026-11-10 22:00", "2026-11-10 23:50", "2026-11-10"}, // ends before midnight
		{"2026-11-11 00:00", "2026-11-11 08:00", "2026-11-11"}, // starts at midnight
	} {
		// The instants may come in any zone: the rule reads them in Moscow.
		if got := ReportDate(msk(c.from).UTC(), msk(c.to).UTC()); got != c.want {
			t.Errorf("%s — %s: %s, want %s", c.from, c.to, got, c.want)
		}
	}
}

// The source_key of an accrual says its source; a sheet's has no suffix, as before any switch.
func TestAccrualKeyOfEachSource(t *testing.T) {
	for _, source := range []string{SourceSheet, SourceFacts, SourceReports} {
		key := accrualKey("Alpha", "Альфа", "2026-11-10", 3, source)
		day, n, got, ok := parseAccrualKey(key)
		if !ok || day != "Alpha/Альфа:2026-11-10" || n != 3 || got != source {
			t.Errorf("%s: %q → %q %d %q %v", source, key, day, n, got, ok)
		}
	}
	if key := accrualKey("Alpha", "Альфа", "2026-11-10", 1, SourceSheet); key != "accrual:payroll:Alpha/Альфа:2026-11-10#1" {
		t.Errorf("a sheet's key changed: %q", key)
	}
}
