package reports

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// A chatter's post in the «Отчётность» topic, as people write it: a date line, a chatter line with a #tag, account
// lines between «Аккаунты» and «Общий доход», the total, then sales and a comment that are not money. The formats
// vary; ParsePost reads all the variants in use and nothing else (stage R2a of docs/plans/2026-11-10-shift-reports.md).

// PostPage is an account line of a post.
type PostPage struct {
	Name    string // as written, without bullets and separators
	InShift bool   // false — «не в смене»
	Micros  int64
}

// ParsedPost is what a post says. Date: the date line as written; Day/Month always set when DateOK, Year 0 when the
// line had no year.
type ParsedPost struct {
	DateLine         bool
	DateOK           bool
	Day, Month, Year int
	Tag              string // the #tag of the chatter line, with «#», as written; "" — none
	Pages            []PostPage
	TotalMicros      int64
}

var (
	dateLineRe = regexp.MustCompile(`(?i)дата\s*:`)
	dateRe     = regexp.MustCompile(`(\d{1,2})\s*[./]\s*(\d{1,2})(?:\s*[./]\s*(\d{4}|\d{2}))?`)
	tagRe      = regexp.MustCompile(`#[\p{L}\p{N}_.\-]+`)
	totalRe    = regexp.MustCompile(`(?i)общий\s+доход`)
	// An amount: «$24,00», «＄ 1 204.5», «24$». A space, dot or comma before three digits separates thousands; a dot
	// or comma before one or two digits at the end is the decimal mark.
	amountBefore = regexp.MustCompile(`[$＄]\s*(` + number + `)`)
	amountAfter  = regexp.MustCompile(`(` + number + `)\s*[$＄]`)
)

const number = `\d+(?:[ .,\x{00a0}\x{202f}]\d{3})*(?:[.,]\d{1,2})?`

const (
	accountsWord = "аккаунты"
	salesWord    = "основные продажи"
	commentWord  = "комментарий"
	offShift     = "не в смене"
)

// platformWords are the section headers of account lines.
var platformWords = map[string]string{"of": "onlyfans", "onlyfans": "onlyfans", "fansly": "fansly", "фансли": "fansly"}

// PlatformOf is the Hub platform a page name stands for when it is a bare platform word («Fansly - $204»), or "".
func PlatformOf(name string) string { return platformWords[NormName(name)] }

// NormName is a page name as the settings key it: lowercased, spaces and a leading «-» or «•» removed.
func NormName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimLeft(s, "-•–—·* ")
	return strings.TrimSpace(s)
}

// ParsePost reads a post. ok is false when the text is not a report: it needs both a date line and a total line.
func ParsePost(text string) (ParsedPost, bool) {
	var p ParsedPost
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// Everything from «Основные продажи» on is not money.
	for i, l := range lines {
		if low := strings.ToLower(l); strings.Contains(low, salesWord) || strings.Contains(low, commentWord) {
			lines = lines[:i]
			break
		}
	}
	total := -1
	for i, l := range lines {
		if totalRe.MatchString(l) {
			total = i
			break
		}
	}
	for _, l := range lines {
		if dateLineRe.MatchString(l) {
			p.DateLine = true
			p.Day, p.Month, p.Year, p.DateOK = readDate(l[dateLineRe.FindStringIndex(l)[1]:])
			break
		}
	}
	if !p.DateLine || total < 0 {
		return ParsedPost{}, false
	}
	// The total: on its line, or on the next line that holds an amount.
	for i := total; i < len(lines); i++ {
		l := clean(lines[i])
		if i == total {
			l = l[totalRe.FindStringIndex(l)[1]:]
		}
		if v, _, ok := findAmount(l); ok {
			p.TotalMicros = v
			break
		}
		if i > total && strings.TrimSpace(l) != "" {
			break
		}
	}
	start := 0
	for i := 0; i < total; i++ {
		if strings.Contains(strings.ToLower(lines[i]), accountsWord) {
			start = i + 1
			break
		}
	}
	for _, raw := range lines[start:total] {
		l := clean(raw)
		low := strings.ToLower(l)
		if strings.TrimSpace(l) == "" || dateLineRe.MatchString(l) || strings.Contains(low, "чаттер") {
			continue
		}
		bare := strings.TrimRight(NormName(l), ": ")
		if _, header := platformWords[bare]; header {
			continue
		}
		if i := strings.Index(low, offShift); i >= 0 {
			if name := pageName(l[:i]); name != "" {
				p.Pages = append(p.Pages, PostPage{Name: name})
			}
			continue
		}
		v, at, ok := findAmount(l)
		if !ok {
			continue
		}
		if name := pageName(l[:at]); name != "" {
			p.Pages = append(p.Pages, PostPage{Name: name, InShift: true, Micros: v})
		}
	}
	for _, l := range lines[:total] {
		if strings.Contains(strings.ToLower(l), "чаттер") {
			p.Tag = tagRe.FindString(l)
			break
		}
	}
	return p, true
}

// clean drops markdown bold and odd spaces.
func clean(s string) string {
	s = strings.ReplaceAll(s, "*", "")
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == ' ' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// pageName is the text before an amount or «не в смене», without bullets, emoji and separators.
func pageName(s string) string {
	s = strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return strings.TrimRight(s, " -:–—=|")
}

// findAmount is the first amount of a line in micros and where it starts: «$» before the number first, then after.
func findAmount(s string) (int64, int, bool) {
	for _, re := range []*regexp.Regexp{amountBefore, amountAfter} {
		if m := re.FindStringSubmatchIndex(s); m != nil {
			if v, ok := parseAmount(s[m[2]:m[3]]); ok {
				return v, m[0], true
			}
		}
	}
	return 0, 0, false
}

// parseAmount reads «1 204,50», «76.79», «8», «1,234»: the last «.» or «,» followed by one or two digits is the
// decimal mark, any other is a thousands separator.
func parseAmount(s string) (int64, bool) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, ".,")
	if s == "" {
		return 0, false
	}
	whole, frac := s, ""
	if i := strings.LastIndexAny(s, ".,"); i >= 0 && len(s)-i-1 <= 2 {
		whole, frac = s[:i], s[i+1:]
	}
	whole = strings.NewReplacer(".", "", ",", "").Replace(whole)
	if whole == "" {
		whole = "0"
	}
	d, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || d > maxMills/1000 {
		return 0, false
	}
	for len(frac) < 2 {
		frac += "0"
	}
	c, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, false
	}
	return d*1_000_000 + c*10_000, true
}

func readDate(s string) (day, month, year int, ok bool) {
	m := dateRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, false
	}
	day, _ = strconv.Atoi(m[1])
	month, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		year, _ = strconv.Atoi(m[3])
		if year < 100 {
			year += 2000
		}
	}
	y := year
	if y == 0 {
		y = 2024 // a leap year: 29.02 without a year stays a date
	}
	if month < 1 || month > 12 || day < 1 || day != time.Date(y, time.Month(month), day, 0, 0, 0, 0, ts.MSK).Day() {
		return 0, 0, 0, false
	}
	return day, month, year, true
}

// ReportDate is the post's date as YYYY-MM-DD: a date without a year takes the year that puts it closest to the
// Moscow day the message was posted. "" — no readable date.
func (p ParsedPost) ReportDate(posted time.Time) string {
	if !p.DateOK {
		return ""
	}
	if p.Year != 0 {
		return time.Date(p.Year, time.Month(p.Month), p.Day, 0, 0, 0, 0, ts.MSK).Format(ts.DateLayout)
	}
	m := posted.In(ts.MSK)
	day := time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, ts.MSK)
	best, bestGap := "", time.Duration(-1)
	for _, y := range []int{m.Year() - 1, m.Year(), m.Year() + 1} {
		d := time.Date(y, time.Month(p.Month), p.Day, 0, 0, 0, 0, ts.MSK)
		if d.Day() != p.Day {
			continue // 29.02 in a year without it
		}
		gap := d.Sub(day)
		if gap < 0 {
			gap = -gap
		}
		if bestGap < 0 || gap < bestGap {
			best, bestGap = d.Format(ts.DateLayout), gap
		}
	}
	return best
}

// PagesMicros is the sum of the pages in the shift.
func (p ParsedPost) PagesMicros() int64 {
	var s int64
	for _, pg := range p.Pages {
		if pg.InShift {
			s += pg.Micros
		}
	}
	return s
}
