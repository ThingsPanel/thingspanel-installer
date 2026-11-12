//go:build linux

package agent

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// Generic storage: signed catalog entries may declare a few named volumes and bounded tmpfs scratch directories.
// Nothing else reaches the container: no bind source, no driver, no options. The Agent derives every volume name.

const (
	maxVolumes     = 4
	maxTmpfs       = 2
	maxTmpfsMiB    = 256
	volumePrefix   = "tp-agent-vol-"
	tmpfsOptionFmt = "rw,noexec,nosuid,nodev,size="
)

var volumeKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

// PreparationMount is a durable named volume. Source is never stored: it is derived from host, instance and Key.
type PreparationMount struct {
	Key         string `json:"key"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

type PreparationTmpfs struct {
	Destination string `json:"destination"`
	SizeMiB     int    `json:"size_mib"`
}

var reservedMountPrefixes = []string{"/proc", "/sys", "/dev", "/etc", "/run", "/var/run", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/boot"}

func validDestination(d string) bool {
	if d == "" || len(d) > 255 || !path.IsAbs(d) || path.Clean(d) != d || d == "/" || strings.ContainsAny(d, ",:\x00\r\n\\") {
		return false
	}
	for _, p := range reservedMountPrefixes {
		if d == p || strings.HasPrefix(d, p+"/") {
			return false
		}
	}
	return true
}

func nested(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func validStorage(volumes []CatalogVolume, tmpfs []CatalogTmpfs) bool {
	m := make([]PreparationMount, 0, len(volumes))
	for _, v := range volumes {
		if v.Mode != "rw" && v.Mode != "ro" {
			return false
		}
		m = append(m, PreparationMount{Key: v.Key, Destination: v.Destination})
	}
	t := make([]PreparationTmpfs, 0, len(tmpfs))
	for _, x := range tmpfs {
		t = append(t, PreparationTmpfs(x))
	}
	return validMounts(m, t)
}

func validMounts(mounts []PreparationMount, tmpfs []PreparationTmpfs) bool {
	if len(mounts) > maxVolumes || len(tmpfs) > maxTmpfs {
		return false
	}
	var dests []string
	keys := map[string]bool{}
	for _, m := range mounts {
		if !volumeKeyPattern.MatchString(m.Key) || keys[m.Key] || !validDestination(m.Destination) {
			return false
		}
		keys[m.Key] = true
		dests = append(dests, m.Destination)
	}
	for _, t := range tmpfs {
		if t.SizeMiB < 1 || t.SizeMiB > maxTmpfsMiB || !validDestination(t.Destination) {
			return false
		}
		dests = append(dests, t.Destination)
	}
	for i := range dests {
		for j := i + 1; j < len(dests); j++ {
			if nested(dests[i], dests[j]) {
				return false
			}
		}
	}
	return true
}

// volumeName identifies a volume by host + instance (which carries the tenant install id) + key. The generation is
// deliberately absent: upgrade, rollback and re-prepare keep the same data.
func volumeName(host, instance, key string) string {
	return volumePrefix + preparationHash([]string{"volume", host, instance, key})[:40]
}

type dockerMount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

func expectedMounts(record containerPreparation, host string) ([]json.RawMessage, map[string]string) {
	mounts := []json.RawMessage{}
	for _, m := range record.Profile.Mounts {
		raw, _ := json.Marshal(dockerMount{Type: "volume", Source: volumeName(host, record.Instance, m.Key), Target: m.Destination, ReadOnly: m.ReadOnly})
		mounts = append(mounts, raw)
	}
	var tmp map[string]string
	if len(record.Profile.Tmpfs) > 0 {
		tmp = map[string]string{}
		for _, t := range record.Profile.Tmpfs {
			tmp[t.Destination] = tmpfsOptionFmt + itoa(t.SizeMiB) + "m"
		}
	}
	return mounts, tmp
}

// onlyMountKeys: Docker omits ReadOnly when false, so a legal readback has 3 or 4 keys; any other key (VolumeOptions,
// BindOptions, TmpfsOptions, Consistency, ...) is a knob the allowlist did not set.
func onlyMountKeys(m map[string]json.RawMessage) bool {
	for k := range m {
		if k != "Type" && k != "Source" && k != "Target" && k != "ReadOnly" {
			return false
		}
	}
	return true
}

func itoa(n int) string { raw, _ := json.Marshal(n); return string(raw) }

// inspectedMount is the shape Docker reports under container.Mounts for a volume.
type inspectedMount struct {
	Type        string
	Name        string
	Destination string
	Driver      string
	RW          bool
}

// safeStorage compares the container readback with exactly what the profile allows: the requested volumes (and
// nothing else), no binds, and the requested tmpfs.
func safeStorage(container dockerContainer, record containerPreparation, host string) bool {
	limits := container.HostConfig
	wantMounts, wantTmpfs := expectedMounts(record, host)
	if len(limits.Binds) != 0 || len(limits.Mounts) != len(wantMounts) || len(container.Mounts) != len(wantMounts) || len(limits.Tmpfs) != len(wantTmpfs) {
		return false
	}
	wantByTarget := map[string]dockerMount{}
	for _, raw := range wantMounts {
		var want dockerMount
		_ = json.Unmarshal(raw, &want)
		wantByTarget[want.Target] = want
	}
	for _, raw := range limits.Mounts { // order is not guaranteed by Docker; match by destination
		var got dockerMount
		var extra map[string]json.RawMessage // reject driver/option knobs the allowlist did not set
		if json.Unmarshal(raw, &got) != nil || json.Unmarshal(raw, &extra) != nil || !onlyMountKeys(extra) || wantByTarget[got.Target] != got {
			return false
		}
		delete(wantByTarget, got.Target)
	}
	for dest, opts := range wantTmpfs {
		if limits.Tmpfs[dest] != opts {
			return false
		}
	}
	wantInspected := map[string]PreparationMount{}
	for _, m := range record.Profile.Mounts {
		wantInspected[m.Destination] = m
	}
	for _, raw := range container.Mounts {
		var got inspectedMount
		if json.Unmarshal(raw, &got) != nil {
			return false
		}
		m, ok := wantInspected[got.Destination]
		if !ok || got.Type != "volume" || got.Name != volumeName(host, record.Instance, m.Key) || got.RW == m.ReadOnly || (got.Driver != "" && got.Driver != "local") {
			return false
		}
		delete(wantInspected, got.Destination)
	}
	return true
}
