//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func initialized(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.json")
	if err := Initialize(p, "host-reviewed-01"); err != nil {
		t.Fatal(err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestFIFOStateIsRejectedWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Open(p); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("FIFO was not rejected: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening FIFO blocked")
	}
}
func req(gen uint64, key, resource, hash string) Request {
	return Request{Instance: "instance-a", Generation: gen, RequestKey: key, Resource: resource, PayloadHash: hash}
}
func reserveAccepted(t *testing.T, s *Store, r Request) (Reservation, error) {
	t.Helper()
	if err := s.AcceptGeneration(r.Instance, r.Generation); err != nil {
		return Reservation{}, err
	}
	return s.Reserve(r)
}

func TestExplicitInitAndFailClosedMissingOrCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if _, err := Open(p); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("missing open: %v", err)
	}
	if err := Initialize(p, "host-1"); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(p, "host-2"); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("reinit: %v", err)
	}
	if err := os.WriteFile(p, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("corrupt open: %v", err)
	}
}
func TestReserveRequiresExplicitGenerationAcceptance(t *testing.T) {
	s, p := initialized(t)
	r := req(5, "unaccepted", "volume:x", "hash")
	if _, err := s.Reserve(r); !errors.Is(err, ErrGenerationNotAccepted) {
		t.Fatalf("unaccepted generation: %v", err)
	}
	_, gens, records, err := s.Snapshot()
	if err != nil || gens["instance-a"] != 0 || len(records) != 0 {
		t.Fatalf("rejected request mutated state: %#v %#v %v", gens, records, err)
	}
	reopened, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	_, gens, records, err = reopened.Snapshot()
	if err != nil || gens["instance-a"] != 0 || len(records) != 0 {
		t.Fatalf("rejected request persisted state: %#v %#v %v", gens, records, err)
	}
}

func TestIdempotencyGenerationAndExclusiveResource(t *testing.T) {
	s, _ := initialized(t)
	a := req(4, "req-1", "docker:service-x", "abc")
	first, err := reserveAccepted(t, s, a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reserveAccepted(t, s, a)
	if err != nil || first != again {
		t.Fatalf("replay %v %v", again, err)
	}
	if _, err = reserveAccepted(t, s, req(4, "req-1", "docker:service-x", "changed")); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("changed request: %v", err)
	}
	if _, err = reserveAccepted(t, s, req(3, "req-2", "docker:other", "def")); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale gen: %v", err)
	}
	if _, err = reserveAccepted(t, s, Request{Instance: "instance-b", Generation: 1, RequestKey: "req-b", Resource: "docker:service-x", PayloadHash: "x"}); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("resource conflict: %v", err)
	}
	if err = s.Release("instance-b", 4, "docker:service-x"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("wrong release: %v", err)
	}
	if err = s.Release("instance-a", 4, "docker:service-x"); err != nil {
		t.Fatal(err)
	}
	if _, err = reserveAccepted(t, s, req(3, "req-3", "docker:service-x", "ghi")); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("watermark lowered: %v", err)
	}
	if _, err = reserveAccepted(t, s, req(5, "req-5", "docker:service-x", "jkl")); err != nil {
		t.Fatalf("new generation after explicit release: %v", err)
	}
}
func TestRejectsAliasOverwriteOldReleaseAndReleasedReplay(t *testing.T) {
	s, _ := initialized(t)
	if _, err := reserveAccepted(t, s, req(1, "first", "volume:shared", "h1")); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveAccepted(t, s, req(1, "alias", "volume:shared", "h2")); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("same owner alias overwrote: %v", err)
	}
	if _, err := reserveAccepted(t, s, req(2, "next", "volume:other", "h3")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("instance-a", 1, "volume:shared"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("old generation released resource: %v", err)
	}
	if err := s.Release("instance-a", 2, "volume:other"); err != nil {
		t.Fatal(err)
	}
	// Old generation is rejected before its prior request can be replayed.
	if _, err := reserveAccepted(t, s, req(1, "first", "volume:shared", "h1")); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale replay accepted: %v", err)
	}
	// Same generation/request remains terminal after release; it cannot claim the resource again.
	if _, err := reserveAccepted(t, s, req(2, "next", "volume:other", "h3")); err != nil {
		t.Fatalf("identical terminal replay: %v", err)
	}
	if _, ok, err := s.Query("instance-a", "next"); err != nil || !ok {
		t.Fatalf("released terminal request lost: %v %v", ok, err)
	}
	if _, err := reserveAccepted(t, s, req(2, "next", "volume:other", "different")); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("released identity reused with changed content: %v", err)
	}
	newOwner := Request{Instance: "instance-b", Generation: 1, RequestKey: "new-request", Resource: "volume:other", PayloadHash: "h4"}
	if _, err := reserveAccepted(t, s, newOwner); err != nil {
		t.Fatalf("released resource should be available to a new request: %v", err)
	}
	// A repeated old terminal request remains released and cannot overwrite the new owner.
	if got, err := reserveAccepted(t, s, req(2, "next", "volume:other", "h3")); err != nil || got.State != "released" {
		t.Fatalf("terminal replay changed state: %#v %v", got, err)
	}
	_, _, reservations, err := s.Snapshot()
	var matched int
	for _, held := range reservations {
		if held.Resource == "volume:other" && held.Instance == "instance-b" && held.RequestKey == "new-request" {
			matched++
		}
	}
	if err != nil || len(reservations) != 2 || matched != 1 {
		t.Fatalf("old replay overwrote new reservation: %#v %v", reservations, err)
	}
}

func TestAcceptedGenerationFencesOldTaskEvenWhenReservationConflicts(t *testing.T) {
	s, _ := initialized(t)
	if _, err := reserveAccepted(t, s, req(1, "old", "volume:held", "h1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveAccepted(t, s, req(2, "new", "volume:held", "h2")); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("expected reservation conflict: %v", err)
	}
	if _, err := reserveAccepted(t, s, req(1, "old", "volume:held", "h1")); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old task not fenced after failed reserve: %v", err)
	}
	if err := s.Release("instance-a", 1, "volume:held"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("old owner released after newer accept: %v", err)
	}
}

func TestReconciliationRequiredNeedsTrustedProof(t *testing.T) {
	s, _ := initialized(t)
	if _, err := reserveAccepted(t, s, req(1, "old", "volume:held", "h1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Query("instance-a", "old"); err != nil || !ok {
		t.Fatalf("old claim must remain inspectable: %v %v", ok, err)
	}
	if err := s.ReconcileRelease("instance-a", 2, "volume:held", nil); err == nil {
		t.Fatal("missing proof accepted")
	}
	denied := errors.New("external owner not confirmed")
	if err := s.ReconcileRelease("instance-a", 2, "volume:held", func(Reservation) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("failed proof accepted: %v", err)
	}
	if _, err := reserveAccepted(t, s, req(2, "new", "volume:held", "h2")); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("resource transferred without proof: %v", err)
	}
	if err := s.ReconcileRelease("instance-a", 2, "volume:held", func(r Reservation) error {
		if r.Generation != 1 {
			return errors.New("wrong stale owner")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveAccepted(t, s, req(2, "new", "volume:held", "h2")); err != nil {
		t.Fatalf("new owner after verified reconciliation: %v", err)
	}
}

func TestReconcileVerifierCanReadSnapshotWithoutDeadlock(t *testing.T) {
	s, _ := initialized(t)
	if _, err := reserveAccepted(t, s, req(1, "old", "volume:verify", "h1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.ReconcileRelease("instance-a", 2, "volume:verify", func(r Reservation) error {
			_, _, _, err := s.Snapshot()
			if err != nil {
				return err
			}
			if r.Generation != 1 {
				return errors.New("unexpected snapshot")
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("verifier Snapshot deadlocked under store lock")
	}
}

func TestReconcileRejectsLocalGenerationOrResourceChangesDuringVerify(t *testing.T) {
	t.Run("generation changed", func(t *testing.T) {
		s, _ := initialized(t)
		if _, err := reserveAccepted(t, s, req(1, "old", "volume:generation", "h1")); err != nil {
			t.Fatal(err)
		}
		if err := s.AcceptGeneration("instance-a", 2); err != nil {
			t.Fatal(err)
		}
		err := s.ReconcileRelease("instance-a", 2, "volume:generation", func(Reservation) error { return s.AcceptGeneration("instance-a", 3) })
		if !errors.Is(err, ErrGenerationStale) {
			t.Fatalf("generation change accepted: %v", err)
		}
	})
	t.Run("resource changed", func(t *testing.T) {
		s, _ := initialized(t)
		if _, err := reserveAccepted(t, s, req(1, "old", "volume:resource", "h1")); err != nil {
			t.Fatal(err)
		}
		if err := s.AcceptGeneration("instance-a", 2); err != nil {
			t.Fatal(err)
		}
		err := s.ReconcileRelease("instance-a", 2, "volume:resource", func(Reservation) error {
			return s.ReconcileRelease("instance-a", 2, "volume:resource", func(Reservation) error { return nil })
		})
		if !errors.Is(err, ErrNotOwner) {
			t.Fatalf("resource change accepted: %v", err)
		}
	})
}

func TestPersistenceAcrossReopen(t *testing.T) {
	s, p := initialized(t)
	if _, err := reserveAccepted(t, s, req(9, "req-9", "port:1234", "hash")); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s2.Query("instance-a", "req-9")
	if err != nil || !ok || got.Generation != 9 {
		t.Fatalf("query after reopen %#v %v %v", got, ok, err)
	}
	_, gens, _, err := s2.Snapshot()
	if err != nil || gens["instance-a"] != 9 {
		t.Fatalf("generation after reopen %v %v", gens, err)
	}
}

func TestLockHelperProcess(t *testing.T) {
	mode := os.Getenv("AGENT_HELPER")
	if mode != "1" && mode != "compete" {
		return
	}
	path := os.Getenv("AGENT_STATE")
	s, e := Open(path)
	if e == nil {
		instance := "instance-a"
		gen := uint64(22)
		key := "same-request"
		resource := "volume:data"
		hash := "same-hash"
		if mode == "compete" {
			instance = os.Getenv("AGENT_INSTANCE")
			gen = 1
			key = "claim-" + instance
			resource = "volume:race"
			hash = "payload-" + instance
		}
		e = s.AcceptGeneration(instance, gen)
		if e == nil {
			_, e = s.Reserve(Request{Instance: instance, Generation: gen, RequestKey: key, Resource: resource, PayloadHash: hash})
		}
	}
	if e == nil {
		os.Exit(0)
	}
	if mode == "compete" && errors.Is(e, ErrResourceConflict) {
		os.Exit(3)
	}
	os.Exit(2)
}

func TestCrossProcessAtomicIdempotentReserve(t *testing.T) {
	_, p := initialized(t)
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
			c.Env = append(os.Environ(), "AGENT_HELPER=1", "AGENT_STATE="+p)
			b, e := c.CombinedOutput()
			if e != nil {
				errs <- fmt.Errorf("helper: %w: %s", e, strings.TrimSpace(string(b)))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	_, ok, e := s.Query("instance-a", "same-request")
	if e != nil || !ok {
		t.Fatalf("missing record after contenders: %v %v", ok, e)
	}
	_, _, rs, e := s.Snapshot()
	if e != nil || len(rs) != 1 {
		t.Fatalf("expected one reservation: %d %v", len(rs), e)
	}
}

func TestHundredConcurrentSameRequestHasOneLogicalResult(t *testing.T) {
	s, _ := initialized(t)
	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reserveAccepted(t, s, req(7, "same", "volume:100", "payload"))
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	_, _, rs, err := s.Snapshot()
	if err != nil || len(rs) != 1 {
		t.Fatalf("expected one logical reservation, got %d: %v", len(rs), err)
	}
}

func TestCrossProcessDifferentInstancesCompeteForOneResource(t *testing.T) {
	_, p := initialized(t)
	const n = 2
	var wg sync.WaitGroup
	results := make(chan error, n)
	for _, instance := range []string{"instance-a", "instance-b"} {
		instance := instance
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
			c.Env = append(os.Environ(), "AGENT_HELPER=compete", "AGENT_STATE="+p, "AGENT_INSTANCE="+instance)
			b, err := c.CombinedOutput()
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 3 {
					results <- ErrResourceConflict
				} else {
					results <- fmt.Errorf("%s: %w (%s)", instance, err, strings.TrimSpace(string(b)))
				}
			} else {
				results <- nil
			}
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrResourceConflict) {
			conflicts++
		} else {
			t.Error(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected one winner and one resource conflict; wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestPrivateStateDirectoryAndFilesAccepted(t *testing.T) {
	s, p := initialized(t)
	if _, err := s.HostID(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(p), p, lockPath(p)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 && path != filepath.Dir(p) {
			t.Fatalf("state file is not private: %s %o", path, info.Mode().Perm())
		}
	}
}

func TestRejectsSymlinkStateAndLockFiles(t *testing.T) {
	t.Run("state symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		real := filepath.Join(dir, "real.json")
		if err := Initialize(real, "host-1"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, path); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("state symlink accepted")
		}
	})
	t.Run("lock symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(""), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, lockPath(path)); err != nil {
			t.Fatal(err)
		}
		if err := Initialize(path, "host-1"); err == nil {
			t.Fatal("lock symlink accepted")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state created through unsafe lock: %v", err)
		}
	})
}

func TestRejectsInsecureDirectoryAndStateFileModes(t *testing.T) {
	t.Run("directory writable by group", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := os.Chmod(dir, 0770); err != nil {
			t.Fatal(err)
		}
		if err := Initialize(path, "host-1"); err == nil {
			t.Fatal("group-writable state directory accepted")
		}
	})
	t.Run("state readable by others", func(t *testing.T) {
		_, path := initialized(t)
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("world-readable state file accepted")
		}
	})
	t.Run("lock readable by group", func(t *testing.T) {
		_, path := initialized(t)
		if err := os.Chmod(lockPath(path), 0640); err != nil {
			t.Fatal(err)
		}
		s, err := Open(path)
		if err == nil {
			t.Fatal("group-readable lock accepted")
		}
		if s != nil {
			t.Fatal("store opened with insecure lock")
		}
	})
}
