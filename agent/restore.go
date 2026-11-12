//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Restore puts a verified backup back. Order (plan section 4.2): verify the package and check that it fits the
// running system, stop everything except PostgreSQL, take a safety snapshot of the current state, rebuild each
// database (TimescaleDB pre/post restore around pg_restore), restore the uploaded files and the configuration,
// start the stack in order and run the health gate. If the gate fails, the safety snapshot is restored the same
// way (once); if that fails too, the stack is left stopped with the maintenance flag on for a person.

// Codes for restore failures and refusals.
const (
	CodeRestoreVerify    = "backup_invalid"
	CodeRestoreNewer     = "backup_newer_than_program"
	CodeRestoreVersion   = "version_not_comparable"
	CodeRestoreTimescale = "timescaledb_version_mismatch"
	CodeRestoreSchema    = "schema_newer_than_program"
	CodeRestoreSnapshot  = "safety_snapshot_failed"
	CodeRestoreDatabase  = "database_restore_failed"
	CodeRestoreFiles     = "files_restore_failed"
	CodeRestoreConfig    = "config_restore_failed"
	CodeRestoreStart     = "start_failed"
	CodeRestoreGate      = "health_gate_failed"
	CodeRolledBack       = "restore_failed_rolled_back"
	CodeRollbackFailed   = "rollback_failed_needs_human"

	StepVerifyingBackup = "verifying_backup"
	StepSnapshot        = "safety_snapshot"
	StepRestoreDB       = "restoring_databases"
	StepRestoreFiles    = "restoring_files"
	StepRestoreConfig   = "restoring_config"
	StepRollingBack     = "rolling_back"
)

var versionPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+].*)?$`)

// CompareVersions returns -1, 0, 1 for release-style versions (v1.4.0). Anything else is comparable only for
// string equality; otherwise ok is false.
func CompareVersions(a, b string) (cmp int, ok bool) {
	if a == b && a != "" && a != "unknown" {
		return 0, true
	}
	ma, mb := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0, false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		switch {
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
	}
	return 0, true
}

// CheckRestoreCompatible applies the restore rules: same version restores directly; an older backup restores
// and is then migrated forward by the program; a newer backup, a different TimescaleDB, an unreadable version or
// a newer schema are refused.
func CheckRestoreCompatible(m Manifest, currentVersion string, currentSchema int, currentTimescale string) error {
	cmp, ok := CompareVersions(m.PlatformVersion, currentVersion)
	if !ok {
		return CodedError{CodeRestoreVersion}
	}
	if cmp > 0 {
		return CodedError{CodeRestoreNewer}
	}
	if st := m.Stores["relational"]; st.Timescaledb != currentTimescale {
		return CodedError{CodeRestoreTimescale}
	} else if st.SysVersion > currentSchema && currentSchema > 0 {
		return CodedError{CodeRestoreSchema}
	}
	return nil
}

// RestoreService restores backups.
type RestoreService struct {
	Backup       *BackupService
	Operator     *Operator
	Manager      StackManager
	Verifier     *Verifier
	Grace        time.Duration
	VerifyBudget time.Duration
	// Dir inside the PostgreSQL container where dumps are placed; removed afterwards.
	WorkDir string
}

func (r *RestoreService) pg() *PostgresAdapter { return r.Backup.Adapter.(*PostgresAdapter) }

func (r *RestoreService) psql(ctx context.Context, database, sql string) (string, error) {
	return r.pg().query(ctx, database, sql)
}

func (r *RestoreService) workdir() string {
	if r.WorkDir != "" {
		return r.WorkDir
	}
	return "/tmp"
}

// Restore runs the whole sequence. It must be called from an Operator task, which holds the one-at-a-time lock.
func (r *RestoreService) Restore(ctx context.Context, name string, report func(string, int)) error {
	if report == nil {
		report = func(string, int) {}
	}
	project := r.Backup.Project
	// 1. preflight (nothing changed yet)
	report(StepVerifyingBackup, 2)
	manifest, err := r.Backup.Verify(name)
	if err != nil {
		return CodedError{CodeRestoreVerify}
	}
	currentVersion, _ := r.Backup.platformVersionLive(ctx) // falls back to the last version the backend reported
	if currentVersion == "" {
		currentVersion = "unknown"
	}
	info, err := r.Backup.Adapter.Describe(ctx)
	if err != nil {
		return CodedError{CodeBackupStore}
	}
	if err := CheckRestoreCompatible(manifest, currentVersion, info.SysVersion, info.Timescaledb); err != nil {
		return err
	}
	if err := r.apply(ctx, project, manifest, name, currentVersion, report, true); err != nil {
		return err
	}
	return nil
}

// apply stops the stack, snapshots (when allowed), restores package `name` and brings the stack back.
func (r *RestoreService) apply(ctx context.Context, project string, manifest Manifest, name, currentVersion string, report func(string, int), mayRollback bool) error {
	grace := r.Grace
	if grace <= 0 {
		grace = 30 * time.Second
	}
	// 2. maintenance on, stop everything except the data services (control plane in reverse start order, then broker)
	r.Operator.maintenance(true)
	var control, broker []Component
	for _, c := range r.Operator.Config.Components {
		switch c.Group {
		case "control":
			control = append(control, c)
		case "broker":
			broker = append(broker, c)
		}
	}
	report(StepStopping, 8)
	for _, c := range append(reverse(control), reverse(broker)...) {
		if err := r.Manager.Stop(ctx, project, c.Name, grace); err != nil && !errors.Is(err, ErrNoContainer) {
			return CodedError{CodeStopFailed}
		}
	}
	// 3. safety snapshot of the current state (the backend is stopped, so its version is passed in)
	var snapshot string
	if mayRollback {
		report(StepSnapshot, 15)
		var err error
		snapshot, err = r.Backup.CreateWith(ctx, CreateOptions{Trigger: "pre-restore", Version: currentVersion}, nil)
		if err != nil {
			r.startAll(ctx, project, control, broker) // nothing was changed; bring the stack back
			return CodedError{CodeRestoreSnapshot}
		}
	}
	// 4-6. databases, files, configuration
	err := r.restoreContents(ctx, project, name, manifest, report)
	if err == nil {
		report(StepStarting, 85)
		err = r.startAll(ctx, project, control, broker)
		if err == nil {
			report(StepVerifying, 92)
			budget := r.VerifyBudget
			if budget <= 0 {
				budget = 5 * time.Minute
			}
			if res := r.Verifier.Wait(ctx, budget); !res.OK {
				err = CodedError{CodeRestoreGate}
			}
		}
	}
	if err == nil {
		r.Operator.maintenance(false)
		return nil
	}
	if !mayRollback || snapshot == "" {
		return err // the rollback itself failed: stay stopped with the maintenance page for a person
	}
	// 7. rollback with the safety snapshot
	report(StepRollingBack, 90)
	m, verr := r.Backup.Verify(snapshot)
	if verr == nil {
		_ = r.stopAll(ctx, project, control, broker, grace)
		verr = r.restoreContents(ctx, project, snapshot, m, nil)
		if verr == nil {
			if verr = r.startAll(ctx, project, control, broker); verr == nil {
				if res := r.Verifier.Wait(ctx, r.verifyBudget()); !res.OK {
					verr = CodedError{CodeRestoreGate}
				}
			}
		}
	}
	if verr != nil {
		return CodedError{CodeRollbackFailed}
	}
	r.Operator.maintenance(false)
	return CodedError{CodeRolledBack}
}

func (r *RestoreService) verifyBudget() time.Duration {
	if r.VerifyBudget > 0 {
		return r.VerifyBudget
	}
	return 5 * time.Minute
}

func (r *RestoreService) stopAll(ctx context.Context, project string, control, broker []Component, grace time.Duration) error {
	for _, c := range append(reverse(control), reverse(broker)...) {
		if err := r.Manager.Stop(ctx, project, c.Name, grace); err != nil && !errors.Is(err, ErrNoContainer) {
			return err
		}
	}
	return nil
}

func (r *RestoreService) startAll(ctx context.Context, project string, control, broker []Component) error {
	for _, c := range append(append([]Component{}, broker...), control...) {
		if err := r.Manager.Start(ctx, project, c.Name); err != nil {
			return CodedError{CodeRestoreStart}
		}
	}
	return nil
}

// restoreContents puts the databases, files and configuration of package `name` in place.
func (r *RestoreService) restoreContents(ctx context.Context, project, name string, m Manifest, report func(string, int)) error {
	if report == nil {
		report = func(string, int) {}
	}
	cfg := r.Backup.Config
	pkg := filepath.Join(cfg.Dir, name)
	var dbs []string
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "db/") && strings.HasSuffix(f.Path, ".dump") {
			dbs = append(dbs, strings.TrimSuffix(strings.TrimPrefix(f.Path, "db/"), ".dump"))
		}
	}
	work := path.Join(r.workdir(), "tp-restore")
	report(StepRestoreDB, 30)
	// the dumps travel into the PostgreSQL container as one tar, no stdin streaming needed
	if err := r.putEntries(ctx, pkg, cfg.PostgresService, r.workdir(), "db/", "tp-restore/"); err != nil {
		return CodedError{CodeRestoreDatabase}
	}
	defer func() {
		var ignore bytes.Buffer
		_, _, _ = r.Backup.Runner.ExecStream(ctx, project, cfg.PostgresService, []string{"rm", "-rf", work}, &ignore)
	}()
	for i, db := range dbs {
		report(StepRestoreDB, 30+45*i/len(dbs))
		if !databaseName.MatchString(db) {
			return CodedError{CodeRestoreDatabase}
		}
		if err := r.restoreDatabase(ctx, db, path.Join(work, db+".dump")); err != nil {
			return CodedError{CodeRestoreDatabase}
		}
	}
	report(StepRestoreFiles, 78)
	if hasEntry(m, "files/files.tar") {
		if err := r.restoreFiles(ctx, pkg); err != nil {
			return CodedError{CodeRestoreFiles}
		}
	}
	report(StepRestoreConfig, 82)
	if err := r.restoreConfig(pkg, m); err != nil {
		return CodedError{CodeRestoreConfig}
	}
	return nil
}

func hasEntry(m Manifest, p string) bool {
	for _, f := range m.Files {
		if f.Path == p {
			return true
		}
	}
	return false
}

func (r *RestoreService) restoreDatabase(ctx context.Context, db, dumpPath string) error {
	pg := r.pg()
	q := func(database, sql string) error { _, err := r.psql(ctx, database, sql); return err }
	// every connection to it goes away, then it is recreated with the server's default encoding and collation
	if err := q("postgres", fmt.Sprintf("select pg_terminate_backend(pid) from pg_stat_activity where datname='%s' and pid<>pg_backend_pid()", db)); err != nil {
		return err
	}
	if err := q("postgres", fmt.Sprintf(`drop database if exists "%s"`, db)); err != nil {
		return err
	}
	enc, err := r.psql(ctx, "postgres", "select pg_encoding_to_char(encoding)||'|'||datcollate||'|'||datctype from pg_database where datname='template1'")
	if err != nil {
		return err
	}
	parts := strings.Split(enc, "|")
	if len(parts) != 3 {
		return errors.New("cannot read template encoding")
	}
	if err := q("postgres", fmt.Sprintf(`create database "%s" template template0 encoding '%s' lc_collate '%s' lc_ctype '%s'`, db, parts[0], parts[1], parts[2])); err != nil {
		return err
	}
	hasTimescale := r.dumpHasTimescale(ctx, dumpPath)
	if hasTimescale {
		if err := q(db, "create extension if not exists timescaledb"); err != nil {
			return err
		}
		if err := q(db, "select timescaledb_pre_restore()"); err != nil {
			return err
		}
	}
	var out bytes.Buffer
	code, _, err := r.Backup.Runner.ExecStream(ctx, r.Backup.Project, pg.Service, []string{"pg_restore", "-U", pg.User, "-d", db, "--no-owner", "--exit-on-error", dumpPath}, &out)
	if err != nil || code != 0 {
		return errors.New("pg_restore failed")
	}
	if hasTimescale {
		if err := q(db, "select timescaledb_post_restore()"); err != nil {
			return err
		}
	}
	return nil
}

func (r *RestoreService) dumpHasTimescale(ctx context.Context, dumpPath string) bool {
	var out bytes.Buffer
	pg := r.pg()
	code, _, err := r.Backup.Runner.ExecStream(ctx, r.Backup.Project, pg.Service, []string{"pg_restore", "-l", dumpPath}, &out)
	return err == nil && code == 0 && strings.Contains(out.String(), "EXTENSION - timescaledb")
}

// putEntries copies the package entries under prefix into the container directory as a tar stream, renaming
// the prefix (db/ -> tp-restore/).
func (r *RestoreService) putEntries(ctx context.Context, pkgPath, service, destDir, from, to string) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(copyEntries(pkgPath, pw, func(name string) (string, bool) {
			if strings.HasPrefix(name, from) {
				return to + strings.TrimPrefix(name, from), true
			}
			return "", false
		}))
	}()
	err := r.Backup.Runner.CopyTo(ctx, r.Backup.Project, service, destDir, pr)
	pr.CloseWithError(err)
	return err
}

// copyEntries writes the selected entries of the package tar as a new tar to w.
func copyEntries(pkgPath string, w io.Writer, pick func(string) (string, bool)) error {
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	tw := tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		newName, ok := pick(h.Name)
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		nh := *h
		nh.Name = newName
		if err := tw.WriteHeader(&nh); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	return tw.Close()
}

// restoreFiles extracts files/files.tar (itself a tar of the uploads directory) into the parent of FilesPath.
func (r *RestoreService) restoreFiles(ctx context.Context, pkgPath string) error {
	cfg := r.Backup.Config
	pr, pw := io.Pipe()
	go func() {
		f, err := os.Open(pkgPath)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		defer f.Close()
		tr := tar.NewReader(f)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				pw.CloseWithError(errors.New("files.tar not found"))
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if h.Name == "files/files.tar" {
				_, err := io.Copy(pw, tr)
				pw.CloseWithError(err)
				return
			}
		}
	}()
	err := r.Backup.Runner.CopyTo(ctx, r.Backup.Project, cfg.FilesService, path.Dir(cfg.FilesPath), pr)
	pr.CloseWithError(err)
	return err
}

// restoreConfig writes the backed-up configuration files back, atomically, unless an installer transaction is
// open. Identical files are left alone.
func (r *RestoreService) restoreConfig(pkgPath string, m Manifest) error {
	cfg := r.Backup.Config
	if raw, err := os.ReadFile(filepath.Join(cfg.Root, ".thingspanel-update/journal.json")); err == nil {
		if !bytes.Contains(raw, []byte(`"complete"`)) && !bytes.Contains(raw, []byte(`"recovered"`)) {
			return ErrScopeJournal
		}
	}
	allowed := map[string]bool{}
	for _, rel := range cfg.ConfigFiles {
		allowed["config/"+rel] = true
	}
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !allowed[h.Name] {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 32<<20))
		if err != nil {
			return err
		}
		dest := filepath.Join(cfg.Root, strings.TrimPrefix(h.Name, "config/"))
		if cur, err := os.ReadFile(dest); err == nil && bytes.Equal(cur, data) {
			continue
		}
		if err := atomicWrite(dest, data, 0o644, false); err != nil {
			return err
		}
	}
}
