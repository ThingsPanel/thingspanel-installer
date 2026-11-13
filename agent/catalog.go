//go:build linux

package agent

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const catalogSchema = "thingspanel.plugin-catalog.v1"

var ErrCatalog = errors.New("plugin catalog is not trusted")

type CatalogUserField struct {
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
	Description string `json:"description"`
	Validate    string `json:"validate,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
	Default     string `json:"default,omitempty"`
}

// CatalogVolume declares one named volume. The catalog never names a host path or a Docker volume: the Agent derives
// the volume identity from host + install instance + Key, so two tenants never share one.
type CatalogVolume struct {
	Key         string `json:"key"`
	Destination string `json:"destination"`
	Mode        string `json:"mode"` // "rw" or "ro"
}

// CatalogTmpfs declares a bounded writable scratch directory. It is never durable.
type CatalogTmpfs struct {
	Destination string `json:"destination"`
	SizeMiB     int    `json:"size_mib"`
}

type CatalogServicePlugin struct {
	Name        string            `json:"name"`
	ServiceType int32             `json:"service_type"`
	Description string            `json:"description"`
	Config      map[string]string `json:"config"`
}

// CatalogEntry is the signed catalog entry (same file the Python publisher writes). Only the fields the Agent acts
// on are read; the public projection is what the control plane sees.
type CatalogEntry struct {
	ID           string                      `json:"id"`
	Version      string                      `json:"version"`
	Description  string                      `json:"description"`
	ImageID      string                      `json:"image_id"`
	Entrypoint   string                      `json:"entrypoint"`
	Args         []string                    `json:"args"`
	Env          []string                    `json:"env"`
	EnvFromStack map[string]string           `json:"env_from_stack"`
	EnvGenerated map[string]int              `json:"env_generated"`
	EnvUser      map[string]CatalogUserField `json:"env_user"`
	Volumes      []CatalogVolume             `json:"volumes"`
	Tmpfs        []CatalogTmpfs              `json:"tmpfs"`
	Health       *struct {
		Port int    `json:"port"`
		Path string `json:"path"`
	} `json:"health"`
	Identity struct {
		TokenEnv string `json:"token_env"`
	} `json:"identity"`
	Platform struct {
		Binding           string                `json:"binding"`
		ServiceIdentifier string                `json:"service_identifier"`
		ProtocolType      string                `json:"protocol_type"`
		NetworkAlias      string                `json:"network_alias"`
		HTTPPort          int                   `json:"http_port"`
		ServicePlugin     *CatalogServicePlugin `json:"service_plugin"`
	} `json:"platform"`
}

type PublicEntry struct {
	// ImagePresent: the image this entry pins is present on this host, so an install or upgrade can start it. nil
	// when the Agent could not tell. The control plane refuses to upgrade a plugin whose new image is missing,
	// because the old generation is revoked before the new one is prepared.
	ImagePresent      *bool                       `json:"image_present,omitempty"`
	ID                string                      `json:"id"`
	DisplayName       string                      `json:"display_name"`
	Version           string                      `json:"version"`
	Description       string                      `json:"description"`
	ServiceIdentifier string                      `json:"service_identifier"`
	NetworkAlias      string                      `json:"network_alias"`
	HTTPPort          int                         `json:"http_port"`
	Health            map[string]any              `json:"health,omitempty"`
	ServicePlugin     *CatalogServicePlugin       `json:"service_plugin,omitempty"`
	EnvUser           map[string]CatalogUserField `json:"env_user"`
}

var aliasPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (e CatalogEntry) ServiceIdentifier() string {
	if e.Platform.ServiceIdentifier != "" {
		return e.Platform.ServiceIdentifier
	}
	return e.Platform.ProtocolType
}

func (e CatalogEntry) NetworkAlias() string {
	if e.Platform.NetworkAlias != "" {
		return e.Platform.NetworkAlias
	}
	return "tp-plugin-" + e.ID
}

func (e CatalogEntry) tokenEnv() string {
	if e.Identity.TokenEnv != "" {
		return e.Identity.TokenEnv
	}
	return "TP_PLUGIN_RUNTIME_TOKEN"
}

func (e CatalogEntry) Public() PublicEntry {
	out := PublicEntry{ID: e.ID, DisplayName: e.ID, Version: e.Version, Description: e.Description, ServiceIdentifier: e.ServiceIdentifier(),
		NetworkAlias: e.NetworkAlias(), HTTPPort: e.Platform.HTTPPort, EnvUser: e.EnvUser}
	if out.EnvUser == nil {
		out.EnvUser = map[string]CatalogUserField{}
	}
	if sp := e.Platform.ServicePlugin; sp != nil {
		c := *sp
		if c.ServiceType == 0 {
			c.ServiceType = 2
		}
		out.ServicePlugin, out.DisplayName = &c, c.Name
	}
	if e.Health != nil {
		out.Health = map[string]any{"port": e.Health.Port, "path": e.Health.Path}
	}
	return out
}

// valid is the Agent's own re-check of a verified entry (defence in depth, design 11.3): only what it needs to build
// a safe profile, not the publisher's full schema.
func (e CatalogEntry) validate() error {
	if !valid(e.ID) || len(e.ID) > 64 || !aliasPattern.MatchString(e.ID) || e.Version == "" || !validImageID(e.ImageID) ||
		!strings.HasPrefix(e.Entrypoint, "/") || e.Platform.Binding != "none" || e.ServiceIdentifier() == "" || !aliasPattern.MatchString(e.NetworkAlias()) {
		return ErrCatalog
	}
	for key := range e.EnvUser {
		if !validEnvKey(key) {
			return ErrCatalog
		}
	}
	if !validStorage(e.Volumes, e.Tmpfs) {
		return ErrCatalog
	}
	return nil
}

// SplitInstance separates a runtime instance into its catalog id and tenant install id. Catalog ids never contain
// a dot, so "<catalog>.<install>" is unambiguous; a bare "<catalog>" is the legacy singleton install (no install id).
func SplitInstance(instance string) (catalogID, installID string) {
	catalogID, installID, _ = strings.Cut(instance, ".")
	return
}

var installIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{7,62}[a-z0-9]$`)

func validInstallID(id string) bool { return installIDPattern.MatchString(id) }

// CatalogSource re-verifies the signed catalog file on every use, so a republished catalog takes effect without a
// restart and a tampered or expired file is refused immediately.
type CatalogSource struct {
	path  string
	trust *ecdsa.PublicKey
	now   func() time.Time
}

func NewCatalogSource(catalogPath, trustRootPath string) (*CatalogSource, error) {
	raw, err := os.ReadFile(trustRootPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, ErrCatalog
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	pub, ok := key.(*ecdsa.PublicKey)
	if err != nil || !ok || pub.Curve.Params().Name != "P-256" {
		return nil, ErrCatalog
	}
	return &CatalogSource{path: catalogPath, trust: pub, now: time.Now}, nil
}

func (c *CatalogSource) Entries() ([]CatalogEntry, error) {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return nil, ErrCatalog
	}
	sig, err := os.ReadFile(c.path + ".sig")
	if err != nil {
		return nil, ErrCatalog
	}
	digest := sha256.Sum256(raw)
	if !ecdsa.VerifyASN1(c.trust, digest[:], sig) {
		return nil, ErrCatalog
	}
	var doc struct {
		Schema    string         `json:"schema"`
		ExpiresAt int64          `json:"expires_at"`
		Plugins   []CatalogEntry `json:"plugins"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Schema != catalogSchema || c.now().Unix() >= doc.ExpiresAt {
		return nil, ErrCatalog
	}
	out := doc.Plugins[:0]
	seen := map[string]bool{}
	for _, e := range doc.Plugins {
		if e.validate() != nil || seen[e.ID] {
			continue // an entry the Agent will not run is simply not offered
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	return out, nil
}

func (c *CatalogSource) Entry(id string) (CatalogEntry, error) {
	entries, err := c.Entries()
	if err != nil {
		return CatalogEntry{}, err
	}
	for _, e := range entries {
		if e.ID == id {
			return e, nil
		}
	}
	return CatalogEntry{}, ErrPreparationPolicy
}

// ProfileBuilder turns a verified entry plus the control plane's request into a preparation profile. Everything
// secret becomes an "@secret:" placeholder resolved only when the container is created or verified.
type ProfileBuilder struct {
	Secrets  *SecretStore
	Network  string
	StackEnv func() (map[string]string, error)
}

func (b ProfileBuilder) Build(e CatalogEntry, instance string, generation uint64, values map[string]string, token string) (PreparationProfile, error) {
	catalogID, installID := SplitInstance(instance)
	if catalogID != e.ID || generation == 0 || token == "" || b.Secrets == nil {
		return PreparationProfile{}, ErrPreparationPolicy
	}
	if installID != "" && !validInstallID(installID) {
		return PreparationProfile{}, ErrPreparationPolicy
	}
	// A volume belongs to the instance string (host + instance + key). Generation, secrets and state are keyed by that
	// same string, so a bare "<catalog>" instance is exactly one install and gets exactly one volume. A control plane
	// that runs several installs of one plugin must pass "<catalog>.<installID>" so each gets its own.
	env := append([]string{}, e.Env...)
	targets := make([]string, 0, len(e.EnvFromStack))
	for t := range e.EnvFromStack {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	if len(targets) > 0 {
		stack, err := b.StackEnv()
		if err != nil {
			return PreparationProfile{}, ErrPreparationPolicy
		}
		for _, t := range targets {
			v, ok := stack[e.EnvFromStack[t]]
			if !ok || b.Secrets.Put(instance, "stack/"+t, v) != nil {
				return PreparationProfile{}, ErrPreparationPolicy
			}
			env = append(env, t+"="+secretPrefix+"stack/"+t)
		}
	}
	generated := make([]string, 0, len(e.EnvGenerated))
	for k := range e.EnvGenerated {
		generated = append(generated, k)
	}
	sort.Strings(generated)
	for _, k := range generated {
		if _, err := b.Secrets.GetOrCreateHex(instance, "generated/"+k, max(e.EnvGenerated[k]/2, 8)); err != nil {
			return PreparationProfile{}, err
		}
		env = append(env, k+"="+secretPrefix+"generated/"+k)
	}
	user := make([]string, 0, len(e.EnvUser))
	for k := range e.EnvUser {
		user = append(user, k)
	}
	sort.Strings(user)
	for _, k := range user {
		spec := e.EnvUser[k]
		if spec.Secret {
			if _, ok := b.Secrets.Get(instance, "config/"+k); !ok {
				if spec.Required {
					return PreparationProfile{}, fmt.Errorf("%w: missing %s", ErrPreparationPolicy, k)
				}
				continue
			}
			env = append(env, k+"="+secretPrefix+"config/"+k)
			continue
		}
		v := values[k]
		if v == "" {
			v = spec.Default
		}
		if v == "" {
			if spec.Required {
				return PreparationProfile{}, fmt.Errorf("%w: missing %s", ErrPreparationPolicy, k)
			}
			continue
		}
		if err := checkUserValue(spec, v); err != nil {
			return PreparationProfile{}, err
		}
		env = append(env, k+"="+v)
	}
	if err := b.Secrets.Put(instance, fmt.Sprintf("identity/%d", generation), token); err != nil {
		return PreparationProfile{}, err
	}
	env = append(env, e.tokenEnv()+"="+secretPrefix+fmt.Sprintf("identity/%d", generation), "TP_PLUGIN_SERVICE_IDENTIFIER="+e.ServiceIdentifier())
	profile := PreparationProfile{Name: fmt.Sprintf("%s.g%d", e.ID, generation), ImageID: e.ImageID, Entrypoint: e.Entrypoint,
		Args: append([]string{}, e.Args...), Network: b.Network, NetworkAlias: e.NetworkAlias(), Env: env, ReviewedImageMetadata: true}
	for _, v := range e.Volumes {
		profile.Mounts = append(profile.Mounts, PreparationMount{Key: v.Key, Destination: v.Destination, ReadOnly: v.Mode == "ro"})
	}
	for _, t := range e.Tmpfs {
		profile.Tmpfs = append(profile.Tmpfs, PreparationTmpfs{Destination: t.Destination, SizeMiB: t.SizeMiB})
	}
	if !validPreparationProfile(profile) {
		return PreparationProfile{}, ErrPreparationPolicy
	}
	return profile, nil
}

func checkUserValue(spec CatalogUserField, v string) error {
	if strings.ContainsAny(v, "\r\n\x00") {
		return ErrPreparationPolicy
	}
	if spec.Validate == "url" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return ErrPreparationPolicy
		}
	}
	if spec.Pattern != "" {
		if ok, _ := regexp.MatchString("^(?:"+spec.Pattern+")$", v); !ok {
			return ErrPreparationPolicy
		}
	}
	return nil
}

// ReadEnvFile parses KEY=VALUE lines (the stack's .env).
func ReadEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			out[k] = strings.TrimRight(v, "\r")
		}
	}
	return out, nil
}
