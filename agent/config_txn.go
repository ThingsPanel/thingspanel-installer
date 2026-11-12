//go:build linux

package agent

// Go port of update.py's Transaction (R5a): stage a complete payload, back up every managed file, replace
// them one by one with a journal written before each step, and recover from any interruption. The journal
// file (.thingspanel-update/journal.json) and the lock file are the same ones update.py uses, so a
// transaction started by one implementation can be recovered by the other and they never run together.
//
// Not ported on purpose: the post-commit activation tail (prepare.py --bootstrap, compose up, verification)
// stays with the caller via ActivateFunc; Python-only checks (ast.parse of staged .py files, the
// UPDATE_ENGINE_SHA256 pin of the management script) have no Go equivalent and are replaced by the signed
// release check when a policy is given.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Managed file sets, identical to update.py.
var (
	commonArtifacts = []Artifact{
		{"docker-compose.yml", 0o644}, {"niginx.conf", 0o644}, {"config/native/prepare.py", 0o644},
		{"config/native/runtime.encore.json", 0o644}, {"config/native/runtime.notification.json", 0o644},
		{"config/native/tick.sh", 0o644}, {"config/native/README.md", 0o644},
	}
	legacyWatched     = []string{".env", "config/native/plugin-token"}
	activationFiles   = []string{".env", "config/native/broker/plugin-token", "config/native/broker/owner-tokens.json", "config/native/broker/services.json"}
	watchedFiles      = append(append(append([]string{}, legacyWatched...), "config/native/owner-tokens.json", "config/native/services.json"), activationFiles[1:]...)
	operationPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	journalPhases     = []string{"prepared", "applying", "config_committed", "activation_started", "complete", "recovering", "recovered"}
	maxDownloadedFile = int64(16 * 1024 * 1024)
)

// Artifact is a managed file and the mode a new copy gets.
type Artifact struct {
	Path string
	Mode uint32
}

// DefaultArtifacts is the managed set update.py uses for platform ("posix" or "windows").
func DefaultArtifacts(platform string) []Artifact {
	list := append([]Artifact{}, commonArtifacts...)
	if platform == "windows" {
		list = append(list, Artifact{"upgrade.ps1", 0o644}, Artifact{"uninstall.ps1", 0o644})
	} else {
		list = append(list, Artifact{"upgrade.sh", 0o755}, Artifact{"uninstall.sh", 0o755})
	}
	return append(list, Artifact{"config/native/update.py", 0o644})
}

// Downloader fetches url into dest (a path inside the private stage).
type Downloader func(url, dest string) error

// HTTPSDownloader is the default: HTTPS only (also across redirects), 180 s, 16 MiB, fsynced.
func HTTPSDownloader(rawURL, dest string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return txnErr("Installer file download failed")
	}
	client := &http.Client{Timeout: 180 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Scheme != "https" {
			return txnErr("Installer file download failed")
		}
		return nil
	}}
	resp, err := client.Get(rawURL)
	if err != nil {
		return txnErr("Installer file download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return txnErr("Installer file download failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadedFile+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maxDownloadedFile {
		return txnErr("Downloaded installer file is empty or too large")
	}
	return durableWrite(dest, body, 0o600, nil)
}

// ActivateFunc is the caller's post-commit step. While it runs the journal is "activation_started"; if it
// fails the committed configuration is kept for review and nothing is rolled back (no database or service
// rollback is ever claimed).
type ActivateFunc func(root string) error

// ConfigTransaction replaces a stack's managed files atomically and recoverably.
type ConfigTransaction struct {
	Root      string
	Platform  string // "posix" (default) or "windows"; selects the managed set
	Artifacts []Artifact
	// KeepInstalled lists managed files that are carried over from the installed copy instead of downloaded
	// (update.py does this for itself); when the file is absent it is downloaded like the others.
	KeepInstalled []string
	Download      Downloader
	// Validate checks the complete stage before anything is replaced; nil uses ValidateStage.
	Validate func(stage, expectedVersion string) error
	Release  *ReleasePolicy
	// Checkpoint is called at named steps; an error aborts as if the step failed (tests use it to crash).
	Checkpoint func(event string) error

	state, journalPath string
}

type journalFile struct {
	Path string  `json:"path"`
	Old  *FileFP `json:"old"`
	New  *FileFP `json:"new"`
	Mode uint32  `json:"mode"`
}

type journal struct {
	Version     int                `json:"version"`
	Root        string             `json:"root"`
	Operation   string             `json:"operation"`
	Phase       string             `json:"phase"`
	Files       []journalFile      `json:"files"`
	Watched     map[string]*FileFP `json:"watched"`
	CreatedDirs []string           `json:"created_dirs"`
	Intent      *string            `json:"intent"`
	// Written only by update.py's activation (prepare.py) and preserved verbatim here.
	ActivationFiles json.RawMessage `json:"activation_files,omitempty"`
	ActivationDirs  json.RawMessage `json:"activation_dirs,omitempty"`
}

func (t *ConfigTransaction) init() error {
	root, err := filepath.Abs(t.Root)
	if err != nil {
		return err
	}
	t.Root = root
	if info, err := os.Lstat(root); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return txnErr("Install directory must be a real directory")
	}
	if err := makeDirs(root, 0o755); err != nil {
		return err
	}
	t.state = filepath.Join(root, ".thingspanel-update")
	if err := privateDir(t.state); err != nil {
		return err
	}
	t.journalPath = filepath.Join(t.state, "journal.json")
	if t.Platform == "" {
		t.Platform = "posix"
	}
	if t.Platform != "posix" && t.Platform != "windows" {
		return txnErr("Unsupported installer platform")
	}
	if t.Artifacts == nil {
		t.Artifacts = DefaultArtifacts(t.Platform)
	}
	if t.Download == nil {
		t.Download = HTTPSDownloader
	}
	if t.Validate == nil {
		t.Validate = func(stage, version string) error { return ValidateStage(stage, t.Artifacts, version) }
	}
	return nil
}

func (t *ConfigTransaction) checkpoint(event string) error {
	if t.Checkpoint == nil {
		return nil
	}
	return t.Checkpoint(event)
}

func (t *ConfigTransaction) save(j *journal) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return durableWrite(t.journalPath, append(raw, '\n'), 0o600, nil)
}

func inList(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func validFP(fp *FileFP, nullable bool) bool {
	if fp == nil {
		return nullable
	}
	return hex64Pattern.MatchString(fp.SHA256) && fp.Size >= 0 && fp.Mode <= 0o7777
}

// load reads and validates the journal exactly as strictly as update.py: anything unexpected is an error that
// preserves the journal for a human, never a guess.
func (t *ConfigTransaction) load() (*journal, error) {
	raw, err := os.ReadFile(t.journalPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	bad := txnErr("Update journal is invalid; preserve it for recovery")
	if err != nil {
		return nil, bad
	}
	var j journal
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil || j.Version != 1 {
		return nil, bad
	}
	if j.Root != t.Root {
		// A finished installation may be relocated; an unresolved one is bound to its path.
		if j.Phase != "complete" && j.Phase != "recovered" {
			return nil, bad
		}
		j.Root = t.Root
	}
	if !operationPattern.MatchString(j.Operation) || !inList(journalPhases, j.Phase) {
		return nil, bad
	}
	expected := map[string]bool{}
	for _, a := range t.Artifacts {
		expected[a.Path] = true
	}
	seen := map[string]bool{}
	for _, f := range j.Files {
		if seen[f.Path] || !expected[f.Path] {
			return nil, bad
		}
		seen[f.Path] = true
	}
	if len(seen) != len(expected) {
		return nil, bad
	}
	if len(j.Watched) != len(watchedFiles) && len(j.Watched) != len(legacyWatched) {
		return nil, bad
	}
	for name, fp := range j.Watched {
		if !inList(watchedFiles, name) || !validFP(fp, true) {
			return nil, bad
		}
	}
	if len(j.Watched) == len(legacyWatched) {
		for _, name := range legacyWatched {
			if _, ok := j.Watched[name]; !ok {
				return nil, bad
			}
		}
	}
	activated := len(j.ActivationFiles) > 0 || len(j.ActivationDirs) > 0
	if activated && j.Phase != "activation_started" && j.Phase != "complete" {
		return nil, bad
	}
	if len(j.ActivationFiles) > 0 {
		var af map[string]struct {
			Old     *FileFP `json:"old"`
			New     *FileFP `json:"new"`
			Applied *bool   `json:"applied"`
		}
		dec := json.NewDecoder(strings.NewReader(string(j.ActivationFiles)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&af); err != nil {
			return nil, bad
		}
		for name, e := range af {
			if !inList(activationFiles, name) || e.Applied == nil || !validFP(e.Old, true) || !validFP(e.New, false) {
				return nil, bad
			}
			if w, ok := j.Watched[name]; !ok || !sameFP(e.Old, w) {
				return nil, bad
			}
			if j.Phase == "complete" && !*e.Applied {
				return nil, bad
			}
			if _, err := safeTarget(t.Root, name); err != nil {
				return nil, bad
			}
		}
	}
	if len(j.ActivationDirs) > 0 {
		var ad []string
		if err := json.Unmarshal(j.ActivationDirs, &ad); err != nil {
			return nil, bad
		}
		seenDir := map[string]bool{}
		for _, d := range ad {
			if seenDir[d] || !inList([]string{"config", "config/native", "config/native/broker"}, d) {
				return nil, bad
			}
			seenDir[d] = true
		}
	}
	seenDir := map[string]bool{}
	for _, d := range j.CreatedDirs {
		if seenDir[d] || !inList([]string{"config", "config/native"}, d) {
			return nil, bad
		}
		seenDir[d] = true
	}
	if j.Intent != nil && !expected[*j.Intent] {
		return nil, bad
	}
	for _, f := range j.Files {
		if _, err := safeTarget(t.Root, f.Path); err != nil {
			return nil, bad
		}
		if !validFP(f.Old, true) || !validFP(f.New, false) || f.Mode != f.New.Mode || (f.Old != nil && f.Mode != f.Old.Mode) {
			return nil, bad
		}
	}
	for _, prefix := range []string{"stage", "backup"} {
		if _, err := safeTarget(t.state, j.Operation+"/"+prefix+"/placeholder"); err != nil {
			return nil, bad
		}
	}
	return &j, nil
}

// ValidateStage is the Go replacement of update.py's validate for what Go can check: every staged file
// exists, is text without NUL, non-empty, JSON files are objects, and (when expectedVersion is set) the
// backend image tag in docker-compose.yml equals it.
func ValidateStage(stage string, artifacts []Artifact, expectedVersion string) error {
	for _, a := range artifacts {
		raw, err := os.ReadFile(filepath.Join(stage, a.Path))
		text := strings.TrimPrefix(string(raw), "\uFEFF")
		if err != nil || strings.ContainsRune(text, 0) || strings.TrimSpace(text) == "" || !utf8.Valid(raw) {
			return txnErr("Invalid staged installer file: %s", a.Path)
		}
		if strings.HasSuffix(a.Path, ".json") {
			var obj map[string]any
			if json.Unmarshal([]byte(text), &obj) != nil || obj == nil {
				return txnErr("Invalid staged installer file: %s", a.Path)
			}
		}
	}
	if expectedVersion == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(stage, "docker-compose.yml"))
	if err != nil {
		return txnErr("Invalid staged installer file: docker-compose.yml")
	}
	image, ok := ComposeBackendImage(string(raw))
	if def := regexp.MustCompile(`^\$\{TP_BACKEND_IMAGE:-([^${}]+)\}$`).FindStringSubmatch(image); def != nil {
		image = def[1]
	}
	if !ok || strings.Contains(image, "@") || !strings.Contains(image, ":") || image[strings.LastIndex(image, ":")+1:] != expectedVersion {
		return txnErr("Requested version does not match the staged Compose backend image")
	}
	return nil
}


// ComposeBackendImage returns the `image:` value of the top-level service named backend, read from the text of
// a docker-compose.yml (no YAML library: the reviewed Compose file keeps services two spaces deep).
func ComposeBackendImage(compose string) (string, bool) {
	inServices, inBackend := false, false
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimRight(line, " \r")
		if trimmed == "" || strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		switch {
		case indent == 0:
			inServices = strings.HasPrefix(trimmed, "services:")
			inBackend = false
		case inServices && indent == 2:
			inBackend = strings.TrimSpace(trimmed) == "backend:"
		case inBackend && indent == 4 && strings.HasPrefix(strings.TrimSpace(trimmed), "image:"):
			v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), "image:"))
			return strings.Trim(v, `"'`), v != ""
		}
	}
	return "", false
}

func newOperationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (t *ConfigTransaction) checkUnchanged(j *journal, beforeApply bool) error {
	for name, orig := range j.Watched {
		target, err := safeTarget(t.Root, name)
		if err != nil {
			return err
		}
		cur, err := fileFingerprint(target)
		if err != nil {
			return err
		}
		if !sameFP(cur, orig) {
			return txnErr("Install configuration changed during update: %s", name)
		}
	}
	if beforeApply {
		for _, f := range j.Files {
			target, err := safeTarget(t.Root, f.Path)
			if err != nil {
				return err
			}
			cur, err := fileFingerprint(target)
			if err != nil {
				return err
			}
			if !sameFP(cur, f.Old) {
				return txnErr("Managed file changed after preflight: %s", f.Path)
			}
		}
	}
	return nil
}

func (t *ConfigTransaction) prepare(source, expectedVersion string) (*journal, error) {
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, txnErr("An HTTPS installer distribution base is required")
	}
	op, err := newOperationID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(t.state, op)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	stage, backups := filepath.Join(dir, "stage"), filepath.Join(dir, "backup")
	for _, p := range []string{stage, backups} {
		if err := os.Mkdir(p, 0o700); err != nil {
			return nil, err
		}
	}
	if err := fsyncDir(dir); err != nil {
		return nil, err
	}
	j := &journal{Version: 1, Root: t.Root, Operation: op, Phase: "prepared", Watched: map[string]*FileFP{}, CreatedDirs: []string{}, Files: []journalFile{}}
	for _, name := range watchedFiles {
		target, err := safeTarget(t.Root, name)
		if err != nil {
			return nil, err
		}
		if j.Watched[name], err = fileFingerprint(target); err != nil {
			return nil, err
		}
	}
	for _, a := range t.Artifacts {
		target, err := safeTarget(t.Root, a.Path)
		if err != nil {
			return nil, err
		}
		orig, err := fileFingerprint(target)
		if err != nil {
			return nil, err
		}
		entry := journalFile{Path: a.Path, Old: orig, Mode: a.Mode}
		if orig != nil {
			entry.Mode = orig.Mode
		}
		j.Files = append(j.Files, entry)
		if orig != nil {
			content, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			bp := filepath.Join(backups, a.Path)
			if err := durableWrite(bp, content, orig.Mode, orig); err != nil {
				return nil, err
			}
			bfp, err1 := fileFingerprint(bp)
			tfp, err2 := fileFingerprint(target)
			if err1 != nil || err2 != nil || !sameFP(bfp, orig) || !sameFP(tfp, orig) {
				return nil, txnErr("Managed file changed during backup: %s", a.Path)
			}
		}
	}
	for i := range j.Files {
		f := &j.Files[i]
		sp := filepath.Join(stage, f.Path)
		target, _ := safeTarget(t.Root, f.Path)
		if inList(t.KeepInstalled, f.Path) && f.Old != nil {
			content, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			if err := durableWrite(sp, content, f.Mode, f.Old); err != nil {
				return nil, err
			}
		} else {
			if err := t.Download(strings.TrimRight(source, "/")+"/"+f.Path, sp); err != nil {
				return nil, err
			}
			content, err := os.ReadFile(sp)
			if err != nil {
				return nil, err
			}
			// Keep existing permissions and ownership, including private generated runtime files.
			if err := durableWrite(sp, content, f.Mode, f.Old); err != nil {
				return nil, err
			}
		}
		if f.New, err = fileFingerprint(sp); err != nil {
			return nil, err
		}
	}
	if t.Release != nil {
		if err := t.verifySigned(source, dir, stage, j, expectedVersion); err != nil {
			return nil, err
		}
	}
	if err := t.Validate(stage, expectedVersion); err != nil {
		return nil, err
	}
	if err := t.checkpoint("validated"); err != nil {
		return nil, err
	}
	if err := t.checkUnchanged(j, true); err != nil {
		return nil, err
	}
	if err := t.save(j); err != nil {
		return nil, err
	}
	if err := t.checkpoint("prepared"); err != nil {
		return nil, err
	}
	return j, nil
}

func (t *ConfigTransaction) verifySigned(source, dir, stage string, j *journal, expectedVersion string) error {
	release := filepath.Join(dir, "release")
	if err := privateDir(release); err != nil {
		return err
	}
	base := strings.TrimRight(source, "/") + "/release-manifest.json"
	if err := t.Download(base, filepath.Join(release, "manifest.json")); err != nil {
		return err
	}
	if err := t.Download(base+".sig", filepath.Join(release, "manifest.json.sig")); err != nil {
		return err
	}
	manifest, err1 := os.ReadFile(filepath.Join(release, "manifest.json"))
	sig, err2 := os.ReadFile(filepath.Join(release, "manifest.json.sig"))
	if err1 != nil || err2 != nil {
		return txnErr("Installer file download failed")
	}
	files := map[string]string{}
	for _, f := range j.Files {
		files[f.Path] = filepath.Join(stage, f.Path)
	}
	if _, err := VerifyFiles(manifest, sig, *t.Release, expectedVersion, files); err != nil {
		if re, ok := err.(*ReleaseError); ok {
			return txnErr("Signed release verification failed: %s", re.Reason)
		}
		return err
	}
	return nil
}

func (t *ConfigTransaction) apply(j *journal) error {
	if err := t.checkUnchanged(j, true); err != nil {
		return err
	}
	j.Phase = "applying"
	if err := t.save(j); err != nil {
		return err
	}
	for i := range j.Files {
		f := &j.Files[i]
		target, err := safeTarget(t.Root, f.Path)
		if err != nil {
			return err
		}
		if err := t.checkUnchanged(j, false); err != nil {
			return err
		}
		if cur, err := fileFingerprint(target); err != nil || !sameFP(cur, f.Old) {
			return txnErr("Managed file changed before replacement: %s", f.Path)
		}
		var missing []string
		for p := filepath.Dir(target); p != t.Root; p = filepath.Dir(p) {
			if _, err := os.Lstat(p); err == nil {
				break
			}
			missing = append(missing, p)
		}
		for k := len(missing) - 1; k >= 0; k-- {
			rel, _ := filepath.Rel(t.Root, missing[k])
			if !inList(j.CreatedDirs, rel) {
				j.CreatedDirs = append(j.CreatedDirs, rel)
				if err := t.save(j); err != nil { // write intent before creating anything
					return err
				}
			}
			if err := os.Mkdir(missing[k], 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			if err := fsyncDir(filepath.Dir(missing[k])); err != nil {
				return err
			}
		}
		intent := f.Path
		j.Intent = &intent
		if err := t.save(j); err != nil {
			return err
		}
		if err := t.checkpoint("before-replace:" + f.Path); err != nil {
			return err
		}
		if err := t.checkUnchanged(j, false); err != nil {
			return err
		}
		if cur, err := fileFingerprint(target); err != nil || !sameFP(cur, f.Old) {
			return txnErr("Managed file changed before replacement: %s", f.Path)
		}
		sp := filepath.Join(t.state, j.Operation, "stage", f.Path)
		if cur, err := fileFingerprint(sp); err != nil || !sameFP(cur, f.New) {
			return txnErr("Staged file changed before replacement: %s", f.Path)
		}
		content, err := os.ReadFile(sp)
		if err != nil {
			return err
		}
		if err := durableWrite(target, content, f.Mode, f.Old); err != nil {
			return err
		}
		if err := t.checkpoint("after-replace:" + f.Path); err != nil {
			return err
		}
		j.Intent = nil
		if err := t.save(j); err != nil {
			return err
		}
	}
	j.Phase = "config_committed"
	if err := t.save(j); err != nil {
		return err
	}
	return t.checkpoint("config_committed")
}

func (t *ConfigTransaction) recover(j *journal) error {
	if j.Phase == "complete" || j.Phase == "recovered" {
		return nil
	}
	if j.Phase == "activation_started" {
		return txnErr("Previous activation may have changed databases or services; inspect the journal and reconcile before another update. No database rollback was attempted")
	}
	// Inspect all targets first: neither a later user edit nor an unknown replacement is overwritten.
	for _, f := range j.Files {
		target, err := safeTarget(t.Root, f.Path)
		if err != nil {
			return err
		}
		cur, err := fileFingerprint(target)
		if err != nil {
			return err
		}
		if !sameFP(cur, f.Old) && !sameFP(cur, f.New) {
			return txnErr("Recovery requires review of an externally changed file: %s", f.Path)
		}
		if f.Old != nil {
			bfp, err := fileFingerprint(filepath.Join(t.state, j.Operation, "backup", f.Path))
			if err != nil || !sameFP(bfp, f.Old) {
				return txnErr("Recovery backup is missing or changed: %s", f.Path)
			}
		}
	}
	j.Phase = "recovering"
	if err := t.save(j); err != nil {
		return err
	}
	for i := len(j.Files) - 1; i >= 0; i-- {
		f := j.Files[i]
		target, _ := safeTarget(t.Root, f.Path)
		cur, err := fileFingerprint(target)
		if err != nil {
			return err
		}
		if sameFP(cur, f.Old) {
			continue
		}
		if !sameFP(cur, f.New) {
			return txnErr("File changed during recovery: %s", f.Path)
		}
		if f.Old == nil {
			if err := os.Remove(target); err != nil {
				return err
			}
			if err := fsyncDir(filepath.Dir(target)); err != nil {
				return err
			}
		} else {
			content, err := os.ReadFile(filepath.Join(t.state, j.Operation, "backup", f.Path))
			if err != nil {
				return err
			}
			if err := durableWrite(target, content, f.Old.Mode, f.Old); err != nil {
				return err
			}
		}
		if err := t.checkpoint("recovered-file:" + f.Path); err != nil {
			return err
		}
	}
	for i := len(j.CreatedDirs) - 1; i >= 0; i-- {
		dir, err := safeTarget(t.Root, j.CreatedDirs[i]+"/placeholder")
		if err != nil {
			continue
		}
		dir = filepath.Dir(dir)
		if os.Remove(dir) == nil { // never recursive: foreign contents stay
			_ = fsyncDir(filepath.Dir(dir))
		}
	}
	j.Phase = "recovered"
	return t.save(j)
}

// Run takes the update lock, first recovers an interrupted earlier transaction, then (unless recoveryOnly)
// prepares, applies and optionally activates a new one. A failure before activation restores every original
// file; once activation has started nothing is rolled back.
func (t *ConfigTransaction) Run(source, expectedVersion string, activate ActivateFunc, recoveryOnly bool) (err error) {
	if err := t.init(); err != nil {
		return err
	}
	unlock, err := AcquireUpdateLock(t.state)
	if err != nil {
		return err
	}
	defer unlock()
	old, err := t.load()
	if err != nil {
		return err
	}
	if old != nil {
		if err := t.recover(old); err != nil {
			return err
		}
	}
	if recoveryOnly {
		return nil
	}
	j, err := t.prepare(source, expectedVersion)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && j.Phase != "activation_started" {
			if rerr := t.recover(j); rerr != nil {
				err = fmt.Errorf("%w (recovery also failed: %v)", err, rerr)
			}
		}
	}()
	if err = t.apply(j); err != nil {
		return err
	}
	if activate != nil {
		if err = t.checkUnchanged(j, false); err != nil {
			return err
		}
		for _, f := range j.Files {
			target, _ := safeTarget(t.Root, f.Path)
			if cur, ferr := fileFingerprint(target); ferr != nil || !sameFP(cur, f.New) {
				err = txnErr("Managed file changed before activation: %s", f.Path)
				return err
			}
		}
		j.Phase = "activation_started"
		if err = t.save(j); err != nil {
			return err
		}
		if err = t.checkpoint("activation_started"); err != nil {
			return err
		}
		if err = activate(t.Root); err != nil {
			return err
		}
		// An activation (prepare.py) may have added receipts to this journal; keep them.
		var latest *journal
		if latest, err = t.load(); err != nil {
			return err
		}
		if latest == nil || latest.Operation != j.Operation || latest.Phase != "activation_started" {
			err = txnErr("Activation journal changed unexpectedly")
			return err
		}
		j = latest
	}
	j.Phase = "complete"
	return t.save(j)
}
