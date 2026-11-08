package agents

import (
	"encoding/json"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

// BrowserNavigationFromCodexItem shares the same strict projection with the
// explicitly authorized desktop history reader. Callers must already own the
// containing session; this function does not admit or discover desktop tasks.
func BrowserNavigationFromCodexItem(raw json.RawMessage) *protocol.BrowserNavigation {
	var item cxItem
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	return codexBrowserNavigation(item)
}

func codexBrowserNavigation(item cxItem) *protocol.BrowserNavigation {
	if item.Status != "completed" || len(item.Error) > 0 && string(item.Error) != "null" {
		return nil
	}
	var input map[string]any
	if json.Unmarshal(item.Arguments, &input) != nil {
		return nil
	}
	switch item.Type {
	case "mcpToolCall":
		if item.Success != nil && !*item.Success {
			return nil
		}
		return browserNavigation(item.Server, item.Tool, input, item.Result)
	case "dynamicToolCall":
		if item.Success == nil || !*item.Success {
			return nil
		}
		namespace := ""
		if item.Namespace != nil {
			namespace = *item.Namespace
		}
		// Inspect both output shapes together, so an embedded MCP error vetoes
		// success and conflicting page identities cannot be combined.
		result, err := json.Marshal(struct {
			ContentItems json.RawMessage `json:"contentItems,omitempty"`
			Result       json.RawMessage `json:"result,omitempty"`
		}{item.ContentItems, item.Result})
		if err == nil {
			return browserNavigation(namespace, item.Tool, input, result)
		}
	}
	return nil
}

// Match explicit navigation interfaces only. The caller supplies completion
// ownership: starting, cancelled, failed and unknown tool calls stay ordinary
// timeline activities. In particular, never extract URLs from prose or shell.
func browserNavigation(server, tool string, input map[string]any, result json.RawMessage) *protocol.BrowserNavigation {
	server, tool, valid := browserToolIdentity(server, tool)
	if !valid || input == nil {
		return nil
	}
	action := "navigate"
	target := input
	switch tool {
	case "browser_navigate", "puppeteer_navigate":
	case "new_page", "navigate_page":
		if server != "chrome-devtools" && server != "chrome_devtools" {
			return nil
		}
		if tool == "new_page" {
			action = "open"
		} else if kind, exists := input["type"]; exists && kind != "url" {
			return nil // back/forward/reload do not provide a destination URL
		}
	case "open_in_codex":
		if server != "codex_app" {
			return nil
		}
		if _, exists := input["threadId"]; exists {
			return nil // an explicitly addressed different chat has another owner
		}
		var ok bool
		target, ok = input["target"].(map[string]any)
		if !ok || target["type"] != "browser" {
			return nil
		}
		action = "open"
		if _, exists := target["tabId"]; exists {
			action = "navigate"
		}
	default:
		return nil
	}
	rawURL, ok := target["url"].(string)
	if !ok || !validBrowserURL(rawURL) {
		return nil
	}
	pageID, ok := explicitBrowserPageID(target)
	if !ok {
		return nil
	}
	if len(result) > 1<<20 {
		return nil // an uninspectable result cannot establish success metadata
	}
	if len(result) > 0 {
		var payload any
		if json.Unmarshal(result, &payload) != nil || !browserResultMetadata(payload, 0, &pageID) {
			return nil
		}
	}
	navigation := &protocol.BrowserNavigation{Action: action, URL: rawURL}
	if pageID != "" {
		if server == "" {
			server = tool
		}
		identity := url.PathEscape(server) + ":" + url.PathEscape(pageID)
		if len(identity) > 256 {
			return nil
		}
		navigation.PageID = identity
	}
	return navigation
}

func browserToolIdentity(server, tool string) (string, string, bool) {
	server = strings.TrimPrefix(server, "mcp__")
	if strings.HasPrefix(tool, "mcp__") {
		namespace, name, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
		if !ok || server != "" && server != "functions" && server != namespace {
			return "", "", false
		}
		server, tool = namespace, name
	} else if strings.Contains(tool, ".") && (server == "" || server == "functions") {
		server, tool, _ = strings.Cut(tool, ".")
	}
	valid := func(value string) bool {
		if len(value) > 128 {
			return false
		}
		for _, r := range value {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
				return false
			}
		}
		return true
	}
	return server, tool, tool != "" && valid(server) && valid(tool)
}

func validBrowserURL(raw string) bool {
	if raw == "" || len(raw) > 4096 || !utf8.ValidString(raw) || strings.Contains(raw, "\\") {
		return false
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" {
		return false
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.Contains(decoded, "\\") {
		return false
	}
	for _, r := range decoded {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	host := parsed.Hostname()
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.Contains(host, ":") || strings.HasPrefix(parsed.Host, "[") {
		return false
	}
	if strings.Trim(host, "0123456789.") == "" {
		return false // do not reinterpret malformed or abbreviated IPv4 as DNS
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' {
				return false
			}
		}
	}
	return len(host) <= 253
}

func explicitBrowserPageID(fields map[string]any) (string, bool) {
	id := ""
	for _, key := range []string{"tab_id", "page_id", "tabId", "pageId"} {
		value, exists := fields[key]
		if !exists {
			continue
		}
		var candidate string
		switch v := value.(type) {
		case string:
			candidate = v
		case float64:
			if v < 0 || v > 9007199254740991 || math.Trunc(v) != v {
				return "", false
			}
			candidate = strconv.FormatFloat(v, 'f', 0, 64)
		default:
			return "", false
		}
		if candidate == "" || len(candidate) > 256 || !utf8.ValidString(candidate) {
			return "", false
		}
		for _, r := range candidate {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
				return "", false
			}
		}
		if id != "" && id != candidate {
			return "", false
		}
		id = candidate
	}
	return id, true
}

// Read only typed result envelopes, including JSON serialized as MCP text.
// Human-readable page lists, HTML and ordinary text are never interpreted.
func browserResultMetadata(payload any, depth int, pageID *string) bool {
	if depth > 8 {
		return false
	}
	switch value := payload.(type) {
	case map[string]any:
		for _, flag := range []string{"isError", "is_error"} {
			if state, exists := value[flag]; exists && state != false {
				return false
			}
		}
		if success, exists := value["success"]; exists && success != true {
			return false
		}
		id, ok := explicitBrowserPageID(value)
		if !ok || id != "" && *pageID != "" && id != *pageID {
			return false
		}
		if id != "" {
			*pageID = id
		}
		for _, key := range []string{"structuredContent", "result", "content", "contentItems"} {
			if nested, exists := value[key]; exists && !browserResultMetadata(nested, depth+1, pageID) {
				return false
			}
		}
		if value["type"] == "text" || value["type"] == "inputText" {
			return browserResultMetadata(value["text"], depth+1, pageID)
		}
	case []any:
		for _, item := range value {
			if !browserResultMetadata(item, depth+1, pageID) {
				return false
			}
		}
	case string:
		text := strings.TrimSpace(value)
		if strings.HasPrefix(text, "{") {
			var object map[string]any
			if json.Unmarshal([]byte(text), &object) == nil {
				return browserResultMetadata(object, depth+1, pageID)
			}
		}
	}
	return true
}
