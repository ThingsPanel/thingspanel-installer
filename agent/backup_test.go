//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	failDump string
	dumps    []string
	noFiles  bool
}

func (f *fakeRunner) ExecStream(_ context.Context, _, _ string, argv []string, w io.Writer) (int, string, error) {
	switch argv[0] {
	case "psql":
		sql := argv[len(argv)-1]
		switch {
		case strings.Contains(sql, "datname from pg_database"):
			io.WriteString(w, "ThingsPanel\ndevice_metadata\n")
		case strings.Contains(sql, "server_version"):
			io.WriteString(w, "14.5\n")
		case strings.Contains(sql, "timescaledb"):
			io.WriteString(w, "2.8.1\n")
		case strings.Contains(sql, "schema_migrations"):
			io.WriteString(w, "39\n")
		case strings.Contains(sql, "sys_version"):
			io.WriteString(w, "25\n")
		case strings.Contains(sql, "pg_database_size"):
			io.WriteString(w, "1000000\n")
		}
	case "pg_dump":
		db := argv[len(argv)-1]
		f.dumps = append(f.dumps, db)
		if db == f.failDump {
			return 1, "boom", nil
		}
		io.WriteString(w, "PGDMP-"+db+strings.Repeat("x", 1000))
	default: // version command
		io.WriteString(w, "v1.4.0\n")
	}
	return 0, "", nil
}

func (f *fakeRunner) CopyFrom(_ context.Context, _, _, _ string, w io.Writer) error {
	if f.noFiles {
		return ErrNoContainer
	}
	tw := tar.NewWriter(w)
	_ = tw.WriteHeader(&tar.Header{Name: "files/a.txt", Size: 3, Mode: 0o600, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("abc"))
	return tw.Close()
}

func backupFixture(t *testing.T) (*BackupService, *fakeRunner, string) {
	t.Helper()
	root := t.TempDir()
	for rel, body := range map[string]string{
		"docker-compose.yml":                 "services: {}\n",
		"niginx.conf":                        "server {}\n",
		"config/native/runtime.encore.json":  "{}\n",
		".env":                               "SECRET=1\n",
		"config/native/broker/services.json": "{\"password\":\"x\"}\n",
	} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		_ = os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600)
	}
	runner := &fakeRunner{}
	cfg := BackupConfig{Dir: filepath.Join(root, "backups"), Root: root, PostgresService: "postgres", PostgresUser: "postgres",
		FilesService: "backend", FilesPath: "/go/src/app/files", VersionService: "backend", VersionArgv: []string{"/app/go", "--version"},
		ConfigFiles: []string{"docker-compose.yml", "niginx.conf", "config/native/runtime.encore.json", "config/native/not-there.json"}}
	svc := &BackupService{Config: cfg, Project: "tp", Runner: runner, Version: "test", FreeBytes: func(string) (uint64, error) { return 1 << 40, nil },
		Adapter: &PostgresAdapter{Runner: runner, Project: "tp", Service: "postgres", User: "postgres"},
		Plugins: func() (json.RawMessage, error) { return json.RawMessage(`{"reservations":[]}`), nil }}
	return svc, runner, root
}

func entries(t *testing.T, p string) map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func TestBackupCreateIsVerifiableAndHasNoSecrets(t *testing.T) {
	svc, runner, _ := backupFixture(t)
	name, err := svc.Create(context.Background(), "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !backupNamePattern.MatchString(name) || !strings.Contains(name, "v1.4.0") {
		t.Fatalf("name %q", name)
	}
	m, err := svc.Verify(name)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if m.Format != 1 || m.Trigger != "manual" || !m.Includes["history"] || m.Includes["secrets"] || m.PlatformVersion != "v1.4.0" || m.Schema.SysVersion != 25 {
		t.Fatalf("%+v", m)
	}
	if m.Stores["relational"].Timescaledb != "2.8.1" || m.Stores["relational"].Server != "14.5" || len(runner.dumps) != 2 {
		t.Fatalf("%+v dumps=%v", m.Stores, runner.dumps)
	}
	got := entries(t, filepath.Join(svc.Config.Dir, name))
	for _, must := range []string{"manifest.json", "db/ThingsPanel.dump", "db/device_metadata.dump", "files/files.tar", "config/docker-compose.yml", "config/niginx.conf", "config/config/native/runtime.encore.json", "plugins/instances.json"} {
		if _, ok := got[must]; !ok {
			t.Errorf("missing %s", must)
		}
	}
	for entry, body := range got {
		if strings.Contains(entry, ".env") || strings.Contains(entry, "services.json") || strings.Contains(body, "SECRET=1") || strings.Contains(body, `"password"`) {
			t.Errorf("secret material in %s", entry)
		}
	}
	list := svc.List()
	if len(list) != 1 || !list[0].Valid || list[0].PlatformVersion != "v1.4.0" || !list[0].History {
		t.Fatalf("%+v", list)
	}
	if fi, _ := os.Stat(svc.Config.Dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("backup dir mode %v", fi.Mode().Perm())
	}
}

// A dump that fails half way must leave nothing that looks like a package, and no partial files.
func TestFailedBackupLeavesNothingBehind(t *testing.T) {
	svc, runner, _ := backupFixture(t)
	runner.failDump = "device_metadata"
	_, err := svc.Create(context.Background(), "manual", nil)
	var ce CodedError
	if !errors.As(err, &ce) || ce.Code != CodeBackupDump {
		t.Fatalf("err=%v", err)
	}
	left, _ := os.ReadDir(svc.Config.Dir)
	if len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// A process killed mid-write leaves *.partial files. They are not listed, not verifiable as a package, and
// the next backup removes them.
func TestStalePartialIsNeverAPackageAndIsCleaned(t *testing.T) {
	svc, _, _ := backupFixture(t)
	_ = os.MkdirAll(svc.Config.Dir, 0o700)
	stale := filepath.Join(svc.Config.Dir, "tp-backup-20261112T030000Z-v1.4.0-full.tar.partial")
	_ = os.WriteFile(stale, []byte("half a tar"), 0o600)
	_ = os.MkdirAll(filepath.Join(svc.Config.Dir, "tp-backup-20261112T030000Z-v1.4.0-full.partial.d"), 0o700)
	if len(svc.List()) != 0 {
		t.Fatal("a partial file was listed")
	}
	if _, err := svc.Verify("tp-backup-20261112T030000Z-v1.4.0-full.tar.partial"); !errors.Is(err, ErrBackupName) {
		t.Fatalf("partial verified: %v", err)
	}
	if _, err := svc.Create(context.Background(), "manual", nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(svc.Config.Dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".partial") {
			t.Fatalf("stale partial survived: %s", e.Name())
		}
	}
}

func TestVerifyRejectsTamperedAndTruncatedPackages(t *testing.T) {
	svc, _, _ := backupFixture(t)
	name, _ := svc.Create(context.Background(), "manual", nil)
	p := filepath.Join(svc.Config.Dir, name)
	raw, _ := os.ReadFile(p)
	flip := bytes.Index(raw, []byte("PGDMP-ThingsPanel"))
	tampered := append([]byte{}, raw...)
	tampered[flip+3] ^= 0xff
	_ = os.WriteFile(p, tampered, 0o600)
	if _, err := svc.Verify(name); err == nil {
		t.Fatal("tampered dump accepted")
	}
	_ = os.WriteFile(p, raw[:len(raw)/2], 0o600)
	if _, err := svc.Verify(name); err == nil {
		t.Fatal("truncated package accepted")
	}
	_ = os.WriteFile(p, raw, 0o600)
	if _, err := svc.Verify(name); err != nil {
		t.Fatalf("restored package rejected: %v", err)
	}
}

func TestSpacePrecheckRefusesBeforeWritingAnything(t *testing.T) {
	svc, _, _ := backupFixture(t)
	svc.FreeBytes = func(string) (uint64, error) { return 1 << 20, nil } // estimate is 1 MB: needs 2 MB + 64 MB
	_, err := svc.Create(context.Background(), "manual", nil)
	var ce CodedError
	if !errors.As(err, &ce) || ce.Code != CodeBackupSpace {
		t.Fatalf("err=%v", err)
	}
	if left, _ := os.ReadDir(svc.Config.Dir); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestRetentionKeepsNewestAutomaticAndAllManual(t *testing.T) {
	svc, _, _ := backupFixture(t)
	svc.Config.KeepAuto = 2
	clock := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	_, _ = svc.Create(context.Background(), "manual", nil)
	var autos []string
	for i := 0; i < 4; i++ {
		n, err := svc.Create(context.Background(), "pre-upgrade", nil)
		if err != nil {
			t.Fatal(err)
		}
		autos = append(autos, n)
	}
	list := svc.List()
	names := map[string]bool{}
	manual := 0
	for _, b := range list {
		names[b.Name] = true
		if b.Trigger == "manual" {
			manual++
		}
	}
	if manual != 1 || len(list) != 3 || !names[autos[2]] || !names[autos[3]] || names[autos[0]] || names[autos[1]] {
		t.Fatalf("list=%+v", list)
	}
}

func TestNamesAreValidatedAndDeleteOnlyTouchesPackages(t *testing.T) {
	svc, _, root := backupFixture(t)
	outside := filepath.Join(root, "precious.txt")
	_ = os.WriteFile(outside, []byte("keep"), 0o600)
	for _, bad := range []string{"../precious.txt", "/etc/passwd", "tp-backup-x.tar", "", "tp-backup-20261112T030000Z-v1-full.tar/../../precious.txt"} {
		if err := svc.Delete(bad); !errors.Is(err, ErrBackupName) {
			t.Errorf("Delete(%q) = %v", bad, err)
		}
		if _, err := svc.Verify(bad); !errors.Is(err, ErrBackupName) {
			t.Errorf("Verify(%q) = %v", bad, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("file outside the backup directory was touched")
	}
	name, _ := svc.Create(context.Background(), "manual", nil)
	if err := svc.Delete(name); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(name); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if len(svc.List()) != 0 {
		t.Fatal("deleted package still listed")
	}
}

func TestBackupConfigRefusesSecretsAndEscapes(t *testing.T) {
	ok := map[string]bool{"postgres": true, "backend": true}
	base := BackupConfig{Dir: "/b", Root: "/r", PostgresService: "postgres", PostgresUser: "postgres", FilesService: "backend", FilesPath: "/f", VersionService: "backend", VersionArgv: []string{"/v"}}
	for _, bad := range []string{".env", "config/native/broker/services.json", "config/native/plugin-token", "../x", "/etc/passwd", "config/native/internal-ca-key.pem", "secrets.json"} {
		c := base
		c.ConfigFiles = []string{bad}
		if c.validate(ok) == nil {
			t.Errorf("config file %q accepted", bad)
		}
	}
	c := base
	c.ConfigFiles = []string{"docker-compose.yml", "config/native/runtime.encore.json"}
	if err := c.validate(ok); err != nil {
		t.Fatal(err)
	}
}

func TestMissingFilesVolumeStillMakesAValidBackup(t *testing.T) {
	svc, runner, _ := backupFixture(t)
	runner.noFiles = true
	name, err := svc.Create(context.Background(), "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(name); err != nil {
		t.Fatal(err)
	}
	if _, ok := entries(t, filepath.Join(svc.Config.Dir, name))["files/files.tar"]; ok {
		t.Fatal("files entry present without a source")
	}
}

func TestDemuxSplitsStdoutAndKeepsStderrTail(t *testing.T) {
	frame := func(kind byte, s string) []byte {
		h := []byte{kind, 0, 0, 0, 0, 0, 0, byte(len(s))}
		return append(h, s...)
	}
	var in bytes.Buffer
	in.Write(frame(1, "hello "))
	in.Write(frame(2, "warn"))
	in.Write(frame(1, "world"))
	var out bytes.Buffer
	tail, err := demux(&in, &out)
	if err != nil || out.String() != "hello world" || tail != "warn" {
		t.Fatalf("%q %q %v", out.String(), tail, err)
	}
}
