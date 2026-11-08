package codexaccounts

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// ParseImport accepts documented credential JSON shapes and newline-delimited
// JSON. Each document remains subject to strict duplicate-key validation. It
// never reads files, follows URLs, or combines secrets from separate records.
func ParseImport(data []byte) (*PreparedImport, error) {
	if len(data) == 0 || len(data) > MaxImportBytes {
		return nil, ErrInvalidImport
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	value, err := strictJSON(data)
	p := &PreparedImport{source: "codex-auth"}
	if err == nil {
		if err := appendImportValue(p, value, 0); err != nil {
			return nil, ErrInvalidImport
		}
	} else {
		// JSONL is deliberately line based. Two adjacent JSON documents on one
		// line, or a malformed pretty-printed document, never become a batch.
		count := 0
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			row, parseErr := strictJSON(line)
			if parseErr != nil || appendImportValue(p, row, 0) != nil {
				return nil, ErrInvalidImport
			}
			count++
		}
		if count < 2 {
			return nil, ErrInvalidImport
		}
		p.source = "jsonl"
	}
	if len(p.records) == 0 {
		return nil, ErrInvalidImport
	}
	return p, nil
}

// ParseImportFiles validates the entire selection before any credential can be
// committed. Byte and account bounds apply across files, not independently.
func ParseImportFiles(inputs [][]byte, format string) (*PreparedImport, error) {
	if len(inputs) == 0 || len(inputs) > MaxAccounts {
		return nil, ErrInvalidImport
	}
	p := &PreparedImport{source: "files"}
	total := 0
	for _, data := range inputs {
		if len(data) > MaxImportBytes-total {
			return nil, ErrInvalidImport
		}
		total += len(data)
		var next *PreparedImport
		var err error
		switch format {
		case "", "json":
			next, err = ParseImport(data)
		case "access_token", "refresh_token":
			next, err = ParseTokenImport(data, format)
		default:
			return nil, ErrInvalidImport
		}
		if err != nil || len(p.records)+len(next.records) > MaxAccounts {
			return nil, ErrInvalidImport
		}
		p.records = append(p.records, next.records...)
		if len(inputs) == 1 {
			p.source = next.source
		}
	}
	return p, nil
}

func appendImportValue(p *PreparedImport, value any, depth int) error {
	if depth > 4 || len(p.records) >= MaxAccounts {
		return ErrInvalidImport
	}
	if rows, ok := value.([]any); ok {
		if len(rows) == 0 || len(rows) > MaxAccounts {
			return ErrInvalidImport
		}
		p.source = "json-array"
		for _, row := range rows {
			if err := appendImportValue(p, row, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	root, ok := value.(map[string]any)
	if !ok {
		return ErrInvalidImport
	}
	if err := validateImportAuthMarkers(root); err != nil {
		return err
	}
	if raw, exists := root["accounts"]; exists {
		if hasImportCredentialMaterial(root) {
			return ErrInvalidImport
		}
		rows, ok := raw.([]any)
		if !ok || len(rows) == 0 || len(rows) > MaxAccounts {
			return ErrInvalidImport
		}
		if provider := stringValue(root, "provider"); provider != "" && provider != "codex" {
			return ErrInvalidImport
		}
		if version, exists := root["version"]; exists {
			if n, ok := version.(json.Number); !ok || n.String() != "1" {
				return ErrInvalidImport
			}
		}
		p.source = "account-bundle"
		if root["version"] != nil {
			p.source = "bundle-v1"
		}
		for _, row := range rows {
			if err := appendImportValue(p, row, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if root["version"] != nil {
		return ErrInvalidImport
	}
	c, err := parseCredential(root)
	if err != nil {
		return err
	}
	p.records = append(p.records, c)
	return nil
}

func (p *PreparedImport) Preview() ImportPreview {
	result := ImportPreview{Version: BundleVersion, Accounts: []Account{}}
	if p == nil {
		return result
	}
	result.Source = p.source
	for _, c := range p.records {
		result.Accounts = append(result.Accounts, c.account(time.Now()))
	}
	return result
}

// containsRefreshToken reports whether an opaque prepared import contains the
// supplied refresh token.  It is intentionally package-private: callers can
// parse a user-selected native auth file and ask the service for a boolean
// conflict result without ever receiving the vault token or a token digest.
func (p *PreparedImport) containsRefreshToken(token string) bool {
	if p == nil || token == "" {
		return false
	}
	for _, c := range p.records {
		if c.RefreshToken != "" && subtle.ConstantTimeCompare([]byte(c.RefreshToken), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

func parseCredential(root map[string]any) (credential, error) {
	var err error
	root, err = unwrapSessionCredential(root, 0)
	if err != nil {
		return credential{}, err
	}
	root, err = normalizeImportCredentialFields(root)
	if err != nil {
		return credential{}, err
	}
	if err := validateCredentialKind(root, ""); err != nil {
		return credential{}, ErrInvalidImport
	}
	m := root
	container := false
	for _, key := range []string{"tokens", "metadata", "credentials"} {
		if raw, exists := root[key]; exists {
			nested, ok := raw.(map[string]any)
			if !ok || container {
				return credential{}, ErrInvalidImport
			}
			m, container = nested, true
		}
	}
	m, err = normalizeImportCredentialFields(m)
	if err != nil {
		return credential{}, err
	}
	if container {
		for _, key := range []string{"access_token", "refresh_token", "id_token"} {
			if _, exists := root[key]; exists {
				return credential{}, ErrInvalidImport
			}
		}
		for _, key := range []string{"account_id", "email", "plan_type", "expires_at", "subscription_active_until", "token_source_mode"} {
			if raw, exists := root[key]; exists && raw != nil {
				if current, exists := m[key]; exists && current != nil && !sameImportScalar(current, raw) {
					if key != "expires_at" {
						return credential{}, ErrInvalidImport
					}
					expiry, equal := equivalentImportExpiry(current, raw)
					if !equal {
						return credential{}, ErrInvalidImport
					}
					// Preserve the exact agreed instant even when one representation
					// uses a millisecond epoch and the other uses an ISO timestamp.
					m[key] = expiry.Format(time.RFC3339Nano)
					continue
				}
				m[key] = raw
			}
		}
	}
	personalAccessToken := false
	for _, hints := range []map[string]any{root, m} {
		if err := validateCredentialKind(hints, stringValue(root, "platform")); err != nil {
			return credential{}, ErrInvalidImport
		}
		for _, key := range []string{"auth_mode", "authMode", "openai_auth_mode", "openaiAuthMode"} {
			personalAccessToken = personalAccessToken || strings.EqualFold(stringValue(hints, key), "personal_access_token")
		}
	}
	if err := applyCredentialProfileHints(m, root); err != nil {
		return credential{}, err
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "account_id", "email", "plan_type", "token_source_mode"} {
		if value, exists := m[key]; exists {
			if value == nil {
				continue
			}
			if _, ok := value.(string); !ok {
				return credential{}, ErrInvalidImport
			}
		}
	}
	if !validAccountName(stringValue(m, "email")) {
		return credential{}, ErrInvalidImport
	}
	c := credential{AccessToken: stringValue(m, "access_token"), RefreshToken: stringValue(m, "refresh_token"), IDToken: stringValue(m, "id_token"), AccountID: stringValue(m, "account_id"), Email: accountEmailHint(stringValue(m, "email")), NameMode: nameModeDefault, Plan: stringValue(m, "plan_type")}
	if personalAccessToken && (c.AccessToken == "" || c.RefreshToken != "" || c.IDToken != "") {
		return credential{}, ErrInvalidImport
	}
	mode := stringValue(m, "token_source_mode")
	if mode == "chatgpt_web_session" || isWebSessionCredential(root) || isWebSessionCredential(m) {
		c.TokenSourceMode = "chatgpt_web_session"
	} else if mode != "" && mode != "managed" && mode != "codex_oauth" && mode != "legacy_codex_cli" {
		return credential{}, ErrInvalidImport
	}
	if c.TokenSourceMode == "chatgpt_web_session" && c.AccessToken == "" {
		return credential{}, ErrInvalidImport
	}
	if attrs, ok := root["attributes"].(map[string]any); ok && c.Plan == "" {
		c.Plan = stringValue(attrs, "plan_type")
	}
	if c.AccessToken == "" && c.RefreshToken == "" || c.AccessToken != "" && (!validImportToken(c.AccessToken) || strings.HasPrefix(strings.ToLower(c.AccessToken), "sk-")) || c.RefreshToken != "" && (!validImportToken(c.RefreshToken) || strings.HasPrefix(strings.ToLower(c.RefreshToken), "sk-")) || c.IDToken != "" && !validImportToken(c.IDToken) {
		return credential{}, ErrInvalidImport
	}
	if raw, exists := m["expires_at"]; exists && raw != nil {
		text := subscriptionScalar(raw)
		expiry, ok := parseSubscriptionExpiry(text)
		blank, isString := raw.(string)
		if text != "" && !ok || text == "" && (!isString || strings.TrimSpace(blank) != "") {
			return credential{}, ErrInvalidImport
		}
		if ok {
			c.ExpiresAt = expiry
		}
	}
	if c.AccessToken == "" {
		// A supplied future expiry cannot make a refresh-only record usable.
		// Keep the private sentinel even when the JSON contains such a hint.
		c.ExpiresAt = time.Unix(1, 0).UTC()
	}
	// Unverified JWTs supply display hints only. They are never used to accept an
	// identity, validate a token, route an account, or grant plan access.
	for _, token := range []string{c.IDToken, c.AccessToken} {
		claims := displayClaims(token)
		if c.Email == "" {
			c.Email = accountEmailHint(stringValue(claims, "email"))
		}
		if info, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if c.Plan == "" {
				c.Plan = stringValue(info, "chatgpt_plan_type")
			}
		}
		if c.ExpiresAt.IsZero() && token == c.AccessToken {
			if exp, ok := claims["exp"].(json.Number); ok {
				if sec, err := strconv.ParseInt(exp.String(), 10, 64); err == nil && sec > 0 && sec < 253402300800 {
					c.ExpiresAt = time.Unix(sec, 0).UTC()
				}
			}
		}
	}
	hintApplied := false
	for _, hints := range []map[string]any{m, root} {
		if hintApplied {
			break
		}
		for _, key := range []string{"subscription_active_until"} {
			if raw, exists := hints[key]; exists {
				if raw == nil {
					continue
				}
				var text string
				switch v := raw.(type) {
				case string:
					text = v
				case json.Number:
					text = v.String()
				default:
					return credential{}, ErrInvalidImport
				}
				if strings.TrimSpace(text) == "" {
					continue
				}
				if !applySubscriptionHint(&c, text, "import_hint") {
					return credential{}, ErrInvalidImport
				}
				hintApplied = true
				break
			}
		}
	}
	applySubscriptionHintFromTokens(&c)
	c.Name = c.Email
	if c.Name == "" {
		c.Name = stringValue(root, "name")
	}
	if c.Name == "" {
		c.Name = stringValue(root, "label")
	}
	if c.Name == "" {
		c.Name = "Codex account"
	}
	if len(c.Name) > 320 || len(c.Plan) > 64 || len(c.AccountID) > 512 || strings.ContainsAny(c.Name, "\r\n\x00") || strings.ContainsAny(c.Plan, "\r\n\x00") || strings.ContainsAny(c.AccountID, "\r\n\x00") {
		return credential{}, ErrInvalidImport
	}
	return c, nil
}

// OAuth exports may retain null or blank defaults for other credential modes.
// Any populated or malformed marker is ambiguous: never silently interpret API
// Key or Agent Identity material as an OAuth account, even if tokens coexist.
func validateImportAuthMarkers(root map[string]any) error {
	for _, key := range []string{
		"OPENAI_API_KEY", "openai_api_key", "agent_identity", "agentIdentity",
		"agent_runtime_id", "agentRuntimeId", "agent_private_key", "agentPrivateKey",
	} {
		if raw, exists := root[key]; exists && raw != nil {
			text, ok := raw.(string)
			if !ok || strings.TrimSpace(text) != "" {
				return ErrInvalidImport
			}
		}
	}
	return nil
}

func validateCredentialKind(root map[string]any, parentPlatform string) error {
	if err := validateImportAuthMarkers(root); err != nil {
		return err
	}
	for _, key := range []string{"type", "provider", "platform", "auth_mode", "authMode", "openai_auth_mode", "openaiAuthMode"} {
		if raw, exists := root[key]; exists && raw != nil {
			if _, ok := raw.(string); !ok {
				return ErrInvalidImport
			}
		}
	}
	platform := stringValue(root, "platform")
	if platform == "" {
		platform = parentPlatform
	}
	if platform != "" && platform != "openai" {
		return ErrInvalidImport
	}
	for _, key := range []string{"type", "provider"} {
		if kind := stringValue(root, key); kind != "" && kind != "codex" && !(key == "type" && kind == "oauth" && platform == "openai") {
			return ErrInvalidImport
		}
	}
	for _, key := range []string{"auth_mode", "authMode", "openai_auth_mode", "openaiAuthMode"} {
		switch strings.ToLower(stringValue(root, key)) {
		case "", "chatgpt", "oauth", "personal_access_token":
		default:
			return ErrInvalidImport
		}
	}
	return nil
}

func unwrapSessionCredential(root map[string]any, depth int) (map[string]any, error) {
	if depth > 4 {
		return nil, ErrInvalidImport
	}
	var nested map[string]any
	outer := make(map[string]any, len(root))
	for key, value := range root {
		if key != "session" && key != "session_json" {
			outer[key] = value
			continue
		}
		if nested != nil {
			return nil, ErrInvalidImport
		}
		if text, ok := value.(string); ok {
			parsed, err := strictJSON([]byte(text))
			if err != nil {
				return nil, ErrInvalidImport
			}
			value = parsed
		}
		var ok bool
		nested, ok = value.(map[string]any)
		if !ok {
			return nil, ErrInvalidImport
		}
	}
	if nested == nil {
		return root, nil
	}
	// Do not construct a credential using an access token from one container
	// and a refresh token (or identity) belonging to another container.
	if hasImportCredentialMaterial(outer) {
		return nil, ErrInvalidImport
	}
	result, err := unwrapSessionCredential(nested, depth+1)
	if err != nil {
		return nil, err
	}
	for key, value := range outer {
		if current, exists := result[key]; exists && !sameImportScalar(current, value) {
			return nil, ErrInvalidImport
		}
		result[key] = value
	}
	result["token_source_mode"] = "chatgpt_web_session"
	return result, nil
}

func sameImportScalar(a, b any) bool {
	switch left := a.(type) {
	case string:
		right, ok := b.(string)
		return ok && strings.TrimSpace(left) == strings.TrimSpace(right)
	case json.Number:
		right, ok := b.(json.Number)
		return ok && left == right
	}
	return a == nil && b == nil
}

func isWebSessionCredential(root map[string]any) bool {
	if stringValue(root, "access_token") == "" {
		return false
	}
	_, user := root["user"].(map[string]any)
	_, account := root["account"].(map[string]any)
	return user || account || root["expires"] != nil || root["sessionToken"] != nil || strings.EqualFold(stringValue(root, "authProvider"), "openai")
}

func applyCredentialProfileHints(target, root map[string]any) error {
	for _, source := range []map[string]any{root, target} {
		for _, name := range []string{"user", "profile", "account"} {
			raw, exists := source[name]
			if !exists || raw == nil {
				continue
			}
			profile, ok := raw.(map[string]any)
			if !ok {
				return ErrInvalidImport
			}
			profile, err := normalizeImportCredentialFields(profile)
			if err != nil || hasImportCredentialMaterial(profile) || validateImportAuthMarkers(profile) != nil {
				return ErrInvalidImport
			}
			if name == "account" {
				for from, to := range map[string]string{"id": "account_id", "plan": "plan_type"} {
					if value, exists := profile[from]; exists && value != nil {
						if current, exists := profile[to]; exists && current != nil && !sameImportScalar(value, current) {
							return ErrInvalidImport
						}
						profile[to] = value
					}
				}
			}
			for _, key := range []string{"email", "account_id", "plan_type", "subscription_active_until"} {
				if value, exists := profile[key]; exists && value != nil {
					if current, exists := target[key]; exists && current != nil && !sameImportScalar(value, current) {
						return ErrInvalidImport
					}
					target[key] = value
				}
			}
		}
	}
	return nil
}

func stringValue(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}
func displayClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > maxTokenBytes {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	value, err := strictJSON(raw)
	if err != nil {
		return nil
	}
	m, _ := value.(map[string]any)
	return m
}

// strictJSON rejects duplicate keys at every depth, trailing documents, huge
// input, excessive nesting, and excessive aggregate object/array entries.
func strictJSON(data []byte) (any, error) {
	return strictJSONLimit(data, MaxImportBytes)
}
func strictJSONLimit(data []byte, limit int) (any, error) {
	if len(data) == 0 || len(data) > limit {
		return nil, ErrInvalidImport
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	budget := 16384
	v, err := readJSON(dec, 0, &budget)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, ErrInvalidImport
	}
	return v, nil
}
func readJSON(dec *json.Decoder, depth int, budget *int) (any, error) {
	if depth > 16 || *budget <= 0 {
		return nil, ErrInvalidImport
	}
	*budget--
	token, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidImport
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, ErrInvalidImport
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, ErrInvalidImport
			}
			if _, exists := m[key]; exists {
				return nil, ErrInvalidImport
			}
			value, err := readJSON(dec, depth+1, budget)
			if err != nil {
				return nil, err
			}
			m[key] = value
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalidImport
		}
		return m, nil
	case '[':
		rows := []any{}
		for dec.More() {
			value, err := readJSON(dec, depth+1, budget)
			if err != nil {
				return nil, err
			}
			rows = append(rows, value)
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalidImport
		}
		return rows, nil
	default:
		return nil, ErrInvalidImport
	}
}
