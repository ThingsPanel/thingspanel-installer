//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The system role is the host-side manager's second, isolated role. It serves a fixed set of actions on its
// own socket with its own token. It never reads an action, command, image, URL or compose argument from a
// request: everything it may touch comes from SystemConfig, a root-owned file written by the installer.

var (
	ErrSystemConfig  = errors.New("system configuration is invalid")
	ErrSystemTokens  = errors.New("the system and plugin tokens must differ")
	componentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	projectPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

const (
	ProbeDocker = "docker" // container is running and Docker's own healthcheck (if any) says healthy
	ProbeHTTP   = "http"   // container is running and GET URL answers 2xx
	ProbeTCP    = "tcp"    // container is running and Addr accepts a connection
)

// Component is one service of the platform stack, named as in docker-compose.yml.
type Component struct {
	Name  string `json:"name"`           // compose service name
	Group string `json:"group"`          // "control" (restarted by default), "broker" or "data"
	Probe string `json:"probe"`          // ProbeDocker (default), ProbeHTTP or ProbeTCP
	URL   string `json:"url,omitempty"`  // ProbeHTTP only
	Addr  string `json:"addr,omitempty"` // ProbeTCP only: host:port
}

// SystemConfig is the fixed description of the stack the system role manages.
type SystemConfig struct {
	Project    string      `json:"project"`    // compose project name (label com.docker.compose.project)
	Components []Component `json:"components"` // in START order; stop order is the reverse
	// CheckConfig is run inside a running container before a restart. Optional; fixed argv from this file only.
	CheckConfig *CheckConfig `json:"check_config,omitempty"`
	// ScopeSync keeps the Broker's device scope in step with the platform (the former P1 watcher). Optional.
	ScopeSync *ScopeSyncConfig `json:"scope_sync,omitempty"`
	// StatusDir receives status.json and the maintenance flag `active` (world-readable, no secrets). Optional.
	StatusDir string `json:"status_dir,omitempty"`
}

// CheckConfig names the component that validates configuration and the exact command to run in it.
type CheckConfig struct {
	Service string   `json:"service"`
	Argv    []string `json:"argv"`
}

// LoadSystemConfig reads and validates the config file. The file must be a private regular file.
func LoadSystemConfig(path string) (SystemConfig, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return SystemConfig{}, fmt.Errorf("%w: file must be a regular file not writable by group or others", ErrSystemConfig)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return SystemConfig{}, ErrSystemConfig
	}
	var c SystemConfig
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return SystemConfig{}, fmt.Errorf("%w: %v", ErrSystemConfig, err)
	}
	return c, c.Validate()
}

func (c SystemConfig) Validate() error {
	if !projectPattern.MatchString(c.Project) || len(c.Components) == 0 || len(c.Components) > 64 {
		return ErrSystemConfig
	}
	seen := map[string]bool{}
	for _, comp := range c.Components {
		if !componentPattern.MatchString(comp.Name) || seen[comp.Name] {
			return ErrSystemConfig
		}
		seen[comp.Name] = true
		switch comp.Group {
		case "control", "broker", "data":
		default:
			return ErrSystemConfig
		}
		switch comp.Probe {
		case "", ProbeDocker:
			if comp.URL != "" || comp.Addr != "" {
				return ErrSystemConfig
			}
		case ProbeHTTP:
			if !strings.HasPrefix(comp.URL, "http://") || comp.Addr != "" {
				return ErrSystemConfig
			}
		case ProbeTCP:
			if _, _, err := net.SplitHostPort(comp.Addr); err != nil || comp.URL != "" {
				return ErrSystemConfig
			}
		default:
			return ErrSystemConfig
		}
	}
	if cc := c.CheckConfig; cc != nil {
		if !seen[cc.Service] || len(cc.Argv) == 0 || len(cc.Argv) > 8 || !strings.HasPrefix(cc.Argv[0], "/") {
			return ErrSystemConfig
		}
	}
	if c.StatusDir != "" && !strings.HasPrefix(c.StatusDir, "/") {
		return ErrSystemConfig
	}
	if c.ScopeSync != nil {
		if err := c.ScopeSync.validate(seen); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDistinctTokens refuses to run the two roles on the same secret: a holder of the plugin token must not
// be able to use the system socket.
func EnsureDistinctTokens(pluginToken, systemToken string) error {
	if pluginToken == systemToken {
		return ErrSystemTokens
	}
	return nil
}

// ContainerState is what the system role needs to know about one container, read-only.
type ContainerState struct {
	Found      bool
	Running    bool
	Restarting bool
	Health     string // "" (no healthcheck), "starting", "healthy" or "unhealthy"
}

// StackInspector reads the containers of one compose project, keyed by compose service name.
type StackInspector interface {
	Containers(ctx context.Context, project string) (map[string]ContainerState, error)
}

// Prober checks one component beyond Docker state. Replaceable in tests.
type Prober interface {
	HTTP(ctx context.Context, url string) error
	TCP(ctx context.Context, addr string) error
}

// Component states.
const (
	StateOK       = "ok"
	StateFailed   = "failed"
	StateStarting = "starting"
)

// Fixed reason codes. They carry no error text, URL or address.
const (
	ReasonMissing          = "container_missing"
	ReasonNotRunning       = "not_running"
	ReasonRestarting       = "restarting"
	ReasonUnhealthy        = "unhealthy"
	ReasonHealthStarting   = "health_starting"
	ReasonProbeFailed      = "probe_failed"
	ReasonDockerUnreadable = "docker_unreadable"
)

type ComponentStatus struct {
	Name   string `json:"name"`
	Group  string `json:"group"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type VerifyResult struct {
	OK         bool              `json:"ok"`
	Components []ComponentStatus `json:"components"`
	CheckedAt  time.Time         `json:"checked_at"`
	WaitedMS   int64             `json:"waited_ms,omitempty"`
}

// Verifier is the whole-stack health gate.
type Verifier struct {
	Config    SystemConfig
	Inspector StackInspector
	Prober    Prober
	Now       func() time.Time
	Sleep     func(time.Duration)
}

// Once checks every component exactly once. It never stops at the first failure.
func (v *Verifier) Once(ctx context.Context) VerifyResult {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	result := VerifyResult{OK: true, CheckedAt: now().UTC(), Components: make([]ComponentStatus, 0, len(v.Config.Components))}
	states, err := v.Inspector.Containers(ctx, v.Config.Project)
	for _, comp := range v.Config.Components {
		st := ComponentStatus{Name: comp.Name, Group: comp.Group}
		switch {
		case err != nil:
			st.State, st.Reason = StateFailed, ReasonDockerUnreadable
		default:
			st.State, st.Reason = v.judge(ctx, comp, states[comp.Name])
		}
		if st.State != StateOK {
			result.OK = false
		}
		result.Components = append(result.Components, st)
	}
	sort.SliceStable(result.Components, func(i, j int) bool { return result.Components[i].Name < result.Components[j].Name })
	return result
}

func (v *Verifier) judge(ctx context.Context, comp Component, c ContainerState) (string, string) {
	switch {
	case !c.Found:
		return StateFailed, ReasonMissing
	case c.Restarting:
		return StateFailed, ReasonRestarting
	case !c.Running:
		return StateFailed, ReasonNotRunning
	}
	switch comp.Probe {
	case ProbeHTTP:
		if v.Prober == nil || v.Prober.HTTP(ctx, comp.URL) != nil {
			return StateFailed, ReasonProbeFailed
		}
		return StateOK, ""
	case ProbeTCP:
		if v.Prober == nil || v.Prober.TCP(ctx, comp.Addr) != nil {
			return StateFailed, ReasonProbeFailed
		}
		return StateOK, ""
	}
	switch c.Health {
	case "", "healthy":
		return StateOK, ""
	case "starting":
		return StateStarting, ReasonHealthStarting
	}
	return StateFailed, ReasonUnhealthy
}

// Wait repeats Once until the stack is ok or the budget is spent. A zero budget is a single check.
func (v *Verifier) Wait(ctx context.Context, budget time.Duration) VerifyResult {
	sleep := time.Sleep
	if v.Sleep != nil {
		sleep = v.Sleep
	}
	start := time.Now()
	for {
		r := v.Once(ctx)
		if r.OK || budget <= 0 || time.Since(start) >= budget || ctx.Err() != nil {
			r.WaitedMS = time.Since(start).Milliseconds()
			return r
		}
		sleep(2 * time.Second)
	}
}

// NetProber is the production Prober.
type NetProber struct{}

func (NetProber) HTTP(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (NetProber) TCP(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}
