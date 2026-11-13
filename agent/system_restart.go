//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Restart scopes.
const (
	ScopeControl = "control" // the control plane only; broker and devices are not touched
	ScopeFull    = "full"    // control plane plus the broker (devices reconnect)
)

var (
	ErrBusy       = errors.New("another system operation is running")
	ErrBadScope   = errors.New("scope must be control or full")
	ErrNoCheckRun = errors.New("check command could not run")
)

// Steps and result codes written to status.json. No error text, only fixed codes.
const (
	StepPreflight   = "preflight"
	StepMaintenance = "maintenance_on"
	StepStopping    = "stopping"
	StepStarting    = "starting"
	StepVerifying   = "verifying"
	StepDone        = "done"

	CodeConfigInvalid = "config_invalid"
	CodeStopFailed    = "stop_failed"
	CodeStartFailed   = "start_failed"
	CodeVerifyFailed  = "verify_failed"
	CodeCheckSkipped  = "check_skipped_backend_down"
)

// OperationStatus is what status.json holds. It is read by the maintenance page and by `system status`.
type OperationStatus struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	State     string    `json:"state"` // idle | running | done | failed | aborted
	Step      string    `json:"step"`
	Percent   int       `json:"percent"`
	Component string    `json:"component,omitempty"`
	Code      string    `json:"code,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Operator runs system operations one at a time.
type Operator struct {
	Config       SystemConfig
	Manager      StackManager
	Verifier     *Verifier
	Grace        time.Duration
	VerifyBudget time.Duration
	Now          func() time.Time

	mu      sync.Mutex
	running bool
	current OperationStatus
	done    chan struct{}
}

func (o *Operator) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// Status returns the current or last operation.
func (o *Operator) Status() OperationStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current.ID == "" {
		return OperationStatus{State: "idle"}
	}
	return o.current
}

// Wait blocks until the running operation (if any) finishes. For tests and the CLI.
func (o *Operator) Wait() {
	o.mu.Lock()
	ch := o.done
	o.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// RunExclusive runs fn only if no system operation is running, and blocks new ones meanwhile.
func (o *Operator) RunExclusive(fn func()) error {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return ErrBusy
	}
	o.running = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
	}()
	fn()
	return nil
}

// CodedError is a failure whose reason is a fixed code that may be shown to the user.
type CodedError struct{ Code string }

func (e CodedError) Error() string { return e.Code }

// StartTask runs a long operation (backup, ...) in the background under the same one-at-a-time rule and the
// same status.json as restart. fn reports progress; a CodedError becomes the status code.
func (o *Operator) StartTask(kind string, fn func(ctx context.Context, report func(step string, percent int)) error) (OperationStatus, error) {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return OperationStatus{}, ErrBusy
	}
	o.running = true
	o.done = make(chan struct{})
	st := OperationStatus{ID: o.now().Format("20060102T150405.000Z"), Kind: kind, State: "running", Step: StepPreflight, StartedAt: o.now(), UpdatedAt: o.now()}
	o.current = st
	done := o.done
	o.mu.Unlock()
	o.publish(st)
	go func() {
		defer func() {
			o.mu.Lock()
			o.running = false
			o.mu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		err := fn(ctx, func(step string, percent int) {
			o.update(func(s *OperationStatus) { s.Step, s.Percent = step, percent })
		})
		if err != nil {
			code := "failed"
			var ce CodedError
			if errors.As(err, &ce) {
				code = ce.Code
			}
			o.update(func(s *OperationStatus) { s.State, s.Code = "failed", code })
			return
		}
		o.update(func(s *OperationStatus) { s.State, s.Step, s.Percent, s.Code = "done", StepDone, 100, "" })
	}()
	return st, nil
}

// StartRestart begins a restart in the background and returns at once. Only one operation runs at a time.
func (o *Operator) StartRestart(scope string) (OperationStatus, error) {
	if scope != ScopeControl && scope != ScopeFull {
		return OperationStatus{}, ErrBadScope
	}
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return OperationStatus{}, ErrBusy
	}
	o.running = true
	o.done = make(chan struct{})
	st := OperationStatus{ID: o.now().Format("20060102T150405.000Z"), Kind: "restart", Scope: scope, State: "running", Step: StepPreflight, StartedAt: o.now(), UpdatedAt: o.now()}
	o.current = st
	done := o.done
	o.mu.Unlock()
	o.publish(st)
	go func() {
		defer func() {
			o.mu.Lock()
			o.running = false
			o.mu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		o.runRestart(ctx, scope)
	}()
	return st, nil
}

func (o *Operator) update(f func(*OperationStatus)) {
	o.mu.Lock()
	f(&o.current)
	o.current.UpdatedAt = o.now()
	st := o.current
	o.mu.Unlock()
	o.publish(st)
}

func (o *Operator) publish(st OperationStatus) {
	dir := o.Config.StatusDir
	if dir == "" {
		return
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, ".status.json.tmp")
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "status.json"))
	}
}

func (o *Operator) maintenance(on bool) {
	dir := o.Config.StatusDir
	if dir == "" {
		return
	}
	flag := filepath.Join(dir, "active")
	if on {
		_ = os.WriteFile(flag, []byte("1"), 0o644)
	} else {
		_ = os.Remove(flag)
	}
}

func (o *Operator) fail(state, step, component, code string) {
	o.update(func(s *OperationStatus) { s.State, s.Step, s.Component, s.Code = state, step, component, code })
}

// selected returns the components a scope covers, in start order.
func (o *Operator) selected(scope string) (broker, control []Component) {
	for _, c := range o.Config.Components {
		switch {
		case c.Group == "control":
			control = append(control, c)
		case c.Group == "broker" && scope == ScopeFull:
			broker = append(broker, c)
		}
	}
	return
}

// CheckResult is the answer to "is the configuration valid": OK, or the service whose check failed, or Skipped
// when the backend is not running so the check cannot be run (a restart then proceeds, it is the recovery).
type CheckResult struct {
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Service string `json:"service,omitempty"`
	Code    string `json:"code,omitempty"`
}

// CheckConfigOnce runs the configured check command in the running container. It never changes anything.
func (o *Operator) CheckConfigOnce(ctx context.Context) CheckResult {
	cc := o.Config.CheckConfig
	if cc == nil {
		return CheckResult{OK: true, Skipped: true}
	}
	states, err := o.Manager.Containers(ctx, o.Config.Project)
	if err != nil || !states[cc.Service].Running {
		return CheckResult{OK: true, Skipped: true, Service: cc.Service, Code: CodeCheckSkipped}
	}
	code, err := o.Manager.Exec(ctx, o.Config.Project, cc.Service, cc.Argv)
	if err != nil || code != 0 {
		return CheckResult{OK: false, Service: cc.Service, Code: CodeConfigInvalid}
	}
	return CheckResult{OK: true, Service: cc.Service}
}

func (o *Operator) runRestart(ctx context.Context, scope string) {
	project := o.Config.Project
	grace := o.Grace
	if grace <= 0 {
		grace = 30 * time.Second
	}
	// 1. preflight: validate configuration before touching anything.
	if cc := o.Config.CheckConfig; cc != nil {
		states, err := o.Manager.Containers(ctx, project)
		if err == nil && states[cc.Service].Running {
			code, err := o.Manager.Exec(ctx, project, cc.Service, cc.Argv)
			if err != nil || code != 0 {
				o.fail("aborted", StepPreflight, cc.Service, CodeConfigInvalid)
				return
			}
		} else {
			o.update(func(s *OperationStatus) { s.Notes = append(s.Notes, CodeCheckSkipped) })
		}
	}
	broker, control := o.selected(scope)
	total := len(broker)*2 + len(control)*2
	steps := 0
	progress := func() int { steps++; return 10 + 70*steps/total }
	// 2. maintenance page on.
	o.update(func(s *OperationStatus) { s.Step, s.Percent = StepMaintenance, 5 })
	o.maintenance(true)
	// 3. stop: control plane in reverse start order, then broker in reverse start order.
	stopList := append(reverse(control), reverse(broker)...)
	for _, c := range stopList {
		name := c.Name
		o.update(func(s *OperationStatus) { s.Step, s.Component, s.Percent = StepStopping, name, progress() })
		if err := o.Manager.Stop(ctx, project, name, grace); err != nil && !errors.Is(err, ErrNoContainer) {
			o.fail("failed", StepStopping, name, CodeStopFailed)
			return
		}
	}
	// 4. start: broker first (it must be up for the backend), then the control plane in start order.
	for _, c := range append(append([]Component{}, broker...), control...) {
		name := c.Name
		o.update(func(s *OperationStatus) { s.Step, s.Component, s.Percent = StepStarting, name, progress() })
		if err := o.Manager.Start(ctx, project, name); err != nil {
			o.fail("failed", StepStarting, name, CodeStartFailed)
			return
		}
	}
	// 5. verify the whole stack.
	o.update(func(s *OperationStatus) { s.Step, s.Component, s.Percent = StepVerifying, "", 85 })
	budget := o.VerifyBudget
	if budget <= 0 {
		budget = 5 * time.Minute
	}
	result := o.Verifier.Wait(ctx, budget)
	if !result.OK {
		bad := ""
		for _, c := range result.Components {
			if c.State != StateOK {
				bad = c.Name
				break
			}
		}
		o.fail("failed", StepVerifying, bad, CodeVerifyFailed)
		return
	}
	o.maintenance(false)
	o.update(func(s *OperationStatus) {
		s.State, s.Step, s.Percent, s.Component, s.Code = "done", StepDone, 100, "", ""
	})
}

func reverse(in []Component) []Component {
	out := make([]Component, len(in))
	for i, c := range in {
		out[len(in)-1-i] = c
	}
	return out
}
