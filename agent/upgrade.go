//go:build linux

package agent

// Platform upgrade with a mandatory snapshot, a health gate and automatic rollback (R5b, plan §3.4 and §4.3).
//
//	preflight ─▶ staging ─▶ snapshot_done ─▶ stopping ─▶ applying ─▶ applied ─▶ starting ─▶ verifying ─▶ committed
//	   │ fails      │ fails       │                                    any failure from snapshot_done on
//	   ▼            ▼             ▼                                                  ▼
//	aborted (nothing was changed)                    rolling_back ─▶ rolled_back   or   needs_human (site kept as is)
//
// Every phase is written to the state file BEFORE its work starts, so after a crash (kill -9, power loss) the
// state file says how far the operation may have got. Resume() then either cleans up (nothing was changed yet)
// or rolls back with the snapshot. The orchestration here only decides and records; the real work lives behind
// UpgradeSteps (upgrade_live.go) so every failure point can be injected in tests.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Upgrade phases (state file). The terminal ones are committed, aborted, rolled_back and needs_human.
const (
	UpPreflight    = "preflight"
	UpStaging      = "staging"
	UpSnapshotDone = "snapshot_done"
	UpStopping     = "stopping"
	UpApplying     = "applying"
	UpApplied      = "applied"
	UpStarting     = "starting"
	UpVerifying    = "verifying"
	UpCommitted    = "committed"
	UpAborted      = "aborted"
	UpRollingBack  = "rolling_back"
	UpRolledBack   = "rolled_back"
	UpNeedsHuman   = "needs_human"
)

// Codes shown to the user.
const (
	CodeUpgradeBadVersion  = "upgrade_bad_version"
	CodeUpgradeNotNewer    = "upgrade_not_newer"
	CodeUpgradeUnresolved  = "upgrade_unresolved"
	CodeUpgradeBusy        = "upgrade_busy"
	CodeUpgradePreflight   = "upgrade_preflight_failed"
	CodeUpgradeRelease     = "upgrade_release_rejected"
	CodeUpgradeSnapshot    = "upgrade_snapshot_failed"
	CodeUpgradeApply       = "upgrade_apply_failed"
	CodeUpgradeStart       = "upgrade_start_failed"
	CodeUpgradeGate        = "upgrade_health_gate_failed"
	CodeUpgradeRolledBack  = "upgrade_failed_rolled_back"
	CodeUpgradeNeedsHuman  = "upgrade_needs_human"
	CodeUpgradeAborted     = "upgrade_aborted"
	StepUpgradeStaging     = "staging_release"
	StepUpgradeSnapshot    = "snapshot"
	StepUpgradeApplying    = "applying"
	StepUpgradeStarting    = "starting_new_version"
	StepUpgradeVerifying   = "verifying"
	StepUpgradeRollingBack = "rolling_back"
)

var upgradeVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(\.[0-9]+)?([-.][0-9A-Za-z]+)*$`)

// UpgradeState is the persisted record of an upgrade. It holds no secrets.
type UpgradeState struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Phase     string    `json:"phase"`
	Snapshot  string    `json:"snapshot,omitempty"`  // backup package taken before anything was changed
	Operation string    `json:"operation,omitempty"` // id of the configuration transaction (journal)
	Code      string    `json:"code,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s UpgradeState) terminal() bool {
	switch s.Phase {
	case UpCommitted, UpAborted, UpRolledBack, UpNeedsHuman:
		return true
	}
	return false
}

// ApplyHooks are called from inside the configuration transaction, after the staged release has been
// downloaded and verified, so a rejected release never costs a snapshot or a stop.
type ApplyHooks struct {
	// AfterValidated runs once the whole payload is staged, verified and validated; nothing is changed yet.
	// It receives the stage directory (to pre-pull the images it names).
	AfterValidated func(stageDir string) error
	// BeforeApply runs after the transaction journal is saved and just before the first file is replaced.
	BeforeApply func(operation string) error
}

// UpgradeSteps is the real work of an upgrade. Each method must be safe to call again after a crash.
type UpgradeSteps interface {
	Current(ctx context.Context) (string, error)
	Preflight(ctx context.Context, to string) error
	// ApplyConfig downloads and verifies the release, then replaces the managed files, calling the hooks.
	ApplyConfig(ctx context.Context, to string, hooks ApplyHooks) error
	Snapshot(ctx context.Context, fromVersion string) (string, error)
	Maintenance(on bool)
	StopControl(ctx context.Context) error
	StartControl(ctx context.Context) error
	// RecoverConfig recovers an interrupted, not yet committed configuration transaction (no-op otherwise).
	RecoverConfig(ctx context.Context) error
	// RevertConfig restores the files of transaction operation (also after it completed).
	RevertConfig(ctx context.Context, operation string) error
	RestoreSnapshot(ctx context.Context, name string) error
	ComposeUp(ctx context.Context) error
	Verify(ctx context.Context) error
	// AbortHelpers removes helper containers a dead Agent left running (a half-finished `compose up`).
	AbortHelpers(ctx context.Context) error
}

// Upgrader runs upgrades and resumes interrupted ones.
type Upgrader struct {
	Steps     UpgradeSteps
	StateFile string
	// LockDir is the installer's update state directory; the lock is held for the whole operation.
	LockDir string
	Now     func() time.Time
	// Crash is a test hook called after each phase is persisted; production leaves it nil.
	Crash func(phase string)
	// Logf receives short, secret-free progress lines.
	Logf func(format string, args ...any)
}

func (u *Upgrader) now() time.Time {
	if u.Now != nil {
		return u.Now().UTC()
	}
	return time.Now().UTC()
}

func (u *Upgrader) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

// Load returns the persisted state, or nil when there is none.
func (u *Upgrader) Load() (*UpgradeState, error) {
	raw, err := os.ReadFile(u.StateFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s UpgradeState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || s.Phase == "" {
		return nil, errors.New("upgrade state is invalid; preserve it for review")
	}
	return &s, nil
}

func (u *Upgrader) save(s *UpgradeState, phase string) error {
	u.logf("upgrade %s: phase %s", s.ID, phase)
	s.Phase = phase
	s.UpdatedAt = u.now()
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.StateFile), 0o700); err != nil {
		return err
	}
	if err := atomicWrite(u.StateFile, append(raw, '\n'), 0o600, false); err != nil {
		return err
	}
	if u.Crash != nil {
		u.Crash(phase)
	}
	return nil
}

// finish records a terminal phase and the code; a failure to write is reported but never hides the outcome.
func (u *Upgrader) finish(s *UpgradeState, phase, code string) {
	s.Code = code
	if err := u.save(s, phase); err != nil {
		u.logf("upgrade: could not record %s: %v", phase, err)
	}
}

func (u *Upgrader) lock() (func(), error) {
	if u.LockDir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(u.LockDir, 0o700); err != nil {
		return nil, err
	}
	return AcquireUpdateLock(u.LockDir)
}

// Run performs an upgrade to version `to`. It must be called from an Operator task (one operation at a time).
// A nil result means the new version passed the health gate and is committed. CodeUpgradeRolledBack means the
// upgrade failed and the old version is running again; CodeUpgradeNeedsHuman means it failed and the rollback
// failed too: the site is left as it is, in maintenance, for a person.
func (u *Upgrader) Run(ctx context.Context, to string, report func(step string, percent int)) error {
	if report == nil {
		report = func(string, int) {}
	}
	if !upgradeVersionPattern.MatchString(to) {
		return CodedError{CodeUpgradeBadVersion}
	}
	unlock, err := u.lock()
	if err != nil {
		return CodedError{CodeUpgradeBusy}
	}
	defer unlock()
	if prev, err := u.Load(); err != nil {
		return CodedError{CodeUpgradeUnresolved}
	} else if prev != nil && !prev.terminal() {
		return CodedError{CodeUpgradeUnresolved}
	} else if prev != nil && prev.Phase == UpNeedsHuman {
		return CodedError{CodeUpgradeNeedsHuman} // a person has not resolved the previous failure
	}
	s := &UpgradeState{ID: u.now().Format("20060102T150405Z"), To: to, StartedAt: u.now()}
	if err := u.save(s, UpPreflight); err != nil {
		return CodedError{CodeUpgradePreflight}
	}
	report(StepPreflight, 2)
	from, err := u.Steps.Current(ctx)
	if err != nil || from == "" {
		return u.abort(ctx, s, CodeUpgradePreflight, false)
	}
	s.From = from
	cmp, ok := CompareVersions(to, from)
	if !ok || cmp <= 0 {
		code := CodeUpgradeNotNewer
		if !ok {
			code = CodeUpgradeBadVersion
		}
		return u.abort(ctx, s, code, false)
	}
	if err := u.Steps.Preflight(ctx, to); err != nil {
		return u.abort(ctx, s, CodeUpgradePreflight, false)
	}
	if err := u.save(s, UpStaging); err != nil {
		return u.abort(ctx, s, CodeUpgradePreflight, false)
	}
	report(StepUpgradeStaging, 8)
	var hookCode string
	hooks := ApplyHooks{
		BeforeApply: func(operation string) error {
			s.Operation = operation
			report(StepUpgradeSnapshot, 20)
			name, err := u.Steps.Snapshot(ctx, from)
			if err != nil {
				hookCode = CodeUpgradeSnapshot
				return err
			}
			s.Snapshot = name
			if err := u.save(s, UpSnapshotDone); err != nil {
				hookCode = CodeUpgradeSnapshot
				return err
			}
			u.Steps.Maintenance(true)
			if err := u.save(s, UpStopping); err != nil {
				hookCode = CodeUpgradeApply
				return err
			}
			if err := u.Steps.StopControl(ctx); err != nil {
				hookCode = CodeUpgradeApply
				return err
			}
			report(StepUpgradeApplying, 45)
			if err := u.save(s, UpApplying); err != nil {
				hookCode = CodeUpgradeApply
				return err
			}
			return nil
		},
	}
	if err := u.Steps.ApplyConfig(ctx, to, hooks); err != nil {
		code := hookCode
		if code == "" {
			code = CodeUpgradeRelease
			if s.Phase != UpStaging {
				code = CodeUpgradeApply
			}
		}
		switch s.Phase {
		case UpPreflight, UpStaging, UpSnapshotDone, UpStopping:
			// Nothing in the installation was replaced yet: put the control plane back, no restore needed.
			return u.abort(ctx, s, code, true)
		}
		return u.rollback(ctx, s, code, report)
	}
	if err := u.save(s, UpApplied); err != nil {
		return u.rollback(ctx, s, CodeUpgradeApply, report)
	}
	report(StepUpgradeStarting, 55)
	if err := u.save(s, UpStarting); err != nil {
		return u.rollback(ctx, s, CodeUpgradeStart, report)
	}
	if err := u.Steps.ComposeUp(ctx); err != nil {
		return u.rollback(ctx, s, CodeUpgradeStart, report)
	}
	report(StepUpgradeVerifying, 75)
	if err := u.save(s, UpVerifying); err != nil {
		return u.rollback(ctx, s, CodeUpgradeGate, report)
	}
	if err := u.Steps.Verify(ctx); err != nil {
		return u.rollback(ctx, s, CodeUpgradeGate, report)
	}
	u.Steps.Maintenance(false)
	u.finish(s, UpCommitted, "")
	return nil
}

// abort ends an upgrade that changed nothing: it undoes a half-staged transaction, brings the control plane
// back if it was stopped, and records the outcome.
func (u *Upgrader) abort(ctx context.Context, s *UpgradeState, code string, cleanup bool) error {
	if cleanup {
		_ = u.Steps.RecoverConfig(ctx)
		_ = u.Steps.StartControl(ctx)
		u.Steps.Maintenance(false)
	}
	u.finish(s, UpAborted, code)
	return CodedError{code}
}

// rollback puts the old version back: stop, restore the configuration files, restore the databases and files
// from the snapshot, start the old images, pass the health gate again. It is idempotent: Resume calls it again
// after a crash in any of its steps.
func (u *Upgrader) rollback(ctx context.Context, s *UpgradeState, cause string, report func(string, int)) error {
	if report == nil {
		report = func(string, int) {}
	}
	if s.Code == "" {
		s.Code = cause
	}
	if err := u.save(s, UpRollingBack); err != nil {
		return CodedError{CodeUpgradeNeedsHuman}
	}
	report(StepUpgradeRollingBack, 85)
	err := u.Steps.AbortHelpers(ctx)
	if err == nil {
		err = u.Steps.StopControl(ctx)
	}
	if err == nil {
		err = u.Steps.RevertConfig(ctx, s.Operation)
	}
	if err == nil && s.Snapshot != "" {
		err = u.Steps.RestoreSnapshot(ctx, s.Snapshot)
	}
	if err == nil {
		err = u.Steps.ComposeUp(ctx)
	}
	if err == nil {
		err = u.Steps.Verify(ctx)
	}
	if err != nil {
		u.logf("upgrade: rollback failed")
		u.finish(s, UpNeedsHuman, CodeUpgradeNeedsHuman)
		return CodedError{CodeUpgradeNeedsHuman}
	}
	u.Steps.Maintenance(false)
	u.finish(s, UpRolledBack, CodeUpgradeRolledBack)
	return CodedError{CodeUpgradeRolledBack}
}

// Resume deals with an upgrade the Agent was killed in the middle of. Before the configuration was touched it
// only cleans up; from "applying" on it rolls back with the snapshot. Terminal or absent state is left alone.
// Call it once at start-up, from an Operator task.
func (u *Upgrader) Resume(ctx context.Context, report func(step string, percent int)) error {
	unlock, err := u.lock()
	if err != nil {
		return CodedError{CodeUpgradeBusy}
	}
	defer unlock()
	s, err := u.Load()
	if err != nil {
		return CodedError{CodeUpgradeUnresolved}
	}
	if s == nil || s.terminal() {
		return nil
	}
	u.logf("upgrade: resuming interrupted upgrade at phase %s", s.Phase)
	_ = u.Steps.AbortHelpers(ctx)
	switch s.Phase {
	case UpPreflight, UpStaging, UpSnapshotDone, UpStopping:
		_ = u.Steps.RecoverConfig(ctx)
		if err := u.Steps.StartControl(ctx); err != nil {
			u.finish(s, UpNeedsHuman, CodeUpgradeNeedsHuman)
			return CodedError{CodeUpgradeNeedsHuman}
		}
		u.Steps.Maintenance(false)
		u.finish(s, UpAborted, CodeUpgradeAborted)
		return CodedError{CodeUpgradeAborted}
	default: // applying, applied, starting, verifying, rolling_back
		return u.rollback(ctx, s, CodeUpgradeAborted, report)
	}
}
