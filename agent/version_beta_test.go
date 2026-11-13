//go:build linux

package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// R9/B8: vX.Y.Z-beta.N is a first-class version. The rules are in CompareVersions.
func TestCompareVersionsBeta(t *testing.T) {
	// each row: a < b must hold, and b > a, and a == a.
	ordered := [][2]string{
		{"v1.4.0-beta.1", "v1.4.0-beta.2"},
		{"v1.4.0-beta.2", "v1.4.0-beta.10"}, // numeric, not string, comparison
		{"v1.4.0-beta.2", "v1.4.0"},         // final is greater than any beta of the same number
		{"v1.4.0-beta.99", "v1.4.0"},
		{"v1.3.0", "v1.4.0-beta.1"}, // a beta of the next number is above the previous final
		{"v1.3.9", "v1.4.0-beta.1"},
		{"v1.4.0-beta.5", "v1.4.1-beta.1"},
		{"v1.4.0", "v1.4.1-beta.1"},
		{"v1.9.0-beta.1", "v1.10.0-beta.1"},
		{"v1.4.0-beta.1", "v1.4.0-beta.2+build.7"}, // build metadata is ignored
	}
	for _, c := range ordered {
		if cmp, ok := CompareVersions(c[0], c[1]); !ok || cmp != -1 {
			t.Errorf("Compare(%q,%q) = %d,%v want -1,true", c[0], c[1], cmp, ok)
		}
		if cmp, ok := CompareVersions(c[1], c[0]); !ok || cmp != 1 {
			t.Errorf("Compare(%q,%q) = %d,%v want 1,true", c[1], c[0], cmp, ok)
		}
	}
	for _, v := range []string{"v1.4.0-beta.1", "v1.4.0", "v1.4.0-beta.3", "1.4.0-beta.3"} {
		if cmp, ok := CompareVersions(v, v); !ok || cmp != 0 {
			t.Errorf("Compare(%q,%q) = %d,%v want 0,true", v, v, cmp, ok)
		}
	}
	if cmp, ok := CompareVersions("v1.4.0-beta.1", "v1.4.0-beta.1+other"); !ok || cmp != 0 {
		t.Errorf("build metadata must not change the order: %d,%v", cmp, ok)
	}
	// anything that is not vX.Y.Z[-beta.N] stays comparable only by equality
	for _, bad := range []string{"v1.4.0-beta", "v1.4.0-beta.", "v1.4.0-beta.x", "v1.4.0-beta.1.2", "v1.4.0-rc.1", "v1.4.0-dev", "v1.4.0-dirty", "v1.4.0-beta.1-x", "v1.4", "v1.4.0.1", "unknown", ""} {
		if cmp, ok := CompareVersions(bad, "v1.3.0"); ok {
			t.Errorf("Compare(%q, v1.3.0) = %d,true: an unparsed version must not be ordered", bad, cmp)
		}
		if cmp, ok := CompareVersions("v1.3.0", bad); ok {
			t.Errorf("Compare(v1.3.0, %q) = %d,true: an unparsed version must not be ordered", bad, cmp)
		}
	}
	if cmp, ok := CompareVersions("v1.4.0-rc.1", "v1.4.0-rc.1"); !ok || cmp != 0 {
		t.Errorf("an identical unparsed tag is still equal: %d,%v", cmp, ok)
	}
	if _, ok := CompareVersions("v1.4.0-beta.18446744073709551616", "v1.4.0"); ok {
		t.Error("an overflowing number must not be ordered")
	}
}

// Backups are refused when newer than the machine, for betas too; older (or the same) restores.
func TestRestoreCompatibilityWithBeta(t *testing.T) {
	m := func(v string) Manifest {
		return Manifest{PlatformVersion: v, Stores: map[string]StoreInfo{"relational": {Timescaledb: "2.8.1", SysVersion: 25}}}
	}
	for _, c := range []struct{ backup, machine, want string }{
		{"v1.4.0-beta.1", "v1.4.0-beta.1", "ok"},
		{"v1.4.0-beta.1", "v1.4.0-beta.2", "ok"}, // older beta restores onto a newer beta
		{"v1.4.0-beta.2", "v1.4.0-beta.1", CodeRestoreNewer},
		{"v1.4.0-beta.3", "v1.4.0", "ok"},             // beta backup onto the final machine of the same number
		{"v1.4.0", "v1.4.0-beta.3", CodeRestoreNewer}, // final backup onto a beta machine of the same number: refused
		{"v1.3.0", "v1.4.0-beta.1", "ok"},             // v1.3.0 backup onto a beta machine
		{"v1.4.0-beta.1", "v1.3.0", CodeRestoreNewer}, // beta backup onto the old final machine
		{"v1.4.0", "v1.4.1-beta.1", "ok"},
		{"v1.4.1-beta.1", "v1.4.0", CodeRestoreNewer},
		{"v1.4.0-rc.1", "v1.4.0", CodeRestoreVersion}, // unparsed tag: cannot be judged
	} {
		if got := code(CheckRestoreCompatible(m(c.backup), c.machine, 25, "2.8.1")); got != c.want && !(got == "" && c.want == "ok") {
			t.Errorf("backup %s onto machine %s: got %q want %q", c.backup, c.machine, got, c.want)
		}
	}
}

type currentIs struct {
	fakeSteps
	from string
}

func (c currentIs) Current(context.Context) (string, error) { return c.from, nil }

func TestUpgradePathsWithBeta(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"v1.3.0", "v1.4.0-beta.1", ""},        // old final -> first beta
		{"v1.4.0-beta.1", "v1.4.0-beta.2", ""}, // beta -> higher beta
		{"v1.4.0-beta.2", "v1.4.0", ""},        // beta -> final
		{"v1.4.0-beta.1", "v1.4.1", ""},        // beta -> next number's final
		{"v1.4.0-beta.2", "v1.4.0-beta.2", CodeUpgradeNotNewer},
		{"v1.4.0-beta.2", "v1.4.0-beta.1", CodeUpgradeNotNewer}, // no downgrade between betas
		{"v1.4.0", "v1.4.0-beta.9", CodeUpgradeNotNewer},        // final -> beta of the same number is a downgrade
		{"v1.4.0", "v1.3.9", CodeUpgradeNotNewer},
		{"v1.4.0-beta.1", "v1.4.0-rc.1", CodeUpgradeBadVersion}, // not a version the Agent can order
		{"v1.4.0-rc.1", "v1.4.0", CodeUpgradeBadVersion},        // running an unparsed tag: cannot judge
	} {
		w := newWorld()
		dir := t.TempDir()
		u := &Upgrader{Steps: currentIs{fakeSteps{w}, c.from}, StateFile: filepath.Join(dir, "upgrade.json"), LockDir: filepath.Join(dir, "lock"), Now: time.Now}
		got := code(u.Run(context.Background(), c.to, nil))
		if got != c.want {
			t.Errorf("%s -> %s: got %q want %q", c.from, c.to, got, c.want)
		}
		if c.want != "" && len(w.snapshots) != 0 {
			t.Errorf("%s -> %s: a refused upgrade must not take a snapshot", c.from, c.to)
		}
	}
}

func TestUpdateCheckWithBeta(t *testing.T) {
	s := newSigner(t)
	for _, c := range []struct {
		latest, current string
		newer           bool
	}{
		{"v1.4.0-beta.2", "v1.4.0-beta.1", true},
		{"v1.4.0-beta.1", "v1.4.0-beta.2", false},
		{"v1.4.0-beta.2", "v1.4.0-beta.2", false},
		{"v1.4.0", "v1.4.0-beta.2", true},
		{"v1.4.0-beta.2", "v1.4.0", false},
		{"v1.4.0-beta.1", "v1.3.0", true},
	} {
		manifest, sig := s.manifest(t, newPayload(c.latest), manifestOpts{Version: c.latest})
		info, err := updateChecker(t, s, manifest, sig, c.current, time.Now()).Check(context.Background())
		if err != nil || info.Newer != c.newer || info.Latest != c.latest || info.Current != c.current {
			t.Errorf("latest %s current %s: %+v %v", c.latest, c.current, info, err)
		}
	}
}
