//go:build linux

package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxVerifyWait bounds how long one verify request may poll.
const maxVerifyWait = 10 * time.Minute

// SystemService is the system role's control API. It is a separate http.Handler from Service (the plugin
// role): it has its own token and is served on its own socket, and it has exactly the routes below. Nothing in
// a request selects a command, image, URL or compose argument.
type SystemService struct {
	token    []byte
	verifier *Verifier
	version  string
	operator *Operator // nil: restart is not available
	scope    *ScopeSyncer
}

// EnableScopeStatus makes `status` report the last Broker scope synchronisation.
func (s *SystemService) EnableScopeStatus(sc *ScopeSyncer) { s.scope = sc }

// EnableOperations turns on the actions that change the stack (restart).
func (s *SystemService) EnableOperations(op *Operator) { s.operator = op }

func NewSystemService(token string, verifier *Verifier, agentVersion string) (*SystemService, error) {
	if len(token) < 32 || verifier == nil || verifier.Inspector == nil || verifier.Config.Validate() != nil {
		return nil, ErrSystemConfig
	}
	return &SystemService{token: []byte(token), verifier: verifier, version: agentVersion}, nil
}

type systemRequest struct {
	WaitSeconds int64  `json:"wait_seconds"`
	Scope       string `json:"scope"`
}

type systemResponse struct {
	OK           bool             `json:"ok"`
	Error        string           `json:"error,omitempty"`
	Role         string           `json:"role,omitempty"`
	AgentVersion string           `json:"agent_version,omitempty"`
	Project      string           `json:"project,omitempty"`
	Result       *VerifyResult    `json:"result,omitempty"`
	Operation    *OperationStatus `json:"operation,omitempty"`
	ScopeSync    *ScopeResult     `json:"scope_sync,omitempty"`
}

func (s *SystemService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/system/status", s.guard(func(ctx context.Context, _ systemRequest) systemResponse {
		r := s.verifier.Once(ctx)
		resp := systemResponse{OK: true, Role: "system", AgentVersion: s.version, Project: s.verifier.Config.Project, Result: &r}
		if s.operator != nil {
			op := s.operator.Status()
			resp.Operation = &op
		}
		if s.scope != nil {
			last := s.scope.Last()
			resp.ScopeSync = &last
		}
		return resp
	}))
	mux.HandleFunc("/v1/system/restart", s.guard(func(ctx context.Context, req systemRequest) systemResponse {
		if s.operator == nil {
			return systemResponse{Error: "restart is not enabled"}
		}
		op, err := s.operator.StartRestart(req.Scope)
		if err != nil {
			return systemResponse{Error: err.Error()}
		}
		return systemResponse{OK: true, Role: "system", Operation: &op}
	}))
	mux.HandleFunc("/v1/system/verify", s.guard(func(ctx context.Context, req systemRequest) systemResponse {
		wait := time.Duration(req.WaitSeconds) * time.Second
		if wait < 0 || wait > maxVerifyWait {
			return systemResponse{Error: "wait_seconds out of range"}
		}
		r := s.verifier.Wait(ctx, wait)
		return systemResponse{OK: true, Role: "system", AgentVersion: s.version, Project: s.verifier.Config.Project, Result: &r}
	}))
	return mux
}

func (s *SystemService) guard(do func(context.Context, systemRequest) systemResponse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeSystem(w, http.StatusUnauthorized, systemResponse{Error: "unauthorized"})
			return
		}
		if r.Method != http.MethodPost {
			writeSystem(w, http.StatusMethodNotAllowed, systemResponse{Error: "POST required"})
			return
		}
		var req systemRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 4*1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			writeSystem(w, http.StatusBadRequest, systemResponse{Error: "invalid request"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), maxVerifyWait+30*time.Second)
		defer cancel()
		resp := do(ctx, req)
		status := http.StatusOK
		if !resp.OK {
			status = http.StatusBadRequest
		}
		writeSystem(w, status, resp)
	}
}

func (s *SystemService) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), s.token) == 1
}

func writeSystem(w http.ResponseWriter, status int, body systemResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
