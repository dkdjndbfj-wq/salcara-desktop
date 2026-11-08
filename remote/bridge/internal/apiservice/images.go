package apiservice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/usageledger"
)

const maxImageResponse = 128 << 20

func imageBudgetKey(a config.LocalAccount) (string, bool) {
	if a.SourceType == "oauth" {
		return "", false
	}
	return gateway.CredentialBudgetIdentity(a)
}

func resolveImageToolModels(body []byte, client config.NamedAPIClient) ([]byte, int) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil, 400
	}
	raw, exists := request["tools"]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return body, 0
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, 400
	}
	changed := false
	for _, tool := range tools {
		for key := range tool {
			if key != "type" && strings.EqualFold(key, "type") || key != "model" && strings.EqualFold(key, "model") {
				return nil, 400
			}
		}
		var kind string
		if !imageString(tool["type"], &kind) {
			return nil, 400
		}
		if kind != "image_generation" {
			continue
		}
		if raw, exists := tool["model"]; exists {
			var visible string
			if !imageString(raw, &visible) {
				return nil, 400
			}
			actual, allowed := config.ResolveAPIClientModel(client, visible)
			if !allowed {
				return nil, 403
			}
			tool["model"], _ = json.Marshal(actual)
			changed = true
		}
	}
	if !changed {
		return body, 0
	}
	request["tools"], _ = json.Marshal(tools)
	out, err := json.Marshal(request)
	if err != nil {
		return nil, 400
	}
	return out, 0
}

func resolvedImageToolModels(body []byte) ([]string, int) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil, 400
	}
	var tools []map[string]json.RawMessage
	if raw, ok := request["tools"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &tools) != nil {
			return nil, 400
		}
	}
	models := make([]string, 0, len(tools))
	for _, tool := range tools {
		for key := range tool {
			if key != "type" && strings.EqualFold(key, "type") || key != "model" && strings.EqualFold(key, "model") {
				return nil, 400
			}
		}
		var kind string
		if !imageString(tool["type"], &kind) || kind == "" {
			return nil, 400
		}
		if kind != "image_generation" {
			continue
		}
		model := "gpt-image-2"
		if raw, ok := tool["model"]; ok && !imageString(raw, &model) {
			return nil, 400
		}
		models = append(models, model)
	}
	var choice struct {
		Type string `json:"type"`
	}
	var choiceObject map[string]json.RawMessage
	_ = json.Unmarshal(request["tool_choice"], &choiceObject)
	for key := range choiceObject {
		if key != "type" && strings.EqualFold(key, "type") {
			return nil, 400
		}
	}
	_ = json.Unmarshal(request["tool_choice"], &choice)
	if choice.Type == "image_generation" && len(models) == 0 {
		return nil, 400
	}
	return models, 0
}

func imageModelSupportedByAccount(a config.LocalAccount, model string) bool {
	if a.SourceType != "official_account" {
		return true
	}
	switch model {
	case "gpt-image-1.5", "gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst":
		return true
	default:
		return false
	}
}

type imageAccountCandidate struct {
	account     config.LocalAccount
	fingerprint string
	budgetKey   string
}

func imageAccountIDs(client config.NamedAPIClient) []string {
	if len(client.ImageAccountIDs) > 0 {
		return client.ImageAccountIDs
	}
	return []string{client.AccountID}
}

func codexDispatchAccount(a config.LocalAccount) config.LocalAccount {
	a.Kind, a.Target, a.Protocol = "codex", "codex", "responses"
	if protocol := config.InferProtocol(a.Model, a.Wire); protocol != "" {
		a.Protocol = protocol
	}
	return a
}

func (s *Service) fixedImageClientAccountCurrent(client config.NamedAPIClient, account config.LocalAccount, clientCurrent func() bool) func() bool {
	fingerprint := config.AccountFingerprint(account)
	return func() bool {
		if !clientCurrent() || s.Config == nil {
			return false
		}
		latest, ok := s.Config().LocalAccount(account.ID)
		return ok && config.AccountFingerprint(latest) == fingerprint
	}
}

// dispatchWebSocketAccount applies the same image-only member policy to each
// real Responses WebSocket turn. Ordinary turns keep the client's fixed
// AccountID and acquire only its normal shared credential slot.
func (s *Service) dispatchWebSocketAccount(ctx context.Context, primary config.LocalAccount, body []byte, c config.Config, client config.NamedAPIClient, clientCurrent func() bool, socketSession string) (config.LocalAccount, func() bool, func(), int) {
	storedPrimary, ok := c.LocalAccount(primary.ID)
	if !ok {
		return config.LocalAccount{}, nil, nil, 409
	}
	models, status := resolvedImageToolModels(body)
	if status != 0 {
		return config.LocalAccount{}, nil, nil, status
	}
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) != nil || !config.ValidCatalogModel(request.Model) {
		return config.LocalAccount{}, nil, nil, 400
	}
	if len(models) == 0 {
		if namedTextPoolEnabled(client) {
			selected, current, release, status := s.selectNamedTextAccount(c, client, request.Model, namedTextSessionKey(nil, body, socketSession), clientCurrent)
			if status != 0 {
				return config.LocalAccount{}, nil, nil, status
			}
			return selected, current, release, 0
		}
		current := s.fixedImageClientAccountCurrent(client, storedPrimary, clientCurrent)
		if !current() {
			return config.LocalAccount{}, nil, nil, 409
		}
		release := func() {}
		if !current() {
			release()
			return config.LocalAccount{}, nil, nil, 409
		}
		return codexDispatchAccount(storedPrimary), current, release, 0
	}
	if !config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) || config.ValidateCodexAPIImages(c.CodexAPI.ImagesMode, c.CodexAPI.MaxConcurrentImageRequests) != nil {
		return config.LocalAccount{}, nil, nil, 503
	}
	for _, model := range models {
		if !config.APIClientModelAllowed(client, model) {
			return config.LocalAccount{}, nil, nil, 403
		}
	}
	allModels := append([]string{request.Model}, models...)
	preAdmittedKey, _ := imageBudgetKey(storedPrimary)
	selected, current, release, status := s.acquireImageAccount(ctx, c, client, allModels, clientCurrent, preAdmittedKey)
	if status != 0 {
		return config.LocalAccount{}, nil, nil, status
	}
	return codexDispatchAccount(selected), current, release, 0
}

// acquireImageAccount is the explicit image-only selector used by dedicated
// image endpoints and Responses image tools. It chooses only configured member
// IDs, deduplicates credential aliases, bypasses session affinity, and binds
// the returned current check to the selected member's exact fingerprint.
func (s *Service) acquireImageAccount(ctx context.Context, c config.Config, client config.NamedAPIClient, models []string, clientCurrent func() bool, preAdmittedKey string) (config.LocalAccount, func() bool, func(), int) {
	if !clientCurrent() {
		return config.LocalAccount{}, nil, nil, 409
	}
	byKey := make(map[string]imageAccountCandidate)
	keys := make([]string, 0, len(client.ImageAccountIDs)+1)
	present, policyEligible, supported := 0, 0, 0
	for _, id := range imageAccountIDs(client) {
		account, ok := c.LocalAccount(id)
		if !ok {
			continue
		}
		present++
		allowed := true
		for _, model := range models {
			if !config.ModelAllowed(account, model) {
				allowed = false
				break
			}
		}
		if !allowed {
			continue
		}
		policyEligible++
		imageModelsSupported := true
		for _, model := range models {
			if strings.HasPrefix(model, "gpt-image-") && !imageModelSupportedByAccount(account, model) {
				imageModelsSupported = false
				break
			}
		}
		if !imageModelsSupported {
			continue
		}
		supported++
		key, usable := imageBudgetKey(account)
		if !usable {
			continue
		}
		if _, duplicate := byKey[key]; duplicate {
			continue
		}
		byKey[key] = imageAccountCandidate{account: account, fingerprint: config.AccountFingerprint(account), budgetKey: key}
		keys = append(keys, key)
	}
	if present == 0 {
		return config.LocalAccount{}, nil, nil, 409
	}
	if policyEligible == 0 {
		return config.LocalAccount{}, nil, nil, 403
	}
	if supported == 0 {
		return config.LocalAccount{}, nil, nil, 501
	}
	if len(keys) == 0 {
		return config.LocalAccount{}, nil, nil, 503
	}

	for len(keys) > 0 {
		key, imageRelease, err := s.images.acquireAny(ctx, keys, c.CodexAPI.MaxConcurrentImageRequests)
		if err != nil {
			if ctx.Err() != nil {
				return config.LocalAccount{}, nil, nil, 408
			}
			return config.LocalAccount{}, nil, nil, 429
		}
		candidate := byKey[key]
		memberCurrent := func() bool {
			if !clientCurrent() || s.Config == nil {
				return false
			}
			latest := s.Config()
			nextClient, ok := latest.APIClient(client.ID)
			// AuthEpoch is the Store's change token for this client — comparing it
			// is faster and more correct than reflect.DeepEqual on the full struct.
			if !ok || nextClient.AuthEpoch != client.AuthEpoch || nextClient.ID != client.ID {
				return false
			}
			next, ok := latest.LocalAccount(candidate.account.ID)
			return ok && config.AccountFingerprint(next) == candidate.fingerprint
		}
		if !memberCurrent() {
			imageRelease()
			return config.LocalAccount{}, nil, nil, 409
		}
		poolRelease := func() {}
		if s.Pool != nil {
			if key == preAdmittedKey {
				err = s.Pool.ValidateFixedModels(candidate.account, models...)
			} else {
				poolRelease, err = s.Pool.AdmitFixedModel(candidate.account, c.LocalAccounts, models...)
			}
			if err != nil {
				imageRelease()
				remaining := keys[:0]
				for _, candidateKey := range keys {
					if candidateKey != key {
						remaining = append(remaining, candidateKey)
					}
				}
				keys = remaining
				continue
			}
		}
		if !memberCurrent() {
			poolRelease()
			imageRelease()
			return config.LocalAccount{}, nil, nil, 409
		}
		var once sync.Once
		release := func() { once.Do(func() { poolRelease(); imageRelease() }) }
		return candidate.account, memberCurrent, release, 0
	}
	return config.LocalAccount{}, nil, nil, 429
}

// Explicit image tools on Responses share the same switch, image-model ACL
// and queue as dedicated image endpoints, including real WebSocket turns.
// A caller-defined function named image_generation is still a function tool.
func (s *Service) admitImageTools(ctx context.Context, a config.LocalAccount, body []byte, c config.Config, client config.NamedAPIClient, current func() bool) (func(), int) {
	models, status := resolvedImageToolModels(body)
	if status != 0 {
		return nil, status
	}
	for _, model := range models {
		if !config.APIClientModelAllowed(client, model) || !config.ModelAllowed(a, model) {
			return nil, 403
		}
		if !imageModelSupportedByAccount(a, model) {
			return nil, 501
		}
	}
	if len(models) == 0 {
		return nil, 0
	}
	if !config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) || config.ValidateCodexAPIImages(c.CodexAPI.ImagesMode, c.CodexAPI.MaxConcurrentImageRequests) != nil {
		return nil, 503
	}
	if !current() {
		return nil, 409
	}
	key, ok := imageBudgetKey(a)
	if !ok {
		return nil, 503
	}
	release, err := s.images.acquire(ctx, key, c.CodexAPI.MaxConcurrentImageRequests)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 408
		}
		return nil, 429
	}
	if !current() {
		release()
		return nil, 409
	}
	return release, 0
}

func (s *Service) serveImages(w *statusWriter, r *http.Request, path string, client config.NamedAPIClient, c config.Config, clientCurrent func() bool, observation *usageledger.Observation, beginUsage func(string, uint64) func()) {
	if !config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) || config.ValidateCodexAPIImages(c.CodexAPI.ImagesMode, c.CodexAPI.MaxConcurrentImageRequests) != nil {
		fail(w, 503)
		return
	}
	body, status := s.readRequestBody(w, r)
	if status != 0 {
		fail(w, status)
		return
	}
	input, status := parseImageRequest(body, r.Header.Get("Content-Type"), path, false)
	if status != 0 {
		fail(w, status)
		return
	}
	actual, allowed := config.ResolveAPIClientModel(client, input.model)
	if !allowed {
		fail(w, 403)
		return
	}
	var object map[string]json.RawMessage
	_ = json.Unmarshal(input.body, &object)
	object["model"], _ = json.Marshal(actual)
	input.body, _ = json.Marshal(object)
	input.model = actual
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	a, current, release, status := s.acquireImageAccount(ctx, c, client, []string{actual}, clientCurrent, "")
	if status != 0 {
		fail(w, status)
		return
	}
	defer release()
	observation.AccountID = a.ID
	observation.AccountEpoch = a.CredentialEpoch
	if beginUsage != nil {
		defer beginUsage(a.ID, a.CredentialEpoch)()
	}
	official := a.CredentialRef != "" || a.SourceType == "official_account"
	if a.SourceType == "oauth" {
		fail(w, 501)
		return
	}
	if official {
		if s.OfficialImages == nil || a.CredentialRef == "" {
			fail(w, 503)
			return
		}
		// Validate the resolved image model against the actual pinned adapter.
		if _, status = parseImageRequest(input.body, "application/json", path, true); status != 0 {
			fail(w, status)
			return
		}
	}
	if !current() {
		fail(w, 409)
		return
	}
	observation.Model, observation.State = actual, "unknown"
	var response *http.Response
	var err error
	if official {
		response, err = s.OfficialImages(ctx, a, input.body, input.stream, "/"+path, current)
	} else {
		_, base, e := config.APIBase(a.BaseURL)
		if e != nil {
			fail(w, 409)
			return
		}
		upstream, e := http.NewRequestWithContext(ctx, http.MethodPost, base+strings.TrimPrefix("/"+path, "/v1"), bytes.NewReader(input.body))
		if e != nil {
			fail(w, 400)
			return
		}
		upstream.Header.Set("Content-Type", "application/json")
		if a.AuthMode == "api-key" {
			upstream.Header.Set("x-api-key", a.Key)
		} else {
			upstream.Header.Set("Authorization", "Bearer "+a.Key)
		}
		if input.stream {
			upstream.Header.Set("Accept", "text/event-stream")
		}
		if s.Client == nil {
			fail(w, 503)
			return
		}
		transport := *s.Client
		transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if !current() {
			fail(w, 409)
			return
		}
		response, err = transport.Do(upstream)
	}
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		observation.State = "failed"
		if !gateway.WriteExecutorError(w, err) {
			fail(w, 502)
		}
		return
	}
	defer response.Body.Close()
	observation.UpstreamHTTPStatus = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		observation.State = "failed"
		raw, _ := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		gateway.WriteProviderError(w, response.StatusCode, raw)
		return
	}
	// Once output begins, accepted requests drain their fixed source. Before
	// first output, revoke/rotate/replace must still fence the result.
	if !current() {
		fail(w, 409)
		return
	}
	var collector usageledger.Collector
	defer func() {
		observation.Tokens, observation.UsageFinal = collector.Tokens(), collector.Final()
		observation.ProviderCost, observation.ServiceTier = collector.ProviderCost(), collector.ServiceTier()
		if observation.State == "unknown" {
			observation.State = collector.State()
		}
	}()
	if input.stream {
		if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
			observation.State = "failed"
			fail(w, 502)
			return
		}
		s.copyImageStream(w, r, response.Body, &collector, observation, current)
		return
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxImageResponse+1))
	if readErr != nil || len(raw) > maxImageResponse || !imageUniqueJSON(raw) {
		observation.State = "failed"
		fail(w, 502)
		return
	}
	collector.ObserveImageJSON(raw)
	if collector.State() != "completed" {
		observation.State = "failed"
		fail(w, 502)
		return
	}
	if !current() {
		fail(w, 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *Service) copyImageStream(w *statusWriter, r *http.Request, reader io.Reader, collector *usageledger.Collector, observation *usageledger.Observation, current func() bool) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 32<<10), maxImageResponse)
	var frame []byte
	written, terminal, broken := false, false, false
	var total int64
	flush := func() bool {
		if len(frame) == 0 {
			return true
		}
		var data [][]byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = append(data, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
			}
		}
		raw := bytes.Join(data, []byte("\n"))
		if len(raw) == 0 {
			frame = nil
			return true
		}
		if bytes.Equal(raw, []byte("[DONE]")) {
			frame = nil
			return terminal
		}
		if !imageUniqueJSON(raw) {
			return false
		}
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &event)
		collector.ObserveImageJSON(raw)
		if event.Type != "image_generation.partial_image" && event.Type != "image_generation.completed" && event.Type != "image_edit.partial_image" && event.Type != "image_edit.completed" {
			return false
		}
		if collector.State() == "failed" {
			return false
		}
		if !written {
			if !current() {
				fail(w, 409)
				return false
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			written = true
		}
		if _, err := w.Write(append(frame, '\n')); err != nil {
			return false
		}
		w.Flush()
		terminal = terminal || collector.State() == "completed"
		frame = nil
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		total += int64(len(line) + 1)
		if len(frame)+len(line)+1 > maxImageResponse || total > 512<<20 {
			broken = true
			break
		}
		if len(line) == 0 {
			if !flush() {
				broken = true
				break
			}
			continue
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
	}
	clean := !broken && scanner.Err() == nil && len(frame) == 0 && terminal && collector.State() == "completed"
	if clean {
		return
	}
	observation.State = "interrupted"
	if collector.State() == "failed" {
		observation.State = "failed"
	}
	if r.Context().Err() != nil {
		observation.State = "cancelled"
	}
	if !written {
		observation.State = "failed"
		if !w.wrote {
			fail(w, 502)
		}
		return
	}
	_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"salcara_local_api_error\",\"message\":\"Image stream interrupted\"}}\n\n"))
	w.Flush()
}
