//go:build linux

package agent

// Go port of the file-level helpers of config/native/update.py (R5a): durable atomic writes, content
// fingerprints that do not follow symlinks, managed-path validation and the installer's update lock.
// The lock is the same flock(2) on <root>/.thingspanel-update/lock that update.py takes, so the two
// implementations exclude each other during the transition.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrUpdateLockHeld is returned when another installer or Agent operation holds the update lock.
var ErrUpdateLockHeld = errors.New("another installer operation is active")

// TxnError is a configuration transaction failure. Messages never carry file contents or secrets.
type TxnError struct{ Msg string }

func (e *TxnError) Error() string { return e.Msg }

func txnErr(format string, args ...any) error { return &TxnError{Msg: fmt.Sprintf(format, args...)} }

// FileFP is the fingerprint the journal stores for a managed file. Field names match update.py's journal.
type FileFP struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
}

func sameFP(a, b *FileFP) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// makeDirs creates every missing directory in path, persisting each new entry.
func makeDirs(path string, mode os.FileMode) error {
	var missing []string
	cur := path
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], mode); err != nil && !os.IsExist(err) {
			return err
		}
		if err := fsyncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

// privateDir requires path to be a real, owner-only directory, creating it 0700 when absent.
func privateDir(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return txnErr("Update state path is not a real directory")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return txnErr("Update state directory must be private")
		}
		return nil
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(path))
}

// fileFingerprint hashes a regular file without following its final symlink. A missing file is (nil, nil).
func fileFingerprint(path string) (*FileFP, error) {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if !before.Mode().IsRegular() {
		return nil, txnErr("A managed file is not a regular file: %s", name)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	bs, os1 := before.Sys().(*syscall.Stat_t), opened.Sys().(*syscall.Stat_t)
	if bs.Dev != os1.Dev || bs.Ino != os1.Ino {
		return nil, txnErr("A managed file changed while being read: %s", name)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || after.Mode() != opened.Mode() {
		return nil, txnErr("A managed file changed while being read: %s", name)
	}
	st := after.Sys().(*syscall.Stat_t)
	return &FileFP{SHA256: hex.EncodeToString(h.Sum(nil)), Size: after.Size(), Mode: uint32(st.Mode & 0o7777), UID: st.Uid, GID: st.Gid}, nil
}

// safeTarget joins a managed relative path to root, refusing odd components and symlinked or non-directory parents.
func safeTarget(root, relative string) (string, error) {
	parts := strings.Split(relative, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", txnErr("Invalid managed path")
		}
	}
	if strings.ContainsRune(relative, '\\') {
		return "", txnErr("Invalid managed path")
	}
	target := root
	for _, p := range parts[:len(parts)-1] {
		target = filepath.Join(target, p)
		if info, err := os.Lstat(target); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", txnErr("A managed parent is not a real directory")
			}
		}
	}
	return filepath.Join(target, parts[len(parts)-1]), nil
}

// durableWrite replaces path with content through a temp file in the same directory: write, set mode and
// (when owner is given) ownership, fsync, atomic rename, fsync of the directory.
func durableWrite(path string, content []byte, mode uint32, owner *FileFP) error {
	dir := filepath.Dir(path)
	if err := makeDirs(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(os.FileMode(mode)); err != nil {
		tmp.Close()
		return err
	}
	if owner != nil {
		st := &syscall.Stat_t{}
		if err := syscall.Fstat(int(tmp.Fd()), st); err != nil {
			tmp.Close()
			return err
		}
		if st.Uid != owner.UID || st.Gid != owner.GID {
			if err := tmp.Chown(int(owner.UID), int(owner.GID)); err != nil {
				tmp.Close()
				return err
			}
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// AcquireUpdateLock takes the installer's exclusive, non-blocking lock in state (normally
// <root>/.thingspanel-update). The kernel releases it if the process dies.
func AcquireUpdateLock(state string) (release func(), err error) {
	path := filepath.Join(state, "lock")
	if info, lerr := os.Lstat(path); lerr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, txnErr("Invalid update lock")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrUpdateLockHeld
	}
	return func() { f.Close() }, nil
}
