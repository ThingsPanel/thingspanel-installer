//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompareAndCompatibility(t *testing.T) {
	cases := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.4.0", "v1.4.0", 0, true}, {"v1.3.0", "v1.4.0", -1, true}, {"v1.10.0", "v1.9.9", 1, true},
		{"r3-aebaed52", "r3-aebaed52", 0, true}, {"r3-aebaed52", "v1.4.0", 0, false}, {"unknown", "unknown", 0, false}, {"", "v1.0.0", 0, false},
	}
	for _, c := range cases {
		if cmp, ok := CompareVersions(c.a, c.b); cmp != c.cmp || ok != c.ok {
			t.Errorf("Compare(%q,%q) = %d,%v want %d,%v", c.a, c.b, cmp, ok, c.cmp, c.ok)
		}
	}
	m := func(v string, sys int, ts string) Manifest {
		return Manifest{PlatformVersion: v, Stores: map[string]StoreInfo{"relational": {Timescaledb: ts, SysVersion: sys}}}
	}
	code := func(err error) string {
		var ce CodedError
		if errors.As(err, &ce) {
			return ce.Code
		}
		return "ok"
	}
	for _, c := range []struct {
		name string
		m    Manifest
		want string
	}{
		{"same", m("v1.4.0", 25, "2.8.1"), "ok"},
		{"older backup restores and migrates forward", m("v1.3.0", 20, "2.8.1"), "ok"},
		{"newer backup refused", m("v1.5.0", 25, "2.8.1"), CodeRestoreNewer},
		{"timescale differs", m("v1.4.0", 25, "2.9.0"), CodeRestoreTimescale},
		{"schema newer than the program", m("v1.4.0", 30, "2.8.1"), CodeRestoreSchema},
		{"unknown version", m("unknown", 25, "2.8.1"), CodeRestoreVersion},
	} {
		if got := code(CheckRestoreCompatible(c.m, "v1.4.0", 25, "2.8.1")); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

type restoreFixtureT struct {
	svc    *RestoreService
	runner *fakeRunner
	mgr    *fakeManager
	root   string
	backup string
}

func restoreFixture(t *testing.T) restoreFixtureT {
	t.Helper()
	b, runner, root := backupFixture(t)
	runner.log = nil
	name, err := b.Create(context.Background(), "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.log, runner.dumps = nil, nil
	mgr := &fakeManager{}
	cfg := restartConfig(filepath.Join(root, "status"))
	_ = os.MkdirAll(cfg.StatusDir, 0o755)
	mgr.fakeInspector = fakeInspector{states: allUp()}
	verifier := &Verifier{Config: cfg, Inspector: mgr, Prober: fakeProber{}, Sleep: func(d time.Duration) {}}
	op := &Operator{Config: cfg, Manager: mgr, Verifier: verifier, VerifyBudget: time.Second}
	svc := &RestoreService{Backup: b, Operator: op, Manager: mgr, Verifier: verifier, VerifyBudget: time.Second}
	return restoreFixtureT{svc, runner, mgr, root, name}
}

// nothing (a dead host) is running: the restore still starts PostgreSQL and Redis before anything else.
func TestRestoreStartsDataServicesFirst(t *testing.T) {
	f := restoreFixture(t)
	if err := f.svc.Restore(context.Background(), f.backup, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.mgr.calls) < 1 || f.mgr.calls[0] != "start:postgres" {
		t.Fatalf("first calls: %v", f.mgr.calls)
	}
}

func (f restoreFixtureT) flagOn() bool {
	_, err := os.Stat(filepath.Join(f.svc.Operator.Config.StatusDir, "active"))
	return err == nil
}

func indexOf(log []string, prefix string, from int) int {
	for i := from; i < len(log); i++ {
		if strings.HasPrefix(log[i], prefix) {
			return i
		}
	}
	return -1
}

func TestRestoreHappyPathOrder(t *testing.T) {
	f := restoreFixture(t)
	if err := f.svc.Restore(context.Background(), f.backup, nil); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(f.mgr.calls, " ")
	if !strings.Contains(calls, "start:postgres") || strings.Index(calls, "start:postgres") > strings.Index(calls, "stop:") {
		t.Fatalf("data services must be started first:\n%s", calls)
	}
	wantStop := "stop:automation-scheduler stop:encore-api stop:backend stop:mosquitto stop:broker-owner"
	if !strings.Contains(calls, wantStop) {
		t.Fatalf("stop order:\n%s", calls)
	}
	if !strings.HasSuffix(calls, "start:broker-owner start:mosquitto start:backend start:encore-api start:automation-scheduler") {
		t.Fatalf("start order:\n%s", calls)
	}
	// safety snapshot exists, named pre-restore, taken after the stack was stopped
	var pre int
	for _, b := range f.svc.Backup.List() {
		if b.Trigger == "pre-restore" {
			pre++
		}
	}
	if pre != 1 {
		t.Fatalf("pre-restore snapshots: %d", pre)
	}
	// per database: terminate, drop, create, create extension, pre_restore, pg_restore, post_restore, in that order
	log := f.runner.log
	i := indexOf(log, "psql -U postgres -d postgres -At -c select pg_terminate_backend", 0)
	if i < 0 {
		t.Fatalf("no terminate:\n%v", log)
	}
	for _, step := range []string{`psql -U postgres -d postgres -At -c drop database if exists "ThingsPanel"`, `psql -U postgres -d postgres -At -c select pg_encoding_to_char`, `psql -U postgres -d postgres -At -c create database "ThingsPanel" template template0 encoding 'UTF8' lc_collate 'C' lc_ctype 'C'`,
		"psql -U postgres -d ThingsPanel -At -c create extension if not exists timescaledb", "psql -U postgres -d ThingsPanel -At -c select timescaledb_pre_restore()", "pg_restore -U postgres -d ThingsPanel --no-owner --exit-on-error /tmp/tp-restore/ThingsPanel.dump", "psql -U postgres -d ThingsPanel -At -c select timescaledb_post_restore()"} {
		j := indexOf(log, step, i)
		if j < 0 {
			t.Fatalf("step %q missing or out of order after index %d:\n%s", step, i, strings.Join(log, "\n"))
		}
		i = j
	}
	if f.flagOn() {
		t.Fatal("maintenance flag left on after success")
	}
	if len(f.runner.copied) < 2 {
		t.Fatalf("dumps and files were not copied in: %v", f.runner.copied)
	}
	if indexOf(log, "rm -rf /tmp/tp-restore", 0) < 0 {
		t.Fatal("dump work directory was not removed")
	}
}

func TestRestoreRefusalsTouchNothing(t *testing.T) {
	f := restoreFixture(t)
	// a package from a program newer than the running one
	f.svc.Backup.Runner.(*fakeRunner).log = nil
	f.svc.Backup.Adapter.(*PostgresAdapter).Runner = f.runner
	// make the running program older: the fake reports v1.4.0; the package says v1.4.0 -> pretend the program is v1.3.0
	old := f.svc.Backup.Runner.(*fakeRunner)
	_ = old
	p := filepath.Join(f.svc.Backup.Config.Dir, f.backup)
	raw, _ := os.ReadFile(p)
	_ = os.WriteFile(p, raw[:len(raw)/3], 0o600) // truncated package
	f.mgr.calls = nil
	err := f.svc.Restore(context.Background(), f.backup, nil)
	var ce CodedError
	if !errors.As(err, &ce) || ce.Code != CodeRestoreVerify {
		t.Fatalf("err=%v", err)
	}
	for _, c := range f.mgr.calls {
		if strings.HasPrefix(c, "stop:") {
			t.Fatalf("a refused restore stopped something: %v", f.mgr.calls)
		}
	}
	if f.flagOn() {
		t.Fatal("a refused restore turned maintenance on")
	}
	if indexOf(f.runner.log, "psql -U postgres -d postgres -At -c drop", 0) >= 0 {
		t.Fatal("a database was dropped")
	}
}

func TestRestoreRollsBackToTheSafetySnapshotWhenDatabaseRestoreFails(t *testing.T) {
	f := restoreFixture(t)
	f.runner.failRestore = 1 // the first pg_restore (of the requested backup) fails; the rollback's succeeds
	err := f.svc.Restore(context.Background(), f.backup, nil)
	var ce CodedError
	if !errors.As(err, &ce) || ce.Code != CodeRolledBack {
		t.Fatalf("err=%v", err)
	}
	if f.flagOn() {
		t.Fatal("maintenance flag must be off after a successful rollback")
	}
	n := 0
	for _, l := range f.runner.log {
		if strings.HasPrefix(l, "pg_restore -U postgres -d ThingsPanel ") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected the failed restore and the rollback restore, got %d", n)
	}
	if !strings.HasSuffix(strings.Join(f.mgr.calls, " "), "start:automation-scheduler") {
		t.Fatalf("stack not started after rollback: %v", f.mgr.calls)
	}
}

func TestRollbackFailureStaysStoppedForAPerson(t *testing.T) {
	f := restoreFixture(t)
	f.runner.failRestore = 99 // every pg_restore fails, including the rollback
	err := f.svc.Restore(context.Background(), f.backup, nil)
	var ce CodedError
	if !errors.As(err, &ce) || ce.Code != CodeRollbackFailed {
		t.Fatalf("err=%v", err)
	}
	if !f.flagOn() {
		t.Fatal("the maintenance flag must stay on when a person has to step in")
	}
	// the safety snapshot is kept
	pre := 0
	for _, b := range f.svc.Backup.List() {
		if b.Trigger == "pre-restore" {
			pre++
		}
	}
	if pre != 1 {
		t.Fatalf("safety snapshot must be kept, found %d", pre)
	}
}

func TestRestoreConfigIsAtomicSkipsIdenticalAndRespectsOpenJournal(t *testing.T) {
	f := restoreFixture(t)
	compose := filepath.Join(f.root, "docker-compose.yml")
	_ = os.WriteFile(compose, []byte("services: {changed: true}\n"), 0o600)
	m, _ := f.svc.Backup.Verify(f.backup)
	// open installer transaction: refuse, change nothing
	_ = os.MkdirAll(filepath.Join(f.root, ".thingspanel-update"), 0o700)
	_ = os.WriteFile(filepath.Join(f.root, ".thingspanel-update/journal.json"), []byte(`{"phase":"activation_started"}`), 0o600)
	if err := f.svc.restoreConfig(filepath.Join(f.svc.Backup.Config.Dir, f.backup), m); err == nil {
		t.Fatal("config restored while an installer transaction was open")
	}
	if b, _ := os.ReadFile(compose); !strings.Contains(string(b), "changed") {
		t.Fatal("file changed despite the open transaction")
	}
	_ = os.WriteFile(filepath.Join(f.root, ".thingspanel-update/journal.json"), []byte(`{"phase":"complete"}`), 0o600)
	if err := f.svc.restoreConfig(filepath.Join(f.svc.Backup.Config.Dir, f.backup), m); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(compose); string(b) != "services: {}\n" {
		t.Fatalf("config not restored: %q", b)
	}
	// secrets that were never in the package are untouched
	if b, _ := os.ReadFile(filepath.Join(f.root, ".env")); string(b) != "SECRET=1\n" {
		t.Fatal(".env was touched")
	}
}

// With the backend stopped, the program version comes from the last time it answered.
func TestPlatformVersionFallsBackToLastKnown(t *testing.T) {
	svc, runner, _ := backupFixture(t)
	if v := svc.platformVersion(context.Background()); v != "v1.4.0" {
		t.Fatalf("live: %s", v)
	}
	down := &failingVersionRunner{fakeRunner: runner}
	svc.Runner = down
	if v := svc.platformVersion(context.Background()); v != "v1.4.0" {
		t.Fatalf("fallback: %s", v)
	}
	_ = os.Remove(filepath.Join(svc.Config.Dir, ".last-version"))
	if v := svc.platformVersion(context.Background()); v != "unknown" {
		t.Fatalf("no cache: %s", v)
	}
}

type failingVersionRunner struct{ *fakeRunner }

func (f *failingVersionRunner) ExecStream(ctx context.Context, p, s string, argv []string, w io.Writer) (int, string, error) {
	if argv[0] == "/app/go" {
		return -1, "", ErrNoContainer
	}
	return f.fakeRunner.ExecStream(ctx, p, s, argv, w)
}
