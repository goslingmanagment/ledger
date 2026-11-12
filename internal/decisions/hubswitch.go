package decisions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// One home for «чья продажа»: Hub writes the decisions, the questions and the owner's word on a nobody's sale (its agent
// door `hub sale decide|decisions|ask|answer|questions`). From settings.sale_decisions_hub_from ledger writes none of
// them — `ledger decision add|applied|cancel`, `ledger question add|answer|drop` and `ledger accruals claim|unclaim`,
// beside the database and over HTTP, refuse with one line naming Hub's command — and keeps what it has as history:
// `decision list` and `question list` read as before. Empty — ledger writes them as it always did.

// SettingHubFrom is the first Moscow day on which only Hub writes the decisions about whose a sale is; empty — ledger too.
const SettingHubFrom = "sale_decisions_hub_from"

// What a refusal is about.
const (
	HubDecisions = "decisions"
	HubQuestions = "questions"
	HubClaims    = "claims"
)

var hubLines = map[string]string{
	HubDecisions: "решения «чья продажа» с %s пишет только хаб: hub sale decide (что решено — hub sale decisions); ledger decision list — история до этого дня",
	HubQuestions: "вопросы «чья продажа» с %s ведёт только хаб: hub sale ask, ответ — hub sale answer (открытые — hub sale questions); ledger question list — история до этого дня",
	HubClaims:    "ничью продажу человеку с %s отдаёт только решение в хабе: hub sale decide; заявки ledger до этого дня — история (v_sale_claims)",
}

// HubFrom is the switch day, "" when ledger still writes the decisions.
func HubFrom(ctx context.Context, q ledger.Querier) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, SettingHubFrom).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(ts.DateLayout, v); err != nil || t.Format(ts.DateLayout) != v {
		return "", fmt.Errorf("настройка %s: нужна дата ГГГГ-ММ-ДД, а не %q", SettingHubFrom, v)
	}
	return v, nil
}

// SetHubFrom sets the switch day ("" — ledger writes again).
func SetHubFrom(ctx context.Context, q ledger.Querier, day string, now time.Time) error {
	if day != "" {
		if t, err := time.Parse(ts.DateLayout, day); err != nil || t.Format(ts.DateLayout) != day {
			return fmt.Errorf("дата: нужна ГГГГ-ММ-ДД, а не %q", day)
		}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO settings(key, value, note, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, SettingHubFrom, day, hubFromNote, ts.Format(now))
	return err
}

const hubFromNote = "С какого дня (МСК) решения «чья продажа», вопросы о ней и слово владельца о ничьей продаже пишет только хаб " +
	"(hub sale decide|ask|answer); ledger decision add|applied|cancel, question add|answer|drop и accruals claim|unclaim отказывают, " +
	"списки остаются историей. Пусто — пишет и ledger. Ставит ledger decision hub-from"

// HubOnly says, from the switch day on, why ledger does not write this (what: HubDecisions, HubQuestions, HubClaims);
// "" — ledger writes it.
func HubOnly(ctx context.Context, q ledger.Querier, now time.Time, what string) (string, error) {
	day, err := HubFrom(ctx, q)
	if err != nil || day == "" || ts.Date(now) < day {
		return "", err
	}
	return fmt.Sprintf(hubLines[what], day), nil
}

// hubOnly is HubOnly as a refusal.
func hubOnly(ctx context.Context, q ledger.Querier, now time.Time, what string) error {
	why, err := HubOnly(ctx, q, now, what)
	if err != nil {
		return err
	}
	if why != "" {
		return &Refusal{Msg: why}
	}
	return nil
}
