package console

import (
	"encoding/json"
	"net/http"
	"sort"

	"salcara/bridge/internal/protocol"
)

type activitySession struct {
	SessionKey string `json:"sessionKey"`
	Status     string `json:"status"`
}

// publishActivity serializes state changes with their SSE delivery. A floating
// window can reconcile a late snapshot without reviving a finished task or a
// resolved approval. This projection observes events only; it must never start
// an agent, list private histories, or contact a provider just to draw the orb.
func (s *Server) publishActivity(ev protocol.Event) bool {
	if ev.Type != "session.updated" && ev.Type != "approval.request" && ev.Type != "approval.resolved" {
		return false
	}
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	s.activityRevision++
	switch ev.Type {
	case "session.updated":
		if info := ev.Session; info != nil && info.SessionKey != "" {
			if info.Status == "running" || info.Status == "waiting_approval" {
				if s.activeSessions == nil {
					s.activeSessions = make(map[string]string)
				}
				s.activeSessions[info.SessionKey] = info.Status
			} else {
				delete(s.activeSessions, info.SessionKey)
			}
		}
	case "approval.request":
		if ev.ApprovalID != "" {
			s.pending[ev.ApprovalID] = ev
		}
	case "approval.resolved":
		delete(s.pending, ev.ApprovalID)
	}
	b, _ := json.Marshal(struct {
		protocol.Event
		ActivityRevision uint64 `json:"activityRevision"`
	}{Event: ev, ActivityRevision: s.activityRevision})
	s.publish(sseMsg{event: "event", data: b})
	return true
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	s.pendMu.Lock()
	revision := s.activityRevision
	sessions := make([]activitySession, 0, len(s.activeSessions))
	for key, status := range s.activeSessions {
		sessions = append(sessions, activitySession{SessionKey: key, Status: status})
	}
	approvalIDs := make([]string, 0, len(s.pending))
	for id := range s.pending {
		approvalIDs = append(approvalIDs, id)
	}
	s.pendMu.Unlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].SessionKey < sessions[j].SessionKey })
	sort.Strings(approvalIDs)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"revision": revision, "sessions": sessions, "approvalIds": approvalIDs})
}
