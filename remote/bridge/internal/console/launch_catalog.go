package console

import (
	"context"
	"errors"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

// Refresh once before applying a tool profile, including the direct Agent
// launch entry. A provider without /models can still use an explicitly saved
// directory; discovery failure never silently deletes that manual directory.
// This GET has a short independent deadline and never starts inference.
func (s *Server) refreshLaunchCatalog(ctx context.Context, a config.LocalAccount) (config.LocalAccount, error) {
	a, _, err := s.refreshLaunchCatalogChecked(ctx, a, nil)
	return a, err
}

// The optional guard runs inside the catalog commit transaction. Managed
// client previews use the returned exact snapshot to follow only their own
// catalog update, never an unrelated client rotation/replacement.
func (s *Server) refreshLaunchCatalogChecked(ctx context.Context, a config.LocalAccount, guard func(config.Config) error) (config.LocalAccount, config.Config, error) {
	if a.CredentialRef != "" || a.SourceType == "official_account" {
		launcher.LaunchPhase(ctx, "catalog", "读取可用模型", "使用已保存的官方模型目录")
		return a, s.d.Store.Get(), nil // protected accounts use the native Codex picker hint path
	}
	seq := s.beginModelProbe(a.ID)
	defer func() {
		s.modelProbeMu.Lock()
		if s.modelProbeSeq[a.ID] == seq {
			delete(s.modelProbeSeq, a.ID)
		}
		s.modelProbeMu.Unlock()
	}()
	probe := s.launchModelProbe
	if probe == nil {
		probe = launcher.Models
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := probe(probeCtx, a)
	if ctx.Err() != nil {
		return config.LocalAccount{}, config.Config{}, ctx.Err()
	}
	if errors.Is(err, launcher.ErrEmptyModelCatalog) {
		models, err = nil, nil
	}
	if err != nil {
		var status *launcher.ModelCatalogHTTPError
		if errors.As(err, &status) && (status.StatusCode == 401 || status.StatusCode == 403) {
			return config.LocalAccount{}, config.Config{}, err
		}
		snapshot := s.d.Store.Get()
		if guard != nil {
			if err := guard(snapshot); err != nil {
				return config.LocalAccount{}, config.Config{}, err
			}
		}
		current, exists := snapshot.LocalAccount(a.ID)
		if !exists || config.AccountFingerprint(current) != config.AccountFingerprint(a) || strings.Join(current.Models, "\x00") != strings.Join(a.Models, "\x00") {
			return config.LocalAccount{}, config.Config{}, errors.New("读取模型期间 API 已更换，请重新打开")
		}
		launcher.LaunchWarning(ctx, "catalog", "模型刷新失败，使用已保存目录："+launcher.RedactLaunchError(err, a.Key).Error())
		return current, snapshot, nil
	}
	// A manually curated picker is deliberate. We still check the provider
	// before launch, but only an explicit refresh replaces that saved picker.
	if a.ModelsManual && len(models) > 0 {
		models = a.Models
	}
	models = visibleAccountModels(a, cleanModels(models))
	snapshot, err := s.commitModelProbeChecked(a, seq, config.AccountFingerprint(a), models, a.ModelsManual, guard)
	if err != nil {
		return config.LocalAccount{}, config.Config{}, err
	}
	if s.d.Hub != nil {
		s.d.Hub.InvalidateModelCatalog(a.ID)
	}
	if len(models) == 0 {
		return config.LocalAccount{}, config.Config{}, errors.New("当前 API 没有可用模型，请在 API 中刷新或编辑模型目录")
	}
	if guard == nil {
		snapshot = s.d.Store.Get()
	}
	current, exists := snapshot.LocalAccount(a.ID)
	expected := a
	expected.Models = models
	if expected.Model != "" && !containsLocalModel(models, expected.Model) {
		expected.Model = ""
	}
	if !exists || !sameLaunchAccount(current, expected) {
		return config.LocalAccount{}, config.Config{}, errors.New("API 已更换，请重新选择")
	}
	return current, snapshot, nil
}

// A native profile may remember a model from a previous API or upstream group.
// Keep it only when the current directory still advertises it. The native
// picker remains the place to change models after launch.
func launchCatalogDefault(a config.LocalAccount) config.LocalAccount {
	if len(a.Models) == 0 {
		return a
	}
	if a.Model != "" && containsLocalModel(a.Models, a.Model) && config.ModelAllowed(a, a.Model) {
		return a
	}
	a.Model = ""
	for _, model := range a.Models {
		if config.ModelAllowed(a, model) {
			a.Model = model
			break
		}
	}
	a.Protocol = config.InferProtocol(a.Model, a.Wire)
	if a.Protocol == "" {
		a.Protocol = config.NativeProtocol(a.Kind)
	}
	return a
}

func sameLaunchAccount(current, expected config.LocalAccount) bool {
	return config.AccountFingerprint(current) == config.AccountFingerprint(expected) &&
		current.ModelsManual == expected.ModelsManual &&
		strings.Join(current.Models, "\x00") == strings.Join(expected.Models, "\x00")
}

func sameLaunchSelection(current, expected config.Config, target string) bool {
	before, _ := expected.SelectedToolAccount(target)
	after, _ := current.SelectedToolAccount(target)
	return before.ID == after.ID && current.ToolModels[target] == expected.ToolModels[target] &&
		current.ToolProtocols[target] == expected.ToolProtocols[target] &&
		current.ToolCatalogOverride[target] == expected.ToolCatalogOverride[target] &&
		current.LocalToolPaths[target] == expected.LocalToolPaths[target]
}

func unchangedSelectionAfterCatalog(current, before config.Config, account config.LocalAccount, target string) bool {
	// Discovery may clear only selections absent from its new directory. An
	// external tool/API/override/path change while GET /models was pending
	// must not be adopted as the baseline of an older launch request.
	expected := before.Clone()
	for i := range expected.LocalAccounts {
		if expected.LocalAccounts[i].ID == account.ID {
			expected.LocalAccounts[i] = account
		}
	}
	invalidateAccountModelSelections(&expected, account, len(account.Models) == 0)
	return sameLaunchSelection(current, before, target) || sameLaunchSelection(current, expected, target)
}
