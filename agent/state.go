//go:build linux

// Package agent implements local host state and a create-only Docker adapter.
// It never starts workloads and has no public control or activation API.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

var (
	ErrAlreadyInitialized    = errors.New("state already exists; refusing reinitialization")
	ErrStateMissing          = errors.New("state missing; explicit host identity review required")
	ErrInvalidState          = errors.New("state missing, corrupt, or inconsistent; refusing automatic recovery")
	ErrGenerationStale       = errors.New("execution generation is lower than persisted maximum")
	ErrGenerationNotAccepted = errors.New("execution generation has not been explicitly accepted")
	ErrResourceConflict      = errors.New("resource is reserved by another owner or generation")
	ErrNotOwner              = errors.New("release owner or generation does not match reservation")
)

type Reservation struct {
	Resource    string `json:"resource"`
	Instance    string `json:"instance"`
	Generation  uint64 `json:"generation"`
	RequestKey  string `json:"request_key"`
	PayloadHash string `json:"payload_hash"`
	State       string `json:"state"` // held | released; request identities are never reusable
}

type Request struct {
	Instance    string
	Generation  uint64
	RequestKey  string
	Resource    string
	PayloadHash string
}

type diskState struct {
	SchemaVersion          int                             `json:"schema_version"`
	HostID                 string                          `json:"host_id"`
	MaxGeneration          map[string]uint64               `json:"max_generation"`
	Requests               map[string]Reservation          `json:"requests"`
	Resources              map[string]Reservation          `json:"resources"`
	ReconciliationRequired map[string]bool                 `json:"reconciliation_required"`
	Preparations           map[string]containerPreparation `json:"preparations,omitempty"`
}

type Store struct{ path string }

// Initialize creates a new host identity and empty state exactly once. The
// caller must supply a reviewed identity; this function never generates one.
// If the state file or its directory is later missing, Open fails closed; only a
// separate reviewed registration process may decide whether to reinitialize.
func Initialize(path, hostID string) error {
	if !valid(hostID) {
		return fmt.Errorf("invalid host identity")
	}
	return withLock(path, func() error {
		if _, err := os.Lstat(path); err == nil {
			return ErrAlreadyInitialized
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeState(path, diskState{SchemaVersion: 1, HostID: hostID, MaxGeneration: map[string]uint64{}, Requests: map[string]Reservation{}, Resources: map[string]Reservation{}, ReconciliationRequired: map[string]bool{}})
	})
}

// Open refuses absent or malformed state; it never silently initializes.
func Open(path string) (*Store, error) {
	err := withLock(path, func() error { _, e := load(path); return e })
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrStateMissing
		}
		if errors.Is(err, ErrInvalidState) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	return &Store{path: path}, nil
}

// AcceptGeneration persists task acceptance before any resource reservation.
// A higher accepted generation fences stale state operations even if subsequent
// reservation fails. This is NOT a side-effect execution fence; future executors
// must re-check under an instance lock immediately before each external effect.
func (s *Store) AcceptGeneration(instance string, generation uint64) error {
	if !valid(instance) || generation == 0 {
		return errors.New("invalid generation acceptance")
	}
	return withLock(s.path, func() error {
		d, err := load(s.path)
		if err != nil {
			return err
		}
		current := d.MaxGeneration[instance]
		if generation < current {
			return ErrGenerationStale
		}
		if generation > current {
			// A timed-out Docker create can still complete after its client
			// releases the local lock. Do not accept a new generation while an
			// original preparation result is unresolved; local epoch changes
			// cannot cancel an already-dispatched daemon request.
			for _, preparation := range d.Preparations {
				if preparation.Instance == instance && preparation.State != PreparationPrepared {
					return ErrPreparationRecoveryRequired
				}
			}
			d.MaxGeneration[instance] = generation
			for resource, held := range d.Resources {
				if held.Instance == instance && held.Generation < generation {
					d.ReconciliationRequired[resource] = true
				}
			}
			return writeState(s.path, d)
		}
		return nil
	})
}

func (s *Store) HostID() (string, error) {
	var out string
	err := withLock(s.path, func() error {
		d, e := load(s.path)
		if e == nil {
			out = d.HostID
		}
		return e
	})
	return out, err
}

// Reserve persists the maximum generation and exclusive resource ownership.
// A repeated identical request is idempotent; changing its contents conflicts.
// This watermark alone is bookkeeping, not an execution fence. Effect adapters
// must re-check generation while holding the same local state lock. The bounded
// DockerPreparer does that for its create-only operation; Reserve has no effects.
func (s *Store) Reserve(r Request) (Reservation, error) {
	var result Reservation
	if !valid(r.Instance) || !valid(r.Resource) || !valid(r.RequestKey) || !valid(r.PayloadHash) || r.Generation == 0 {
		return result, errors.New("invalid reservation request")
	}
	err := withLock(s.path, func() error {
		d, e := load(s.path)
		if e != nil {
			return e
		}
		reqID := r.Instance + "\x00" + r.RequestKey
		if r.Generation < d.MaxGeneration[r.Instance] {
			return ErrGenerationStale
		}
		if r.Generation > d.MaxGeneration[r.Instance] {
			return ErrGenerationNotAccepted
		}
		candidate := Reservation{Resource: r.Resource, Instance: r.Instance, Generation: r.Generation, RequestKey: r.RequestKey, PayloadHash: r.PayloadHash, State: "held"}
		if old, ok := d.Requests[reqID]; ok {
			if old.Resource != candidate.Resource || old.Instance != candidate.Instance || old.Generation != candidate.Generation || old.RequestKey != candidate.RequestKey || old.PayloadHash != candidate.PayloadHash {
				return ErrResourceConflict
			}
			result = old
			return nil
		}
		if _, ok := d.Resources[r.Resource]; ok {
			return ErrResourceConflict
		}
		d.Requests[reqID] = candidate
		d.Resources[r.Resource] = candidate
		result = candidate
		return writeState(s.path, d)
	})
	return result, err
}

// Resource ownership is strict: a newer generation or different instance cannot
// take over a held resource. Release requires the exact owner and generation. It does not lower
// the persisted generation watermark, so stale requests remain rejected.
func (s *Store) Release(instance string, generation uint64, resource string) error {
	if !valid(instance) || !valid(resource) || generation == 0 {
		return errors.New("invalid release request")
	}
	return withLock(s.path, func() error {
		d, e := load(s.path)
		if e != nil {
			return e
		}
		h, ok := d.Resources[resource]
		if !ok || h.Instance != instance || h.Generation != generation || d.MaxGeneration[instance] != generation {
			return ErrNotOwner
		}
		if _, prepared := d.Preparations[instance+"\x00"+h.RequestKey]; prepared {
			return ErrPreparationRecoveryRequired
		}
		delete(d.Resources, resource)
		h.State = "released"
		d.Requests[instance+"\x00"+h.RequestKey] = h
		return writeState(s.path, d)
	})
}

// ReconcileRelease is a two-phase reconciliation. The injected verifier must
// have a trusted identity and independently verify external reality; neither
// its identity nor a fence against external writers is provided here. It runs
// outside the state lock. Re-checking the local snapshot prevents stale local
// decisions, but does not fence external side effects. Without a real trusted
// verifier and executor-side fencing, this candidate must not be deployed.
func (s *Store) ReconcileRelease(instance string, currentGeneration uint64, resource string, verify func(Reservation) error) error {
	if !valid(instance) || !valid(resource) || currentGeneration == 0 || verify == nil {
		return errors.New("current generation and trusted verifier are required")
	}
	var host string
	var snapshot Reservation
	err := withLock(s.path, func() error {
		d, e := load(s.path)
		if e != nil {
			return e
		}
		if d.MaxGeneration[instance] != currentGeneration {
			return ErrGenerationStale
		}
		held, ok := d.Resources[resource]
		if !ok || held.Instance != instance || held.Generation >= currentGeneration || !d.ReconciliationRequired[resource] {
			return ErrNotOwner
		}
		if _, prepared := d.Preparations[instance+"\x00"+held.RequestKey]; prepared {
			return ErrPreparationRecoveryRequired
		}
		host = d.HostID
		snapshot = held
		return nil
	})
	if err != nil {
		return err
	}
	// No file lock is held while calling external/trusted verification code.
	if err = verify(snapshot); err != nil {
		return fmt.Errorf("reconciliation verification failed: %w", err)
	}
	return withLock(s.path, func() error {
		d, e := load(s.path)
		if e != nil {
			return e
		}
		if d.HostID != host {
			return ErrInvalidState
		}
		if d.MaxGeneration[instance] != currentGeneration {
			return ErrGenerationStale
		}
		held, ok := d.Resources[resource]
		if !ok || held != snapshot || !d.ReconciliationRequired[resource] {
			return ErrNotOwner
		}
		delete(d.Resources, resource)
		delete(d.ReconciliationRequired, resource)
		held.State = "released"
		d.Requests[instance+"\x00"+held.RequestKey] = held
		return writeState(s.path, d)
	})
}

func (s *Store) Query(instance, requestKey string) (Reservation, bool, error) {
	var r Reservation
	var ok bool
	err := withLock(s.path, func() error {
		d, e := load(s.path)
		if e != nil {
			return e
		}
		r, ok = d.Requests[instance+"\x00"+requestKey]
		return nil
	})
	return r, ok, err
}

func valid(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsRune(s, '\x00')
}
func lockPath(path string) string { return path + ".lock" }

func withLock(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := checkDirectory(dir); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_RDWR
	flags |= syscall.O_NOFOLLOW
	f, err := os.OpenFile(lockPath(path), flags, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = checkPrivateRegularFile(f); err != nil {
		return fmt.Errorf("unsafe lock file: %w", err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func checkDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("state directory must be a real directory")
	}
	if info.Mode().Perm()&0022 != 0 {
		return errors.New("state directory must not be group/other writable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return errors.New("state directory must be owned by current service user")
	}
	return nil
}

func checkPrivateRegularFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("file must have no group/other permissions")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return errors.New("file must be owned by current service user")
	}
	return nil
}

func load(path string) (diskState, error) {
	var d diskState
	err := readState(path, &d)
	if err != nil {
		return d, err
	}
	if err = validateState(d); err != nil {
		return d, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	return d, nil
}
func readState(path string, v any) error {
	// Opening a FIFO for read can block before fstat rejects its type.
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = checkPrivateRegularFile(f); e != nil {
		return fmt.Errorf("unsafe state file: %w", e)
	}
	b, e := io.ReadAll(f)
	if e != nil {
		return e
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if e = dec.Decode(v); e != nil {
		return e
	}
	if e = dec.Decode(new(any)); e != io.EOF {
		return errors.New("trailing state data")
	}
	return nil
}
func validateState(d diskState) error {
	if d.SchemaVersion != 1 || !valid(d.HostID) || d.MaxGeneration == nil || d.Requests == nil || d.Resources == nil || d.ReconciliationRequired == nil {
		return errors.New("invalid state header")
	}
	for instance, generation := range d.MaxGeneration {
		if !valid(instance) || generation == 0 {
			return errors.New("invalid maximum generation")
		}
	}
	for key, r := range d.Requests {
		if key != r.Instance+"\x00"+r.RequestKey || !valid(r.Instance) || !valid(r.RequestKey) || !valid(r.Resource) || !valid(r.PayloadHash) || r.Generation == 0 || d.MaxGeneration[r.Instance] < r.Generation || (r.State != "held" && r.State != "released") {
			return errors.New("invalid request record")
		}
		if r.State == "held" {
			if d.Resources[r.Resource] != r {
				return errors.New("held request missing resource index")
			}
		} else if h, ok := d.Resources[r.Resource]; ok && h.Instance == r.Instance && h.RequestKey == r.RequestKey {
			return errors.New("released request still owns resource")
		}
	}
	for resource, r := range d.Resources {
		if resource != r.Resource || r.State != "held" || d.Requests[r.Instance+"\x00"+r.RequestKey] != r {
			return errors.New("invalid resource record")
		}
		if d.MaxGeneration[r.Instance] > r.Generation && !d.ReconciliationRequired[resource] {
			return errors.New("stale reservation missing reconciliation marker")
		}
	}
	for resource, required := range d.ReconciliationRequired {
		held, ok := d.Resources[resource]
		if !required || !ok || held.Generation >= d.MaxGeneration[held.Instance] {
			return errors.New("invalid reconciliation marker")
		}
	}
	return validatePreparations(d)
}
func writeState(path string, d diskState) error {
	b, e := json.MarshalIndent(d, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	tmp, e := os.CreateTemp(filepath.Dir(path), ".agent-state-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(b)
	}
	if e == nil {
		e = tmp.Sync()
	}
	ce := tmp.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}

// Snapshot returns a deterministic read-only view useful to local diagnostics.
func (s *Store) Snapshot() (host string, generations map[string]uint64, reservations []Reservation, err error) {
	err = withLock(s.path, func() error {
		d, e := load(s.path)
		if e != nil {
			return e
		}
		host = d.HostID
		generations = d.MaxGeneration
		for _, r := range d.Resources {
			reservations = append(reservations, r)
		}
		sort.Slice(reservations, func(i, j int) bool { return reservations[i].Resource < reservations[j].Resource })
		return nil
	})
	return
}
