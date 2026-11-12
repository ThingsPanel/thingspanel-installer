//go:build linux

package agent

// The real steps of an upgrade (R5b): they use the Docker Engine API (stop/start/exec, image pull), a helper
// container running the compose CLI with fixed argv for "up", the backup/restore services of R3/R4 and the
// Go configuration transaction of R5a. Nothing is taken from a request except the target version, which must
// match the signed release and the staged Compose file.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultReleasePublisher = "https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml"

var composeServiceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// UpgradeConfig is the "upgrade" block of system.json.
type UpgradeConfig struct {
	Source       string   `json:"source"`                   // HTTPS base URL of the signed release payload (files and release-manifest.json)
	TrustRoot    string   `json:"trust_root"`               // PEM public key that must have signed the release; supplied out of band
	Publisher    string   `json:"publisher,omitempty"`      // expected release publisher; default: the installer's release workflow
	Revoked      []string `json:"revoked,omitempty"`        // versions withdrawn out of band
	CAFile       string   `json:"ca_file,omitempty"`        // extra CA certificates for Source (private mirrors)
	ComposeImage string   `json:"compose_image"`            // image providing `docker compose`, pulled before the upgrade
	HostRoot     string   `json:"host_root"`                // the stack directory as the Docker host sees it
	Services     []string `json:"services"`                 // compose services brought up, in order; only those that exist are touched
	StateFile    string   `json:"state_file"`               // upgrade state (see UpgradeState)
	VerifySecs   int      `json:"verify_seconds,omitempty"` // health-gate budget per gate, default 300
}

func (c *UpgradeConfig) validate(components map[string]bool) error {
	if !strings.HasPrefix(c.Source, "https://") || !filepath.IsAbs(c.TrustRoot) || !filepath.IsAbs(c.HostRoot) ||
		!filepath.IsAbs(c.StateFile) || c.ComposeImage == "" || strings.ContainsAny(c.ComposeImage, " \t\n") ||
		len(c.Services) == 0 || len(c.Services) > 64 || c.VerifySecs < 0 || c.VerifySecs > 3600 {
		return ErrSystemConfig
	}
	if c.CAFile != "" && !filepath.IsAbs(c.CAFile) {
		return ErrSystemConfig
	}
	for _, s := range c.Services {
		if !composeServiceName.MatchString(s) {
			return ErrSystemConfig
		}
	}
	return nil
}

// LiveUpgrade implements UpgradeSteps against a real stack.
type LiveUpgrade struct {
	Config   SystemConfig
	Manager  StackManager
	Images   ImageRegistry
	Helper   HelperRunner
	Backup   *BackupService
	Restore  *RestoreService
	Verifier *Verifier
	Operator *Operator
	Download Downloader
	Grace    time.Duration
}

func (l *LiveUpgrade) cfg() *UpgradeConfig { return l.Config.Upgrade }

func (l *LiveUpgrade) grace() time.Duration {
	if l.Grace > 0 {
		return l.Grace
	}
	return 30 * time.Second
}

func (l *LiveUpgrade) budget() time.Duration {
	if s := l.cfg().VerifySecs; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 5 * time.Minute
}

func (l *LiveUpgrade) control() []Component {
	var out []Component
	for _, c := range l.Config.Components {
		if c.Group == "control" {
			out = append(out, c)
		}
	}
	return out
}

func (l *LiveUpgrade) Current(ctx context.Context) (string, error) {
	v, _ := l.Backup.platformVersionLive(ctx)
	if v == "" {
		return "", errors.New("current version unknown")
	}
	return v, nil
}

func (l *LiveUpgrade) Preflight(ctx context.Context, to string) error {
	if err := l.Restore.ensureData(ctx, l.Backup.Project); err != nil {
		return err
	}
	if _, err := os.Stat(l.cfg().TrustRoot); err != nil {
		return errors.New("release trust root is missing")
	}
	return l.ensureImage(ctx, l.cfg().ComposeImage)
}

func (l *LiveUpgrade) ensureImage(ctx context.Context, ref string) error {
	have, err := l.Images.HaveImage(ctx, ref)
	if err != nil {
		return err
	}
	if have {
		return nil
	}
	return l.Images.Pull(ctx, ref)
}

func (l *LiveUpgrade) tx(hooks ApplyHooks) *ConfigTransaction {
	cfg := l.cfg()
	publisher := cfg.Publisher
	if publisher == "" {
		publisher = defaultReleasePublisher
	}
	t := &ConfigTransaction{Root: l.Config.Backup.Root, Download: l.Download, LockHeld: true,
		Release: &ReleasePolicy{TrustRoot: cfg.TrustRoot, Publisher: publisher, Revoked: cfg.Revoked}}
	t.Checkpoint = func(event string) error {
		switch event {
		case "validated":
			if hooks.AfterValidated != nil {
				return hooks.AfterValidated(t.StageDir())
			}
		case "prepared":
			if hooks.BeforeApply != nil {
				return hooks.BeforeApply(t.Operation())
			}
		}
		return nil
	}
	return t
}

func (l *LiveUpgrade) ApplyConfig(ctx context.Context, to string, hooks ApplyHooks) error {
	if hooks.AfterValidated == nil {
		hooks.AfterValidated = l.prePull
	}
	return l.tx(hooks).Run(strings.TrimRight(l.cfg().Source, "/"), to, nil, false)
}

// prePull fetches every image the staged Compose file names, before anything is stopped or changed, so a
// missing or unreachable image is found while the old version is still running.
func (l *LiveUpgrade) prePull(stageDir string) error {
	raw, err := os.ReadFile(filepath.Join(stageDir, "docker-compose.yml"))
	if err != nil {
		return err
	}
	env, _ := ReadEnvFile(filepath.Join(l.Config.Backup.Root, ".env"))
	images := ComposeServiceImages(string(raw), env)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	for _, svc := range l.upServices(ctx) {
		ref, ok := images[svc]
		if !ok {
			continue
		}
		if err := l.ensureImage(ctx, ref); err != nil {
			return fmt.Errorf("image for %s is not available", svc)
		}
	}
	return nil
}

func (l *LiveUpgrade) Snapshot(ctx context.Context, from string) (string, error) {
	return l.Backup.CreateWith(ctx, CreateOptions{Trigger: "pre-upgrade", Version: from}, nil)
}

func (l *LiveUpgrade) Maintenance(on bool) { l.Operator.maintenance(on) }

func (l *LiveUpgrade) StopControl(ctx context.Context) error {
	for _, c := range reverse(l.control()) {
		if err := l.Manager.Stop(ctx, l.Backup.Project, c.Name, l.grace()); err != nil && !errors.Is(err, ErrNoContainer) {
			return err
		}
	}
	return nil
}

func (l *LiveUpgrade) StartControl(ctx context.Context) error {
	for _, c := range l.control() {
		if err := l.Manager.Start(ctx, l.Backup.Project, c.Name); err != nil {
			return err
		}
	}
	return nil
}

func (l *LiveUpgrade) RecoverConfig(ctx context.Context) error {
	return l.tx(ApplyHooks{}).Run("", "", nil, true)
}

func (l *LiveUpgrade) RevertConfig(ctx context.Context, operation string) error {
	return l.tx(ApplyHooks{}).Revert(operation)
}

func (l *LiveUpgrade) RestoreSnapshot(ctx context.Context, name string) error {
	project := l.Backup.Project
	if err := l.Restore.ensureData(ctx, project); err != nil {
		return err
	}
	m, err := l.Backup.Verify(name)
	if err != nil {
		return err
	}
	return l.Restore.restoreContents(ctx, project, name, m, nil)
}

func (l *LiveUpgrade) Verify(ctx context.Context) error {
	if res := l.Verifier.Wait(ctx, l.budget()); !res.OK {
		return errors.New("health gate failed")
	}
	return nil
}

// upServices lists the configured compose services that currently exist as containers of the project, in the
// configured order. Anything not deployed (an optional service) is left alone.
func (l *LiveUpgrade) upServices(ctx context.Context) []string {
	states, err := l.Manager.Containers(ctx, l.Backup.Project)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range l.cfg().Services {
		if _, ok := states[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

// ComposeUp runs `docker compose up -d --no-deps --pull never <services>` in the helper container. Compose
// recreates exactly the services whose configuration or image changed and leaves the others running.
func (l *LiveUpgrade) ComposeUp(ctx context.Context) error {
	services := l.upServices(ctx)
	if len(services) == 0 {
		return errors.New("no services to start")
	}
	host := l.cfg().HostRoot
	argv := []string{"compose", "--project-name", l.Backup.Project, "--project-directory", host, "-f", filepath.Join(host, "docker-compose.yml")}
	if _, err := os.Stat(filepath.Join(l.Config.Backup.Root, "docker-compose.override.yml")); err == nil {
		argv = append(argv, "-f", filepath.Join(host, "docker-compose.override.yml"))
	}
	if _, err := os.Stat(filepath.Join(l.Config.Backup.Root, ".env")); err == nil {
		argv = append(argv, "--env-file", filepath.Join(host, ".env"))
	}
	argv = append(argv, "up", "-d", "--no-deps", "--pull", "never")
	argv = append(argv, services...)
	code, tail, err := l.Helper.RunHelper(ctx, HelperSpec{Image: l.cfg().ComposeImage, Entrypoint: []string{"docker"}, Cmd: argv, WorkingDir: host,
		Binds: []string{"/var/run/docker.sock:/var/run/docker.sock", host + ":" + host}})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("compose up failed (exit %d): %s", code, strings.TrimSpace(tail))
	}
	return nil
}
