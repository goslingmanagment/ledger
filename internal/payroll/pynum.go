package payroll

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Python's numbers, exactly: the monthly card must reproduce payroll.py to the cent, so a cell keeps
// whether Python would have read it as int or float, and every sum goes through pySum. Products that
// feed a sum are wrapped in float64(...): the Go spec lets arm64 fuse x*y+z into one rounding, and an
// explicit conversion forbids it.

// Cell is one value of a sheet, decoded with json.Decoder.UseNumber: string, json.Number, bool or nil.
type Cell = any

// num is a cell Python treats as a number. Integers below 2^53 are exact in a float64, and nothing
// on a payout sheet comes near that.
type num struct {
	v     float64
	isInt bool
}

// asNum is isnum(x): a JSON number, not a bool.
func asNum(c Cell) (num, bool) {
	n, ok := c.(json.Number)
	if !ok {
		return num{}, false
	}
	s := n.String()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(v, 0) {
		return num{}, false
	}
	return num{v: v, isInt: !strings.ContainsAny(s, ".eE")}, true
}

// pySum is Python's builtin sum() as the owner's interpreter runs it (CPython 3.15, compared with the
// builtin on 400 000 random lists): exact over the leading integers; the first float is a plain
// addition; every later item, integer or float, goes through Neumaier's compensation, which is added
// at the end. A plain loop differs in the last bits, and those show up in the cents of the card.
func pySum(xs []num) num {
	total, k := 0.0, 0
	for ; k < len(xs) && xs[k].isInt; k++ {
		total += xs[k].v
	}
	if k == len(xs) {
		return num{v: total, isInt: true}
	}
	f, c := total+xs[k].v, 0.0
	for _, x := range xs[k+1:] {
		t := f + x.v
		if math.Abs(f) >= math.Abs(x.v) {
			c += (f - t) + x.v
		} else {
			c += (x.v - t) + f
		}
		f = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		f += c
	}
	return num{v: f}
}

// fmtG is format(x, "g").
func fmtG(x float64) string { return strconv.FormatFloat(x, 'g', 6, 64) }

// pyRepr is repr(float).
func pyRepr(x float64) string {
	if a := math.Abs(x); x == 0 || (a >= 1e-4 && a < 1e16) {
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	return strconv.FormatFloat(x, 'e', -1, 64)
}

// pyStr is str(cell).
func pyStr(c Cell) string {
	switch x := c.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		n, ok := asNum(x)
		switch {
		case !ok:
			return x.String()
		case n.isInt && n.v == 0:
			return "0"
		case n.isInt:
			return x.String()
		}
		return pyRepr(n.v)
	}
	return ""
}

// pyStrip is str.strip(): Unicode whitespace plus the separators \x1c-\x1f Python also counts.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// cellEq is cell == text.
func cellEq(c Cell, text string) bool {
	s, ok := c.(string)
	return ok && s == text
}
