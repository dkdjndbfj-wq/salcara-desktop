package apiservice

import (
	"net/http"
	"net/url"
	"strings"
)

// Native protocols enter the same named-client admission, model policy and
// dispatch path as Chat. Managed tool credentials never enter these routes.
type nativeRoute struct {
	kind, model                   string
	stream, sse, models, generate bool
}

func parseNativeRoute(path string) *nativeRoute {
	switch path {
	case "v1beta/models":
		return &nativeRoute{kind: "gemini", models: true}
	case "api/tags":
		return &nativeRoute{kind: "ollama", models: true}
	case "api/chat", "api/generate":
		return &nativeRoute{kind: "ollama", generate: path == "api/generate"}
	}
	if model, ok := strings.CutPrefix(path, "v1beta/models/"); ok {
		for _, action := range []string{":generateContent", ":streamGenerateContent"} {
			if strings.HasSuffix(model, action) {
				model = strings.TrimSuffix(model, action)
				if model != "" && !strings.ContainsAny(model, ":?#\\\r\n") {
					return &nativeRoute{kind: "gemini", model: model, stream: action == ":streamGenerateContent"}
				}
			}
		}
	}
	return nil
}

// Credential ambiguity is rejected instead of choosing a silent precedence.
// Query credentials are accepted only on Gemini routes and are never forwarded.
func localAPIKey(r *http.Request, path, managed string, native *nativeRoute) (string, int) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", 400
	}
	keys := []string{}
	for _, header := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		values := r.Header.Values(header)
		if len(values) > 1 {
			return "", 401
		}
		if len(values) == 0 {
			continue
		}
		value := values[0]
		switch header {
		case "Authorization":
			prefix, token, ok := strings.Cut(value, " ")
			if !ok || !strings.EqualFold(prefix, "Bearer") {
				return "", 401
			}
			value = token
		case "x-api-key":
			if path != "v1/models" && path != "v1/messages" && path != "v1/messages/count_tokens" {
				return "", 401
			}
		case "x-goog-api-key":
			if native == nil || native.kind != "gemini" || managed != "" {
				return "", 401
			}
		}
		keys = append(keys, value)
	}
	for name, values := range query {
		if native == nil || native.kind != "gemini" || managed != "" || len(values) != 1 {
			return "", 404
		}
		switch name {
		case "key":
			keys = append(keys, values[0])
		case "alt":
			if !native.stream || values[0] != "sse" {
				return "", 400
			}
			native.sse = true
		default:
			return "", 400
		}
	}
	if len(keys) != 1 || keys[0] == "" || len(keys[0]) > 256 || strings.ContainsAny(keys[0], " \t\r\n\x00,") {
		return "", 401
	}
	return keys[0], 0
}
