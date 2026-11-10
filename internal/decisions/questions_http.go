package decisions

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/goslingmanagment/ledger/internal/auth"
	"github.com/goslingmanagment/ledger/internal/server"
	"github.com/goslingmanagment/ledger/internal/ts"
)

// The questions are written and read by the same keys as the decisions.
func init() {
	server.Register(func(s *server.Server) {
		s.Mux.Handle("POST /v1/questions", auth.RequireAny(s, Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in QuestionInput
			if !read(w, r, &in) {
				return
			}
			q, err := AddQuestion(r.Context(), s.Store, in, auth.KeyName(r.Context()), ts.Now())
			answerQuestion(s, w, q, err)
		})))
		s.Mux.Handle("POST /v1/questions/{id}/answer", auth.RequireAny(s, Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := questionID(w, r)
			if !ok {
				return
			}
			var in AnswerInput
			if !read(w, r, &in) {
				return
			}
			q, err := AnswerQuestion(r.Context(), s.Store, id, in, ts.Now())
			answerQuestion(s, w, q, err)
		})))
		s.Mux.Handle("POST /v1/questions/{id}/drop", auth.RequireAny(s, Writers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := questionID(w, r)
			if !ok {
				return
			}
			var in DropInput
			if !read(w, r, &in) {
				return
			}
			q, err := DropQuestion(r.Context(), s.Store, id, in, ts.Now())
			answerQuestion(s, w, q, err)
		})))
		s.Mux.Handle("GET /v1/questions", auth.RequireAny(s, Readers, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			f := QuestionFilter{Open: q.Get("open") == "1", AskedOf: strings.TrimSpace(q.Get("asked_of")), Fan: strings.TrimSpace(q.Get("fan"))}
			if f.AskedOf != "" && f.AskedOf != AskedOwner && f.AskedOf != AskedChatters {
				server.WriteError(w, http.StatusBadRequest, "asked_of бывает owner или chatters, а не "+strconv.Quote(f.AskedOf))
				return
			}
			for name, dst := range map[string]*string{"from": &f.From, "to": &f.To} {
				if v := strings.TrimSpace(q.Get(name)); v != "" {
					d, err := NormDate(name, v)
					if err != nil {
						server.WriteError(w, http.StatusBadRequest, err.Error())
						return
					}
					*dst = d
				}
			}
			list, err := ListQuestions(r.Context(), s.Store.RO, f)
			if err != nil {
				s.Log.Error("questions list", "key", auth.KeyName(r.Context()), "err", err)
				server.WriteError(w, http.StatusInternalServerError, "вопросы не прочитаны: сбой сервиса, подробности в журнале")
				return
			}
			server.WriteJSON(w, http.StatusOK, QuestionsAnswer{Questions: list})
		})))
	})
}

// QuestionsAnswer is the reply of GET /v1/questions.
type QuestionsAnswer struct {
	Questions []Question `json:"questions"`
}

func questionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		server.WriteError(w, http.StatusBadRequest, "нужен номер вопроса: /v1/questions/<id>/…")
		return 0, false
	}
	return id, true
}

func answerQuestion(s *server.Server, w http.ResponseWriter, q Question, err error) {
	var ref *Refusal
	switch {
	case errors.As(err, &ref):
		server.WriteError(w, http.StatusUnprocessableEntity, ref.Error())
	case errors.Is(err, ErrNoQuestion):
		server.WriteError(w, http.StatusNotFound, "нет такого вопроса")
	case err != nil:
		s.Log.Error("questions", "err", err)
		server.WriteError(w, http.StatusInternalServerError, "вопрос не записан: сбой сервиса, подробности в журнале")
	default:
		server.WriteJSON(w, http.StatusOK, q)
	}
}
