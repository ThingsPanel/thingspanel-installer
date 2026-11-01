//go:build linux

package agent

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestOpenRejectsImpossibleGenerationState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*diskState)
	}{
		{"empty instance", func(d *diskState) { d.MaxGeneration[""] = 1 }},
		{"zero generation", func(d *diskState) { d.MaxGeneration["other-instance"] = 0 }},
		{"current generation marked for reconciliation", func(d *diskState) { d.ReconciliationRequired["port:fixture"] = true }},
		{"false reconciliation marker", func(d *diskState) { d.ReconciliationRequired["port:fixture"] = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, path := initialized(t)
			if err := s.AcceptGeneration("instance-a", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reserve(req(1, "fixture-request", "port:fixture", "fixture-hash")); err != nil {
				t.Fatal(err)
			}
			var state diskState
			if err := readState(path, &state); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&state)
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = Open(path); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("inconsistent state must fail closed: %v", err)
			}
		})
	}
}

func TestOpenPreservesValidStaleHoldForReconciliation(t *testing.T) {
	s, path := initialized(t)
	if err := s.AcceptGeneration("instance-a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(req(1, "fixture-request", "port:fixture", "fixture-hash")); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptGeneration("instance-a", 2); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReconcileRelease("instance-a", 2, "port:fixture", func(Reservation) error { return nil }); err != nil {
		t.Fatalf("valid stale hold should remain reconcilable: %v", err)
	}
}
