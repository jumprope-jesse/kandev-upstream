import { test } from "../../fixtures/test-base";
import {
  checkMCPUsageNavigation,
  checkGlobalProfileWorkspaceGuard,
  checkMCPUsagePaging,
} from "../../helpers/mcp-management";

test("navigates named MCP usage to its existing repository editor", async ({
  testPage,
  apiClient,
  seedData,
}) => {
  await checkMCPUsageNavigation(testPage, apiClient, seedData, false);
});

test("guards global profile workspace changes", async ({ testPage, apiClient, seedData }) => {
  await checkGlobalProfileWorkspaceGuard(testPage, apiClient, seedData, false);
});

test("keeps partial usage, stale reads and unavailable owners distinct", async ({
  testPage,
  apiClient,
  seedData,
}) => {
  await checkMCPUsagePaging(testPage, apiClient, seedData);
});
