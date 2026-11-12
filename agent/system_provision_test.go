//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionSystemWritesLoadableConfigAndStableToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "system")
	if err := ProvisionSystem(dir, "thingspanel"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSystemConfig(filepath.Join(dir, "system.json"))
	if err != nil {
		t.Fatalf("the Agent cannot load what the installer provisions: %v", err)
	}
	if cfg.Project != "thingspanel" || cfg.Backup == nil || cfg.ScopeSync == nil || cfg.CheckConfig == nil {
		t.Fatalf("incomplete config: %+v", cfg)
	}
	// Start order matters: data, control, then broker-owner before mosquitto (shared network namespace).
	order := map[string]int{}
	for i, c := range cfg.Components {
		order[c.Name] = i
	}
	if !(order["postgres"] < order["backend"] && order["broker-owner"] < order["mosquitto"]) {
		t.Fatalf("bad start order %v", order)
	}
	info, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() < 32 {
		t.Fatalf("token: %v %v", info, err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
	first, _ := os.ReadFile(filepath.Join(dir, "token"))
	if err := ProvisionSystem(dir, "thingspanel"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "token"))
	if string(first) != string(second) {
		t.Fatal("an existing system token must survive re-provisioning")
	}
	// A too-short token is replaced rather than trusted.
	os.WriteFile(filepath.Join(dir, "token"), []byte("short"), 0o600)
	if err := ProvisionSystem(dir, "thingspanel"); err != nil {
		t.Fatal(err)
	}
	if third, _ := os.ReadFile(filepath.Join(dir, "token")); len(third) < 32 {
		t.Fatal("weak token kept")
	}
}

func TestProvisionSystemBacksUpNoSecrets(t *testing.T) {
	// The production backup allowlist is checked by BackupConfig.validate, which refuses secret-looking names.
	cfg := ProductionSystemConfig("tp")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, f := range cfg.Backup.ConfigFiles {
		if secretNames.MatchString(f) {
			t.Errorf("secret-looking file in the backup allowlist: %s", f)
		}
	}
}

func TestProvisionSystemRejectsBadProject(t *testing.T) {
	if err := ProvisionSystem(filepath.Join(t.TempDir(), "s"), "Bad Name; rm"); err == nil {
		t.Fatal("invalid project accepted")
	}
}
