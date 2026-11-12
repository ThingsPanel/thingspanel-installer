//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ScopeSync is the Go replacement for the Python Broker scope watcher (tools/scope_sync.py and
// prepare.py --sync-scope). The platform computes the device scope (GET /internal/broker-scope, with GORM);
// the Agent only fetches it, builds the Broker service identities from the stack's .env, writes services.json
// when it changed, and restarts broker-owner and then mosquitto. The Agent never touches the database.

const maxScopeDevices = 256

var (
	ErrScopeBusy       = errors.New("another installer operation holds the update lock")
	ErrScopeTooLarge   = errors.New("broker scope exceeds the reviewed limit")
	ErrScopeJournal    = errors.New("installer transaction is not finished")
	pendingFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ScopeSyncConfig is read from the trusted system config. Every path and URL is fixed there.
type ScopeSyncConfig struct {
	URL             string `json:"url"`            // http://backend:9999/internal/broker-scope
	EnvFile         string `json:"env_file"`       // the stack's .env (holds the Broker accounts and the scope token)
	TokenKey        string `json:"token_key"`      // key in EnvFile for the scope token, e.g. TP_BROKER_SCOPE_TOKEN
	Root            string `json:"root"`           // installer root; services.json, pending marker and lock live under it
	OwnerService    string `json:"owner_service"`  // compose service, restarted first
	BrokerService   string `json:"broker_service"` // compose service, restarted second
	IntervalSeconds int    `json:"interval_seconds"`
}

func (c *ScopeSyncConfig) validate(components map[string]bool) error {
	if !strings.HasPrefix(c.URL, "http://") || !filepath.IsAbs(c.EnvFile) || !filepath.IsAbs(c.Root) ||
		!components[c.OwnerService] || !components[c.BrokerService] || c.OwnerService == c.BrokerService ||
		!regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`).MatchString(c.TokenKey) || c.IntervalSeconds < 5 || c.IntervalSeconds > 3600 {
		return ErrSystemConfig
	}
	return nil
}

func (c *ScopeSyncConfig) services() string {
	return filepath.Join(c.Root, "config/native/broker/services.json")
}
func (c *ScopeSyncConfig) pending() string {
	return filepath.Join(c.Root, "config/native/broker/.scope-sync-restart.json")
}
func (c *ScopeSyncConfig) journal() string {
	return filepath.Join(c.Root, ".thingspanel-update/journal.json")
}
func (c *ScopeSyncConfig) lockPath() string { return filepath.Join(c.Root, ".thingspanel-update/lock") }

// Broker service identity, field order as the installer wrote it.
type brokerPrincipal struct {
	TenantID      string `json:"tenant_id"`
	DeviceID      string `json:"device_id"`
	PrincipalKind string `json:"principal_kind"`
}
type brokerPolicy struct {
	Publish       []string `json:"publish"`
	Subscribe     []string `json:"subscribe"`
	MaxPayload    int      `json:"max_payload"`
	MaxQoS        int      `json:"max_qos"`
	RetainProfile string   `json:"retain_profile"`
}
type brokerService struct {
	Username         string          `json:"username"`
	Password         string          `json:"password"`
	BrokerCell       string          `json:"broker_cell"`
	BrokerInstance   string          `json:"broker_instance"`
	AllowedClientIDs []string        `json:"allowed_client_ids"`
	AllowedDeviceIDs []string        `json:"allowed_device_ids"`
	AllowedTenantIDs []string        `json:"allowed_tenant_ids"`
	Principal        brokerPrincipal `json:"principal"`
	Policy           brokerPolicy    `json:"policy"`
	PolicyVersion    string          `json:"policy_version"`
	MappingVersion   string          `json:"mapping_version"`
	AllowAnyClientID bool            `json:"allow_any_client_id,omitempty"`
	PluginSocket     bool            `json:"plugin_socket,omitempty"`
}

// ScopeDevice is one device of the scope fetched from the platform.
type ScopeDevice struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
}

type scopeResponse struct {
	Targets []ScopeDevice `json:"targets"`
	Plugins []ScopeDevice `json:"plugins"`
}

func ids(list []ScopeDevice) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.ID)
	}
	return out
}

func tenants(list []ScopeDevice) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, d := range list {
		if !seen[d.TenantID] {
			seen[d.TenantID] = true
			out = append(out, d.TenantID)
		}
	}
	sort.Strings(out)
	return out
}

func serviceFor(env map[string]string, targets []ScopeDevice) brokerService {
	publish := []string{}
	if len(targets) > 0 {
		publish = []string{"devices/command/+/+", "gateway/command/+/+", "devices/telemetry/control/+", "gateway/telemetry/control/+",
			"devices/attributes/get/+", "gateway/attributes/get/+", "devices/attributes/set/+/+", "gateway/attributes/set/+/+",
			"devices/attributes/response/+/+", "gateway/attributes/response/+/+", "devices/event/response/+/+", "gateway/event/response/+/+"}
	}
	return brokerService{
		Username: env["TP_MQTT_USER"], Password: env["TP_MQTT_PASSWORD"], BrokerCell: env["TP_BROKER_CELL"], BrokerInstance: env["TP_BROKER_INSTANCE"],
		AllowedClientIDs: []string{env["TP_MQTT_CLIENT_ID"], "tp-community-readiness"},
		AllowedDeviceIDs: ids(targets), AllowedTenantIDs: tenants(targets),
		Principal:     brokerPrincipal{TenantID: "platform", DeviceID: "community-backend", PrincipalKind: "service"},
		Policy:        brokerPolicy{Publish: publish, Subscribe: []string{"tp-internal/ingress/v1/#", "tp-internal/state/v1/#"}, MaxPayload: 1048576, MaxQoS: 1, RetainProfile: "live-ingress"},
		PolicyVersion: "installer-v1", MappingVersion: "standard-v1",
	}
}

// BuildBrokerServices ports prepare.py scope_services: the platform account covers MQTT targets and plugin
// devices; the protocol account covers plugin devices only.
func BuildBrokerServices(env map[string]string, targets, plugins []ScopeDevice) []brokerService {
	platform, seen := []ScopeDevice{}, map[string]bool{}
	for _, d := range append(append([]ScopeDevice{}, targets...), plugins...) {
		if !seen[d.ID] {
			seen[d.ID] = true
			platform = append(platform, d)
		}
	}
	plugin := serviceFor(env, nil)
	plugin.Username, plugin.Password = env["TP_PROTOCOL_MQTT_USER"], env["TP_PROTOCOL_MQTT_PASSWORD"]
	plugin.AllowedClientIDs = []string{}
	plugin.AllowAnyClientID, plugin.PluginSocket = true, true
	plugin.AllowedDeviceIDs, plugin.AllowedTenantIDs = ids(plugins), tenants(plugins)
	plugin.Principal.DeviceID = "community-protocols"
	plugin.Policy.Subscribe = []string{}
	return []brokerService{serviceFor(env, platform), plugin}
}

// ScopeResult is what one pass did. Codes only, no values.
type ScopeResult struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"` // unchanged | applied | retried_pending | busy | error
	Code    string    `json:"code,omitempty"`
}

// ScopeSyncer runs the scope synchronisation.
type ScopeSyncer struct {
	Config  ScopeSyncConfig
	Project string
	Manager StackManager
	Op      *Operator // shared with restart: the two never overlap; nil in tests that do not need it
	HTTP    *http.Client
	Now     func() time.Time

	mu   sync.Mutex
	last ScopeResult
}

func (s *ScopeSyncer) Last() ScopeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *ScopeSyncer) record(outcome, code string) ScopeResult {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	r := ScopeResult{At: now().UTC(), Outcome: outcome, Code: code}
	s.mu.Lock()
	s.last = r
	s.mu.Unlock()
	return r
}

func (s *ScopeSyncer) fetch(ctx context.Context, token string) (scopeResponse, error) {
	var out scopeResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Config.URL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("scope endpoint answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return out, err
	}
	if len(out.Targets) > maxScopeDevices || len(out.Plugins) > maxScopeDevices {
		return out, ErrScopeTooLarge
	}
	return out, nil
}

func canonical(raw []byte) (any, []byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, err
	}
	b, err := json.Marshal(v) // map keys sorted, compact
	return v, b, err
}

// Once runs one pass under the installer's update lock.
func (s *ScopeSyncer) Once(ctx context.Context) ScopeResult {
	cfg := &s.Config
	if err := os.MkdirAll(filepath.Dir(cfg.lockPath()), 0o700); err != nil {
		return s.record("error", "lock_unavailable")
	}
	lock, err := os.OpenFile(cfg.lockPath(), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return s.record("error", "lock_unavailable")
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return s.record("busy", "update_lock_held")
	}
	// An unfinished installer transaction owns the configuration: leave it alone.
	if raw, err := os.ReadFile(cfg.journal()); err == nil {
		var j struct {
			Phase string `json:"phase"`
		}
		if json.Unmarshal(raw, &j) != nil || (j.Phase != "complete" && j.Phase != "recovered") {
			return s.record("busy", "installer_transaction_open")
		}
	}
	env, err := ReadEnvFile(cfg.EnvFile)
	if err != nil || env[cfg.TokenKey] == "" {
		return s.record("error", "env_unreadable")
	}
	scope, err := s.fetch(ctx, env[cfg.TokenKey])
	if err != nil {
		if errors.Is(err, ErrScopeTooLarge) {
			return s.record("error", "scope_too_large")
		}
		return s.record("error", "scope_fetch_failed")
	}
	wanted, err := json.MarshalIndent(BuildBrokerServices(env, scope.Targets, scope.Plugins), "", "  ")
	if err != nil {
		return s.record("error", "encode_failed")
	}
	wantedV, wantedC, _ := canonical(wanted)
	changed := true
	if raw, err := os.ReadFile(cfg.services()); err == nil {
		if curV, _, err := canonical(raw); err == nil {
			changed = !reflect.DeepEqual(curV, wantedV)
		}
	}
	sum := sha256.Sum256(wantedC)
	fingerprint := hex.EncodeToString(sum[:])
	pending := ""
	if raw, err := os.ReadFile(cfg.pending()); err == nil {
		var p struct {
			SHA256 string `json:"sha256"`
		}
		if json.Unmarshal(raw, &p) != nil || !pendingFingerprint.MatchString(p.SHA256) {
			return s.record("error", "pending_record_invalid")
		}
		pending = p.SHA256
	}
	if !changed && pending == "" {
		return s.record("unchanged", "")
	}
	if changed || pending != fingerprint {
		if err := atomicWrite(cfg.pending(), []byte(fmt.Sprintf("{\"sha256\": %q}\n", fingerprint)), 0o600, false); err != nil {
			return s.record("error", "pending_write_failed")
		}
	}
	if changed {
		if err := atomicWrite(cfg.services(), append(wanted, '\n'), 0o440, true); err != nil {
			return s.record("error", "services_write_failed")
		}
	}
	restart := func() error {
		for _, name := range []string{cfg.OwnerService, cfg.BrokerService} {
			if err := s.Manager.Stop(ctx, s.Project, name, 30*time.Second); err != nil {
				return err
			}
			if err := s.Manager.Start(ctx, s.Project, name); err != nil {
				return err
			}
		}
		return nil
	}
	var rerr error
	if s.Op != nil {
		if err := s.Op.RunExclusive(func() { rerr = restart() }); err != nil {
			return s.record("busy", "system_operation_running") // pending marker stays; the next pass retries
		}
	} else {
		rerr = restart()
	}
	if rerr != nil {
		return s.record("error", "broker_restart_failed") // pending marker stays
	}
	_ = os.Remove(cfg.pending())
	if changed {
		return s.record("applied", "")
	}
	return s.record("retried_pending", "")
}

// Run repeats Once until ctx ends. Errors are reported through report (codes only).
func (s *ScopeSyncer) Run(ctx context.Context, report func(ScopeResult)) {
	t := time.NewTicker(time.Duration(s.Config.IntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		r := s.Once(ctx)
		if report != nil && (r.Outcome == "applied" || r.Outcome == "retried_pending" || r.Outcome == "error") {
			report(r)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// atomicWrite replaces path through a temp file in the same directory. With chownBroker and root it hands the
// file to the Broker's uid, as the installer does.
func atomicWrite(path string, content []byte, mode os.FileMode, chownBroker bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.Copy(tmp, bytes.NewReader(content)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if chownBroker && os.Geteuid() == 0 {
		if err := tmp.Chown(1883, 1883); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
