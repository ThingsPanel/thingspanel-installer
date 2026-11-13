//go:build linux

package agent

import (
	"context"
	"errors"
	"testing"
)

type fakeRegistry struct {
	have      bool
	pullErr   error
	pulled    []string
	haveCalls int
}

func (f *fakeRegistry) HaveImage(context.Context, string) (bool, error) {
	f.haveCalls++
	return f.have, nil
}
func (f *fakeRegistry) Pull(_ context.Context, ref string) error {
	f.pulled = append(f.pulled, ref)
	return f.pullErr
}

// R9/B7: the helper image is checked before an upgrade does anything; a host that has neither the image nor a
// registry gets one precise code, and the abort path changes nothing.
func TestHelperImageCheck(t *testing.T) {
	cfg := SystemConfig{Upgrade: &UpgradeConfig{ComposeImage: "registry.example.test/tools/docker:27-cli"}}
	for _, c := range []struct {
		name   string
		reg    *fakeRegistry
		want   string
		pulled int
	}{
		{"present: no pull", &fakeRegistry{have: true}, "", 0},
		{"missing, pull works", &fakeRegistry{}, "", 1},
		{"missing, offline", &fakeRegistry{pullErr: errors.New("dial tcp: lookup registry: no such host")}, CodeUpgradeHelperImage, 1},
	} {
		l := &LiveUpgrade{Config: cfg, Images: c.reg}
		got := code(l.checkHelperImage(context.Background()))
		if got != c.want || len(c.reg.pulled) != c.pulled {
			t.Errorf("%s: code %q want %q, pulls %v", c.name, got, c.want, c.reg.pulled)
		}
	}
}

type helperMissingSteps struct{ fakeSteps }

func (h helperMissingSteps) Preflight(ctx context.Context, to string) error {
	_ = h.fakeSteps.Preflight(ctx, to)
	return CodedError{CodeUpgradeHelperImage}
}

func TestUpgradeAbortsCleanlyWhenHelperImageIsMissing(t *testing.T) {
	w := newWorld()
	u := newUpgrader(t, w)
	u.Steps = helperMissingSteps{fakeSteps{w}}
	if got := code(u.Run(context.Background(), "v1.4.0-beta.1", nil)); got != CodeUpgradeHelperImage {
		t.Fatalf("got %q", got)
	}
	assertOldSite(t, w)
	if len(w.snapshots) != 0 {
		t.Fatal("a refused upgrade must not take a snapshot")
	}
	for _, call := range w.calls {
		if call != "current" && call != "preflight" {
			t.Fatalf("nothing but the read-only checks may have run, got %v", w.calls)
		}
	}
	if s := state(t, u); s.Phase != UpAborted || s.Code != CodeUpgradeHelperImage {
		t.Fatalf("%+v", s)
	}
}

func TestProvisionedComposeImageIsConfigurable(t *testing.T) {
	for _, c := range []struct {
		image string
		ok    bool
	}{
		{"", true}, // default docker:27-cli
		{"registry.example.test:5000/tools/docker:27-cli", true},
		{"docker@sha256:" + "ab"[:2] + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd", true},
		{"docker 27", false}, {"docker:27;rm -rf /", false}, {"-flag", false}, {"a b", false},
	} {
		dir := t.TempDir() + "/system"
		up := &ProvisionUpgrade{Source: "https://updates.example.test/r", TrustRootCtr: "/run/agent/release-root.pem", HostRoot: "/opt/tp", ComposeImage: c.image}
		err := ProvisionSystem(dir, "tp", up)
		if (err == nil) != c.ok {
			t.Errorf("%q: err=%v want ok=%v", c.image, err, c.ok)
			continue
		}
		if c.ok {
			cfg, err := LoadSystemConfig(dir + "/system.json")
			want := c.image
			if want == "" {
				want = "docker:27-cli"
			}
			if err != nil || cfg.Upgrade.ComposeImage != want {
				t.Errorf("%q: %v %+v", c.image, err, cfg.Upgrade)
			}
		}
	}
}
