//go:build linux

package agent

// Interoperability with the Python implementation (update.py, release_manifest.py): a transaction started by
// one side must be recoverable by the other, the two must exclude each other through the shared lock, and the
// two release verifiers must reject the same inputs for the same reason. These tests need python3 (and, for the
// verifier comparison, the openssl CLI); without them they are skipped and say so. They run update.py itself,
// not a copy of its behaviour.

import (
	"bufio"
	"encoding/json"
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

func nativeDir(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available: interoperability with update.py not exercised")
	}
	dir, err := filepath.Abs("../config/native")
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "update.py")); err != nil {
		t.Skip("update.py not found next to the agent: interoperability not exercised")
	}
	return dir
}

func py(t *testing.T, native string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"-I", "testdata/py_txn.py"}, args...)...)
	cmd.Env = append(os.Environ(), "TP_NATIVE_DIR="+native)
	return cmd
}

func writePayload(t *testing.T, version string) string {
	t.Helper()
	raw, _ := json.Marshal(newPayload(version))
	p := filepath.Join(t.TempDir(), "payload.json")
	must(t, os.WriteFile(p, raw, 0o600))
	return p
}

func TestInteropPythonKilledTransactionIsRecoveredByGo(t *testing.T) {
	native := nativeDir(t)
	for _, event := range []string{"before-replace:niginx.conf", "after-replace:config/native/prepare.py", "config_committed"} {
		root := t.TempDir()
		installOld(t, root)
		before := snapshot(t, root)
		err := py(t, native, "run", root, writePayload(t, "v2.0.0"), "kill", event).Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("%s: python was not killed: %v", event, err)
		}
		// Go loads the journal Python wrote, validates it as strictly, and restores the originals.
		must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, true))
		equalSnapshots(t, before, snapshot(t, root))
		if p := readJournal(t, root)["phase"]; p != "recovered" {
			t.Fatalf("%s: phase %v", event, p)
		}
	}
}

func TestInteropGoKilledTransactionIsRecoveredByPython(t *testing.T) {
	native := nativeDir(t)
	for _, event := range []string{"before-replace:niginx.conf", "after-replace:config/native/prepare.py", "config_committed"} {
		root := t.TempDir()
		installOld(t, root)
		before := snapshot(t, root)
		err := runHelper(t, root, "kill", event).Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("%s: go helper was not killed: %v", event, err)
		}
		out, err := py(t, native, "run", root, writePayload(t, "v2.0.0"), "recover", "").CombinedOutput()
		if err != nil || strings.Contains(string(out), "PYERROR") {
			t.Fatalf("%s: python could not recover the Go journal: %v %s", event, err, out)
		}
		equalSnapshots(t, before, snapshot(t, root))
		if p := readJournal(t, root)["phase"]; p != "recovered" {
			t.Fatalf("%s: phase %v", event, p)
		}
	}
}

func TestInteropCompletedGoTransactionIsAcceptedByPythonAndViceVersa(t *testing.T) {
	native := nativeDir(t)
	root := t.TempDir()
	installOld(t, root)
	must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false))
	done := snapshot(t, root)
	// Python runs a whole new transaction on top of a Go-completed journal ...
	out, err := py(t, native, "run", root, writePayload(t, "v3.0.0"), "run", "").CombinedOutput()
	if err != nil || strings.Contains(string(out), "PYERROR") {
		t.Fatalf("python rejected the Go journal: %v %s", err, out)
	}
	if got := snapshot(t, root)["niginx.conf"]; !strings.Contains(got, "new nginx v3.0.0") {
		t.Fatalf("python did not apply: %q", got)
	}
	// ... and Go accepts Python's completed journal and updates again.
	must(t, newTxn(root, newPayload("v4.0.0")).Run(txnBase, "", nil, false))
	if got := snapshot(t, root)["niginx.conf"]; !strings.Contains(got, "new nginx v4.0.0") {
		t.Fatalf("go did not apply: %q", got)
	}
	_ = done
}

func TestInteropTheLockExcludesTheOtherImplementation(t *testing.T) {
	native := nativeDir(t)
	// Go holds it: Python is refused.
	root := t.TempDir()
	state := filepath.Join(root, ".thingspanel-update")
	must(t, os.MkdirAll(state, 0o700))
	release, err := AcquireUpdateLock(state)
	must(t, err)
	out, err := py(t, native, "trylock", state).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "BUSY" {
		t.Fatalf("python should be refused while Go holds the lock: %v %q", err, out)
	}
	release()
	out, _ = py(t, native, "trylock", state).CombinedOutput()
	if strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("python should get the lock after Go released it: %q", out)
	}
	// Python holds it: Go is refused, and a whole Go run touches nothing.
	installOld(t, root)
	before := snapshot(t, root)
	holder := py(t, native, "lock", state)
	stdin, err := holder.StdinPipe()
	must(t, err)
	stdout, err := holder.StdoutPipe()
	must(t, err)
	must(t, holder.Start())
	defer holder.Process.Kill()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "READY" {
		t.Fatalf("python lock holder: %q", line)
	}
	if err := newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false); !errors.Is(err, ErrUpdateLockHeld) {
		t.Fatalf("go should be refused while python holds the lock: %v", err)
	}
	equalSnapshots(t, before, snapshot(t, root))
	stdin.Close()
	_ = holder.Wait()
	must(t, newTxn(root, newPayload("v2.0.0")).Run(txnBase, "", nil, false))
}

// Both verifiers must reject the same inputs for the same reason.
func TestInteropReleaseVerifiersAgree(t *testing.T) {
	native := nativeDir(t)
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl CLI not available: Python verifier cannot run")
	}
	good, other := newSigner(t), newSigner(t)
	payload := newPayload("v2.0.0")
	publisher := "https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml"
	now := time.Now()
	type tc struct {
		name     string
		signer   signer
		opts     manifestOpts
		version  string
		revoked  string
		publish  string
		tamper   bool
		noTrust  bool
		nowEpoch int64
	}
	cases := []tc{
		{name: "valid", signer: good},
		{name: "other key", signer: other},
		{name: "tampered", signer: good, tamper: true},
		{name: "unlisted", signer: good, opts: manifestOpts{Omit: "uninstall.sh"}},
		{name: "revoked outside", signer: good, revoked: "v2.0.0"},
		{name: "revoked inside", signer: good, opts: manifestOpts{Revoked: []string{"v2.0.0"}}},
		{name: "expired", signer: good, opts: manifestOpts{NotBefore: now.Add(-48 * time.Hour).Unix(), Expires: now.Add(-time.Hour).Unix()}},
		{name: "not yet", signer: good, opts: manifestOpts{NotBefore: now.Add(time.Hour).Unix(), Expires: now.Add(2 * time.Hour).Unix()}},
		{name: "publisher", signer: good, publish: "https://evil.example/w.yml"},
		{name: "version", signer: good, version: "v9.9.9"},
		{name: "no trust root", signer: good, noTrust: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			manifest, sig := c.signer.manifest(t, payload, c.opts)
			dir := t.TempDir()
			files := map[string]string{}
			for rel, content := range payload {
				if c.tamper && rel == "niginx.conf" {
					content = "evil\n"
				}
				p := filepath.Join(dir, "stage", rel)
				must(t, os.MkdirAll(filepath.Dir(p), 0o755))
				must(t, os.WriteFile(p, []byte(content), 0o644))
				files[rel] = p
			}
			mp, sp, fp := filepath.Join(dir, "m.json"), filepath.Join(dir, "m.sig"), filepath.Join(dir, "files.json")
			must(t, os.WriteFile(mp, manifest, 0o644))
			must(t, os.WriteFile(sp, sig, 0o644))
			rawFiles, _ := json.Marshal(files)
			must(t, os.WriteFile(fp, rawFiles, 0o644))
			trust := good.root
			if c.noTrust {
				trust = filepath.Join(dir, "missing.pem")
			}
			publisherArg := publisher
			if c.publish != "" {
				publisherArg = c.publish
			}
			nowArg := now.Unix()
			out, err := py(t, native, "verify", trust, mp, sp, publisherArg, c.version, c.revoked, fmt.Sprint(nowArg), fp).CombinedOutput()
			if err != nil {
				t.Fatalf("python verifier failed to run: %v %s", err, out)
			}
			pyResult := strings.TrimSpace(string(out))
			policy := ReleasePolicy{TrustRoot: trust, Publisher: publisherArg, Now: nowArg}
			if c.revoked != "" {
				policy.Revoked = []string{c.revoked}
			}
			goResult := "OK"
			if _, err := VerifyFiles(manifest, sig, policy, c.version, files); err != nil {
				goResult = err.(*ReleaseError).Reason
			}
			if goResult != pyResult {
				t.Fatalf("verifiers disagree: go=%q python=%q", goResult, pyResult)
			}
		})
	}
}
