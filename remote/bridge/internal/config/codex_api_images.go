package config

import "errors"

type CodexAPIImageSettings struct {
	Mode                           string `json:"mode"`
	MaxConcurrentRequests          int    `json:"maxConcurrentRequests"`
	EffectiveMode                  string `json:"effectiveMode"`
	EffectiveMaxConcurrentRequests int    `json:"effectiveMaxConcurrentRequests"`
	RuntimeRestricted              bool   `json:"runtimeRestricted"`
	RequiresStart                  bool   `json:"requiresStart"`
	Serving                        bool   `json:"serving"`
}

// This separates the last successful disk write from restrictions that took
// effect immediately even when a later write failed. Read both under one lock.
func (s *Store) CodexAPIImageSettings() CodexAPIImageSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalize := func(mode string, n int) (string, int) {
		if mode == "" {
			mode = "disabled"
		}
		if n == 0 {
			n = 1
		}
		return mode, n
	}
	mode, n := normalize(s.savedImagesMode, s.savedImagesConcurrency)
	effective, currentN := normalize(s.cfg.CodexAPI.ImagesMode, s.cfg.CodexAPI.MaxConcurrentImageRequests)
	out := CodexAPIImageSettings{Mode: mode, MaxConcurrentRequests: n, EffectiveMode: effective, EffectiveMaxConcurrentRequests: currentN, RuntimeRestricted: mode != effective || n != currentN, Serving: s.cfg.CodexAPIActive()}
	out.RequiresStart = s.cfg.CodexAPI.Enabled && !out.Serving
	if !out.Serving {
		out.EffectiveMode = "disabled"
	}
	return out
}

func ValidateCodexAPIImages(mode string, concurrency int) error {
	if mode != "" && mode != "disabled" && mode != "enabled" && mode != "images_only" || concurrency < 0 || concurrency > 16 {
		return errors.New("图片服务模式或并发上限无效")
	}
	return nil
}

func CodexAPIImagesEnabled(mode string) bool {
	return mode == "enabled" || mode == "images_only"
}

// RetireCodexAPIImagesRuntime applies only restrictions before persistence.
// A disk failure must not revive image access or a higher concurrency limit.
// It does not enable images, text, or the service, and cannot mutate a newer
// authorization captured after this operation started.
func (s *Store) RetireCodexAPIImagesRuntime(expectedEpoch, mode string, concurrency int) (string, bool) {
	s.mu.Lock()
	if expectedEpoch == "" || s.cfg.CodexAPI.AuthEpoch != expectedEpoch || ValidateCodexAPIImages(mode, concurrency) != nil {
		s.mu.Unlock()
		return "", false
	}
	old := s.cfg.Clone()
	if !CodexAPIImagesEnabled(mode) {
		s.cfg.CodexAPI.ImagesMode = "disabled"
	} else if mode == "images_only" && old.CodexAPI.ImagesMode == "enabled" {
		s.cfg.CodexAPI.ImagesMode = mode
	}
	before, after := old.CodexAPI.MaxConcurrentImageRequests, concurrency
	if before == 0 {
		before = 1
	}
	if after == 0 {
		after = 1
	}
	if after < before {
		s.cfg.CodexAPI.MaxConcurrentImageRequests = after
	}
	fenceCodexAPIAuthorization(old, &s.cfg)
	epoch := s.cfg.CodexAPI.AuthEpoch
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return epoch, true
}
