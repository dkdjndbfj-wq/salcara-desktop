package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// UsageDays is Usage with a history window: sub2api returns per-day and
// per-model statistics for the last `days` days (1–90).
func UsageDays(ctx context.Context, root, key string, days int) (map[string]any, error) {
	if root == "" || key == "" {
		return nil, errors.New("缺少地址或 API Key")
	}
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}
	q := url.Values{"days": {strconv.Itoa(days)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/v1/usage?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("地址无效：%w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "SalcaraBridge")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidKey
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("这个服务商不提供用量查询")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("返回 %d", resp.StatusCode)
	}
	return decodeUsageResponse(resp.Body, 4<<20)
}
