package console

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"salcara/bridge/internal/config"
)

func modelRulesRequest(s *Server, id string, allow, deny []string) map[string]any {
	c := s.d.Store.Get()
	a, _ := c.LocalAccount(id)
	return map[string]any{"id": id, "expectedSourceRevision": c.AccountSourceRevision(a), "modelAllowlist": allow, "modelDenylist": deny}
}

func TestLocalModelRulesNarrowUpdatePreservesAccountSources(t *testing.T) {
	for _, official := range []bool{false, true} {
		t.Run(map[bool]string{false: "API", true: "official"}[official], func(t *testing.T) {
			var s *Server
			var h http.Handler
			var id string
			if official {
				s, h = officialConsole(t)
				s.d.Store.SetRuntimeGatewayKey("synthetic-model-rules-runtime")
				ref := importOfficialConsole(t, s, h)
				for _, account := range s.d.Store.Get().LocalAccounts {
					if account.CredentialRef == ref {
						id = account.ID
					}
				}
			} else {
				s, h, id = sourceRevisionFixture(t)
			}
			if id == "" {
				t.Fatal("no linked fixture account")
			}
			updateSourceFixture(t, s, id, func(a *config.LocalAccount) {
				a.Models, a.ModelsManual, a.Model = []string{"model-a", "model-b"}, true, "model-a"
				a.PoolEnabled, a.PoolGroup, a.Priority, a.Weight, a.MaxConcurrency = true, "private-pool", 7, 3, 2
				a.CreatedAt, a.LastUsedAt = 12345, 67890
			})
			before, _ := s.d.Store.Get().LocalAccount(id)
			body := modelRulesRequest(s, id, []string{" model-a ", "model-b", "model-a", ""}, []string{"model-b"})
			w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", body)
			if w.Code != http.StatusOK {
				t.Fatalf("save returned %d: %s", w.Code, w.Body)
			}
			afterConfig := s.d.Store.Get()
			after, _ := afterConfig.LocalAccount(id)
			want := before
			want.ModelAllowlist, want.ModelDenylist = []string{"model-a", "model-b"}, []string{"model-b"}
			want.Models, want.ModelsManual, want.Model = nil, false, ""
			if !reflect.DeepEqual(after, want) {
				t.Fatal("model-policy update changed unrelated account fields")
			}
			if !config.ModelAllowed(after, "model-a") || config.ModelAllowed(after, "model-b") || config.ModelAllowed(after, "other") {
				t.Fatal("saved model policy did not retain deny precedence")
			}
			var out struct {
				OK                bool   `json:"ok"`
				ID                string `json:"id"`
				SourceRevision    string `json:"sourceRevision"`
				ModelsInvalidated bool   `json:"modelsInvalidated"`
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.OK || out.ID != id || !out.ModelsInvalidated || out.SourceRevision != afterConfig.AccountSourceRevision(after) || out.SourceRevision == body["expectedSourceRevision"] {
				t.Fatal("save did not return the new exact committed policy revision")
			}
			for _, secret := range []string{before.Key, afterConfig.GatewayRuntimeKey, config.AccountFingerprint(before), `"credentialRef"`, `"credentialEpoch"`, `"sourceType"`, `"key"`} {
				if secret != "" && strings.Contains(w.Body.String(), secret) {
					t.Fatal("save response leaked a private source field")
				}
			}
			assertNoOfficialSecrets(t, w.Body.String())
		})
	}
}

func TestLocalModelRulesInvalidatesOnlyAffectedCatalogAndSelections(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	other := createLocal(t, s, h, "api", "other", "synthetic-other-key", "https://other.example.test")
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ToolAPISelections = map[string]string{"codex": id, "claude": other}
		c.ToolModels = map[string]string{"codex": "model-a", "claude": "other-model"}
		c.ToolProtocols = map[string]string{"codex": "responses", "claude": "anthropic"}
		c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: id, Model: "model-a"}, "claude": {AccountID: other, Model: "other-model"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.modelProbeMu.Lock()
	s.modelProbeSeq = map[string]uint64{id: 5, other: 9}
	s.modelProbeMu.Unlock()
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", modelRulesRequest(s, id, []string{}, []string{"model-b"}))
	if w.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", w.Code, w.Body)
	}
	c := s.d.Store.Get()
	if c.ToolModels["codex"] != "" || c.ToolProtocols["codex"] != "" || c.RemoteAPI["codex"].Model != "" {
		t.Fatal("old model selections survived policy change")
	}
	if c.ToolModels["claude"] != "other-model" || c.ToolProtocols["claude"] != "anthropic" || c.RemoteAPI["claude"].Model != "other-model" || c.ToolAPISelections["codex"] != id {
		t.Fatal("policy change altered another source or account selection")
	}
	if _, exists := s.modelProbeSeq[id]; exists || s.modelProbeSeq[other] != 9 {
		t.Fatal("policy change did not retire only its own pending probe")
	}
}

func TestLocalModelRulesRejectsStaleOrMissingSourceRevision(t *testing.T) {
	changes := map[string]func(*Server, string, map[string]any){
		"missing": func(_ *Server, _ string, in map[string]any) { delete(in, "expectedSourceRevision") },
		"empty":   func(_ *Server, _ string, in map[string]any) { in["expectedSourceRevision"] = "" },
		"null":    func(_ *Server, _ string, in map[string]any) { in["expectedSourceRevision"] = nil },
		"key": func(s *Server, id string, _ map[string]any) {
			updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "synthetic-new-key" })
		},
		"policy": func(s *Server, id string, _ map[string]any) {
			updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.ModelDenylist = []string{"other-model"} })
		},
		"restart": func(s *Server, _ string, _ map[string]any) {
			s.d.Store.SetRuntimeGatewayKey("synthetic-restarted-runtime")
		},
		"unarmed": func(s *Server, _ string, _ map[string]any) { s.d.Store.SetRuntimeGatewayKey("") },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, h, id := sourceRevisionFixture(t)
			body := modelRulesRequest(s, id, []string{"wanted"}, []string{})
			change(s, id, body)
			before := s.d.Store.Get()
			w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", body)
			if w.Code != http.StatusConflict || !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatalf("stale write was not rejected atomically: %d", w.Code)
			}
		})
	}
}

func TestLocalModelRulesRejectsRotatedOfficialIdentity(t *testing.T) {
	s, h := officialConsole(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-official-rules-runtime")
	id := "official-" + importOfficialConsole(t, s, h)
	body := modelRulesRequest(s, id, []string{"model-a"}, []string{})
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.CredentialEpoch++ })
	before := s.d.Store.Get()
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", body)
	if w.Code != http.StatusConflict || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatalf("a replacement official identity inherited the stale rule editor: %d", w.Code)
	}
}

func TestLocalModelRulesRejectsNonPolicyFieldsAndInvalidRules(t *testing.T) {
	changes := map[string]func(map[string]any){
		"source type":   func(in map[string]any) { in["sourceType"] = "api_key" },
		"key":           func(in map[string]any) { in["key"] = "synthetic-replacement" },
		"reference":     func(in map[string]any) { in["credentialRef"] = "" },
		"pool":          func(in map[string]any) { in["poolEnabled"] = false },
		"missing allow": func(in map[string]any) { delete(in, "modelAllowlist") },
		"null deny":     func(in map[string]any) { in["modelDenylist"] = nil },
		"wrong type":    func(in map[string]any) { in["modelAllowlist"] = "model-a" },
		"too many":      func(in map[string]any) { in["modelAllowlist"] = make([]string, 1001) },
		"long model":    func(in map[string]any) { in["modelAllowlist"] = []string{strings.Repeat("a", 201)} },
		"long utf8":     func(in map[string]any) { in["modelAllowlist"] = []string{strings.Repeat("模", 67)} },
		"newline":       func(in map[string]any) { in["modelDenylist"] = []string{"model\nb"} },
		"nul":           func(in map[string]any) { in["modelDenylist"] = []string{"model\x00b"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, h, id := sourceRevisionFixture(t)
			body := modelRulesRequest(s, id, []string{"wanted"}, []string{})
			change(body)
			before := s.d.Store.Get()
			w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", body)
			if w.Code != http.StatusBadRequest || !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatalf("invalid narrow update changed state: %d", w.Code)
			}
		})
	}
}

func TestLocalModelRulesNoopKeepsModelsAndClearRemovesRules(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) {
		a.ModelAllowlist, a.ModelDenylist = []string{"model-a"}, []string{"model-b"}
		a.Model, a.Models, a.ModelsManual = "model-a", []string{"model-a"}, true
	})
	before := s.d.Store.Get()
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", modelRulesRequest(s, id, []string{" model-a ", "model-a"}, []string{"model-b"}))
	if w.Code != http.StatusOK || !reflect.DeepEqual(before, s.d.Store.Get()) || !strings.Contains(w.Body.String(), `"modelsInvalidated":false`) {
		t.Fatal("equivalent policy invalidated a usable catalog")
	}
	w = localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", modelRulesRequest(s, id, []string{}, []string{}))
	a, _ := s.d.Store.Get().LocalAccount(id)
	if w.Code != http.StatusOK || len(a.ModelAllowlist) != 0 || len(a.ModelDenylist) != 0 || !config.ModelAllowed(a, "any-model") {
		t.Fatal("explicit empty lists did not remove policy")
	}
}

func TestLocalModelRulesConcurrentEditorsHaveOneWinner(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	first := modelRulesRequest(s, id, []string{"first-model"}, []string{})
	second := modelRulesRequest(s, id, []string{"second-model"}, []string{})
	start := make(chan struct{})
	results := make(chan int, 2)
	var workers sync.WaitGroup
	for _, request := range []map[string]any{first, second} {
		workers.Add(1)
		go func(body map[string]any) {
			defer workers.Done()
			<-start
			results <- localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", body).Code
		}(request)
	}
	close(start)
	workers.Wait()
	close(results)
	counts := map[int]int{}
	for result := range results {
		counts[result]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("expected exactly one accepted editor: %v", counts)
	}
}

func TestLocalModelRulesRejectsTrailingBodyAndRemovedAccount(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	body := modelRulesRequest(s, id, []string{"model-a"}, []string{})
	raw, _ := json.Marshal(body)
	for _, suffix := range []string{"{}", strings.Repeat(" ", (1<<20)+1)} {
		w := do(h, http.MethodPost, "/api/local/accounts/model-rules", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://127.0.0.1:47831"}, string(raw)+suffix)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid trailing body accepted: %d", w.Code)
		}
	}
	if w := localRequest(s, h, http.MethodPost, "/api/local/accounts/delete", map[string]string{"id": id}); w.Code != http.StatusOK {
		t.Fatal("fixture delete failed")
	}
	if w := localRequest(s, h, http.MethodPost, "/api/local/accounts/model-rules", body); w.Code != http.StatusNotFound {
		t.Fatalf("removed account accepted: %d", w.Code)
	}
}
