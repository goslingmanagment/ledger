package journal

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// GET /v1/payouts/counterparty-payments is what the journal paid to or got from the counterparties Hub names, for Hub's
// owner screen «Трафик → Источники» (what a traffic contractor was paid beside what its links brought). Hub's server asks
// with its `payouts` key for the names of its contractors as the journal writes them («Кому/Откуда»), and only that scope
// opens the route (a read key does not). It reads the latest version of each row of type expense or income that is paid
// or pending, whose counterparty is one of the names after trimming, in any case; it writes nothing.

// Limits of the read.
const (
	MaxCounterpartyNames    = 50   // names in one request
	MaxCounterpartyNameLen  = 200  // characters of a name after trimming
	MaxCounterpartyPayments = 2000 // rows in one answer: the newest ones, truncated says more matched
	MaxCounterpartyNoteLen  = 500  // characters of a row's note in the answer
)

// CounterpartyPayment is one journal row as Hub reads it.
type CounterpartyPayment struct {
	Num          int    `json:"num"`
	Date         string `json:"date"`
	Type         string `json:"type"` // expense or income
	Category     string `json:"category"`
	Counterparty string `json:"counterparty"` // as the journal writes it
	Model        string `json:"model"`
	AmountMicros int64  `json:"amountMicros"`
	Currency     string `json:"currency"`
	Status       string `json:"status"` // paid or pending
	Note         string `json:"note"`   // cut to MaxCounterpartyNoteLen characters
}

// CounterpartyPayments is the reply of GET /v1/payouts/counterparty-payments.
type CounterpartyPayments struct {
	OK          bool                  `json:"ok"`
	GeneratedAt string                `json:"generatedAt"` // RFC 3339, UTC
	Truncated   bool                  `json:"truncated"`   // more rows matched than MaxCounterpartyPayments
	Payments    []CounterpartyPayment `json:"payments"`    // newest first: date, then num, descending
}

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/payouts/counterparty-payments", auth.Require(s, auth.ScopePayouts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			names, err := counterpartyNames(q["name"])
			if err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			from, to := q.Get("from"), q.Get("to")
			for _, d := range []struct{ name, v string }{{"from", from}, {"to", to}} {
				if d.v == "" {
					continue
				}
				if t, err := time.Parse(ts.DateLayout, d.v); err != nil || t.Format(ts.DateLayout) != d.v {
					server.WriteError(w, http.StatusBadRequest, fmt.Sprintf("%s: нужна дата ГГГГ-ММ-ДД, а не %q", d.name, d.v))
					return
				}
			}
			if from != "" && to != "" && to < from {
				server.WriteError(w, http.StatusBadRequest, fmt.Sprintf("окно %s — %s: from позже to", from, to))
				return
			}
			ans, err := ReadCounterpartyPayments(r.Context(), s.Store.RO, names, from, to, time.Now())
			if err != nil {
				s.Log.Error("counterparty payments", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "платежи подрядчикам не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, ans)
		})))
	})
}

// counterpartyNames checks the names of a request: 1 to MaxCounterpartyNames, each 1 to MaxCounterpartyNameLen characters
// after trimming.
func counterpartyNames(raw []string) ([]string, error) {
	if len(raw) == 0 || len(raw) > MaxCounterpartyNames {
		return nil, fmt.Errorf("name: нужно от 1 до %d имён, а не %d", MaxCounterpartyNames, len(raw))
	}
	out := make([]string, 0, len(raw))
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if l := utf8.RuneCountInString(n); l == 0 || l > MaxCounterpartyNameLen || !utf8.ValidString(n) {
			return nil, fmt.Errorf("name: имя от 1 до %d знаков, а не %q", MaxCounterpartyNameLen, n)
		}
		out = append(out, n)
	}
	return out, nil
}

// counterpartyKey is how a counterparty is compared: trimmed and lower-cased in Go, Cyrillic included (SQLite's lower()
// knows only ASCII).
func counterpartyKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ReadCounterpartyPayments reads the latest version of every expense or income row, paid or pending, dated from…to
// (inclusive; "" — open), whose counterparty is one of names; the newest MaxCounterpartyPayments of them.
func ReadCounterpartyPayments(ctx context.Context, q ledger.Querier, names []string, from, to string, now time.Time) (CounterpartyPayments, error) {
	ans := CounterpartyPayments{OK: true, GeneratedAt: now.UTC().Format(time.RFC3339), Payments: []CounterpartyPayment{}}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[counterpartyKey(n)] = true
	}
	query := `SELECT num, date, type, category, counterparty, model, amount_micros, currency, status, note FROM v_documents
		WHERE type IN ('expense', 'income') AND status IN ('paid', 'pending')`
	var args []any
	if from != "" {
		query += ` AND date >= ?`
		args = append(args, from)
	}
	if to != "" {
		query += ` AND date <= ?`
		args = append(args, to)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY date DESC, num DESC`, args...)
	if err != nil {
		return ans, err
	}
	defer rows.Close()
	for rows.Next() {
		var p CounterpartyPayment
		if err := rows.Scan(&p.Num, &p.Date, &p.Type, &p.Category, &p.Counterparty, &p.Model, &p.AmountMicros, &p.Currency,
			&p.Status, &p.Note); err != nil {
			return ans, err
		}
		if !want[counterpartyKey(p.Counterparty)] {
			continue
		}
		if len(ans.Payments) == MaxCounterpartyPayments {
			ans.Truncated = true
			break
		}
		if r := []rune(p.Note); len(r) > MaxCounterpartyNoteLen {
			p.Note = string(r[:MaxCounterpartyNoteLen])
		}
		ans.Payments = append(ans.Payments, p)
	}
	return ans, rows.Err()
}
