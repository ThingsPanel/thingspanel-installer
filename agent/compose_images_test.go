//go:build linux

package agent

import "testing"

func TestComposeServiceImages(t *testing.T) {
	text := "services:\n  backend:\n    image: ${TP_BACKEND_IMAGE:-reg/x:v1.3.0}\n  frontend:\n    image: reg/web:v1.3.0\n  encore-api:\n    image: \"reg/enc:v2\"\n  weird:\n    image: ${NOPE:?must be set}\n  built:\n    build: .\nvolumes:\n  v:\n    image: nope\n"
	got := ComposeServiceImages(text, nil)
	want := map[string]string{"backend": "reg/x:v1.3.0", "frontend": "reg/web:v1.3.0", "encore-api": "reg/enc:v2"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q", k, got[k])
		}
	}
	if got := ComposeServiceImages(text, map[string]string{"TP_BACKEND_IMAGE": "mine:tag"}); got["backend"] != "mine:tag" {
		t.Fatalf("env override: %v", got)
	}
}
