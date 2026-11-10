package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ledger"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// Settings are the business parameters from the settings table (ledger import business). No value
// lives in code: a missing key is an error that names the key.
type Settings struct{ raw map[string]string }

// LoadSettings reads every setting.
func LoadSettings(ctx context.Context, q ledger.Querier) (Settings, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	s := Settings{raw: map[string]string{}}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, err
		}
		s.raw[k] = v
	}
	return s, rows.Err()
}

func (s Settings) str(key string) (string, error) {
	v := strings.TrimSpace(s.raw[key])
	if v == "" {
		return "", fmt.Errorf("нет настройки %s: ledger import business", key)
	}
	return v, nil
}

// Int is a whole number setting.
func (s Settings) Int(key string) (int, error) {
	v, err := s.str(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("настройка %s: «%s» — нужно целое ≥ 0", key, v)
	}
	return n, nil
}

// Days is a whole number of days.
func (s Settings) Days(key string) (time.Duration, error) {
	n, err := s.Int(key)
	return time.Duration(n) * 24 * time.Hour, err
}

// Float is a decimal setting.
func (s Settings) Float(key string) (float64, error) {
	v, err := s.str(key)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("настройка %s: «%s» — нужно число", key, v)
	}
	return f, nil
}

// Time is a timestamp with an offset (or a date: MSK midnight).
func (s Settings) Time(key string) (time.Time, error) {
	v, err := s.str(key)
	if err != nil {
		return time.Time{}, err
	}
	t, err := ts.Parse(v)
	if err != nil {
		return t, fmt.Errorf("настройка %s: %w", key, err)
	}
	return t, nil
}

// List is a comma-separated list of non-empty words.
func (s Settings) List(key string) ([]string, error) {
	v, err := s.str(key)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("настройка %s: пустой элемент в «%s»", key, v)
		}
		out = append(out, p)
	}
	return out, nil
}

// Set is List as a set.
func (s Settings) Set(key string) (map[string]bool, error) {
	l, err := s.List(key)
	out := map[string]bool{}
	for _, x := range l {
		out[x] = true
	}
	return out, err
}

// Map is a JSON object of strings.
func (s Settings) Map(key string) (map[string]string, error) {
	v, err := s.str(key)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("настройка %s: нужен JSON-объект строк: %v", key, err)
	}
	return out, nil
}

// settingKinds validates the known keys on import; other keys are stored as they are.
var settingKinds = map[string]string{
	"share": "float", "k_norm_low": "float", "k_norm_high": "float",
	"hold_days": "int", "lookback_days": "int", "hub_tail_days": "int", "hub_full_every_days": "int", "hub_rpm": "int",
	"full_ledger_from": "time", "hub_window_start": "time", "accrual_from": "date", SettingTeamSince: "date",
	"fansly_payout_methods": "list", "hold_types": "list",
	"hub_model_slugs": "map", "tab_aliases": "map", "sheet_pages": "map",
	"owner_salary_rate": "owner_rates", "category_caps": "caps",
}

func checkSetting(key, value string) error {
	s := Settings{raw: map[string]string{key: value}}
	var err error
	switch settingKinds[key] {
	case "float":
		_, err = s.Float(key)
	case "int":
		_, err = s.Int(key)
		if err == nil && key == "hub_rpm" {
			if n, _ := s.Int(key); n < 1 {
				err = fmt.Errorf("настройка hub_rpm: нужно ≥ 1")
			}
		}
	case "time":
		_, err = s.Time(key)
	case "date":
		if _, derr := time.Parse(ts.DateLayout, strings.TrimSpace(value)); derr != nil {
			err = fmt.Errorf("настройка %s: нужна дата ГГГГ-ММ-ДД, не «%s»", key, value)
		}
	case "list":
		_, err = s.List(key)
	case "map":
		_, err = s.Map(key)
	case "owner_rates":
		_, err = ledger.ParseOwnerRates(value)
	case "caps":
		caps := map[string]float64{}
		if jerr := json.Unmarshal([]byte(value), &caps); jerr != nil {
			err = fmt.Errorf("настройка %s: нужен JSON-объект {\"категория\": лимит}: %v", key, jerr)
		}
	}
	return err
}
