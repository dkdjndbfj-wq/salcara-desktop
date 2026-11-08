package agents

import (
	"context"
	"fmt"
	"path/filepath"

	"salcara/bridge/internal/protocol"
)

// Only a fixed stage is retained for local admission diagnostics. Native RPC
// errors can contain conversation text and must not become metadata errors.
func codexMetadataUnavailable(stage string) error {
	return fmt.Errorf("%w (%s)", errSessionMetadata, stage)
}

// SessionMetadata uses only the native metadata request. Even on an older
// host, failure never falls back to includeTurns:true or the first 100 chats.
func (a *codexAgent) SessionMetadata(ctx context.Context, id string) (protocol.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	if !validChildID(id) {
		return protocol.SessionInfo{}, codexMetadataUnavailable("invalid_session_id")
	}
	connection, err := a.ensureRead(ctx)
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return protocol.SessionInfo{}, canceled
		}
		return protocol.SessionInfo{}, codexMetadataUnavailable("runtime_initialization")
	}
	if err = ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	// Deliberately omit message, preview, path and turns fields: this lookup
	// cannot copy conversation content or allocate a second history snapshot.
	var reply struct {
		Thread struct {
			ID        string         `json:"id"`
			Cwd       string         `json:"cwd"`
			Ephemeral bool           `json:"ephemeral"`
			UpdatedAt int64          `json:"updatedAt"`
			Status    cxThreadStatus `json:"status"`
		} `json:"thread"`
	}
	if err = connection.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &reply); err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return protocol.SessionInfo{}, canceled
		}
		// A host error may contain preview/transcript fragments. The exact
		// metadata surface never exports raw native RPC errors to the phone.
		return protocol.SessionInfo{}, codexMetadataUnavailable("native_metadata_read")
	}
	if err = ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	a.mu.Lock()
	current := !a.closed && a.conn == connection
	a.mu.Unlock()
	if !current {
		return protocol.SessionInfo{}, codexMetadataUnavailable("runtime_connection_changed")
	}
	if reply.Thread.ID != id {
		return protocol.SessionInfo{}, codexMetadataUnavailable("session_identity_mismatch")
	}
	if reply.Thread.Ephemeral {
		return protocol.SessionInfo{}, codexMetadataUnavailable("ephemeral_session")
	}
	if !sessionMetadataCwd(reply.Thread.Cwd) {
		return protocol.SessionInfo{}, codexMetadataUnavailable("invalid_working_directory")
	}
	status := "failed" // Unknown future status never implies readiness.
	switch reply.Thread.Status.Type {
	case "idle", "notLoaded":
		status = "idle"
	case "active":
		status = "running"
		for _, flag := range reply.Thread.Status.ActiveFlags {
			if flag == "waitingOnApproval" {
				status = "waiting_approval"
			}
		}
	}
	updated := reply.Thread.UpdatedAt
	if updated > 0 && updated < 100000000000 {
		updated *= 1000
	}
	return protocol.SessionInfo{SessionKey: "codex:" + id, Tool: "codex", Client: "Codex", ControlSurface: "cli",
		Cwd: filepath.Clean(reply.Thread.Cwd), UpdatedAt: updated, Status: status, Controllable: false}, nil
}
