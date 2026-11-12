//go:build linux

package agent

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Opt-in like the preparation tests: TP_AGENT_REAL_DOCKER=1 with a reviewed local image ID.
func activationFixture(t *testing.T) (*Activator, *DockerPreparer, *Store, *UnixDockerEngine) {
	t.Helper()
	store, _, engine, profile := realPreparationFixture(t)
	// The reviewed fixture image contains one inert program; the fixed entrypoint is that program.
	profile.Entrypoint = "/pluginstub"
	profile.Args = []string{}
	preparer, err := NewDockerPreparer(store, engine, []PreparationProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	activator, err := NewActivator(store, preparer, engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Runs before the preparation fixture's own cleanup: stop and remove what this test started.
		disk, err := load(store.path)
		if err != nil {
			return
		}
		for _, record := range disk.Preparations {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			container, exists, err := engine.inspectContainer(ctx, record.ContainerName)
			if err == nil && exists && container.ID == record.ContainerID {
				_ = engine.stopContainer(ctx, container.ID)
				engine.call(ctx, http.MethodDelete, "/containers/"+container.ID+"?force=false&v=false", nil, nil)
			}
			cancel()
		}
	})
	return activator, preparer, store, engine
}

func prepareInstance(t *testing.T, preparer *DockerPreparer, profile string, instance string, generation uint64, key string) PreparationObservation {
	t.Helper()
	observation, err := preparer.Prepare(context.Background(), PrepareContainerRequest{Instance: instance, Generation: generation, RequestKey: key, Profile: profile})
	if err != nil || observation.State != PreparationPrepared {
		t.Fatalf("prepare %s gen %d: %v %+v", instance, generation, err, observation)
	}
	return observation
}

func containerState(t *testing.T, engine *UnixDockerEngine, observation PreparationObservation) (string, bool) {
	t.Helper()
	container, exists, err := engine.inspectContainer(context.Background(), observation.ContainerName)
	if err != nil || !exists {
		t.Fatalf("inspect: exists=%v err=%v", exists, err)
	}
	return container.State.Status, container.State.Running
}

func profileName(preparer *DockerPreparer) string {
	for name := range preparer.profiles {
		return name
	}
	return ""
}

func TestActivationWithoutGrantNeverStartsRealDocker(t *testing.T) {
	activator, preparer, _, engine := activationFixture(t)
	observation := prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k1")
	if _, err := activator.Activate(context.Background(), "instance-a", 1, "k1"); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("activate without grant must be refused, got %v", err)
	}
	if status, running := containerState(t, engine, observation); status != "created" || running {
		t.Fatalf("an unactivated instance must stay created, got %s running=%v", status, running)
	}
}

func TestActivationGrantStartRevokeIsPermanentRealDocker(t *testing.T) {
	activator, preparer, _, engine := activationFixture(t)
	ctx := context.Background()
	observation := prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k1")
	if _, err := activator.Grant(ctx, "instance-a", 1, "k1", "grant-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if status, running := containerState(t, engine, observation); status != "created" || running {
		t.Fatalf("a grant alone must not start anything, got %s running=%v", status, running)
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, observation); !running {
		t.Fatal("granted instance must be running")
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k1"); err != nil {
		t.Fatalf("repeating a granted activation must be idempotent: %v", err)
	}
	if _, err := activator.Revoke(ctx, "instance-a", 1, "operator revoked"); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, observation); running {
		t.Fatal("revoked instance must be stopped")
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k1"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("a revoked generation must not start again, got %v", err)
	}
	if _, err := activator.Grant(ctx, "instance-a", 1, "k1", "grant-2", time.Minute); !errors.Is(err, ErrRevoked) {
		t.Fatalf("a revoked generation must not be re-granted, got %v", err)
	}
	if _, running := containerState(t, engine, observation); running {
		t.Fatal("no refused call may restart the container")
	}
}

func TestActivationOldGenerationHasNoEffectRealDocker(t *testing.T) {
	activator, preparer, store, engine := activationFixture(t)
	ctx := context.Background()
	old := prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k-old")
	if _, err := activator.Grant(ctx, "instance-a", 1, "k-old", "grant-old", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	// A queued task from the old generation arrives late.
	if _, err := activator.Activate(ctx, "instance-a", 1, "k-old"); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old generation activation must be refused as stale, got %v", err)
	}
	if _, err := activator.Grant(ctx, "instance-a", 1, "k-old", "grant-late", time.Minute); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old generation grant must be refused as stale, got %v", err)
	}
	if status, running := containerState(t, engine, old); status != "created" || running {
		t.Fatalf("a stale task must not change the container, got %s running=%v", status, running)
	}
	// Taking over: the superseded container is removed (not forced) and its resource released.
	if err := activator.Retire(ctx, "instance-a", 1); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := engine.inspectContainer(ctx, old.ContainerName); err != nil || exists {
		t.Fatalf("the retired container must be gone: exists=%v err=%v", exists, err)
	}
	// The newer generation proceeds independently.
	fresh := prepareInstance(t, preparer, profileName(preparer), "instance-a", 2, "k-new")
	if _, err := activator.Grant(ctx, "instance-a", 2, "k-new", "grant-new", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Activate(ctx, "instance-a", 2, "k-new"); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, fresh); !running {
		t.Fatal("new generation must run")
	}
}

func TestActivationSupersededRunningInstanceIsStoppedRealDocker(t *testing.T) {
	activator, preparer, store, engine := activationFixture(t)
	ctx := context.Background()
	old := prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k-old")
	if _, err := activator.Grant(ctx, "instance-a", 1, "k-old", "grant-old", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k-old"); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, old); !running {
		t.Fatal("precondition: old instance runs until swept")
	}
	if _, err := activator.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, old); running {
		t.Fatal("a superseded instance must be stopped by the sweep")
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k-old"); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("superseded generation must stay refused, got %v", err)
	}
	if err := activator.Retire(ctx, "instance-a", 1); err != nil {
		t.Fatal(err)
	}
	fresh := prepareInstance(t, preparer, profileName(preparer), "instance-a", 2, "k-new")
	if _, err := activator.Grant(ctx, "instance-a", 2, "k-new", "grant-new", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Activate(ctx, "instance-a", 2, "k-new"); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, fresh); !running {
		t.Fatal("takeover generation must run")
	}
}

func TestActivationRunningInstanceCannotBeRetiredRealDocker(t *testing.T) {
	activator, preparer, store, engine := activationFixture(t)
	ctx := context.Background()
	old := prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k-old")
	if _, err := activator.Grant(ctx, "instance-a", 1, "k-old", "g", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k-old"); err != nil {
		t.Fatal(err)
	}
	if err := activator.Retire(ctx, "instance-a", 1); !errors.Is(err, ErrGenerationNotAccepted) {
		t.Fatalf("the current generation cannot be retired, got %v", err)
	}
	if err := store.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	if err := activator.Retire(ctx, "instance-a", 1); err == nil {
		t.Fatal("a still-running superseded instance must not be removed before it is stopped")
	}
	if _, running := containerState(t, engine, old); !running {
		t.Fatal("a refused retire must not touch the container")
	}
}

func TestActivationLostControlPlaneLeaseStopsInstanceRealDocker(t *testing.T) {
	activator, preparer, _, engine := activationFixture(t)
	ctx := context.Background()
	observation := prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k1")
	if _, err := activator.Grant(ctx, "instance-a", 1, "k1", "grant-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k1"); err != nil {
		t.Fatal(err)
	}
	// The control plane goes silent: no renewal arrives before the lease ends.
	base := time.Now()
	activator.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := activator.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, running := containerState(t, engine, observation); running {
		t.Fatal("an instance whose lease expired must be stopped")
	}
	if _, err := activator.Activate(ctx, "instance-a", 1, "k1"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("an expired instance must need a newer generation, got %v", err)
	}
}

func TestActivationStatusSurvivesRestartRealDocker(t *testing.T) {
	activator, preparer, store, _ := activationFixture(t)
	ctx := context.Background()
	prepareInstance(t, preparer, profileName(preparer), "instance-a", 1, "k1")
	if _, err := activator.Grant(ctx, "instance-a", 1, "k1", "grant-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := activator.Revoke(ctx, "instance-a", 1, "before restart"); err != nil {
		t.Fatal(err)
	}
	reopened, err := newActivatorFor(store, preparer)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reopened.Status("instance-a")
	if err != nil || !record.Revoked || record.Generation != 1 {
		t.Fatalf("revocation must survive a restart: %+v %v", record, err)
	}
}

func newActivatorFor(store *Store, preparer *DockerPreparer) (*Activator, error) {
	return &Activator{store: store, preparer: preparer, engine: preparer.engine.(activationEngine), path: store.path + ".activation", now: time.Now}, nil
}

// Real daemon, real start: data written into the declared volume survives a generation takeover, a second tenant
// gets its own volume, and a second writer is refused while the first generation still holds the instance.
// The image must hold /pluginstub, which appends "n=<lines seen>" to /data/log (see TP_AGENT_DOCKER_IMAGE_ID).
func TestActivationVolumeSurvivesTakeoverAndIsPerTenantRealDocker(t *testing.T) {
	var volumes []string
	t.Cleanup(func() { // registered first: runs after the containers were removed
		engine, err := NewUnixDockerEngine(os.Getenv("TP_AGENT_DOCKER_SOCKET"))
		if err != nil {
			return
		}
		for _, name := range volumes {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			status, _ := engine.call(ctx, http.MethodDelete, "/volumes/"+name, nil, nil)
			cancel()
			if status != http.StatusNoContent {
				t.Errorf("test volume %s not removed (status %d)", name, status)
			}
		}
	})
	activator, preparer, store, engine := activationFixture(t)
	ctx := context.Background()
	host, err := store.HostID()
	if err != nil {
		t.Fatal(err)
	}
	base := preparer.profiles[profileName(preparer)]
	mk := func(name string) PreparationProfile {
		p := base
		p.Name, p.Mounts = name, []PreparationMount{{Key: "fabric", Destination: "/data"}}
		return p
	}
	for _, n := range []string{"vol-a-g1", "vol-a-g2", "vol-b-g1"} {
		if err := preparer.SetProfile(mk(n)); err != nil {
			t.Fatal(err)
		}
	}
	a, b := "matter."+tenantA, "matter."+tenantB
	volumes = append(volumes, volumeName(host, a, "fabric"), volumeName(host, b, "fabric"))
	for _, instance := range []string{a, b} {
		if err := store.AcceptGeneration(instance, 1); err != nil {
			t.Fatal(err)
		}
	}
	start := func(profile, instance string, generation uint64, key string) PreparationObservation {
		obs := prepareInstance(t, preparer, profile, instance, generation, key)
		if _, err := activator.Grant(ctx, instance, generation, key, "grant-"+key, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := activator.Activate(ctx, instance, generation, key); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond) // let the stub write its line
		if _, running := containerState(t, engine, obs); !running {
			t.Fatalf("%s gen %d is not running", instance, generation)
		}
		return obs
	}
	readLog := func(obs PreparationObservation) string { // Docker archive API: no docker CLI needed
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+dockerPreparationAPI+"/containers/"+obs.ContainerID+"/archive?path=/data/log", nil)
		response, err := engine.client.Do(request)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("read /data/log of %s: %v", obs.ContainerName, err)
		}
		defer response.Body.Close()
		archive := tar.NewReader(response.Body)
		if _, err := archive.Next(); err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(io.LimitReader(archive, 1<<16))
		return string(raw)
	}

	start("vol-a-g1", a, 1, "ka1")
	// A second writer for the same install while generation 1 holds it is refused.
	if err := store.AcceptGeneration(a, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.Prepare(ctx, PrepareContainerRequest{Instance: a, Generation: 2, RequestKey: "ka2", Profile: "vol-a-g2"}); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("second writer must be refused until the old generation is retired, got %v", err)
	}
	if _, err := activator.Sweep(ctx); err != nil { // superseded gen 1 is stopped
		t.Fatal(err)
	}
	if err := activator.Retire(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	obsA2 := start("vol-a-g2", a, 2, "ka2")
	if got := readLog(obsA2); got != "n=0\nn=1\n" {
		t.Fatalf("tenant A data did not survive the takeover: %q", got)
	}
	obsB := start("vol-b-g1", b, 1, "kb1")
	if got := readLog(obsB); got != "n=0\n" {
		t.Fatalf("tenant B must start with its own empty volume, got %q", got)
	}
}
