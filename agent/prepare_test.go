//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type preparationFixtureEngine struct {
	mu          sync.Mutex
	image       dockerImage
	containers  map[string]dockerContainer
	creates     int
	createError error
	omitCreate  bool
	onCreate    func()
	onAbsent    func()
}

func (engine *preparationFixtureEngine) createCount() int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.creates
}

func preparationFixtureProfile() PreparationProfile {
	return PreparationProfile{Name: "reviewed-fixture", ImageID: "sha256:" + strings.Repeat("a", 64), Entrypoint: "/bin/true", Args: []string{}}
}

func newPreparationFixtureEngine() *preparationFixtureEngine {
	p := preparationFixtureProfile()
	return &preparationFixtureEngine{image: dockerImage{ID: p.ImageID, OS: "linux", Config: dockerConfig{Env: []string{preparationPath}}}, containers: map[string]dockerContainer{}}
}

func (engine *preparationFixtureEngine) inspectImage(context.Context, string) (dockerImage, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.image, nil
}
func (engine *preparationFixtureEngine) inspectContainer(_ context.Context, name string) (dockerContainer, bool, error) {
	engine.mu.Lock()
	value, exists := engine.containers[name]
	hook := engine.onAbsent
	engine.mu.Unlock()
	if !exists && hook != nil {
		hook()
	}
	return value, exists, nil
}
func (engine *preparationFixtureEngine) createContainer(_ context.Context, name string, request dockerCreateRequest) (string, error) {
	engine.mu.Lock()
	engine.creates++
	id := preparationHash("container:" + name)
	if !engine.omitCreate {
		container := dockerContainer{ID: id, Name: "/" + name, Image: request.Image, Config: request.dockerConfig, HostConfig: request.HostConfig}
		container.State.Status = "created"
		container.State.StartedAt = "0001-01-01T00:00:00Z"
		engine.containers[name] = container
	}
	hook, resultErr := engine.onCreate, engine.createError
	engine.mu.Unlock()
	if hook != nil {
		hook()
	}
	return id, resultErr
}

func preparedFixture(t *testing.T) (*Store, string, *DockerPreparer, *preparationFixtureEngine, PrepareContainerRequest) {
	t.Helper()
	store, path := initialized(t)
	request := PrepareContainerRequest{Instance: "instance-a", Generation: 1, RequestKey: "original-key", Profile: preparationFixtureProfile().Name}
	if err := store.AcceptGeneration(request.Instance, request.Generation); err != nil {
		t.Fatal(err)
	}
	engine := newPreparationFixtureEngine()
	preparer, err := newDockerPreparer(store, engine, []PreparationProfile{preparationFixtureProfile()})
	if err != nil {
		t.Fatal(err)
	}
	return store, path, preparer, engine, request
}

func TestPreparationOriginalKeyAndIndependentInstances(t *testing.T) {
	store, path, preparer, engine, request := preparedFixture(t)
	first, err := preparer.Prepare(context.Background(), request)
	if err != nil || first.State != PreparationPrepared || first.Active || !validHexID(first.ContainerID) || engine.createCount() != 1 {
		t.Fatalf("prepare: %#v %v", first, err)
	}
	host, err := store.HostID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release(request.Instance, 1, preparationResource(host, request.Instance)); !errors.Is(err, ErrPreparationRecoveryRequired) {
		t.Fatalf("generic release discarded prepared ownership: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newDockerPreparer(reopened, engine, []PreparationProfile{preparationFixtureProfile()})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []bool{false, true} {
		var observed PreparationObservation
		if query {
			observed, err = restarted.Query(context.Background(), request.Instance, request.Generation, request.RequestKey)
		} else {
			observed, err = restarted.Prepare(context.Background(), request)
		}
		if err != nil || observed != first || engine.createCount() != 1 {
			t.Fatalf("replay changed receipt/created again: %#v %v", observed, err)
		}
	}
	changed := preparationFixtureProfile()
	changed.Args = []string{"changed"}
	different, err := newDockerPreparer(reopened, engine, []PreparationProfile{changed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := different.Prepare(context.Background(), request); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("same key changed payload: %v", err)
	}
	request.Instance = "instance-b"
	if err := store.AcceptGeneration(request.Instance, 1); err != nil {
		t.Fatal(err)
	}
	second, err := preparer.Prepare(context.Background(), request)
	if err != nil || second.State != PreparationPrepared || second.Active || second.ContainerID == first.ContainerID || second.ContainerName == first.ContainerName || engine.createCount() != 2 {
		t.Fatalf("instance identity collapsed: %#v %v", second, err)
	}
}

func TestPreparationUnknownNeverBlindlyCreatesAgain(t *testing.T) {
	store, path, preparer, engine, request := preparedFixture(t)
	engine.createError, engine.omitCreate = errors.New("lost response"), true
	first, err := preparer.Prepare(context.Background(), request)
	if err != nil || first.State != PreparationUnknown || first.Active || first.ErrorCode != "CONTAINER_NOT_OBSERVED_AFTER_INTENT" || engine.createCount() != 1 {
		t.Fatalf("unknown create: %#v %v", first, err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newDockerPreparer(reopened, engine, []PreparationProfile{preparationFixtureProfile()})
	if err != nil {
		t.Fatal(err)
	}
	engine.createError, engine.omitCreate = nil, false
	for i := 0; i < 3; i++ {
		observed, err := restarted.Prepare(context.Background(), request)
		if err != nil || observed.State != PreparationUnknown || engine.createCount() != 1 {
			t.Fatalf("unknown original intent retried create: %#v creates=%d %v", observed, engine.createCount(), err)
		}
	}
	changedKey := request
	changedKey.RequestKey = "another-key-for-same-instance"
	if _, err := restarted.Prepare(context.Background(), changedKey); !errors.Is(err, ErrResourceConflict) || engine.createCount() != 1 {
		t.Fatalf("new key bypassed unresolved instance ownership: %v", err)
	}
	if err := store.AcceptGeneration(request.Instance, 2); !errors.Is(err, ErrPreparationRecoveryRequired) {
		t.Fatalf("generation advanced while dispatched create outcome remained unknown: %v", err)
	}
	_, generations, _, err := store.Snapshot()
	if err != nil || generations[request.Instance] != 1 {
		t.Fatalf("refused generation was persisted: %#v %v", generations, err)
	}
	if missing, err := restarted.Query(context.Background(), request.Instance, 1, "never-submitted"); err != nil || missing.State != PreparationNotObserved || missing.Active {
		t.Fatalf("original-key absence: %#v %v", missing, err)
	}
}

func TestPreparationLostReplyUsesInspectAndUnownedContainerIsNotAdopted(t *testing.T) {
	_, _, preparer, engine, request := preparedFixture(t)
	engine.createError = errors.New("created but reply lost")
	first, err := preparer.Prepare(context.Background(), request)
	if err != nil || first.State != PreparationPrepared || engine.createCount() != 1 {
		t.Fatalf("inspect did not recover actual original container: %#v %v", first, err)
	}
	store, path := initialized(t)
	if err := store.AcceptGeneration(request.Instance, 1); err != nil {
		t.Fatal(err)
	}
	other, err := newDockerPreparer(store, engine, []PreparationProfile{preparationFixtureProfile()})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture has the same reviewed host ID but a distinct empty ledger.
	// Even an exactly labelled prior container is not adopted after lost state.
	if _, err := other.Prepare(context.Background(), request); !errors.Is(err, ErrResourceConflict) || engine.createCount() != 1 {
		t.Fatalf("container adopted without durable original intent: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Prepare(context.Background(), request); err == nil || engine.createCount() != 1 {
		t.Fatal("missing state recreated or adopted workload")
	}
}

func TestPreparationGenerationLockAndGenericReleaseRemainClosed(t *testing.T) {
	store, path, preparer, engine, request := preparedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	engine.onCreate = func() { close(entered); <-release }
	prepared := make(chan error, 1)
	go func() { _, err := preparer.Prepare(context.Background(), request); prepared <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("create was not reached")
	}
	newGeneration := make(chan error, 1)
	go func() { newGeneration <- store.AcceptGeneration(request.Instance, 2) }()
	select {
	case err := <-newGeneration:
		t.Fatalf("generation overtook in-flight create: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-prepared; err != nil {
		t.Fatal(err)
	}
	if err := <-newGeneration; err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.Prepare(context.Background(), request); !errors.Is(err, ErrGenerationStale) || engine.createCount() != 1 {
		t.Fatalf("old generation created: %v", err)
	}
	if _, err := preparer.Query(context.Background(), request.Instance, 1, request.RequestKey); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old generation confirmed current preparation: %v", err)
	}
	disk, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	record := disk.Preparations[request.Instance+"\x00"+request.RequestKey]
	resource := preparationResource(disk.HostID, record.Instance)
	if err := store.ReconcileRelease(request.Instance, 2, resource, func(Reservation) error { return nil }); !errors.Is(err, ErrPreparationRecoveryRequired) {
		t.Fatalf("generic proof forgot actual container: %v", err)
	}
	if !disk.ReconciliationRequired[resource] {
		t.Fatal("new generation lost old container reconciliation marker")
	}
	newer := request
	newer.Generation, newer.RequestKey = 2, "new-generation-key"
	if _, err := preparer.Prepare(context.Background(), newer); !errors.Is(err, ErrResourceConflict) || engine.createCount() != 1 {
		t.Fatalf("new generation stole existing instance resource: %v", err)
	}
}

func TestPreparationInspectRejectsChangedExecutionOrOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*dockerContainer)
	}{
		{"running", func(c *dockerContainer) { c.State.Status = "running"; c.State.Running = true }},
		{"previously-started", func(c *dockerContainer) { c.State.StartedAt = "2026-11-02T00:00:00Z" }},
		{"different-id", func(c *dockerContainer) { c.ID = strings.Repeat("b", 64) }},
		{"different-image", func(c *dockerContainer) { c.Image = "sha256:" + strings.Repeat("b", 64) }},
		{"different-owner", func(c *dockerContainer) { c.Config.Labels["io.thingspanel.agent.instance"] = "other" }},
		{"network", func(c *dockerContainer) { c.HostConfig.NetworkMode = "bridge" }},
		{"mount", func(c *dockerContainer) { c.Mounts = []json.RawMessage{json.RawMessage(`{"Source":"/secret"}`)} }},
		{"device", func(c *dockerContainer) {
			c.HostConfig.Devices = []json.RawMessage{json.RawMessage(`{"PathOnHost":"/dev/test"}`)}
		}},
		{"secret-env", func(c *dockerContainer) { c.Config.Env = append(c.Config.Env, "TOKEN=forbidden") }},
		{"privileged", func(c *dockerContainer) { c.HostConfig.Privileged = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, preparer, engine, request := preparedFixture(t)
			original, err := preparer.Prepare(context.Background(), request)
			if err != nil || original.State != PreparationPrepared {
				t.Fatal(err)
			}
			container := engine.containers[original.ContainerName]
			test.mutate(&container)
			engine.containers[original.ContainerName] = container
			observation, err := preparer.Query(context.Background(), request.Instance, 1, request.RequestKey)
			if err != nil || observation.State != PreparationUnknown || observation.Active || observation.ErrorCode != "CONTAINER_REQUIRES_OWNER_REVIEW" || engine.createCount() != 1 {
				t.Fatalf("untrusted container became prepared: %#v %v", observation, err)
			}
		})
	}
}

func TestPreparationRejectsImageInheritedResourcesAndUnacceptedGeneration(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*dockerImage)
	}{
		{"image-id", func(i *dockerImage) { i.ID = "sha256:" + strings.Repeat("b", 64) }},
		{"anonymous-volume", func(i *dockerImage) { i.Config.Volumes = map[string]json.RawMessage{"/data": json.RawMessage(`{}`)} }},
		{"exposed-port", func(i *dockerImage) {
			i.Config.ExposedPorts = map[string]json.RawMessage{"1883/tcp": json.RawMessage(`{}`)}
		}},
		{"inherited-env", func(i *dockerImage) { i.Config.Env = []string{"TOKEN=not-permitted"} }},
		{"inherited-label", func(i *dockerImage) {
			i.Config.Labels = map[string]string{"application": "not-approved-by-this-profile"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, preparer, engine, request := preparedFixture(t)
			test.mutate(&engine.image)
			if _, err := preparer.Prepare(context.Background(), request); !errors.Is(err, ErrPreparationPolicy) || engine.createCount() != 0 {
				t.Fatalf("unsafe image created: %v", err)
			}
		})
	}
	_, _, preparer, engine, request := preparedFixture(t)
	request.Generation = 2
	if _, err := preparer.Prepare(context.Background(), request); !errors.Is(err, ErrGenerationNotAccepted) || engine.createCount() != 0 {
		t.Fatalf("unaccepted generation created: %v", err)
	}
}

func TestPreparationReceiptFilesystemFailureDoesNotReturnPrepared(t *testing.T) {
	_, path, preparer, engine, request := preparedFixture(t)
	backup := path + ".intent-preserved"
	engine.onCreate = func() {
		if err := os.Rename(path, backup); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(path, "refuse-replace"), []byte("fixture"), 0600); err != nil {
			t.Error(err)
		}
	}
	result, err := preparer.Prepare(context.Background(), request)
	if err == nil || result.State == PreparationPrepared || result.Active || engine.createCount() != 1 {
		t.Fatalf("receipt write failure returned completed preparation: %#v %v", result, err)
	}
	if err := os.Remove(filepath.Join(path, "refuse-replace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	result, err = preparer.Query(context.Background(), request.Instance, 1, request.RequestKey)
	if err != nil || result.State != PreparationPrepared || engine.createCount() != 1 {
		t.Fatalf("original intent did not recover existing container: %#v %v", result, err)
	}
}

func TestPreparationIntentFilesystemFailurePreventsCreate(t *testing.T) {
	_, path, preparer, engine, request := preparedFixture(t)
	backup := path + ".before-intent"
	engine.onAbsent = func() {
		if err := os.Rename(path, backup); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Error(err)
		}
	}
	result, err := preparer.Prepare(context.Background(), request)
	if err == nil || result.State == PreparationPrepared || engine.createCount() != 0 {
		t.Fatalf("uncommitted intent reached Docker create: %#v creates=%d %v", result, engine.createCount(), err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := preparer.store.Query(request.Instance, request.RequestKey); err != nil || exists {
		t.Fatalf("failed intent changed reservation: exists=%v %v", exists, err)
	}
}
