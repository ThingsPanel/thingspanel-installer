//go:build linux

package agent

import (
	"testing"
	"time"
)

func TestRenewOnlyExtendsARunningAcceptedGeneration(t *testing.T) {
	store, _ := initialized(t)
	if err := store.AcceptGeneration("i", 1); err != nil {
		t.Fatal(err)
	}
	a := newActivator(store, nil, nil)
	now := time.Unix(1_000_000, 0)
	a.now = func() time.Time { return now }
	save := func(r *ActivationRecord) {
		d, _ := a.loadLocked()
		d.Instances["i"] = r
		if err := a.saveLocked(d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Renew("i", 1, "k", "g", time.Hour); err != ErrNotGranted {
		t.Fatalf("unknown instance: %v", err)
	}
	save(&ActivationRecord{Instance: "i", Generation: 1, RequestKey: "k", Started: false})
	if _, err := a.Renew("i", 1, "k", "g", time.Hour); err != ErrNotGranted {
		t.Fatalf("never started: %v", err)
	}
	save(&ActivationRecord{Instance: "i", Generation: 1, RequestKey: "k", GrantID: "original-grant", Started: true, LeaseExpires: now.Unix() + 10})
	if _, err := a.Renew("i", 1, "other", "g", time.Hour); err != ErrNotGranted {
		t.Fatalf("other key: %v", err)
	}
	got, err := a.Renew("i", 1, "k", "g2", time.Hour)
	if err != nil || got.LeaseExpires != now.Add(time.Hour).Unix() || got.GrantID != "original-grant" || !got.Started {
		t.Fatalf("%v %+v", err, got)
	}
	save(&ActivationRecord{Instance: "i", Generation: 1, RequestKey: "k", Started: true, LeaseExpires: now.Unix()})
	if _, err := a.Renew("i", 1, "k", "g3", time.Hour); err != ErrLeaseExpired {
		t.Fatalf("expired lease must not be renewed: %v", err)
	}
	save(&ActivationRecord{Instance: "i", Generation: 1, RequestKey: "k", Started: true, Revoked: true})
	if _, err := a.Renew("i", 1, "k", "g4", time.Hour); err != ErrRevoked {
		t.Fatalf("revoked: %v", err)
	}
	if _, err := a.Renew("i", 2, "k", "g4", time.Hour); err == nil {
		t.Fatal("an unaccepted generation must be refused")
	}
}
