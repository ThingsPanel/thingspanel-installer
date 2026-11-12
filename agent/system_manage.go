//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// StackManager is the system role's whole write surface on Docker: stop and start containers that belong to
// the configured compose project, and run the one configured check command. It cannot create, remove, pull,
// or exec anything else, and the caller names compose services, never container ids or commands.
type StackManager interface {
	StackInspector
	Stop(ctx context.Context, project, service string, grace time.Duration) error
	Start(ctx context.Context, project, service string) error
	// Exec runs argv inside the running container of service and returns its exit code.
	Exec(ctx context.Context, project, service string, argv []string) (int, error)
}

var ErrNoContainer = errors.New("no container for that service")

func (d *DockerStackInspector) containerID(ctx context.Context, project, service string) (string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project, "com.docker.compose.service=" + service}})
	var listed []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := d.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &listed); err != nil {
		return "", err
	}
	for _, c := range listed {
		if c.Labels["com.docker.compose.oneoff"] != "True" {
			return c.ID, nil
		}
	}
	return "", ErrNoContainer
}

func (d *DockerStackInspector) post(ctx context.Context, path string, body any, okCodes ...int) ([]byte, int, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+dockerPreparationAPI+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := *d.client
	client.Timeout = 0 // stop may take the grace period; the context bounds it
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(http.MaxBytesReader(nil, resp.Body, 1<<20))
	for _, c := range okCodes {
		if resp.StatusCode == c {
			return buf.Bytes(), resp.StatusCode, nil
		}
	}
	return nil, resp.StatusCode, fmt.Errorf("docker answered %d", resp.StatusCode)
}

func (d *DockerStackInspector) Stop(ctx context.Context, project, service string, grace time.Duration) error {
	id, err := d.containerID(ctx, project, service)
	if err != nil {
		return err
	}
	_, _, err = d.post(ctx, fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(id), int(grace.Seconds())), nil, 204, 304)
	return err
}

func (d *DockerStackInspector) Start(ctx context.Context, project, service string) error {
	id, err := d.containerID(ctx, project, service)
	if err != nil {
		return err
	}
	_, _, err = d.post(ctx, "/containers/"+url.PathEscape(id)+"/start", nil, 204, 304)
	return err
}

func (d *DockerStackInspector) Exec(ctx context.Context, project, service string, argv []string) (int, error) {
	id, err := d.containerID(ctx, project, service)
	if err != nil {
		return -1, err
	}
	raw, _, err := d.post(ctx, "/containers/"+url.PathEscape(id)+"/exec", map[string]any{"Cmd": argv, "AttachStdout": false, "AttachStderr": false}, 201)
	if err != nil {
		return -1, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(raw, &created) != nil || created.ID == "" {
		return -1, errors.New("exec was not created")
	}
	if _, _, err := d.post(ctx, "/exec/"+url.PathEscape(created.ID)+"/start", map[string]any{"Detach": true}, 200); err != nil {
		return -1, err
	}
	for {
		var inspected struct {
			Running  bool
			ExitCode int
		}
		if err := d.get(ctx, "/exec/"+url.PathEscape(created.ID)+"/json", &inspected); err != nil {
			return -1, err
		}
		if !inspected.Running {
			return inspected.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
