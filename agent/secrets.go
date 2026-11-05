//go:build linux

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// SecretStore keeps values the control plane must never persist elsewhere: site secrets typed in the UI, the
// per-generation runtime identity token, generated plugin keys and the stack's MQTT credentials. It is one file
// (mode 0600, written atomically) owned by the Agent. Profiles and the state journal only ever hold the placeholder
// "@secret:<name>", so no secret reaches profiles.json, state.json, a label or a log line.
type SecretStore struct {
	path string
	mu   sync.Mutex
}

const secretPrefix = "@secret:"

var secretKey = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,128}$`)

func OpenSecretStore(path string) (*SecretStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	s := &SecretStore{path: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return s, s.write(map[string]map[string]string{})
	}
	if _, err := s.read(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SecretStore) read() (map[string]map[string]string, error) {
	info, err := os.Lstat(s.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrPreparationPolicy
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *SecretStore) write(data map[string]map[string]string) error {
	raw, _ := json.Marshal(data)
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func (s *SecretStore) Put(ns, key, value string) error {
	if !valid(ns) || len(ns) > 256 || !secretKey.MatchString(key) || len(value) == 0 || len(value) > 8192 || strings.ContainsAny(value, "\x00\r\n") {
		return ErrPreparationPolicy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return err
	}
	if data[ns] == nil {
		data[ns] = map[string]string{}
	}
	data[ns][key] = value
	return s.write(data)
}

func (s *SecretStore) Get(ns, key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return "", false
	}
	v, ok := data[ns][key]
	return v, ok
}

// Keys lists the key names (never values) under a prefix, prefix stripped.
func (s *SecretStore) Keys(ns, prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	out := []string{}
	if err != nil {
		return out
	}
	for k := range data[ns] {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(out)
	return out
}

func (s *SecretStore) DeleteKey(ns, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return err
	}
	delete(data[ns], key)
	return s.write(data)
}

func (s *SecretStore) DeletePrefix(ns, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return err
	}
	for k := range data[ns] {
		if strings.HasPrefix(k, prefix) {
			delete(data[ns], k)
		}
	}
	return s.write(data)
}

// GetOrCreateHex returns the stored value or stores size random bytes (hex) once.
func (s *SecretStore) GetOrCreateHex(ns, key string, size int) (string, error) {
	if v, ok := s.Get(ns, key); ok {
		return v, nil
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	v := hex.EncodeToString(raw)
	return v, s.Put(ns, key, v)
}

// ResolveEnv replaces KEY=@secret:name by KEY=value. A missing secret fails closed.
func (s *SecretStore) ResolveEnv(instance string, env []string) ([]string, error) {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(value, secretPrefix) {
			secret, ok := s.Get(instance, strings.TrimPrefix(value, secretPrefix))
			if !ok {
				return nil, ErrPreparationPolicy
			}
			entry = key + "=" + secret
		}
		out = append(out, entry)
	}
	return out, nil
}

func hasPlaceholder(env []string) bool {
	for _, entry := range env {
		if _, value, _ := strings.Cut(entry, "="); strings.HasPrefix(value, secretPrefix) {
			return true
		}
	}
	return false
}
