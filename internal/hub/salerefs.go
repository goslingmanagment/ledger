package hub

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/goslingmanagment/ledger/internal/ledger"
)

// One sale, two references. Hub names a sale by its own id of the transaction — the key of the row in its transactions
// dataset (hub_transactions.key): that is what its team facts (hub_sales.transaction_ref), its decisions about whose a
// sale is (hub_sale_decisions.transaction_ref) and the lines of the reports handed in in Hub (hub_shift_report_lines)
// carry. The platform names the same sale by its own reference of the transaction (hub_transactions.ref): that is what
// the reports the team's agent saves from the posts carry (shift_report_lines.transaction_ref). A row one of ledger's
// own writers made — a pot purchase, a decision, a question — carries whichever its writer had, and keeps it: a stored
// reference is never rewritten.
//
// The mirror of Hub's transactions is the one place that holds both, so whatever asks «is this the same sale» asks
// here: SaleRefsOf for a reference of either kind, PlatformRefs for many of Hub's ids, SameSaleSQL inside a query. The
// mirror is read hourly and only for the pages it proves, so it may not have a sale yet: then the other reference is
// not known, and the caller decides what it does without it.

// SaleRefs are the two references of one sale.
type SaleRefs struct {
	// Given is the reference that was asked about.
	Given string
	// HubID is Hub's id of the sale and PlatformRef the platform's reference of its transaction, by the mirror of Hub's
	// transactions; both "" — the mirror does not have the sale. PlatformRef alone is "" when the mirror's row has none.
	HubID, PlatformRef string
}

// Known says whether the mirror of Hub's transactions has the sale.
func (r SaleRefs) Known() bool { return r.HubID != "" }

// Hub is Hub's id of the sale as far as it is known: the mirror's, else the reference given — which may be Hub's id of
// a sale the mirror does not have yet.
func (r SaleRefs) Hub() string {
	if r.Known() {
		return r.HubID
	}
	return r.Given
}

// Either is what a stored reference of either kind is compared with — `transaction_ref IN (?, ?)`: the sale's two
// references, or the one given, twice, while the mirror does not have the sale.
func (r SaleRefs) Either() (string, string) {
	switch {
	case !r.Known():
		return r.Given, r.Given
	case r.PlatformRef == "":
		return r.HubID, r.HubID
	}
	return r.HubID, r.PlatformRef
}

// SaleRefsOf finds the sale a reference names — Hub's id or the platform's reference, whichever it is — and returns
// both. Hub's ids are short numbers and the platforms' references long strings, so one string is not both; should it
// ever be, it is read as Hub's id.
func SaleRefsOf(ctx context.Context, q ledger.Querier, ref string) (SaleRefs, error) {
	out := SaleRefs{Given: ref}
	if ref == "" {
		return out, nil
	}
	var platform sql.NullString
	err := q.QueryRowContext(ctx, `SELECT key, ref FROM hub_transactions WHERE key = ? OR ref = ?
		ORDER BY key = ? DESC, updated_at DESC, page_label LIMIT 1`, ref, ref, ref).Scan(&out.HubID, &platform)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return SaleRefs{Given: ref}, err
	}
	out.PlatformRef = platform.String
	return out, nil
}

// PlatformRefs are the platforms' references of the sales Hub's ids name, as far as the mirror has them, in order and
// without repeats.
func PlatformRefs(ctx context.Context, q ledger.Querier, hubIDs []string) ([]string, error) {
	if len(hubIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(hubIDs))
	for i, id := range hubIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT ref FROM hub_transactions WHERE ref IS NOT NULL AND ref <> ''
		AND key IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(hubIDs)), ", ")+`) ORDER BY ref`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// SameSaleSQL is the SQL condition «the references in a and b name one sale» for two columns (or expressions) that
// each hold a reference of either kind: they are equal, or they are the two references of one row of the mirror of
// Hub's transactions. NULL in either is no sale.
func SameSaleSQL(a, b string) string {
	return "(" + a + " = " + b +
		" OR " + a + " IN (SELECT sr.ref FROM hub_transactions sr WHERE sr.key = " + b + ")" +
		" OR " + a + " IN (SELECT sr.key FROM hub_transactions sr WHERE sr.ref = " + b + "))"
}
