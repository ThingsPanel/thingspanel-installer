package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultDataDir  = "/var/lib/thingspanel-notification"
	defaultTemplate = "/run/notification-input/notification-runtime.json.template"
	defaultSystemCA = "/etc/ssl/certs/ca-certificates.crt"
	keyRef          = "installer-v1"
)

type setupOptions struct {
	dataDir       string
	templatePath  string
	systemCAPath  string
	postgresPass  string
	jwtKey        string
	privateCIDR   string
	legacyBaseURL string
}

func main() {
	if err := runFromEnv(); err != nil {
		// Avoid printing paths, environment values, certificate data, or tokens.
		if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
			fmt.Fprintln(os.Stderr, "notification service is not ready")
		} else {
			fmt.Fprintln(os.Stderr, "notification setup failed; verify notification data directory ownership and readable private files")
		}
		os.Exit(1)
	}
}

func runFromEnv() error {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		return checkCoreReadiness()
	}
	if os.Getenv("NOTIFICATION_SETUP_MODE") == "nginx" {
		return installNginx(envOr("NOTIFICATION_SETUP_NGINX_DIR", "/var/lib/nginx"), envOr("NOTIFICATION_SETUP_NGINX_SNIPPET", "/run/notification-input/notification-routes.conf"))
	}
	options := setupOptions{
		dataDir:       envOr("NOTIFICATION_SETUP_DIR", defaultDataDir),
		templatePath:  envOr("NOTIFICATION_SETUP_TEMPLATE", defaultTemplate),
		systemCAPath:  envOr("NOTIFICATION_SETUP_SYSTEM_CA", defaultSystemCA),
		postgresPass:  envOr("POSTGRES_PASSWORD", "postgresThingsPanel"),
		jwtKey:        envOr("TP_JWT_KEY", "1hj5b0sp9"),
		privateCIDR:   envOr("NOTIFICATION_PRIVATE_CIDR", "172.30.254.0/28"),
		legacyBaseURL: envOr("NOTIFICATION_LEGACY_BASE_URL", "https://notification-smtp-tls:8443"),
	}
	return setup(options)
}

func checkCoreReadiness() error {
	return checkCoreReadinessAt("http://127.0.0.1:8080/api/v2/notification-plugins")
}

func checkCoreReadinessAt(endpoint string) error {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("cannot create readiness request")
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Core readiness request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		return errors.New("Core readiness contract not satisfied")
	}
	var body struct {
		Code    int `json:"code"`
		Details struct {
			Reason string `json:"reason"`
		} `json:"details"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || len(data) == 0 || len(data) > 16<<10 || json.Unmarshal(data, &body) != nil || body.Code != http.StatusUnauthorized || body.Details.Reason != "unauthorized" {
		return errors.New("Core readiness response is invalid")
	}
	return nil
}

func setup(options setupOptions) error {
	if options.dataDir == "" || options.postgresPass == "" || strings.ContainsAny(options.postgresPass, "\r\n\x00") || options.jwtKey == "" || strings.ContainsAny(options.jwtKey, "\r\n\x00") || !validCIDR(options.privateCIDR) || !validLegacyBase(options.legacyBaseURL) {
		return errors.New("invalid setup options")
	}
	if err := os.MkdirAll(options.dataDir, 0700); err != nil {
		return errors.New("cannot prepare data directory")
	}
	tlsDir := filepath.Join(options.dataDir, "tls")
	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		return errors.New("cannot prepare TLS directory; check notification data directory ownership")
	}
	if err := ensureTLS(tlsDir, options.systemCAPath); err != nil {
		return err
	}

	pluginToken, err := ensureToken(options.dataDir)
	if err != nil {
		return err
	}
	readerToken, err := ensureSharedToken(options.dataDir)
	if err != nil {
		return err
	}
	keyring, err := ensureKeyring(options.dataDir)
	if err != nil {
		return err
	}
	if err := ensureDBFiles(options); err != nil {
		return err
	}
	deploymentID, err := ensureUUID(filepath.Join(options.dataDir, "source-deployment-id"))
	if err != nil {
		return err
	}
	if err := ensureCoreEnv(options, pluginToken, readerToken, keyring, deploymentID); err != nil {
		return err
	}
	if err := ensureLegacyReaderEnv(options.dataDir, readerToken, deploymentID); err != nil {
		return err
	}
	if err := ensureSMTPEnv(options.dataDir, pluginToken); err != nil {
		return err
	}
	if err := ensureInfra(options); err != nil {
		return err
	}
	return nil
}

func ensureSharedToken(dir string) (string, error) {
	const file = "legacy-reader-token"
	path := filepath.Join(dir, file)
	if b, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(b))
		if len(value) < 32 || strings.ContainsAny(value, " \t\r\n\x00") {
			return "", errors.New("invalid existing directory reader token")
		}
		if err := checkEnvToken(filepath.Join(dir, "core.env"), "NOTIFICATION_LEGACY_READER_TOKEN", value); err != nil {
			return "", errors.New("existing directory reader tokens do not match")
		}
		if err := checkEnvToken(filepath.Join(dir, "legacy-reader.env"), "NOTIFICATION_MEMBER_READER_TOKEN", value); err != nil {
			return "", errors.New("existing directory reader tokens do not match")
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot read directory reader token")
	}
	var existing string
	for _, item := range []struct{ file, key string }{{"core.env", "NOTIFICATION_LEGACY_READER_TOKEN"}, {"legacy-reader.env", "NOTIFICATION_MEMBER_READER_TOKEN"}} {
		value, exists, err := readEnvValue(filepath.Join(dir, item.file), item.key)
		if err != nil {
			return "", err
		}
		if exists {
			if existing != "" && existing != value {
				return "", errors.New("existing directory reader tokens do not match")
			}
			existing = value
		}
	}
	if existing != "" {
		if len(existing) < 32 || strings.ContainsAny(existing, " \t\r\n\x00") {
			return "", errors.New("invalid existing directory reader token")
		}
		if err := createIfAbsent(path, []byte(existing+"\n"), 0600); err != nil {
			return "", err
		}
		return existing, nil
	}
	value, err := randomToken(48)
	if err != nil {
		return "", errors.New("cannot generate directory reader token")
	}
	if err := createIfAbsent(path, []byte(value+"\n"), 0600); err != nil {
		return "", err
	}
	return value, nil
}

func ensureToken(dir string) (string, error) {
	path := filepath.Join(dir, "plugin-token")
	if b, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(b))
		if len(value) < 32 || strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("invalid existing plugin token")
		}
		if err := checkEnvToken(filepath.Join(dir, "core.env"), "NOTIFICATION_DEFAULT_SMTP_PLUGIN_TOKEN", value); err != nil {
			return "", err
		}
		if err := checkEnvToken(filepath.Join(dir, "smtp.env"), "NOTIFICATION_PLUGIN_TOKEN", value); err != nil {
			return "", err
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot read plugin token")
	}
	var existing string
	for _, item := range []struct{ file, key string }{{"core.env", "NOTIFICATION_DEFAULT_SMTP_PLUGIN_TOKEN"}, {"smtp.env", "NOTIFICATION_PLUGIN_TOKEN"}} {
		value, exists, err := readEnvValue(filepath.Join(dir, item.file), item.key)
		if err != nil {
			return "", err
		}
		if exists {
			if existing != "" && existing != value {
				return "", errors.New("existing plugin tokens do not match")
			}
			existing = value
		}
	}
	if existing != "" {
		if len(existing) < 32 || strings.ContainsAny(existing, "\r\n\x00") {
			return "", errors.New("invalid existing plugin token")
		}
		if err := createIfAbsent(path, []byte(existing+"\n"), 0600); err != nil {
			return "", err
		}
		return existing, nil
	}
	value, err := randomToken(48)
	if err != nil {
		return "", errors.New("cannot generate plugin token")
	}
	if err := createIfAbsent(path, []byte(value+"\n"), 0600); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != value {
		return "", errors.New("plugin token changed during setup")
	}
	return value, nil
}

func ensureKeyring(dir string) (string, error) {
	path := filepath.Join(dir, "keyring.json")
	if b, err := os.ReadFile(path); err == nil {
		var keys map[string]string
		if json.Unmarshal(b, &keys) != nil || len(keys[keyRef]) == 0 {
			return "", errors.New("invalid existing keyring")
		}
		key, err := base64.StdEncoding.DecodeString(keys[keyRef])
		if err != nil || len(key) != 32 {
			return "", errors.New("invalid existing keyring")
		}
		return base64.StdEncoding.EncodeToString(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot read keyring")
	}
	if existing, exists, err := readEnvValue(filepath.Join(dir, "core.env"), "NOTIFICATION_KEYRING_B64"); err != nil {
		return "", err
	} else if exists {
		decoded, err := base64.StdEncoding.DecodeString(existing)
		var keys map[string]string
		if err != nil || json.Unmarshal(decoded, &keys) != nil || len(keys[keyRef]) == 0 {
			return "", errors.New("existing Core keyring is invalid")
		}
		key, err := base64.StdEncoding.DecodeString(keys[keyRef])
		if err != nil || len(key) != 32 {
			return "", errors.New("existing Core keyring is invalid")
		}
		if err := createIfAbsent(path, decoded, 0600); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(decoded), nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", errors.New("cannot generate keyring")
	}
	data, err := json.Marshal(map[string]string{keyRef: base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		return "", errors.New("cannot encode keyring")
	}
	if err := createIfAbsent(path, data, 0600); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func ensureDBFiles(options setupOptions) error {
	passForPGPass := strings.ReplaceAll(strings.ReplaceAll(options.postgresPass, `\`, `\\`), `:`, `\:`)
	pgpass := fmt.Sprintf("postgres:5432:notification:postgres:%s\n", passForPGPass)
	if err := ensureExact(filepath.Join(options.dataDir, "db.pgpass"), []byte(pgpass), 0600); err != nil {
		return err
	}
	dbEnv := "POSTGRES_DB=notification\nPOSTGRES_USER=postgres\nPOSTGRES_PASSWORD=" + options.postgresPass + "\n"
	if err := ensureExact(filepath.Join(options.dataDir, "db.env"), []byte(dbEnv), 0600); err != nil {
		return err
	}
	migrateEnv := "PGHOST=postgres\nPGPORT=5432\nPGUSER=postgres\nPGDATABASE=notification\nPGPASSFILE=" + filepath.Join(options.dataDir, "db.pgpass") + "\nNOTIFICATION_MIGRATE_EXPECTED_DB=notification\nNOTIFICATION_MIGRATE_EXPECTED_SCHEMA=public\n"
	if err := ensureExact(filepath.Join(options.dataDir, "migrate.env"), []byte(migrateEnv), 0600); err != nil {
		return err
	}
	return nil
}

func ensureCoreEnv(options setupOptions, token, readerToken, keyring, deploymentID string) error {
	content := strings.Join([]string{
		"NOTIFICATION_KEYRING_B64=" + keyring,
		"NOTIFICATION_ACTIVE_KEY_REF=" + keyRef,
		"NOTIFICATION_LEGACY_BASE_URL=" + options.legacyBaseURL,
		"NOTIFICATION_LEGACY_READER_TOKEN=" + readerToken,
		"NOTIFICATION_FIXTURE_MODE=0",
		"NOTIFICATION_DEFAULT_SMTP_BOOTSTRAP=true",
		"NOTIFICATION_DEFAULT_SMTP_PLUGIN_ORIGIN=https://notification-smtp-tls:8443",
		"NOTIFICATION_DEFAULT_SMTP_PLUGIN_TOKEN=" + token,
		"NOTIFICATION_PLUGIN_ALLOWED_ORIGINS=[\"https://notification-smtp-tls:8443\"]",
		"NOTIFICATION_PLUGIN_ALLOWED_CIDRS=[\"" + options.privateCIDR + "\"]",
		"NOTIFICATION_SOURCE_DEPLOYMENT_ID=" + deploymentID,
		"TP_AUTH_MODE=legacy-jwt",
		"TP_JWT_KEY=" + options.jwtKey,
	}, "\n") + "\n"
	path := filepath.Join(options.dataDir, "core.env")
	if existing, err := os.ReadFile(path); err == nil {
		values, err := parseEnv(existing)
		if err != nil || values["NOTIFICATION_DEFAULT_SMTP_PLUGIN_TOKEN"] != token || values["NOTIFICATION_LEGACY_READER_TOKEN"] != readerToken || values["NOTIFICATION_SOURCE_DEPLOYMENT_ID"] != deploymentID || values["NOTIFICATION_KEYRING_B64"] != keyring || values["NOTIFICATION_ACTIVE_KEY_REF"] != keyRef || values["TP_JWT_KEY"] != options.jwtKey || values["TP_AUTH_MODE"] == "" {
			return errors.New("existing Core runtime configuration is incomplete or inconsistent")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect Core runtime configuration")
	}
	return createIfAbsent(path, []byte(content), 0600)
}

func ensureLegacyReaderEnv(dir, readerToken, deploymentID string) error {
	content := "NOTIFICATION_MEMBER_READER_TOKEN=" + readerToken + "\nNOTIFICATION_MEMBER_READER_DEPLOYMENTS=" + deploymentID + "\nNOTIFICATION_MEMBER_READER_TENANT_IDS=\n"
	path := filepath.Join(dir, "legacy-reader.env")
	if existing, err := os.ReadFile(path); err == nil {
		values, err := parseEnv(existing)
		if err != nil || values["NOTIFICATION_MEMBER_READER_TOKEN"] != readerToken || values["NOTIFICATION_MEMBER_READER_DEPLOYMENTS"] != deploymentID {
			return errors.New("existing legacy reader configuration is inconsistent")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect legacy reader configuration")
	}
	return createIfAbsent(path, []byte(content), 0600)
}

func ensureSMTPEnv(dir, token string) error {
	content := strings.Join([]string{
		"NOTIFICATION_PLUGIN_TOKEN=" + token,
		"NOTIFICATION_SMTP_ALLOWED_SERVERS=",
		"NOTIFICATION_SMTP_ALLOWED_CIDRS=",
		"NOTIFICATION_PLUGIN_UI_ORIGINS=",
		"NOTIFICATION_PLUGIN_LISTEN=0.0.0.0:5005",
	}, "\n") + "\n"
	path := filepath.Join(dir, "smtp.env")
	if existing, err := os.ReadFile(path); err == nil {
		values, err := parseEnv(existing)
		if err != nil || values["NOTIFICATION_PLUGIN_TOKEN"] != token {
			return errors.New("existing SMTP runtime configuration is inconsistent")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect SMTP runtime configuration")
	}
	return createIfAbsent(path, []byte(content), 0600)
}

func ensureInfra(options setupOptions) error {
	data, err := os.ReadFile(options.templatePath)
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return errors.New("notification runtime template is unavailable")
	}
	if err := validateInfraTemplate(data); err != nil {
		return err
	}
	path := filepath.Join(options.dataDir, "infra.json")
	if existing, err := os.ReadFile(path); err == nil {
		if err := validateRuntimeInfra(existing); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot read runtime configuration")
	}
	return createIfAbsent(path, append(data, '\n'), 0600)
}

func validateInfraTemplate(data []byte) error {
	var config map[string]any
	if json.Unmarshal(data, &config) != nil || !hasDatabase(config) || hasTracing(config) {
		return errors.New("invalid notification runtime template")
	}
	return nil
}

func validateRuntimeInfra(data []byte) error {
	var config map[string]any
	if json.Unmarshal(data, &config) != nil || !hasDatabase(config) || hasTracing(config) {
		return errors.New("existing notification runtime configuration is incompatible")
	}
	return nil
}

func hasDatabase(config map[string]any) bool {
	servers, ok := config["sql_servers"].([]any)
	if !ok || len(servers) != 1 {
		return false
	}
	server, ok := servers[0].(map[string]any)
	if !ok || server["host"] != "postgres:5432" {
		return false
	}
	databases, ok := server["databases"].(map[string]any)
	if !ok || len(databases) != 1 {
		return false
	}
	database, ok := databases["notification"].(map[string]any)
	return ok && database["name"] == "notification" && database["username"] == "postgres"
}

func hasTracing(config map[string]any) bool {
	for key := range config {
		if strings.Contains(strings.ToLower(key), "trace") || strings.Contains(strings.ToLower(key), "otel") {
			return true
		}
	}
	return false
}

func ensureTLS(dir, systemCAPath string) error {
	paths := []string{"ca.key", "ca.crt", "server.key", "server.crt"}
	present := 0
	for _, name := range paths {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			present++
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot inspect TLS assets")
		}
	}
	if present == 0 {
		if err := createCertificates(dir); err != nil {
			return err
		}
	} else if present != len(paths) {
		return errors.New("existing TLS assets are incomplete; refusing to replace them")
	}
	if err := validateCertificates(dir); err != nil {
		return err
	}
	systemCA, err := os.ReadFile(systemCAPath)
	if err != nil || len(systemCA) == 0 {
		return errors.New("system CA bundle is unavailable")
	}
	internalCA, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return errors.New("internal CA certificate is unavailable")
	}
	bundle := append(append(systemCA, '\n'), internalCA...)
	bundlePath := filepath.Join(dir, "system-ca-bundle.pem")
	if err := replaceManagedFile(bundlePath, bundle, 0600); err != nil {
		return err
	}
	return nil
}

func createCertificates(dir string) error {
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return errors.New("cannot generate internal CA")
	}
	caTemplate := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "ThingsPanel notification internal CA"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return errors.New("cannot create internal CA certificate")
	}
	caKeyPEM, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return errors.New("cannot encode internal CA key")
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return errors.New("cannot generate TLS key")
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "notification-smtp-tls"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"notification-smtp-tls"}, BasicConstraintsValid: true,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return errors.New("cannot create TLS certificate")
	}
	serverKeyPEM, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return errors.New("cannot encode TLS key")
	}
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"ca.key":     {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyPEM}), 0600},
		"ca.crt":     {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600},
		"server.key": {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyPEM}), 0600},
		"server.crt": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0600},
	}
	for name, file := range files {
		if err := createIfAbsent(filepath.Join(dir, name), file.data, file.mode); err != nil {
			return err
		}
	}
	return nil
}

func validateCertificates(dir string) error {
	readCert := func(name string) (*x509.Certificate, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("invalid certificate")
		}
		return x509.ParseCertificate(block.Bytes)
	}
	ca, err := readCert("ca.crt")
	if err != nil || !ca.IsCA {
		return errors.New("existing internal CA is invalid")
	}
	server, err := readCert("server.crt")
	if err != nil || server.VerifyHostname("notification-smtp-tls") != nil {
		return errors.New("existing TLS certificate is invalid")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "notification-smtp-tls", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return errors.New("existing TLS certificate is not signed by the configured CA")
	}
	for _, pair := range [][2]string{{"ca.key", "ca.crt"}, {"server.key", "server.crt"}} {
		keyBytes, err := os.ReadFile(filepath.Join(dir, pair[0]))
		if err != nil {
			return errors.New("existing TLS key is unavailable")
		}
		block, _ := pem.Decode(keyBytes)
		if block == nil {
			return errors.New("existing TLS key is invalid")
		}
		privateKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return errors.New("existing TLS key is invalid")
		}
		var public any
		switch key := privateKey.(type) {
		case *ecdsa.PrivateKey:
			public = &key.PublicKey
		default:
			return errors.New("unsupported existing TLS key")
		}
		var certificate *x509.Certificate
		if pair[1] == "ca.crt" {
			certificate = ca
		} else {
			certificate = server
		}
		pubDER, _ := x509.MarshalPKIXPublicKey(public)
		certDER, _ := x509.MarshalPKIXPublicKey(certificate.PublicKey)
		if sha256.Sum256(pubDER) != sha256.Sum256(certDER) {
			return errors.New("existing TLS key does not match its certificate")
		}
	}
	return nil
}

func ensureUUID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(b))
		if len(value) != 36 || strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("invalid existing source deployment ID")
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot read source deployment ID")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("cannot generate source deployment ID")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	value := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	if err := createIfAbsent(path, []byte(value+"\n"), 0600); err != nil {
		return "", err
	}
	return value, nil
}

func ensureExact(path string, expected []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(expected) {
			return errors.New("existing private notification configuration conflicts with current inputs")
		}
		if err := os.Chmod(path, mode); err != nil {
			return errors.New("cannot protect existing notification configuration")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect notification configuration")
	}
	return createIfAbsent(path, expected, mode)
}

func createIfAbsent(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot prepare notification asset directory")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".notification-setup-*")
	if err != nil {
		return errors.New("cannot stage notification asset")
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return errors.New("cannot protect notification asset")
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return errors.New("cannot write notification asset")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return errors.New("cannot sync notification asset")
	}
	if err := temp.Close(); err != nil {
		return errors.New("cannot close notification asset")
	}
	if err := os.Link(tempName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return errors.New("cannot install notification asset")
	}
	return nil
}

func replaceManagedFile(path string, data []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		if err := os.Chmod(path, mode); err != nil {
			return errors.New("cannot protect managed notification asset")
		}
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect managed notification asset")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot prepare managed notification asset directory")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".notification-managed-*")
	if err != nil {
		return errors.New("cannot stage managed notification asset")
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return errors.New("cannot protect managed notification asset")
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return errors.New("cannot write managed notification asset")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return errors.New("cannot sync managed notification asset")
	}
	if err := temp.Close(); err != nil {
		return errors.New("cannot close managed notification asset")
	}
	if err := os.Rename(tempName, path); err != nil {
		return errors.New("cannot update managed notification asset")
	}
	return nil
}

func installNginx(configDir, snippetPath string) error {
	configPath := filepath.Join(configDir, "nginx.conf")
	config, err := os.ReadFile(configPath)
	if err != nil || len(config) == 0 || len(config) > 4<<20 {
		return errors.New("active Nginx configuration is unavailable")
	}
	snippet, err := os.ReadFile(snippetPath)
	if err != nil || len(snippet) == 0 || len(snippet) > 64<<10 || !strings.Contains(string(snippet), "notification-core:8080") {
		return errors.New("managed Nginx routes are unavailable")
	}
	const include = "include /etc/nginx/conf.d/notification-routes.locations;"
	confDir := filepath.Join(configDir, "conf.d")
	snippetTarget := filepath.Join(confDir, "notification-routes.locations")
	if strings.Contains(string(config), include) {
		return replaceManagedFile(snippetTarget, snippet, 0644)
	}
	block, err := findNginxServer(string(config))
	if err != nil {
		return err
	}
	patched := make([]byte, 0, len(config)+len(include)+8)
	patched = append(patched, config[:block.close]...)
	patched = append(patched, []byte("\n    "+include+"\n")...)
	patched = append(patched, config[block.close:]...)
	info, err := os.Stat(configPath)
	if err != nil {
		return errors.New("cannot inspect active Nginx configuration")
	}
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return errors.New("cannot prepare Nginx managed configuration directory")
	}
	if err := createIfAbsent(snippetTarget, snippet, 0644); err != nil {
		return err
	}
	backup := configPath + ".notification-backup"
	if err := createIfAbsent(backup, config, info.Mode().Perm()); err != nil {
		return err
	}
	return replaceManagedFile(configPath, patched, info.Mode().Perm())
}

type nginxBlock struct {
	close int
}

func findNginxServer(config string) (nginxBlock, error) {
	var candidates []nginxBlock
	for i := 0; i < len(config); {
		if config[i] == '#' {
			for i < len(config) && config[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(config[i:], "server") && (i == 0 || !isNginxIdent(config[i-1])) && (i+6 == len(config) || !isNginxIdent(config[i+6])) {
			open := i + 6
			for open < len(config) && (config[open] == ' ' || config[open] == '\t' || config[open] == '\r' || config[open] == '\n') {
				open++
			}
			if open < len(config) && config[open] == '{' {
				close := matchingNginxBrace(config, open)
				if close < 0 {
					return nginxBlock{}, errors.New("active Nginx configuration has unbalanced blocks")
				}
				body := config[open+1 : close]
				if hasNginxListen8080(body) {
					candidates = append(candidates, nginxBlock{close: close})
				}
				i = close + 1
				continue
			}
		}
		i++
	}
	if len(candidates) != 1 {
		return nginxBlock{}, errors.New("active Nginx configuration does not have one unambiguous frontend server")
	}
	return candidates[0], nil
}

func matchingNginxBrace(config string, open int) int {
	depth, quote := 1, byte(0)
	for i := open + 1; i < len(config); i++ {
		c := config[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '#' {
			for i < len(config) && config[i] != '\n' {
				i++
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func hasNginxListen8080(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "listen ") {
			fields := strings.Fields(strings.TrimSuffix(line, ";"))
			if len(fields) >= 2 {
				address := strings.Trim(fields[1], "'\"")
				if address == "8080" || strings.HasSuffix(address, ":8080") {
					return true
				}
			}
		}
	}
	return false
}

func isNginxIdent(c byte) bool {
	return c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomSerial() *big.Int {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return serial
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func validCIDR(value string) bool {
	prefix, err := netip.ParsePrefix(value)
	return err == nil && prefix.Bits() > 0 && prefix != netip.MustParsePrefix("0.0.0.0/0") && prefix != netip.MustParsePrefix("::/0")
}

func validLegacyBase(value string) bool {
	return value == "https://notification-smtp-tls:8443"
}

func parseEnv(data []byte) (map[string]string, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \t\x00") || strings.ContainsAny(value, "\x00") {
			return nil, errors.New("invalid environment file")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, errors.New("duplicate environment setting")
		}
		values[key] = value
	}
	return values, nil
}

func readEnvValue(path, key string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, errors.New("cannot read existing runtime settings")
	}
	values, err := parseEnv(data)
	if err != nil {
		return "", false, errors.New("invalid existing runtime settings")
	}
	value, exists := values[key]
	return value, exists, nil
}

func checkEnvToken(path, key, token string) error {
	value, exists, err := readEnvValue(path, key)
	if err != nil || exists && value != token {
		return errors.New("existing plugin tokens do not match")
	}
	return nil
}
