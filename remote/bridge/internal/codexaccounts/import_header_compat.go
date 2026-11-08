package codexaccounts

import "strings"

// Portable exports may put the workspace selector in custom_headers or
// customHeaders. Consume only the two supported credential hints; imported
// request headers never become executor configuration. Canonical field and
// value validation remain centralized in normalizeCredentialFields.
func normalizeImportCredentialFields(in map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	var headers map[string]any
	for _, container := range []string{"headers", "custom_headers", "customHeaders"} {
		raw, exists := in[container]
		if !exists {
			continue
		}
		values, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrInvalidImport
		}
		if headers == nil {
			headers = make(map[string]any)
		}
		for name, value := range values {
			key := strings.ToLower(name)
			if key != "authorization" && key != "chatgpt-account-id" {
				continue
			}
			// Even equal values are ambiguous when repeated in separate header
			// containers, matching the existing case-insensitive header rule.
			if _, exists := headers[key]; exists {
				return nil, ErrInvalidImport
			}
			headers[key] = value
		}
		delete(out, container)
	}
	if headers != nil {
		out["headers"] = headers
	}
	return normalizeCredentialFields(out)
}

func hasImportCredentialMaterial(root map[string]any) bool {
	if hasCredentialMaterial(root) {
		return true
	}
	for _, key := range []string{"custom_headers", "customHeaders"} {
		if _, exists := root[key]; exists {
			return true
		}
	}
	return false
}
