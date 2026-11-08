//go:build linux

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in only. No image pull or build occurs here: the operator must supply a
// reviewed existing image ID satisfying the create-only profile restrictions.
func realPreparationFixture(t *testing.T) (*Store, string, *UnixDockerEngine, PreparationProfile) {
	t.Helper()
	if os.Getenv("TP_AGENT_REAL_DOCKER") != "1" {
		t.Skip("TP_AGENT_REAL_DOCKER=1 plus explicit trusted socket/image ID required; not Docker acceptance")
	}
	imageID, socket := os.Getenv("TP_AGENT_DOCKER_IMAGE_ID"), os.Getenv("TP_AGENT_DOCKER_SOCKET")
	if !validImageID(imageID) || socket == "" {
		t.Fatal("an immutable reviewed image ID and explicit local socket are required")
	}
	engine, err := NewUnixDockerEngine(socket)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	host := "tp-takeover-b-installer-agent-" + hex.EncodeToString(random)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Initialize(path, host); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptGeneration("instance-a", 1); err != nil {
		t.Fatal(err)
	}
	profile := preparationFixtureProfile()
	profile.ImageID = imageID
	t.Cleanup(func() {
		// Test-only cleanup: never force removal, never remove a running,
		// unowned or altered container, and never infer ownership from a name.
		disk, err := load(path)
		if err != nil {
			t.Errorf("fixture ledger needs owner review before cleanup: %v", err)
			return
		}
		for _, record := range disk.Preparations {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			container, exists, err := engine.inspectContainer(ctx, record.ContainerName)
			if err != nil {
				cancel()
				t.Errorf("fixture inspect before cleanup: %v", err)
				continue
			}
			if !exists {
				cancel()
				continue
			}
			if disk.HostID != host || !safePreparedContainer(container, record, host) {
				cancel()
				t.Errorf("fixture container %s changed; owner review required, no cleanup attempted", record.ContainerName)
				continue
			}
			status, err := engine.call(ctx, http.MethodDelete, "/containers/"+container.ID+"?force=false&v=false", nil, nil)
			cancel()
			if err != nil || status != http.StatusNoContent {
				t.Errorf("fixture container cleanup status=%d error=%v", status, err)
			}
		}
	})
	return store, path, engine, profile
}

type preparationRoundTripFunc func(*http.Request) (*http.Response, error)

func (call preparationRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return call(request)
}

func TestPreparationRealDockerLostReplyAndRestart(t *testing.T) {
	store, path, engine, profile := realPreparationFixture(t)
	base := engine.client.Transport
	var createCalls atomic.Int64
	interrupted := &UnixDockerEngine{client: &http.Client{Transport: preparationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(request)
		if request.Method == http.MethodPost && request.URL.Path == dockerPreparationAPI+"/containers/create" {
			createCalls.Add(1)
			if err == nil && response.StatusCode == http.StatusCreated {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				return nil, errors.New("fixture dropped actual Docker create reply")
			}
		}
		return response, err
	}), Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	preparer, err := NewDockerPreparer(store, interrupted, []PreparationProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	request := PrepareContainerRequest{Instance: "instance-a", Generation: 1, RequestKey: "original-key", Profile: profile.Name}
	first, err := preparer.Prepare(context.Background(), request)
	if err != nil || first.State != PreparationPrepared || first.Active || first.ImageID != profile.ImageID || createCalls.Load() != 1 {
		t.Fatalf("real Docker lost response was not reconciled: %#v creates=%d error=%v", first, createCalls.Load(), err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDockerPreparer(reopened, interrupted, []PreparationProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.Prepare(context.Background(), request)
	if err != nil || first != second || createCalls.Load() != 1 {
		t.Fatalf("restart repeated Docker create or changed receipt: %#v creates=%d %v", second, createCalls.Load(), err)
	}
	container, exists, err := engine.inspectContainer(context.Background(), first.ContainerName)
	if err != nil || !exists || container.State.Status != "created" || container.State.Running || container.Image != profile.ImageID {
		t.Fatalf("prepared instance executed or changed: exists=%v state=%s running=%v error=%v", exists, container.State.Status, container.State.Running, err)
	}
	t.Logf("actual Docker immutable image=%s original container=%s state=created active=false", profile.ImageID, first.ContainerID)
}

// The proxy forwards to the real daemon, then withholds its successful create
// reply. It does not simulate the container. The child process's state remains
// an intent while a real container already exists; SIGKILL tests that boundary.
func realPreparationReplyProxy(t *testing.T, engine *UnixDockerEngine, entered chan<- struct{}, release <-chan struct{}, creates *atomic.Int64) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tp-agent-real-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isRead := r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, dockerPreparationAPI+"/images/") || strings.HasPrefix(r.URL.Path, dockerPreparationAPI+"/containers/")) && strings.HasSuffix(r.URL.Path, "/json")
		isCreate := r.Method == http.MethodPost && r.URL.Path == dockerPreparationAPI+"/containers/create"
		if !isRead && !isCreate {
			t.Errorf("unexpected real-daemon proxy action %s %s", r.Method, r.URL.Path)
			w.WriteHeader(403)
			return
		}
		forward := r.Clone(r.Context())
		forward.RequestURI = ""
		forward.URL.Scheme = "http"
		forward.URL.Host = "docker"
		response, err := engine.client.Do(forward)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		defer response.Body.Close()
		if isCreate {
			creates.Add(1)
			if response.StatusCode == http.StatusCreated {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(response.Body, 1024*1024+1))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestPreparationRealDockerTwoProcessesAndSIGKILL(t *testing.T) {
	for _, kill := range []bool{false, true} {
		name := "two-processes"
		if kill {
			name = "sigkill"
		}
		t.Run(name, func(t *testing.T) {
			_, state, engine, profile := realPreparationFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var creates atomic.Int64
			socket := realPreparationReplyProxy(t, engine, entered, release, &creates)
			firstPath := filepath.Join(t.TempDir(), "first.json")
			first, firstOutput := startPreparationChild(t, state, socket, firstPath, "prepare", profile.ImageID)
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				t.Fatal("real Docker create did not reach durable-intent/lost-reply boundary")
			}
			disk, err := load(state)
			if err != nil {
				t.Fatal(err)
			}
			if record := disk.Preparations["instance-a\x00original-key"]; record.State != "intent" {
				t.Fatal("receipt was committed before the withheld real reply")
			}
			secondPath := filepath.Join(t.TempDir(), "second.json")
			if kill {
				if err := first.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := first.Wait(); err == nil {
					t.Fatal("expected actual SIGKILL of preparing process")
				}
				close(release)
			} else {
				// Start another process while the original state lock spans the
				// real create. Both processes must converge to the same receipt.
				second, secondOutput := startPreparationChild(t, state, socket, secondPath, "prepare", profile.ImageID)
				close(release)
				if err := first.Wait(); err != nil {
					t.Fatalf("first process: %v %s", err, firstOutput.String())
				}
				if err := second.Wait(); err != nil {
					t.Fatalf("second process: %v %s", err, secondOutput.String())
				}
				if a, b := readPreparationChild(t, firstPath), readPreparationChild(t, secondPath); a != b {
					t.Fatalf("real two-process receipts differ: %#v %#v", a, b)
				}
			}
			if kill {
				restarted, restartedOutput := startPreparationChild(t, state, socket, secondPath, "prepare", profile.ImageID)
				if err := restarted.Wait(); err != nil {
					t.Fatalf("restarted process: %v %s", err, restartedOutput.String())
				}
			}
			result := readPreparationChild(t, secondPath)
			if result.State != PreparationPrepared || result.Active || result.ImageID != profile.ImageID || creates.Load() != 1 {
				t.Fatalf("real process recovery guessed or recreated: %#v creates=%d", result, creates.Load())
			}
			t.Logf("actual Docker image=%s container=%s create_calls=1 state=created active=false", profile.ImageID, result.ContainerID)
		})
	}
}
