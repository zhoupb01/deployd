package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/zhoupb01/deployd/internal/auth"
	"github.com/zhoupb01/deployd/internal/config"
	"github.com/zhoupb01/deployd/internal/deploy"
)

const maxBodyBytes = 64 << 10

var tagRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

type Server struct {
	cfg      *config.Config
	deployer *deploy.Manager
	verifier *auth.Verifier
}

func New(cfg *config.Config, deployer *deploy.Manager, verifier *auth.Verifier) *Server {
	return &Server{cfg: cfg, deployer: deployer, verifier: verifier}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /deploy/{service}", s.handleDeploy)
	mux.HandleFunc("GET /deploy/{service}/status", s.handleStatus)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return logMiddleware(mux)
}

type deployRequest struct {
	Tag string `json:"tag,omitempty"`
}

type deployResponse struct {
	DeployID string `json:"deploy_id"`
	State    string `json:"state"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("service")
	svc, ok := s.cfg.Services[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown service"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "body too large or unreadable"})
		return
	}

	if err := s.verifier.Verify(svc.Secret,
		body,
		r.Header.Get(auth.HeaderTimestamp),
		r.Header.Get(auth.HeaderSignature),
	); err != nil {
		log.Printf("deployd: auth fail service=%s reason=%s", name, auth.Describe(err))
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	var req deployRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json body"})
			return
		}
	}
	if req.Tag != "" && !tagRe.MatchString(req.Tag) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid tag format"})
		return
	}

	deployID, err := s.deployer.Trigger(name, req.Tag)
	if err != nil {
		switch {
		case errors.Is(err, deploy.ErrBusy):
			writeJSON(w, http.StatusConflict, errorResponse{Error: "service is currently being deployed"})
		case errors.Is(err, deploy.ErrUnknownService):
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown service"})
		default:
			log.Printf("deployd: trigger failed service=%s err=%v", name, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		}
		return
	}

	log.Printf("deployd: trigger service=%s deploy_id=%s tag=%q", name, deployID, req.Tag)
	writeJSON(w, http.StatusAccepted, deployResponse{DeployID: deployID, State: deploy.StateRunning})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("service")
	svc, ok := s.cfg.Services[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown service"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "body too large"})
		return
	}
	if err := s.verifier.Verify(svc.Secret,
		body,
		r.Header.Get(auth.HeaderTimestamp),
		r.Header.Get(auth.HeaderSignature),
	); err != nil {
		log.Printf("deployd: auth fail service=%s reason=%s", name, auth.Describe(err))
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	rec, err := s.deployer.Status(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown service"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("deployd: %s %s -> %d (%dms)",
			r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
	})
}
