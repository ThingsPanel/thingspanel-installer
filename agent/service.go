//go:build linux

package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Service is the Agent's control API. It is served only over a private Unix socket and every
// request needs the bearer token that the operator provisioned out of band. The caller supplies the
// instance identity and generation; the Agent never invents either, and never lets a generation that
// is not the accepted one change a container.
type Service struct {
	store     *Store
	preparer  *DockerPreparer
	activator *Activator
	token     []byte
	catalog   *CatalogSource
	builder   ProfileBuilder
}

// EnableCatalog turns on the catalog-driven install path: the Agent verifies the signed catalog itself, builds the
// container profile from the verified entry, and keeps all secrets in its own store. The caller (control plane) can
// only name a catalog id, hand over site values and a one-shot identity token.
func (s *Service) EnableCatalog(catalog *CatalogSource, builder ProfileBuilder) {
	s.catalog, s.builder = catalog, builder
	s.preparer.SetSecrets(builder.Secrets)
}

func NewService(store *Store, preparer *DockerPreparer, activator *Activator, token string) (*Service, error) {
	if store == nil || preparer == nil || activator == nil || len(token) < 32 {
		return nil, ErrPreparationPolicy
	}
	return &Service{store: store, preparer: preparer, activator: activator, token: []byte(token)}, nil
}

type apiRequest struct {
	Instance   string `json:"instance"`
	Generation uint64 `json:"generation"`
	RequestKey string `json:"request_key"`
	Profile    string `json:"profile"`
	GrantID    string `json:"grant_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Reason     string `json:"reason"`

	// Catalog install path only.
	Key               string            `json:"key"`
	Value             string            `json:"value"`
	Values            map[string]string `json:"values"`
	IdentityToken     string            `json:"identity_token"`
	ServiceIdentifier string            `json:"service_identifier"`
}

type apiResponse struct {
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Preparation *PreparationObservation `json:"preparation,omitempty"`
	Activation  *ActivationRecord `json:"activation,omitempty"`
	Stopped     []string          `json:"stopped,omitempty"`
	Catalog     []PublicEntry     `json:"catalog,omitempty"`
	Keys        []string          `json:"keys,omitempty"`
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	routes := map[string]func(context.Context, apiRequest) (apiResponse, error){
		"/v1/accept-generation": s.acceptGeneration,
		"/v1/prepare":           s.prepare,
		"/v1/grant":             s.grant,
		"/v1/activate":          s.activate,
		"/v1/renew":             s.renew,
		"/v1/revoke":            s.revoke,
		"/v1/retire":            s.retire,
		"/v1/status":            s.status,
		"/v1/sweep":             s.sweep,
		"/v1/catalog":           s.listCatalog,
		"/v1/config-put":        s.configPut,
		"/v1/config-keys":       s.configKeys,
		"/v1/config-clear":      s.configClear,
	}
	for path, handle := range routes {
		handle := handle
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !s.authorized(r) {
				write(w, http.StatusUnauthorized, apiResponse{Error: "unauthorized"})
				return
			}
			if r.Method != http.MethodPost {
				write(w, http.StatusMethodNotAllowed, apiResponse{Error: "POST required"})
				return
			}
			var request apiRequest
			decoder := json.NewDecoder(io.LimitReader(r.Body, 64*1024))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				write(w, http.StatusBadRequest, apiResponse{Error: "invalid request"})
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			response, err := handle(ctx, request)
			if err != nil {
				write(w, statusFor(err), apiResponse{Error: err.Error(), Preparation: response.Preparation, Activation: response.Activation})
				return
			}
			response.OK = true
			write(w, http.StatusOK, response)
		})
	}
	return mux
}

func (s *Service) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), s.token) == 1
}

func write(w http.ResponseWriter, status int, body apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrGenerationStale), errors.Is(err, ErrGenerationNotAccepted), errors.Is(err, ErrRevoked),
		errors.Is(err, ErrLeaseExpired), errors.Is(err, ErrNotGranted), errors.Is(err, ErrNotPrepared),
		errors.Is(err, ErrResourceConflict), errors.Is(err, ErrActivationUnresolved), errors.Is(err, ErrPreparationRecoveryRequired):
		return http.StatusConflict
	case errors.Is(err, ErrPreparationPolicy), errors.Is(err, ErrCatalog):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Service) acceptGeneration(_ context.Context, r apiRequest) (apiResponse, error) {
	return apiResponse{}, s.store.AcceptGeneration(r.Instance, r.Generation)
}

// prepare and status answer from the activation ledger first for a started generation: asking the
// create-only adapter about a container that has run would rewrite its receipt as unknown.
func (s *Service) prepare(ctx context.Context, r apiRequest) (apiResponse, error) {
	if s.activator.HasStarted(r.Instance, r.Generation) {
		record, err := s.activator.Status(r.Instance)
		return apiResponse{Activation: &record}, err
	}
	if r.IdentityToken != "" {
		if s.catalog == nil {
			return apiResponse{}, ErrPreparationPolicy
		}
		entry, err := s.catalog.Entry(r.Profile)
		if err != nil {
			return apiResponse{}, ErrPreparationPolicy
		}
		profile, err := s.builder.Build(entry, r.Instance, r.Generation, r.Values, r.IdentityToken)
		if err != nil {
			return apiResponse{}, err
		}
		if err := s.preparer.SetProfile(profile); err != nil {
			return apiResponse{}, err
		}
		r.Profile = profile.Name
	}
	observation, err := s.preparer.Prepare(ctx, PrepareContainerRequest{Instance: r.Instance, Generation: r.Generation, RequestKey: r.RequestKey, Profile: r.Profile})
	return apiResponse{Preparation: &observation}, err
}

func (s *Service) grant(ctx context.Context, r apiRequest) (apiResponse, error) {
	record, err := s.activator.Grant(ctx, r.Instance, r.Generation, r.RequestKey, r.GrantID, time.Duration(r.TTLSeconds)*time.Second)
	return apiResponse{Activation: &record}, err
}

func (s *Service) renew(_ context.Context, r apiRequest) (apiResponse, error) {
	record, err := s.activator.Renew(r.Instance, r.Generation, r.RequestKey, r.GrantID, time.Duration(r.TTLSeconds)*time.Second)
	return apiResponse{Activation: &record}, err
}

func (s *Service) activate(ctx context.Context, r apiRequest) (apiResponse, error) {
	record, err := s.activator.Activate(ctx, r.Instance, r.Generation, r.RequestKey)
	return apiResponse{Activation: &record}, err
}

func (s *Service) revoke(ctx context.Context, r apiRequest) (apiResponse, error) {
	record, err := s.activator.Revoke(ctx, r.Instance, r.Generation, r.Reason)
	return apiResponse{Activation: &record}, err
}

func (s *Service) retire(ctx context.Context, r apiRequest) (apiResponse, error) {
	err := s.activator.Retire(ctx, r.Instance, r.Generation)
	if err == nil && s.builder.Secrets != nil {
		_ = s.builder.Secrets.DeleteKey(r.Instance, fmt.Sprintf("identity/%d", r.Generation)) // the old generation's token is no longer needed
	}
	return apiResponse{}, err
}

func (s *Service) listCatalog(_ context.Context, _ apiRequest) (apiResponse, error) {
	if s.catalog == nil {
		return apiResponse{}, ErrPreparationPolicy
	}
	entries, err := s.catalog.Entries()
	if err != nil {
		return apiResponse{}, ErrCatalog
	}
	out := make([]PublicEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Public())
	}
	return apiResponse{Catalog: out}, nil
}

// declaredSecret checks that the instance is a catalog plugin and the key is one of its declared secret fields:
// nothing else can be written into the secret store through the API.
func (s *Service) declaredSecret(instance, key string) error {
	if s.catalog == nil || s.builder.Secrets == nil {
		return ErrPreparationPolicy
	}
	entry, err := s.catalog.Entry(instance)
	if err != nil {
		return ErrPreparationPolicy
	}
	if spec, ok := entry.EnvUser[key]; !ok || !spec.Secret {
		return ErrPreparationPolicy
	}
	return nil
}

func (s *Service) configPut(_ context.Context, r apiRequest) (apiResponse, error) {
	if err := s.declaredSecret(r.Instance, r.Key); err != nil {
		return apiResponse{}, err
	}
	if err := checkSecretValue(r.Value); err != nil {
		return apiResponse{}, err
	}
	return apiResponse{}, s.builder.Secrets.Put(r.Instance, "config/"+r.Key, r.Value)
}

func checkSecretValue(v string) error {
	if v == "" || len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
		return ErrPreparationPolicy
	}
	return nil
}

func (s *Service) configKeys(_ context.Context, r apiRequest) (apiResponse, error) {
	if s.catalog == nil || s.builder.Secrets == nil || !valid(r.Instance) {
		return apiResponse{}, ErrPreparationPolicy
	}
	return apiResponse{Keys: s.builder.Secrets.Keys(r.Instance, "config/")}, nil
}

// configClear removes the site secrets and every identity token of the instance (uninstall). Generated keys and the
// stack credentials stay: a reinstall must keep the same generated template secret.
func (s *Service) configClear(_ context.Context, r apiRequest) (apiResponse, error) {
	if s.catalog == nil || s.builder.Secrets == nil || !valid(r.Instance) {
		return apiResponse{}, ErrPreparationPolicy
	}
	if err := s.builder.Secrets.DeletePrefix(r.Instance, "config/"); err != nil {
		return apiResponse{}, err
	}
	return apiResponse{}, s.builder.Secrets.DeletePrefix(r.Instance, "identity/")
}

func (s *Service) status(ctx context.Context, r apiRequest) (apiResponse, error) {
	record, err := s.activator.Status(r.Instance)
	response := apiResponse{Activation: &record}
	if err == nil && record.Generation == 0 && r.Generation != 0 && valid(r.RequestKey) {
		var observation PreparationObservation
		observation, err = s.preparer.Query(ctx, r.Instance, r.Generation, r.RequestKey)
		response.Preparation = &observation
	}
	return response, err
}

func (s *Service) sweep(ctx context.Context, _ apiRequest) (apiResponse, error) {
	stopped, err := s.activator.Sweep(ctx)
	return apiResponse{Stopped: stopped}, err
}

// RunSweeper stops expired or superseded instances on a fixed cadence until ctx ends.
func (s *Service) RunSweeper(ctx context.Context, every time.Duration, report func(error)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			if _, err := s.activator.Sweep(sweepCtx); err != nil && report != nil {
				report(err)
			}
			cancel()
		}
	}
}
