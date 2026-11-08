package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func directSideQuestionConfig(t *testing.T, family string, explicit, pool bool) config.Config {
	t.Helper()
	cfg := directWorkerConfig(t)
	cfg.RemoteExecutionArmed, cfg.RemoteStationAuthorization = false, nil
	for i, source := range []string{"local", "hub", "direct"} {
		cfg.LocalAccounts[i].Model = source + "-model"
		cfg.LocalAccounts[i].Models = []string{source + "-model", source + "-alternate"}
		cfg.LocalAccounts[i].ModelAllowlist = []string{source + "-model", source + "-alternate"}
		cfg.LocalAccounts[i].PoolEnabled = pool
	}
	cfg.RecordAppliedAPI(family, cfg.APIForTool(cfg.LocalAccounts[0], family), "openai")
	if !explicit {
		delete(cfg.DirectAPI, family)
	}
	return cfg
}

func installDirectCodexSideConfig(f *codexSideFixture, cfg config.Config) {
	f.cfg, f.ctx = cfg, WithDirectConfigSnapshot(context.Background(), cfg)
	settings := DirectSettingsWithGateway(cfg, f.port)
	settings.LocalPort = f.port
	f.a.settings = func() Settings { return settings }
	f.a.mu.Lock()
	f.a.connSig = codexSig(settings)
	f.a.mu.Unlock()
}

func installDirectClaudeSideConfig(f *claudeSideFixture, cfg config.Config) {
	f.cfg, f.ctx = cfg, WithDirectConfigSnapshot(context.Background(), cfg)
	settings := DirectSettingsWithGateway(cfg, f.port)
	settings.LocalPort = f.port
	f.a.settings = func() Settings { return settings }
	f.p.workerSettingsSig = claudeSettingsSig(settings, "")
}

func TestDirectSideQuestionUsesSelectedRouteAndModelPolicy(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct-choice=%v", family, explicit), func(t *testing.T) {
				cfg := directSideQuestionConfig(t, family, explicit, false)
				target, source := family, "local"
				credential := cfg.LocalGatewayCredential(family)
				if explicit {
					target, source, credential = config.DirectGatewayTarget(family), "direct", cfg.DirectGatewayCredential(config.DirectGatewayTarget(family))
				}
				var calls atomic.Int32
				listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					endpoint := "/v1/responses"
					if family == "claude" {
						endpoint = "/v1/messages"
					}
					if r.URL.Path != "/gateway/"+target+endpoint || r.Header.Get("Authorization") != "Bearer "+credential {
						t.Error("side question used another control surface's gateway")
					}
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != source+"-alternate" {
						t.Error("side question ignored the selected source's model")
					}
					if family == "claude" {
						writeSideAnswer(w, "direct-answer")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, codexSideAnswer)
					}
				}))
				defer listener.Close()
				var ask func(protocol.SideQuestionRequest) (protocol.SideQuestionResult, error)
				var request protocol.SideQuestionRequest
				var ctx context.Context
				if family == "codex" {
					f := newCodexSideFixture(t, false, cfg.Clone(), listener.URL)
					installDirectCodexSideConfig(f, cfg)
					request, ctx = f.request(t, 1), f.ctx
					ask = func(r protocol.SideQuestionRequest) (protocol.SideQuestionResult, error) {
						return f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, r, f.validate)
					}
				} else {
					f := newClaudeSideFixtureAt(t, listener.URL)
					installDirectClaudeSideConfig(f, cfg)
					request, ctx = sideFixtureRequest(f.catalog(t)), f.ctx
					ask = func(r protocol.SideQuestionRequest) (protocol.SideQuestionResult, error) {
						return f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, r, f.guard)
					}
				}
				if validateSideQuestionModel(ctx, family, source+"-alternate") != nil || !errors.Is(validateSideQuestionModel(ctx, family, "hub-model"), errSideQuestionInvalid) {
					t.Fatal("direct model validation inherited Hub policy")
				}
				request.Model = source + "-alternate"
				result, err := ask(request)
				if err != nil || result.State != "completed" || calls.Load() != 1 {
					t.Fatalf("side question failed: state=%s calls=%d err=%v", result.State, calls.Load(), err)
				}
				request.OperationID, request.Model = newUUID(), "hub-model"
				if _, err := ask(request); !errors.Is(err, errSideQuestionInvalid) || calls.Load() != 1 {
					t.Fatal("foreign model policy reached inference")
				}
			})
		}
	}
}

func TestDirectSideQuestionKeepsLocalPoolRestrictionAndFailsClosed(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct-choice=%v", family, explicit), func(t *testing.T) {
				cfg := directSideQuestionConfig(t, family, explicit, true)
				var connect func(config.Config) (string, string, error)
				if family == "codex" {
					f := newCodexSideFixture(t, false, cfg.Clone(), "http://127.0.0.1:47831")
					connect = func(c config.Config) (string, string, error) {
						installDirectCodexSideConfig(f, c)
						x, reason, err := f.a.sideQuestionConnection(f.ctx, sessionActionThread, f.validate)
						return x.root, reason, err
					}
				} else {
					f := newClaudeSideFixtureAt(t, "http://127.0.0.1:47831")
					connect = func(c config.Config) (string, string, error) {
						installDirectClaudeSideConfig(f, c)
						x, reason, err := f.a.sideQuestionConnection(f.ctx, f.p.id, f.guard)
						return x.root, reason, err
					}
				}
				root, reason, err := connect(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if !explicit {
					if root != "" || !strings.Contains(reason, "账号池") {
						t.Fatal("follow-computer side question bypassed the local pool restriction")
					}
					return
				}
				if reason != "" || !strings.HasSuffix(root, "/gateway/"+config.DirectGatewayTarget(family)) {
					t.Fatal("fixed direct source was incorrectly treated as a local pool")
				}
				for _, mutation := range []func(*config.Config){
					func(c *config.Config) { c.DirectRelayEnabled = false },
					func(c *config.Config) {
						c.DirectAPI[family] = config.RemoteAPI{AccountID: "missing-account", Model: "direct-model"}
					},
					func(c *config.Config) { delete(c.GatewayAuthEpochs, config.DirectGatewayTarget(family)) },
				} {
					changed := cfg.Clone()
					mutation(&changed)
					root, reason, err = connect(changed)
					if root != "" || reason == "" && err == nil {
						t.Fatal("invalid explicit direct selection fell back to another source")
					}
				}
			})
		}
	}
}

func TestDirectSideQuestionHTTPRejectsRouteVariantsBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	for _, path := range []string{
		"/gateway/direct-codex/v1/messages", "/gateway/direct-claude/v1/responses",
		"/gateway/direct-codex-desktop/v1/responses", "/gateway/direct-remote-codex/v1/responses",
		"/gateway/direct-hermes/v1/chat/completions", "/gateway/direct-claude/v1/messages/",
		"/gateway/direct-codex/v1/responses?key=fixture", "/gateway/direct-codex/v1/responses#fragment",
		"/gateway/direct-claude/v1/%6dessages", "/gateway/direct-codex/../codex/v1/responses",
	} {
		_, attempted, err := sideQuestionGatewayRequest(context.Background(), server.URL+path, "synthetic-key", []byte(`{}`))
		if !errors.Is(err, errSideQuestionInvalid) || attempted {
			t.Fatalf("invalid direct route was dispatched: %s", path)
		}
	}
	for _, endpoint := range []string{
		strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/gateway/direct-codex/v1/responses",
		strings.Replace(server.URL, "http://", "http://user@", 1) + "/gateway/direct-claude/v1/messages",
	} {
		_, attempted, err := sideQuestionGatewayRequest(context.Background(), endpoint, "synthetic-key", []byte(`{}`))
		if !errors.Is(err, errSideQuestionInvalid) || attempted {
			t.Fatal("invalid direct authority was dispatched")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected direct routes reached the listener")
	}
}
