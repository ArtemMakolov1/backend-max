package api

import (
	"net/http"

	"maxpilot/backend/internal/app"
)

func (s *Server) getWorkspaceMAXVideoPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	attachmentID, err := parseAttachmentID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if r.URL.RawQuery != "" {
		s.writeError(w, validationError("MAX video preview does not accept query parameters"))
		return
	}
	preview, err := s.app.GetMAXVideoPreviewForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID, attachmentID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, preview)
}
