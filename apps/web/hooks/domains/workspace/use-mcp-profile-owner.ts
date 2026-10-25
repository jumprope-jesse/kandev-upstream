import { useCallback, useEffect, useState } from "react";
import { getMCPProfileOwner } from "@/lib/api/domains/mcp-api";
import type { MCPProfileOwner } from "@/lib/types/http-mcp";

type OwnerState = { key: string; owner: MCPProfileOwner | null; loading: boolean; error: unknown };

export function useMCPProfileOwner(
  workspaceId: string | null,
  agentKey: string,
  profileId: string,
) {
  const [revision, setRevision] = useState(0);
  const key = JSON.stringify([workspaceId, agentKey, profileId, revision]);
  const ready = Boolean(workspaceId && agentKey && profileId);
  const [state, setState] = useState<OwnerState>({ key, owner: null, loading: ready, error: null });
  const current = state.key === key ? state : { key, owner: null, loading: ready, error: null };
  useEffect(() => {
    if (!workspaceId || !agentKey || !profileId) return;
    let cancelled = false;
    getMCPProfileOwner(workspaceId, profileId)
      .then((owner) => {
        if (cancelled) return;
        if (
          owner.id !== profileId ||
          (owner.workspace_id !== "" && owner.workspace_id !== workspaceId) ||
          (owner.agent_id !== agentKey && owner.agent_name !== agentKey)
        ) {
          // i18n-exempt: internal identity guard; the presenter uses localized unavailable copy.
          throw new Error("MCP profile owner context changed");
        }
        setState({ key, owner, loading: false, error: null });
      })
      .catch((error) => {
        if (!cancelled) setState({ key, owner: null, loading: false, error });
      });
    return () => {
      cancelled = true;
    };
  }, [key, workspaceId, agentKey, profileId]);
  const reload = useCallback(() => setRevision((value) => value + 1), []);
  return { ...current, reload };
}
