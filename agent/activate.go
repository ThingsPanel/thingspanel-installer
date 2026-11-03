//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Activation is the only place an Agent-prepared container is ever started.
//
// A prepared container stays "created" until a control plane that owns the
// instance issues an explicit, time-limited grant for exactly one accepted
// generation. Every later step re-checks that generation, the grant lease and
// the revocation flag under one lock. Revocation is written before anything is
// stopped and is permanent for that generation: a new execution needs a newer
// accepted generation. Lease expiry and a newer accepted generation both stop
// the container, so an instance that lost its control plane or was superseded
// cannot keep producing effects.
var (
	ErrNotPrepared          = errors.New("container is not in the prepared state for this generation")
	ErrNotGranted           = errors.New("no activation grant for this instance, generation and request key")
	ErrLeaseExpired         = errors.New("activation lease has expired; a newer generation is required")
	ErrRevoked              = errors.New("activation was revoked for this generation; a newer generation is required")
	ErrActivationUnresolved = errors.New("an earlier start or stop outcome is unresolved; owner review is required")
)

const (
	activationVersion = 1
	maxGrantTTL       = 24 * time.Hour
)

type activationEngine interface {
	inspectContainer(ctx context.Context, name string) (dockerContainer, bool, error)
	startContainer(ctx context.Context, id string) error
	stopContainer(ctx context.Context, id string) error
	removeContainer(ctx context.Context, id string) error
}

// ActivationRecord is the durable per-instance activation receipt.
type ActivationRecord struct {
	Instance      string `json:"instance"`
	Generation    uint64 `json:"generation"`
	RequestKey    string `json:"request_key"`
	ContainerName string `json:"container_name"`
	ContainerID   string `json:"container_id"`
	GrantID       string `json:"grant_id"`
	LeaseExpires  int64  `json:"lease_expires_unix"`
	Revoked       bool   `json:"revoked"`
	RevokeReason  string `json:"revoke_reason,omitempty"`
	StartIntent   bool   `json:"start_intent"`
	Started       bool   `json:"started"`
	StopIntent    bool   `json:"stop_intent"`
}

type activationDisk struct {
	Version   int                          `json:"version"`
	Instances map[string]*ActivationRecord `json:"instances"`
}

type Activator struct {
	store    *Store
	preparer *DockerPreparer
	engine   activationEngine
	path     string
	now      func() time.Time
}

func NewActivator(store *Store, preparer *DockerPreparer, engine *UnixDockerEngine) (*Activator, error) {
	if store == nil || preparer == nil || engine == nil {
		return nil, ErrPreparationPolicy
	}
	return newActivator(store, preparer, engine), nil
}

func newActivator(store *Store, preparer *DockerPreparer, engine activationEngine) *Activator {
	return &Activator{store: store, preparer: preparer, engine: engine, path: store.path + ".activation", now: time.Now}
}

func (engine *UnixDockerEngine) startContainer(ctx context.Context, id string) error {
	if !validHexID(id) {
		return ErrPreparationPolicy
	}
	status, err := engine.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotModified {
		return fmt.Errorf("Docker start result unconfirmed (status %d)", status)
	}
	return nil
}

func (engine *UnixDockerEngine) stopContainer(ctx context.Context, id string) error {
	if !validHexID(id) {
		return ErrPreparationPolicy
	}
	status, err := engine.call(ctx, http.MethodPost, "/containers/"+id+"/stop?t=5", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotModified {
		return fmt.Errorf("Docker stop result unconfirmed (status %d)", status)
	}
	return nil
}

func (engine *UnixDockerEngine) removeContainer(ctx context.Context, id string) error {
	if !validHexID(id) {
		return ErrPreparationPolicy
	}
	// Never forced: a running or otherwise busy container is a refusal, not a kill.
	status, err := engine.call(ctx, http.MethodDelete, "/containers/"+id+"?force=false&v=false", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("Docker remove result unconfirmed (status %d)", status)
	}
	return nil
}

func (a *Activator) loadLocked() (activationDisk, error) {
	disk := activationDisk{Version: activationVersion, Instances: map[string]*ActivationRecord{}}
	raw, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return disk, nil
	}
	if err != nil {
		return disk, err
	}
	if err := json.Unmarshal(raw, &disk); err != nil || disk.Version != activationVersion || disk.Instances == nil {
		return activationDisk{}, ErrInvalidState
	}
	return disk, nil
}

func (a *Activator) saveLocked(disk activationDisk) error {
	raw, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(a.path), ".activation-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, a.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(a.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *Activator) accepted(instance string, generation uint64) error {
	_, generations, _, err := a.store.Snapshot()
	if err != nil {
		return err
	}
	current := generations[instance]
	switch {
	case generation == 0 || current == 0:
		return ErrGenerationNotAccepted
	case generation < current:
		return ErrGenerationStale
	case generation > current:
		return ErrGenerationNotAccepted
	}
	return nil
}

// Grant lets exactly one accepted generation of a prepared container start
// until now+ttl. It never starts anything.
func (a *Activator) Grant(ctx context.Context, instance string, generation uint64, key, grantID string, ttl time.Duration) (ActivationRecord, error) {
	var out ActivationRecord
	if !valid(instance) || !valid(key) || !valid(grantID) || ttl <= 0 || ttl > maxGrantTTL {
		return out, ErrPreparationPolicy
	}
	if err := a.accepted(instance, generation); err != nil {
		return out, err
	}
	// A container that was ever started must never be queried through the create-only
	// adapter: that check would rewrite its ledger entry as unknown. The activation
	// ledger answers first.
	if existing, err := a.Status(instance); err != nil {
		return out, err
	} else if existing.Generation == generation && (existing.Revoked || existing.StartIntent || existing.Started) {
		if existing.Revoked {
			return out, ErrRevoked
		}
		return out, ErrActivationUnresolved
	} else if existing.Generation > generation {
		return out, ErrGenerationStale
	}
	observation, err := a.preparer.Query(ctx, instance, generation, key)
	if err != nil {
		return out, err
	}
	if observation.State != PreparationPrepared || observation.ContainerID == "" {
		return out, ErrNotPrepared
	}
	err = withLock(a.path, func() error {
		disk, err := a.loadLocked()
		if err != nil {
			return err
		}
		if existing := disk.Instances[instance]; existing != nil {
			switch {
			case existing.Generation > generation:
				return ErrGenerationStale
			case existing.Generation == generation && existing.Revoked:
				return ErrRevoked
			case existing.Generation == generation && (existing.StartIntent || existing.StopIntent) && existing.RequestKey != key:
				return ErrActivationUnresolved
			}
		}
		record := &ActivationRecord{Instance: instance, Generation: generation, RequestKey: key, ContainerName: observation.ContainerName,
			ContainerID: observation.ContainerID, GrantID: grantID, LeaseExpires: a.now().Add(ttl).Unix()}
		if existing := disk.Instances[instance]; existing != nil && existing.Generation == generation {
			record.StartIntent, record.Started = existing.StartIntent, existing.Started
		}
		disk.Instances[instance] = record
		out = *record
		return a.saveLocked(disk)
	})
	return out, err
}

// Activate starts the prepared container if, and only if, the grant is still valid.
func (a *Activator) Activate(ctx context.Context, instance string, generation uint64, key string) (ActivationRecord, error) {
	var out ActivationRecord
	if !valid(instance) || !valid(key) {
		return out, ErrPreparationPolicy
	}
	err := withLock(a.path, func() error {
		if err := a.accepted(instance, generation); err != nil {
			return err
		}
		disk, err := a.loadLocked()
		if err != nil {
			return err
		}
		record := disk.Instances[instance]
		switch {
		case record == nil || record.Generation != generation || record.RequestKey != key:
			return ErrNotGranted
		case record.Revoked:
			return ErrRevoked
		case record.StopIntent:
			return ErrActivationUnresolved
		case a.now().Unix() >= record.LeaseExpires:
			return ErrLeaseExpired
		}
		if record.Started {
			out = *record
			return nil
		}
		if !record.StartIntent {
			// The preparation must still be the untouched create-only container.
			observation, err := a.preparer.Query(ctx, instance, generation, key)
			if err != nil {
				return err
			}
			if observation.State != PreparationPrepared || observation.ContainerID != record.ContainerID {
				return ErrNotPrepared
			}
		}
		// Intent is durable before the daemon call: a lost reply is then resolved by
		// inspection, never by a blind second start.
		record.StartIntent = true
		if err := a.saveLocked(disk); err != nil {
			return err
		}
		startErr := a.engine.startContainer(ctx, record.ContainerID)
		container, exists, inspectErr := a.engine.inspectContainer(ctx, record.ContainerName)
		if inspectErr != nil || !exists || container.ID != record.ContainerID {
			return ErrActivationUnresolved
		}
		if container.State.Running {
			record.Started = true
			out = *record
			return a.saveLocked(disk)
		}
		if startErr != nil {
			return startErr
		}
		return ErrActivationUnresolved
	})
	return out, err
}

// Revoke permanently ends the generation. The revocation is durable before the
// container is stopped, so a crash between the two can only leave a stopped-late
// container that Sweep (or a repeated Revoke) stops, never a re-activation.
func (a *Activator) Revoke(ctx context.Context, instance string, generation uint64, reason string) (ActivationRecord, error) {
	var out ActivationRecord
	if !valid(instance) || generation == 0 {
		return out, ErrPreparationPolicy
	}
	err := withLock(a.path, func() error {
		disk, err := a.loadLocked()
		if err != nil {
			return err
		}
		record := disk.Instances[instance]
		if record != nil && record.Generation > generation {
			return ErrGenerationStale
		}
		if record == nil || record.Generation < generation {
			record = &ActivationRecord{Instance: instance, Generation: generation}
			disk.Instances[instance] = record
		}
		record.Revoked, record.RevokeReason = true, strings.TrimSpace(reason)
		record.StopIntent = record.StartIntent || record.Started
		if err := a.saveLocked(disk); err != nil {
			return err
		}
		out = *record
		return a.stopLocked(ctx, &disk, record)
	})
	if err == nil {
		out, _ = a.Status(instance)
	}
	return out, err
}

func (a *Activator) stopLocked(ctx context.Context, disk *activationDisk, record *ActivationRecord) error {
	if !record.StopIntent || record.ContainerID == "" {
		return nil
	}
	stopErr := a.engine.stopContainer(ctx, record.ContainerID)
	container, exists, inspectErr := a.engine.inspectContainer(ctx, record.ContainerName)
	if inspectErr != nil {
		return ErrActivationUnresolved
	}
	if exists && container.ID == record.ContainerID && container.State.Running {
		if stopErr != nil {
			return stopErr
		}
		return ErrActivationUnresolved
	}
	record.StopIntent, record.Started = false, false
	return a.saveLocked(*disk)
}

// Sweep stops every container whose lease expired or whose generation is no
// longer the accepted one, and finishes interrupted stops. It is safe to repeat.
func (a *Activator) Sweep(ctx context.Context) ([]string, error) {
	var stopped []string
	_, generations, _, err := a.store.Snapshot()
	if err != nil {
		return nil, err
	}
	err = withLock(a.path, func() error {
		disk, err := a.loadLocked()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(disk.Instances))
		for name := range disk.Instances {
			names = append(names, name)
		}
		sort.Strings(names)
		var firstErr error
		for _, name := range names {
			record := disk.Instances[name]
			expired := record.LeaseExpires != 0 && a.now().Unix() >= record.LeaseExpires
			superseded := generations[name] > record.Generation
			if !record.Revoked && (expired || superseded) && (record.StartIntent || record.Started) {
				record.Revoked = true
				if superseded {
					record.RevokeReason = "superseded by a newer accepted generation"
				} else {
					record.RevokeReason = "lease expired"
				}
				record.StopIntent = true
				if err := a.saveLocked(disk); err != nil {
					return err
				}
			}
			if record.StopIntent {
				if err := a.stopLocked(ctx, &disk, record); err != nil && firstErr == nil {
					firstErr = err
				} else if err == nil {
					stopped = append(stopped, name)
				}
			}
		}
		return firstErr
	})
	return stopped, err
}

// Status reports the durable activation receipt for an instance.
func (a *Activator) Status(instance string) (ActivationRecord, error) {
	var out ActivationRecord
	err := withLock(a.path, func() error {
		disk, err := a.loadLocked()
		if err != nil {
			return err
		}
		if record := disk.Instances[instance]; record != nil {
			out = *record
		}
		return nil
	})
	return out, err
}

// HasStarted reports whether preparation Query/Prepare must be bypassed for this
// generation: querying a started container through the create-only adapter would
// rewrite its ledger entry as unknown.
func (a *Activator) HasStarted(instance string, generation uint64) bool {
	record, err := a.Status(instance)
	return err == nil && record.Generation == generation && (record.StartIntent || record.Started)
}

// Retire is the approved takeover step the preparation ledger waits for: once a
// newer generation is accepted and the older container is provably not running,
// the older container is removed (never forced) and its exclusive runtime
// resource is released, so the newer generation can prepare its own container.
func (a *Activator) Retire(ctx context.Context, instance string, oldGeneration uint64) error {
	if !valid(instance) || oldGeneration == 0 {
		return ErrPreparationPolicy
	}
	_, generations, _, err := a.store.Snapshot()
	if err != nil {
		return err
	}
	if generations[instance] <= oldGeneration {
		return ErrGenerationNotAccepted // only a superseded generation can be retired
	}
	return withLock(a.path, func() error {
		disk, err := a.loadLocked()
		if err != nil {
			return err
		}
		record := disk.Instances[instance]
		if record != nil && record.Generation == oldGeneration && (record.StopIntent || (record.Started && !record.Revoked)) {
			return ErrActivationUnresolved // still running or being stopped: sweep or revoke first
		}
		var prepared containerPreparation
		var key string
		err = withLock(a.store.path, func() error {
			d, e := load(a.store.path)
			if e != nil {
				return e
			}
			for identity, candidate := range d.Preparations {
				if candidate.Instance == instance && candidate.Generation == oldGeneration {
					key, prepared = identity, candidate
				}
			}
			if key == "" {
				return ErrNotPrepared
			}
			return nil
		})
		if err != nil {
			return err
		}
		if prepared.ContainerID != "" {
			container, exists, err := a.engine.inspectContainer(ctx, prepared.ContainerName)
			if err != nil {
				return err
			}
			if exists {
				if container.ID != prepared.ContainerID || container.State.Running || container.State.Restarting {
					return ErrActivationUnresolved
				}
				if err := a.engine.removeContainer(ctx, container.ID); err != nil {
					return err
				}
			}
			if _, stillThere, err := a.engine.inspectContainer(ctx, prepared.ContainerName); err != nil || stillThere {
				return ErrActivationUnresolved
			}
		}
		return withLock(a.store.path, func() error {
			d, e := load(a.store.path)
			if e != nil {
				return e
			}
			if d.MaxGeneration[instance] <= oldGeneration {
				return ErrGenerationNotAccepted
			}
			current, found := d.Preparations[key]
			if !found || current.ContainerID != prepared.ContainerID {
				return ErrInvalidState
			}
			resource := preparationResource(d.HostID, instance)
			held, reserved := d.Resources[resource]
			if reserved && (held.Instance != instance || held.Generation != oldGeneration) {
				return ErrNotOwner
			}
			delete(d.Preparations, key)
			if reserved {
				delete(d.Resources, resource)
				delete(d.ReconciliationRequired, resource)
				held.State = "released"
				d.Requests[key] = held
			}
			return writeState(a.store.path, d)
		})
	})
}
