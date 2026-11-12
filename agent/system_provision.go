//go:build linux

package agent

// Production description of the reviewed Compose stack for the system role (R5a). The installer starts the Agent
// once with `-provision-system DIR`; this writes DIR/system.json (what the Agent may restart, back up and
// restore, in start order) and DIR/token (the system bearer token, generated once, 0600). Everything is a fixed
// value of the reviewed docker-compose.yml, so the config is produced here and covered by tests instead of being
// assembled in installer scripts.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// Paths as the Agent container sees them (agent_host.py mounts the host directories there).
const (
	ContainerStackRoot = "/run/agent/stack"
	ContainerBackupDir = "/run/agent/backups"
	ContainerStatusDir = "/run/agent/status"
)

// ProductionSystemConfig is the system-role description of the reviewed docker-compose.yml.
func ProductionSystemConfig(project string) SystemConfig {
	return SystemConfig{
		Project: project,
		Components: []Component{
			{Name: "postgres", Group: "data"},
			{Name: "redis", Group: "data"},
			{Name: "backend", Group: "control", Probe: "http", URL: "http://backend:9999/health"},
			{Name: "encore-api", Group: "control", Probe: "http", URL: "http://encore-api:14000/__encore/healthz"},
			{Name: "notification-core", Group: "control", Probe: "http", URL: "http://notification-core:18091/__encore/healthz"},
			{Name: "automation-scheduler", Group: "control"},
			{Name: "broker-owner", Group: "broker"},
			{Name: "mosquitto", Group: "broker", Probe: "tcp", Addr: "mosquitto:1883"},
		},
		CheckConfig: &CheckConfig{Service: "backend", Argv: []string{"/go/src/app/ThingsPanel-Go", "--check-config"}},
		StatusDir:   ContainerStatusDir,
		Backup: &BackupConfig{
			Dir: ContainerBackupDir, Root: ContainerStackRoot,
			PostgresService: "postgres", PostgresUser: "postgres",
			FilesService: "backend", FilesPath: "/go/src/app/files",
			// Non-secret files only: .env, broker tokens and services.json are never backed up.
			ConfigFiles: []string{"docker-compose.yml", "niginx.conf", "config/native/runtime.encore.json",
				"config/native/runtime.notification.json", "config/native/prepare.py", "config/native/tick.sh"},
			VersionService: "backend", VersionArgv: []string{"/go/src/app/ThingsPanel-Go", "--version"},
			KeepAuto: 3,
		},
		ScopeSync: &ScopeSyncConfig{
			URL: "http://backend:9999/internal/broker-scope", EnvFile: ContainerStackRoot + "/.env", TokenKey: "TP_BROKER_SCOPE_TOKEN",
			Root: ContainerStackRoot, OwnerService: "broker-owner", BrokerService: "mosquitto", IntervalSeconds: 20,
		},
	}
}

// ProvisionSystem writes DIR/system.json and, when absent, DIR/token. DIR is created 0700. An existing token is
// kept so a restart of the Agent does not invalidate the credentials operators hold.
func ProvisionSystem(dir, project string) error {
	cfg := ProductionSystemConfig(project)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, "system.json"), append(raw, '\n'), 0o644, false); err != nil {
		return err
	}
	tokenPath := filepath.Join(dir, "token")
	if info, err := os.Lstat(tokenPath); err == nil && info.Mode().IsRegular() && info.Size() >= 32 {
		return os.Chmod(tokenPath, 0o600)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return atomicWrite(tokenPath, []byte(hex.EncodeToString(b)), 0o600, false)
}
