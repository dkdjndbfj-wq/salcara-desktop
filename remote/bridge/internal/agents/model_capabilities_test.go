package agents

import (
	"encoding/json"
	"reflect"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexModelCatalogOnlyUsesExplicitPerModelMetadata(t *testing.T) {
	var page codexModelPage
	if err := json.Unmarshal([]byte(`{"data":[
	{"model":"same-family-one","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"},{"reasoningEffort":"ultra"},{"reasoningEffort":"high"},{"reasoningEffort":"imagined"}],"inputModalities":["text","image","invented"]},
	{"model":"same-family-two","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]},
	{"model":"gpt-name-is-not-evidence"},
	{"model":"explicit-no-effort","supportedReasoningEfforts":[]},
	{"model":"hidden","hidden":true,"supportedReasoningEfforts":[{"reasoningEffort":"low"}]}
	]}`), &page); err != nil {
		t.Fatal(err)
	}
	out := protocol.ModelCatalog{ModelCapabilities: map[string]protocol.ModelCapability{}}
	appendCodexModelPage(&out, page)
	if len(out.Models) != 4 {
		t.Fatal(out)
	}
	if cap := out.ModelCapabilities["same-family-one"]; !cap.ReasoningKnown || !reflect.DeepEqual(cap.ReasoningEfforts, []string{"low", "high", "ultra"}) || !reflect.DeepEqual(cap.InputModalities, []string{"text", "image"}) {
		t.Fatal(cap)
	}
	if cap := out.ModelCapabilities["same-family-two"]; !reflect.DeepEqual(cap.ReasoningEfforts, []string{"medium"}) {
		t.Fatal("sibling inherited unsupported Low", cap)
	}
	if cap := out.ModelCapabilities["gpt-name-is-not-evidence"]; cap.ReasoningKnown || len(cap.ReasoningEfforts) != 0 || len(cap.InputModalities) != 0 {
		t.Fatal("model name invented capabilities", cap)
	}
	if cap := out.ModelCapabilities["explicit-no-effort"]; !cap.ReasoningKnown || cap.ReasoningEfforts == nil || len(cap.ReasoningEfforts) != 0 {
		t.Fatal(cap)
	}
}

func TestKnownSessionModelNeverGuessesOrCreatesAnUnknownThread(t *testing.T) {
	a := &codexAgent{threads: map[string]*codexThread{"known": {info: protocol.SessionInfo{Model: "exact-model"}}}}
	if a.KnownSessionModel("known") != "exact-model" || a.KnownSessionModel("unknown") != "" || len(a.threads) != 1 {
		t.Fatal("read-only model hint altered a thread")
	}
}
