package apiservice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"salcara/bridge/internal/usageledger"
)

const syntheticImageResult = `{"created":100,"data":[{"b64_json":"synthetic-image-result"}],"usage":{"input_tokens":7,"output_tokens":12,"total_tokens":19}}`

func callImage(s *Service, key, path, body, kind string) *httptest.ResponseRecorder {
	r := apiRequest(context.Background(), key, strings.NewReader(body))
	r.URL.Path = "/codex-api/" + path
	r.Header.Set("Content-Type", kind)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func imageService(t *testing.T) (*Service, *liveConfig) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) { c.CodexAPI.ImagesMode = "enabled"; c.CodexAPI.MaxConcurrentImageRequests = 1 })
	return s, live
}

func TestImageAPIUsesPinnedSourceNamespaceAliasAndGenuineUsage(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].ModelPrefix = "team/"
		c.CodexAPI.Clients[0].ModelAliases = map[string]string{"picture": "gpt-image-2"}
		c.CodexAPI.Clients[0].ModelAllowlist = []string{"gpt-image-2"}
	})
	var calls atomic.Int32
	var observed []usageledger.Observation
	s.Observe = func(o usageledger.Observation) { observed = append(observed, o) }
	s.OfficialImages = func(_ context.Context, a config.LocalAccount, body []byte, stream bool, path string, current func() bool) (*http.Response, error) {
		calls.Add(1)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if a.CredentialRef != "credential-A" || path != "/v1/images/generations" || v["model"] != "gpt-image-2" || stream || !current() {
			t.Error("image source, path, model or authorization changed")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	w := callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"team/picture","prompt":"fixture","n":2}`, "application/json")
	if w.Code != 200 || calls.Load() != 1 || len(observed) != 1 || observed[0].State != "completed" || observed[0].Model != "gpt-image-2" || observed[0].Tokens.Total == nil || *observed[0].Tokens.Total != 19 {
		t.Fatalf("image result or ledger mismatch: status %d observed %+v", w.Code, observed)
	}
	for _, model := range []string{"picture", "wrong/picture", "team/team/picture", "team/gpt-denied"} {
		w = callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"`+model+`","prompt":"fixture"}`, "application/json")
		if w.Code != 403 {
			t.Fatalf("namespace/policy bypass for %s: %d", model, w.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("rejected image entered executor")
	}
}

func TestImageAPIDisabledAndAmbiguousRequestsNeverDispatch(t *testing.T) {
	s, live := imageService(t)
	var calls atomic.Int32
	s.OfficialImages = func(context.Context, config.LocalAccount, []byte, bool, string, func() bool) (*http.Response, error) {
		calls.Add(1)
		return nil, nil
	}
	for _, body := range []string{`{"model":"gpt-image-2","Model":"gpt-denied","prompt":"fixture"}`, `{"model":"gpt-image-2","model":"gpt-denied","prompt":"fixture"}`, `{"model":"gpt-image-2","prompt":"fixture","stream":null}`, `{"model":"gpt-image-2","prompt":"fixture","n":1.5}`, `{"model":"gpt-image-2","prompt":"fixture","images":[]}`} {
		if w := callImage(s, syntheticClientKey, "v1/images/generations", body, "application/json"); w.Code != 400 && w.Code != 501 {
			t.Fatalf("invalid image accepted: %d", w.Code)
		}
	}
	live.change(func(c *config.Config) { c.CodexAPI.ImagesMode = "disabled" })
	if w := callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"fixture"}`, "application/json"); w.Code != 503 {
		t.Fatalf("disabled images: %d", w.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid image dispatched")
	}
}

func TestImageAPIQueuedCredentialIsSharedAndRevocationFencesWaiter(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].MaxConcurrency = 4
		c.CodexAPI.Clients[1].AccountID = c.CodexAPI.Clients[0].AccountID
	})
	entered, unblock := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.OfficialImages = func(context.Context, config.LocalAccount, []byte, bool, string, func() bool) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-unblock
		}
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"fixture"}`, "application/json")
	}()
	<-entered
	go func() {
		second <- callImage(s, syntheticKeyB, "v1/images/generations", `{"model":"gpt-image-2","prompt":"fixture"}`, "application/json")
	}()
	waitImageQueue(t, &s.images, 1, 1)
	live.change(func(c *config.Config) { c.CodexAPI.Clients[1].Disabled = true })
	close(unblock)
	if (<-first).Code != 200 || (<-second).Code != 409 || calls.Load() != 1 {
		t.Fatal("queued revoked image dispatched or fixed source switched")
	}
}

func TestImageAPIRelayPreservesPathAndNeverFollowsCredentialRedirect(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/prefix/v1/images/generations" || r.Header.Get("Authorization") != "Bearer synthetic-relay-key" {
			t.Error("wrong relay image route or key")
		}
		w.Header().Set("Location", "/credential-trap")
		w.WriteHeader(307)
	}))
	defer upstream.Close()
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		a := &c.LocalAccounts[0]
		a.SourceType = "api_key"
		a.CredentialRef = ""
		a.BaseURL = upstream.URL + "/prefix/v1"
		a.Key = "synthetic-relay-key"
	})
	w := callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"fixture-image","prompt":"fixture"}`, "application/json")
	if w.Code != 502 || calls.Load() != 1 || strings.Contains(w.Body.String(), "synthetic-relay-key") {
		t.Fatal("relay redirect followed or leaked")
	}
}

func TestImageAPIStreamingNeedsActualCompletionAndSanitizesFailure(t *testing.T) {
	for _, tc := range []struct {
		name, raw, state string
		code             int
	}{
		{"completed", "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"fixture\",\"usage\":{\"total_tokens\":9}}\n\n", "completed", 200},
		{"empty200", "", "failed", 502},
		{"partial", "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"fixture\"}\n\n", "interrupted", 200},
		{"error", "data: {\"type\":\"error\",\"error\":{\"message\":\"synthetic-secret\"}}\n\n", "failed", 502},
		{"late_error", "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"fixture\"}\n\ndata: {\"type\":\"error\",\"error\":{\"message\":\"synthetic-secret\"}}\n\n", "failed", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := imageService(t)
			var o usageledger.Observation
			s.Observe = func(got usageledger.Observation) { o = got }
			s.OfficialImages = func(context.Context, config.LocalAccount, []byte, bool, string, func() bool) (*http.Response, error) {
				return successfulResponse(io.NopCloser(strings.NewReader(tc.raw)), true), nil
			}
			w := callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"fixture","stream":true}`, "application/json")
			if w.Code != tc.code || o.State != tc.state || strings.Contains(w.Body.String(), "synthetic-secret") {
				t.Fatalf("stream status/state: %d %+v", w.Code, o)
			}
		})
	}
}

func TestImageEditMultipartPreservesImagesMaskAndRejectsMixedFiles(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "fixture edit")
	_ = writer.WriteField("stream", "true")
	png := append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, make([]byte, 64)...)
	for _, field := range []string{"image[]", "mask"} {
		part, err := writer.CreateFormFile(field, "fixture.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(png)
	}
	_ = writer.Close()
	got, status := parseImageRequest(buf.Bytes(), writer.FormDataContentType(), "v1/images/edits", true)
	if status != 0 || !got.stream || !strings.Contains(string(got.body), "data:image/png;base64,") || !strings.Contains(string(got.body), "\"mask\"") {
		t.Fatal("multipart fields lost", status)
	}
	for _, raw := range []string{`{"model":"gpt-image-2","prompt":"fixture","images":[{"image_url":"file:///private/token"}]}`, `{"model":"gpt-image-2","prompt":"fixture","images":[{"image_url":"http://localhost/secret"}]}`, `{"model":"gpt-image-2","prompt":"fixture","images":[]}`} {
		if _, code := parseImageRequest([]byte(raw), "application/json", "v1/images/edits", true); code != 400 {
			t.Fatal("invalid edit reference accepted")
		}
	}
}

func TestResponsesImageToolsShareSwitchModelPolicyAndImageQueue(t *testing.T) {
	s, live := imageService(t)
	c := live.get()
	a := c.LocalAccounts[0]
	client := c.CodexAPI.Clients[0]
	request := []byte(`{"model":"gpt-fixture","input":"fixture","tools":[{"type":"image_generation"}]}`)
	disabled := c.Clone()
	disabled.CodexAPI.ImagesMode = "disabled"
	if _, status := s.admitImageTools(context.Background(), a, request, disabled, client, func() bool { return true }); status != 503 {
		t.Fatal("Responses bypassed disabled image switch", status)
	}
	denied := client
	denied.ModelDenylist = []string{"gpt-image-2"}
	if _, status := s.admitImageTools(context.Background(), a, request, c, denied, func() bool { return true }); status != 403 {
		t.Fatal("default image model bypassed client policy", status)
	}
	release, status := s.admitImageTools(context.Background(), a, request, c, client, func() bool { return true })
	if status != 0 {
		t.Fatal(status)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan int, 1)
	go func() {
		_, status := s.admitImageTools(ctx, a, request, c, client, func() bool { return true })
		waiting <- status
	}()
	waitImageQueue(t, &s.images, 1, 1)
	cancel()
	if <-waiting != 408 {
		t.Fatal("image tool queue did not cancel")
	}
	if release, status := s.admitImageTools(context.Background(), a, []byte(`{"model":"gpt-fixture","tools":[{"type":"function","name":"image_generation"}]}`), disabled, client, func() bool { return true }); status != 0 || release != nil {
		t.Fatal("ordinary function confused with built-in image tool")
	}
}

func TestResponsesImageToolsRejectNestedAuthorizationAmbiguity(t *testing.T) {
	s, live := imageService(t)
	c := live.get()
	a := c.LocalAccounts[0]
	client := c.CodexAPI.Clients[0]
	for _, raw := range []string{`{"tools":[{"Type":"image_generation"}]}`, `{"tools":[{"type":"image_generation","Model":"gpt-denied"}]}`, `{"tools":[null]}`, `{"tools":[{"type":null}]}`, `{"tool_choice":{"Type":"image_generation"}}`} {
		if _, status := s.admitImageTools(context.Background(), a, []byte(raw), c, client, func() bool { return true }); status != 400 {
			t.Fatalf("ambiguous tools accepted %d", status)
		}
	}
}

func TestImageBudgetNormalizesDuplicateRelayEndpointsAndKeepsSourceEpoch(t *testing.T) {
	a := config.LocalAccount{SourceType: "api_key", Key: "synthetic-key", BaseURL: "https://PROVIDER.example:443/mount/v1/"}
	b := a
	b.BaseURL = "https://provider.example/mount"
	first, ok := imageBudgetKey(a)
	second, other := imageBudgetKey(b)
	if !ok || !other || first != second {
		t.Fatal("endpoint spelling multiplied image concurrency")
	}
	b.BaseURL = "https://provider.example/other"
	third, _ := imageBudgetKey(b)
	if third == first {
		t.Fatal("different API tenant merged")
	}
	a = config.LocalAccount{SourceType: "official_account", CredentialRef: "fixture-ref", CredentialEpoch: 1}
	b = a
	b.CredentialEpoch = 2
	first, _ = imageBudgetKey(a)
	second, _ = imageBudgetKey(b)
	if first == second {
		t.Fatal("replacement image identity inherited old queue")
	}
}

func TestImagesOnlyServiceRejectsTextRoutesAndKeepsDiscovery(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) { c.CodexAPI.ImagesMode = "images_only" })
	if w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`); w.Code != 503 {
		t.Fatal("images-only service still executes text", w.Code)
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-image-2"}}, nil
	}
	r := apiRequest(context.Background(), syntheticClientKey, nil)
	r.Method = http.MethodGet
	r.URL.Path = "/codex-api/v1/models"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("images-only model discovery unavailable", w.Code)
	}
}
