// Package apiservice provides an authenticated, account-scoped local Codex API.
// It reuses the gateway protocol boundary, not the remote Hub authorization.
package apiservice

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/pluginhook"
	"salcara/bridge/internal/protocol"
	"salcara/bridge/internal/usageledger"

	"github.com/gorilla/websocket"
)

const (
	maxServiceInflight     = 32
	maxRequestBodyBytes    = 64 << 20
	requestBodyReadTimeout = 60 * time.Second
)

type Stats struct {
	ClientID       string `json:"clientId"`
	Requests       uint64 `json:"requests"`
	Succeeded      uint64 `json:"http2xx"` // HTTP outcome, not task completion or billed usage.
	Failed         uint64 `json:"httpErrors"`
	Inflight       int    `json:"inflight"`
	LastStatus     int    `json:"lastStatus,omitempty"`
	LastDurationMS int64  `json:"lastDurationMs,omitempty"`
}
type Service struct {
	Plugins    *pluginhook.Hooks
	Logger     *log.Logger
	Observe    func(usageledger.Observation)
	BeginUsage func(string, uint64) func()
	Config     func() config.Config
	// Health tracks per-account request outcomes for smart pool selection.
	// Initialised automatically by New(); callers should not set this directly.
	Health *accounthealth.Tracker
	// AllowRemote may approve a non-loopback socket after the API-only listener
	// has proven its private-network and service-epoch policy. It never bypasses
	// Origin, client-key, account, model, or runtime-lease checks.
	AllowRemote    func(*http.Request, config.Config) bool
	Official       func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error)
	OfficialImages func(context.Context, config.LocalAccount, []byte, bool, string, func() bool) (*http.Response, error)
	// ModelCatalog is read-only shared native/saved picker metadata, never an
	// official-account entitlement check or an inference request.
	ModelCatalog  func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error)
	Client        *http.Client
	Pool          *gateway.AccountPool
	mu            sync.Mutex
	stats         map[string]*Stats
	inflight      int
	launchPending bool
	launchKey     string
	// Closing belongs to this Service instance, not a mutable Config snapshot.
	// A late lifecycle commit can never reopen its request admission.
	admissionClosed atomic.Bool
	images          imageQueue
	// Tests may lower these bounds; values above production limits are clamped.
	maxInflight       int
	bodyReadTimeout   time.Duration
	firstByteTimeout  time.Duration
	streamIdleTimeout time.Duration
}

func (s *Service) logf(format string, args ...any) {
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}

func New(get func() config.Config) *Service {
	s := &Service{Config: get, stats: map[string]*Stats{}, Client: gateway.New(nil, nil).Client}
	s.initHealth()
	return s
}

// CloseAdmission irreversibly fences new and queued requests for this service.
// Already dispatched work keeps draining; reopening requires a new instance.
// It does not wait for configuration callbacks, uploads, or a profile writer.
func (s *Service) CloseAdmission() {
	s.admissionClosed.Store(true)
}

func (s *Service) Snapshot() []Stats {
	var cfg config.Config
	if s.Config != nil {
		cfg = s.Config()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStatsLocked(cfg)
	out := make([]Stats, 0, len(s.stats))
	for _, v := range s.stats {
		out = append(out, *v)
	}
	return out
}

func (s *Service) pruneStatsLocked(cfg config.Config) {
	active := make(map[string]bool, len(cfg.CodexAPI.Clients))
	for _, client := range cfg.CodexAPI.Clients {
		active[client.ID] = true
	}
	for id, state := range s.stats {
		if !active[id] && state.Inflight == 0 {
			delete(s.stats, id)
		}
	}
}

// A deadline applies only while uploading the body. A bounded watcher also
// closes cancellable bodies in tests/embedded handlers without socket support.
// Neither the timer nor its context survives into the downstream response.
func (s *Service) readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	timeout := s.bodyReadTimeout
	if timeout <= 0 || timeout > requestBodyReadTimeout {
		timeout = requestBodyReadTimeout
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(timeout))
	stop, stopped := make(chan struct{}), make(chan struct{})
	var expired atomic.Bool
	go func() {
		defer close(stopped)
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-r.Context().Done():
		case <-timer.C:
			expired.Store(true)
		}
		_ = controller.SetReadDeadline(time.Now())
		_ = r.Body.Close()
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		// Do not clear a socket timeout while net/http still has an unread body
		// it might otherwise try to drain before writing our error response.
		w.Header().Set("Connection", "close")
		_ = controller.SetReadDeadline(time.Now())
		_ = r.Body.Close()
	}
	close(stop)
	<-stopped
	_ = controller.SetReadDeadline(time.Time{})
	var timeoutError net.Error
	if expired.Load() || r.Context().Err() != nil || (errors.As(err, &timeoutError) && timeoutError.Timeout()) {
		w.Header().Set("Connection", "close")
		return nil, http.StatusRequestTimeout
	}
	if err != nil {
		return nil, http.StatusRequestEntityTooLarge
	}
	return body, 0
}
func fail(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "salcara_local_api_error", "message": http.StatusText(status)}})
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	managedTarget, path, validPath := managedAPIPath(r.URL.Path)
	native := parseNativeRoute(path)
	if native != nil {
		w = &nativeErrorWriter{ResponseWriter: w, kind: native.kind}
	}
	if s.admissionClosed.Load() || s.Config == nil {
		fail(w, 503)
		return
	}
	c := s.Config()
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	remoteAllowed := false
	if err == nil && ip != nil && !ip.IsLoopback() && s.AllowRemote != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.logf("AllowRemote panic: %v", r)
				}
			}()
			remoteAllowed = s.AllowRemote(r, c)
		}()
	}
	if err != nil || ip == nil || !ip.IsLoopback() && !remoteAllowed || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fail(w, 403)
		return
	}
	if !c.CodexAPIActive() {
		fail(w, 503)
		return
	}
	if !validPath {
		fail(w, 404)
		return
	}
	if managedTarget == serviceProfileTarget && !ip.IsLoopback() {
		fail(w, 403)
		return
	}
	key, keyStatus := localAPIKey(r, path, managedTarget, native)
	if keyStatus != 0 {
		fail(w, keyStatus)
		return
	}
	// Keep only the public path in observations, never query credentials. The
	// inner gateway sees the canonical authenticated request without native keys.
	observationPath := path
	if native != nil {
		if native.kind == "gemini" && !native.models {
			observationPath = "v1beta/models/{model}:generateContent"
			if native.stream {
				observationPath = "v1beta/models/{model}:streamGenerateContent"
			}
		}
		if managedTarget != "" {
			fail(w, 404)
			return
		}
		if native.models {
			path = "v1/models"
		} else {
			path = "v1/chat/completions"
		}
	}
	digest := config.APIClientDigest(key)
	var selected config.NamedAPIClient
	found := false
	for _, client := range c.CodexAPI.Clients {
		if managedTarget != "" {
			break
		}
		if subtle.ConstantTimeCompare([]byte(digest), []byte(client.KeyDigest)) == 1 {
			selected, found = client, true
			break
		}
	}
	if managedTarget != "" && ip.IsLoopback() {
		selected, found = resolveManagedAPIClient(c, managedTarget, key)
	}
	if !found || selected.Disabled || config.ValidateAPIClient(selected) != nil {
		fail(w, 401)
		return
	}
	messages := path == "v1/messages" || path == "v1/messages/count_tokens"
	if managedTarget == serviceProfileTarget && path != "v1/models" && path != "v1/responses" && path != "v1/responses/compact" {
		fail(w, 404)
		return
	}
	if managedTarget != "" && path != "v1/models" && (strings.HasPrefix(managedTarget, "claude") != messages) {
		fail(w, 404)
		return
	}
	started := time.Now()
	// Use the exact same route/method decision for lease ownership and dispatch;
	// upgrade-looking headers on an ordinary POST do not make it a socket turn.
	webSocketRequest := path == "v1/responses" && r.Method == http.MethodGet && websocket.IsWebSocketUpgrade(r)
	var releaseUsage func()
	defer func() {
		if releaseUsage != nil {
			releaseUsage()
		}
	}()
	beginUsage := func(id string, epoch uint64) func() {
		if s.BeginUsage == nil {
			return func() {}
		}
		release := s.BeginUsage(id, epoch)
		// WebSocket inner turns queue their own observations; ordinary HTTP
		// observation is queued by this handler's final defer.
		if webSocketRequest {
			return release
		}
		previousRelease := releaseUsage
		releaseUsage = func() {
			if previousRelease != nil {
				previousRelease()
			}
			if release != nil {
				release()
			}
		}
		return func() {}
	}
	ledgerObservation := usageledger.Observation{StartedAt: started, AccountID: selected.AccountID, ClientID: selected.ID, Route: "codex-api/" + observationPath, State: "rejected"}
	status := 200
	tracked := &statusWriter{ResponseWriter: w, status: &status}
	w = tracked
	defer func() {
		if s.Observe == nil || tracked.hijacked {
			return
		}
		ledgerObservation.HTTPStatus, ledgerObservation.DurationMS = status, time.Since(started).Milliseconds()
		if r.Context().Err() != nil {
			ledgerObservation.State = "cancelled"
		} else if tracked.writeFailed {
			ledgerObservation.State = "interrupted"
		} else if status >= 500 && ledgerObservation.State == "rejected" {
			ledgerObservation.State = "failed"
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.logf("Observe panic: %v", r)
				}
			}()
			s.Observe(ledgerObservation)
		}()
	}()
	textPoolClient := namedTextPoolEnabled(selected)
	a, ok := c.LocalAccount(selected.AccountID)
	if !ok && textPoolClient {
		// AccountID remains the compatibility anchor, but an explicit text pool
		// is allowed to operate when that anchor was retired. Pick only a listed
		// member for the socket/bootstrap snapshot; execution selects per request.
		for _, id := range selected.AccountIDs {
			if candidate, present := c.LocalAccount(id); present {
				a, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		fail(w, 409)
		return
	}
	textVirtualGroup := ""
	if path != "v1/models" && path != "v1/responses" && path != "v1/responses/compact" && path != "v1/chat/completions" && !messages && !imagePath(path) {
		fail(w, 404)
		return
	}
	if path == "v1/models" && r.Method != http.MethodGet || path != "v1/models" && r.Method != http.MethodPost && !webSocketRequest {
		fail(w, 405)
		return
	}
	if c.CodexAPI.ImagesMode == "images_only" && path != "v1/models" && !imagePath(path) {
		fail(w, 503)
		return
	}
	// Admission precedes all body allocation/read work, including slow uploads.
	s.mu.Lock()
	if s.admissionClosed.Load() || s.launchPending && (managedTarget == "" || s.launchKey == "" || key != s.launchKey) {
		s.mu.Unlock()
		fail(w, 503)
		return
	}
	if s.stats == nil {
		s.stats = map[string]*Stats{}
	}
	s.pruneStatsLocked(c)
	state := s.stats[selected.ID]
	if state == nil {
		state = &Stats{ClientID: selected.ID}
		s.stats[selected.ID] = state
	}
	limit := selected.MaxConcurrency
	if limit == 0 {
		limit = 16
	}
	globalLimit := s.maxInflight
	if globalLimit <= 0 || globalLimit > maxServiceInflight {
		globalLimit = maxServiceInflight
	}
	if state.Inflight >= limit || s.inflight >= globalLimit {
		s.mu.Unlock()
		fail(w, 429)
		return
	}
	state.Inflight++
	state.Requests++
	s.inflight++
	s.mu.Unlock()
	defer func() {
		latest := s.Config()
		s.mu.Lock()
		state.Inflight--
		s.inflight--
		state.LastStatus = status
		state.LastDurationMS = time.Since(started).Milliseconds()
		if status == http.StatusSwitchingProtocols {
			// Successful upgrades are neither HTTP 2xx nor HTTP errors. The
			// socket still owns admission until disconnect; real turns own usage.
		} else if status >= 200 && status < 300 {
			state.Succeeded++
		} else {
			state.Failed++
		}
		s.pruneStatsLocked(latest)
		s.mu.Unlock()
	}()
	validService := func(latest config.Config) bool {
		return !s.admissionClosed.Load() && latest.CodexAPIActive() && latest.CodexAPI.AuthEpoch == c.CodexAPI.AuthEpoch &&
			latest.GatewayRuntimeKey == c.GatewayRuntimeKey && latest.CodexAPI.RuntimeLease == c.CodexAPI.RuntimeLease
	}
	validClient := func() bool {
		latest := s.Config()
		next, ok := latest.APIClient(selected.ID)
		// Closing the service fences work still waiting for its body, bootstrap
		// or refresh. Requests already forwarded keep draining uninterrupted.
		managedCurrent := managedTarget == "" || managedAPICredential(latest, managedTarget) == key
		return validService(latest) && ok && reflect.DeepEqual(next, selected) && managedCurrent
	}
	source := func(string) (config.LocalAccount, bool) {
		latest := s.Config()
		nextClient, exists := latest.APIClient(selected.ID)
		if !validService(latest) || !exists || !reflect.DeepEqual(nextClient, selected) || managedTarget != "" && managedAPICredential(latest, managedTarget) != key {
			return config.LocalAccount{}, false
		}
		next, ok := latest.LocalAccount(a.ID)
		if !ok {
			return next, false
		}
		next = codexDispatchAccount(next)
		if textVirtualGroup != "" {
			next.PoolEnabled = true
			next.PoolGroup = textVirtualGroup
		}
		expected := codexDispatchAccount(a)
		if config.AccountFingerprint(next) != config.AccountFingerprint(expected) {
			return next, false
		}
		return next, true
	}
	if !validClient() {
		fail(tracked, 409)
		return
	}
	primaryRelease := func() {}
	if s.Pool != nil && !textPoolClient && path != "v1/models" && !imagePath(path) {
		primaryRelease, err = s.Pool.AdmitFixed(a, c.LocalAccounts)
		if err != nil {
			fail(tracked, 429)
			return
		}
		defer func() { primaryRelease() }()
	}
	if imagePath(path) {
		s.serveImages(tracked, r, path, selected, c, validClient, &ledgerObservation, beginUsage)
		return
	}
	var body []byte
	var actualModel string
	if r.Method == http.MethodPost {
		var readStatus int
		body, readStatus = s.readRequestBody(w, r)
		if readStatus != 0 {
			fail(tracked, readStatus)
			return
		}
		if native != nil {
			body, err = native.request(body)
			if err != nil {
				fail(tracked, 400)
				return
			}
		}
		if path == "v1/chat/completions" && !gateway.ValidChatEnvelope(body) {
			fail(tracked, 400)
			return
		}
		if messages && !gateway.ValidMessagesEnvelope(body) || !messages && path != "v1/chat/completions" && !gateway.ValidResponsesEnvelope(body) {
			fail(tracked, 400)
			return
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(body, &input) != nil {
			fail(tracked, 400)
			return
		}
		var model string
		if json.Unmarshal(input["model"], &model) != nil {
			fail(tracked, 400)
			return
		}
		if managedTarget == "claude-desktop" {
			binding := c.ToolAPIApplied[config.ToolFamily(managedTarget)]
			visible, ok := config.ResolveClaudeDesktopModel(config.LocalAccount{Models: binding.ManagedModels}, model)
			if !ok {
				fail(tracked, 403)
				return
			}
			model = visible
			input["model"], _ = json.Marshal(model)
			body, _ = json.Marshal(input)
		}
		actual, allowed := config.ResolveAPIClientModel(selected, model)
		if !allowed {
			fail(tracked, http.StatusForbidden)
			return
		}
		if actual != model {
			input["model"], _ = json.Marshal(actual)
			body, err = json.Marshal(input)
			if err != nil {
				fail(tracked, 400)
				return
			}
		}
		actualModel = actual
		if path == "v1/responses" {
			body, readStatus = resolveImageToolModels(body, selected)
			if readStatus != 0 {
				fail(tracked, readStatus)
				return
			}
		}
	}
	if path == "v1/models" {
		models := append(append([]string{}, a.Models...), a.Model)
		nativeSource := ""
		var textCandidates []config.LocalAccount
		if textPoolClient {
			var catalogStatus int
			models, catalogStatus = namedTextCatalog(c, selected)
			if catalogStatus != 0 {
				fail(tracked, catalogStatus)
				return
			}
			textCandidates, catalogStatus = namedTextPoolCandidates(c, selected)
			if catalogStatus != 0 {
				fail(tracked, catalogStatus)
				return
			}
		} else if a.CredentialRef != "" || a.SourceType == "official_account" {
			if s.ModelCatalog == nil {
				fail(tracked, 503)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			catalog, catalogErr := s.ModelCatalog(ctx, a, false)
			catalogExpired := ctx.Err() != nil
			cancel()
			if catalogErr != nil || catalogExpired || len(catalog.Models) > 1000 {
				fail(tracked, 502)
				return
			}
			models = catalog.Models
			nativeSource = "official-picker-hint"
			for _, capability := range catalog.ModelCapabilities {
				if capability.Source == "codex-native-picker-hint" || capability.Source == "saved-official-picker-hint" {
					nativeSource = capability.Source
					break
				}
			}
		}
		seen := map[string]bool{}
		data := []map[string]string{}
		for _, model := range models {
			model = strings.TrimSpace(model)
			// An alias shadows an equally named native model on requests, so only
			// its resolved target may authorize that name in discovery as well.
			if _, shadowed := selected.ModelAliases[model]; shadowed {
				continue
			}
			visible, public := config.APIClientVisibleModel(selected, model)
			memberAllowed := config.ModelAllowed(a, model)
			if textPoolClient {
				memberAllowed = namedTextModelAllowed(textCandidates, model)
			}
			if public && memberAllowed && config.APIClientModelAllowed(selected, model) && !seen[visible] {
				seen[visible] = true
				owner := "configured-account"
				if nativeSource != "" {
					owner = "official-picker-hint"
				}
				data = append(data, map[string]string{"id": visible, "object": "model", "owned_by": owner})
			}
		}
		for alias, actual := range selected.ModelAliases {
			visible, public := config.APIClientVisibleModel(selected, alias)
			memberAllowed := config.ModelAllowed(a, actual)
			if textPoolClient {
				memberAllowed = namedTextModelAllowed(textCandidates, actual)
			}
			if public && memberAllowed && config.APIClientModelAllowed(selected, actual) && !seen[visible] {
				seen[visible] = true
				data = append(data, map[string]string{"id": visible, "object": "model", "owned_by": "configured-alias"})
			}
		}
		// Catalogue reads can outlive client rotation, policy or identity changes.
		if _, current := source(""); !current || r.Context().Err() != nil {
			fail(tracked, 409)
			return
		}
		response := map[string]any{"object": "list", "data": data}
		if nativeSource != "" {
			ids := make([]string, 0, len(data))
			for _, model := range data {
				ids = append(ids, model["id"])
			}
			response["nativeSource"], response["catalogIsEntitlement"] = nativeSource, false
			response["modelCapabilities"] = protocol.UnknownModelCapabilities(ids, nativeSource)
		}
		tracked.Header().Set("Content-Type", "application/json")
		ledgerObservation.State = "read_only"
		if native != nil {
			_ = json.NewEncoder(tracked).Encode(native.catalog(data))
		} else {
			_ = json.NewEncoder(tracked).Encode(response)
		}
		return
	}
	imagePreAdmitted := false
	if path == "v1/responses" && !webSocketRequest {
		imageModels, imageStatus := resolvedImageToolModels(body)
		if imageStatus != 0 {
			fail(tracked, imageStatus)
			return
		}
		if len(imageModels) > 0 {
			if !config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) || config.ValidateCodexAPIImages(c.CodexAPI.ImagesMode, c.CodexAPI.MaxConcurrentImageRequests) != nil {
				fail(tracked, 503)
				return
			}
			for _, model := range imageModels {
				if !config.APIClientModelAllowed(selected, model) {
					fail(tracked, 403)
					return
				}
			}
			models := append([]string{actualModel}, imageModels...)
			preAdmittedKey := ""
			if webSocketRequest {
				preAdmittedKey, _ = imageBudgetKey(a)
			} else {
				// Slow upload was protected by the fixed account reservation. Once
				// an explicit image tool is known, release that provisional slot and
				// let the image-only member selector own the actual execution.
				primaryRelease()
				primaryRelease = func() {}
			}
			chosen, _, release, imageStatus := s.acquireImageAccount(r.Context(), c, selected, models, validClient, preAdmittedKey)
			if imageStatus != 0 {
				fail(tracked, imageStatus)
				return
			}
			a = chosen
			ledgerObservation.AccountID = chosen.ID
			imagePreAdmitted = true
			defer release()
		}
	}
	var initialTextRelease func()
	if textPoolClient && !webSocketRequest && !imagePreAdmitted {
		chosen, current, release, textStatus := s.selectNamedTextAccount(c, selected, actualModel, namedTextSessionKey(r, body, ""), validClient)
		if textStatus != 0 {
			fail(tracked, textStatus)
			return
		}
		if current == nil || !current() {
			release()
			fail(tracked, 409)
			return
		}
		a = chosen
		initialTextRelease = release
		textVirtualGroup = chosen.PoolGroup
		ledgerObservation.AccountID = chosen.ID
		defer release()
	}
	h := gateway.New(func() string {
		if validClient() {
			return key
		}
		return ""
	}, source)
	h.Official = s.Official
	if s.Health != nil {
		if scope, valid := gateway.CredentialBudgetIdentity(a); valid {
			s.Health.Bind(a.ID, scope)
		}
	}
	h.FirstByteTimeout, h.StreamIdleTimeout = s.firstByteTimeout, s.streamIdleTimeout
	h.InitialAttemptRelease = initialTextRelease
	h.AttemptObserver = s.recordExecutionOutcome
	if messages {
		h.Source = func(target string) (config.LocalAccount, bool) {
			selected, ok := source(target)
			selected.Kind, selected.Target = "claude", "claude"
			return selected, ok
		}
	}
	h.Plugins = s.Plugins
	h.ChatCompletions = true
	if !messages && (path != "v1/responses" || webSocketRequest) {
		h.NormalizeRequest = func(body []byte) ([]byte, int) { return resolveImageToolModels(body, selected) }
	}
	h.WebSocketModel = func(model string) (string, bool) {
		actual, allowed := config.ResolveAPIClientModel(selected, model)
		return actual, allowed
	}
	if textPoolClient {
		// Warmup frames do not invoke the execution selector. Check the union
		// of explicitly listed text members (and image members for a Responses
		// parent model) while real frames recheck the selected card below.
		h.WebSocketModelKnown = func(model string) bool {
			candidates, status := namedTextPoolCandidates(c, selected)
			if status == 0 && namedTextModelAllowed(candidates, model) {
				return true
			}
			for _, id := range imageAccountIDs(selected) {
				account, present := c.LocalAccount(id)
				if !present || !config.ModelAllowed(account, model) {
					continue
				}
				if account.CredentialRef != "" && account.SourceType == "official_account" && config.ValidCatalogModel(model) {
					return true
				}
				if account.Model == model || len(account.Models) == 0 {
					return true
				}
				for _, candidate := range account.Models {
					if candidate == model {
						return true
					}
				}
			}
			return false
		}
	}
	socketSession := ""
	if webSocketRequest {
		socketSession = config.RandomToken(16)
	}
	h.BeforeDispatchAccount = func(ctx context.Context, account config.LocalAccount, body []byte) (config.LocalAccount, func() bool, func(), int) {
		return s.dispatchWebSocketAccount(ctx, account, body, c, selected, validClient, socketSession)
	}
	h.BeforeDispatch = func(ctx context.Context, account config.LocalAccount, body []byte) (func(), int) {
		if imagePreAdmitted {
			return nil, 0
		}
		current := func() bool {
			_, available := source("")
			return validClient() && available
		}
		return s.admitImageTools(ctx, account, body, c, selected, current)
	}
	if textPoolClient && s.Pool != nil {
		h.Pool = s.Pool
		if !imagePreAdmitted {
			h.RetryAccount = func(ctx context.Context, body []byte, excluded []string) (config.LocalAccount, func() bool, func(), int) {
				if ctx.Err() != nil || !validClient() {
					return config.LocalAccount{}, nil, nil, 409
				}
				var input struct {
					Model string `json:"model"`
				}
				// Body already carries the model resolved under this unchanged client
				// policy. Resolving it again could reinterpret an upstream name as an alias.
				if json.Unmarshal(body, &input) != nil || !config.ValidCatalogModel(input.Model) {
					return config.LocalAccount{}, nil, nil, 403
				}
				account, current, release, status := s.selectNamedTextAccountExcept(c, selected, input.Model, namedTextSessionKey(r, body, socketSession), validClient, excluded)
				if messages {
					account.Kind, account.Target = "claude", "claude"
				}
				return account, current, release, status
			}
		}
	}
	if s.Observe != nil {
		h.BeginUsage = beginUsage
		h.Observe = func(o usageledger.Observation) {
			if o.AccountID == "" {
				o.AccountID = a.ID
				o.AccountEpoch = a.CredentialEpoch
			}
			o.ClientID, o.Route = selected.ID, "codex-api/"+observationPath
			if webSocketRequest && tracked.hijacked {
				// Gateway skips its upgrade envelope and invokes this only for
				// real inner HTTP turns. Preserve each turn's time and duration.
				s.Observe(o)
			} else {
				if o.FirstByteMS != nil {
					ms := o.StartedAt.Sub(started).Milliseconds() + *o.FirstByteMS
					o.FirstByteMS = &ms
				}
				o.StartedAt = started
				ledgerObservation = o
			}
		}
	}
	if s.Client != nil {
		h.Client = s.Client
	}
	inner := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/gateway/codex/" + path
	u.RawQuery, u.RawPath = "", ""
	if messages {
		u.Path = "/gateway/claude/" + path
	}
	inner.URL = &u
	inner.RequestURI = u.RequestURI()
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	inner.Header = r.Header.Clone()
	inner.Header.Set("Authorization", "Bearer "+key)
	if native != nil {
		inner.Header.Set("Content-Type", "application/json")
	}
	inner.Header.Del("x-api-key")
	inner.Header.Del("x-goog-api-key")
	// The outer service already authenticated the real socket and optional LAN
	// listener epoch. The internal gateway remains loopback-only for every other
	// caller and receives no generic remote bypass.
	inner.RemoteAddr = "127.0.0.1:0"
	if native != nil {
		adapter := newNativeResponseWriter(tracked, native)
		h.ServeHTTP(adapter, inner)
		adapter.Finish()
		if adapter.failed {
			ledgerObservation.State = "failed"
			ledgerObservation.UsageFinal = false
		}
	} else {
		h.ServeHTTP(tracked, inner)
	}
}

// Preserve streaming; the gateway checks this interface before flushing SSE.
type statusWriter struct {
	http.ResponseWriter
	status                       *int
	wrote, writeFailed, hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		*w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		*w.status = 200
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		w.writeFailed = true
	}
	return n, err
}
func (w *statusWriter) Flush() {
	if !w.wrote {
		*w.status = 200
		w.wrote = true
	}
	if http.NewResponseController(w.ResponseWriter).Flush() != nil {
		w.writeFailed = true
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		*w.status = http.StatusSwitchingProtocols
		w.wrote, w.hijacked = true, true
	}
	return conn, rw, err
}
