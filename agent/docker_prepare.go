//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	preparationMemory    int64 = 64 * 1024 * 1024
	preparationNanoCPUs  int64 = 100000000
	preparationPids      int64 = 16
	preparationPath            = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	dockerPreparationAPI       = "/v1.44"
)

// UnixDockerEngine speaks only to an operator-selected local Unix socket.
// There are no exported generic requests, registry pulls, start/exec, removal,
// network, volume, or device operations. Unsupported API versions fail closed.
type UnixDockerEngine struct{ client *http.Client }

func NewUnixDockerEngine(socketPath string) (*UnixDockerEngine, error) {
	if !filepath.IsAbs(socketPath) {
		return nil, errors.New("an absolute trusted Docker socket is required")
	}
	resolved, err := filepath.EvalSymlinks(socketPath)
	if err != nil {
		return nil, errors.New("Docker socket is unavailable")
	}
	info, err := os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0002 != 0 {
		return nil, errors.New("Docker endpoint must be a non-world-writable local socket")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", resolved)
	}, DisableKeepAlives: true}
	return &UnixDockerEngine{client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type dockerHealthcheck struct{ Test []string }
type dockerConfig struct {
	Image           string
	User            string
	WorkingDir      string
	Entrypoint      []string
	Cmd             []string
	Env             []string
	Labels          map[string]string
	Volumes         map[string]json.RawMessage
	ExposedPorts    map[string]json.RawMessage
	OnBuild         []string
	Healthcheck     dockerHealthcheck
	NetworkDisabled bool
	Tty             bool
	OpenStdin       bool
	AttachStdin     bool
	AttachStdout    bool
	AttachStderr    bool
}
type dockerImage struct {
	ID     string `json:"Id"`
	OS     string `json:"Os"`
	Config dockerConfig
}
type dockerRestartPolicy struct {
	Name              string
	MaximumRetryCount int
}
type dockerLogConfig struct {
	Type   string
	Config map[string]string
}
type dockerHostConfig struct {
	NetworkMode       string
	ReadonlyRootfs    bool
	Privileged        bool
	AutoRemove        bool
	PublishAllPorts   bool
	CapDrop           []string
	CapAdd            []string
	SecurityOpt       []string
	RestartPolicy     dockerRestartPolicy
	IpcMode           string
	PidMode           string
	UTSMode           string
	UsernsMode        string
	CgroupnsMode      string
	Memory            int64
	NanoCPUs          int64
	PidsLimit         int64
	LogConfig         dockerLogConfig
	Binds             []string
	Mounts            []json.RawMessage
	Devices           []json.RawMessage
	DeviceRequests    []json.RawMessage
	DeviceCgroupRules []string
	VolumesFrom       []string
	PortBindings      map[string]json.RawMessage
	Links             []string
	ExtraHosts        []string
	GroupAdd          []string
	Tmpfs             map[string]string
	Sysctls           map[string]string
}
type dockerEndpointConfig struct {
	Aliases []string `json:",omitempty"`
}
type dockerNetworkingConfig struct {
	EndpointsConfig map[string]dockerEndpointConfig
}
type dockerCreateRequest struct {
	dockerConfig
	HostConfig       dockerHostConfig
	NetworkingConfig *dockerNetworkingConfig `json:",omitempty"`
}
type dockerContainer struct {
	ID           string `json:"Id"`
	Name         string
	Image        string
	Config       dockerConfig
	HostConfig   dockerHostConfig
	RestartCount int
	State        struct {
		Status     string
		Running    bool
		Paused     bool
		Restarting bool
		Dead       bool
		StartedAt  string
	}
	Mounts          []json.RawMessage
	NetworkSettings struct {
		Ports    map[string]json.RawMessage
		Networks map[string]struct{ Aliases []string }
	}
}

func preparationCreate(record containerPreparation, host string) dockerCreateRequest {
	request := preparationCreateBase(record, host)
	if alias := record.Profile.NetworkAlias; alias != "" {
		request.NetworkingConfig = &dockerNetworkingConfig{EndpointsConfig: map[string]dockerEndpointConfig{record.Profile.Network: {Aliases: []string{alias}}}}
	}
	return request
}

func preparationCreateBase(record containerPreparation, host string) dockerCreateRequest {
	return dockerCreateRequest{
		dockerConfig: dockerConfig{Image: record.Profile.ImageID, User: "65534:65534", WorkingDir: "/", Entrypoint: []string{record.Profile.Entrypoint}, Cmd: append([]string{}, record.Profile.Args...), Env: append([]string{preparationPath}, record.Profile.Env...), Labels: record.labels(host), Volumes: map[string]json.RawMessage{}, ExposedPorts: map[string]json.RawMessage{}, Healthcheck: dockerHealthcheck{Test: []string{"NONE"}}, NetworkDisabled: record.Profile.Network == ""},
		HostConfig:   dockerHostConfig{NetworkMode: preparedNetwork(record.Profile), ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, RestartPolicy: dockerRestartPolicy{Name: "no"}, IpcMode: "none", CgroupnsMode: "private", Memory: preparationMemory, NanoCPUs: preparationNanoCPUs, PidsLimit: preparationPids, LogConfig: dockerLogConfig{Type: "none"}, Binds: []string{}, Mounts: []json.RawMessage{}, Devices: []json.RawMessage{}, DeviceRequests: []json.RawMessage{}, PortBindings: map[string]json.RawMessage{}},
	}
}

func (engine *UnixDockerEngine) inspectImage(ctx context.Context, imageID string) (dockerImage, error) {
	var image dockerImage
	if !validImageID(imageID) {
		return image, ErrPreparationPolicy
	}
	status, err := engine.call(ctx, http.MethodGet, "/images/"+url.PathEscape(imageID)+"/json", nil, &image)
	if err != nil {
		return image, err
	}
	if status != http.StatusOK {
		return image, fmt.Errorf("Docker image inspection returned status %d", status)
	}
	return image, nil
}

func validPreparationName(name string) bool {
	if !strings.HasPrefix(name, "tp-agent-prepared-") || len(name) != len("tp-agent-prepared-")+40 {
		return false
	}
	for _, c := range strings.TrimPrefix(name, "tp-agent-prepared-") {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (engine *UnixDockerEngine) inspectContainer(ctx context.Context, name string) (dockerContainer, bool, error) {
	var container dockerContainer
	if !validPreparationName(name) {
		return container, false, ErrPreparationPolicy
	}
	status, err := engine.call(ctx, http.MethodGet, "/containers/"+name+"/json", nil, &container)
	if err != nil {
		return container, false, err
	}
	if status == http.StatusNotFound {
		return container, false, nil
	}
	if status != http.StatusOK {
		return container, false, fmt.Errorf("Docker container inspection returned status %d", status)
	}
	return container, true, nil
}

func (engine *UnixDockerEngine) createContainer(ctx context.Context, name string, request dockerCreateRequest) (string, error) {
	if !validPreparationName(name) || !validImageID(request.Image) {
		return "", ErrPreparationPolicy
	}
	var created struct {
		ID string `json:"Id"`
	}
	status, err := engine.call(ctx, http.MethodPost, "/containers/create?"+url.Values{"name": {name}}.Encode(), request, &created)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated || !validHexID(created.ID) {
		return "", fmt.Errorf("Docker create result unconfirmed (status %d)", status)
	}
	return created.ID, nil
}

func (engine *UnixDockerEngine) call(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+dockerPreparationAPI+path, body)
	if err != nil {
		return 0, errors.New("invalid Docker preparation request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := engine.client.Do(request)
	if err != nil {
		return 0, errors.New("Docker preparation response is unknown")
	}
	defer response.Body.Close()
	// Error bodies may contain daemon paths or workload data; never propagate
	// them. Only successful responses are parsed, with a strict size bound.
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return response.StatusCode, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return response.StatusCode, errors.New("Docker preparation response is incomplete or oversized")
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return response.StatusCode, errors.New("Docker preparation response is invalid")
	}
	return response.StatusCode, nil
}
