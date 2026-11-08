//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This is a wire-protocol/process fixture, not a real Docker daemon. Separate
// opt-in real Docker tests must pass before recording runtime preparation proof.
func preparationUnixFixture(t *testing.T, engine *preparationFixtureEngine, beforeCreate, afterCreate func(context.Context) bool) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tp-agent-socket-")
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
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, dockerPreparationAPI+"/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			image, err := engine.inspectImage(r.Context(), preparationFixtureProfile().ImageID)
			if err != nil {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(image)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, dockerPreparationAPI+"/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, dockerPreparationAPI+"/containers/"), "/json")
			container, exists, err := engine.inspectContainer(r.Context(), name)
			if err != nil {
				w.WriteHeader(503)
				return
			}
			if !exists {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(container)
		case r.Method == http.MethodPost && r.URL.Path == dockerPreparationAPI+"/containers/create":
			var request dockerCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if request.Image != preparationFixtureProfile().ImageID || request.HostConfig.NetworkMode != "none" || len(request.HostConfig.Binds) != 0 || len(request.HostConfig.Mounts) != 0 || len(request.HostConfig.Devices) != 0 || len(request.HostConfig.PortBindings) != 0 || !request.HostConfig.ReadonlyRootfs || request.HostConfig.Privileged || !equalStrings(request.Env, []string{preparationPath}) {
				t.Error("transport changed bounded create configuration")
				w.WriteHeader(400)
				return
			}
			if beforeCreate != nil && !beforeCreate(r.Context()) {
				return
			}
			id, err := engine.createContainer(r.Context(), r.URL.Query().Get("name"), request)
			if err != nil {
				w.WriteHeader(503)
				return
			}
			if afterCreate != nil && !afterCreate(r.Context()) {
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		default:
			t.Errorf("forbidden/unexpected Docker method: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestPreparationUnixTransportCreatesWithoutStarting(t *testing.T) {
	store, _, _, fixture, request := preparedFixture(t)
	socket := preparationUnixFixture(t, fixture, nil, nil)
	engine, err := NewUnixDockerEngine(socket)
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := NewDockerPreparer(store, engine, []PreparationProfile{preparationFixtureProfile()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := preparer.Prepare(context.Background(), request)
	if err != nil || first.State != PreparationPrepared || first.Active || fixture.createCount() != 1 {
		t.Fatalf("wire preparation: %#v %v", first, err)
	}
	second, err := preparer.Query(context.Background(), request.Instance, 1, request.RequestKey)
	if err != nil || second != first || fixture.createCount() != 1 {
		t.Fatalf("wire original query: %#v %v", second, err)
	}
	if _, err := NewUnixDockerEngine("tcp://untrusted:2375"); err == nil {
		t.Fatal("non-local endpoint accepted")
	}
}

type preparationChildResult struct {
	Observation PreparationObservation
	Error       string
}

func TestPreparationProcessChild(t *testing.T) {
	if os.Getenv("TP_AGENT_PREPARATION_CHILD") != "1" {
		return
	}
	store, err := Open(os.Getenv("TP_AGENT_PREPARATION_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewUnixDockerEngine(os.Getenv("TP_AGENT_PREPARATION_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	profile := preparationFixtureProfile()
	if image := os.Getenv("TP_AGENT_PREPARATION_IMAGE"); image != "" {
		profile.ImageID = image
	}
	preparer, err := NewDockerPreparer(store, engine, []PreparationProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request := PrepareContainerRequest{Instance: "instance-a", Generation: 1, RequestKey: "original-key", Profile: profile.Name}
	var result PreparationObservation
	if os.Getenv("TP_AGENT_PREPARATION_MODE") == "query" {
		result, err = preparer.Query(ctx, request.Instance, 1, request.RequestKey)
	} else {
		result, err = preparer.Prepare(ctx, request)
	}
	output := preparationChildResult{Observation: result}
	if err != nil {
		output.Error = err.Error()
	}
	raw, _ := json.Marshal(output)
	if err := os.WriteFile(os.Getenv("TP_AGENT_PREPARATION_RESULT"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func startPreparationChild(t *testing.T, state, socket, result, mode, image string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPreparationProcessChild$", "-test.v")
	command.Env = append(os.Environ(), "TP_AGENT_PREPARATION_CHILD=1", "TP_AGENT_PREPARATION_STATE="+state, "TP_AGENT_PREPARATION_SOCKET="+socket, "TP_AGENT_PREPARATION_RESULT="+result, "TP_AGENT_PREPARATION_MODE="+mode, "TP_AGENT_PREPARATION_IMAGE="+image)
	output := new(bytes.Buffer)
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return command, output
}

func readPreparationChild(t *testing.T, path string) PreparationObservation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result preparationChildResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "" {
		t.Fatalf("child error: %s", result.Error)
	}
	return result.Observation
}

func TestPreparationTwoProcessesReuseOneDurableOperation(t *testing.T) {
	_, state, _, fixture, _ := preparedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	socket := preparationUnixFixture(t, fixture, nil, func(ctx context.Context) bool {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	})
	firstPath, secondPath := filepath.Join(t.TempDir(), "first.json"), filepath.Join(t.TempDir(), "second.json")
	first, firstOutput := startPreparationChild(t, state, socket, firstPath, "prepare", "")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first process did not reach create")
	}
	second, secondOutput := startPreparationChild(t, state, socket, secondPath, "prepare", "")
	close(release)
	if err := first.Wait(); err != nil {
		t.Fatalf("first process: %v %s", err, firstOutput.String())
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second process: %v %s", err, secondOutput.String())
	}
	a, b := readPreparationChild(t, firstPath), readPreparationChild(t, secondPath)
	fixture.mu.Lock()
	creates := fixture.creates
	fixture.mu.Unlock()
	if a.State != PreparationPrepared || a.Active || a != b || creates != 1 {
		t.Fatalf("two-process receipt diverged: %#v %#v creates=%d", a, b, creates)
	}
}

func TestPreparationSIGKILLRecoversOriginalIntentWithoutRecreate(t *testing.T) {
	for _, afterSideEffect := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-side-effect-%t", afterSideEffect), func(t *testing.T) {
			_, state, _, fixture, request := preparedFixture(t)
			entered := make(chan struct{})
			var once sync.Once
			var createRequests atomic.Int64
			block := func(ctx context.Context) bool {
				createRequests.Add(1)
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return false
			}
			var before, after func(context.Context) bool
			if afterSideEffect {
				after = block
			} else {
				before = block
			}
			socket := preparationUnixFixture(t, fixture, before, after)
			killed, killedOutput := startPreparationChild(t, state, socket, filepath.Join(t.TempDir(), "killed.json"), "prepare", "")
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("child did not reach durable create boundary")
			}
			disk, err := load(state)
			if err != nil {
				t.Fatal(err)
			}
			intent := disk.Preparations[request.Instance+"\x00"+request.RequestKey]
			if intent.State != "intent" || disk.Requests[request.Instance+"\x00"+request.RequestKey].State != "held" {
				t.Fatal("Docker create preceded durable original intent")
			}
			if err := killed.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := killed.Wait(); err == nil {
				t.Fatalf("child unexpectedly succeeded: %s", killedOutput.String())
			}
			resultPath := filepath.Join(t.TempDir(), "restarted.json")
			restarted, restartedOutput := startPreparationChild(t, state, socket, resultPath, "prepare", "")
			if err := restarted.Wait(); err != nil {
				t.Fatalf("restart: %v %s", err, restartedOutput.String())
			}
			result := readPreparationChild(t, resultPath)
			want := PreparationUnknown
			if afterSideEffect {
				want = PreparationPrepared
			}
			if result.State != want || result.Active || createRequests.Load() != 1 {
				t.Fatalf("SIGKILL recovery recreated/guessed outcome: %#v create requests=%d", result, createRequests.Load())
			}
		})
	}
}
