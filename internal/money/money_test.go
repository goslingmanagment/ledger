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
