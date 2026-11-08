//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPreparationStateRejectsReceiptIdentityAndOwnershipTampering(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*diskState, string)
	}{
		{"digest", func(d *diskState, key string) {
			r := d.Preparations[key]
			r.PayloadHash = "sha256:" + strings.Repeat("0", 64)
			d.Preparations[key] = r
		}},
		{"profile", func(d *diskState, key string) {
			r := d.Preparations[key]
			r.Profile.Args = []string{"different"}
			d.Preparations[key] = r
		}},
		{"missing-container-id", func(d *diskState, key string) { r := d.Preparations[key]; r.ContainerID = ""; d.Preparations[key] = r }},
		{"reservation-released", func(d *diskState, key string) {
			r := d.Requests[key]
			r.State = "released"
			d.Requests[key] = r
			delete(d.Resources, r.Resource)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, path, preparer, engine, request := preparedFixture(t)
			if result, err := preparer.Prepare(context.Background(), request); err != nil || result.State != PreparationPrepared {
				t.Fatal(err)
			}
			disk, err := load(path)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&disk, request.Instance+"\x00"+request.RequestKey)
			raw, err := json.Marshal(disk)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("corrupt preparation accepted: %v", err)
			}
			if result, err := preparer.Prepare(context.Background(), request); err == nil || result.State == PreparationPrepared || engine.createCount() != 1 {
				t.Fatalf("corrupt preparation reached effects: %#v %v", result, err)
			}
		})
	}
}
