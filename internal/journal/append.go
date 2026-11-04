package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/money"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Write says who writes a row and why: what ledger itself adds to the journal after the switchover.
type Write struct {
	Author     string
	Origin     string
	ProposalID int64
	DisputeID  int64
	EventIDs   []int64
	Now        time.Time
}

// Sanitize keeps a row printable: a cell cannot hold the column separator or a line break.
func Sanitize(r Row) Row {
	clean := func(s string) string {
		s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "|", "/").Replace(s)
		return strings.TrimSpace(s)
	}
	r.Date, r.Type, r.Category, r.Counterparty = clean(r.Date), clean(r.Type), clean(r.Category), clean(r.Counterparty)
	r.Model, r.Amount, r.Currency, r.Method = clean(r.Model), clean(r.Amount), clean(r.Currency), clean(r.Method)
	r.Status, r.Note = clean(r.Status), clean(r.Note)
	return r
}

// CheckCells applies the checks Parse makes on a file line, so a written row reads back from the mirror.
func CheckCells(r Row) error {
	switch {
	case !dateRe.MatchString(r.Date):
		return fmt.Errorf("дата «%s»", r.Date)
	case r.Type != "income" && r.Type != "expense" && r.Type != "transfer":
		return fmt.Errorf("тип «%s»", r.Type)
	case r.Status != "paid" && r.Status != "pending" && r.Status != "cancelled":
		return fmt.Errorf("статус «%s»", r.Status)
	case !amountRe.MatchString(r.Amount):
		return fmt.Errorf("сумма «%s»", r.Amount)
	case r.Currency != "USD" && r.Currency != "USDT":
		return fmt.Errorf("валюта «%s»", r.Currency)
	case r.Category == "":
		return errors.New("нет категории")
	case r.Counterparty == "":
		return errors.New("нет контрагента")
	case r.Model == "":
		return errors.New("нет модели (без модели — «—»)")
	case r.Method == "":
		return errors.New("нет метода")
	case r.Note == "":
		return errors.New("нет примечания")
	}
	if _, err := time.Parse(ts.DateLayout, r.Date); err != nil {
		return fmt.Errorf("дата «%s»", r.Date)
	}
	_, err := money.Parse(r.Amount)
	return err
}

// RowsCheck is a further check of the whole journal before rows are written: the month card refuses to
// count a journal its transfers do not fit, so such a row must not get in.
var RowsCheck func(ctx context.Context, q ledger.Querier, rows []Row) error

// LoadContext builds what the mapping of a row needs from the current journal plus rows about to be
// added (extra), and returns all those rows in number order for Lint.
func LoadContext(ctx context.Context, q ledger.Querier, extra ...Row) (*Context, []Row, error) {
	c := &Context{FirstOfDeal: map[string]int{}, Models: map[string]bool{}}
	var models string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'accrual_from'`).Scan(&c.AccrualFrom)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, err
	}
	err = q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'models'`).Scan(&models)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, err
	}
	for _, m := range strings.Split(models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			c.Models[m] = true
		}
	}
	if c.Overrides, err = loadOverrides(ctx, q); err != nil {
		return nil, nil, err
	}
	docs, err := ledger.DocumentsAsOf(ctx, q, "")
	if err != nil {
		return nil, nil, err
	}
	byNum := map[int]Row{}
	nums := make([]int, 0, len(docs)+len(extra))
	for _, d := range docs {
		byNum[d.Num] = rowFromDoc(d)
		nums = append(nums, d.Num)
	}
	for _, r := range extra {
		if _, ok := byNum[r.Num]; !ok {
			nums = append(nums, r.Num)
		}
		byNum[r.Num] = r
	}
	rows := make([]Row, 0, len(nums))
	for _, n := range nums {
		rows = append(rows, byNum[n])
	}
	for _, r := range SortedByDate(rows) {
		t, err := RowTags(r)
		if err != nil {
			continue
		}
		if t.Deal != nil && r.Status == "paid" && r.Type == "income" && !notOff[r.Category] {
			if _, ok := c.FirstOfDeal[t.Deal.ID]; !ok {
				c.FirstOfDeal[t.Deal.ID] = r.Num
			}
		}
	}
	return c, rows, nil
}

// NextNum is the next free journal number.
func NextNum(ctx context.Context, q ledger.Querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(max(num), 0) + 1 FROM documents`).Scan(&n)
	return n, err
}

// Check says whether the rows, as the next rows of the journal, would be accepted and what they post.
// A row keeps its number when it has one (a correction), otherwise it takes the next free number.
func Check(ctx context.Context, q ledger.Querier, rows []Row) ([]Row, [][]ledger.Entry, error) {
	next, err := NextNum(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		r = Sanitize(r)
		if r.Num == 0 {
			r.Num = next
			next++
		}
		if err := CheckCells(r); err != nil {
			return nil, nil, fmt.Errorf("#%d: %w", r.Num, err)
		}
		out = append(out, r)
	}
	c, all, err := LoadContext(ctx, q, out...)
	if err != nil {
		return nil, nil, err
	}
	if errs := Lint(all); len(errs) > 0 {
		return nil, nil, errs[0]
	}
	if RowsCheck != nil {
		if err := RowsCheck(ctx, q, all); err != nil {
			return nil, nil, err
		}
	}
	entries := make([][]ledger.Entry, 0, len(out))
	for _, r := range out {
		t, err := RowTags(r)
		if err != nil {
			return nil, nil, err
		}
		want, err := Entries(r, t, c)
		if err != nil {
			return nil, nil, fmt.Errorf("#%d: %w", r.Num, err)
		}
		entries = append(entries, want)
	}
	return out, entries, nil
}

func put(ctx context.Context, q ledger.Querier, r Row, want []ledger.Entry, w Write) (ledger.Document, error) {
	t, err := RowTags(r)
	if err != nil {
		return ledger.Document{}, err
	}
	d := docFromRow(r, t.JSON(), ts.Format(w.Now), w.Origin, w.Author)
	d.ProposalID, d.DisputeID = w.ProposalID, w.DisputeID
	stored, isNew, err := ledger.PutDocument(ctx, q, d, w.Now)
	if err != nil {
		return stored, err
	}
	if !isNew {
		return stored, fmt.Errorf("#%d: строка не изменилась", r.Num)
	}
	for _, e := range want {
		e.DocumentID, e.Author = stored.ID, w.Author
		e.ProposalID, e.DisputeID, e.EventIDs = w.ProposalID, w.DisputeID, w.EventIDs
		if _, err := ledger.Post(ctx, q, e, w.Now); err != nil {
			return stored, fmt.Errorf("#%d: %w", r.Num, err)
		}
	}
	return stored, nil
}

// Append writes rows as the next rows of the journal, with their entries, in q's transaction. The
// numbers are taken here; nothing is written when any row fails the checks.
func Append(ctx context.Context, q ledger.Querier, rows []Row, w Write) ([]ledger.Document, error) {
	for i := range rows {
		rows[i].Num = 0
	}
	checked, entries, err := Check(ctx, q, rows)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Document, 0, len(checked))
	for i, r := range checked {
		d, err := put(ctx, q, r, entries[i], w)
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, settleDeals(ctx, q, checked, w)
}

// settleDeals reposts the other rows of the written rows' deals. The model's share sits on the deal's first
// paid row, so a row dated earlier, or a corrected date or status, moves it from one row to another.
func settleDeals(ctx context.Context, q ledger.Querier, written []Row, w Write) error {
	deals := map[string]bool{}
	for _, r := range written {
		if t, err := RowTags(r); err == nil && t.Deal != nil {
			deals[t.Deal.ID] = true
		}
	}
	if len(deals) == 0 {
		return nil
	}
	c, all, err := LoadContext(ctx, q)
	if err != nil {
		return err
	}
	active, err := loadActive(ctx, q)
	if err != nil {
		return err
	}
	for _, r := range all {
		t, err := RowTags(r)
		if err != nil || t.Deal == nil || !deals[t.Deal.ID] {
			continue
		}
		want, err := Entries(r, t, c)
		if err != nil {
			return fmt.Errorf("#%d: %w", r.Num, err)
		}
		sigs := make([]string, 0, len(want))
		for _, e := range want {
			sigs = append(sigs, Signature(e))
		}
		have := active[r.Num]
		if have != nil && sameSet(have.sigs, sigs) {
			continue
		}
		if have != nil {
			for _, id := range have.ids {
				if _, err := ledger.Reverse(ctx, q, id, ledger.Reversal{Author: w.Author, DisputeID: w.DisputeID,
					Memo: fmt.Sprintf("сторно #%d: сменилась первая строка сделки", r.Num)}, w.Now); err != nil {
					return err
				}
			}
		}
		doc, err := ledger.CurrentDocument(ctx, q, r.Num)
		if err != nil {
			return err
		}
		for _, e := range want {
			e.DocumentID, e.Author, e.DisputeID = doc.ID, w.Author, w.DisputeID
			if _, err := ledger.Post(ctx, q, e, w.Now); err != nil {
				return fmt.Errorf("#%d: %w", r.Num, err)
			}
		}
	}
	return nil
}

// Correction is what a corrected row did: the new version, the reversals and the new entries.
type Correction struct {
	Document  ledger.Document `json:"document"`
	Reversals []int64         `json:"reversals"`
	Posted    int             `json:"posted"`
}

// Correct writes r as a new version of its row: the entries of the old version are reversed and the
// new ones posted, all carrying w.DisputeID. Nothing old is changed; entries recorded now do not touch
// what a closed month shows «на закрытии».
func Correct(ctx context.Context, q ledger.Querier, r Row, w Write) (Correction, error) {
	var res Correction
	if r.Num <= 0 {
		return res, errors.New("исправление без номера строки")
	}
	if _, err := ledger.CurrentDocument(ctx, q, r.Num); err != nil {
		if err == sql.ErrNoRows {
			return res, fmt.Errorf("строки #%d нет", r.Num)
		}
		return res, err
	}
	checked, entries, err := Check(ctx, q, []Row{r})
	if err != nil {
		return res, err
	}
	active, err := loadActive(ctx, q)
	if err != nil {
		return res, err
	}
	if have := active[r.Num]; have != nil {
		for _, id := range have.ids {
			rev, err := ledger.Reverse(ctx, q, id, ledger.Reversal{Author: w.Author, DisputeID: w.DisputeID,
				Memo: fmt.Sprintf("сторно #%d по спору %d", r.Num, w.DisputeID)}, w.Now)
			if err != nil {
				return res, err
			}
			res.Reversals = append(res.Reversals, rev)
		}
	}
	if res.Document, err = put(ctx, q, checked[0], entries[0], w); err != nil {
		return res, err
	}
	res.Posted = len(entries[0])
	return res, settleDeals(ctx, q, checked, w)
}

// RowFromDocument is the journal row of a stored document.
func RowFromDocument(d ledger.Document) Row { return rowFromDoc(d) }

// Effect sums the lines of the entries on one account; an account ending in ":" is a prefix.
func Effect(entries []ledger.Entry, account string) money.Micros {
	var sum money.Micros
	for _, e := range entries {
		for _, l := range e.Lines {
			if l.Account == account || (strings.HasSuffix(account, ":") && strings.HasPrefix(l.Account, account)) {
				sum += l.Amount
			}
		}
	}
	return sum
}
