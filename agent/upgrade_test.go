//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWorld models what an upgrade changes, so every test can assert the final state of the "site", not only
// which calls were made.
type fakeWorld struct {
	config        string // "old" | "new"
	db            string // "old" | "new"
	running       bool   // control plane running
	runningCfg    string // configuration the running containers were created from
	maintenance   bool
	snapshots     map[string]string // name -> db content at snapshot time
	calls         []string
	fail          map[string]bool // step name -> inject a failure
	configJournal string          // "", "partial", "complete", "recovered"
}

func newWorld() *fakeWorld {
	return &fakeWorld{config: "old", db: "old", running: true, runningCfg: "old", snapshots: map[string]string{}, fail: map[string]bool{}, configJournal: "recovered"}
}

type fakeSteps struct{ w *fakeWorld }

func (f fakeSteps) rec(name string) error {
	f.w.calls = append(f.w.calls, name)
	if f.w.fail[name] {
		return errors.New("injected failure at " + name)
	}
	return nil
}

func (f fakeSteps) Current(context.Context) (string, error) { return "v1.3.0", f.rec("current") }
func (f fakeSteps) Preflight(context.Context, string) error { return f.rec("preflight") }
func (f fakeSteps) ApplyConfig(_ context.Context, to string, h ApplyHooks) error {
	if err := f.rec("stage"); err != nil { // release rejected / download failed: nothing in the site changed
		return err
	}
	if h.AfterValidated != nil {
		if err := h.AfterValidated("/stage"); err != nil {
			return err
		}
	}
	f.w.configJournal = "partial"
	if h.BeforeApply != nil {
		if err := h.BeforeApply("op1"); err != nil {
			f.w.configJournal = "recovered" // the real transaction recovers itself when a hook fails
			return err
		}
	}
	if err := f.rec("apply"); err != nil {
		f.w.configJournal = "recovered"
		return err
	}
	f.w.config, f.w.configJournal = "new", "complete"
	return nil
}
func (f fakeSteps) Snapshot(context.Context, string) (string, error) {
	if err := f.rec("snapshot"); err != nil {
		return "", err
	}
	f.w.snapshots["snap1"] = f.w.db
	return "snap1", nil
}
func (f fakeSteps) Maintenance(on bool) {
	f.w.maintenance = on
	f.w.calls = append(f.w.calls, fmt.Sprintf("maintenance=%v", on))
}
func (f fakeSteps) StopControl(context.Context) error {
	if err := f.rec("stop"); err != nil {
		return err
	}
	f.w.running = false
	return nil
}
func (f fakeSteps) StartControl(context.Context) error {
	if err := f.rec("start-control"); err != nil {
		return err
	}
	f.w.running = true
	return nil
}
func (f fakeSteps) RecoverConfig(context.Context) error {
	f.w.calls = append(f.w.calls, "recover-config")
	if f.w.configJournal == "partial" {
		f.w.config, f.w.configJournal = "old", "recovered"
	}
	return nil
}
func (f fakeSteps) RevertConfig(_ context.Context, op string) error {
	if err := f.rec("revert"); err != nil {
		return err
	}
	if f.w.configJournal == "partial" || f.w.configJournal == "complete" {
		f.w.config, f.w.configJournal = "old", "recovered"
	}
	return nil
}
func (f fakeSteps) RestoreSnapshot(_ context.Context, name string) error {
	if err := f.rec("restore"); err != nil {
		return err
	}
	f.w.db = f.w.snapshots[name]
	return nil
}
func (f fakeSteps) ComposeUp(context.Context) error {
	f.w.calls = append(f.w.calls, "up")
	f.w.running, f.w.runningCfg = true, f.w.config
	if f.w.config == "new" {
		f.w.db = "new" // the new version migrates the database as soon as it starts
		if f.w.fail["up"] {
			return errors.New("injected failure at up (new version cannot start)")
		}
	}
	return nil
}
func (f fakeSteps) AbortHelpers(context.Context) error {
	f.w.calls = append(f.w.calls, "abort-helpers")
	return nil
}
func (f fakeSteps) Verify(context.Context) error {
	f.w.calls = append(f.w.calls, "verify")
	if f.w.runningCfg == "new" && f.w.fail["verify"] {
		return errors.New("injected health gate failure")
	}
	if f.w.fail["verify-always"] || (f.w.fail["verify-rollback"] && f.w.runningCfg == "old") {
		return errors.New("injected: old version does not come back")
	}
	return nil
}

func newUpgrader(t *testing.T, w *fakeWorld) *Upgrader {
	dir := t.TempDir()
	return &Upgrader{Steps: fakeSteps{w}, StateFile: filepath.Join(dir, "upgrade.json"), LockDir: filepath.Join(dir, "lock")}
}

func code(err error) string {
	var ce CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}

func state(t *testing.T, u *Upgrader) UpgradeState {
	t.Helper()
	s, err := u.Load()
	if err != nil || s == nil {
		t.Fatalf("no upgrade state: %v", err)
	}
	return *s
}

// assertOldSite checks the site is exactly as before the upgrade.
func assertOldSite(t *testing.T, w *fakeWorld) {
	t.Helper()
	if w.config != "old" || w.db != "old" || !w.running || w.runningCfg != "old" || w.maintenance {
		t.Fatalf("site is not back to the old version: config=%s db=%s running=%v runningCfg=%s maintenance=%v calls=%v", w.config, w.db, w.running, w.runningCfg, w.maintenance, w.calls)
	}
}

func TestUpgradeSuccessCommits(t *testing.T) {
	w := newWorld()
	u := newUpgrader(t, w)
	if err := u.Run(context.Background(), "v1.3.1", nil); err != nil {
		t.Fatal(err)
	}
	s := state(t, u)
	if s.Phase != UpCommitted || s.From != "v1.3.0" || s.To != "v1.3.1" || s.Snapshot != "snap1" || s.Operation != "op1" {
		t.Fatalf("%+v", s)
	}
	if w.config != "new" || w.db != "new" || !w.running || w.maintenance {
		t.Fatalf("%+v", w)
	}
	// The snapshot is taken before anything is stopped or replaced.
	order := strings.Join(w.calls, ",")
	for _, pair := range [][2]string{{"snapshot", "stop"}, {"stop", "apply"}, {"apply", "up"}, {"up", "verify"}} {
		if strings.Index(order, pair[0]) > strings.Index(order, pair[1]) {
			t.Fatalf("%s must come before %s: %s", pair[0], pair[1], order)
		}
	}
}

func TestUpgradeRefusals(t *testing.T) {
	for _, tc := range []struct{ to, want string }{
		{"1.3.1", CodeUpgradeBadVersion}, {"latest", CodeUpgradeBadVersion}, {"v1.3.1; rm -rf /", CodeUpgradeBadVersion},
		{"v1.3.0", CodeUpgradeNotNewer}, {"v1.2.9", CodeUpgradeNotNewer},
	} {
		w := newWorld()
		u := newUpgrader(t, w)
		if got := code(u.Run(context.Background(), tc.to, nil)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.to, got, tc.want)
		}
		assertOldSite(t, w)
		if len(w.snapshots) != 0 {
			t.Errorf("%s: a refused upgrade must not even take a snapshot", tc.to)
		}
	}
}

// Failures before the first file is replaced change nothing: no restore, the old version just keeps running.
func TestUpgradeFailureBeforeApplyAbortsWithoutRestore(t *testing.T) {
	for _, step := range []string{"current", "preflight", "stage", "snapshot", "stop"} {
		w := newWorld()
		w.fail[step] = true
		u := newUpgrader(t, w)
		err := u.Run(context.Background(), "v1.3.1", nil)
		if err == nil {
			t.Fatalf("%s: expected failure", step)
		}
		s := state(t, u)
		if s.Phase != UpAborted {
			t.Errorf("%s: phase %s (%s)", step, s.Phase, code(err))
		}
		assertOldSite(t, w)
		if strings.Contains(strings.Join(w.calls, ","), "restore") {
			t.Errorf("%s: nothing was changed, restoring a snapshot would be wrong: %v", step, w.calls)
		}
	}
}

// Failures from the replacement on roll back with the snapshot and bring the old version back.
func TestUpgradeFailureAfterApplyRollsBack(t *testing.T) {
	for _, tc := range []struct {
		fail string
		want string
	}{{"apply", CodeUpgradeApply}, {"up", CodeUpgradeStart}, {"verify", CodeUpgradeGate}} {
		w := newWorld()
		w.fail[tc.fail] = true
		u := newUpgrader(t, w)
		err := u.Run(context.Background(), "v1.3.1", nil)
		if code(err) != CodeUpgradeRolledBack {
			t.Fatalf("%s: %q", tc.fail, code(err))
		}
		s := state(t, u)
		if s.Phase != UpRolledBack || s.Code != CodeUpgradeRolledBack {
			t.Fatalf("%s: %+v", tc.fail, s)
		}
		assertOldSite(t, w) // config, DATABASE (migrated by the new version), running images, maintenance
	}
}

func TestUpgradeRollbackFailureLeavesSiteForAPerson(t *testing.T) {
	for _, step := range []string{"revert", "restore", "verify-rollback", "stop"} {
		w := newWorld()
		w.fail["verify"] = true // the upgrade itself fails ...
		u := newUpgrader(t, w)
		if step == "stop" { // ... and stopping for the rollback fails
			base := fakeSteps{w}
			n := 0
			u.Steps = failNth{base, "stop", 2, &n}
		} else {
			w.fail[step] = true
		}
		err := u.Run(context.Background(), "v1.3.1", nil)
		if code(err) != CodeUpgradeNeedsHuman {
			t.Fatalf("%s: %q", step, code(err))
		}
		s := state(t, u)
		if s.Phase != UpNeedsHuman {
			t.Fatalf("%s: %+v", step, s)
		}
		if !w.maintenance {
			t.Errorf("%s: the maintenance page must stay up for a person", step)
		}
		if s.Snapshot == "" {
			t.Errorf("%s: the snapshot must stay recorded", step)
		}
		// A new upgrade is refused until a person resolves it.
		if got := code(u.Run(context.Background(), "v1.3.2", nil)); got != CodeUpgradeNeedsHuman {
			t.Errorf("%s: next upgrade: %q", step, got)
		}
	}
}

// failNth wraps fakeSteps and fails StopControl on its nth call.
type failNth struct {
	fakeSteps
	name string
	n    int
	seen *int
}

func (f failNth) StopControl(ctx context.Context) error {
	*f.seen++
	if *f.seen == f.n {
		f.w.calls = append(f.w.calls, "stop(failed)")
		return errors.New("injected")
	}
	return f.fakeSteps.StopControl(ctx)
}

type crashSignal struct{ phase string }

// runUntilCrash runs the upgrade and "kills the process" (no cleanup runs) right after phase `at` is persisted.
func runUntilCrash(t *testing.T, u *Upgrader, to, at string) (crashed bool) {
	t.Helper()
	u.Crash = func(phase string) {
		if phase == at {
			panic(crashSignal{phase})
		}
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crashSignal); !ok {
				panic(r)
			}
			crashed = true
		}
		u.Crash = nil
	}()
	_ = u.Run(context.Background(), to, nil)
	return false
}

// The Agent can die at any persisted step. A fresh Agent resumes from the state file and leaves the site either
// untouched (nothing replaced yet) or on the old version restored from the snapshot — never half way.
func TestUpgradeKilledAtEveryPhaseIsRecovered(t *testing.T) {
	for _, phase := range []string{UpPreflight, UpStaging, UpSnapshotDone, UpStopping, UpApplying, UpApplied, UpStarting, UpVerifying, UpRollingBack} {
		t.Run(phase, func(t *testing.T) {
			w := newWorld()
			if phase == UpRollingBack {
				w.fail["verify"] = true // reach rolling_back through a failed health gate
			}
			u := newUpgrader(t, w)
			if !runUntilCrash(t, u, "v1.3.1", phase) {
				t.Fatalf("never reached phase %s", phase)
			}
			if s := state(t, u); s.Phase != phase {
				t.Fatalf("persisted phase %s, want %s", s.Phase, phase)
			}
			// New process: same state file, same site.
			fresh := &Upgrader{Steps: u.Steps, StateFile: u.StateFile, LockDir: u.LockDir}
			err := fresh.Resume(context.Background(), nil)
			if err == nil {
				t.Fatal("Resume must report that it had to undo an upgrade")
			}
			assertOldSite(t, w)
			s := state(t, fresh)
			wantPhase, wantCode := UpRolledBack, CodeUpgradeRolledBack
			switch phase {
			case UpPreflight, UpStaging, UpSnapshotDone, UpStopping:
				wantPhase, wantCode = UpAborted, CodeUpgradeAborted
			}
			if s.Phase != wantPhase || code(err) != wantCode {
				t.Fatalf("resume ended in %s / %s, want %s / %s (calls %v)", s.Phase, code(err), wantPhase, wantCode, w.calls)
			}
			// Resuming again is a no-op.
			before := len(w.calls)
			if err := fresh.Resume(context.Background(), nil); err != nil || len(w.calls) != before {
				t.Fatalf("second Resume must do nothing: %v %v", err, w.calls[before:])
			}
		})
	}
}

// A crash inside the rollback, then another crash inside the resumed rollback, still ends on the old version.
func TestUpgradeCrashDuringRollbackTwice(t *testing.T) {
	w := newWorld()
	w.fail["verify"] = true
	u := newUpgrader(t, w)
	if !runUntilCrash(t, u, "v1.3.1", UpRollingBack) {
		t.Fatal("no crash")
	}
	second := &Upgrader{Steps: u.Steps, StateFile: u.StateFile, LockDir: u.LockDir}
	second.Crash = func(phase string) {
		if phase == UpRollingBack {
			panic(crashSignal{phase})
		}
	}
	func() {
		defer func() { recover() }()
		_ = second.Resume(context.Background(), nil)
	}()
	third := &Upgrader{Steps: u.Steps, StateFile: u.StateFile, LockDir: u.LockDir}
	if code(third.Resume(context.Background(), nil)) != CodeUpgradeRolledBack {
		t.Fatal("rollback did not finish")
	}
	assertOldSite(t, w)
}

func TestUpgradeResumeLeavesFinishedOperationsAlone(t *testing.T) {
	for _, mode := range []string{"none", "committed", "needs_human"} {
		w := newWorld()
		u := newUpgrader(t, w)
		switch mode {
		case "committed":
			if err := u.Run(context.Background(), "v1.3.1", nil); err != nil {
				t.Fatal(err)
			}
		case "needs_human":
			w.fail["verify"], w.fail["revert"] = true, true
			_ = u.Run(context.Background(), "v1.3.1", nil)
		}
		before := append([]string{}, w.calls...)
		if err := u.Resume(context.Background(), nil); err != nil || strings.Join(before, ",") != strings.Join(w.calls, ",") {
			t.Errorf("%s: Resume touched a finished operation: %v %v", mode, err, w.calls[len(before):])
		}
	}
}

func TestUpgradeRefusesWhileAnotherOperationHoldsTheLock(t *testing.T) {
	w := newWorld()
	u := newUpgrader(t, w)
	release, err := AcquireUpdateLock(func() string { os.MkdirAll(u.LockDir, 0o700); return u.LockDir }())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := code(u.Run(context.Background(), "v1.3.1", nil)); got != CodeUpgradeBusy {
		t.Fatalf("got %q", got)
	}
	assertOldSite(t, w)
	if len(w.calls) != 0 {
		t.Fatalf("nothing may run without the lock: %v", w.calls)
	}
}

func TestUpgradeRefusesUnresolvedState(t *testing.T) {
	w := newWorld()
	u := newUpgrader(t, w)
	os.WriteFile(u.StateFile, []byte(`{"id":"x","phase":"applying","to":"v1.3.1","started_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`), 0o600)
	if got := code(u.Run(context.Background(), "v1.3.2", nil)); got != CodeUpgradeUnresolved {
		t.Fatalf("got %q", got)
	}
	os.WriteFile(u.StateFile, []byte(`{not json`), 0o600)
	if got := code(u.Run(context.Background(), "v1.3.2", nil)); got != CodeUpgradeUnresolved {
		t.Fatalf("corrupt state: %q", got)
	}
}
