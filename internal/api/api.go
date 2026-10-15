// Package api exposes the task engine over HTTP/JSON.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/iamanishx/tether/internal/task"
)

const maxBody = 1 << 20

type Server struct {
	eng   *task.Engine
	token string // empty disables auth
}

func New(eng *task.Engine, token string) *Server { return &Server{eng: eng, token: token} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })

	mux.HandleFunc("POST /v1/tasks", s.auth(s.createTask))
	mux.HandleFunc("GET /v1/tasks", s.auth(s.listTasks))
	mux.HandleFunc("GET /v1/tasks/{id}", s.auth(s.getTask))
	mux.HandleFunc("POST /v1/tasks/{id}/actions", s.auth(s.submit))
	mux.HandleFunc("POST /v1/tasks/{id}/actions/{aid}/approve", s.auth(s.approve(true)))
	mux.HandleFunc("POST /v1/tasks/{id}/actions/{aid}/reject", s.auth(s.approve(false)))
	mux.HandleFunc("POST /v1/tasks/{id}/actions/{aid}/resolve", s.auth(s.resolve))
	mux.HandleFunc("POST /v1/tasks/{id}/complete", s.auth(s.complete))
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.auth(s.cancel))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string            `json:"id"`
		Title    string            `json:"title"`
		Metadata map[string]string `json:"metadata"`
	}
	if !decode(w, r, &in) {
		return
	}
	t, err := s.eng.CreateTask(in.ID, in.Title, in.Metadata)
	respond(w, http.StatusCreated, t, err)
}

func (s *Server) listTasks(w http.ResponseWriter, _ *http.Request) {
	ts, err := s.eng.List()
	respond(w, http.StatusOK, ts, err)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.eng.Get(r.PathValue("id"))
	respond(w, http.StatusOK, t, err)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ActionID         string `json:"action_id"`
		Language         string `json:"language"`
		Code             string `json:"code"`
		Idempotent       bool   `json:"idempotent"`
		RequiresApproval bool   `json:"requires_approval"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, replayed, err := s.eng.Submit(r.Context(), r.PathValue("id"), task.SubmitReq{
		ActionID: in.ActionID, Language: in.Language, Code: in.Code,
		Idempotent: in.Idempotent, RequiresApproval: in.RequiresApproval,
	})
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	code := http.StatusOK
	if err == nil && a.Status == task.StatusPendingApproval {
		code = http.StatusAccepted
	}
	respond(w, code, a, err)
}

func (s *Server) approve(yes bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Note string `json:"note"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, err := s.eng.Approve(r.Context(), r.PathValue("id"), r.PathValue("aid"), yes, in.Note)
		respond(w, http.StatusOK, a, err)
	}
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, err := s.eng.Resolve(r.Context(), r.PathValue("id"), r.PathValue("aid"), in.Outcome, in.Note)
	respond(w, http.StatusOK, a, err)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		State    task.State `json:"state"`
		Evidence string     `json:"evidence"`
	}
	if !decode(w, r, &in) {
		return
	}
	t, err := s.eng.Complete(r.Context(), r.PathValue("id"), in.State, in.Evidence)
	respond(w, http.StatusOK, t, err)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	t, err := s.eng.Cancel(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, t, err)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func respond(w http.ResponseWriter, okCode int, v any, err error) {
	switch {
	case err == nil:
		writeJSON(w, okCode, v)
	case errors.Is(err, task.ErrBadRequest):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, task.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, task.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		log.Printf("internal error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
