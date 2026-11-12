//go:build linux

package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const txnBase = "https://updates.example.invalid/release"

// newPayload is the "new release": every managed file with distinct, recognisable content.
func newPayload(version string) map[string]string {
	return map[string]string{
		"docker-compose.yml":                      "services:\n  backend:\n    image: ${TP_BACKEND_IMAGE:-registry.example/thingspanel-go:" + version + "}\n",
		"niginx.conf":                             "new nginx " + version + "\n",
		"config/native/prepare.py":                "print('prepare " + version + "')\n",
		"config/native/runtime.encore.json":       `{"v": "` + version + `"}`,
		"config/native/runtime.notification.json": `{"n": "` + version + `"}`,
		"config/native/tick.sh":                   "#!/bin/sh\necho " + version + "\n",
		"config/native/README.md":                 "readme " + version + "\n",
		"upgrade.sh":                              "#!/bin/sh\necho upgrade " + version + "\n",
		"uninstall.sh":                            "#!/bin/sh\necho uninstall " + version + "\n",
		"config/native/update.py":                 "# updater " + version + "\n",
	}
}

var oldModes = map[string]os.FileMode{
	"docker-compose.yml": 0o644, "niginx.conf": 0o640, "config/native/prepare.py": 0o644,
	"config/native/runtime.encore.json": 0o600, "config/native/runtime.notification.json": 0o644,
	"config/native/tick.sh": 0o755, "config/native/README.md": 0o644, "upgrade.sh": 0o755, "uninstall.sh": 0o700,
	"config/native/update.py": 0o644,
}

// installOld writes the "installed" release v1 (all files, except those named in skip) plus a .env.
func installOld(t *testing.T, root string, skip ...string) map[string]string {
	t.Helper()
	old := newPayload("v1.0.0")
	for rel, content := range old {
		if inList(skip, rel) {
			delete(old, rel)
			continue
		}
		p := filepath.Join(root, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), oldModes[rel]))
		must(t, os.Chmod(p, oldModes[rel]))
	}
	must(t, os.WriteFile(filepath.Join(root, ".env"), []byte("TP_AUTH=secret-user-value\n"), 0o600))
	return old
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func memoryDownloader(files map[string]string, fail string) Downloader {
	return func(u, dest string) error {
		rel := strings.TrimPrefix(u, txnBase+"/")
		if rel == fail {
			return txnErr("Installer file download failed")
		}
		content, ok := files[rel]
		if !ok {
			return txnErr("Installer file download failed")
		}
		return durableWrite(dest, []byte(content), 0o600, nil)
	}
}

// snapshot captures every managed file's bytes and mode (and absence) for before/after comparison.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, a := range DefaultArtifacts("posix") {
		p := filepath.Join(root, a.Path)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			out[a.Path] = "<absent>"
			continue
		}
		must(t, err)
		b, err := os.ReadFile(p)
		must(t, err)
		out[a.Path] = fmt.Sprintf("%o|%s", info.Mode().Perm(), b)
	}
	b, _ := os.ReadFile(filepath.Join(root, ".env"))
	out[".env"] = string(b)
	return out
}

func equalSnapshots(t *testing.T, a, b map[string]string) {
	t.Helper()
	for k, v := range a {
		if b[k] != v {
			t.Errorf("%s differs:\n  before %q\n  after  %q", k, v, b[k])
		}
	}
}

func newTxn(root string, files map[string]string) *ConfigTransaction {
	return &ConfigTransaction{Root: root, Download: memoryDownloader(files, "")}
}

var errCrash = errors.New("simulated failure")

func crashAt(event string) func(string) error {
	return func(e string) error {
		if e == event {
			return errCrash
		}
		return nil
	}
}

func readJournal(t *testing.T, root string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".thingspanel-update/journal.json"))
	must(t, err)
	var j map[string]any
	must(t, json.Unmarshal(b, &j))
	return j
}

func TestTxnHappyPathStagesEverythingBeforeTouchingTargets(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	before := snapshot(t, root)
	tx := newTxn(root, newPayload("v2.0.0"))
	staged := false
	tx.Checkpoint = func(e string) error {
		if e == "validated" {
			equalSnapshots(t, before, snapshot(t, root)) // nothing replaced yet
			staged = true
		}
		return nil
	}
	must(t, tx.Run(txnBase, "v2.0.0", nil, false))
	if !staged {
		t.Fatal("validation checkpoint never reached")
	}
	after := snapshot(t, root)
	for rel, want := range newPayload("v2.0.0") {
		got := after[rel]
		if !strings.HasSuffix(got, "|"+want) {
			t.Errorf("%s not replaced: %q", rel, got)
		}
		// Existing permissions are preserved through the update.
		if !strings.HasPrefix(got, fmt.Sprintf("%o|", oldModes[rel])) {
			t.Errorf("%s mode changed: %q", rel, got)
		}
	}
	if after[".env"] != before[".env"] {
		t.Error(".env must be untouched")
	}
	if p := readJournal(t, root)["phase"]; p != "complete" {
		t.Errorf("phase %v", p)
	}
}

func TestTxnDownloadFailureLeavesEverythingUnchanged(t *testing.T) {
	for _, bad := range []string{"docker-compose.yml", "config/native/tick.sh", "uninstall.sh"} {
		root := t.TempDir()
		installOld(t, root)
		before := snapshot(t, root)
		tx := &ConfigTransaction{Root: root, Download: memoryDownloader(newPayload("v2.0.0"), bad)}
		if err := tx.Run(txnBase, "", nil, false); err == nil {
			t.Fatalf("%s: expected failure", bad)
		}
		equalSnapshots(t, before, snapshot(t, root))
	}
}

func TestTxnInvalidPayloadAndVersionMismatchAreRefusedBeforeCommit(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	before := snapshot(t, root)
	broken := newPayload("v2.0.0")
	broken["config/native/runtime.encore.json"] = "[1,2,3]" // not an object
	if err := newTxn(root, broken).Run(txnBase, "", nil, false); err == nil || !strings.Contains(err.Error(), "runtime.encore.json") {
		t.Fatalf("invalid json accepted: %v", err)
	}
	empty := newPayload("v2.0.0")
	empty["niginx.conf"] = "  \n"
	if err := newTxn(root, empty).Run(txnBase, "", nil, false); err == nil {
		t.Fatal("empty file accepted")
	}
	if err := newTxn(root, newPayload("v2.0.0")).Run(txnBase, "v9.9.9", nil, false); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("version mismatch accepted: %v", err)
	}
	equalSnapshots(t, before, snapshot(t, root))
	if err := newTxn(root, newPayload("v2.0.0")).Run("http://insecure.example/x", "", nil, false); err == nil {
		t.Fatal("plain http accepted")
	}
}

func TestTxnFailureAtEveryReplacementRestoresOriginalFilesAndModes(t *testing.T) {
	for _, a := range DefaultArtifacts("posix") {
		for _, event := range []string{"before-replace:" + a.Path, "after-replace:" + a.Path} {
			root := t.TempDir()
			installOld(t, root)
			before := snapshot(t, root)
			tx := newTxn(root, newPayload("v2.0.0"))
			tx.Checkpoint = crashAt(event)
			err := tx.Run(txnBase, "", nil, false)
			if !errors.Is(err, errCrash) {
				t.Fatalf("%s: %v", event, err)
			}
			equalSnapshots(t, before, snapshot(t, root))
			if p := readJournal(t, root)["phase"]; p != "recovered" {
				t.Errorf("%s: phase %v", event, p)
			}
		}
	}
}

func TestTxnAbsentFilesAndNewDirectoriesAreRemovedAgain(t *testing.T) {
	root := t.TempDir()
	installOld(t, root, "config/native/tick.sh", "config/native/update.py")
	// config/native stays because other files exist there; remove the whole directory case separately.
	tx := newTxn(root, newPayload("v2.0.0"))
	tx.Checkpoint = crashAt("config_committed")
	if err := tx.Run(txnBase, "", nil, false); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	for _, rel := range []string{"config/native/tick.sh", "config/native/update.py"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s should be absent after recovery", rel)
		}
	}
	// A fresh root: the created config directories disappear too.
	fresh := t.TempDir()
	must(t, os.WriteFile(filepath.Join(fresh, ".env"), []byte("X=1\n"), 0o600))
	tx = newTxn(fresh, newPayload("v2.0.0"))
	tx.Checkpoint = crashAt("config_committed")
	if err := tx.Run(txnBase, "", nil, false); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fresh, "config")); !os.IsNotExist(err) {
		t.Error("created config directory was not removed")
	}
}

func TestTxnUserEditsAreNeverOverwritten(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	// Edit of a watched file after validation: the update stops, the edit survives.
	tx := newTxn(root, newPayload("v2.0.0"))
	tx.Checkpoint = func(e string) error {
		if e == "validated" {
			return os.WriteFile(filepath.Join(root, ".env"), []byte("TP_AUTH=edited\n"), 0o600)
		}
		return nil
	}
	if err := tx.Run(txnBase, "", nil, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".env")); string(b) != "TP_AUTH=edited\n" {
		t.Fatal(".env edit lost")
	}
	// Foreign edit of a managed file mid-way blocks recovery instead of being overwritten.
	root2 := t.TempDir()
	installOld(t, root2)
	tx = newTxn(root2, newPayload("v2.0.0"))
	tx.Checkpoint = func(e string) error {
		if e == "before-replace:uninstall.sh" {
			must(t, os.WriteFile(filepath.Join(root2, "niginx.conf"), []byte("foreign edit\n"), 0o640))
			return errCrash
		}
		return nil
	}
	err := tx.Run(txnBase, "", nil, false)
	if err == nil || !strings.Contains(err.Error(), "externally changed") {
		t.Fatalf("recovery should require review: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root2, "niginx.conf")); string(b) != "foreign edit\n" {
		t.Fatal("foreign edit overwritten")
	}
}

func TestTxnActivationFailureKeepsCommittedConfigAndBlocksNextRun(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	tx := newTxn(root, newPayload("v2.0.0"))
	err := tx.Run(txnBase, "", func(string) error { return errCrash }, false)
	if !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if p := readJournal(t, root)["phase"]; p != "activation_started" {
		t.Fatalf("phase %v", p)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "niginx.conf")); !strings.Contains(string(b), "new nginx") {
		t.Fatal("committed configuration was rolled back")
	}
	// The next attempt must not recover (no database rollback is ever claimed).
	err = newTxn(root, newPayload("v3.0.0")).Run(txnBase, "", nil, false)
	if err == nil || !strings.Contains(err.Error(), "No database rollback was attempted") {
		t.Fatalf("expected activation refusal, got %v", err)
	}
}

func TestTxnCompletedJournalIsNotRolledBackAndMayBeRelocated(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false))
	done := snapshot(t, root)
	must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, true)) // recovery only: no-op
	equalSnapshots(t, done, snapshot(t, root))
	// Relocation: a finished installation moved elsewhere still loads.
	moved := t.TempDir()
	must(t, os.RemoveAll(moved))
	must(t, os.Rename(root, moved))
	must(t, newTxn(moved, newPayload("v2.0.0")).Run(txnBase, "", nil, true))
	// An unresolved one is bound to its original path.
	root3 := t.TempDir()
	installOld(t, root3)
	tx := newTxn(root3, newPayload("v2.0.0"))
	tx.Checkpoint = crashAt("before-replace:upgrade.sh")
	_ = tx.Run(txnBase, "", nil, false)
	// Make the journal look unresolved again (it was recovered), then move it.
	j := readJournal(t, root3)
	j["phase"] = "applying"
	raw, _ := json.Marshal(j)
	must(t, os.WriteFile(filepath.Join(root3, ".thingspanel-update/journal.json"), raw, 0o600))
	moved3 := t.TempDir()
	must(t, os.RemoveAll(moved3))
	must(t, os.Rename(root3, moved3))
	if err := newTxn(moved3, newPayload("v2.0.0")).Run(txnBase, "", nil, true); err == nil || !strings.Contains(err.Error(), "journal is invalid") {
		t.Fatalf("relocated unresolved journal accepted: %v", err)
	}
}

func TestTxnCorruptJournalMissingBackupAndSymlinksFailClosed(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	before := snapshot(t, root)
	state := filepath.Join(root, ".thingspanel-update")
	must(t, os.MkdirAll(state, 0o700))
	for _, bad := range []string{"{", `{"version":2}`, `{"version":1,"root":"x","operation":"zz","phase":"applying","files":[],"watched":{}}`} {
		must(t, os.WriteFile(filepath.Join(state, "journal.json"), []byte(bad), 0o600))
		if err := newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false); err == nil || !strings.Contains(err.Error(), "journal is invalid") {
			t.Fatalf("%q: %v", bad, err)
		}
		equalSnapshots(t, before, snapshot(t, root))
	}
	// Missing backup: crash mid-way, delete a backup, recovery must refuse and touch nothing.
	root2 := t.TempDir()
	installOld(t, root2)
	tx := newTxn(root2, newPayload("v2.0.0"))
	tx.Checkpoint = func(e string) error {
		if e == "after-replace:niginx.conf" {
			return errors.New("stop") // recover will run; break it first below
		}
		return nil
	}
	// Run with a failing recovery by removing the backup right at the failure point.
	tx.Checkpoint = func(e string) error {
		if e == "after-replace:niginx.conf" {
			ops, _ := filepath.Glob(filepath.Join(root2, ".thingspanel-update", "*", "backup", "niginx.conf"))
			for _, p := range ops {
				_ = os.Remove(p)
			}
			return errCrash
		}
		return nil
	}
	err := tx.Run(txnBase, "", nil, false)
	if err == nil || !strings.Contains(err.Error(), "backup is missing") {
		t.Fatalf("recovery with a missing backup must fail closed: %v", err)
	}
	// Symlinked managed target and symlinked state directory are not followed.
	root3 := t.TempDir()
	installOld(t, root3)
	must(t, os.Remove(filepath.Join(root3, "niginx.conf")))
	must(t, os.Symlink("/etc/hostname", filepath.Join(root3, "niginx.conf")))
	if err := newTxn(root3, newPayload("v2.0.0")).Run(txnBase, "", nil, false); err == nil {
		t.Fatal("symlink target accepted")
	}
	root4 := t.TempDir()
	must(t, os.Symlink(t.TempDir(), filepath.Join(root4, ".thingspanel-update")))
	if err := newTxn(root4, newPayload("v2.0.0")).Run(txnBase, "", nil, false); err == nil {
		t.Fatal("symlinked state directory accepted")
	}
}

func TestTxnSecondRunnerIsRefusedByTheLock(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	state := filepath.Join(root, ".thingspanel-update")
	must(t, os.MkdirAll(state, 0o700))
	release, err := AcquireUpdateLock(state)
	must(t, err)
	if _, err2 := AcquireUpdateLock(state); !errors.Is(err2, ErrUpdateLockHeld) {
		t.Fatalf("second lock: %v", err2)
	}
	before := snapshot(t, root)
	if err := newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false); !errors.Is(err, ErrUpdateLockHeld) {
		t.Fatalf("run during lock: %v", err)
	}
	equalSnapshots(t, before, snapshot(t, root))
	release()
	must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false))
}

// ---- crash tests with a real SIGKILL in a child process ----

// helperTxn is run as a child process (see runHelper) and kills itself at the requested checkpoint.
func TestHelperTxn(t *testing.T) {
	root := os.Getenv("TP_TXN_ROOT")
	if root == "" {
		t.Skip("child process helper")
	}
	event, mode := os.Getenv("TP_TXN_EVENT"), os.Getenv("TP_TXN_MODE")
	tx := newTxn(root, newPayload("v2.0.0"))
	tx.Checkpoint = func(e string) error {
		if e != event {
			return nil
		}
		switch mode {
		case "kill":
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		case "hold":
			_ = os.WriteFile(filepath.Join(root, ".worker-ready"), []byte("ready"), 0o600)
			time.Sleep(60 * time.Second)
		}
		return nil
	}
	_ = tx.Run(txnBase, "", nil, mode == "recover")
}

func runHelper(t *testing.T, root, mode, event string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperTxn$")
	cmd.Env = append(os.Environ(), "TP_TXN_ROOT="+root, "TP_TXN_MODE="+mode, "TP_TXN_EVENT="+event)
	return cmd
}

func TestTxnKill9DuringReplacementIsRecoveredByNewProcess(t *testing.T) {
	for _, event := range []string{"before-replace:niginx.conf", "after-replace:config/native/tick.sh", "config_committed"} {
		root := t.TempDir()
		installOld(t, root)
		before := snapshot(t, root)
		err := runHelper(t, root, "kill", event).Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("%s: child was not killed by SIGKILL: %v", event, err)
		}
		if p := readJournal(t, root)["phase"]; p == "complete" || p == "recovered" {
			t.Fatalf("%s: journal phase %v after kill", event, p)
		}
		must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, true)) // recovery only
		equalSnapshots(t, before, snapshot(t, root))
		if p := readJournal(t, root)["phase"]; p != "recovered" {
			t.Fatalf("%s: phase %v", event, p)
		}
		must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, true)) // idempotent
		equalSnapshots(t, before, snapshot(t, root))
	}
}

func TestTxnLockIsHeldByLiveProcessAndReleasedByItsDeath(t *testing.T) {
	root := t.TempDir()
	installOld(t, root)
	cmd := runHelper(t, root, "hold", "config_committed")
	must(t, cmd.Start())
	defer cmd.Process.Kill()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, ".worker-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never reached the checkpoint")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false); !errors.Is(err, ErrUpdateLockHeld) {
		t.Fatalf("expected lock refusal while another process works: %v", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	// The dead process left a half-applied transaction; the next run recovers it and then proceeds.
	must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false))
}

// ---- signed release ----

type signer struct {
	key  *ecdsa.PrivateKey
	root string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	must(t, err)
	p := filepath.Join(t.TempDir(), "root.pem")
	must(t, os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644))
	return signer{key: key, root: p}
}

type manifestOpts struct {
	Version, Publisher string
	NotBefore, Expires int64
	Revoked            []string
	Omit               string
}

func (s signer) manifest(t *testing.T, payload map[string]string, o manifestOpts) ([]byte, []byte) {
	t.Helper()
	if o.Version == "" {
		o.Version = "v2.0.0"
	}
	if o.Publisher == "" {
		o.Publisher = "https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml"
	}
	if o.Expires == 0 {
		o.NotBefore, o.Expires = time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()
	}
	var files []map[string]any
	for rel, c := range payload {
		if rel == o.Omit {
			continue
		}
		files = append(files, map[string]any{"path": rel, "sha256": manifestDigest([]byte(c)), "size": len(c)})
	}
	doc := map[string]any{"schema": ReleaseSchema, "files": files, "revoked_versions": o.Revoked,
		"release": map[string]any{"version": o.Version, "publisher": o.Publisher, "commit": strings.Repeat("a", 40), "not_before": o.NotBefore, "expires_at": o.Expires}}
	raw, err := json.Marshal(doc)
	must(t, err)
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, sha256Sum(raw))
	must(t, err)
	return raw, sig
}

func signedDownloader(files map[string]string, manifest, sig []byte) Downloader {
	inner := memoryDownloader(files, "")
	return func(u, dest string) error {
		switch strings.TrimPrefix(u, txnBase+"/") {
		case "release-manifest.json":
			return durableWrite(dest, manifest, 0o600, nil)
		case "release-manifest.json.sig":
			return durableWrite(dest, sig, 0o600, nil)
		}
		return inner(u, dest)
	}
}

func TestSignedReleaseMatrix(t *testing.T) {
	good := newSigner(t)
	other := newSigner(t)
	payload := newPayload("v2.0.0")
	now := time.Now()
	cases := []struct {
		name   string
		build  func() ([]byte, []byte)
		policy ReleasePolicy
		files  map[string]string // payload actually served
		reason string            // "" means accepted
	}{
		{name: "valid", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{}) }, reason: ""},
		{name: "tampered file", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{}) },
			files: func() map[string]string { p := newPayload("v2.0.0"); p["niginx.conf"] = "evil\n"; return p }(), reason: "digest_mismatch"},
		{name: "other key", build: func() ([]byte, []byte) { return other.manifest(t, payload, manifestOpts{}) }, reason: "bad_signature"},
		{name: "file not in manifest", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{Omit: "uninstall.sh"}) }, reason: "unlisted_file"},
		{name: "revoked outside", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{}) },
			policy: ReleasePolicy{Revoked: []string{"v2.0.0"}}, reason: "revoked"},
		{name: "revoked inside", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{Revoked: []string{"v2.0.0"}}) }, reason: "revoked"},
		{name: "expired", build: func() ([]byte, []byte) {
			return good.manifest(t, payload, manifestOpts{NotBefore: now.Add(-48 * time.Hour).Unix(), Expires: now.Add(-time.Hour).Unix()})
		}, reason: "expired"},
		{name: "not yet valid", build: func() ([]byte, []byte) {
			return good.manifest(t, payload, manifestOpts{NotBefore: now.Add(time.Hour).Unix(), Expires: now.Add(2 * time.Hour).Unix()})
		}, reason: "not_yet_valid"},
		{name: "other publisher", build: func() ([]byte, []byte) {
			return good.manifest(t, payload, manifestOpts{Publisher: "https://evil.example/workflow.yml"})
		}, reason: "wrong_publisher"},
		{name: "other version", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{Version: "v3.0.0"}) }, reason: "version_mismatch"},
		{name: "no trust root", build: func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{}) },
			policy: ReleasePolicy{TrustRoot: filepath.Join(t.TempDir(), "missing.pem")}, reason: "trust_root_not_configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			installOld(t, root)
			before := snapshot(t, root)
			manifest, sig := c.build()
			served := c.files
			if served == nil {
				served = payload
			}
			policy := c.policy
			if policy.TrustRoot == "" {
				policy.TrustRoot = good.root
			}
			if policy.Publisher == "" {
				policy.Publisher = "https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml"
			}
			tx := &ConfigTransaction{Root: root, Download: signedDownloader(served, manifest, sig), Release: &policy}
			err := tx.Run(txnBase, "v2.0.0", nil, false)
			if c.reason == "" {
				must(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), "Signed release verification failed: "+c.reason) {
				t.Fatalf("want reason %q, got %v", c.reason, err)
			}
			equalSnapshots(t, before, snapshot(t, root)) // a rejected release never touches the install
		})
	}
}

func TestComposeBackendImageParsing(t *testing.T) {
	text := "services:\n  postgres:\n    image: postgres:14\n  backend:\n    # comment\n    image: \"${TP_BACKEND_IMAGE:-reg/x:v1.3.0}\"\n    environment:\n      A: b\n  frontend:\n    image: web:latest\n"
	got, ok := ComposeBackendImage(text)
	if !ok || got != "${TP_BACKEND_IMAGE:-reg/x:v1.3.0}" {
		t.Fatalf("%q %v", got, ok)
	}
	if _, ok := ComposeBackendImage("services:\n  web:\n    image: a:b\n"); ok {
		t.Fatal("no backend service must not match")
	}
}

func sha256Sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }
