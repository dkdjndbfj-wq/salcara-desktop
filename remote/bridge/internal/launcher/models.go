package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"salcara/bridge/internal/config"
)

var ErrEmptyModelCatalog = errors.New("模型列表为空")

type ModelCatalogHTTPError struct{ StatusCode int }

func (e *ModelCatalogHTTPError) Error() string {
	return fmt.Sprintf("模型列表返回 HTTP %d", e.StatusCode)
}

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
	config.ApplyAccountHeaders(req, a)
	client := &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		// Especially important for x-api-key: Go only strips selected headers on redirect.
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		// Keep the transport cause without echoing a URL that could contain
		// account credentials or private query parameters.
		var transport *url.Error
		if errors.As(err, &transport) {
			err = transport.Err
		}
		return nil, fmt.Errorf("模型列表连接失败：%w", RedactLaunchError(err, a.Key))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Provider bodies can echo secrets; never surface them to UI/logs.
		return nil, &ModelCatalogHTTPError{StatusCode: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return nil, fmt.Errorf("模型列表响应过大或读取失败")
	}
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("服务商返回的不是兼容的模型列表，可手动填写模型")
	}
	// A successful but incompatible response is not evidence that a model
	// group was emptied. Only an explicit array may retire a saved directory.
	var entries []struct {
		ID string `json:"id"`
	}
	rawData := strings.TrimSpace(string(payload.Data))
	if !strings.HasPrefix(rawData, "[") || json.Unmarshal(payload.Data, &entries) != nil {
		return nil, fmt.Errorf("服务商返回的不是兼容的模型列表，可手动填写模型")
	}
	seen := map[string]bool{}
	models := []string{}
	for _, item := range entries {
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
		if len(entries) > 0 {
			return nil, fmt.Errorf("模型列表包含无效名称，未替换已保存目录")
		}
		return nil, ErrEmptyModelCatalog
	}
	sort.Strings(models)
	return models, nil
}
