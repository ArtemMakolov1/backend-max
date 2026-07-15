-- MAX currently rejects notify=false for channel publications. Keep legacy
-- drafts consistent with the supported channel-delivery mode.
UPDATE posts SET notify = TRUE WHERE notify = FALSE;
