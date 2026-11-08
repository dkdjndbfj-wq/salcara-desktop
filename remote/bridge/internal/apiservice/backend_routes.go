package apiservice

import "strings"

// Expose only known protocol routes under the Codex backend path. Unknown
// backend operations are not an authenticated arbitrary upstream proxy.
func canonicalAPIPath(path string) string {
	path = strings.TrimPrefix(path, "/codex-api/")
	backend := strings.TrimPrefix(path, "/")
	if suffix, ok := strings.CutPrefix(backend, "backend-api/codex/"); ok {
		switch suffix {
		case "models", "responses", "responses/compact":
			return "v1/" + suffix
		}
	}
	return path
}
