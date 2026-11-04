package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/db"
	"github.com/goslingmanagment/ledger/internal/month"
	"github.com/goslingmanagment/ledger/internal/ts"
)

func init() {
	Command(Cmd{Name: "month show", Summary: "карточка месяца: ledger month show ГГГГ-ММ [--as-of] [--ceo-rate N] [--json]", Run: monthShow})
	Command(Cmd{Name: "month close", Summary: "закрыть месяц снимком: ledger month close ГГГГ-ММ --ceo-rate N (не раньше 8-го, один раз)", Run: monthClose})
	Command(Cmd{Name: "month brief", Summary: "сообщение владельцу в понедельник [--ceo-rate N]", Run: monthBrief})
	Command(Cmd{Name: "months", Summary: "закрытые месяцы: «сейчас» против сообщённого [--accept]", Run: months})
	Command(Cmd{Name: "month import-close", Summary: "принять снимок закрытия из /OS: ledger month import-close <ГГГГ-ММ.json>", Run: monthImportClose})
	Command(Cmd{Name: "month import-reported", Summary: "принять колонки «Сообщено» из /OS finance/months.md", Run: monthImportReported})
	Command(Cmd{Name: "month compare", Summary: "сверить карточку со снимком до цента: ledger month compare ГГГГ-ММ --snapshot <файл>", Run: monthCompare})
	Command(Cmd{Name: "month refresh", Summary: "пересчитать карточки месяцев сейчас (рядом с базой)", Run: monthRefresh})
	Command(Cmd{Name: "import plan", Summary: "планы оборота из closes.md: ledger import plan <файл>", Run: importPlan})
}

// local says the command runs next to the database (the server container, a local copy).
func local() bool { return os.Getenv("LEDGER_DB") != "" }

func openDirect(ctx context.Context) (*db.Store, error) {
	cfg := config.LoadServer()
	s, err := db.Open(cfg.DB)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func readTabs(path string) ([]month.Tab, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tabs []month.Tab
	if err := json.Unmarshal(raw, &tabs); err != nil {
		return nil, fmt.Errorf("--tabs: %w", err)
	}
	return tabs, nil
}

func dataExit(err error) error {
	var f month.Fail
	if errors.As(err, &f) {
		return Exit{Code: 2, Msg: f.Msg}
	}
	return err
}

func monthShow(ctx context.Context, args []string) error {
	fs := Flags("month show")
	asOf := fs.String("as-of", "", "срез (по умолчанию последний доказанный хабом)")
	rate := fs.Float64("ceo-rate", 0, "ставка CEO: вердикт предохранителя")
	docsAsOf := fs.String("docs-as-of", "", "журнал на это время (только рядом с базой)")
	tabsFile := fs.String("tabs", "", "вкладки выплат из JSON-файла (только рядом с базой)")
	asJSON := fs.Bool("json", false, "весь ответ JSON")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("нужен месяц ГГГГ-ММ")
	}
	var out map[string]any
	var ratePtr *float64
	if *rate != 0 {
		ratePtr = rate
	}
	if local() {
		in := month.Inputs{}
		if *asOf != "" {
			if in.AsOf, err = ts.Parse(*asOf); err != nil {
				return err
			}
		}
		if *docsAsOf != "" {
			t, err := ts.Parse(*docsAsOf)
			if err != nil {
				return err
			}
			in.DocsAsOf = ts.Format(t)
		}
		if *tabsFile != "" {
			if in.Tabs, err = readTabs(*tabsFile); err != nil {
				return err
			}
		}
		store, err := openDirect(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		if out, err = month.Show(ctx, store.RO, pos[0], in, ratePtr); err != nil {
			return dataExit(err)
		}
	} else {
		c, err := apiClient()
		if err != nil {
			return err
		}
		q := url.Values{}
		if *asOf != "" {
			q.Set("as_of", *asOf)
		}
		if *rate != 0 {
			q.Set("ceo_rate", fmt.Sprint(*rate))
		}
		if err := c.Do(ctx, "GET", "/v1/month/"+url.PathEscape(pos[0])+"?"+q.Encode(), nil, &out); err != nil {
			return apiError(err)
		}
	}
	if *asJSON {
		return printJSON(out)
	}
	fmt.Println(out["text"])
	if checksFailed(out["checks"]) {
		return Exit{Code: 3, Msg: "проверки не пройдены"}
	}
	return nil
}

// checksFailed reads the card's checks: typed next to the database, decoded JSON through the API.
func checksFailed(v any) bool {
	switch cs := v.(type) {
	case []month.Check:
		for _, c := range cs {
			if !c.OK {
				return true
			}
		}
	case []any:
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok && m["ok"] != true {
				return true
			}
		}
	}
	return false
}

func monthClose(ctx context.Context, args []string) error {
	fs := Flags("month close")
	rate := fs.Float64("ceo-rate", 0, "ставка CEO")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *rate == 0 {
		return fmt.Errorf("нужны месяц и --ceo-rate")
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	var out struct {
		Closed bool     `json:"closed"`
		Stops  []string `json:"stops"`
		Text   string   `json:"text"`
	}
	path := "/v1/month/" + url.PathEscape(pos[0]) + "/close?ceo_rate=" + fmt.Sprint(*rate)
	if err := c.Do(ctx, "POST", path, nil, &out); err != nil {
		return apiError(err)
	}
	if out.Text != "" {
		fmt.Println(out.Text)
		fmt.Println()
	}
	if !out.Closed {
		fmt.Println("ЗАКРЫТИЕ ОТКЛОНЕНО:\n- " + strings.Join(out.Stops, "\n- "))
		return Exit{Code: 3, Msg: "закрытие отклонено"}
	}
	fmt.Printf("%s закрыт\n", pos[0])
	return nil
}

func monthBrief(ctx context.Context, args []string) error {
	fs := Flags("month brief")
	rate := fs.Float64("ceo-rate", 0, "ставка CEO")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var text string
	var flagged int
	if local() {
		store, err := openDirect(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		var r *float64
		if *rate != 0 {
			r = rate
		}
		lines, fl, err := month.BriefAt(ctx, store.RO, time.Now(), r)
		if err != nil {
			return dataExit(err)
		}
		text, flagged = strings.Join(lines, "\n"), len(fl)
	} else {
		c, err := apiClient()
		if err != nil {
			return err
		}
		var out struct {
			Text    string `json:"text"`
			Flagged int    `json:"flagged"`
		}
		path := "/v1/month-brief"
		if *rate != 0 {
			path += "?ceo_rate=" + fmt.Sprint(*rate)
		}
		if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
			return apiError(err)
		}
		text, flagged = out.Text, out.Flagged
	}
	fmt.Println(text)
	if flagged > 0 {
		return Exit{Code: 3, Msg: "есть что сообщить владельцу"}
	}
	return nil
}

func months(ctx context.Context, args []string) error {
	fs := Flags("months")
	accept := fs.Bool("accept", false, "владелец узнал «сейчас»: записать как сообщённое")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := apiClient()
	if err != nil {
		return err
	}
	if *accept {
		var out map[string]any
		if err := c.Do(ctx, "POST", "/v1/months/accept", nil, &out); err != nil {
			return apiError(err)
		}
		return printJSON(out)
	}
	var out struct {
		Months []month.DriftRow `json:"months"`
	}
	if err := c.Do(ctx, "GET", "/v1/months", nil, &out); err != nil {
		return apiError(err)
	}
	flag := false
	for _, x := range out.Months {
		fmt.Printf("%s: на закрытии %s (%s), сообщено %s (%s), сейчас %s (%s), Δ %+.2f\n", x.Month, month.Money(x.AtClose), x.VerdictClose,
			month.Money(x.Reported), x.VerdictTold, month.Money(x.Now), x.VerdictNow, x.Delta)
		flag = flag || x.Flag
	}
	if len(out.Months) == 0 {
		fmt.Println("закрытых месяцев нет")
	}
	if flag {
		return Exit{Code: 3, Msg: "есть что сообщить владельцу"}
	}
	return nil
}

func monthImportClose(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен файл снимка")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	mo, err := month.ImportClose(ctx, tx, raw)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "month": mo})
}

func monthImportReported(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен months.md")
	}
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n, err := month.ImportReported(ctx, tx, args[0], time.Now())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "written": n})
}

func importPlan(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("нужен closes.md")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	plans, err := month.ParsePlans(string(raw))
	if err != nil {
		return err
	}
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range plans {
		if _, err := tx.ExecContext(ctx, `INSERT INTO month_plans(month, page_label, per_day, created_at, source) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(month, page_label) DO UPDATE SET per_day = excluded.per_day`, p.Month, p.Page, p.PerDay, ts.Format(time.Now()), "closes.md"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "rows": len(plans)})
}

// monthCompare is the acceptance: the card at the snapshot's cut, the sheets as they stood at the close
// (differences are listed), against every number of the snapshot.
func monthCompare(ctx context.Context, args []string) error {
	fs := Flags("month compare")
	snapFile := fs.String("snapshot", "", "снимок закрытия")
	docsAsOf := fs.String("docs-as-of", "", "журнал на это время (по умолчанию closed_at снимка)")
	tabsFile := fs.String("tabs", "", "вкладки выплат из JSON-файла вместо базы")
	pos, err := ParseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *snapFile == "" {
		return fmt.Errorf("нужны месяц и --snapshot")
	}
	raw, err := os.ReadFile(*snapFile)
	if err != nil {
		return err
	}
	var s struct {
		AsOf     string                        `json:"as_of"`
		ClosedAt string                        `json:"closed_at"`
		Payroll  map[string]map[string]float64 `json:"payroll"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	asOf, err := ts.Parse(s.AsOf)
	if err != nil {
		return err
	}
	cut := s.ClosedAt
	if *docsAsOf != "" {
		cut = *docsAsOf
	}
	cutT, err := ts.Parse(cut)
	if err != nil {
		return err
	}
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	var tabs []month.Tab
	if *tabsFile != "" {
		if tabs, err = readTabs(*tabsFile); err != nil {
			return err
		}
	} else if tabs, _, err = month.LoadTabs(ctx, store.RO); err != nil {
		return err
	}
	over, tabDiffs := month.OverlayPayroll(tabs, s.Payroll, pos[0])
	cv, _, _, err := month.PrepareCover(ctx, store.RO, month.Inputs{AsOf: asOf, DocsAsOf: ts.Format(cutT), Tabs: over})
	if err != nil {
		return dataExit(err)
	}
	card, err := month.BuildCard(cv, pos[0], over)
	if err != nil {
		return dataExit(err)
	}
	diffs, err := month.CompareSnapshot(card, raw)
	if err != nil {
		return err
	}
	if diffs == nil {
		diffs = []string{}
	}
	t := card.Totals()
	if err := printJSON(map[string]any{"month": pos[0], "asOf": ts.Format(asOf), "journalAsOf": ts.Format(cutT), "sheetCellsChangedAfterClose": tabDiffs,
		"decision": *t.Decision, "differences": diffs, "matches": len(diffs) == 0}); err != nil {
		return err
	}
	if len(diffs) > 0 {
		return Exit{Code: 3, Msg: fmt.Sprintf("%d расхождений", len(diffs))}
	}
	return nil
}

func monthRefresh(ctx context.Context, args []string) error {
	store, err := openDirect(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	tx, err := store.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cfg, err := month.LoadConfig(ctx, tx)
	if err != nil {
		return err
	}
	asOf, err := month.LiveAsOf(ctx, tx, cfg)
	if err != nil {
		return dataExit(err)
	}
	if err := month.RefreshCards(ctx, tx, asOf, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "asOf": ts.Format(asOf)})
}
