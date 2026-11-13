//go:build linux

package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// missingImageEngine reports every image as present except the ones listed.
type missingImageEngine struct {
	*preparationFixtureEngine
	missing map[string]bool
}

func (e missingImageEngine) inspectImage(ctx context.Context, id string) (dockerImage, error) {
	if e.missing[id] {
		return dockerImage{}, os.ErrNotExist
	}
	return e.preparationFixtureEngine.inspectImage(ctx, id)
}

// The control plane must be able to tell whether the image an upgrade needs is on the host before it revokes the
// running generation.
func TestCatalogListReportsWhetherEachImageIsPresent(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	root := filepath.Join(dir, "root.pem")
	os.WriteFile(root, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
	have, lack := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	entry := func(id, image string) map[string]any {
		return map[string]any{"id": id, "version": "1.0.0", "image_id": image, "entrypoint": "/bin/x",
			"platform": map[string]any{"binding": "none", "service_identifier": id, "http_port": 9001}}
	}
	raw, _ := json.Marshal(map[string]any{"schema": catalogSchema, "expires_at": time.Now().Add(time.Hour).Unix(),
		"plugins": []any{entry("present", have), entry("absent", lack)}})
	cat := filepath.Join(dir, "plugin-catalog.json")
	sum := sha256.Sum256(raw)
	sig, _ := ecdsa.SignASN1(rand.Reader, key, sum[:])
	os.WriteFile(cat, raw, 0o644)
	os.WriteFile(cat+".sig", sig, 0o644)
	src, err := NewCatalogSource(cat, root)
	if err != nil {
		t.Fatal(err)
	}
	engine := missingImageEngine{newPreparationFixtureEngine(), map[string]bool{lack: true}}
	statePath := filepath.Join(dir, "state.json")
	if err := Initialize(statePath, "host-reviewed-01"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := newDockerPreparer(store, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{preparer: preparer, catalog: src}
	resp, err := svc.listCatalog(context.Background(), apiRequest{})
	if err != nil || len(resp.Catalog) != 2 {
		t.Fatalf("%v %+v", err, resp)
	}
	got := map[string]*bool{}
	for _, e := range resp.Catalog {
		got[e.ID] = e.ImagePresent
	}
	if got["present"] == nil || !*got["present"] || got["absent"] == nil || *got["absent"] {
		t.Fatalf("image presence misreported: %v %v", got["present"], got["absent"])
	}
}
