package reports

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

const (
	postAnna = `🗓 Дата: 08.11.2026
👤 Чаттер: #Anna

📌 Аккаунты:
Fansly
• Page-1 - не в смене
• Page-2 - $24,00
• Page-3 - $0
OF
• Page VIP - $16,00
• Page Free - $0

💰Общий доход: $40,00

📈 Основные продажи:
https://example.com/fan1 - name, ппв $24,00 …

📝 Комментарий: всё хорошо $999`

	postBoris = `Дата: 03.11.2026
👤 Чаттер: #Boris

📌 Аккаунты:
OF
-Page VIP: $76,79
-Page Free: $0,00
Fansly
-Page-2: $0,00
💰Общий доход: $76,79`

	postClara = `Дата: 03.11.2026
👤 Чаттер: #Clara
📌 Аккаунты:
OF
-Page VIP: $8
Fansly
-Page-2: $92
💰Общий доход: **$140**`

	postDenis = `🗓 Дата: 04.11.2026
👤 Чаттер: #Denis

📌 Аккаунты:
Fansly
- Page-1 - $338,40

💰Общий доход: $338,40`

	postEgor = `Дата: 05.11.2026
👤 Чаттер: #Egor

📌 Аккаунты:
-Fansly - $204,00

💰Общий доход: $204,00`
)

func pages(p ParsedPost) string {
	var out []string
	for _, pg := range p.Pages {
		if pg.InShift {
			out = append(out, fmt.Sprintf("%s=%d", pg.Name, pg.Micros/10_000))
		} else {
			out = append(out, pg.Name+"=off")
		}
	}
	return strings.Join(out, "|")
}

func TestParsePost(t *testing.T) {
	cases := []struct {
		name, text string
		report     bool
		date       string // as ReportDate gives it for a post on 2026-11-08
		tag        string
		pages      string // name=cents
		total      int64  // cents
	}{
		{"bullets, off shift, sales and comment ignored", postAnna, true, "2026-11-08", "#Anna",
			"Page-1=off|Page-2=2400|Page-3=0|Page VIP=1600|Page Free=0", 4000},
		{"dash-colon lines, sections", postBoris, true, "2026-11-03", "#Boris", "Page VIP=7679|Page Free=0|Page-2=0", 7679},
		{"bold, whole dollars, total not the sum", postClara, true, "2026-11-03", "#Clara", "Page VIP=800|Page-2=9200", 14000},
		{"one page", postDenis, true, "2026-11-04", "#Denis", "Page-1=33840", 33840},
		{"platform word carries the amount", postEgor, true, "2026-11-05", "#Egor", "Fansly=20400", 20400},
		{"dot decimals, full-width dollar, thousands", "Дата: 07.11\nЧаттер: #x\nАккаунты:\n• Page-1 - ＄1 204.50\n- Page-2 - $1,234\n" +
			"- Page-3 - $2.345,6\nОбщий доход: $4 784,10", true, "2026-11-07", "#x", "Page-1=120450|Page-2=123400|Page-3=234560", 478410},
		{"dollar after the number", "Дата: 08.11.2026\nЧаттер: #x\nАккаунты:\nPage-1: 12,5$\nОбщий доход: 12,5 $", true, "2026-11-08", "#x",
			"Page-1=1250", 1250},
		{"total on the next line", "Дата: 08.11.26\nЧаттер: #x\nАккаунты:\nPage-1 - $5\nОбщий доход:\n$5", true, "2026-11-08", "#x",
			"Page-1=500", 500},
		{"no accounts heading", "Дата: 08.11.2026\nЧаттер: #x\nPage-1 - $5\nPage-2 - не в смене\nОбщий доход - $5", true, "2026-11-08", "#x",
			"Page-1=500|Page-2=off", 500},
		{"unreadable date", "Дата: вчера\nЧаттер: #x\nPage-1 - $5\nОбщий доход: $5", true, "", "#x", "Page-1=500", 500},
		{"no tag", "Дата: 08.11.2026\nЧаттер: Anna\nPage-1 - $5\nОбщий доход: $5", true, "2026-11-08", "", "Page-1=500", 500},
		{"talk: no total", "Дата: 08.11.2026\nкто сегодня в смене?", false, "", "", "", 0},
		{"talk: no date", "Общий доход за неделю $500, молодцы", false, "", "", "", 0},
		{"total only after sales", "Дата: 08.11.2026\nОсновные продажи:\nОбщий доход: $5", false, "", "", "", 0},
	}
	posted := time.Date(2026, 11, 8, 23, 50, 0, 0, ts.MSK)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ok := ParsePost(c.text)
			if ok != c.report {
				t.Fatalf("report %v, want %v", ok, c.report)
			}
			if !ok {
				return
			}
			if got := p.ReportDate(posted); got != c.date {
				t.Errorf("date %q, want %q", got, c.date)
			}
			if p.Tag != c.tag {
				t.Errorf("tag %q, want %q", p.Tag, c.tag)
			}
			if got := pages(p); got != c.pages {
				t.Errorf("pages %s, want %s", got, c.pages)
			}
			if p.TotalMicros/10_000 != c.total {
				t.Errorf("total %d, want %d cents", p.TotalMicros/10_000, c.total)
			}
		})
	}
}

func TestDateWithoutYearAcrossTheYear(t *testing.T) {
	p, ok := ParsePost("Дата: 31.12\nЧаттер: #x\nPage-1 - $5\nОбщий доход: $5")
	if !ok {
		t.Fatal("not a report")
	}
	if got := p.ReportDate(time.Date(2027, 1, 1, 0, 30, 0, 0, ts.MSK)); got != "2026-12-31" {
		t.Errorf("31.12 posted on 1 January: %s, want the year before", got)
	}
	if got := p.ReportDate(time.Date(2026, 12, 31, 23, 0, 0, 0, ts.MSK)); got != "2026-12-31" {
		t.Errorf("31.12 posted the same day: %s", got)
	}
	p, _ = ParsePost("Дата: 01.01\nЧаттер: #x\nPage-1 - $5\nОбщий доход: $5")
	if got := p.ReportDate(time.Date(2026, 12, 31, 23, 0, 0, 0, ts.MSK)); got != "2027-01-01" {
		t.Errorf("01.01 posted on 31 December: %s, want the year after", got)
	}
	// The posting time counts in Moscow: 21:30 UTC on 31 December is already 1 January.
	if got := p.ReportDate(time.Date(2026, 12, 31, 21, 30, 0, 0, time.UTC)); got != "2027-01-01" {
		t.Errorf("01.01 posted just after Moscow midnight: %s", got)
	}
	if _, ok := ParsePost("Дата: 31.02.2026\nЧаттер: #x\nОбщий доход: $5"); !ok {
		t.Error("an impossible date is still a report")
	} else if p, _ := ParsePost("Дата: 31.02.2026\nЧаттер: #x\nОбщий доход: $5"); p.DateOK {
		t.Error("31.02 read as a date")
	}
}

func TestPlatformWordAndNames(t *testing.T) {
	if PlatformOf("-Fansly ") != "fansly" || PlatformOf("OF") != "onlyfans" || PlatformOf("Page-1") != "" {
		t.Error("platform words")
	}
	if NormName(" • Page VIP ") != "page vip" || NormName("-Page-2") != "page-2" {
		t.Errorf("names: %q %q", NormName(" • Page VIP "), NormName("-Page-2"))
	}
}
