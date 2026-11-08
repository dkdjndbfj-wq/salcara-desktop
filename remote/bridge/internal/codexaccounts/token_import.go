package codexaccounts

import (
	"bytes"
	"strings"
)

// ParseTokenImport takes an explicit token kind rather than guessing whether
// an opaque string is an access token or a rotating refresh token. Preview is
// entirely local; committing a refresh-only record uses the normal refresh owner.
func ParseTokenImport(data []byte, kind string) (*PreparedImport, error) {
	if kind != "access_token" && kind != "refresh_token" || len(data) == 0 || len(data) > MaxImportBytes {
		return nil, ErrInvalidImport
	}
	p := &PreparedImport{source: kind}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	lines := strings.Split(string(data), "\n")
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '"' || trimmed[0] == '{') {
		value, err := strictJSON(data)
		if err != nil {
			return nil, ErrInvalidImport
		}
		switch v := value.(type) {
		case string:
			lines = []string{v}
		case []any:
			lines = nil
			for _, item := range v {
				text, ok := item.(string)
				if !ok {
					return nil, ErrInvalidImport
				}
				lines = append(lines, text)
			}
		default:
			return nil, ErrInvalidImport
		}
	}
	for _, line := range lines {
		token := strings.TrimSpace(line)
		if token == "" {
			continue
		}
		if kind == "access_token" && len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
			token = strings.TrimSpace(token[7:])
		}
		if !validImportToken(token) || strings.HasPrefix(strings.ToLower(token), "sk-") || len(p.records) >= MaxAccounts {
			return nil, ErrInvalidImport
		}
		c, err := parseCredential(map[string]any{kind: token})
		if err != nil {
			return nil, ErrInvalidImport
		}
		p.records = append(p.records, c)
	}
	if len(p.records) == 0 {
		return nil, ErrInvalidImport
	}
	return p, nil
}

// RefreshOnly identifies records requiring the normal refresh owner without
// exposing the underlying secret.
func (p *PreparedImport) RefreshOnly(index int) bool {
	return p != nil && index >= 0 && index < len(p.records) && p.records[index].AccessToken == "" && p.records[index].RefreshToken != ""
}

func validImportToken(token string) bool {
	if token == "" || len(token) > maxTokenBytes {
		return false
	}
	for _, r := range token {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Normalize supported JSON aliases without losing ambiguity checks. Exact JSON
// bytes still pass strictJSON first; two aliases for one secret are rejected.
func normalizeCredentialFields(in map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	for canonical, aliases := range map[string][]string{
		"access_token":  {"accessToken", "personal_access_token", "personalAccessToken", "at_token", "atToken", "token"},
		"refresh_token": {"refreshToken"}, "id_token": {"idToken"},
		"account_id":                {"accountId", "chatgpt_account_id", "chatgptAccountId", "workspace_id", "workspaceId"},
		"email":                     {"account_email", "accountEmail"},
		"plan_type":                 {"planType", "chatgpt_plan_type"},
		"expires_at":                {"expiresAt", "expired"},
		"token_source_mode":         {"tokenSourceMode"},
		"subscription_active_until": {"subscriptionActiveUntil", "subscription_expires_at", "subscriptionExpiresAt"},
	} {
		_, seen := out[canonical]
		for _, alias := range aliases {
			if value, exists := in[alias]; exists {
				if seen {
					// Some portable exports repeat an explicit workspace ID under
					// several names. Equal strings are unambiguous; secrets never
					// receive this exception.
					if canonical == "account_id" {
						left, leftOK := out[canonical].(string)
						right, rightOK := value.(string)
						if leftOK && rightOK && strings.TrimSpace(left) == strings.TrimSpace(right) {
							delete(out, alias)
							continue
						}
					}
					return nil, ErrInvalidImport
				}
				out[canonical], seen = value, true
				delete(out, alias)
			}
		}
	}
	if raw, exists := out["headers"]; exists {
		headers, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrInvalidImport
		}
		seenAuth, seenAccount := false, false
		for name, value := range headers {
			switch strings.ToLower(name) {
			case "authorization":
				text, ok := value.(string)
				text = strings.TrimSpace(text)
				if !ok || seenAuth || out["access_token"] != nil || len(text) <= 7 || !strings.EqualFold(text[:7], "Bearer ") {
					return nil, ErrInvalidImport
				}
				out["access_token"], seenAuth = strings.TrimSpace(text[7:]), true
			case "chatgpt-account-id":
				text, ok := value.(string)
				if !ok || seenAccount || out["account_id"] != nil {
					return nil, ErrInvalidImport
				}
				out["account_id"], seenAccount = strings.TrimSpace(text), true
			}
		}
		// Normalization can be called again on a flat record. Consume these
		// recognized headers so the second pass cannot invent an ambiguity.
		delete(out, "headers")
	}
	return out, nil
}

func hasCredentialMaterial(root map[string]any) bool {
	for _, key := range []string{"access_token", "accessToken", "personal_access_token", "personalAccessToken", "at_token", "atToken", "token", "refresh_token", "refreshToken", "id_token", "idToken", "tokens", "credentials", "metadata", "headers", "session", "session_json"} {
		if _, ok := root[key]; ok {
			return true
		}
	}
	return false
}
