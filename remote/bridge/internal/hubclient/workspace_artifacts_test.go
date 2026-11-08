package hubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func artifactCommand(action, root string, fields map[string]any) map[string]any {
	cmd := map[string]any{"type": "workspace.artifacts", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "action": action}
	for key, value := range fields {
		cmd[key] = value
	}
	return cmd
}
func TestWorkspaceArtifactsDiscoverOnlyCurrentSessionExplicitFiles(t *testing.T) {
	c, a, root := workspaceFixture(t)
	for _, name := range []string{"output/report.pdf", "output/chart.svg", "output/notes.md", "not-mentioned.txt", "foreign.txt", "child.txt", "stdout.txt", "auth.json"} {
		workspaceWrite(t, root, name, []byte("synthetic"))
	}
	a.openEvents = []protocol.Event{
		{SessionKey: workspaceTestKey, Type: "message", Role: "assistant", Text: "[报告](output/report.pdf) [重复](output/report.pdf) [notes](<output/notes.md>) [directory](output/) [网络](https://example.com/file.pdf) [secret](auth.json)"},
		{SessionKey: workspaceTestKey, Type: "tool", Kind: "file_change", Detail: "output/chart.svg\n../outside.txt"},
		{SessionKey: "codex:other", Type: "message", Role: "assistant", Text: "[foreign](foreign.txt)"},
		{SessionKey: workspaceTestKey, ParentID: "child-tool", Type: "message", Role: "assistant", Text: "[child](child.txt)"},
		{SessionKey: workspaceTestKey, Type: "tool", Kind: "command", Output: "[stdout](stdout.txt)", Detail: "not-mentioned.txt"},
	}
	raw, err := c.Dispatch(context.Background(), artifactCommand("list", root, nil))
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(artifactPage)
	if len(page.Artifacts) != 3 {
		t.Fatalf("discovery leaked unrelated files: %#v", page)
	}
	for _, item := range page.Artifacts {
		if !strings.HasPrefix(item.Path, "output/") || item.ID == "" || len(item.Revision) != 64 {
			t.Fatalf("invalid artifact: %#v", item)
		}
	}
	if len(a.started)+len(a.sent) != 0 {
		t.Fatal("artifact discovery executed a model")
	}
}
func TestWorkspaceArtifactsBinaryChunksAndRevisionConflict(t *testing.T) {
	c, a, root := workspaceFixture(t)
	data := bytes.Repeat([]byte{0, 1, 2, 255}, 40000)
	workspaceWrite(t, root, "result.pdf", data)
	var got []byte
	revision := ""
	offset := 0
	for offset < len(data) {
		fields := map[string]any{"path": "result.pdf", "offset": offset, "length": artifactChunkBytes}
		if revision != "" {
			fields["expectedRevision"] = revision
		}
		raw, err := c.Dispatch(context.Background(), artifactCommand("read", root, fields))
		if err != nil {
			t.Fatal(err)
		}
		chunk := raw.(artifactChunk)
		piece, err := base64.StdEncoding.DecodeString(chunk.Content)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Offset != offset || chunk.NextOffset != offset+len(piece) || chunk.Artifact.Size != int64(len(data)) || len(piece) > artifactChunkBytes {
			t.Fatal("bad chunk boundary")
		}
		revision = chunk.Artifact.Revision
		offset = chunk.NextOffset
		got = append(got, piece...)
	}
	if !bytes.Equal(got, data) || revision != workspaceFileRevision(data) {
		t.Fatal("binary bytes changed")
	}
	workspaceWrite(t, root, "result.pdf", []byte("changed"))
	if _, err := c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "result.pdf", "offset": 1, "expectedRevision": revision})); !errors.Is(err, errWorkspaceFileConflict) {
		t.Fatalf("revision conflict not detected: %v", err)
	}
	if len(a.opened)+len(a.started)+len(a.sent) != 0 {
		t.Fatal("explicit read used model/history instead of metadata")
	}
}

func TestWorkspaceArtifactsTwentyMiBBoundary(t *testing.T) {
	c, _, root := workspaceFixture(t)
	data := bytes.Repeat([]byte{0x35}, artifactMaxBytes)
	workspaceWrite(t, root, "large.pdf", data)
	revision := workspaceFileRevision(data)
	for _, offset := range []int{0, artifactMaxBytes - artifactChunkBytes} {
		raw, err := c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "large.pdf", "offset": offset, "expectedRevision": revision}))
		if err != nil {
			t.Fatal(err)
		}
		chunk := raw.(artifactChunk)
		piece, err := base64.StdEncoding.DecodeString(chunk.Content)
		if err != nil || len(piece) != artifactChunkBytes || chunk.Artifact.Size != artifactMaxBytes || chunk.NextOffset != offset+artifactChunkBytes || !bytes.Equal(piece, data[offset:offset+artifactChunkBytes]) {
			t.Fatal("20 MiB boundary lost bytes")
		}
	}
}
func TestWorkspaceArtifactsSaveTextCASAndReadOnlyOffice(t *testing.T) {
	for _, name := range []string{"page.html", "page.htm", "chart.svg", "notes.md", "notes.txt", "data.json"} {
		t.Run(name, func(t *testing.T) {
			c, _, root := workspaceFixture(t)
			before := "original"
			after := "updated 中文\n"
			workspaceWrite(t, root, name, []byte(before))
			cmd := artifactCommand("save", root, map[string]any{"path": name, "text": after, "expectedRevision": workspaceFileRevision([]byte(before))})
			raw, err := c.Dispatch(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			payload := raw.(map[string]any)
			desc := payload["artifact"].(artifactDescriptor)
			if desc.Size != int64(len([]byte(after))) || desc.Revision != workspaceFileRevision([]byte(after)) {
				t.Fatal("save not confirmed")
			}
			if _, err = c.Dispatch(context.Background(), cmd); !errors.Is(err, errWorkspaceFileConflict) {
				t.Fatalf("stale save accepted: %v", err)
			}
			actual, _ := os.ReadFile(filepath.Join(root, name))
			if string(actual) != after {
				t.Fatal("stale save overwrote disk")
			}
		})
	}
	c, _, root := workspaceFixture(t)
	for _, name := range []string{"report.docx", "sheet.xlsx", "slides.pptx", "image.png", "source.ts"} {
		workspaceWrite(t, root, name, []byte("original"))
		if _, err := c.Dispatch(context.Background(), artifactCommand("save", root, map[string]any{"path": name, "text": "no", "expectedRevision": workspaceFileRevision([]byte("original"))})); err == nil {
			t.Fatalf("editable binary/code %s", name)
		}
	}
}
func TestWorkspaceArtifactsRejectEscapesStaleProjectAndMalformedCommands(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "safe.txt", []byte("safe"))
	outside := t.TempDir()
	workspaceWrite(t, outside, "outside.txt", []byte("secret"))
	for _, path := range []string{"../outside.txt", filepath.Join(outside, "outside.txt"), "nested/../../outside.txt", ".env", "auth.json", "node_modules/package.json", "nested\\file.txt", "file.txt:stream"} {
		if _, err := c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": path})); err == nil {
			t.Fatalf("allowed invalid path %q", path)
		}
	}
	for _, fields := range []map[string]any{{"path": "safe.txt", "expectedCwd": outside}, {"path": "safe.txt", "sessionKey": "codex:other"}, {"path": "safe.txt", "controlSurface": "desktop"}, {"path": "safe.txt", "offset": 1}, {"path": "safe.txt", "length": 0}, {"path": "safe.txt", "offset": 0.2}, {"path": "safe.txt", "length": artifactChunkBytes + 1}, {"path": "safe.txt", "unexpected": "value"}} {
		if _, err := c.Dispatch(context.Background(), artifactCommand("read", root, fields)); err == nil {
			t.Fatalf("allowed invalid command %#v", fields)
		}
	}
	_ = c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil })
	if _, err := c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "safe.txt"})); err == nil {
		t.Fatal("removed project still readable")
	}
}
func TestWorkspaceArtifactsRejectSymlink(t *testing.T) {
	c, _, root := workspaceFixture(t)
	outside := t.TempDir()
	workspaceWrite(t, outside, "secret.txt", []byte("secret"))
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "linked.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "linked.txt"})); err == nil {
		t.Fatal("symlink disclosed outside bytes")
	}
}
func TestWorkspaceArtifactsBoundsAndEmptyFile(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "empty.txt", nil)
	raw, err := c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "empty.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if chunk := raw.(artifactChunk); chunk.Content != "" || chunk.NextOffset != 0 || chunk.Artifact.Revision != workspaceFileRevision(nil) {
		t.Fatal("empty file rejected")
	}
	workspaceWrite(t, root, "large.pdf", bytes.Repeat([]byte{0}, artifactMaxBytes+1))
	if _, err = c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "large.pdf"})); err == nil {
		t.Fatal("oversize artifact read")
	}
	workspaceWrite(t, root, "bad.txt", []byte{0xff, 0xfe})
	if _, err = c.Dispatch(context.Background(), artifactCommand("read", root, map[string]any{"path": "bad.txt"})); err == nil {
		t.Fatal("invalid UTF8 accepted")
	}
	workspaceWrite(t, root, "bounded.txt", []byte("original"))
	if _, err = c.Dispatch(context.Background(), artifactCommand("save", root, map[string]any{"path": "bounded.txt", "expectedRevision": workspaceFileRevision([]byte("original"))})); err == nil {
		t.Fatal("missing text silently cleared an existing artifact")
	}
	unchanged, _ := os.ReadFile(filepath.Join(root, "bounded.txt"))
	if string(unchanged) != "original" {
		t.Fatal("malformed save changed file")
	}
	if _, err = c.Dispatch(context.Background(), artifactCommand("save", root, map[string]any{"path": "bounded.txt", "text": strings.Repeat("x", workspaceFileBytes+1), "expectedRevision": workspaceFileRevision([]byte("original"))})); err == nil {
		t.Fatal("oversize save accepted")
	}
}
func TestWorkspaceArtifactsListPaginationAndCursorScope(t *testing.T) {
	c, a, root := workspaceFixture(t)
	var links strings.Builder
	for index := 0; index < 55; index++ {
		name := fmt.Sprintf("file-%02d.txt", index)
		workspaceWrite(t, root, name, []byte(name))
		fmt.Fprintf(&links, "[file](%s) ", name)
	}
	a.openEvents = []protocol.Event{{Type: "message", Role: "assistant", Text: links.String()}}
	raw, err := c.Dispatch(context.Background(), artifactCommand("list", root, nil))
	if err != nil {
		t.Fatal(err)
	}
	first := raw.(artifactPage)
	if len(first.Artifacts) != 50 || first.NextCursor == "" {
		t.Fatal("first page not bounded")
	}
	raw, err = c.Dispatch(context.Background(), artifactCommand("list", root, map[string]any{"cursor": first.NextCursor}))
	if err != nil {
		t.Fatal(err)
	}
	second := raw.(artifactPage)
	if len(second.Artifacts) != 5 || second.NextCursor != "" {
		t.Fatal("missing continuation")
	}
	otherKey := "codex:22222222-2222-2222-2222-222222222222"
	a.sessions = append(a.sessions, protocol.SessionInfo{SessionKey: otherKey, Cwd: root, ControlSurface: "cli"})
	if _, err = c.Dispatch(context.Background(), artifactCommand("list", root, map[string]any{"cursor": first.NextCursor, "sessionKey": otherKey})); err == nil {
		t.Fatal("cursor reused across sessions")
	}
	a.openEvents = []protocol.Event{{Type: "message", Role: "assistant", Text: "[changed](file-00.txt)"}}
	if _, err = c.Dispatch(context.Background(), artifactCommand("list", root, map[string]any{"cursor": first.NextCursor})); err == nil {
		t.Fatal("changed history offset reused")
	}
}

type artifactRevokingAgent struct {
	*fakeAgent
	afterRead func()
}

func (a *artifactRevokingAgent) Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	info, events, err := a.fakeAgent.Open(ctx, id)
	a.afterRead()
	return info, events, err
}
func TestWorkspaceArtifactsRevalidateAfterHistoryRead(t *testing.T) {
	c, a, root := workspaceFixture(t)
	workspaceWrite(t, root, "result.txt", []byte("private"))
	a.openEvents = []protocol.Event{{Type: "message", Role: "assistant", Text: "[result](result.txt)"}}
	revoker := &artifactRevokingAgent{a, func() { _ = c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }) }}
	c.SetManager(&fakeManager{list: []agents.Agent{revoker}})
	if result, err := c.Dispatch(context.Background(), artifactCommand("list", root, nil)); err == nil || result != nil {
		t.Fatal("buffered result escaped after project revocation")
	}
}
