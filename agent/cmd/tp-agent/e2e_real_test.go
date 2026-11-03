//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in end to end: the real tp-agent binary, a real Unix socket and the real Docker Engine.
// TP_AGENT_E2E_BIN is the built tp-agent; TP_AGENT_DOCKER_IMAGE_ID a reviewed local image whose only
// program is /pluginstub; TP_AGENT_DOCKER_SOCKET the engine socket.
func TestServiceEndToEndRealDocker(t *testing.T) {
	bin, image, dockerSocket := os.Getenv("TP_AGENT_E2E_BIN"), os.Getenv("TP_AGENT_DOCKER_IMAGE_ID"), os.Getenv("TP_AGENT_DOCKER_SOCKET")
	if bin == "" || image == "" || dockerSocket == "" {
		t.Skip("TP_AGENT_E2E_BIN, TP_AGENT_DOCKER_IMAGE_ID and TP_AGENT_DOCKER_SOCKET are required")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, socket, tokenFile, profilesFile := filepath.Join(dir, "state.json"), filepath.Join(dir, "agent.sock"), filepath.Join(dir, "token"), filepath.Join(dir, "profiles.json")
	token := "0123456789abcdef0123456789abcdef-e2e"
	os.WriteFile(tokenFile, []byte(token), 0600)
	profile := []map[string]any{{"name": "stub", "image_id": image, "entrypoint": "/pluginstub", "args": []string{}}}
	raw, _ := json.Marshal(profile)
	os.WriteFile(profilesFile, raw, 0600)
	if out, err := exec.Command(bin, "-state", state, "-init", "e2e-host-"+time.Now().Format("150405")).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	cmd := exec.Command(bin, "-state", state, "-socket", socket, "-token-file", tokenFile, "-profiles", profilesFile, "-docker-socket", dockerSocket, "-sweep-interval", "1s")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, Timeout: 90 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not start listening")
		}
		time.Sleep(100 * time.Millisecond)
	}
	call := func(path, bearer string, body map[string]any) (int, map[string]any) {
		payload, _ := json.Marshal(body)
		request, _ := http.NewRequest(http.MethodPost, "http://agent"+path, bytes.NewReader(payload))
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var out map[string]any
		json.NewDecoder(response.Body).Decode(&out)
		return response.StatusCode, out
	}
	req := func(gen int, key string) map[string]any {
		return map[string]any{"instance": "plugin-1", "generation": gen, "request_key": key, "profile": "stub", "grant_id": "g-" + key, "ttl_seconds": 120}
	}
	if code, _ := call("/v1/status", "", req(1, "k1")); code != http.StatusUnauthorized {
		t.Fatalf("a request without the token must be unauthorized, got %d", code)
	}
	if code, _ := call("/v1/status", "wrong-token-wrong-token-wrong-token-xx", req(1, "k1")); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token must be unauthorized, got %d", code)
	}
	steps := []struct {
		path string
		gen  int
		key  string
		want int
	}{
		{"/v1/accept-generation", 1, "k1", 200},
		{"/v1/prepare", 1, "k1", 200},
		{"/v1/activate", 1, "k1", 409}, // not granted yet: nothing may start
		{"/v1/grant", 1, "k1", 200},
		{"/v1/activate", 1, "k1", 200},
		{"/v1/accept-generation", 2, "k2", 200},
		{"/v1/activate", 1, "k1", 409}, // old generation task arrives late
		{"/v1/grant", 1, "k1", 409},
	}
	for i, step := range steps {
		code, body := call(step.path, token, req(step.gen, step.key))
		if code != step.want {
			t.Fatalf("step %d %s gen %d: want %d got %d %v", i, step.path, step.gen, step.want, code, body)
		}
	}
	// The sweeper stops the superseded, still-running generation 1 without any call.
	stopped := false
	for i := 0; i < 20 && !stopped; i++ {
		time.Sleep(500 * time.Millisecond)
		_, body := call("/v1/status", token, req(1, "k1"))
		if activation, ok := body["activation"].(map[string]any); ok && activation["revoked"] == true && activation["started"] == false {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("the sweeper must stop and revoke the superseded instance")
	}
	if code, body := call("/v1/retire", token, req(1, "k1")); code != 200 {
		t.Fatalf("retire: %d %v", code, body)
	}
	for _, step := range []struct {
		path string
		want int
	}{{"/v1/prepare", 200}, {"/v1/grant", 200}, {"/v1/activate", 200}} {
		if code, body := call(step.path, token, req(2, "k2")); code != step.want {
			t.Fatalf("generation 2 %s: want %d got %d %v", step.path, step.want, code, body)
		}
	}
	if code, body := call("/v1/revoke", token, req(2, "k2")); code != 200 {
		t.Fatalf("revoke: %d %v", code, body)
	}
	if code, _ := call("/v1/activate", token, req(2, "k2")); code != 409 {
		t.Fatalf("a revoked generation must not start again, got %d", code)
	}
}
