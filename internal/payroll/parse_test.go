package payroll

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type goldenTab struct {
	Rows   [][]Cell           `json:"rows"`
	Today  string             `json:"today"`
	Error  string             `json:"error"`
	Days   map[string]float64 `json:"days"`
	Worked []string           `json:"worked"`
	Notes  []string           `json:"notes"`
}

type goldenNames struct {
	Rows  [][]Cell `json:"rows"`
	Names []string `json:"names"`
	Error string   `json:"error"`
}

// golden.json is the original Python parser's output on synthetic tabs (gen_golden.py): every refusal
// message, the notes and the float path of day amounts, compared bit for bit.
func loadGolden(t *testing.T) (tabs map[string]goldenTab, names map[string]goldenNames) {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var g struct {
		Tabs  map[string]goldenTab   `json:"tabs"`
		Names map[string]goldenNames `json:"names"`
	}
	if err := dec.Decode(&g); err != nil {
		t.Fatal(err)
	}
	return g.Tabs, g.Names
}

func TestParseTabGolden(t *testing.T) {
	tabs, _ := loadGolden(t)
	if len(tabs) < 40 {
		t.Fatalf("only %d golden tabs", len(tabs))
	}
	refusals := 0
	for name, g := range tabs {
		t.Run(name, func(t *testing.T) {
			today, err := time.Parse("2006-01-02", g.Today)
			if err != nil {
				t.Fatal(err)
			}
			tab, err := ParseTab(g.Rows, "Model/Tab", today)
			if g.Error != "" {
				refusals++
				if err == nil || err.Error() != g.Error {
					t.Fatalf("refusal differs\n got %v\nwant %s", err, g.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(tab.Days) != len(g.Days) {
				t.Fatalf("days %d, want %d", len(tab.Days), len(g.Days))
			}
			for d, want := range g.Days {
				if got := tab.Days[d]; got != want {
					t.Errorf("%s: %v, want %v", d, got, want)
				}
			}
			if strings.Join(tab.Worked, ",") != strings.Join(g.Worked, ",") {
				t.Errorf("worked %v, want %v", tab.Worked, g.Worked)
			}
			if strings.Join(tab.Notes, "|") != strings.Join(g.Notes, "|") {
				t.Errorf("notes %v, want %v", tab.Notes, g.Notes)
			}
		})
	}
	if refusals < 25 {
		t.Fatalf("only %d refusals in the golden set", refusals)
	}
}

func TestNamesGolden(t *testing.T) {
	_, names := loadGolden(t)
	for name, g := range names {
		got, err := Names(g.Rows, "Model/Итого")
		if g.Error != "" {
			if err == nil || err.Error() != g.Error {
				t.Errorf("%s: refusal %v, want %s", name, err, g.Error)
			}
			continue
		}
		if err != nil || strings.Join(got, "|") != strings.Join(g.Names, "|") {
			t.Errorf("%s: %v %v, want %v", name, got, err, g.Names)
		}
	}
}

// Python's sum() is not a plain loop: the classic case, which a naive sum gets wrong.
func TestPySumMatchesPython(t *testing.T) {
	fl := func(xs ...float64) []num {
		out := make([]num, len(xs))
		for i, x := range xs {
			out[i] = num{v: x}
		}
		return out
	}
	if got := pySum(fl(0.1, 0.2, 0.3)).v; got != 0.6 { // sum([0.1, 0.2, 0.3]) == 0.6 in Python ≥ 3.12
		t.Fatalf("got %v", got)
	}
	if got := pySum(fl(1e100, 1, -1e100)).v; got != 1 {
		t.Fatalf("got %v", got)
	}
	if got := pySum([]num{{v: 3, isInt: true}, {v: 0.1}, {v: 2, isInt: true}, {v: 0.2}}); got.isInt || got.v != 5.3 {
		t.Fatalf("got %+v", got)
	}
	if got := pySum([]num{{v: 3, isInt: true}, {v: 2, isInt: true}}); !got.isInt || got.v != 5 {
		t.Fatalf("got %+v", got)
	}
}

func TestFormattingMatchesPython(t *testing.T) {
	for x, want := range map[float64]string{0: "0.0", 12.5: "12.5", 1234: "1234.0", 1e16: "1e+16", 1.5e-5: "1.5e-05", 0.0001: "0.0001", -3: "-3.0"} {
		if got := pyRepr(x); got != want {
			t.Errorf("repr(%v) = %s, want %s", x, got, want)
		}
	}
	for x, want := range map[float64]string{0: "0", 100000: "100000", 1e6: "1e+06", 1234567.5: "1.23457e+06", 0.0001: "0.0001", 0.00001: "1e-05", 12.5: "12.5", 1000.25: "1000.25"} {
		if got := fmtG(x); got != want {
			t.Errorf("format(%v, g) = %s, want %s", x, got, want)
		}
	}
	points := [][2]int{{351, 2}, {351, 3}, {351, 4}, {12, 6}}
	if got := cellRange(points); got != "G12, C351:E351" {
		t.Errorf("cellRange %s", got)
	}
}
