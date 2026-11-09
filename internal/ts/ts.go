// Package ts holds the time conventions of the database: Moscow time with an explicit +03:00
// offset, so text comparison of timestamps and dates is chronological.
package ts

import (
	"fmt"
	"time"
)

// MSK is Moscow time (fixed +03:00 since 2014).
var MSK = time.FixedZone("MSK", 3*3600)

// Layout of stored timestamps.
const Layout = "2006-01-02T15:04:05-07:00"

// DateLayout of stored dates.
const DateLayout = "2006-01-02"

// Format renders t in MSK, seconds precision.
func Format(t time.Time) string { return t.In(MSK).Format(Layout) }

// FormatMilli renders t in MSK with milliseconds, for sources whose order depends on them.
func FormatMilli(t time.Time) string { return t.In(MSK).Format("2006-01-02T15:04:05.000-07:00") }

// Date renders the MSK calendar date of t.
func Date(t time.Time) string { return t.In(MSK).Format(DateLayout) }

// Month renders the MSK month of t (ГГГГ-ММ).
func Month(t time.Time) string { return t.In(MSK).Format("2006-01") }

// WeekStart is Monday 00:00 MSK of the week holding t.
func WeekStart(t time.Time) time.Time {
	m := t.In(MSK)
	day := time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, MSK)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

// Now is the current time; tests replace it.
var Now = time.Now

// Parse reads a stored timestamp, an RFC 3339 time or a date (MSK midnight).
func Parse(s string) (time.Time, error) {
	for _, l := range []string{time.RFC3339Nano, Layout} {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation(DateLayout, s, MSK); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, MSK); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, MSK); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("not a time: %q", s)
}

// MonthStart returns MSK midnight of the first day of month "ГГГГ-ММ" and of the next month.
func MonthStart(month string) (time.Time, time.Time, error) {
	t, err := time.ParseInLocation("2006-01", month, MSK)
	if err != nil {
		return t, t, fmt.Errorf("month %q: want ГГГГ-ММ", month)
	}
	return t, t.AddDate(0, 1, 0), nil
}
