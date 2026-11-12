//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// DockerStackInspector reads container state over the local Docker socket. It issues only two GET requests
// (list by compose project label, inspect) and exposes nothing else.
type DockerStackInspector struct{ client *http.Client }

func NewDockerStackInspector(socketPath string) (*DockerStackInspector, error) {
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
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", resolved)
	}}
	return &DockerStackInspector{client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (d *DockerStackInspector) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+dockerPreparationAPI+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker answered %d", resp.StatusCode)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(out)
}

func (d *DockerStackInspector) Containers(ctx context.Context, project string) (map[string]ContainerState, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	var listed []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := d.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &listed); err != nil {
		return nil, err
	}
	out := map[string]ContainerState{}
	for _, c := range listed {
		service := c.Labels["com.docker.compose.service"]
		if service == "" || c.Labels["com.docker.compose.oneoff"] == "True" {
			continue
		}
		var inspected struct {
			State struct {
				Running    bool
				Restarting bool
				Health     *struct{ Status string }
			}
		}
		if err := d.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/json", &inspected); err != nil {
			return nil, err
		}
		state := ContainerState{Found: true, Running: inspected.State.Running, Restarting: inspected.State.Restarting}
		if inspected.State.Health != nil {
			state.Health = inspected.State.Health.Status
		}
		// Several containers of one service (scaled) count as the worst one.
		if prev, ok := out[service]; ok && !worse(state, prev) {
			continue
		}
		out[service] = state
	}
	return out, nil
}

func worse(a, b ContainerState) bool {
	rank := func(c ContainerState) int {
		switch {
		case c.Restarting:
			return 3
		case !c.Running:
			return 3
		case c.Health == "unhealthy":
			return 2
		case c.Health == "starting":
			return 1
		}
		return 0
	}
	return rank(a) > rank(b)
}
