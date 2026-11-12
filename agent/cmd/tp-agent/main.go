//go:build linux

// tp-agent is the host Agent service: prepare, grant, activate, revoke and retire plugin containers
// for instances whose generation the control plane has accepted. It listens on a private Unix socket only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agent "thingspanel-installer-agent-state"
)

func main() {
	statePath := flag.String("state", "", "private state file (created with -init)")
	initState := flag.String("init", "", "initialize the state file with this host id and exit")
	socket := flag.String("socket", "", "Unix socket to serve the control API on")
	tokenFile := flag.String("token-file", "", "file holding the control-plane bearer token (mode 0600, >= 32 bytes)")
	dockerSocket := flag.String("docker-socket", "/var/run/docker.sock", "local Docker Engine socket")
	profilesFile := flag.String("profiles", "", "JSON array of reviewed preparation profiles")
	catalogFile := flag.String("catalog", "", "signed plugin catalog (plugin-catalog.json, with .sig beside it): enables the catalog install API")
	trustRoot := flag.String("trust-root", "", "ECDSA P-256 public key (PEM) that must have signed the catalog")
	secretsFile := flag.String("secrets", "", "secret store file (mode 0600); default: secrets.json next to the state file")
	stackEnv := flag.String("stack-env", "", "the stack's .env file; catalog env_from_stack values are read from it")
	network := flag.String("network", "", "existing user-defined Docker network the catalog plugins join")
	systemSocket := flag.String("system-socket", "", "system role: Unix socket for the system API (separate from -socket; never mount it into the platform backend)")
	systemTokenFile := flag.String("system-token-file", "", "system role: bearer token file (0600, >= 32 bytes, must differ from -token-file)")
	systemConfigFile := flag.String("system-config", "", "system role: fixed stack description (compose project and components), written by the installer")
	provision := flag.String("provision-system", "", "write the reviewed system-role config (system.json) and, once, a system token into this directory, then exit; needs -project")
	project := flag.String("project", "", "compose project name (with -provision-system)")
	sweep := flag.Duration("sweep-interval", 5*time.Second, "lease/generation sweep cadence")
	flag.Parse()
	if *initState != "" {
		if *statePath == "" {
			log.Fatal("-state is required with -init")
		}
		if err := agent.Initialize(*statePath, *initState); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *provision != "" {
		if err := agent.ProvisionSystem(*provision, *project); err != nil {
			log.Fatalf("provision system role: %v", err)
		}
		return
	}
	if *statePath == "" || *socket == "" || *tokenFile == "" || (*profilesFile == "" && *catalogFile == "") {
		log.Fatal("-state, -socket, -token-file and one of -profiles or -catalog are required")
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	var profiles []agent.PreparationProfile
	if *profilesFile != "" {
		raw, err := os.ReadFile(*profilesFile)
		if err != nil || json.Unmarshal(raw, &profiles) != nil {
			log.Fatal("profiles file is missing or invalid")
		}
	}
	store, err := agent.Open(*statePath)
	if err != nil {
		log.Fatal(err)
	}
	engine, err := agent.NewUnixDockerEngine(*dockerSocket)
	if err != nil {
		log.Fatal(err)
	}
	preparer, err := agent.NewDockerPreparer(store, engine, profiles)
	if err != nil {
		log.Fatal(err)
	}
	activator, err := agent.NewActivator(store, preparer, engine)
	if err != nil {
		log.Fatal(err)
	}
	service, err := agent.NewService(store, preparer, activator, token)
	if err != nil {
		log.Fatal(err)
	}
	if *catalogFile != "" {
		if *trustRoot == "" || *network == "" {
			log.Fatal("-catalog needs -trust-root and -network")
		}
		source, err := agent.NewCatalogSource(*catalogFile, *trustRoot)
		if err != nil {
			log.Fatalf("catalog trust root: %v", err)
		}
		secretsPath := *secretsFile
		if secretsPath == "" {
			secretsPath = filepath.Join(filepath.Dir(*statePath), "secrets.json")
		}
		secrets, err := agent.OpenSecretStore(secretsPath)
		if err != nil {
			log.Fatalf("secret store: %v", err)
		}
		builder := agent.ProfileBuilder{Secrets: secrets, Network: *network, StackEnv: func() (map[string]string, error) {
			if *stackEnv == "" {
				return nil, fmt.Errorf("-stack-env is not configured")
			}
			return agent.ReadEnvFile(*stackEnv)
		}}
		service.EnableCatalog(source, builder)
		if entries, err := source.Entries(); err == nil {
			vouched := map[string]agent.CatalogEntry{}
			for _, e := range entries {
				vouched[e.ID] = e
			}
			_ = preparer.AdoptRecorded(func(p agent.PreparationProfile) bool {
				id, _, _ := strings.Cut(p.Name, ".g")
				e, ok := vouched[id]
				return ok && e.ImageID == p.ImageID && e.Entrypoint == p.Entrypoint
			})
		} else {
			log.Printf("catalog is not usable yet: %v", err)
		}
	}
	listener, err := listenPrivate(*socket)
	if err != nil {
		log.Fatal(err)
	}
	var systemServer *http.Server
	var scopeSyncer *agent.ScopeSyncer
	if *systemSocket != "" || *systemTokenFile != "" || *systemConfigFile != "" {
		if *systemSocket == "" || *systemTokenFile == "" || *systemConfigFile == "" {
			log.Fatal("-system-socket, -system-token-file and -system-config must be given together")
		}
		if *systemSocket == *socket || *systemTokenFile == *tokenFile {
			log.Fatal("the system role needs its own socket and token file")
		}
		systemToken, err := readToken(*systemTokenFile)
		if err != nil {
			log.Fatalf("system token: %v", err)
		}
		if err := agent.EnsureDistinctTokens(token, systemToken); err != nil {
			log.Fatal(err)
		}
		systemCfg, err := agent.LoadSystemConfig(*systemConfigFile)
		if err != nil {
			log.Fatal(err)
		}
		inspector, err := agent.NewDockerStackInspector(*dockerSocket)
		if err != nil {
			log.Fatal(err)
		}
		verifier := &agent.Verifier{Config: systemCfg, Inspector: inspector, Prober: agent.NetProber{}}
		systemService, err := agent.NewSystemService(systemToken, verifier, agentVersion)
		if err != nil {
			log.Fatal(err)
		}
		operator := &agent.Operator{Config: systemCfg, Manager: inspector, Verifier: verifier}
		systemService.EnableOperations(operator)
		if systemCfg.Backup != nil {
			backupService := &agent.BackupService{
				Config: *systemCfg.Backup, Project: systemCfg.Project, Version: agentVersion, Runner: inspector,
				Adapter: &agent.PostgresAdapter{Runner: inspector, Project: systemCfg.Project, Service: systemCfg.Backup.PostgresService, User: systemCfg.Backup.PostgresUser},
				Plugins: func() (json.RawMessage, error) { return agent.ExportPlugins(store) },
			}
			systemService.EnableBackup(backupService)
			systemService.EnableRestore(&agent.RestoreService{Backup: backupService, Operator: operator, Manager: inspector, Verifier: verifier})
		}
		if systemCfg.ScopeSync != nil {
			syncer := &agent.ScopeSyncer{Config: *systemCfg.ScopeSync, Project: systemCfg.Project, Manager: inspector, Op: operator}
			systemService.EnableScopeStatus(syncer)
			scopeSyncer = syncer
		}
		systemListener, err := listenPrivate(*systemSocket)
		if err != nil {
			log.Fatal(err)
		}
		systemServer = &http.Server{Handler: systemService.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := systemServer.Serve(systemListener); err != nil && err != http.ErrServerClosed {
				log.Printf("system role: %v", err)
			}
		}()
		log.Printf("tp-agent system role listening on %s", *systemSocket)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if scopeSyncer != nil {
		go scopeSyncer.Run(ctx, func(r agent.ScopeResult) { log.Printf("broker scope sync: %s %s", r.Outcome, r.Code) })
	}
	go service.RunSweeper(ctx, *sweep, func(err error) { log.Printf("sweep: %v", err) })
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		if systemServer != nil {
			_ = systemServer.Shutdown(shutdown)
		}
	}()
	log.Printf("tp-agent listening on %s", *socket)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// agentVersion is set at build time: -ldflags "-X main.agentVersion=v0.3.0".
var agentVersion = "dev"

// listenPrivate serves a Unix socket readable only by its owner, replacing a stale socket of a previous run.
func listenPrivate(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	oldMask := syscall.Umask(0177)
	defer syscall.Umask(oldMask)
	return net.Listen("unix", path)
}

func readToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("token file must be a regular file with mode 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		return "", fmt.Errorf("token must be at least 32 characters")
	}
	return token, nil
}
