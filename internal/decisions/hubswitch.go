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

// This switch governs the writers of decisions and questions, pot purchases and report-line agreements.
// Payroll facts take computed sale homes directly from Hub on both sides of this date.

// SettingHubFrom is the first Moscow day on which only Hub writes the decisions about whose a sale is; empty — ledger too.
const SettingHubFrom = "sale_decisions_hub_from"

// What a refusal is about.
const (
	HubDecisions = "decisions"
	HubQuestions = "questions"
)

var hubLines = map[string]string{
	HubDecisions: "решения «чья продажа» с %s пишет только хаб: hub sale decide (что решено — hub sale decisions); ledger decision list — история до этого дня",
	HubQuestions: "вопросы «чья продажа» с %s ведёт только хаб: hub sale ask, ответ — hub sale answer (открытые — hub sale questions); ledger question list — история до этого дня",
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

const hubFromNote = "С какого дня (МСК) решения и вопросы о продажах ведёт только хаб. У продаж этого дня и позже покупки общаков " +
	"и договорённости строк отчётов читают решения хаба; у более ранних продаж — записи ledger. Факты всегда читают дома продаж хаба."

// HubOnly returns the writer refusal for decisions or questions from the switch day.
func HubOnly(ctx context.Context, q ledger.Querier, now time.Time, what string) (string, error) {
	day, err := HubFrom(ctx, q)
	if err != nil || day == "" || ts.Date(now) < day {
		return "", err
	}
	return fmt.Sprintf(hubLines[what], day), nil
}

// HubRules says whether Hub's word rules a sale of the Moscow day given: the switch day is set and the day is not before
// it. from is HubFrom's answer.
func HubRules(from, day string) bool { return from != "" && day >= from }

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
