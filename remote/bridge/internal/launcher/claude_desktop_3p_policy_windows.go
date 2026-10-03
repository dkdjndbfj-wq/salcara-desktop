//go:build windows

package launcher

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func claudeManagedPolicy(_ context.Context) (bool, error) {
	behavior := map[string]bool{"disableAutoUpdates": true, "autoUpdaterEnforcementHours": true, "updateViaUpdatesHost": true, "relaunchEnforcementHours": true, "configRecheckIntervalMinutes": true, "egressProxyUrl": true, "egressProxyPacUrl": true}
	for _, hive := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		key, err := registry.OpenKey(hive, `SOFTWARE\Policies\Claude`, registry.QUERY_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return true, errors.New("无法安全检查 Claude 管理策略，未修改配置")
		}
		names, err := key.ReadValueNames(-1)
		if err != nil {
			key.Close()
			return true, errors.New("无法安全检查 Claude 管理策略，未修改配置")
		}
		present := false
		for _, name := range names {
			_, kind, e := key.GetValue(name, nil)
			if e != nil {
				key.Close()
				return true, errors.New("Claude 管理策略无法读取，未修改配置")
			}
			if kind == registry.SZ || kind == registry.EXPAND_SZ || kind == registry.DWORD {
				present = true
				if !behavior[name] || strings.TrimSpace(name) == "" {
					key.Close()
					return true, nil
				}
			}
		}
		key.Close()
		// Machine and user hives are not merged; app-behavior-only HKLM masks HKCU.
		if present {
			return false, nil
		}
	}
	return false, nil
}
