// Package client is the CLI's client of the ledger API: the agent's bearer key, JSON both
// ways, and errors that keep the HTTP status and the service's message.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/config"
	"github.com/goslingmanagment/ledger/internal/version"
)

// maxAnswer bounds what one call reads: a full /v1/sql answer is well below it.
const maxAnswer = 256 << 20

// Client calls the API at URL with an agent's key.
type Client struct {
	URL  string
	key  string
	HTTP *http.Client
}

// New returns a client for cfg (see config.LoadClient).
func New(cfg config.Client) *Client {
	return &Client{URL: strings.TrimRight(cfg.URL, "/"), key: cfg.Key, HTTP: &http.Client{Timeout: time.Minute}}
}

// Error is an answer outside 2xx.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// Do sends in as JSON (nil: no body) and decodes a 2xx answer into out (nil: drop it). Numbers
// decode as json.Number, so integers of any size stay exact. An answer outside 2xx is *Error.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ledger-cli/"+version.Get())
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorOf(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s %s: ответ не JSON: %w", method, path, err)
	}
	return nil
}

// errorOf reads {"error": "..."} of the service, or the start of whatever a proxy answered.
func errorOf(status int, raw []byte) *Error {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return &Error{Status: status, Message: body.Error}
	}
	msg := strings.Join(strings.Fields(string(raw)), " ")
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &Error{Status: status, Message: msg}
}
