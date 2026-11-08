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

const (
	testSecret    = "fixture-secret-value"
	identityToken = "pri_0123456789abcdef.0123456789abcdef0123456789abcdef0123456789abcdef"
	mqttPass      = "mqtt-pass-123"
)

func catalogJSON(expires int64) []byte {
	doc := map[string]any{"schema": catalogSchema, "version": 1, "expires_at": expires, "plugins": []any{map[string]any{
		"id": "homeassistant", "version": "1.0.0", "kind": "service", "image_id": "sha256:" + strings.Repeat("a", 64),
		"entrypoint": "/usr/local/bin/homeassistant-service", "env": []string{"LOG_LEVEL=info"},
		"env_from_stack": map[string]string{"TP_MQTT_PASS": "TP_PROTOCOL_MQTT_PASSWORD"},
		"env_user": map[string]any{
			"SITE_BASE_URL":      map[string]any{"required": true, "secret": false, "validate": "url"},
			"SITE_ACCESS_SECRET": map[string]any{"required": true, "secret": true},
		},
		"health":   map[string]any{"port": 9001, "path": "/health"},
		"identity": map[string]any{"token_env": "TP_PLUGIN_RUNTIME_TOKEN"},
		"platform": map[string]any{"binding": "none", "service_identifier": "homeassistant", "http_port": 9001,
			"service_plugin": map[string]any{"name": "Home Assistant", "service_type": 2, "config": map[string]any{"http_address": "{alias}:{http_port}"}}},
	}}}
	raw, _ := json.Marshal(doc)
	return raw
}

func signedCatalog(t *testing.T, expires int64) (catalog, root string, key *ecdsa.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	root = filepath.Join(dir, "root.pem")
	os.WriteFile(root, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600)
	catalog = filepath.Join(dir, "plugin-catalog.json")
	raw := catalogJSON(expires)
	digest := sha256.Sum256(raw)
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	os.WriteFile(catalog, raw, 0600)
	os.WriteFile(catalog+".sig", sig, 0600)
	return
}

func TestCatalogVerifiesSignatureExpiryAndTampering(t *testing.T) {
	catalog, root, _ := signedCatalog(t, time.Now().Add(time.Hour).Unix())
	source, err := NewCatalogSource(catalog, root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := source.Entries()
	if err != nil || len(entries) != 1 || entries[0].Public().DisplayName != "Home Assistant" || entries[0].Public().NetworkAlias != "tp-plugin-homeassistant" {
		t.Fatalf("%v %+v", err, entries)
	}
	raw, _ := os.ReadFile(catalog)
	os.WriteFile(catalog, []byte(strings.Replace(string(raw), "1.0.0", "9.9.9", 1)), 0600)
	if _, err := source.Entries(); err != ErrCatalog {
		t.Fatalf("tampered catalog accepted: %v", err)
	}
	expired, root2, _ := signedCatalog(t, time.Now().Add(-time.Hour).Unix())
	s2, _ := NewCatalogSource(expired, root2)
	if _, err := s2.Entries(); err != ErrCatalog {
		t.Fatalf("expired catalog accepted: %v", err)
	}
	other, _, _ := signedCatalog(t, time.Now().Add(time.Hour).Unix())
	s3, _ := NewCatalogSource(other, root) // signed by a different key than the pinned root
	if _, err := s3.Entries(); err != ErrCatalog {
		t.Fatalf("catalog signed by another key accepted: %v", err)
	}
}

func catalogFixture(t *testing.T) (*CatalogSource, *SecretStore, ProfileBuilder) {
	catalog, root, _ := signedCatalog(t, time.Now().Add(time.Hour).Unix())
	source, err := NewCatalogSource(catalog, root)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := OpenSecretStore(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	return source, secrets, ProfileBuilder{Secrets: secrets, Network: "tp_net", StackEnv: func() (map[string]string, error) {
		return map[string]string{"TP_PROTOCOL_MQTT_PASSWORD": mqttPass}, nil
	}}
}

func TestCatalogInstallKeepsSecretsOutOfStateAndInjectsThemIntoTheContainer(t *testing.T) {
	source, secrets, builder := catalogFixture(t)
	entry, err := source.Entry("homeassistant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(entry, "homeassistant", 1, map[string]string{"SITE_BASE_URL": "http://site:8123"}, identityToken); err == nil {
		t.Fatal("a required secret that was never stored must fail the build")
	}
	if err := secrets.Put("homeassistant", "config/SITE_ACCESS_SECRET", testSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(entry, "homeassistant", 1, map[string]string{"SITE_BASE_URL": "ftp://x"}, identityToken); err == nil {
		t.Fatal("a bad URL must be refused")
	}
	profile, err := builder.Build(entry, "homeassistant", 1, map[string]string{"SITE_BASE_URL": "http://site:8123"}, identityToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range profile.Env {
		if strings.Contains(e, testSecret) || strings.Contains(e, identityToken) || strings.Contains(e, mqttPass) {
			t.Fatalf("profile carries a secret: %s", e)
		}
	}

	store, statePath := initialized(t)
	if err := store.AcceptGeneration("homeassistant", 1); err != nil {
		t.Fatal(err)
	}
	engine := newPreparationFixtureEngine()
	preparer, err := newDockerPreparer(store, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	preparer.SetSecrets(secrets)
	if err := preparer.SetProfile(profile); err != nil {
		t.Fatal(err)
	}
	obs, err := preparer.Prepare(context.Background(), PrepareContainerRequest{Instance: "homeassistant", Generation: 1, RequestKey: "install-1", Profile: profile.Name})
	if err != nil || obs.State != PreparationPrepared {
		t.Fatalf("%v %+v", err, obs)
	}
	var env []string
	for _, c := range engine.containers {
		env = c.Config.Env
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"SITE_ACCESS_SECRET=" + testSecret, "TP_PLUGIN_RUNTIME_TOKEN=" + identityToken, "TP_MQTT_PASS=" + mqttPass, "SITE_BASE_URL=http://site:8123", "TP_PLUGIN_SERVICE_IDENTIFIER=homeassistant"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("container env lacks %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	// nothing secret in the Agent's journal or its receipts
	files, _ := filepath.Glob(statePath + "*")
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		for _, s := range []string{testSecret, identityToken, mqttPass} {
			if strings.Contains(string(raw), s) {
				t.Fatalf("%s contains a secret", filepath.Base(f))
			}
		}
	}
	mode, _ := os.Stat(secrets.path)
	if mode.Mode().Perm() != 0600 {
		t.Fatalf("secret store mode %v", mode.Mode().Perm())
	}
	// repeating the prepare is the same operation, not a second container
	if _, err := preparer.Prepare(context.Background(), PrepareContainerRequest{Instance: "homeassistant", Generation: 1, RequestKey: "install-1", Profile: profile.Name}); err != nil || engine.createCount() != 1 {
		t.Fatalf("%v creates=%d", err, engine.createCount())
	}
	// after an Agent restart the recorded profile is adopted only because the signed catalog vouches for it
	again, err := newDockerPreparer(store, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	again.SetSecrets(secrets)
	if _, err := again.Query(context.Background(), "homeassistant", 1, "install-1"); err == nil {
		t.Fatal("an unknown profile must not be trusted before adoption")
	}
	if err := again.AdoptRecorded(func(p PreparationProfile) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Query(context.Background(), "homeassistant", 1, "install-1"); err == nil {
		t.Fatal("a profile the catalog does not vouch for must stay untrusted")
	}
	if err := again.AdoptRecorded(func(p PreparationProfile) bool { return p.ImageID == entry.ImageID && p.Entrypoint == entry.Entrypoint }); err != nil {
		t.Fatal(err)
	}
	if obs, err := again.Query(context.Background(), "homeassistant", 1, "install-1"); err != nil || obs.State != PreparationPrepared {
		t.Fatalf("%v %+v", err, obs)
	}
}

func TestConfigEndpointsOnlyAcceptDeclaredSecrets(t *testing.T) {
	source, secrets, builder := catalogFixture(t)
	s := &Service{catalog: source, builder: builder}
	ctx := context.Background()
	for _, bad := range []apiRequest{
		{Instance: "homeassistant", Key: "SITE_BASE_URL", Value: "x"},       // declared but not secret
		{Instance: "homeassistant", Key: "PATH", Value: "x"},                // undeclared
		{Instance: "unknown-plugin", Key: "SITE_ACCESS_SECRET", Value: "x"}, // not in the catalog
		{Instance: "homeassistant", Key: "SITE_ACCESS_SECRET", Value: "a\nb"},
		{Instance: "homeassistant", Key: "SITE_ACCESS_SECRET", Value: ""},
	} {
		if _, err := s.configPut(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := s.configPut(ctx, apiRequest{Instance: "homeassistant", Key: "SITE_ACCESS_SECRET", Value: testSecret}); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.configKeys(ctx, apiRequest{Instance: "homeassistant"})
	raw, _ := json.Marshal(keys)
	if string(raw) != `{"ok":false,"keys":["SITE_ACCESS_SECRET"]}` || strings.Contains(string(raw), testSecret) {
		t.Fatalf("%s", raw)
	}
	secrets.Put("homeassistant", "identity/3", identityToken)
	if _, err := s.configClear(ctx, apiRequest{Instance: "homeassistant"}); err != nil {
		t.Fatal(err)
	}
	if k := secrets.Keys("homeassistant", ""); len(k) != 0 {
		t.Fatalf("left behind: %v", k)
	}
}
