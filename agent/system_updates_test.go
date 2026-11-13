//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func updateChecker(t *testing.T, s signer, manifest, sig []byte, current string, now time.Time) *UpdateChecker {
	t.Helper()
	files := map[string]string{}
	return &UpdateChecker{
		Config:   UpgradeConfig{Source: txnBase, TrustRoot: s.root, Publisher: "https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml"},
		Download: signedDownloader(files, manifest, sig),
		Current:  func(context.Context) (string, bool) { return current, current != "" },
		Now:      func() time.Time { return now },
	}
}

func TestUpdateCheckReportsNewerVerifiedRelease(t *testing.T) {
	s := newSigner(t)
	manifest, sig := s.manifest(t, newPayload("v1.4.0"), manifestOpts{Version: "v1.4.0"})
	info, err := updateChecker(t, s, manifest, sig, "v1.3.0", time.Now()).Check(context.Background())
	if err != nil || !info.Newer || info.Latest != "v1.4.0" || info.Current != "v1.3.0" || !info.ValidTo.After(time.Now()) {
		t.Fatalf("%+v %v", info, err)
	}
	same, _ := updateChecker(t, s, manifest, sig, "v1.4.0", time.Now()).Check(context.Background())
	older, _ := updateChecker(t, s, manifest, sig, "v1.5.0", time.Now()).Check(context.Background())
	if same.Newer || older.Newer {
		t.Fatalf("an equal or older release must not be offered: %+v %+v", same, older)
	}
}

func TestUpdateCheckRejectsWhatAnUpgradeWouldReject(t *testing.T) {
	good, other := newSigner(t), newSigner(t)
	payload := newPayload("v1.4.0")
	now := time.Now()
	bad := func(name string, mk func() ([]byte, []byte), want string, mutate func(*UpdateChecker)) {
		t.Run(name, func(t *testing.T) {
			m, sg := mk()
			c := updateChecker(t, good, m, sg, "v1.3.0", now)
			if mutate != nil {
				mutate(c)
			}
			_, err := c.Check(context.Background())
			var ce CodedError
			if !errors.As(err, &ce) || ce.Code != want {
				t.Fatalf("want %s, got %v", want, err)
			}
		})
	}
	bad("signed by another key", func() ([]byte, []byte) { return other.manifest(t, payload, manifestOpts{Version: "v1.4.0"}) }, CodeUpdateRejected, nil)
	bad("expired", func() ([]byte, []byte) {
		return good.manifest(t, payload, manifestOpts{Version: "v1.4.0", NotBefore: now.Add(-48 * time.Hour).Unix(), Expires: now.Add(-time.Hour).Unix()})
	}, CodeUpdateRejected, nil)
	bad("revoked", func() ([]byte, []byte) {
		return good.manifest(t, payload, manifestOpts{Version: "v1.4.0", Revoked: []string{"v1.4.0"}})
	}, CodeUpdateRejected, nil)
	bad("other publisher", func() ([]byte, []byte) {
		return good.manifest(t, payload, manifestOpts{Version: "v1.4.0", Publisher: "https://evil.example/w.yml"})
	}, CodeUpdateRejected, nil)
	bad("no source", func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{Version: "v1.4.0"}) }, CodeUpdateNoSource, func(c *UpdateChecker) { c.Config.Source = "" })
	bad("unknown current version", func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{Version: "v1.4.0"}) }, CodeUpdateNoVersion, func(c *UpdateChecker) {
		c.Current = func(context.Context) (string, bool) { return "", false }
	})
	bad("fetch fails", func() ([]byte, []byte) { return good.manifest(t, payload, manifestOpts{Version: "v1.4.0"}) }, CodeUpdateFetch, func(c *UpdateChecker) {
		c.Download = func(string, string) error { return os.ErrNotExist }
	})
	if strings.Contains(CodeUpdateRejected, " ") {
		t.Fatal("codes are fixed identifiers")
	}
}

func TestCheckConfigOnce(t *testing.T) {
	cfg := SystemConfig{Project: "tp", Components: []Component{{Name: "backend", Group: "control"}}, CheckConfig: &CheckConfig{Service: "backend", Argv: []string{"/app/go", "--check-config"}}}
	mk := func(running bool, exit int) *Operator {
		return &Operator{Config: cfg, Manager: &fakeManager{fakeInspector: fakeInspector{states: map[string]ContainerState{"backend": {Running: running}}}, execCode: exit}}
	}
	if r := mk(true, 0).CheckConfigOnce(context.Background()); !r.OK || r.Skipped {
		t.Fatalf("%+v", r)
	}
	if r := mk(true, 1).CheckConfigOnce(context.Background()); r.OK || r.Service != "backend" || r.Code != CodeConfigInvalid {
		t.Fatalf("%+v", r)
	}
	if r := mk(false, 0).CheckConfigOnce(context.Background()); !r.OK || !r.Skipped || r.Code != CodeCheckSkipped {
		t.Fatalf("a stopped backend cannot be checked and must not block a restart: %+v", r)
	}
	if r := (&Operator{Config: SystemConfig{Project: "tp"}}).CheckConfigOnce(context.Background()); !r.OK || !r.Skipped {
		t.Fatalf("no check configured: %+v", r)
	}
}
