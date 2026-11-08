package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func accountCoexistServer(t *testing.T, upstream, mode string, limit int) (*Server, http.Handler, string, config.Config) {
	t.Helper()
	s, h := newTestServer(t)
	id := testChatAccount(t, s, h, upstream)
	s.d.Store.SetRuntimeGatewayKey("synthetic-coexist-runtime")
	if err := s.d.Store.Update(func(c *config.Config) error {
		a := &c.LocalAccounts[0]
		a.MaxConcurrency = limit
		a.Models = []string{"coding-model", "gpt-desktop", "deepseek-desktop", "gpt-phone", "deepseek-phone", "gpt-test", "gpt-test-again"}
		c.ToolModels = map[string]string{"codex": "gpt-desktop", "deepseek-harness": "deepseek-desktop"}
		for target, model := range c.ToolModels {
			applied := c.APIForTool(*a, target)
			applied.Model, applied.Protocol = model, "chat"
			c.RecordAppliedAPI(target, applied, "synthetic-coexist")
		}
		c.ActivateRemoteMode(mode)
		choices := map[string]config.RemoteAPI{"codex": {AccountID: id, Model: "gpt-phone"}, "deepseek-harness": {AccountID: id, Model: "deepseek-phone"}}
		if mode == "direct" {
			c.DirectPhoneID, c.DirectPhoneHash = "synthetic-phone", "synthetic-phone-hash"
			c.DirectAPI = choices
		} else {
			c.HubURL, c.DeviceID, c.DeviceSecret = "https://fixture-hub.invalid/salcara-hub", "synthetic-device", "synthetic-device-secret"
			c.PhoneBindingID, c.PhoneHash = "synthetic-binding", "synthetic-phone-hash"
			c.RemoteAPI = choices
			a.RemoteHubBindingsSet, a.RemoteHubURLs = true, []string{c.HubURL}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if mode != "direct" {
		if err := s.d.Store.BindRemoteStation(config.RemoteIdentity(s.d.Store.Get()), "synthetic-station", &protocol.StationPolicy{Version: 1, AllowAllAccounts: true}, nil); err != nil {
			t.Fatal(err)
		}
	}
	return s, h, id, s.d.Store.Get()
}

func accountCoexistRequest(ctx context.Context, path, key string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(raw))).WithContext(ctx)
	r.Host, r.RemoteAddr = "127.0.0.1:47831", "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

func runAccountCoexistRequest(h http.Handler, r *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); done <- w }()
	return done
}

func awaitAccountCoexistResponse(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
		return nil
	}
}

func TestAccountPlaygroundCoexistsWithCodexDeepSeekAndPhone(t *testing.T) {
	for _, mode := range []string{"hub", "direct"} {
		t.Run(mode, func(t *testing.T) {
			models := []string{"gpt-desktop", "deepseek-desktop", "gpt-phone", "deepseek-phone", "gpt-test", "gpt-test-again"}
			gates, tokenCounts := map[string]chan struct{}{}, map[string]int{}
			for i, model := range models {
				gates[model], tokenCounts[model] = make(chan struct{}), 10+i
			}
			started, stopped := make(chan string, 12), make(chan string, 12)
			cleanup := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(raw, &body)
				if gates[body.Model] == nil || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer private-test-key" {
					t.Error("wrong upstream, model or credential")
					w.WriteHeader(400)
					return
				}
				for _, model := range models {
					if strings.Contains(string(raw), "context/"+model+"/") != (model == body.Model) {
						t.Error("conversation context crossed requests")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"reply-%s\",\"object\":\"chat.completion.chunk\",\"model\":\"%s\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"reply/%s/\"}}]}\n\n", body.Model, body.Model, body.Model)
				w.(http.Flusher).Flush()
				started <- body.Model
				select {
				case <-gates[body.Model]:
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"reply-%s\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\ndata: [DONE]\n\n", body.Model, tokenCounts[body.Model]-3, tokenCounts[body.Model])
				case <-r.Context().Done():
					stopped <- body.Model
				case <-cleanup:
				}
			}))
			defer upstream.Close()
			defer close(cleanup)
			s, h, id, before := accountCoexistServer(t, upstream.URL, mode, 5)
			revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
			prefix := "remote-"
			remoteKey := before.RemoteGatewayCredential
			if mode == "direct" {
				prefix, remoteKey = "direct-", before.DirectGatewayCredential
			}
			credentialOwners := map[string]string{}
			for _, target := range []string{"codex", "deepseek-harness", prefix + "codex", prefix + "deepseek-harness"} {
				key := before.LocalGatewayCredential(target)
				if strings.HasPrefix(target, prefix) {
					key = remoteKey(target)
				}
				if key == "" {
					t.Fatal("missing route credential", target)
				}
				if credentialOwners[key] != "" {
					t.Fatal("tools shared a loopback credential", target)
				}
				credentialOwners[key] = target
				wrong := accountCoexistRequest(context.Background(), "/gateway/"+target+"/v1/models", "another-route-credential", nil)
				wrong.Method = "GET"
				if target != "codex" {
					wrong.Header.Set("Authorization", "Bearer "+before.LocalGatewayCredential("codex"))
				}
				if w := awaitAccountCoexistResponse(t, runAccountCoexistRequest(h, wrong)); w.Code != 401 {
					t.Fatal("another tool's credential authorized this route", target, w.Code)
				}
			}
			workers := map[string]<-chan *httptest.ResponseRecorder{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for i, target := range []string{"codex", "deepseek-harness", prefix + "codex", prefix + "deepseek-harness"} {
				model, key := models[i], before.LocalGatewayCredential(target)
				if strings.HasPrefix(target, prefix) {
					key = remoteKey(target)
				}
				path := "/gateway/" + target + "/v1/chat/completions"
				body := map[string]any{"model": model, "stream": true, "messages": []accountTestMessage{{Role: "user", Content: "context/" + model + "/"}}}
				if strings.HasSuffix(target, "codex") {
					path = "/gateway/" + target + "/v1/responses"
					delete(body, "messages")
					body["input"] = "context/" + model + "/"
				}
				workers[model] = runAccountCoexistRequest(h, accountCoexistRequest(ctx, path, key, body))
			}
			testRequest := func(testCtx context.Context, model string) *http.Request {
				body := accountTestInput(id, revision)
				body["model"], body["messages"] = model, []accountTestMessage{{Role: "user", Content: "context/" + model + "/"}}
				r := accountCoexistRequest(testCtx, "/api/local/accounts/test/send", "", body)
				r.Header.Set("Cookie", CookieName+"="+s.Token())
				return r
			}
			testCtx, stopTest := context.WithCancel(ctx)
			defer stopTest()
			testDone := runAccountCoexistRequest(h, testRequest(testCtx, "gpt-test"))
			seen := map[string]bool{}
			for i := 0; i < 5; i++ {
				select {
				case model := <-started:
					seen[model] = true
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent route did not reach upstream", seen)
				}
			}
			if len(seen) != 5 {
				t.Fatal("one request was replayed or another was lost")
			}
			full := accountCoexistRequest(ctx, "/gateway/deepseek-harness/v1/chat/completions", before.LocalGatewayCredential("deepseek-harness"), map[string]any{"model": "deepseek-desktop", "stream": true, "messages": []accountTestMessage{{Role: "user", Content: "must not dispatch"}}})
			if w := awaitAccountCoexistResponse(t, runAccountCoexistRequest(h, full)); w.Code != 409 {
				t.Fatal("combined credential capacity was bypassed", w.Code)
			}
			stopTest()
			awaitAccountCoexistResponse(t, testDone)
			select {
			case model := <-stopped:
				if model != "gpt-test" {
					t.Fatal("test cancellation stopped another request", model)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("test upstream did not cancel")
			}
			for model, done := range workers {
				select {
				case w := <-done:
					t.Fatalf("test cancellation ended %s: %d %s", model, w.Code, w.Body)
				default:
				}
			}
			again := runAccountCoexistRequest(h, testRequest(ctx, "gpt-test-again"))
			select {
			case model := <-started:
				if model != "gpt-test-again" {
					t.Fatal("unexpected replacement request", model)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stopped test did not release its capacity")
			}
			for _, model := range models {
				if model != "gpt-test" {
					close(gates[model])
				}
			}
			workers["gpt-test-again"] = again
			for model, done := range workers {
				w := awaitAccountCoexistResponse(t, done)
				if w.Code != 200 {
					t.Fatalf("request failed: %s %d %s", model, w.Code, w.Body)
				}
				for _, other := range models {
					if strings.Contains(w.Body.String(), "reply/"+other+"/") != (other == model) {
						t.Fatal("reply crossed request streams", model, other)
					}
				}
			}
			after := s.d.Store.Get()
			if !reflect.DeepEqual(before.ToolAPIApplied, after.ToolAPIApplied) || !reflect.DeepEqual(before.ToolModels, after.ToolModels) || !reflect.DeepEqual(before.RemoteAPI, after.RemoteAPI) || !reflect.DeepEqual(before.DirectAPI, after.DirectAPI) || !reflect.DeepEqual(before.GatewayAuthEpochs, after.GatewayAuthEpochs) {
				t.Fatal("test rewrote another tool or phone configuration")
			}
			flushConsoleLedger(t, s)
			ledger, err := s.usageLedger.Snapshot(100, "")
			if err != nil {
				t.Fatal(err)
			}
			seenRecords := map[string]bool{}
			for _, row := range ledger.Records {
				if row.AccountID != s.usageLedger.AnonymousAccountID(id) {
					continue
				}
				if seenRecords[row.Model] {
					t.Fatal("duplicated usage record", row.Model)
				}
				seenRecords[row.Model] = true
				if row.Model == "gpt-test" {
					if row.State != "cancelled" {
						t.Fatal("cancelled test recorded as completed", row.State)
					}
				} else if row.State != "completed" || row.Tokens.Total == nil || *row.Tokens.Total != int64(tokenCounts[row.Model]) {
					t.Fatal("usage or status crossed request boundaries", row.Model, row.State)
				}
			}
			if len(seenRecords) != 6 {
				t.Fatal("lost per-request usage records", seenRecords)
			}
		})
	}
}

func TestAccountPlaygroundFailureDoesNotCoolProductionCredential(t *testing.T) {
	for _, status := range []int{400, 401, 404, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"synthetic diagnostic failure"}}`)
			}))
			defer upstream.Close()
			s, h, id, cfg := accountCoexistServer(t, upstream.URL, "hub", 5)
			revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
			w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision))
			if w.Code != status {
				t.Fatal("test should report the provider's error", w.Code)
			}
			for _, target := range []string{"codex", "deepseek-harness", "remote-codex", "remote-deepseek-harness"} {
				a, ok := cfg.GatewaySourceAccount(target)
				if !ok {
					t.Fatal("missing production source", target)
				}
				release, err := s.accountPool.AdmitFixedModel(a, cfg.LocalAccounts, a.Model)
				if err != nil {
					t.Fatalf("failed diagnostic cooled unrelated %s work: %v", target, err)
				}
				release()
			}
			// Diagnostics cannot bypass a real cooldown established by production.
			a, _ := cfg.GatewaySourceAccount("codex")
			s.accountPool.MarkFailureFor(a.ID, config.AccountFingerprint(a), time.Minute)
			if w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision)); w.Code != 429 {
				t.Fatal("diagnostic bypassed an existing production cooldown", w.Code)
			}
		})
	}
}
