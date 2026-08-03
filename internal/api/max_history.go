package api

import (
	"net/http"
	"time"

	"maxpilot/backend/internal/app"
)

// importWorkspaceChannelHistory advances one durable MAX history-import batch.
// The browser repeats this endpoint while has_more is true; every batch is
// independently idempotent, so closing the page or retrying after a timeout
// cannot create duplicate posts.
func (s *Server) importWorkspaceChannelHistory(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	channelID, err := parsePositivePathID(r, "channel_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	result, err := s.app.ImportMAXChannelHistoryPage(
		ctx, access.UserID, access.WorkspaceID, channelID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, result)
}
