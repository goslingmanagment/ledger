package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// ParseOwnerRates reads settings.owner_salary_rate: one number (the rate at any time, under the key "")
// or a JSON object {"ГГГГ-ММ-ДД": ставка} of the days each rate starts on.
func ParseOwnerRates(raw string) (map[string]float64, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("настройка owner_salary_rate: нужно число или JSON-объект {\"ГГГГ-ММ-ДД\": ставка}, не «%s»", raw)
		}
		return map[string]float64{"": v}, nil
	}
	var by map[string]float64
	if err := json.Unmarshal([]byte(raw), &by); err != nil {
		return nil, fmt.Errorf("настройка owner_salary_rate: %w", err)
	}
	for from := range by {
		if _, err := time.Parse(ts.DateLayout, from); err != nil {
			return nil, fmt.Errorf("настройка owner_salary_rate: «%s» не дата ГГГГ-ММ-ДД", from)
		}
	}
	return by, nil
}

// OwnerRate is the owner's salary rate in force on day (ГГГГ-ММ-ДД) and the day it started on ("" for a
// setting of one number). A month is judged by the rate of its first day, so a raise accepted for the next
// month does not move the month still being closed. ok is false without the setting or before its first day.
func OwnerRate(ctx context.Context, q Querier, day string) (rate float64, from string, ok bool, err error) {
	var raw string
	switch err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'owner_salary_rate'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", false, nil
	case err != nil:
		return 0, "", false, err
	}
	if strings.TrimSpace(raw) == "" {
		return 0, "", false, nil
	}
	by, err := ParseOwnerRates(raw)
	if err != nil {
		return 0, "", false, err
	}
	for d, v := range by {
		if d <= day && (!ok || d > from) {
			rate, from, ok = v, d, true
		}
	}
	return rate, from, ok, nil
}
