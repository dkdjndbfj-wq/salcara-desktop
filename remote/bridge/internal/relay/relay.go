// Package relay talks to the sub2api relay itself (GET /v1/usage: balance/quota/usage for an API key).
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrInvalidKey means the relay rejected the key (401/403).
var ErrInvalidKey = errors.New("Key 无效：中转站拒绝了这个 API Key")

var httpClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func decodeUsageResponse(body io.Reader, limit int64) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("用量响应不完整或超过大小限制")
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, errors.New("这个服务商不提供有效的用量 JSON")
	}
	return result, nil
}

// Usage calls GET {root}/v1/usage with key and returns the decoded JSON object (fields vary by key mode:
// balance / quota_limited / subscription — callers render defensively).
func Usage(ctx context.Context, root, key string) (map[string]any, error) {
	if root == "" || key == "" {
		return nil, errors.New("请先填写中转站地址和 API Key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/v1/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("中转站地址无效：%w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "SalcaraBridge")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上中转站：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidKey
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("中转站返回 %d", resp.StatusCode)
	}
	return decodeUsageResponse(resp.Body, 2<<20)
}
