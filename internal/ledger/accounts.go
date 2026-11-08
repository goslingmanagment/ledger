package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type pattern struct {
	re     *regexp.Regexp
	kind   string
	grp    string
	liquid int
	name   func(m []string) string
}

// StableCoins are cash on `bybit`; any other coin of the exchange stands on its own account bybit:<coin>.
var StableCoins = map[string]bool{"USDT": true, "USDC": true, "USD": true, "DAI": true}

// CoinAccount is the account of a coin on Bybit: `bybit` for a stable coin, bybit:<coin> for any other.
func CoinAccount(coin string) string {
	coin = strings.ToUpper(strings.TrimSpace(coin))
	if StableCoins[coin] {
		return "bybit"
	}
	return "bybit:" + strings.ToLower(coin)
}

// Accounts named after a page, a person, a card, a service or a coin are opened on first use; their
// names live only in the database. A coin account holds the coin in dollars at what it cost (bybit:btc
// came with the chart; SOL, ETH and the next coins open here, a stable coin is cash on `bybit`).
var patterns = []pattern{
	{regexp.MustCompile(`^bybit:([a-z][a-z0-9]*)$`), "asset", "money", 0, func(m []string) string {
		return "Bybit, " + strings.ToUpper(m[1]) + " (в долларах по цене покупки)"
	}},
	{regexp.MustCompile(`^card:(\d{4})$`), "asset", "money", 1, func(m []string) string { return "Карта CinCin " + m[1] }},
	{regexp.MustCompile(`^prepaid:([a-z0-9_.-]+)$`), "asset", "prepaid", 0, func(m []string) string { return "Предоплата сервису " + m[1] }},
	{regexp.MustCompile(`^page:([a-z0-9-]+):hold$`), "asset", "page", 0, func(m []string) string { return "Страница " + m[1] + ": в холде" }},
	{regexp.MustCompile(`^page:([a-z0-9-]+):available$`), "asset", "page", 0, func(m []string) string { return "Страница " + m[1] + ": доступно к выводу" }},
	{regexp.MustCompile(`^page:([a-z0-9-]+):requested$`), "asset", "page", 0, func(m []string) string { return "Страница " + m[1] + ": заявлено на вывод" }},
	{regexp.MustCompile(`^model:([a-z0-9-]+):due$`), "asset", "model_due", 0, func(m []string) string { return "У модели " + m[1] + " до перевода" }},
	{regexp.MustCompile(`^payable:([a-z0-9_-]+)$`), "liability", "payable", 0, func(m []string) string { return "Долг человеку " + m[1] }},
	{regexp.MustCompile(`^held:([a-z0-9_-]+)$`), "asset", "held", 0, func(m []string) string { return "Деньги агентства у " + m[1] }},
	{regexp.MustCompile(`^income:platforms:([a-z0-9-]+)$`), "income", "income", 0, func(m []string) string { return "Доход платформ: " + m[1] }},
	{regexp.MustCompile(`^income:true_up:([a-z0-9-]+)$`), "income", "income", 0, func(m []string) string { return "Поправка на фактический k: " + m[1] }},
	{regexp.MustCompile(`^expense:traffic:([a-z0-9_-]+)$`), "expense", "expense", 0, func(m []string) string { return "Трафик: " + m[1] }},
}

// EnsureAccount checks that code exists, opening it when it matches a known pattern.
func EnsureAccount(ctx context.Context, q Querier, code, openedAt string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE code = ?`, code).Scan(&one)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if coin, ok := strings.CutPrefix(code, "bybit:"); ok && StableCoins[strings.ToUpper(coin)] {
		return fmt.Errorf("счёт %q: стейблкоин — деньги на счёте bybit, своего счёта у него нет", code)
	}
	for _, p := range patterns {
		if m := p.re.FindStringSubmatch(code); m != nil {
			subject := m[1]
			_, err := q.ExecContext(ctx, `INSERT INTO accounts(code, name, kind, grp, subject, liquid, opened_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, code, p.name(m), p.kind, p.grp, subject, p.liquid, openedAt)
			return err
		}
	}
	return fmt.Errorf("unknown account %q (not in the chart and no pattern matches)", code)
}

// Slug normalises a model or person name for an account code: "Alfa" → "alfa".
func Slug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
