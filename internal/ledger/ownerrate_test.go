package ledger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
)

func TestOwnerRateByDay(t *testing.T) {
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	set := func(v string) {
		t.Helper()
		if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('owner_salary_rate', ?, 'x')
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, err := OwnerRate(ctx, s.RO, "2026-11-01"); ok || err != nil {
		t.Fatalf("no setting: ok %v err %v", ok, err)
	}
	// One number is the rate at any time.
	set("3100")
	if v, from, ok, err := OwnerRate(ctx, s.RO, "2026-01-01"); !ok || err != nil || v != 3100 || from != "" {
		t.Fatalf("number: %v %q %v %v", v, from, ok, err)
	}
	// A raise accepted for November leaves October, the month still being closed, at its own rate.
	set(`{"2026-10-01": 3100, "2026-11-01": 3600}`)
	for day, want := range map[string]float64{"2026-10-01": 3100, "2026-10-31": 3100, "2026-11-01": 3600, "2027-02-10": 3600} {
		if v, _, ok, err := OwnerRate(ctx, s.RO, day); !ok || err != nil || v != want {
			t.Fatalf("%s: %v %v %v, want %v", day, v, ok, err, want)
		}
	}
	if _, from, _, _ := OwnerRate(ctx, s.RO, "2026-11-20"); from != "2026-11-01" {
		t.Fatalf("from %q", from)
	}
	if _, _, ok, err := OwnerRate(ctx, s.RO, "2026-09-30"); ok || err != nil {
		t.Fatalf("before the first day: ok %v err %v", ok, err)
	}
	for _, bad := range []string{"много", `{"ноябрь": 3600}`, `{"2026-11-01": "x"}`} {
		set(bad)
		if _, _, _, err := OwnerRate(ctx, s.RO, "2026-11-01"); err == nil {
			t.Fatalf("%s passed", bad)
		}
	}
}
