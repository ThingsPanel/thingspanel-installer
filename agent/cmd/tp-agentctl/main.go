//go:build linux

// tp-agentctl posts one request to the local tp-agent socket: tp-agentctl -socket S -token-file F /v1/status instance=x generation=1
//
// System role (use the system socket and system token file): tp-agentctl -socket SYS -token-file SYSTOK system status
// and tp-agentctl -socket SYS -token-file SYSTOK system verify [wait_seconds=N]. Exit status 0 means the stack is healthy.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	socket := flag.String("socket", "", "tp-agent Unix socket")
	tokenFile := flag.String("token-file", "", "bearer token file")
	flag.Parse()
	if flag.NArg() < 1 || *socket == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "usage: tp-agentctl -socket S -token-file F /v1/<endpoint> [key=value ...]")
		os.Exit(2)
	}
	endpoint, args := flag.Arg(0), flag.Args()[1:]
	system := false
	if endpoint == "system" {
		sub := flag.Arg(1)
		if sub == "backup" && flag.NArg() >= 3 {
			sub = "backup/" + flag.Arg(2)
		}
		valid := map[string]bool{"status": true, "verify": true, "restart": true, "backup/create": true, "backup/list": true, "backup/verify": true, "backup/delete": true}
		if flag.NArg() < 2 || !valid[sub] {
			fmt.Fprintln(os.Stderr, "usage: tp-agentctl -socket SYS -token-file F system status|verify|restart [wait_seconds=N] [scope=control|full] | system backup create|list|verify|delete [name=...]")
			os.Exit(2)
		}
		skip := 2
		if strings.HasPrefix(sub, "backup/") {
			skip = 3
		}
		endpoint, args, system = "/v1/system/"+sub, flag.Args()[skip:], true
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	body := map[string]any{}
	for _, pair := range args {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "arguments must be key=value")
			os.Exit(2)
		}
		if number, err := strconv.ParseUint(value, 10, 64); err == nil && (key == "generation" || key == "ttl_seconds" || key == "wait_seconds") {
			body[key] = number
		} else {
			body[key] = value
		}
	}
	payload, _ := json.Marshal(body)
	client := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
	}}}
	request, _ := http.NewRequest(http.MethodPost, "http://agent"+endpoint, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(raw)))
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	fmt.Printf("%d %s", response.StatusCode, out)
	if response.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	if system && (endpoint == "/v1/system/restart" || endpoint == "/v1/system/backup/create" || strings.HasPrefix(endpoint, "/v1/system/backup/")) {
		return // accepted; poll `system status` for progress
	}
	if system {
		var parsed struct {
			Result *struct {
				OK bool `json:"ok"`
			} `json:"result"`
		}
		if json.Unmarshal(out, &parsed) != nil || parsed.Result == nil || !parsed.Result.OK {
			os.Exit(1) // the request worked but the stack is not healthy
		}
	}
}
