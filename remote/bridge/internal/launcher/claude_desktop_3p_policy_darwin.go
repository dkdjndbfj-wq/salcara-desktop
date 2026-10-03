//go:build darwin

package launcher

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// claudeManagedPolicy reports an organization-managed Claude Desktop on macOS.
// Enterprise policy arrives as a configuration profile, which macOS materializes
// under /Library/Managed Preferences (machine-wide and per user). Any managed
// preference domain mentioning Anthropic or Claude is treated as managed: the
// Bridge then never writes Claude's configuration or reads its history, in the
// same fail-closed spirit as the Windows registry check.
func claudeManagedPolicy(_ context.Context) (bool, error) {
	dirs := []string{"/Library/Managed Preferences"}
	if u, err := user.Current(); err == nil && u.Username != "" && !strings.ContainsAny(u.Username, `/\`) {
		dirs = append(dirs, filepath.Join("/Library/Managed Preferences", u.Username))
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				continue // the directory exists but holds nothing readable for this user
			}
			return true, errors.New("无法安全检查 Claude 管理策略，未修改配置")
		}
		for _, entry := range entries {
			name := strings.ToLower(entry.Name())
			if strings.HasSuffix(name, ".plist") && (strings.Contains(name, "anthropic") || strings.Contains(name, "claude")) {
				return true, nil
			}
		}
	}
	return false, nil
}
