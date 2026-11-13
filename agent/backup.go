//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Backup: a full backup is one tar holding the relational databases (pg_dump -Fc, TimescaleDB history
// included), the allowlisted configuration files, the uploaded-files volume, and the plugin instance list.
// Secrets (.env, broker tokens, services.json, Agent secrets.json) are never part of it (decision D5).
//
// A package is written as <name>.tar.partial and renamed to <name>.tar only after it has been fsynced, so a
// process killed at any moment leaves only *.partial leftovers, never something that looks like a package.

const (
	backupFormat    = 1
	manifestName    = "manifest.json"
	backupNameRegex = `^tp-backup-[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9._-]{1,40}-full\.tar$`
)

var (
	backupNamePattern = regexp.MustCompile(backupNameRegex)
	databaseName      = regexp.MustCompile(`^[A-Za-z0-9_]{1,63}$`)

	ErrBackupName     = errors.New("invalid backup name")
	ErrBackupNotFound = errors.New("backup not found")
)

// Codes for backup failures.
const (
	CodeBackupSpace  = "insufficient_space"
	CodeBackupStore  = "database_unreadable"
	CodeBackupDump   = "dump_failed"
	CodeBackupFiles  = "files_copy_failed"
	CodeBackupConfig = "config_unreadable"
	CodeBackupWrite  = "write_failed"
	CodeBackupVerify = "verify_failed"
	StepDumping      = "dumping"
	StepCollecting   = "collecting"
	StepPackaging    = "packaging"
	StepRetention    = "retention"
)

// BackupConfig is part of the trusted system config.
type BackupConfig struct {
	Dir             string   `json:"dir"`              // absolute; created 0700
	Root            string   `json:"root"`             // installer root, for ConfigFiles
	PostgresService string   `json:"postgres_service"` // compose service running PostgreSQL
	PostgresUser    string   `json:"postgres_user"`
	FilesService    string   `json:"files_service"` // compose service whose container holds the uploads
	FilesPath       string   `json:"files_path"`    // absolute path inside it
	ConfigFiles     []string `json:"config_files"`  // relative to Root; allowlist, never secrets
	VersionService  string   `json:"version_service"`
	VersionArgv     []string `json:"version_argv"` // prints the platform version
	KeepAuto        int      `json:"keep_auto"`    // automatic snapshots kept (default 3)
}

var secretNames = regexp.MustCompile(`(^|/)(\.env[^/]*|plugin-token|owner-tokens\.json|services\.json|secrets\.json|.*\.key|.*\.pem|.*token.*)$`)

func (c *BackupConfig) validate(components map[string]bool) error {
	if !filepath.IsAbs(c.Dir) || !filepath.IsAbs(c.Root) || !filepath.IsAbs(c.FilesPath) ||
		!components[c.PostgresService] || !components[c.FilesService] || !components[c.VersionService] ||
		c.PostgresUser == "" || len(c.VersionArgv) == 0 || !strings.HasPrefix(c.VersionArgv[0], "/") || c.KeepAuto < 0 || c.KeepAuto > 50 {
		return ErrSystemConfig
	}
	for _, f := range c.ConfigFiles {
		clean := path.Clean(f)
		if clean != f || strings.HasPrefix(f, "/") || strings.HasPrefix(f, "..") || secretNames.MatchString(f) {
			return ErrSystemConfig
		}
	}
	return nil
}

// ---- adapter ----------------------------------------------------------------------------------------------

// StoreInfo describes one database server for the manifest.
type StoreInfo struct {
	Adapter     string            `json:"adapter"`
	Server      string            `json:"server"`
	Timescaledb string            `json:"timescaledb,omitempty"`
	Databases   []string          `json:"databases"`
	SysVersion  int               `json:"sys_version"`
	Migrations  map[string]string `json:"migrations,omitempty"`
}

// BackupAdapter is how a database kind is dumped. Only PostgreSQL exists in phase one (A3).
type BackupAdapter interface {
	Name() string
	Describe(ctx context.Context) (StoreInfo, error)
	Estimate(ctx context.Context) (int64, error)
	Dump(ctx context.Context, database string, w io.Writer) error
}

// StreamRunner runs a fixed command inside a compose service and copies its stdout.
type StreamRunner interface {
	ExecStream(ctx context.Context, project, service string, argv []string, stdout io.Writer) (int, string, error)
	CopyFrom(ctx context.Context, project, service, path string, w io.Writer) error
	// CopyTo extracts the tar stream r into dir inside the container (it may be stopped), like `docker cp - c:dir`.
	CopyTo(ctx context.Context, project, service, dir string, r io.Reader) error
}

// PostgresAdapter dumps with pg_dump inside the PostgreSQL container, so the client version always matches
// the server.
type PostgresAdapter struct {
	Runner  StreamRunner
	Project string
	Service string
	User    string
}

func (p *PostgresAdapter) Name() string { return "postgres" }

func (p *PostgresAdapter) query(ctx context.Context, database, sql string) (string, error) {
	var out bytes.Buffer
	code, _, err := p.Runner.ExecStream(ctx, p.Project, p.Service, []string{"psql", "-U", p.User, "-d", database, "-At", "-c", sql}, &out)
	if err != nil || code != 0 {
		return "", errors.New("query failed")
	}
	return strings.TrimSpace(out.String()), nil
}

func (p *PostgresAdapter) databases(ctx context.Context) ([]string, error) {
	out, err := p.query(ctx, "postgres", "select datname from pg_database where not datistemplate and datname <> 'postgres' order by 1")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(out, "\n") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !databaseName.MatchString(n) {
			return nil, errors.New("unsupported database name")
		}
		names = append(names, n)
	}
	return names, nil
}

func (p *PostgresAdapter) Describe(ctx context.Context) (StoreInfo, error) {
	info := StoreInfo{Adapter: "postgres", Migrations: map[string]string{}}
	var err error
	if info.Databases, err = p.databases(ctx); err != nil || len(info.Databases) == 0 {
		return info, errors.New("cannot list databases")
	}
	if info.Server, err = p.query(ctx, "postgres", "show server_version"); err != nil {
		return info, err
	}
	for _, db := range info.Databases {
		if v, err := p.query(ctx, db, "select extversion from pg_extension where extname='timescaledb'"); err == nil && v != "" && info.Timescaledb == "" {
			info.Timescaledb = v
		}
		// Encore databases keep their version in schema_migrations; a database without that table is recorded as absent.
		if v, err := p.query(ctx, db, "select coalesce(max(version)::text,'') from schema_migrations"); err == nil && v != "" {
			info.Migrations[db] = v
		}
		if v, err := p.query(ctx, db, "select version_number from sys_version order by version_number desc limit 1"); err == nil && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				info.SysVersion = n
			}
		}
	}
	// A freshly initialised server has no extension installed yet: what matters for a restore is the version it would install.
	if info.Timescaledb == "" {
		if v, err := p.query(ctx, "postgres", "select coalesce(default_version,'') from pg_available_extensions where name='timescaledb'"); err == nil {
			info.Timescaledb = v
		}
	}
	return info, nil
}

func (p *PostgresAdapter) Estimate(ctx context.Context) (int64, error) {
	out, err := p.query(ctx, "postgres", "select coalesce(sum(pg_database_size(datname)),0) from pg_database where not datistemplate")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out, 10, 64)
}

func (p *PostgresAdapter) Dump(ctx context.Context, database string, w io.Writer) error {
	if !databaseName.MatchString(database) {
		return errors.New("unsupported database name")
	}
	code, _, err := p.Runner.ExecStream(ctx, p.Project, p.Service, []string{"pg_dump", "-U", p.User, "-Fc", "-d", database}, w)
	if err != nil || code != 0 {
		return errors.New("pg_dump failed")
	}
	return nil
}

// ---- manifest ---------------------------------------------------------------------------------------------

type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Format          int                  `json:"format"`
	PlatformVersion string               `json:"platform_version"`
	AgentVersion    string               `json:"agent_version"`
	Kind            string               `json:"kind"`
	CreatedAt       time.Time            `json:"created_at"`
	Trigger         string               `json:"trigger"`
	Schema          ManifestSchema       `json:"schema"`
	Stores          map[string]StoreInfo `json:"stores"`
	Plugins         json.RawMessage      `json:"plugins,omitempty"`
	Includes        map[string]bool      `json:"includes"`
	Encrypted       bool                 `json:"encrypted"`
	Files           []ManifestFile       `json:"files"`
}

type ManifestSchema struct {
	SysVersion int               `json:"sys_version"`
	Databases  map[string]string `json:"databases,omitempty"`
}

// BackupInfo is one row of the list.
type BackupInfo struct {
	Name            string    `json:"name"`
	Size            int64     `json:"size"`
	CreatedAt       time.Time `json:"created_at"`
	PlatformVersion string    `json:"platform_version"`
	Trigger         string    `json:"trigger"`
	History         bool      `json:"history"`
	Valid           bool      `json:"valid"` // manifest readable; Verify checks the content
}

// ---- service ----------------------------------------------------------------------------------------------

// BackupService creates, lists, verifies and deletes backup packages.
type BackupService struct {
	Config  BackupConfig
	Project string
	Adapter BackupAdapter
	Runner  StreamRunner
	// Plugins returns the plugin instance list (no secrets) for plugins/instances.json; nil to skip.
	Plugins func() (json.RawMessage, error)
	Version string // agent version
	Now     func() time.Time
	// FreeBytes reports free space of the filesystem holding path; replaceable in tests.
	FreeBytes func(path string) (uint64, error)

	protectMu sync.Mutex
	protected map[string]bool // packages retention must not delete right now (a restore is reading one)
}

// Protect keeps a package out of retention until the returned function is called. A restore takes a safety
// snapshot of the current state first; with the store full that snapshot would otherwise delete the oldest
// package, which is exactly the one being restored.
func (s *BackupService) Protect(name string) (release func()) {
	s.protectMu.Lock()
	if s.protected == nil {
		s.protected = map[string]bool{}
	}
	s.protected[name] = true
	s.protectMu.Unlock()
	return func() {
		s.protectMu.Lock()
		delete(s.protected, name)
		s.protectMu.Unlock()
	}
}

func (s *BackupService) isProtected(name string) bool {
	s.protectMu.Lock()
	defer s.protectMu.Unlock()
	return s.protected[name]
}

func (s *BackupService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func freeBytes(p string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func (s *BackupService) ensureDir() error {
	if err := os.MkdirAll(s.Config.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.Config.Dir, 0o700)
}

var versionText = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)

// platformVersion asks the running backend for its version. A restore is usually needed when the backend is
// not running, so the last version it reported is kept in <backup dir>/.last-version and used as a fallback.
func (s *BackupService) platformVersion(ctx context.Context) string {
	v, _ := s.platformVersionLive(ctx)
	if v != "" {
		return v
	}
	return "unknown"
}

// PlatformVersionLive reports the running platform version (or the last one it reported); ok is false if unknown.
func (s *BackupService) PlatformVersionLive(ctx context.Context) (string, bool) {
	return s.platformVersionLive(ctx)
}

func (s *BackupService) platformVersionLive(ctx context.Context) (string, bool) {
	var out bytes.Buffer
	code, _, err := s.Runner.ExecStream(ctx, s.Project, s.Config.VersionService, s.Config.VersionArgv, &out)
	v := strings.TrimSpace(out.String())
	if err == nil && code == 0 && versionText.MatchString(v) {
		if os.MkdirAll(s.Config.Dir, 0o700) == nil {
			_ = atomicWrite(filepath.Join(s.Config.Dir, ".last-version"), []byte(v+"\n"), 0o600, false)
		}
		return v, true
	}
	if raw, err := os.ReadFile(filepath.Join(s.Config.Dir, ".last-version")); err == nil {
		if cached := strings.TrimSpace(string(raw)); versionText.MatchString(cached) {
			return cached, false
		}
	}
	return "", false
}

// CleanPartials removes leftovers of an interrupted backup. The caller holds the operation lock, so any
// partial file is stale.
func (s *BackupService) CleanPartials() {
	entries, _ := os.ReadDir(s.Config.Dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".partial") {
			_ = os.RemoveAll(filepath.Join(s.Config.Dir, e.Name()))
		}
	}
}

// Create writes a full backup. report is optional progress.
func (s *BackupService) Create(ctx context.Context, trigger string, report func(step string, percent int)) (string, error) {
	return s.CreateWith(ctx, CreateOptions{Trigger: trigger}, report)
}

// CreateOptions varies a backup. Version, when set, replaces the lookup in the backend container (which is
// stopped while a restore or upgrade takes its safety snapshot).
type CreateOptions struct {
	Trigger string
	Version string
}

// CreateWith is Create with options.
func (s *BackupService) CreateWith(ctx context.Context, opts CreateOptions, report func(step string, percent int)) (string, error) {
	trigger := opts.Trigger
	if report == nil {
		report = func(string, int) {}
	}
	if err := s.ensureDir(); err != nil {
		return "", CodedError{CodeBackupWrite}
	}
	s.CleanPartials()
	// 1. preflight: store readable, enough space (twice the estimate, plus a margin).
	report(StepPreflight, 2)
	info, err := s.Adapter.Describe(ctx)
	if err != nil {
		return "", CodedError{CodeBackupStore}
	}
	estimate, err := s.Adapter.Estimate(ctx)
	if err != nil {
		return "", CodedError{CodeBackupStore}
	}
	free := s.FreeBytes
	if free == nil {
		free = freeBytes
	}
	if avail, err := free(s.Config.Dir); err != nil || int64(avail) < 2*estimate+64<<20 {
		return "", CodedError{CodeBackupSpace}
	}
	version := opts.Version
	if version == "" {
		version = s.platformVersion(ctx)
	}
	created := s.now()
	base := fmt.Sprintf("tp-backup-%s-%s-full", created.Format("20060102T150405Z"), version)
	name := base + ".tar"
	work := filepath.Join(s.Config.Dir, base+".partial.d")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", CodedError{CodeBackupWrite}
	}
	defer os.RemoveAll(work)

	manifest := Manifest{Format: backupFormat, PlatformVersion: version, AgentVersion: s.Version, Kind: "full", CreatedAt: created, Trigger: trigger,
		Schema: ManifestSchema{SysVersion: info.SysVersion, Databases: info.Migrations}, Stores: map[string]StoreInfo{"relational": info},
		Includes: map[string]bool{"history": true, "secrets": false}}
	type staged struct{ entry, file string }
	var stage []staged
	add := func(entry, file string) { stage = append(stage, staged{entry, file}) }

	// 2. databases
	for i, db := range info.Databases {
		report(StepDumping, 5+60*i/len(info.Databases))
		file := filepath.Join(work, "db-"+db+".dump")
		if err := dumpTo(ctx, s.Adapter, db, file); err != nil {
			return "", CodedError{CodeBackupDump}
		}
		add("db/"+db+".dump", file)
	}
	// 3. files volume, configuration, plugin list
	report(StepCollecting, 70)
	filesTar := filepath.Join(work, "files.tar")
	if err := copyTo(ctx, s.Runner, s.Project, s.Config.FilesService, s.Config.FilesPath, filesTar); err != nil && !errors.Is(err, ErrNoContainer) {
		return "", CodedError{CodeBackupFiles}
	} else if err == nil {
		add("files/files.tar", filesTar)
	}
	for _, rel := range s.Config.ConfigFiles {
		src := filepath.Join(s.Config.Root, rel)
		info, err := os.Lstat(src)
		if err != nil {
			continue // an optional file that does not exist is simply not part of this backup
		}
		if !info.Mode().IsRegular() {
			return "", CodedError{CodeBackupConfig}
		}
		add("config/"+rel, src)
	}
	if s.Plugins != nil {
		raw, err := s.Plugins()
		if err != nil {
			return "", CodedError{CodeBackupConfig}
		}
		manifest.Plugins = raw
		file := filepath.Join(work, "instances.json")
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			return "", CodedError{CodeBackupWrite}
		}
		add("plugins/instances.json", file)
	}
	// 4. package: tar to .partial, manifest last, fsync, atomic rename.
	report(StepPackaging, 80)
	partial := filepath.Join(s.Config.Dir, name+".partial")
	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", CodedError{CodeBackupWrite}
	}
	cleanup := func() { out.Close(); os.Remove(partial) }
	tw := tar.NewWriter(out)
	for _, st := range stage {
		mf, err := addFile(tw, st.entry, st.file, created)
		if err != nil {
			cleanup()
			return "", CodedError{CodeBackupWrite}
		}
		manifest.Files = append(manifest.Files, mf)
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := writeEntry(tw, manifestName, raw, created); err != nil || tw.Close() != nil || out.Sync() != nil || out.Close() != nil {
		cleanup()
		return "", CodedError{CodeBackupWrite}
	}
	final := filepath.Join(s.Config.Dir, name)
	if err := os.Rename(partial, final); err != nil {
		os.Remove(partial)
		return "", CodedError{CodeBackupWrite}
	}
	syncDir(s.Config.Dir)
	// sidecar for quick listing; the manifest inside the tar stays authoritative
	_ = atomicWrite(filepath.Join(s.Config.Dir, name+".json"), raw, 0o600, false)
	// 5. retention for automatic snapshots only
	report(StepRetention, 95)
	s.retention()
	return name, nil
}

func dumpTo(ctx context.Context, a BackupAdapter, db, file string) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := a.Dump(ctx, db, f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyTo(ctx context.Context, r StreamRunner, project, service, p, file string) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := r.CopyFrom(ctx, project, service, p, f); err != nil {
		f.Close()
		os.Remove(file)
		return err
	}
	return f.Close()
}

func addFile(tw *tar.Writer, entry, file string, at time.Time) (ManifestFile, error) {
	f, err := os.Open(file)
	if err != nil {
		return ManifestFile{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ManifestFile{}, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o600, Size: info.Size(), ModTime: at, Typeflag: tar.TypeReg}); err != nil {
		return ManifestFile{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), f)
	if err != nil {
		return ManifestFile{}, err
	}
	return ManifestFile{Path: entry, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func writeEntry(tw *tar.Writer, entry string, data []byte, at time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o600, Size: int64(len(data)), ModTime: at, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func (s *BackupService) path(name string) (string, error) {
	if !backupNamePattern.MatchString(name) {
		return "", ErrBackupName
	}
	return filepath.Join(s.Config.Dir, name), nil
}

func (s *BackupService) retention() {
	keep := s.Config.KeepAuto
	if keep == 0 {
		keep = 3
	}
	var auto []BackupInfo
	for _, b := range s.List() {
		if b.Valid && b.Trigger != "manual" {
			auto = append(auto, b)
		}
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].CreatedAt.After(auto[j].CreatedAt) })
	kept := 0
	for _, b := range auto {
		if kept < keep {
			kept++
			continue
		}
		if s.isProtected(b.Name) {
			continue // in use by a restore; it goes at the next retention pass
		}
		_ = s.Delete(b.Name)
	}
}

// List shows complete packages only (*.tar); partial leftovers are never listed.
func (s *BackupService) List() []BackupInfo {
	entries, _ := os.ReadDir(s.Config.Dir)
	out := []BackupInfo{}
	for _, e := range entries {
		if !backupNamePattern.MatchString(e.Name()) {
			continue
		}
		info := BackupInfo{Name: e.Name()}
		if fi, err := e.Info(); err == nil {
			info.Size = fi.Size()
		}
		if m, err := readManifestFile(filepath.Join(s.Config.Dir, e.Name())); err == nil {
			info.Valid, info.CreatedAt, info.PlatformVersion, info.Trigger, info.History = true, m.CreatedAt, m.PlatformVersion, m.Trigger, m.Includes["history"]
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

func readManifestFile(tarPath string) (Manifest, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	var m Manifest
	found := false
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		if h.Name == manifestName {
			if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&m); err != nil {
				return Manifest{}, err
			}
			found = true
		}
	}
	if !found || m.Format != backupFormat {
		return Manifest{}, errors.New("no usable manifest")
	}
	return m, nil
}

// Verify re-reads the whole package and checks every file against the manifest. It needs nothing but the file.
func (s *BackupService) Verify(name string) (Manifest, error) {
	p, err := s.path(name)
	if err != nil {
		return Manifest{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return Manifest{}, ErrBackupNotFound
	}
	defer f.Close()
	sums := map[string]ManifestFile{}
	var m Manifest
	haveManifest := false
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, CodedError{CodeBackupVerify}
		}
		if h.Typeflag != tar.TypeReg || !allowedEntry(h.Name) {
			return Manifest{}, CodedError{CodeBackupVerify}
		}
		if h.Name == manifestName {
			if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&m); err != nil {
				return Manifest{}, CodedError{CodeBackupVerify}
			}
			haveManifest = true
			continue
		}
		hash := sha256.New()
		n, err := io.Copy(hash, tr)
		if err != nil {
			return Manifest{}, CodedError{CodeBackupVerify}
		}
		sums[h.Name] = ManifestFile{Path: h.Name, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n}
	}
	if !haveManifest || m.Format != backupFormat || m.Includes["secrets"] || len(m.Files) != len(sums) {
		return Manifest{}, CodedError{CodeBackupVerify}
	}
	for _, want := range m.Files {
		got, ok := sums[want.Path]
		if !ok || got.SHA256 != want.SHA256 || got.Size != want.Size {
			return Manifest{}, CodedError{CodeBackupVerify}
		}
	}
	return m, nil
}

func allowedEntry(name string) bool {
	if name == manifestName {
		return true
	}
	if path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.Contains(name, "..") || secretNames.MatchString(name) {
		return false
	}
	for _, prefix := range []string{"db/", "config/", "files/", "plugins/"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// Delete removes one package and its sidecar.
func (s *BackupService) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err != nil {
		return ErrBackupNotFound
	}
	_ = os.Remove(p + ".json")
	return os.Remove(p)
}

// ExportPlugins returns the plugin instance list for plugins/instances.json: instance ids, generations and
// resource names only. It contains no secrets, tokens, request keys or payload hashes.
func ExportPlugins(store *Store) (json.RawMessage, error) {
	host, generations, reservations, err := store.Snapshot()
	if err != nil {
		return nil, err
	}
	type res struct {
		Resource   string `json:"resource"`
		Instance   string `json:"instance"`
		Generation uint64 `json:"generation"`
		State      string `json:"state"`
	}
	list := []res{}
	for _, r := range reservations {
		list = append(list, res{r.Resource, r.Instance, r.Generation, r.State})
	}
	return json.MarshalIndent(map[string]any{"host_id": host, "generations": generations, "reservations": list}, "", "  ")
}
