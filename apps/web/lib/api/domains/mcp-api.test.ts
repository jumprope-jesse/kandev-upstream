import { beforeEach, describe, expect, it, vi } from "vitest";
import { fetchJson } from "../client";
import * as mcpAPI from "./mcp-api";

vi.mock("../client", () => ({ fetchJson: vi.fn() }));
const fetchMock = vi.mocked(fetchJson);

describe("catalog usage transport", () => {
  beforeEach(() => vi.resetAllMocks());
  // @covers AC-AGENTS-MCP-MANAGEMENT-001.4
  it("preserves an empty continuation page and encodes workspace, definition and cursor", async () => {
    const page = { workspace_id: "w/1", definition_id: "d/1", items: [], next_cursor: "next" };
    fetchMock.mockResolvedValue(page);
    const list = mcpAPI.listMCPUsage;
    const signal = new AbortController().signal;
    expect(await list("w/1", "d/1", "opaque+position", { init: { signal } })).toEqual(page);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/workspaces/w%2F1/mcp-servers/d%2F1/usage?cursor=opaque%2Bposition",
      expect.objectContaining({ cache: "no-store", init: { signal } }),
    );
  });
  it("propagates a failed read instead of returning an empty success", async () => {
    const failure = new Error("read failed");
    fetchMock.mockRejectedValue(failure);
    const list = mcpAPI.listMCPUsage;
    await expect(list("workspace", "definition")).rejects.toBe(failure);
  });
});
