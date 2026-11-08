package toolcfg

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

var (
	ErrCodexProfileInvalid       = errors.New("服务配置参数无效")
	ErrCodexProfileUnsafe        = errors.New("无法安全访问服务配置文件")
	ErrCodexProfileCorrupt       = errors.New("服务配置备份无法验证，未改动配置")
	ErrCodexProfileChanged       = errors.New("配置已被其他程序修改，未覆盖或恢复")
	ErrCodexProfileStale         = errors.New("服务配置状态已变化，请刷新后重试")
	ErrCodexProfileOwned         = errors.New("此配置正由本地服务管理，请先在服务面板恢复")
	ErrCodexProfileNotOwned      = errors.New("没有本服务可恢复的配置")
	ErrCodexProfileAuthorization = errors.New("服务授权已变化，未继续应用配置")
	ErrCodexProfileRecovery      = errors.New("服务配置操作未完成，请恢复原配置")
	ErrCodexProfileWrite         = errors.New("服务配置写入失败，原配置已保留或回滚")
)

const (
	serviceProfileFileLimit     = 1 << 20
	serviceProfileManifestLimit = 16 << 20
	serviceProfileManifestName  = "manifest.json"
	serviceProfileMarkerName    = ".salcara-service-profile.json"
	serviceProfileLockName      = ".salcara-service-profile.lock"
)

var serviceProfileNames = []string{"config.toml", "auth.json", serviceProfileMarkerName}

// CodexServiceProfile owns only a service-specific transaction record and the
// two explicit files in Home. It never uses Workbench or .salcara-bak backups.
// Construction does not inspect credentials. Callers must keep the directories
// fixed, and must authorize the concrete endpoint/key/model again in guard.
type CodexServiceProfile struct {
	home, state, stateID string
	// Test-only failure/crash injection, set before the manager is shared.
	checkpoint func(string) error
}

type CodexServiceProfileParams struct{ BaseURL, Key, Model string }

// Revision is an opaque CAS token, not a content hash or credential indicator.
// States: idle, applied, conflict, recovery_required. Errors contain fixed text.
type CodexServiceProfileStatus struct {
	State      string `json:"state"`
	Revision   string `json:"revision,omitempty"`
	Generation string `json:"generation,omitempty"`
	HasBackup  bool   `json:"hasBackup"`
	Error      string `json:"error,omitempty"`
}

type serviceProfileFile struct {
	Exists   bool   `json:"exists"`
	Data     []byte `json:"data,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
	Security string `json:"security,omitempty"`
}

type serviceProfileProof struct {
	Exists   bool   `json:"exists"`
	Hash     string `json:"hash,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
	Security string `json:"security,omitempty"`
}

type serviceProfileTransaction struct {
	Kind       string               `json:"kind"`
	Generation string               `json:"generation"`
	Before     []serviceProfileFile `json:"before"`
	After      []serviceProfileFile `json:"after"`
}

type serviceProfileManifest struct {
	Version            int                        `json:"version"`
	Home               string                     `json:"home"`
	Owner              string                     `json:"owner"`
	Nonce              string                     `json:"nonce"`
	State              string                     `json:"state"`
	Generation         string                     `json:"generation,omitempty"`
	Original           []serviceProfileFile       `json:"original,omitempty"`
	Applied            []serviceProfileProof      `json:"applied,omitempty"`
	RestoredGeneration string                     `json:"restoredGeneration,omitempty"`
	RestoredRevision   string                     `json:"restoredRevision,omitempty"`
	Restored           []serviceProfileProof      `json:"restored,omitempty"`
	Pending            *serviceProfileTransaction `json:"pending,omitempty"`
}

type serviceProfileMarker struct {
	Version    int    `json:"version"`
	Owner      string `json:"owner"`
	StateID    string `json:"stateId"`
	Generation string `json:"generation"`
}

func NewCodexServiceProfile(stateDir, codexHome string) (*CodexServiceProfile, error) {
	home, err := canonicalServiceProfilePath(codexHome)
	if err != nil {
		return nil, err
	}
	stateDir, err = canonicalServiceProfilePath(stateDir)
	if err != nil {
		return nil, err
	}
	id := serviceProfileHash([]byte(serviceProfilePathKey(home)))
	state := filepath.Join(stateDir, "codex-service-profiles", id)
	return &CodexServiceProfile{home: home, state: state, stateID: serviceProfileHash([]byte(serviceProfilePathKey(state)))}, nil
}

// HasRecord reads only the private service record, never auth.json/config.toml.
// An idle preview or a completed restore is not an active takeover. A corrupt
// record returns an error instead of being mistaken for no backup.
func (m *CodexServiceProfile) HasRecord() (bool, error) {
	r, err := openServiceProfileDir(m.state, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, ErrCodexProfileUnsafe
	}
	defer r.close()
	f, err := r.read(serviceProfileManifestName, serviceProfileManifestLimit)
	if err != nil {
		return false, err
	}
	if !f.Exists {
		return false, nil
	}
	manifest, err := m.decode(f.Data)
	if err != nil {
		return false, err
	}
	return manifest.State == "applied" || manifest.Pending != nil, nil
}

func (m *CodexServiceProfile) Status(ctx context.Context) (CodexServiceProfileStatus, error) {
	s, err := m.open(ctx)
	if err != nil {
		return serviceProfileFailure(err), err
	}
	defer s.close()
	return s.status()
}

func (m *CodexServiceProfile) Apply(ctx context.Context, expectedRevision string, params CodexServiceProfileParams, guard func() error) (CodexServiceProfileStatus, error) {
	if !serviceProfileToken(expectedRevision) || !validServiceProfileParams(params) {
		return serviceProfileFailure(ErrCodexProfileInvalid), ErrCodexProfileInvalid
	}
	s, err := m.open(ctx)
	if err != nil {
		return serviceProfileFailure(err), err
	}
	defer s.close()
	status, current, err := s.current()
	if err != nil {
		return status, err
	}
	if err = serviceProfileGuard(ctx, guard); err != nil {
		return s.failure(err)
	}
	if status.Revision != expectedRevision {
		return s.failure(ErrCodexProfileStale)
	}
	if status.State == "conflict" {
		return s.failure(ErrCodexProfileChanged)
	}
	if status.State == "recovery_required" {
		return s.failure(ErrCodexProfileRecovery)
	}
	var parsed map[string]any
	if !utf8.Valid(current[0].Data) || toml.Unmarshal(current[0].Data, &parsed) != nil {
		return s.failure(ErrCodexProfileInvalid)
	}
	configText, err := SwitchCodexTOML(string(current[0].Data), params.BaseURL, params.Key, params.Model)
	if err != nil || toml.Unmarshal([]byte(configText), &parsed) != nil || len(configText) > serviceProfileFileLimit {
		return s.failure(ErrCodexProfileInvalid)
	}
	generation, err := newServiceProfileToken()
	if err != nil {
		return s.failure(ErrCodexProfileWrite)
	}
	private, err := serviceProfilePrivateSecurity(false)
	if err != nil {
		return s.failure(ErrCodexProfileUnsafe)
	}
	configFile := current[0]
	if !configFile.Exists {
		configFile = serviceProfileFile{Exists: true, Mode: serviceProfileStoredMode(0600), Security: private}
	}
	configFile.Data = []byte(configText)
	auth, _ := json.Marshal(struct {
		Mode string `json:"auth_mode"`
		Key  string `json:"OPENAI_API_KEY"`
	}{"apikey", params.Key})
	after := []serviceProfileFile{configFile, {Exists: true, Data: append(auth, '\n'), Mode: serviceProfileStoredMode(0600), Security: private}, m.markerFile(s.manifest.Owner, generation, private)}
	next := s.manifest
	if next.State == "idle" {
		next.Original = cloneServiceProfileFiles(current[:2])
	}
	next.State, next.Generation, next.Applied = "applied", generation, serviceProfileProofs(after[:2])
	next.Restored, next.RestoredGeneration, next.RestoredRevision = nil, "", ""
	return s.transact(ctx, guard, "apply", generation, current, after, next, true)
}

// Restore requires both the latest reading and the generation being removed.
// An interrupted journal can be recovered explicitly to the original files;
// each current file must still equal one of that journal's exact before/after
// states. Recovery never adopts an unrecognized external edit.
func (m *CodexServiceProfile) Restore(ctx context.Context, expectedRevision, expectedGeneration string, guard func() error) (CodexServiceProfileStatus, error) {
	if !serviceProfileToken(expectedRevision) || !serviceProfileToken(expectedGeneration) {
		return serviceProfileFailure(ErrCodexProfileInvalid), ErrCodexProfileInvalid
	}
	s, err := m.open(ctx)
	if err != nil {
		return serviceProfileFailure(err), err
	}
	defer s.close()
	status, current, err := s.current()
	if err != nil {
		return status, err
	}
	if s.manifest.State == "idle" && s.manifest.Pending == nil && s.manifest.RestoredGeneration == expectedGeneration && s.manifest.RestoredRevision == expectedRevision && !current[2].Exists && equalServiceProfileProofs(s.manifest.Restored, serviceProfileProofs(current[:2])) {
		return status, nil
	}
	if status.Revision != expectedRevision || status.Generation != expectedGeneration {
		return s.failure(ErrCodexProfileStale)
	}
	if status.State == "conflict" {
		return s.failure(ErrCodexProfileChanged)
	}
	if status.State != "applied" && status.State != "recovery_required" {
		return s.failure(ErrCodexProfileNotOwned)
	}
	if len(s.manifest.Original) != 2 {
		return s.failure(ErrCodexProfileCorrupt)
	}
	after := append(cloneServiceProfileFiles(s.manifest.Original), serviceProfileFile{})
	next := s.manifest
	next.State, next.Generation, next.Original, next.Applied, next.Pending = "idle", "", nil, nil, nil
	next.RestoredGeneration, next.RestoredRevision, next.Restored = expectedGeneration, expectedRevision, serviceProfileProofs(after[:2])
	kind := "restore"
	rollback := s.manifest.Pending == nil
	if !rollback {
		kind = "recover"
	}
	return s.transact(ctx, guard, kind, expectedGeneration, current, after, next, rollback)
}

type serviceProfileSession struct {
	manager     *CodexServiceProfile
	home, state *serviceProfileDir
	manifest    serviceProfileManifest
	release     func()
}

func (m *CodexServiceProfile) open(ctx context.Context) (*serviceProfileSession, error) {
	release, err := LockCodexProfile(ctx, m.home)
	if err != nil {
		return nil, err
	}
	s := &serviceProfileSession{manager: m, release: release}
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()
	s.home, err = openServiceProfileDir(m.home, false)
	if err != nil {
		return nil, ErrCodexProfileUnsafe
	}
	s.state, err = openServiceProfileDir(m.state, true)
	if err != nil {
		return nil, ErrCodexProfileUnsafe
	}
	if err = s.state.private(); err != nil {
		return nil, ErrCodexProfileUnsafe
	}
	f, err := s.state.read(serviceProfileManifestName, serviceProfileManifestLimit)
	if err != nil {
		return nil, err
	}
	if f.Exists {
		s.manifest, err = m.decode(f.Data)
	} else {
		owner, tokenErr := newServiceProfileToken()
		if tokenErr != nil {
			return nil, ErrCodexProfileWrite
		}
		s.manifest = serviceProfileManifest{Version: 1, Home: m.home, Owner: owner, State: "idle"}
		err = s.save(&s.manifest)
	}
	if err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func (s *serviceProfileSession) close() {
	if s.home != nil {
		s.home.close()
	}
	if s.state != nil {
		s.state.close()
	}
	if s.release != nil {
		s.release()
	}
}

func (s *serviceProfileSession) save(m *serviceProfileManifest) error {
	nonce, err := newServiceProfileToken()
	if err != nil {
		return ErrCodexProfileWrite
	}
	m.Nonce = nonce
	data, err := json.Marshal(m)
	if err != nil || len(data) > serviceProfileManifestLimit {
		return ErrCodexProfileWrite
	}
	security, err := serviceProfilePrivateSecurity(false)
	if err != nil {
		return ErrCodexProfileUnsafe
	}
	return s.state.write(serviceProfileManifestName, serviceProfileFile{Exists: true, Data: data, Mode: serviceProfileStoredMode(0600), Security: security})
}

func (s *serviceProfileSession) readFiles() ([]serviceProfileFile, error) {
	files := make([]serviceProfileFile, len(serviceProfileNames))
	for i, name := range serviceProfileNames {
		limit := serviceProfileFileLimit
		if i == 2 {
			limit = 4096
		}
		f, err := s.home.read(name, limit)
		if err != nil {
			return nil, err
		}
		files[i] = f
	}
	return files, nil
}

func (s *serviceProfileSession) current() (CodexServiceProfileStatus, []serviceProfileFile, error) {
	files, err := s.readFiles()
	if err != nil {
		return serviceProfileFailure(err), nil, err
	}
	m := s.manifest
	proofs := serviceProfileProofs(files)
	encoded, _ := json.Marshal(proofs)
	status := CodexServiceProfileStatus{State: m.State, Revision: serviceProfileHash(append([]byte(m.Nonce), encoded...)), Generation: m.Generation, HasBackup: len(m.Original) == 2}
	if m.Pending != nil {
		status.State, status.Generation, status.Error = "recovery_required", m.Pending.Generation, ErrCodexProfileRecovery.Error()
		if !serviceProfileJournalMatches(files, m.Pending) {
			status.State, status.Error = "conflict", ErrCodexProfileChanged.Error()
		}
	} else if m.State == "applied" {
		private, privateErr := serviceProfilePrivateSecurity(false)
		if privateErr != nil {
			return serviceProfileFailure(ErrCodexProfileUnsafe), nil, ErrCodexProfileUnsafe
		}
		marker := s.manager.markerFile(m.Owner, m.Generation, private)
		if !equalServiceProfileProofs(proofs[:2], m.Applied) || !equalServiceProfileFile(files[2], marker) {
			status.State, status.Error = "conflict", ErrCodexProfileChanged.Error()
		}
	} else if files[2].Exists {
		status.State, status.Error = "conflict", ErrCodexProfileOwned.Error()
	}
	return status, files, nil
}

func (s *serviceProfileSession) status() (CodexServiceProfileStatus, error) {
	value, _, err := s.current()
	return value, err
}
func (s *serviceProfileSession) failure(err error) (CodexServiceProfileStatus, error) {
	value, readErr := s.status()
	if readErr != nil {
		return serviceProfileFailure(readErr), err
	}
	value.Error = serviceProfilePublicError(err)
	return value, err
}

func (s *serviceProfileSession) transact(ctx context.Context, guard func() error, kind, generation string, before, after []serviceProfileFile, next serviceProfileManifest, rollback bool) (CodexServiceProfileStatus, error) {
	previous := s.manifest
	journal := previous
	if kind == "apply" && previous.State == "idle" {
		journal.Original = cloneServiceProfileFiles(before[:2])
	}
	journal.Pending = &serviceProfileTransaction{Kind: kind, Generation: generation, Before: cloneServiceProfileFiles(before), After: cloneServiceProfileFiles(after)}
	if err := s.checkExact(before); err != nil {
		return s.failure(err)
	}
	if err := serviceProfileGuard(ctx, guard); err != nil {
		return s.failure(err)
	}
	if err := s.checkExact(before); err != nil {
		return s.failure(err)
	}
	if err := s.save(&journal); err != nil {
		return s.failure(err)
	}
	s.manifest = journal
	fail := func(cause error) (CodexServiceProfileStatus, error) {
		if rollback && s.rollback(journal.Pending, previous) == nil {
			return s.failure(cause)
		}
		return s.failure(ErrCodexProfileRecovery)
	}
	if err := s.checkpoint("journal"); err != nil {
		return fail(err)
	}
	// Claim first, release last. Other cooperating writers cannot stack their
	// backups over a service-owned profile, including between process restarts.
	order := []int{2, 1, 0}
	if kind != "apply" {
		order = []int{1, 0, 2}
	}
	expected := cloneServiceProfileFiles(before)
	for _, i := range order {
		if err := serviceProfileGuard(ctx, guard); err != nil {
			return fail(err)
		}
		if err := s.checkExact(expected); err != nil {
			return fail(err)
		}
		if !equalServiceProfileFile(expected[i], after[i]) {
			if err := s.home.write(serviceProfileNames[i], after[i]); err != nil {
				return fail(err)
			}
			expected[i] = after[i]
		}
		if err := s.checkpoint(serviceProfileNames[i]); err != nil {
			return fail(err)
		}
	}
	if err := s.checkExact(after); err != nil {
		return fail(err)
	}
	if err := serviceProfileGuard(ctx, guard); err != nil {
		return fail(err)
	}
	if err := s.checkpoint("commit"); err != nil {
		return fail(err)
	}
	if err := s.checkExact(after); err != nil {
		return fail(err)
	}
	next.Pending = nil
	if err := s.save(&next); err != nil {
		return fail(err)
	}
	s.manifest = next
	if err := s.checkpoint("published"); err != nil {
		return s.failure(err)
	}
	// An idle status permits later user edits, so the state label alone is not
	// sufficient evidence that a restore reached its exact terminal bytes.
	if err := s.checkExact(after); err != nil {
		return s.failure(err)
	}
	status, err := s.status()
	if err != nil {
		return status, err
	}
	if status.State != next.State {
		status.Error = ErrCodexProfileChanged.Error()
		return status, ErrCodexProfileChanged
	}
	return status, nil
}

func (s *serviceProfileSession) checkpoint(at string) error {
	if s.manager.checkpoint != nil {
		return s.manager.checkpoint(at)
	}
	return nil
}

func (s *serviceProfileSession) checkExact(expected []serviceProfileFile) error {
	current, err := s.readFiles()
	if err != nil {
		return err
	}
	if !equalServiceProfileProofs(serviceProfileProofs(current), serviceProfileProofs(expected)) {
		return ErrCodexProfileChanged
	}
	return nil
}

func (s *serviceProfileSession) rollback(tx *serviceProfileTransaction, previous serviceProfileManifest) error {
	for _, i := range []int{1, 0, 2} {
		current, err := s.readFiles()
		if err != nil || !serviceProfileJournalMatches(current, tx) {
			return ErrCodexProfileRecovery
		}
		if !equalServiceProfileFile(current[i], tx.Before[i]) {
			if err := s.home.write(serviceProfileNames[i], tx.Before[i]); err != nil {
				return err
			}
		}
	}
	if err := s.checkExact(tx.Before); err != nil {
		return err
	}
	if err := s.save(&previous); err != nil {
		return err
	}
	s.manifest = previous
	return nil
}

func (m *CodexServiceProfile) decode(data []byte) (serviceProfileManifest, error) {
	var value serviceProfileManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Version != 1 || serviceProfilePathKey(value.Home) != serviceProfilePathKey(m.home) || !serviceProfileToken(value.Owner) || !serviceProfileToken(value.Nonce) || value.State != "idle" && value.State != "applied" {
		return value, ErrCodexProfileCorrupt
	}
	if value.State == "applied" && (!serviceProfileToken(value.Generation) || len(value.Original) != 2 || len(value.Applied) != 2) {
		return value, ErrCodexProfileCorrupt
	}
	if value.State == "idle" && (value.Generation != "" || len(value.Applied) != 0) {
		return value, ErrCodexProfileCorrupt
	}
	if len(value.Original) != 0 && len(value.Original) != 2 || len(value.Restored) != 0 && len(value.Restored) != 2 {
		return value, ErrCodexProfileCorrupt
	}
	if value.RestoredGeneration != "" && (!serviceProfileToken(value.RestoredGeneration) || !serviceProfileToken(value.RestoredRevision) || len(value.Restored) != 2) {
		return value, ErrCodexProfileCorrupt
	}
	for _, f := range value.Original {
		if !validServiceProfileFile(f, serviceProfileFileLimit) {
			return value, ErrCodexProfileCorrupt
		}
	}
	for _, p := range append(append([]serviceProfileProof{}, value.Applied...), value.Restored...) {
		if !validServiceProfileProof(p) {
			return value, ErrCodexProfileCorrupt
		}
	}
	if tx := value.Pending; tx != nil {
		if tx.Kind != "apply" && tx.Kind != "restore" && tx.Kind != "recover" || !serviceProfileToken(tx.Generation) || len(tx.Before) != 3 || len(tx.After) != 3 || len(value.Original) != 2 {
			return value, ErrCodexProfileCorrupt
		}
		for i := range tx.Before {
			limit := serviceProfileFileLimit
			if i == 2 {
				limit = 4096
			}
			if !validServiceProfileFile(tx.Before[i], limit) || !validServiceProfileFile(tx.After[i], limit) {
				return value, ErrCodexProfileCorrupt
			}
		}
		if tx.Kind == "apply" {
			if !equalServiceProfileFile(tx.After[2], m.markerFile(value.Owner, tx.Generation, tx.After[2].Security)) {
				return value, ErrCodexProfileCorrupt
			}
		} else if tx.After[2].Exists || !equalServiceProfileProofs(serviceProfileProofs(tx.After[:2]), serviceProfileProofs(value.Original)) {
			return value, ErrCodexProfileCorrupt
		}
	}
	return value, nil
}

func (m *CodexServiceProfile) markerFile(owner, generation, security string) serviceProfileFile {
	data, _ := json.Marshal(serviceProfileMarker{1, owner, m.stateID, generation})
	return serviceProfileFile{Exists: true, Data: data, Mode: serviceProfileStoredMode(0600), Security: security}
}

func serviceProfileGuard(ctx context.Context, guard func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard == nil || guard() != nil {
		return ErrCodexProfileAuthorization
	}
	return ctx.Err()
}

func validServiceProfileParams(p CodexServiceProfileParams) bool {
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || len(p.BaseURL) > 4096 {
		return false
	}
	for _, text := range []string{p.BaseURL, p.Key, p.Model} {
		if !utf8.ValidString(text) || strings.TrimSpace(text) != text || text == "" || strings.ContainsAny(text, "\x00\r\n") {
			return false
		}
	}
	return len(p.Key) <= 64<<10 && len(p.Model) <= 512
}

func serviceProfileJournalMatches(files []serviceProfileFile, tx *serviceProfileTransaction) bool {
	if len(files) != 3 || len(tx.Before) != 3 || len(tx.After) != 3 {
		return false
	}
	for i, file := range files {
		if !equalServiceProfileFile(file, tx.Before[i]) && !equalServiceProfileFile(file, tx.After[i]) {
			return false
		}
	}
	return true
}

func serviceProfileProofs(files []serviceProfileFile) []serviceProfileProof {
	out := make([]serviceProfileProof, len(files))
	for i, file := range files {
		if file.Exists {
			out[i] = serviceProfileProof{true, serviceProfileHash(file.Data), file.Mode, file.Security}
		}
	}
	return out
}
func equalServiceProfileProofs(a, b []serviceProfileProof) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Exists != b[i].Exists || a[i].Hash != b[i].Hash || a[i].Mode != b[i].Mode || !serviceProfileSecurityEqual(a[i].Security, b[i].Security) {
			return false
		}
	}
	return true
}
func equalServiceProfileFile(a, b serviceProfileFile) bool {
	return a.Exists == b.Exists && a.Mode == b.Mode && serviceProfileSecurityEqual(a.Security, b.Security) && bytes.Equal(a.Data, b.Data)
}
func validServiceProfileFile(f serviceProfileFile, limit int) bool {
	if !f.Exists {
		return len(f.Data) == 0 && f.Mode == 0 && f.Security == ""
	}
	return len(f.Data) <= limit && f.Mode & ^uint32(0777) == 0 && len(f.Security) <= 64<<10 && serviceProfileValidSecurity(f.Security)
}
func validServiceProfileProof(p serviceProfileProof) bool {
	if !p.Exists {
		return p.Hash == "" && p.Mode == 0 && p.Security == ""
	}
	return serviceProfileToken(p.Hash) && p.Mode & ^uint32(0777) == 0 && len(p.Security) <= 64<<10 && serviceProfileValidSecurity(p.Security)
}
func cloneServiceProfileFiles(files []serviceProfileFile) []serviceProfileFile {
	out := append([]serviceProfileFile(nil), files...)
	for i := range out {
		out[i].Data = append([]byte(nil), out[i].Data...)
	}
	return out
}
func serviceProfileHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func newServiceProfileToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
func serviceProfileToken(value string) bool {
	raw, err := hex.DecodeString(value)
	return len(raw) == 32 && err == nil && hex.EncodeToString(raw) == value
}
func serviceProfilePublicError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "服务配置操作已取消"
	}
	for _, safe := range []error{ErrCodexProfileInvalid, ErrCodexProfileUnsafe, ErrCodexProfileCorrupt, ErrCodexProfileChanged, ErrCodexProfileStale, ErrCodexProfileOwned, ErrCodexProfileNotOwned, ErrCodexProfileAuthorization, ErrCodexProfileRecovery, ErrCodexProfileWrite} {
		if errors.Is(err, safe) {
			return safe.Error()
		}
	}
	return ErrCodexProfileWrite.Error()
}
func serviceProfileFailure(err error) CodexServiceProfileStatus {
	return CodexServiceProfileStatus{State: "conflict", Error: serviceProfilePublicError(err)}
}
