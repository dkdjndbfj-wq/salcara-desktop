package apiservice

import (
	"errors"
	"strings"
	"sync"

	"salcara/bridge/internal/config"
)

const serviceProfileTarget = "codex-profile"

// BeginManagedLaunch closes inference admission while a native profile changes.
// Already running work is never cancelled. The new managed profile may start
// after its exact authorization has committed, before process Start returns.
func (s *Service) BeginManagedLaunch() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.admissionClosed.Load() {
		return nil, errors.New("本地 API 服务已关闭")
	}
	if s.launchPending || s.inflight != 0 {
		return nil, errors.New("本地 API 正在处理请求，请完成后再打开工具")
	}
	s.launchPending = true
	s.launchKey = ""
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.launchPending = false; s.launchKey = ""; s.mu.Unlock() }) }, nil
}

func (s *Service) AdmitManagedLaunch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.launchPending && !s.admissionClosed.Load() {
		s.launchKey = key
	}
}

func managedAPIPath(path string) (string, string, bool) {
	if route, profile := strings.CutPrefix(path, "/codex-api/profile/"); profile {
		return serviceProfileTarget, route, strings.HasPrefix(route, "v1/")
	}
	remaining, managed := strings.CutPrefix(path, "/codex-api/managed/")
	if !managed {
		return "", canonicalAPIPath(path), true
	}
	target, route, ok := strings.Cut(remaining, "/")
	if !ok || target != "codex" && target != "codex-desktop" && target != "claude" && target != "claude-desktop" {
		return "", "", false
	}
	if !strings.HasPrefix(route, "v1/") {
		return "", "", false
	}
	return target, route, true
}

func managedAPICredential(c config.Config, target string) string {
	if target == serviceProfileTarget {
		return c.CodexServiceProfileCredential()
	}
	return c.ManagedAPICredential(target)
}

func resolveManagedAPIClient(c config.Config, target, key string) (config.NamedAPIClient, bool) {
	if target == serviceProfileTarget {
		return c.ResolveCodexServiceProfileClient(key)
	}
	return c.ResolveManagedAPIClient(target, key)
}
