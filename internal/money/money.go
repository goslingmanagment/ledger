// Package money is the micro-dollar amount used by every ledger line: 1 USD = 1 000 000.
package money

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Micros is an amount in millionths of a dollar.
type Micros int64

// Scale is one dollar in micros.
const Scale = 1_000_000

// Parse reads "1234.56", "-0.0001", "1 234,5". More than six decimals is an error, not a rounding.
func Parse(s string) (Micros, error) {
	t := strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	t = strings.ReplaceAll(t, " ", "")
	t = strings.Replace(t, "−", "-", 1)
	if strings.Count(t, ",") == 1 && !strings.Contains(t, ".") {
		t = strings.Replace(t, ",", ".", 1)
	}
	if t == "" {
		return 0, fmt.Errorf("empty amount")
	}
	neg := strings.HasPrefix(t, "-")
	t = strings.TrimLeft(t, "+-")
	whole, frac, _ := strings.Cut(t, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("amount %q has more than 6 decimals", s)
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("not an amount: %q", s)
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/Scale {
		return 0, fmt.Errorf("amount %q out of range", s)
	}
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
	}
	if w*Scale > math.MaxInt64-f {
		return 0, fmt.Errorf("amount %q out of range", s)
	}
	v := Micros(w*Scale + f)
	if neg {
		v = -v
	}
	return v, nil
}

// MustParse is Parse for constants in tests.
func MustParse(s string) Micros {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// FromFloat rounds a dollar float to micros, half to even on the exact binary value.
func FromFloat(x float64) Micros {
	v, _ := Parse(strconv.FormatFloat(x, 'f', 6, 64))
	return v
}

// Float is the amount in dollars.
func (m Micros) Float() float64 { return float64(m) / Scale }

// String prints the shortest exact decimal with at least two places: 12.50, 121.0148, -3.00.
func (m Micros) String() string {
	neg := m < 0
	u := uint64(m)
	if neg {
		u = uint64(-m)
	}
	s := fmt.Sprintf("%d.%06d", u/Scale, u%Scale)
	s = strings.TrimRight(s, "0")
	if i := strings.IndexByte(s, '.'); len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Cents prints with exactly two decimals, rounding half to even.
func (m Micros) Cents() string {
	return strconv.FormatFloat(RoundCents(m).Float(), 'f', 2, 64)
}

// RoundCents rounds to whole cents, half to even.
func RoundCents(m Micros) Micros {
	q, r := m/10_000, m%10_000
	if r < 0 {
		q, r = q-1, r+10_000
	}
	if r > 5_000 || (r == 5_000 && q%2 != 0) {
		q++
	}
	return q * 10_000
}

// Round2 rounds a float like Python's round(x, 2): exact binary value, ties to even.
func Round2(x float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 2, 64), 64)
	return v
}

// RoundN is Round2 for n places.
func RoundN(x float64, n int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return v
}
