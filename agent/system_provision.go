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

// ProvisionUpgrade is what an installation must supply to enable `system upgrade`.
type ProvisionUpgrade struct {
	Source       string // HTTPS base URL of the signed release
	TrustRootCtr string // release trust root, as the Agent container sees it
	CACtr        string // optional extra CA bundle for Source, as the Agent container sees it
	HostRoot     string // the stack directory on the Docker host
	ComposeImage string // default docker:27-cli
}

// ProductionSystemConfig is the system-role description of the reviewed docker-compose.yml.
func ProductionSystemConfig(project string, up *ProvisionUpgrade) SystemConfig {
	cfg := productionSystemConfig(project)
	if up != nil {
		image := up.ComposeImage
		if image == "" {
			image = "docker:27-cli"
		}
		cfg.Upgrade = &UpgradeConfig{Source: up.Source, TrustRoot: up.TrustRootCtr, CAFile: up.CACtr, ComposeImage: image, HostRoot: up.HostRoot,
			StateFile: "/run/agent/system/upgrade.json",
			// Only services that exist as containers are touched; optional ones are simply absent.
			Services: []string{"postgres", "redis", "backend", "encore-api", "notification-core", "automation-scheduler",
				"broker-owner", "mosquitto", "frontend", "thingsvis-server", "thingsvis-studio"}}
	}
	return cfg
}

func productionSystemConfig(project string) SystemConfig {
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
			URL: "http://encore-api:14000/internal/broker-scope", EnvFile: ContainerStackRoot + "/.env", TokenKey: "TP_BROKER_SCOPE_TOKEN",
			Root: ContainerStackRoot, OwnerService: "broker-owner", BrokerService: "mosquitto", IntervalSeconds: 20,
		},
	}
}

// ProvisionSystem writes DIR/system.json. DIR is created 0700 and holds only the Agent's own system state (the
// config and the upgrade state file); it is never mounted into a platform container.
func ProvisionSystem(dir, project string, up *ProvisionUpgrade) error {
	cfg := ProductionSystemConfig(project, up)
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
	return atomicWrite(filepath.Join(dir, "system.json"), append(raw, '\n'), 0o644, false)
}

// ProvisionSystemToken creates DIR/token (the system bearer token) when it is absent or too short, keeping a
// good one so a restart does not invalidate the credentials operators hold. DIR also receives the system
// socket, and it is the one directory the platform's Encore container mounts (read-only), so that DIR holds
// nothing but the socket and this token.
func ProvisionSystemToken(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
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
