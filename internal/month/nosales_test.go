package month

import (
	"strings"
	"testing"
	"time"

	"github.com/goslingmanagment/ledger/internal/hub"
)

// withTelegram is the golden source with a Telegram page in the key's grant: it has no sales and Hub proves nothing of it.
type withTelegram struct{ fakeSource }

func (f withTelegram) Pages() ([]hub.Page, error) {
	pages, err := f.fakeSource.Pages()
	return append(pages, hub.Page{Label: "alfa-tg", Platform: "telegram", Model: "Alfa"}), err
}

func (f withTelegram) SummaryNet(page string, from, to time.Time) (float64, error) {
	if page == "alfa-tg" {
		return 0, &hub.Refusal{Msg: "хаб alfa-tg transactions: не выгружено (ledger hub sync)"}
	}
	return f.fakeSource.SummaryNet(page, from, to)
}

// A page of a platform without sales in the grant does not make the month refuse: it is skipped, and the card says so.
func TestAPageWithoutSalesDoesNotStopTheMonth(t *testing.T) {
	g, rows := loadGolden(t)
	cv, err := BuildCover(goldenConfig(g), withTelegram{fakeSource{g}}, rows, parseT(g.AsOf))
	if err != nil {
		t.Fatal(err)
	}
	card, err := BuildCard(cv, "2026-09", g.Tabs)
	if err != nil {
		t.Fatalf("the month refused: %v", err)
	}
	if !strings.Contains(strings.Join(card.Notes, "\n"), "alfa-tg (telegram): площадка без продаж — в карточку не входит") {
		t.Fatalf("notes %v", card.Notes)
	}
	for _, l := range card.Lines {
		if strings.HasPrefix(l.Channel, "alfa-tg") {
			t.Fatalf("a line of the Telegram page: %+v", l)
		}
	}
}
