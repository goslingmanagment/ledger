package money

import "testing"

func TestParseAndString(t *testing.T) {
	for in, want := range map[string]string{"2720.00": "2720.00", "121.0148": "121.0148", "-0.03": "-0.03",
		"1 234,5": "1234.50", "0.000001": "0.000001", "−5": "-5.00"} {
		v, err := Parse(in)
		if err != nil || v.String() != want {
			t.Errorf("Parse(%q) = %v %v, want %s", in, v, err, want)
		}
	}
	for _, bad := range []string{"", "1.2345678", "12a", "1.2.3"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if RoundCents(MustParse("0.125")) != MustParse("0.12") || RoundCents(MustParse("0.135")) != MustParse("0.14") ||
		RoundCents(MustParse("-0.125")) != MustParse("-0.12") {
		t.Error("RoundCents is not half-to-even")
	}
	if Round2(2.675) != 2.67 || Round2(0.125) != 0.12 { // Python: round(2.675, 2) == 2.67, round(0.125, 2) == 0.12
		t.Error("Round2 differs from Python round")
	}
}

func TestShareBPAndShareRate(t *testing.T) {
	// Exact integer basis-point arithmetic avoids IEEE-754 drift (e.g. 655.589 * 0.2).
	if got := ShareBP(655_589_000, 2000); got != 131_117_800 {
		t.Fatalf("ShareBP(655.589, 2000) = %d, want 131117800", got)
	}
	if got := ShareRate(655_589_000, 0.2); got != 131_117_800 {
		t.Fatalf("ShareRate(655.589, 0.2) = %d, want 131117800", got)
	}
	// Half-to-even ties at exact 5000/10000 remainder.
	if got := ShareBP(5, 1000); got != 0 { // 0.5 -> 0 (even)
		t.Fatalf("ShareBP(5, 1000) = %d, want 0", got)
	}
	if got := ShareBP(15, 1000); got != 2 { // 1.5 -> 2 (even)
		t.Fatalf("ShareBP(15, 1000) = %d, want 2", got)
	}
	if got := ShareBP(-15, 1000); got != -2 {
		t.Fatalf("ShareBP(-15, 1000) = %d, want -2", got)
	}
	// Large values do not overflow int64.
	if got := ShareBP(100_000_000_000_000, 2500); got != 25_000_000_000_000 {
		t.Fatalf("ShareBP large = %d", got)
	}
	// Non-bp rate falls back cleanly.
	if got := ShareRate(30_000_000, 1.0/3.0); got != 10_000_000 {
		t.Fatalf("ShareRate(30, 1/3) = %d, want 10000000", got)
	}
}
