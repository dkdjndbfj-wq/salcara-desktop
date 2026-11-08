package launcher

import (
	"context"
	"errors"
	"testing"
)

func TestSwitchAuthorizationRunsAfterProfileSavedAndBeforeStart(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex")
	fixtureFile(t, root, "auth.json", `{"auth_mode":"chatgpt"}`)
	ready := false
	s.Start = func(context.Context, Plan) (int, error) {
		if !ready {
			t.Fatal("started before authorizing")
		}
		return 1, nil
	}
	_, err := s.SwitchAuthorized(context.Background(), account("codex", "fixture", "synthetic-private"), "codex", t.TempDir(), nil, func() error { ready = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSwitchAuthorizationFailureRestoresBeforeAnyStart(t *testing.T) {
	s, root, a, starts, _ := fixtureClaude3P(t, true)
	before := fixtureTree(t, root)
	_, err := s.SwitchAuthorized(context.Background(), a, nil, true, func() error { return errors.New("synthetic authorization save failure") })
	if err == nil || *starts != 0 {
		t.Fatal("started without authorization")
	}
	after := fixtureTree(t, root)
	for path, want := range before {
		if after[path] != want {
			t.Fatal("authorization failure did not restore original profile")
		}
	}
}
