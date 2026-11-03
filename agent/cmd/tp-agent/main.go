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
	if *statePath == "" || *socket == "" || *tokenFile == "" || *profilesFile == "" {
		log.Fatal("-state, -socket, -token-file and -profiles are required")
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	var profiles []agent.PreparationProfile
	raw, err := os.ReadFile(*profilesFile)
	if err != nil || json.Unmarshal(raw, &profiles) != nil {
		log.Fatal("profiles file is missing or invalid")
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
	if info, err := os.Lstat(*socket); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(*socket) // stale socket of a previous run of this Agent
	}
	oldMask := syscall.Umask(0177)
	listener, err := net.Listen("unix", *socket)
	syscall.Umask(oldMask)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go service.RunSweeper(ctx, *sweep, func(err error) { log.Printf("sweep: %v", err) })
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("tp-agent listening on %s", *socket)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
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
