package bybit

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/goslingmanagment/ledger/internal/money"
)

// Bybit sends amounts as decimal strings, crypto with eight places and more: exact rationals here,
// micro-dollars only at the edge of the journal.

func rat(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	return new(big.Rat).SetString(s)
}

// roundHalfEven rounds r to an integer, ties to even (Python's Decimal default).
func roundHalfEven(r *big.Rat) *big.Int {
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	twice := new(big.Int).Lsh(new(big.Int).Abs(m), 1)
	if c := twice.Cmp(r.Denom()); c > 0 || (c == 0 && q.Bit(0) == 1) {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// toMicros rounds a decimal string to micro-dollars.
func toMicros(s string) (money.Micros, error) {
	r, ok := rat(s)
	if !ok {
		return 0, fmt.Errorf("не сумма: %q", s)
	}
	v := roundHalfEven(new(big.Rat).Mul(r, big.NewRat(money.Scale, 1)))
	if !v.IsInt64() {
		return 0, fmt.Errorf("сумма вне диапазона: %q", s)
	}
	return money.Micros(v.Int64()), nil
}

// fixed prints r with exactly places decimals, rounding half to even.
func fixed(r *big.Rat, places int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	v := roundHalfEven(new(big.Rat).Mul(r, new(big.Rat).SetInt(scale)))
	neg := v.Sign() < 0
	digits := new(big.Int).Abs(v).String()
	if places > 0 {
		if len(digits) <= places {
			digits = strings.Repeat("0", places-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// short prints an amount for a summary line: 100, 12.3456 (at most four places).
func short(s string) string {
	r, ok := rat(s)
	if !ok {
		return s
	}
	if r.IsInt() {
		return r.Num().String()
	}
	return strings.TrimRight(strings.TrimRight(fixed(r, 4), "0"), ".")
}

// sameAmount compares two decimal strings by value.
func sameAmount(a, b string) bool {
	x, ok1 := rat(a)
	y, ok2 := rat(b)
	if !ok1 || !ok2 {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return x.Cmp(y) == 0
}

// absDiffLE reports |a − b| ≤ tol.
func absDiffLE(a, b, tol *big.Rat) bool {
	d := new(big.Rat).Sub(a, b)
	return d.Abs(d).Cmp(tol) <= 0
}

func mustRat(s string) *big.Rat {
	r, ok := rat(s)
	if !ok {
		panic("bad constant " + s)
	}
	return r
}
