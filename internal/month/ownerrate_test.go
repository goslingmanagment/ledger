package month

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goslingmanagment/ledger/internal/db"
)

func TestRaiseAcceptedBeforeTheCloseAndCaps(t *testing.T) {
	ctx := context.Background()
	s, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	set := func(k, v string) {
		t.Helper()
		if _, err := s.RW.Exec(`INSERT INTO settings(key, value, updated_at) VALUES (?, ?, 'x')
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	// One number: every month has the same rate, no raise is in force yet.
	set("owner_salary_rate", "3100")
	if r, err := settingRate(ctx, s.RO, "2026-10"); err != nil || r == nil || *r != 3100 {
		t.Fatalf("rate %v %v", r, err)
	}
	if from, err := raiseFrom(ctx, s.RO, "2026-10", 3600); err != nil || from != "" {
		t.Fatalf("raise %q %v", from, err)
	}
	// The owner accepted 3600 from November before October closed: October is still judged by 3100 + step,
	// and the card says the raise is in force instead of asking again.
	set("owner_salary_rate", `{"2026-10-01": 3100, "2026-11-01": 3600}`)
	if r, _ := settingRate(ctx, s.RO, "2026-10"); r == nil || *r != 3100 {
		t.Fatalf("October rate %v", r)
	}
	if r, _ := settingRate(ctx, s.RO, "2026-11"); r == nil || *r != 3600 {
		t.Fatalf("November rate %v", r)
	}
	if from, err := raiseFrom(ctx, s.RO, "2026-10", 3600); err != nil || from != "2026-11-01" {
		t.Fatalf("raise %q %v", from, err)
	}
	if from, _ := raiseFrom(ctx, s.RO, "2026-11", 4100); from != "" {
		t.Fatalf("November's raise is not accepted: %q", from)
	}
	if prevMonth("2026-01") != "2025-12" {
		t.Fatal(prevMonth("2026-01"))
	}

	card := &Card{Exp: &Expenses{Cats: newOrdered()}}
	card.Exp.Cats.Add("Сервисы", 491.4)
	card.Exp.Cats.Add("Реклама", 1120)
	if line, err := capLine(ctx, s.RO, card); err != nil || line != "" {
		t.Fatalf("no caps: %q %v", line, err)
	}
	set("category_caps", `{"Сервисы": 1000, "Реклама": 1000, "Пусто": 50}`)
	line, err := capLine(ctx, s.RO, card)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Лимиты: Пусто $0 из $50 · Реклама $1 120 из $1 000, перерасход $120 · Сервисы $491 из $1 000"; line != want {
		t.Fatalf("%q, want %q", line, want)
	}
	set("category_caps", "тысяча")
	if _, err := capLine(ctx, s.RO, card); err == nil {
		t.Fatal("broken caps passed")
	}
}
