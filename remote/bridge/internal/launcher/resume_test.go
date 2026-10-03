package launcher

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestResumeExactOriginalSessionDoesNotRewriteData(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) {
			s, root, _ := switchFixture(t, kind, kind)
			before := map[string]string{}
			for name, text := range map[string]string{"config.toml": "model = \"old-model\"\n", "auth.json": "original-auth", "settings.json": "{}", "sessions/original.jsonl": "original-dialogue", "state_5.sqlite": "database", "projects/original.jsonl": "claude-original-dialogue"} {
				path := fixtureFile(t, root, name, text)
				before[path] = digest([]byte(text))
			}
			cwd := t.TempDir()
			project := fixtureFile(t, cwd, "existing-work.js", "finished work")
			before[project] = digest([]byte("finished work"))
			calls := 0
			s.Start = func(_ context.Context, p Plan) (int, error) {
				calls++
				want := []string{"resume", id, "--model", "coding-model"}
				if kind == "claude" {
					want = []string{"--resume", id, "--model", "coding-model"}
				}
				if !reflect.DeepEqual(p.Args, want) || !p.Shared || p.ProfileDir != root || p.Workspace != cwd || strings.Contains(strings.Join(p.Args, " "), "private-resume-key") {
					t.Fatal("not an exact original session resume")
				}
				assertRetained(t, before)
				return 101, nil
			}
			if _, err := s.Resume(context.Background(), account(kind, "one", "private-resume-key"), kind, id, cwd, nil); err != nil {
				t.Fatal(err)
			}
			assertRetained(t, before)
			for _, bad := range []string{"../escape", "--new-session", "", id + " & echo test"} {
				if _, err := s.Resume(context.Background(), account(kind, "one", "private-resume-key"), kind, bad, cwd, nil); err == nil {
					t.Fatal("invalid session ID accepted")
				}
			}
			if _, err := s.Resume(context.Background(), account(kind, "one", "private-resume-key"), kind, id, cwd+"-missing", nil); err == nil {
				t.Fatal("missing original project silently defaulted")
			}
			if calls != 1 {
				t.Fatal("invalid resume launched a new process")
			}
		})
	}
}
