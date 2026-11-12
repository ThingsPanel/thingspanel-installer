//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	tenantA = "11111111-aaaa-4bbb-8ccc-000000000001"
	tenantB = "22222222-aaaa-4bbb-8ccc-000000000002"
)

func volEntry() CatalogEntry {
	return CatalogEntry{ID: "matter", Version: "1.0.0", ImageID: "sha256:" + strings.Repeat("b", 64), Entrypoint: "/connector",
		Platform: struct {
			Binding           string                `json:"binding"`
			ServiceIdentifier string                `json:"service_identifier"`
			ProtocolType      string                `json:"protocol_type"`
			NetworkAlias      string                `json:"network_alias"`
			HTTPPort          int                   `json:"http_port"`
			ServicePlugin     *CatalogServicePlugin `json:"service_plugin"`
		}{Binding: "none", ServiceIdentifier: "matter"},
		Volumes: []CatalogVolume{{Key: "fabric", Destination: "/var/lib/thingspanel-matter", Mode: "rw"}},
		Tmpfs:   []CatalogTmpfs{{Destination: "/tmp", SizeMiB: 64}}}
}

func TestCatalogStorageValidationFailsClosed(t *testing.T) {
	if err := volEntry().validate(); err != nil {
		t.Fatalf("good entry refused: %v", err)
	}
	bad := map[string]func(*CatalogEntry){
		"bind-like source":   func(e *CatalogEntry) { e.Volumes[0].Destination = "/data:/host" },
		"relative":           func(e *CatalogEntry) { e.Volumes[0].Destination = "data" },
		"traversal":          func(e *CatalogEntry) { e.Volumes[0].Destination = "/var/lib/../../etc" },
		"root":               func(e *CatalogEntry) { e.Volumes[0].Destination = "/" },
		"reserved":           func(e *CatalogEntry) { e.Volumes[0].Destination = "/etc/ssl" },
		"docker socket path": func(e *CatalogEntry) { e.Volumes[0].Destination = "/var/run/docker.sock" },
		"bad mode":           func(e *CatalogEntry) { e.Volumes[0].Mode = "rw,z" },
		"bad key":            func(e *CatalogEntry) { e.Volumes[0].Key = "../x" },
		"duplicate key": func(e *CatalogEntry) {
			e.Volumes = append(e.Volumes, CatalogVolume{Key: "fabric", Destination: "/srv/b", Mode: "rw"})
		},
		"nested": func(e *CatalogEntry) {
			e.Volumes = append(e.Volumes, CatalogVolume{Key: "inner", Destination: "/var/lib/thingspanel-matter/x", Mode: "rw"})
		},
		"tmpfs collides with volume": func(e *CatalogEntry) { e.Tmpfs[0].Destination = "/var/lib/thingspanel-matter" },
		"tmpfs unbounded":            func(e *CatalogEntry) { e.Tmpfs[0].SizeMiB = 0 },
		"tmpfs huge":                 func(e *CatalogEntry) { e.Tmpfs[0].SizeMiB = 4096 },
	}
	for name, mutate := range bad {
		e := volEntry()
		mutate(&e)
		if e.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVolumeEntryNeedsTenantInstallID(t *testing.T) {
	_, _, builder := catalogFixture(t)
	e := volEntry()
	if _, err := builder.Build(e, "matter", 1, nil, identityToken); err == nil {
		t.Fatal("singleton instance must not get a volume shared by every tenant")
	}
	if _, err := builder.Build(e, "matter.bad_id", 1, nil, identityToken); err == nil {
		t.Fatal("malformed install id accepted")
	}
	if _, err := builder.Build(e, "other."+tenantA, 1, nil, identityToken); err == nil {
		t.Fatal("instance of another catalog id accepted")
	}
	p, err := builder.Build(e, "matter."+tenantA, 1, nil, identityToken)
	if err != nil || len(p.Mounts) != 1 || p.Mounts[0].Key != "fabric" || len(p.Tmpfs) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestVolumeIdentityIsPerTenantAndStableAcrossGenerations(t *testing.T) {
	a, b := volumeName("host", "matter."+tenantA, "fabric"), volumeName("host", "matter."+tenantB, "fabric")
	if a == b {
		t.Fatal("two tenants share a volume")
	}
	if a != volumeName("host", "matter."+tenantA, "fabric") {
		t.Fatal("volume identity is not deterministic")
	}
	if a == volumeName("other-host", "matter."+tenantA, "fabric") || a == volumeName("host", "matter."+tenantA, "other") {
		t.Fatal("host or key not part of identity")
	}
	// generation is not an input at all: a new generation or rollback reuses the data.
	host := "host"
	r1 := containerPreparation{Instance: "matter." + tenantA, Generation: 1, Profile: PreparationProfile{Mounts: []PreparationMount{{Key: "fabric", Destination: "/var/lib/x"}}}}
	r2 := r1
	r2.Generation = 7
	m1, _ := expectedMounts(r1, host)
	m2, _ := expectedMounts(r2, host)
	if string(m1[0]) != string(m2[0]) {
		t.Fatal("generation changed the volume source")
	}
}

// storageEngine reports Docker's inspect shape: volumes also appear under container.Mounts.
type storageEngine struct{ *preparationFixtureEngine }

func (e storageEngine) inspectContainer(ctx context.Context, name string) (dockerContainer, bool, error) {
	c, ok, err := e.preparationFixtureEngine.inspectContainer(ctx, name)
	if ok && len(c.Mounts) == 0 {
		for _, raw := range c.HostConfig.Mounts {
			var m dockerMount
			_ = json.Unmarshal(raw, &m)
			in, _ := json.Marshal(inspectedMount{Type: "volume", Name: m.Source, Destination: m.Target, Driver: "local", RW: !m.ReadOnly})
			c.Mounts = append(c.Mounts, in)
		}
	}
	return c, ok, err
}

func storageProfile() PreparationProfile {
	p := preparationFixtureProfile()
	p.Name = "storage-fixture"
	p.Mounts = []PreparationMount{{Key: "fabric", Destination: "/var/lib/thingspanel-matter"}}
	p.Tmpfs = []PreparationTmpfs{{Destination: "/tmp", SizeMiB: 64}}
	return p
}

func prepareWithStorage(t *testing.T, tamper func(*dockerContainer)) PreparationObservation {
	t.Helper()
	store, _ := initialized(t)
	instance := "matter." + tenantA
	if err := store.AcceptGeneration(instance, 1); err != nil {
		t.Fatal(err)
	}
	fixture := newPreparationFixtureEngine()
	engine := storageEngine{fixture}
	preparer, err := newDockerPreparer(store, engine, []PreparationProfile{storageProfile()})
	if err != nil {
		t.Fatal(err)
	}
	if tamper != nil {
		fixture.onCreate = func() { // runs after the fixture released its lock
			fixture.mu.Lock()
			names := make([]string, 0, len(fixture.containers))
			for name := range fixture.containers {
				names = append(names, name)
			}
			fixture.mu.Unlock()
			for _, name := range names {
				c, _, _ := engine.inspectContainer(context.Background(), name) // fills Mounts
				tamper(&c)
				fixture.mu.Lock()
				fixture.containers[name] = c
				fixture.mu.Unlock()
			}
		}
	}
	obs, _ := preparer.Prepare(context.Background(), PrepareContainerRequest{Instance: instance, Generation: 1, RequestKey: "k", Profile: "storage-fixture"})
	return obs
}

func TestPreparedContainerWithExactVolumeAndTmpfsIsAccepted(t *testing.T) {
	obs := prepareWithStorage(t, nil)
	if obs.State != PreparationPrepared {
		t.Fatalf("%+v", obs)
	}
}

func TestReadbackRejectsAnythingBeyondDeclaredStorage(t *testing.T) {
	cases := map[string]func(*dockerContainer){
		"extra bind": func(c *dockerContainer) { c.HostConfig.Binds = []string{"/:/host"} },
		"wrong source": func(c *dockerContainer) {
			c.HostConfig.Mounts[0] = json.RawMessage(`{"Type":"volume","Source":"shared","Target":"/var/lib/thingspanel-matter","ReadOnly":false}`)
		},
		"bind type": func(c *dockerContainer) {
			c.HostConfig.Mounts[0] = json.RawMessage(`{"Type":"bind","Source":"/","Target":"/var/lib/thingspanel-matter","ReadOnly":false}`)
		},
		"driver knob": func(c *dockerContainer) {
			c.HostConfig.Mounts[0] = json.RawMessage(strings.Replace(string(c.HostConfig.Mounts[0]), "}", `,"VolumeOptions":{"DriverConfig":{"Name":"x"}}}`, 1))
		},
		"extra mount": func(c *dockerContainer) {
			c.Mounts = append(c.Mounts, json.RawMessage(`{"Type":"bind","Destination":"/x","RW":true}`))
		},
		"read-only flip": func(c *dockerContainer) {
			c.Mounts[0] = json.RawMessage(strings.Replace(string(c.Mounts[0]), `"RW":true`, `"RW":false`, 1))
		},
		"foreign driver": func(c *dockerContainer) {
			c.Mounts[0] = json.RawMessage(strings.Replace(string(c.Mounts[0]), `"local"`, `"nfs"`, 1))
		},
		"tmpfs changed": func(c *dockerContainer) { c.HostConfig.Tmpfs["/tmp"] = "rw,exec,size=999m" },
		"extra tmpfs":   func(c *dockerContainer) { c.HostConfig.Tmpfs["/var/tmp"] = "rw" },
		"volumes-from":  func(c *dockerContainer) { c.HostConfig.VolumesFrom = []string{"other"} },
	}
	for name, tamper := range cases {
		if obs := prepareWithStorage(t, tamper); obs.State == PreparationPrepared {
			t.Errorf("%s: tampered container accepted", name)
		}
	}
}

func TestStorageChangesTheProfileDigest(t *testing.T) {
	a := storageProfile()
	b := storageProfile()
	b.Mounts[0].ReadOnly = true
	req := PrepareContainerRequest{Instance: "matter." + tenantA, Generation: 1, RequestKey: "k", Profile: a.Name}
	if preparationDigest("h", req, a) == preparationDigest("h", req, b) {
		t.Fatal("storage not part of the digest")
	}
}
