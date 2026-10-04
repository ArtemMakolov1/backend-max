-- Old shared links must not restore a removed member's access. The cutoff is
-- per recipient: it preserves those links for other users and permits owners
-- to deliberately invite the removed person again with a newly issued link.
CREATE TABLE workspace_member_invitation_cutoffs (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    invalid_before TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id,user_id)
);

-- Preserve explicit access decisions made before this deployment too.
INSERT INTO workspace_member_invitation_cutoffs(workspace_id,user_id,invalid_before)
SELECT event.workspace_id,event.entity_id,MAX(event.created_at)
FROM audit_events event
JOIN users recipient ON recipient.id=event.entity_id
WHERE event.entity_type='user' AND event.action IN ('member.removed','member.role_updated')
GROUP BY event.workspace_id,event.entity_id;

UPDATE workspace_invitations invitation SET status='revoked',revoked_at=cutoff.invalid_before
FROM workspace_member_invitation_cutoffs cutoff
JOIN users recipient ON recipient.id=cutoff.user_id
WHERE invitation.workspace_id=cutoff.workspace_id AND invitation.status='pending'
  AND EXISTS(SELECT 1 FROM workspaces workspace WHERE workspace.id=invitation.workspace_id AND workspace.archived_at IS NULL)
  AND invitation.created_at<=cutoff.invalid_before
  AND (invitation.target_user_id=cutoff.user_id OR
       (invitation.email<>'' AND lower(invitation.email)=lower(recipient.email)));

CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_member_invitation_cutoffs
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
