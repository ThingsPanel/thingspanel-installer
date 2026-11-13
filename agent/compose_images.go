//go:build linux

package agent

import (
	"regexp"
	"strings"
)

var interpolation = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?(?::\?[^}]*)?\}`)

// ComposeServiceImages returns service -> image reference for the services of a docker-compose.yml text, with
// ${VAR}, ${VAR:-default} and ${VAR:?msg} resolved from env (an unset ${VAR:?msg} or ${VAR} with no value makes
// the service's image unresolvable and it is skipped). Only top-level services indented two spaces are read,
// which is how the reviewed Compose file is written.
func ComposeServiceImages(compose string, env map[string]string) map[string]string {
	out := map[string]string{}
	inServices := false
	service := ""
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimRight(line, " \r")
		body := strings.TrimSpace(trimmed)
		if body == "" || strings.HasPrefix(body, "#") {
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		switch {
		case indent == 0:
			inServices = strings.HasPrefix(trimmed, "services:")
			service = ""
		case inServices && indent == 2 && strings.HasSuffix(body, ":"):
			service = strings.TrimSuffix(body, ":")
		case inServices && service != "" && indent == 4 && strings.HasPrefix(body, "image:"):
			raw := strings.Trim(strings.TrimSpace(strings.TrimPrefix(body, "image:")), `"'`)
			resolved, ok := resolveInterpolation(raw, env)
			if ok && resolved != "" {
				out[service] = resolved
			}
		}
	}
	return out
}

func resolveInterpolation(v string, env map[string]string) (string, bool) {
	ok := true
	res := interpolation.ReplaceAllStringFunc(v, func(m string) string {
		parts := interpolation.FindStringSubmatch(m)
		name, def := parts[1], parts[2]
		if val := env[name]; val != "" {
			return val
		}
		if strings.Contains(m, "-") && !strings.Contains(m, ":?") {
			return def
		}
		ok = false
		return ""
	})
	return res, ok && !strings.Contains(res, "$")
}
