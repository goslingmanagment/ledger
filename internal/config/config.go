// Package config reads settings from the environment. Secrets never come from flags.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Server is the configuration of the service process (agency-de container).
type Server struct {
	DB         string // path to the SQLite file
	Addr       string // listen address
	Scheduler  bool   // run collectors and checks on a timer
	Deployment string
}

// LoadServer reads LEDGER_DB, LEDGER_ADDR, LEDGER_SCHEDULER, LEDGER_DEPLOYMENT.
func LoadServer() Server {
	return Server{
		DB:         env("LEDGER_DB", "/data/ledger.db"),
		Addr:       env("LEDGER_ADDR", ":8080"),
		Scheduler:  env("LEDGER_SCHEDULER", "on") != "off",
		Deployment: env("LEDGER_DEPLOYMENT", "local"),
	}
}

// Client is what an agent's CLI needs to reach the API.
type Client struct {
	URL string
	Key string
}

// LoadClient reads LEDGER_URL and LEDGER_KEY, falling back to ~/.config/ledger/credentials
// (KEY=value lines, chmod 600).
func LoadClient() (Client, error) {
	c := Client{URL: os.Getenv("LEDGER_URL"), Key: os.Getenv("LEDGER_KEY")}
	if c.URL != "" && c.Key != "" {
		return c, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	path := filepath.Join(home, ".config", "ledger", "credentials")
	vals, err := ReadKV(path)
	if err != nil {
		if os.IsNotExist(err) && c.URL != "" {
			return c, fmt.Errorf("no LEDGER_KEY and no %s", path)
		}
		if os.IsNotExist(err) {
			return c, fmt.Errorf("no LEDGER_URL/LEDGER_KEY and no %s", path)
		}
		return c, err
	}
	if c.URL == "" {
		c.URL = vals["LEDGER_URL"]
	}
	if c.Key == "" {
		c.Key = vals["LEDGER_KEY"]
	}
	if c.URL == "" {
		c.URL = "https://ledger.gosling-agency.ru"
	}
	if c.Key == "" {
		return c, fmt.Errorf("%s has no LEDGER_KEY", path)
	}
	return c, nil
}

// ReadKV parses KEY=value lines; refuses a file readable by group or others.
func ReadKV(path string) (map[string]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others: chmod 600 %s", path, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, sc.Err()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
