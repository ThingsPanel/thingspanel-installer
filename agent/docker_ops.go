//go:build linux

package agent

// Docker Engine operations the upgrade needs beyond start/stop/exec (R5b): pull an image, and run a helper
// container to completion (the compose CLI image, with fixed argv). Nothing here accepts a request-supplied
// command, image or mount: callers pass values derived from the system config and the staged release.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ImageRegistry pulls images and tells whether one is present locally.
type ImageRegistry interface {
	HaveImage(ctx context.Context, ref string) (bool, error)
	Pull(ctx context.Context, ref string) error
}

func (d *DockerStackInspector) HaveImage(ctx context.Context, ref string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+dockerPreparationAPI+"/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return false, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("docker answered %d", resp.StatusCode)
}

// Pull fetches ref (anonymous; the stack's registries are public). The progress stream carries errors in-band.
func (d *DockerStackInspector) Pull(ctx context.Context, ref string) error {
	repo, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") && i > 0 {
		repo, tag = ref[:i], ref[i+1:]
	}
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+dockerPreparationAPI+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	client := *d.client
	client.Timeout = 30 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("docker answered %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Error != "" {
			return errors.New("image pull failed")
		}
	}
}

// HelperSpec describes a one-shot container. Binds are "host:container[:ro]".
type HelperSpec struct {
	Image      string
	Entrypoint []string
	Cmd        []string
	Binds      []string
	WorkingDir string
}

// HelperRunner runs a one-shot container to completion and returns its exit code and the tail of its output.
type HelperRunner interface {
	RunHelper(ctx context.Context, spec HelperSpec) (int, string, error)
}

func (d *DockerStackInspector) engine(ctx context.Context, method, path string, body any, long bool) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+dockerPreparationAPI+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *d.client
	if long {
		client.Timeout = 0
	}
	return client.Do(req)
}

func (d *DockerStackInspector) RunHelper(ctx context.Context, spec HelperSpec) (int, string, error) {
	create := map[string]any{"Image": spec.Image, "Entrypoint": spec.Entrypoint, "Cmd": spec.Cmd, "WorkingDir": spec.WorkingDir,
		"Labels":     map[string]string{"io.thingspanel.agent.helper": "1"},
		"HostConfig": map[string]any{"Binds": spec.Binds, "AutoRemove": false, "NetworkMode": "none"}}
	resp, err := d.engine(ctx, http.MethodPost, "/containers/create", create, false)
	if err != nil {
		return -1, "", err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusCreated || created.ID == "" {
		return -1, "", fmt.Errorf("helper container could not be created (%d)", resp.StatusCode)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if r, err := d.engine(cctx, http.MethodDelete, "/containers/"+created.ID+"?force=true", nil, false); err == nil {
			r.Body.Close()
		}
	}()
	resp, err = d.engine(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, false)
	if err != nil {
		return -1, "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return -1, "", fmt.Errorf("helper container did not start (%d)", resp.StatusCode)
	}
	resp, err = d.engine(ctx, http.MethodPost, "/containers/"+created.ID+"/wait", nil, true)
	if err != nil {
		return -1, "", err
	}
	var waited struct {
		StatusCode int `json:"StatusCode"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&waited)
	resp.Body.Close()
	if err != nil {
		return -1, "", err
	}
	tail := ""
	if r, err := d.engine(ctx, http.MethodGet, "/containers/"+created.ID+"/logs?stdout=1&stderr=1&tail=40", nil, false); err == nil {
		var buf bytes.Buffer
		stderr, _ := demux(r.Body, &buf)
		r.Body.Close()
		tail = stderr + buf.String()
		if len(tail) > 1000 {
			tail = tail[len(tail)-1000:]
		}
	}
	return waited.StatusCode, tail, nil
}
