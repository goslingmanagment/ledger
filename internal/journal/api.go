package journal

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/server"
)

// maxBundle limits one import: the whole history of the journal is well below it.
const maxBundle = 64 << 20

func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("GET /v1/export/ledger-md", auth.Require(s, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			text, err := Export(r.Context(), s.Store.RO)
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, text)
		})))
		s.Mux.Handle("POST /v1/import/ledger-md", auth.Require(s, "import", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			var mode string
			_ = s.Store.RO.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'mode'`).Scan(&mode)
			if mode == "live" {
				server.WriteError(w, http.StatusConflict, "ledger — канон журнала: импорт из файла закрыт")
				return
			}
			var body struct {
				Bundle Bundle `json:"bundle"`
				Align  []struct {
					Account string `json:"account"`
					Date    string `json:"date"`
					UpTo    int    `json:"up_to"`
					Amount  string `json:"amount"`
				} `json:"align"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBundle)).Decode(&body); err != nil {
				server.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			tx, err := s.Store.RW.BeginTx(ctx, nil)
			if err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			defer tx.Rollback()
			opts, err := OptionsFromSettings(ctx, tx)
			if err != nil {
				server.WriteError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			for _, a := range body.Align {
				al, err := ParseAlign(a.Account, a.Date, a.UpTo, a.Amount)
				if err != nil {
					server.WriteError(w, http.StatusBadRequest, err.Error())
					return
				}
				opts.Align = append(opts.Align, al)
			}
			rep, err := Import(ctx, tx, body.Bundle, opts)
			if err != nil {
				var pe ProblemsError
				if errors.As(err, &pe) {
					server.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "импорт остановлен", "problems": pe.Problems})
					return
				}
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			var unbalanced int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM v_unbalanced`).Scan(&unbalanced); err != nil || unbalanced != 0 {
				server.WriteError(w, http.StatusInternalServerError, "несходящиеся проводки после импорта: "+strings.TrimSpace(errText(err)))
				return
			}
			if err := tx.Commit(); err != nil {
				server.WriteError(w, http.StatusInternalServerError, err.Error())
				return
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
		})))
	})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
