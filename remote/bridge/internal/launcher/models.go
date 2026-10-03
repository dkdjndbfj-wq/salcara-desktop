package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"salcara/bridge/internal/config"
)

// Models only makes a GET request. It does not generate tokens or images.
func Models(ctx context.Context, a config.LocalAccount) ([]string, error) {
	_, v1, err := config.APIBase(a.BaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v1+"/models", nil)
	if err != nil {
		return nil, err
	}
	if a.AuthMode == "api-key" {
		req.Header.Set("x-api-key", a.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+a.Key)
	}
	if a.Kind == "claude" || a.AuthMode == "api-key" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		// Especially important for x-api-key: Go only strips selected headers on redirect.
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("模型列表连接失败，请检查地址、网络或 TLS 证书")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Provider bodies can echo secrets; never surface them to UI/logs.
		return nil, fmt.Errorf("模型列表返回 HTTP %d；请检查 Key 权限。若服务商不提供 /v1/models，可手动填写模型", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return nil, fmt.Errorf("模型列表响应过大或读取失败")
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("服务商返回的不是兼容的模型列表，可手动填写模型")
	}
	seen := map[string]bool{}
	models := []string{}
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" && len(id) <= 200 && !strings.ContainsAny(id, "\r\n\x00") && !seen[id] {
			seen[id] = true
			models = append(models, id)
			if len(models) == 1000 {
				break
			}
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("模型列表为空，可手动填写服务商支持的模型")
	}
	sort.Strings(models)
	return models, nil
}
