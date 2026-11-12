//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ExecStream runs argv in the running container of a compose service and copies its stdout to stdout. Docker
// multiplexes stdout and stderr over one stream; this demultiplexes it. The last 2 KiB of stderr are returned
// for diagnostics only (fixed commands, so it carries no request data). The exit code comes from exec inspect.
func (d *DockerStackInspector) ExecStream(ctx context.Context, project, service string, argv []string, stdout io.Writer) (int, string, error) {
	id, err := d.containerID(ctx, project, service)
	if err != nil {
		return -1, "", err
	}
	raw, _, err := d.post(ctx, "/containers/"+url.PathEscape(id)+"/exec", map[string]any{"Cmd": argv, "AttachStdout": true, "AttachStderr": true}, 201)
	if err != nil {
		return -1, "", err
	}
	var created struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(raw, &created) != nil || created.ID == "" {
		return -1, "", errors.New("exec was not created")
	}
	payload, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+dockerPreparationAPI+"/exec/"+url.PathEscape(created.ID)+"/start", bytes.NewReader(payload))
	if err != nil {
		return -1, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := *d.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return -1, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, "", fmt.Errorf("docker answered %d", resp.StatusCode)
	}
	stderrTail, err := demux(resp.Body, stdout)
	if err != nil {
		return -1, stderrTail, err
	}
	var inspected struct {
		Running  bool
		ExitCode int
	}
	if err := d.get(ctx, "/exec/"+url.PathEscape(created.ID)+"/json", &inspected); err != nil {
		return -1, stderrTail, err
	}
	return inspected.ExitCode, stderrTail, nil
}

// demux splits Docker's multiplexed stream (8-byte header: stream type, 3 zero bytes, big-endian length).
func demux(r io.Reader, stdout io.Writer) (string, error) {
	var tail bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF {
				return tail.String(), nil
			}
			return tail.String(), err
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		switch header[0] {
		case 1:
			if _, err := io.CopyN(stdout, r, size); err != nil {
				return tail.String(), err
			}
		default: // stderr (2) and anything else is kept only as a short tail
			var buf bytes.Buffer
			if _, err := io.CopyN(&buf, r, size); err != nil {
				return tail.String(), err
			}
			tail.Write(buf.Bytes())
			if tail.Len() > 2048 {
				s := tail.String()
				tail.Reset()
				tail.WriteString(s[len(s)-2048:])
			}
		}
	}
}

// CopyFrom streams a tar archive of path out of a container (it may be stopped), like `docker cp src -`.
func (d *DockerStackInspector) CopyFrom(ctx context.Context, project, service, path string, w io.Writer) error {
	id, err := d.containerID(ctx, project, service)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+dockerPreparationAPI+"/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(path), nil)
	if err != nil {
		return err
	}
	client := *d.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoContainer
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker answered %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// CopyTo extracts a tar stream into dir inside a container. Used to place dump files in the PostgreSQL
// container and to restore the uploaded files; the destination is always a fixed path from the system config.
func (d *DockerStackInspector) CopyTo(ctx context.Context, project, service, dir string, r io.Reader) error {
	id, err := d.containerID(ctx, project, service)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://docker"+dockerPreparationAPI+"/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(dir), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	client := *d.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker answered %d", resp.StatusCode)
	}
	return nil
}
