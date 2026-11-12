//go:build linux

package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeInspector struct {
	states map[string]ContainerState
	err    error
}

func (f fakeInspector) Containers(context.Context, string) (map[string]ContainerState, error) {
	return f.states, f.err
}

type fakeProber struct{ httpErr, tcpErr error }

func (f fakeProber) HTTP(context.Context, string) error { return f.httpErr }
func (f fakeProber) TCP(context.Context, string) error  { return f.tcpErr }

func testConfig() SystemConfig {
	return SystemConfig{Project: "tp", Components: []Component{
		{Name: "postgres", Group: "data"},
		{Name: "backend", Group: "control", Probe: ProbeHTTP, URL: "http://backend:9999/health"},
		{Name: "mosquitto", Group: "broker", Probe: ProbeTCP, Addr: "mosquitto:1883"},
	}}
}

func up() ContainerState { return ContainerState{Found: true, Running: true, Health: "healthy"} }

func TestVerifyAllHealthy(t *testing.T) {
	v := &Verifier{Config: testConfig(), Inspector: fakeInspector{states: map[string]ContainerState{"postgres": up(), "backend": up(), "mosquitto": up()}}, Prober: fakeProber{}}
	r := v.Once(context.Background())
	if !r.OK || len(r.Components) != 3 {
		t.Fatalf("%+v", r)
	}
}

// Stopping the database must be reported against the database, and the dependent that then fails its probe
// must be reported as well: the caller sees every broken component, not only the first.
func TestVerifyNamesEveryBrokenComponent(t *testing.T) {
	states := map[string]ContainerState{
		"postgres": {Found: true, Running: false},
		"backend":  up(),
		// mosquitto container is absent
	}
	v := &Verifier{Config: testConfig(), Inspector: fakeInspector{states: states}, Prober: fakeProber{httpErr: errors.New("status 503 host=secret")}}
	r := v.Once(context.Background())
	if r.OK {
		t.Fatal("must fail")
	}
	got := map[string]ComponentStatus{}
	for _, c := range r.Components {
		got[c.Name] = c
	}
	if got["postgres"].Reason != ReasonNotRunning || got["backend"].Reason != ReasonProbeFailed || got["mosquitto"].Reason != ReasonMissing {
		t.Fatalf("%+v", got)
	}
	for _, c := range r.Components {
		if strings.Contains(c.Reason, "secret") || strings.Contains(c.Reason, "backend:") {
			t.Fatal("error text or address leaked")
		}
	}
}

func TestVerifyHealthStatesAndDockerFailure(t *testing.T) {
	cfg := SystemConfig{Project: "tp", Components: []Component{{Name: "a", Group: "control"}, {Name: "b", Group: "control"}, {Name: "c", Group: "control"}}}
	v := &Verifier{Config: cfg, Inspector: fakeInspector{states: map[string]ContainerState{
		"a": {Found: true, Running: true, Health: "starting"},
		"b": {Found: true, Running: true, Health: "unhealthy"},
		"c": {Found: true, Running: true, Restarting: true},
	}}}
	r := v.Once(context.Background())
	byName := map[string]ComponentStatus{}
	for _, c := range r.Components {
		byName[c.Name] = c
	}
	if byName["a"].State != StateStarting || byName["b"].Reason != ReasonUnhealthy || byName["c"].Reason != ReasonRestarting {
		t.Fatalf("%+v", byName)
	}
	v.Inspector = fakeInspector{err: errors.New("dial unix /var/run/docker.sock")}
	r = v.Once(context.Background())
	if r.OK || r.Components[0].Reason != ReasonDockerUnreadable {
		t.Fatalf("%+v", r)
	}
}

func TestWaitReturnsWhenHealthyAndStopsAtBudget(t *testing.T) {
	calls := 0
	insp := &flipInspector{after: 3, good: map[string]ContainerState{"postgres": up(), "backend": up(), "mosquitto": up()}}
	v := &Verifier{Config: testConfig(), Inspector: insp, Prober: fakeProber{}, Sleep: func(time.Duration) { calls++ }}
	r := v.Wait(context.Background(), time.Hour)
	if !r.OK || calls != 3 {
		t.Fatalf("ok=%v sleeps=%d", r.OK, calls)
	}
	v = &Verifier{Config: testConfig(), Inspector: fakeInspector{states: map[string]ContainerState{}}, Prober: fakeProber{}, Sleep: func(time.Duration) {}}
	if r := v.Wait(context.Background(), 0); r.OK {
		t.Fatal("zero budget is one check")
	}
}

type flipInspector struct {
	n, after int
	good     map[string]ContainerState
}

func (f *flipInspector) Containers(context.Context, string) (map[string]ContainerState, error) {
	f.n++
	if f.n > f.after {
		return f.good, nil
	}
	return map[string]ContainerState{}, nil
}

func TestConfigValidation(t *testing.T) {
	bad := []SystemConfig{
		{},
		{Project: "Bad Name", Components: testConfig().Components},
		{Project: "tp", Components: []Component{{Name: "a", Group: "x"}}},
		{Project: "tp", Components: []Component{{Name: "a", Group: "data"}, {Name: "a", Group: "data"}}},
		{Project: "tp", Components: []Component{{Name: "a", Group: "data", Probe: ProbeHTTP, URL: "file:///etc/passwd"}}},
		{Project: "tp", Components: []Component{{Name: "a; rm", Group: "data"}}},
		{Project: "tp", Components: []Component{{Name: "a", Group: "data", URL: "http://x"}}},
	}
	for i, c := range bad {
		if c.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSystemConfigRejectsWritableAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system.json")
	body := `{"project":"tp","components":[{"name":"postgres","group":"data"}]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSystemConfig(path); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0666)
	if _, err := LoadSystemConfig(path); err == nil {
		t.Fatal("group/world-writable config accepted")
	}
	_ = os.Chmod(path, 0600)
	_ = os.WriteFile(path, []byte(`{"project":"tp","compose_file":"/x","components":[{"name":"postgres","group":"data"}]}`), 0600)
	if _, err := LoadSystemConfig(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

const sysToken = "system-token-system-token-system-0001"
const pluginToken = "plugin-token-plugin-token-plugin-0002"

func newSystemServer(t *testing.T) *httptest.Server {
	t.Helper()
	v := &Verifier{Config: testConfig(), Inspector: fakeInspector{states: map[string]ContainerState{"postgres": up(), "backend": up(), "mosquitto": up()}}, Prober: fakeProber{}}
	svc, err := NewSystemService(sysToken, v, "test")
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(svc.Handler())
}

func post(t *testing.T, base, path, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestSystemAPIAuthAndRoutes(t *testing.T) {
	srv := newSystemServer(t)
	defer srv.Close()
	if c := post(t, srv.URL, "/v1/system/status", sysToken, "{}").StatusCode; c != 200 {
		t.Fatalf("status %d", c)
	}
	if c := post(t, srv.URL, "/v1/system/verify", sysToken, `{"wait_seconds":0}`).StatusCode; c != 200 {
		t.Fatalf("verify %d", c)
	}
	// The plugin token, no token and a wrong token are all rejected.
	for _, tok := range []string{pluginToken, "", "x"} {
		if c := post(t, srv.URL, "/v1/system/status", tok, "{}").StatusCode; c != 401 {
			t.Fatalf("token %q got %d", tok, c)
		}
	}
	// Anything that is not a fixed action does not exist, even with the right token.
	for _, p := range []string{"/v1/prepare", "/v1/system/exec", "/v1/system/restart", "/v1/status"} {
		if c := post(t, srv.URL, p, sysToken, "{}").StatusCode; c != 404 {
			t.Fatalf("%s got %d", p, c)
		}
	}
	if c := post(t, srv.URL, "/v1/system/verify", sysToken, `{"wait_seconds":99999}`).StatusCode; c != 400 {
		t.Fatalf("range %d", c)
	}
	if c := post(t, srv.URL, "/v1/system/status", sysToken, `{"command":"rm -rf /"}`).StatusCode; c != 400 {
		t.Fatalf("unknown field %d", c)
	}
}

// The plugin role must not serve any system route, and must reject the system token.
func TestPluginHandlerHasNoSystemRoutes(t *testing.T) {
	svc := &Service{token: []byte(pluginToken)}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	if c := post(t, srv.URL, "/v1/system/status", pluginToken, "{}").StatusCode; c != 404 {
		t.Fatalf("plugin socket serves a system route: %d", c)
	}
	if c := post(t, srv.URL, "/v1/status", sysToken, "{}").StatusCode; c != 401 {
		t.Fatalf("plugin socket accepted the system token: %d", c)
	}
}

func TestEnsureDistinctTokens(t *testing.T) {
	if EnsureDistinctTokens(sysToken, sysToken) == nil || EnsureDistinctTokens(sysToken, pluginToken) != nil {
		t.Fatal("token distinctness")
	}
}
