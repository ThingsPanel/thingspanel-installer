//go:build linux

// tp-agentctl posts one request to the local tp-agent socket: tp-agentctl -socket S -token-file F /v1/status instance=x generation=1
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
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	body := map[string]any{}
	for _, pair := range flag.Args()[1:] {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "arguments must be key=value")
			os.Exit(2)
		}
		if number, err := strconv.ParseUint(value, 10, 64); err == nil && (key == "generation" || key == "ttl_seconds") {
			body[key] = number
		} else {
			body[key] = value
		}
	}
	payload, _ := json.Marshal(body)
	client := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
	}}}
	request, _ := http.NewRequest(http.MethodPost, "http://agent"+flag.Arg(0), bytes.NewReader(payload))
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
}
