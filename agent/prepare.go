//go:build linux

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrPreparationRecoveryRequired = errors.New("prepared container needs owner-reviewed recovery; generic release is forbidden")
	ErrPreparationPolicy           = errors.New("container preparation is outside the configured allowlist")
)

const (
	PreparationPrepared    = "prepared"
	PreparationUnknown     = "unknown"
	PreparationNotObserved = "not_observed"
)

// PreparationProfile is trusted service configuration, never an API request.
// Only local immutable image IDs and fixed commands are accepted. Nothing in
// this adapter starts the command; the profile has no network/mount/secret knobs.
type PreparationProfile struct {
	Name       string   `json:"name"`
	ImageID    string   `json:"image_id"`
	Entrypoint string   `json:"entrypoint"`
	Args       []string `json:"args"`
	// The three fields below are reviewed operator configuration for plugins that must talk to the
	// platform. They never come from an API request and are all off by default. Network names an
	// existing user-defined Docker network (no host/bridge/none/container networks); Env is a fixed
	// list of KEY=VALUE pairs; ReviewedImageMetadata accepts the pinned image's own exposed-port and
	// environment declarations (volumes, labels and OnBuild stay forbidden, nothing is ever published).
	Network               string   `json:"network,omitempty"`
	Env                   []string `json:"env,omitempty"`
	ReviewedImageMetadata bool     `json:"reviewed_image_metadata,omitempty"`
	// NetworkAlias is a stable DNS alias on Network (e.g. tp-plugin-homeassistant), so the address the
	// platform registers survives container replacement across generations. Requires Network.
	NetworkAlias string `json:"network_alias,omitempty"`
}

type PrepareContainerRequest struct {
	Instance   string
	Generation uint64
	RequestKey string
	Profile    string
}

type PreparationObservation struct {
	State         string `json:"state"`
	Active        bool   `json:"active"` // always false; never an execution grant
	Instance      string `json:"instance"`
	Generation    uint64 `json:"generation"`
	RequestKey    string `json:"request_key"`
	PayloadHash   string `json:"payload_hash,omitempty"`
	ImageID       string `json:"image_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	ContainerID   string `json:"container_id,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
}

type containerPreparation struct {
	Version       int                `json:"version"`
	Instance      string             `json:"instance"`
	Generation    uint64             `json:"generation"`
	RequestKey    string             `json:"request_key"`
	Profile       PreparationProfile `json:"profile"`
	PayloadHash   string             `json:"payload_hash"`
	ContainerName string             `json:"container_name"`
	ContainerID   string             `json:"container_id,omitempty"`
	State         string             `json:"state"` // intent | unknown | prepared
	ErrorCode     string             `json:"error_code,omitempty"`
}

type preparationEngine interface {
	inspectImage(context.Context, string) (dockerImage, error)
	inspectContainer(context.Context, string) (dockerContainer, bool, error)
	createContainer(context.Context, string, dockerCreateRequest) (string, error)
}

type DockerPreparer struct {
	store    *Store
	engine   preparationEngine
	profiles map[string]PreparationProfile
	profMu   sync.RWMutex
	secrets  *SecretStore
}

// SetSecrets enables "@secret:" placeholders in profile environments (resolved only at create/verify time).
func (p *DockerPreparer) SetSecrets(s *SecretStore) { p.secrets = s }

func (p *DockerPreparer) profile(name string) (PreparationProfile, bool) {
	p.profMu.RLock()
	defer p.profMu.RUnlock()
	profile, ok := p.profiles[name]
	return profile, ok
}

// SetProfile registers a profile built by the Agent from the verified catalog. Names are unique per generation.
func (p *DockerPreparer) SetProfile(profile PreparationProfile) error {
	if !validPreparationProfile(profile) {
		return ErrPreparationPolicy
	}
	p.profMu.Lock()
	defer p.profMu.Unlock()
	if existing, found := p.profiles[profile.Name]; found && !reflect.DeepEqual(existing, profile) {
		return ErrResourceConflict
	}
	profile.Args = append([]string{}, profile.Args...)
	p.profiles[profile.Name] = profile
	return nil
}

// AdoptRecorded re-registers, after an Agent restart, the profiles of preparations that were recorded earlier, but
// only those the verified catalog still vouches for (same image and entrypoint) and whose recorded digest still
// matches; anything else stays unknown and therefore untrusted.
func (p *DockerPreparer) AdoptRecorded(vouches func(PreparationProfile) bool) error {
	return withLock(p.store.path, func() error {
		disk, err := load(p.store.path)
		if err != nil {
			return err
		}
		for _, record := range disk.Preparations {
			if _, known := p.profile(record.Profile.Name); known || !vouches(record.Profile) {
				continue
			}
			if preparationDigest(disk.HostID, record.request(), record.Profile) != record.PayloadHash {
				continue
			}
			_ = p.SetProfile(record.Profile)
		}
		return nil
	})
}

// resolved returns the record with secret placeholders replaced, for the create request and for verification only.
func (p *DockerPreparer) resolved(record containerPreparation) (containerPreparation, error) {
	if !hasPlaceholder(record.Profile.Env) {
		return record, nil
	}
	if p.secrets == nil {
		return record, ErrPreparationPolicy
	}
	env, err := p.secrets.ResolveEnv(record.Instance, record.Profile.Env)
	if err != nil {
		return record, err
	}
	record.Profile.Env = env
	return record, nil
}

// NewDockerPreparer enables only the bounded local Docker transport. There is
// no user-controlled URL or generic command runner and no automatic enrollment.
func NewDockerPreparer(store *Store, engine *UnixDockerEngine, profiles []PreparationProfile) (*DockerPreparer, error) {
	if engine == nil {
		return nil, ErrPreparationPolicy
	}
	return newDockerPreparer(store, engine, profiles)
}

func newDockerPreparer(store *Store, engine preparationEngine, profiles []PreparationProfile) (*DockerPreparer, error) {
	if store == nil || engine == nil || len(profiles) > 64 {
		return nil, ErrPreparationPolicy
	}
	if _, err := store.HostID(); err != nil {
		return nil, err
	}
	out := &DockerPreparer{store: store, engine: engine, profiles: map[string]PreparationProfile{}}
	for _, profile := range profiles {
		if !validPreparationProfile(profile) {
			return nil, ErrPreparationPolicy
		}
		if _, duplicate := out.profiles[profile.Name]; duplicate {
			return nil, ErrPreparationPolicy
		}
		profile.Args = append([]string{}, profile.Args...)
		out.profiles[profile.Name] = profile
	}
	return out, nil
}

func validPreparationProfile(p PreparationProfile) bool {
	if p.NetworkAlias != "" && (p.Network == "" || !validNetworkAlias(p.NetworkAlias)) {
		return false
	}
	if !valid(p.Name) || len(p.Name) > 128 || !validImageID(p.ImageID) || !strings.HasPrefix(p.Entrypoint, "/") || !valid(p.Entrypoint) || len(p.Entrypoint) > 1024 || len(p.Args) > 32 {
		return false
	}
	total := 0
	for _, arg := range p.Args {
		if strings.ContainsRune(arg, '\x00') {
			return false
		}
		total += len(arg)
	}
	if total > 8192 {
		return false
	}
	if p.Network != "" && !validPreparationNetwork(p.Network) {
		return false
	}
	if len(p.Env) > 64 {
		return false
	}
	seen := map[string]bool{}
	envTotal := 0
	for _, entry := range p.Env {
		key, _, found := strings.Cut(entry, "=")
		if !found || !validEnvKey(key) || key == "PATH" || seen[key] || strings.ContainsRune(entry, '\x00') {
			return false
		}
		seen[key] = true
		envTotal += len(entry)
	}
	return envTotal <= 16384
}

func validEnvKey(key string) bool {
	if key == "" || len(key) > 128 || (key[0] >= '0' && key[0] <= '9') {
		return false
	}
	for _, r := range key {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func validPreparationNetwork(name string) bool {
	switch name {
	case "none", "host", "bridge", "default":
		return false
	}
	if len(name) > 128 || name[0] == '-' || name[0] == '.' || name[0] == '_' {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r == '-' || r == '.' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func validImageID(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHexID(strings.TrimPrefix(value, "sha256:"))
}

func validHexID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func preparationHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func preparationName(host, instance, key string) string {
	return "tp-agent-prepared-" + preparationHash([]string{host, instance, key})[:40]
}

func preparationResource(host, instance string) string {
	// A caller cannot evade an unresolved operation by choosing another key.
	// The runtime instance owns one held resource across requests/generations.
	return "docker-instance:" + preparationHash([]string{host, instance})
}

func preparationDigest(host string, request PrepareContainerRequest, profile PreparationProfile) string {
	return "sha256:" + preparationHash(struct {
		Version int
		Host    string
		Request PrepareContainerRequest
		Profile PreparationProfile
	}{1, host, request, profile})
}

func (record containerPreparation) request() PrepareContainerRequest {
	return PrepareContainerRequest{Instance: record.Instance, Generation: record.Generation, RequestKey: record.RequestKey, Profile: record.Profile.Name}
}

func (record containerPreparation) labels(host string) map[string]string {
	return map[string]string{
		"io.thingspanel.agent.protocol":   "create-only-v1",
		"io.thingspanel.agent.host":       preparationHash(host),
		"io.thingspanel.agent.instance":   preparationHash(record.Instance),
		"io.thingspanel.agent.operation":  preparationHash(record.RequestKey),
		"io.thingspanel.agent.generation": strconv.FormatUint(record.Generation, 10),
		"io.thingspanel.agent.payload":    record.PayloadHash,
	}
}

func (record containerPreparation) observation() PreparationObservation {
	state := record.State
	if state == "intent" {
		state = PreparationUnknown
	}
	return PreparationObservation{State: state, Active: false, Instance: record.Instance, Generation: record.Generation, RequestKey: record.RequestKey, PayloadHash: record.PayloadHash, ImageID: record.Profile.ImageID, ContainerName: record.ContainerName, ContainerID: record.ContainerID, ErrorCode: record.ErrorCode}
}

// Prepare never starts or execs a container. The existing OS state lock spans
// durable intent, the one allowed create call, and receipt persistence. Thus an
// accepted generation change cannot overtake an older local create operation.
// A pre-existing intent is query-only, even when Docker reports absence: a lost
// create response may still have an outcome, so this method never retries it.
func (p *DockerPreparer) Prepare(ctx context.Context, request PrepareContainerRequest) (PreparationObservation, error) {
	observation := PreparationObservation{State: PreparationUnknown, Active: false}
	if ctx == nil || !valid(request.Instance) || !valid(request.RequestKey) || len(request.Instance) > 256 || len(request.RequestKey) > 128 || request.Generation == 0 {
		return observation, ErrPreparationPolicy
	}
	profile, allowed := p.profile(request.Profile)
	if !allowed {
		return observation, ErrPreparationPolicy
	}
	err := withLock(p.store.path, func() error {
		disk, err := load(p.store.path)
		if err != nil {
			return err
		}
		if err = preparationGeneration(disk, request.Instance, request.Generation); err != nil {
			return err
		}
		key := request.Instance + "\x00" + request.RequestKey
		digest := preparationDigest(disk.HostID, request, profile)
		if existing, found := disk.Preparations[key]; found {
			if existing.PayloadHash != digest {
				return ErrResourceConflict
			}
			observation, err = p.queryLocked(ctx, &disk, key)
			return err
		}
		if _, reserved := disk.Requests[key]; reserved {
			return ErrResourceConflict
		}
		name := preparationName(disk.HostID, request.Instance, request.RequestKey)
		resource := preparationResource(disk.HostID, request.Instance)
		if _, reserved := disk.Resources[resource]; reserved {
			return ErrResourceConflict
		}
		image, err := p.engine.inspectImage(ctx, profile.ImageID)
		if err != nil {
			return err
		}
		if !safePreparationImage(image, profile) {
			return ErrPreparationPolicy
		}
		// A container without this local durable intent is never adopted, even
		// when its name/labels appear plausible (including after lost state).
		if _, exists, err := p.engine.inspectContainer(ctx, name); err != nil {
			return err
		} else if exists {
			return ErrResourceConflict
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		record := containerPreparation{Version: 1, Instance: request.Instance, Generation: request.Generation, RequestKey: request.RequestKey, Profile: profile, PayloadHash: digest, ContainerName: name, State: "intent"}
		resolvedRecord, resolveErr := p.resolved(record) // before any state is written: a missing secret must not poison the request key
		if resolveErr != nil {
			return resolveErr
		}
		reservation := Reservation{Resource: resource, Instance: request.Instance, Generation: request.Generation, RequestKey: request.RequestKey, PayloadHash: digest, State: "held"}
		if disk.Preparations == nil {
			disk.Preparations = map[string]containerPreparation{}
		}
		disk.Preparations[key], disk.Requests[key], disk.Resources[resource] = record, reservation, reservation
		if err := writeState(p.store.path, disk); err != nil {
			return err
		}
		// No retry is issued on any create error, including timeout/409/5xx.
		id, createErr := p.engine.createContainer(ctx, name, preparationCreate(resolvedRecord, disk.HostID))
		if createErr == nil && validHexID(id) {
			record.ContainerID = id
			disk.Preparations[key] = record
		}
		observation, err = p.queryLocked(ctx, &disk, key)
		return err
	})
	if err != nil {
		observation.State, observation.Active = PreparationUnknown, false
	}
	return observation, err
}

// Query recovers by the original identity only; it cannot submit or create.
// An absent local record is an observation, never permission to use a new key.
func (p *DockerPreparer) Query(ctx context.Context, instance string, generation uint64, key string) (PreparationObservation, error) {
	observation := PreparationObservation{State: PreparationUnknown, Active: false, Instance: instance, Generation: generation, RequestKey: key}
	if ctx == nil || !valid(instance) || !valid(key) || len(instance) > 256 || len(key) > 128 || generation == 0 {
		return observation, ErrPreparationPolicy
	}
	err := withLock(p.store.path, func() error {
		disk, err := load(p.store.path)
		if err != nil {
			return err
		}
		if err = preparationGeneration(disk, instance, generation); err != nil {
			return err
		}
		identity := instance + "\x00" + key
		record, exists := disk.Preparations[identity]
		if !exists {
			if _, reserved := disk.Requests[identity]; reserved {
				return ErrResourceConflict
			}
			observation.State = PreparationNotObserved
			return nil
		}
		if record.Generation != generation {
			return ErrResourceConflict
		}
		profile, allowed := p.profile(record.Profile.Name)
		if !allowed || preparationDigest(disk.HostID, record.request(), profile) != record.PayloadHash {
			return ErrPreparationPolicy
		}
		observation, err = p.queryLocked(ctx, &disk, identity)
		return err
	})
	if err != nil {
		observation.State, observation.Active = PreparationUnknown, false
	}
	return observation, err
}

func preparationGeneration(disk diskState, instance string, generation uint64) error {
	if generation < disk.MaxGeneration[instance] {
		return ErrGenerationStale
	}
	if generation > disk.MaxGeneration[instance] {
		return ErrGenerationNotAccepted
	}
	return nil
}

func (p *DockerPreparer) queryLocked(ctx context.Context, disk *diskState, key string) (PreparationObservation, error) {
	record := disk.Preparations[key]
	container, exists, err := p.engine.inspectContainer(ctx, record.ContainerName)
	record.State, record.ErrorCode = PreparationUnknown, "CONTAINER_RESULT_UNKNOWN"
	if err == nil && !exists {
		record.ErrorCode = "CONTAINER_NOT_OBSERVED_AFTER_INTENT"
	}
	if err == nil && exists {
		if checked, resolveErr := p.resolved(record); resolveErr == nil && safePreparedContainer(container, checked, disk.HostID) {
			record.State, record.ErrorCode, record.ContainerID = PreparationPrepared, "", container.ID
		} else {
			record.ErrorCode = "CONTAINER_REQUIRES_OWNER_REVIEW"
		}
	}
	disk.Preparations[key] = record
	if err := writeState(p.store.path, *disk); err != nil {
		return PreparationObservation{State: PreparationUnknown, Active: false}, fmt.Errorf("preparation receipt persistence failed: %w", err)
	}
	return record.observation(), nil
}

func validatePreparations(disk diskState) error {
	for key, record := range disk.Preparations {
		request := record.request()
		if record.Version != 1 || key != record.Instance+"\x00"+record.RequestKey || !valid(record.Instance) || !valid(record.RequestKey) || record.Generation == 0 || !validPreparationProfile(record.Profile) || record.ContainerName != preparationName(disk.HostID, record.Instance, record.RequestKey) || record.PayloadHash != preparationDigest(disk.HostID, request, record.Profile) {
			return errors.New("invalid preparation identity")
		}
		if record.State != "intent" && record.State != PreparationUnknown && record.State != PreparationPrepared {
			return errors.New("invalid preparation state")
		}
		if record.ContainerID != "" && !validHexID(record.ContainerID) {
			return errors.New("invalid prepared container ID")
		}
		if record.State == PreparationPrepared && (record.ContainerID == "" || record.ErrorCode != "") {
			return errors.New("incomplete prepared receipt")
		}
		reservation := Reservation{Resource: preparationResource(disk.HostID, record.Instance), Instance: record.Instance, Generation: record.Generation, RequestKey: record.RequestKey, PayloadHash: record.PayloadHash, State: "held"}
		if disk.Requests[key] != reservation || disk.Resources[reservation.Resource] != reservation {
			return errors.New("preparation lost its original resource ownership")
		}
	}
	return nil
}

func safePreparationImage(image dockerImage, profile PreparationProfile) bool {
	if image.ID != profile.ImageID || image.OS != "linux" || len(image.Config.Volumes) != 0 || len(image.Config.OnBuild) != 0 || len(image.Config.Labels) != 0 {
		return false
	}
	if profile.ReviewedImageMetadata {
		// The pinned image ID is the review: its declared ports and environment are accepted,
		// but this adapter still publishes nothing and adds only the profile's own environment.
		return true
	}
	if len(image.Config.ExposedPorts) != 0 {
		return false
	}
	// Docker merges image environment into create requests. Reject any inherited
	// application variable; this adapter supplies only a fixed non-secret PATH.
	for _, env := range image.Config.Env {
		if !strings.HasPrefix(env, "PATH=") {
			return false
		}
	}
	return true
}

func safePreparedContainer(container dockerContainer, record containerPreparation, host string) bool {
	if !validHexID(container.ID) || (record.ContainerID != "" && container.ID != record.ContainerID) || container.Name != "/"+record.ContainerName || container.Image != record.Profile.ImageID || container.State.Status != "created" || container.State.Running || container.State.Paused || container.State.Restarting || container.State.Dead || container.RestartCount != 0 {
		return false
	}
	if container.State.StartedAt != "" && container.State.StartedAt != "0001-01-01T00:00:00Z" {
		return false
	}
	expected := preparationCreate(record, host)
	config, limits := container.Config, container.HostConfig
	if config.Image != expected.Image || config.User != expected.User || config.WorkingDir != expected.WorkingDir || !reflect.DeepEqual(config.Entrypoint, expected.Entrypoint) || !equalStrings(config.Cmd, expected.Cmd) || !preparedEnvMatches(config.Env, expected.Env, record.Profile) || !reflect.DeepEqual(config.Labels, expected.Labels) || config.Tty || config.OpenStdin || config.AttachStdin || config.AttachStdout || config.AttachStderr || len(config.Volumes) != 0 || (len(config.ExposedPorts) != 0 && !record.Profile.ReviewedImageMetadata) || !equalStrings(config.Healthcheck.Test, []string{"NONE"}) {
		return false
	}
	if limits.NetworkMode != preparedNetwork(record.Profile) || !limits.ReadonlyRootfs || limits.Privileged || limits.AutoRemove || limits.PublishAllPorts || !equalStrings(limits.CapDrop, []string{"ALL"}) || len(limits.CapAdd) != 0 || !equalStrings(limits.SecurityOpt, []string{"no-new-privileges:true"}) || limits.RestartPolicy.Name != "no" || limits.RestartPolicy.MaximumRetryCount != 0 || limits.IpcMode != "none" || limits.PidMode != "" || limits.UTSMode != "" || limits.UsernsMode != "" || limits.CgroupnsMode != "private" || limits.Memory != preparationMemory || limits.NanoCPUs != preparationNanoCPUs || limits.PidsLimit != preparationPids || limits.LogConfig.Type != "none" {
		return false
	}
	if len(limits.Binds) != 0 || len(limits.Mounts) != 0 || len(limits.Devices) != 0 || len(limits.DeviceRequests) != 0 || len(limits.DeviceCgroupRules) != 0 || len(limits.VolumesFrom) != 0 || len(limits.PortBindings) != 0 || len(limits.Links) != 0 || len(limits.ExtraHosts) != 0 || len(limits.GroupAdd) != 0 || len(limits.Tmpfs) != 0 || len(limits.Sysctls) != 0 || len(container.Mounts) != 0 || len(container.NetworkSettings.Ports) != 0 {
		return false
	}
	for network, endpoint := range container.NetworkSettings.Networks {
		if network != preparedNetwork(record.Profile) {
			return false
		}
		if alias := record.Profile.NetworkAlias; alias != "" && !containsString(endpoint.Aliases, alias) {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// validNetworkAlias accepts a conservative DNS label.
func validNetworkAlias(alias string) bool {
	if alias == "" || len(alias) > 63 {
		return false
	}
	for i, c := range alias {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0 && i < len(alias)-1
		if !ok {
			return false
		}
	}
	return true
}

func preparedNetwork(profile PreparationProfile) string {
	if profile.Network != "" {
		return profile.Network
	}
	return "none"
}

// preparedEnvMatches keeps the original exact comparison unless the profile reviewed the image's own
// environment; then every variable this adapter set must be present with its exact value.
func preparedEnvMatches(actual, expected []string, profile PreparationProfile) bool {
	if !profile.ReviewedImageMetadata {
		return equalStrings(actual, expected)
	}
	have := map[string]bool{}
	for _, entry := range actual {
		have[entry] = true
	}
	for _, entry := range expected {
		if !have[entry] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
