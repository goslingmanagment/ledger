package hub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Decisions of the exceptions file and the hub row types each fits (nil — any type).
var decisionTypes = map[string][]string{
	"приз":      {"other"},
	"исключить": nil,
	"возврат":   {"refund", "chargeback"},
}

// decisionList is list(DECISIONS) of month_files.py, in its order.
const decisionList = "['приз', 'исключить', 'возврат']"

// The three tables of the file, by their exact header cells.
var exceptionTables = map[string]string{
	strings.Join([]string{"Страница", "transactionRef", "Дата UTC", "Net", "Решение", "Основание"}, "\x00"): "row",
	strings.Join([]string{"Страница", "Начало UTC", "Основание"}, "\x00"):                                   "start",
	strings.Join([]string{"Страница", "Заявки с первой", "Основание"}, "\x00"):                              "floor",
}

var (
	sepCellRe  = regexp.MustCompile(`^:?-+:?$`)
	pageRe     = regexp.MustCompile(`^[a-z]+(?:-[a-z0-9]+)+$`)
	netCellRe  = regexp.MustCompile(`^-?[0-9]+\.[0-9][0-9]$`)
	floorRe    = regexp.MustCompile(`^[0-9]{4}-[0-9][0-9]-[0-9][0-9]$`)
	pySpaceSet = "\t\n\v\f\r\x1c\x1d\x1e\x1f \u0085   -     　"
	// strptime("%Y-%m-%d %H:%M:%S"), the way Python's _strptime builds it: one- or two-digit fields,
	// a day may be " 3", any run of whitespace between the date and the time.
	utcRe = regexp.MustCompile(`^([0-9]{4})-(1[0-2]|0[1-9]|[1-9])-(3[01]|[12][0-9]|0[1-9]|[1-9]| [1-9])[` + pySpaceSet +
		`]+(2[0-3]|[01][0-9]|[0-9]):([0-5][0-9]|[0-9]):(6[01]|[0-5][0-9]|[0-9])$`)
)

// RowRef names one hub row: page and transactionRef.
type RowRef struct{ Page, Ref string }

// ExceptionRow is an owner decision about one non-standard hub row.
type ExceptionRow struct {
	Decision string    // приз, исключить, возврат
	At       time.Time // the row's moment, to the second, UTC
	Net      float64   // as float(net) in month_files.py
	NetText  string    // "0.00" as written
	Basis    string
}

// Exceptions are the decisions of hub-exceptions.md: rows, page starts and payout-journal floors.
type Exceptions struct {
	Rows   map[RowRef]ExceptionRow
	Starts map[string]time.Time // first row of a page, UTC
	Floors map[string]string    // first payout request of a page, ГГГГ-ММ-ДД as written
	basis  map[string]string    // kind/page → basis of start and floor rows (stored, not used)
	order  []exceptionKey
}

type exceptionKey struct {
	kind string
	page string
	ref  string
}

func newExceptions() Exceptions {
	return Exceptions{Rows: map[RowRef]ExceptionRow{}, Starts: map[string]time.Time{}, Floors: map[string]string{},
		basis: map[string]string{}}
}

// ReadExceptions parses the file at path like month_files.load_exceptions.
func ReadExceptions(path string) (Exceptions, error) {
	name := filepath.Base(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Exceptions{}, fmt.Errorf("%s:0: не читается: %v", name, err)
	}
	return ParseExceptions(name, raw)
}

// ParseExceptions is the strict parser of month_files.load_exceptions: every table row in the format
// of its header, otherwise an error with the line number.
func ParseExceptions(name string, raw []byte) (Exceptions, error) {
	out := newExceptions()
	bad := func(n int, why string) (Exceptions, error) {
		return Exceptions{}, fmt.Errorf("%s:%d: %s", name, n, why)
	}
	if !utf8.Valid(raw) {
		return bad(0, "не читается: не UTF-8")
	}
	kind, sep, width := "", false, 0
	for i, line := range pySplitLines(string(raw)) {
		n := i + 1
		if pyStrip(line) == "" {
			kind = ""
			continue
		}
		if !strings.Contains(line, "|") && kind == "" {
			continue
		}
		if !(strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|")) {
			return bad(n, "строка таблицы должна начинаться и кончаться «|» без отступа")
		}
		inner := ""
		if len(line) > 1 {
			inner = line[1 : len(line)-1]
		}
		parts := strings.Split(inner, "|")
		cells := make([]string, len(parts))
		for j, p := range parts {
			cells[j] = pyStrip(p)
		}
		if kind == "" {
			k, ok := exceptionTables[strings.Join(cells, "\x00")]
			if !ok {
				return bad(n, "неизвестная таблица "+pyList(cells))
			}
			kind, sep, width = k, true, len(cells)
			continue
		}
		if len(cells) != width {
			return bad(n, fmt.Sprintf("%d ячеек вместо %d", len(cells), width))
		}
		if sep {
			sep = false
			for _, c := range cells {
				if !sepCellRe.MatchString(c) {
					return bad(n, "у шапки нет строки-разделителя")
				}
			}
			continue
		}
		page := cells[0]
		if !pageRe.MatchString(page) {
			return bad(n, "страница «"+page+"»")
		}
		switch kind {
		case "row":
			ref, when, net, dec := cells[1], cells[2], cells[3], cells[4]
			_, known := decisionTypes[dec]
			if !isDigits(ref) || !known || !netCellRe.MatchString(net) {
				return bad(n, fmt.Sprintf("ref «%s» (цифры), net «%s» (0.00), решение «%s» (из %s)", ref, net, dec, decisionList))
			}
			at, err := parseUTC(when)
			if err != nil {
				return bad(n, err.Error())
			}
			f, _ := strconv.ParseFloat(net, 64)
			key := RowRef{page, ref}
			if _, dup := out.Rows[key]; dup {
				return bad(n, "дубль ("+pyRepr(page)+", "+pyRepr(ref)+")")
			}
			out.Rows[key] = ExceptionRow{Decision: dec, At: at, Net: f, NetText: net, Basis: cells[5]}
			out.order = append(out.order, exceptionKey{"row", page, ref})
		case "start":
			at, err := parseUTC(cells[1])
			if err != nil {
				return bad(n, err.Error())
			}
			if _, dup := out.Starts[page]; dup {
				return bad(n, "дубль "+page)
			}
			out.Starts[page], out.basis["start/"+page] = at, cells[2]
			out.order = append(out.order, exceptionKey{"start", page, ""})
		case "floor":
			if !floorRe.MatchString(cells[1]) {
				return bad(n, "дата «"+cells[1]+"»")
			}
			if _, dup := out.Floors[page]; dup {
				return bad(n, "дубль "+page)
			}
			out.Floors[page], out.basis["floor/"+page] = cells[1], cells[2]
			out.order = append(out.order, exceptionKey{"floor", page, ""})
		}
	}
	return out, nil
}

// parseUTC is datetime.strptime(s, "%Y-%m-%d %H:%M:%S") in UTC.
func parseUTC(s string) (time.Time, error) {
	fail := fmt.Errorf("«%s» — нужно ГГГГ-ММ-ДД ЧЧ:ММ:СС (UTC)", s)
	m := utcRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fail
	}
	num := func(x string) int { v, _ := strconv.Atoi(strings.TrimSpace(x)); return v }
	y, mo, d, h, mi, sec := num(m[1]), num(m[2]), num(m[3]), num(m[4]), num(m[5]), num(m[6])
	t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.UTC)
	if y < 1 || sec > 59 || t.Day() != d {
		return time.Time{}, fail
	}
	return t, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isPySpace is Python's str.isspace for one character.
func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// pySplitLines is Python's str.splitlines().
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			i += size
			start = i
			continue
		case '\r':
			out = append(out, s[start:i])
			i += size
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pyRepr renders a string the way Python's repr() does, for messages that quote cells.
func pyRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r != ' ':
			if r < 0x10000 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func pyList(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = pyRepr(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ImportExceptions replaces the rows of source with ex, in one transaction of the caller.
func ImportExceptions(ctx context.Context, q ledger.Querier, ex Exceptions, source string, now time.Time) (int, error) {
	if source == "" {
		return 0, fmt.Errorf("hub_exceptions: нужен источник")
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM hub_exceptions WHERE source = ?`, source); err != nil {
		return 0, err
	}
	at := ts.Format(now)
	for _, k := range ex.order {
		var ref, when, net, dec any
		var basis string
		switch k.kind {
		case "row":
			r := ex.Rows[RowRef{k.page, k.ref}]
			ref, when, net, dec, basis = k.ref, ts.Format(r.At), r.NetText, r.Decision, r.Basis
		case "start":
			when, basis = ts.Format(ex.Starts[k.page]), ex.basis["start/"+k.page]
		case "floor":
			when, basis = ex.Floors[k.page], ex.basis["floor/"+k.page]
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO hub_exceptions(page_label, kind, transaction_ref, at, net, decision,
			basis, created_at, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, k.page, k.kind, ref, when, net, dec, basis, at,
			source); err != nil {
			return 0, fmt.Errorf("hub_exceptions %s %s %s: %w", k.kind, k.page, k.ref, err)
		}
	}
	return len(ex.order), nil
}

// LoadExceptions reads the decisions of every source. A page start or floor given twice is an error.
func LoadExceptions(ctx context.Context, q ledger.Querier) (Exceptions, error) {
	out := newExceptions()
	rows, err := q.QueryContext(ctx, `SELECT page_label, kind, COALESCE(transaction_ref, ''), COALESCE(at, ''),
		COALESCE(net, ''), COALESCE(decision, ''), basis, source FROM hub_exceptions ORDER BY id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		var page, kind, ref, at, net, dec, basis, source string
		if err := rows.Scan(&page, &kind, &ref, &at, &net, &dec, &basis, &source); err != nil {
			return out, err
		}
		if kind != "row" {
			if prev, dup := seen[kind+"/"+page]; dup {
				return out, fmt.Errorf("hub_exceptions: %s страницы %s дважды (%s, %s)", kind, page, prev, source)
			}
			seen[kind+"/"+page] = source
		}
		switch kind {
		case "row":
			t, err := ts.Parse(at)
			f, ferr := strconv.ParseFloat(net, 64)
			if _, ok := decisionTypes[dec]; err != nil || ferr != nil || !ok {
				return out, fmt.Errorf("hub_exceptions: строка %s/%s не разбирается (at %q, net %q, решение %q)", page, ref, at, net, dec)
			}
			out.Rows[RowRef{page, ref}] = ExceptionRow{Decision: dec, At: t.UTC(), Net: f, NetText: net, Basis: basis}
		case "start":
			t, err := ts.Parse(at)
			if err != nil {
				return out, fmt.Errorf("hub_exceptions: начало страницы %s: %w", page, err)
			}
			out.Starts[page] = t.UTC()
		case "floor":
			out.Floors[page] = at
		}
		out.order = append(out.order, exceptionKey{kind, page, ref})
	}
	return out, rows.Err()
}

// Pages lists every page the decisions name.
func (e Exceptions) Pages() []string {
	set := map[string]bool{}
	for k := range e.Rows {
		set[k.Page] = true
	}
	for p := range e.Starts {
		set[p] = true
	}
	for p := range e.Floors {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
