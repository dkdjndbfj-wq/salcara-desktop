package desktoplink

import "testing"

func TestCodexURLOnlyCanonicalExistingThread(t *testing.T) {
	id := "0199aaa1-1234-5678-9abc-0123456789ab"
	url, err := CodexURL("codex:" + id)
	if err != nil || url != "codex://threads/"+id {
		t.Fatalf("url=%q err=%v", url, err)
	}
	for _, key := range []string{"", "codex:new", "codex:" + id + "?prompt=send", "codex:" + id + "/new", "codex:" + id + "\n", "codex:" + id + "&x=1", "codex:" + id + ";exit", "codex:" + id + "%3f", "claude:" + id, "codex:../settings", "codex:" + "0199AAA1-1234-5678-9abc-0123456789ab"} {
		if url, err := CodexURL(key); err == nil || url != "" {
			t.Fatalf("unsafe key accepted: %q, %q", key, url)
		}
	}
}

func TestClaudeDesktopLinksImportOnlyPhoneSessions(t *testing.T) {
	key := "claude:0199aaa1-1234-4678-9abc-000000000001"
	if u, err := ClaudeURL(key, true); err != nil || u != "claude://resume?session=0199aaa1-1234-4678-9abc-000000000001" {
		t.Fatalf("%s %v", u, err)
	}
	if u, err := ClaudeURL(key, false); err != nil || u != "claude://" {
		t.Fatalf("%s %v", u, err)
	}
	if _, err := ClaudeURL("claude:../../x", true); err == nil {
		t.Fatal("non-canonical key accepted")
	}
}
