package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupCreatesProtectedAssetsAndPreservesOperatorChanges(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "runtime.template.json")
	if err := os.WriteFile(template, []byte(`{"metadata":{"app_id":"thingspanel"},"sql_servers":[{"host":"postgres:5432","databases":{"notification":{"name":"notification","username":"postgres","password":{"$env":"POSTGRES_PASSWORD"}}}}],"hosted_services":["notification"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "system-ca.pem")
	if err := os.WriteFile(caPath, []byte("system roots"), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "state")
	options := setupOptions{dataDir: dataDir, templatePath: template, systemCAPath: caPath, postgresPass: "db-secret-fixture", jwtKey: "jwt-fixture-secret", privateCIDR: "172.30.254.0/28", legacyBaseURL: "https://notification-smtp-tls:8443"}
	if err := setup(options); err != nil {
		t.Fatal(err)
	}

	tokenBefore, err := os.ReadFile(filepath.Join(dataDir, "plugin-token"))
	if err != nil {
		t.Fatal(err)
	}
	keyringBefore, err := os.ReadFile(filepath.Join(dataDir, "keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile(filepath.Join(dataDir, "tls", "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	operatorSMTP := strings.Replace(string(mustRead(t, filepath.Join(dataDir, "smtp.env"))), "NOTIFICATION_SMTP_ALLOWED_SERVERS=", "NOTIFICATION_SMTP_ALLOWED_SERVERS=mail.operator.example:465", 1)
	if err := os.WriteFile(filepath.Join(dataDir, "smtp.env"), []byte(operatorSMTP), 0600); err != nil {
		t.Fatal(err)
	}
	operatorCore := strings.Replace(string(mustRead(t, filepath.Join(dataDir, "core.env"))), `NOTIFICATION_PLUGIN_ALLOWED_ORIGINS=["https://notification-smtp-tls:8443"]`, `NOTIFICATION_PLUGIN_ALLOWED_ORIGINS=["https://notification-smtp-tls:8443","https://operator-plugin.example"]`, 1)
	if err := os.WriteFile(filepath.Join(dataDir, "core.env"), []byte(operatorCore), 0600); err != nil {
		t.Fatal(err)
	}
	readerEnvPath := filepath.Join(dataDir, "legacy-reader.env")
	readerEnv := strings.Replace(string(mustRead(t, readerEnvPath)), "NOTIFICATION_MEMBER_READER_TENANT_IDS=\n", "NOTIFICATION_MEMBER_READER_TENANT_IDS=tenant-configured-later\n", 1)
	if err := os.WriteFile(readerEnvPath, []byte(readerEnv), 0600); err != nil {
		t.Fatal(err)
	}

	if err := setup(options); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dataDir, "plugin-token")); string(got) != string(tokenBefore) {
		t.Fatal("setup changed the persistent plugin token")
	}
	if got := mustRead(t, filepath.Join(dataDir, "keyring.json")); string(got) != string(keyringBefore) {
		t.Fatal("setup changed the persistent keyring")
	}
	if got := mustRead(t, filepath.Join(dataDir, "tls", "server.crt")); string(got) != string(certBefore) {
		t.Fatal("setup changed the persistent TLS certificate")
	}
	if got := string(mustRead(t, filepath.Join(dataDir, "smtp.env"))); got != operatorSMTP {
		t.Fatal("setup overwrote the operator SMTP allowlist")
	}
	if got := string(mustRead(t, filepath.Join(dataDir, "core.env"))); got != operatorCore {
		t.Fatal("setup overwrote the operator plugin-origin allowlist")
	}
	if got := string(mustRead(t, readerEnvPath)); got != readerEnv {
		t.Fatal("setup overwrote the operator reader tenant scope")
	}
	readerToken := strings.TrimSpace(string(mustRead(t, filepath.Join(dataDir, "legacy-reader-token"))))
	if values, err := parseEnv(mustRead(t, filepath.Join(dataDir, "core.env"))); err != nil || values["NOTIFICATION_LEGACY_READER_TOKEN"] != readerToken {
		t.Fatal("Core reader token does not match the persistent token")
	}
	if values, err := parseEnv(mustRead(t, readerEnvPath)); err != nil || values["NOTIFICATION_MEMBER_READER_TOKEN"] != readerToken || values["NOTIFICATION_MEMBER_READER_TENANT_IDS"] != "tenant-configured-later" {
		t.Fatal("legacy reader token/scope configuration is inconsistent")
	}
	for _, path := range []string{"core.env", "legacy-reader.env", "db.env", "migrate.env", "db.pgpass", "plugin-token", "legacy-reader-token", "keyring.json", "source-deployment-id", "infra.json", "tls/ca.key", "tls/server.key", "tls/system-ca-bundle.pem"} {
		info, err := os.Stat(filepath.Join(dataDir, path))
		if err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode=%o, want 600", path, info.Mode().Perm())
		}
	}
}

func TestSetupFailsClosedOnTokenMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "core.env"), []byte("NOTIFICATION_DEFAULT_SMTP_PLUGIN_TOKEN=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "smtp.env"), []byte("NOTIFICATION_PLUGIN_TOKEN=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureToken(dir); err == nil {
		t.Fatal("mismatched persistent plugin tokens must fail closed")
	}
}

func TestCreateIfAbsentDoesNotReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := createIfAbsent(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := createIfAbsent(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); got != "first" {
		t.Fatalf("value=%q, expected create-if-absent behavior", got)
	}
}

func TestInstallNginxAddsServerIncludeAndUpdatesManagedSnippet(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "nginx")
	if err := os.MkdirAll(filepath.Join(configDir, "conf.d"), 0755); err != nil {
		t.Fatal(err)
	}
	config := `http {
    include /etc/nginx/conf.d/*.conf;
    server {
        listen 8080;
        location /api { proxy_pass http://backend:9999; }
    }
}`
	configPath := filepath.Join(configDir, "nginx.conf")
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	sourceSnippet := filepath.Join(root, "routes.conf")
	snippet := []byte("location /api/v2/notification-plugins { proxy_pass http://notification-core:8080; }\n")
	if err := os.WriteFile(sourceSnippet, snippet, 0600); err != nil {
		t.Fatal(err)
	}
	if err := installNginx(configDir, sourceSnippet); err != nil {
		t.Fatal(err)
	}
	gotConfig := string(mustRead(t, configPath))
	if !strings.Contains(gotConfig, "include /etc/nginx/conf.d/notification-routes.locations;") {
		t.Fatal("server block does not include managed route snippet")
	}
	managedPath := filepath.Join(configDir, "conf.d", "notification-routes.locations")
	if got := string(mustRead(t, managedPath)); got != string(snippet) {
		t.Fatalf("managed snippet=%q", got)
	}
	if _, err := os.Stat(filepath.Join(configDir, "conf.d", "notification-routes.conf")); !os.IsNotExist(err) {
		t.Fatal("managed location snippet must not match global conf.d/*.conf include")
	}
	updated := []byte("location /api/v2/notifications { proxy_pass http://notification-core:8080; }\n")
	if err := os.WriteFile(sourceSnippet, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if err := installNginx(configDir, sourceSnippet); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, managedPath)); got != string(updated) {
		t.Fatalf("managed snippet did not update atomically: %q", got)
	}
	if got := string(mustRead(t, configPath)); strings.Count(got, "notification-routes.locations") != 1 {
		t.Fatal("repeated setup duplicated the managed include")
	}
}

func TestCoreReadinessRequiresReadyUnauthenticatedContractAndDoesNotFollowRedirects(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/notification-plugins" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected readiness request: method=%s path=%s authorization-present=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"details":{"reason":"unauthorized"}}`))
	}))
	defer ready.Close()
	if err := checkCoreReadinessAt(ready.URL + "/api/v2/notification-plugins"); err != nil {
		t.Fatalf("expected authenticated handler readiness contract: %v", err)
	}

	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":503,"details":{"reason":"notification_unavailable"}}`))
	}))
	defer unavailable.Close()
	if err := checkCoreReadinessAt(unavailable.URL); err == nil {
		t.Fatal("503 before the privacy gate must not pass readiness")
	}

	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"details":{"reason":"unauthorized"}}`))
	}))
	defer redirectTarget.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if err := checkCoreReadinessAt(redirect.URL); err == nil {
		t.Fatal("readiness must not follow a redirect to another endpoint")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
