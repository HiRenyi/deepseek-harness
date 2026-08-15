// Profile: test (committed source; build script overwrites with prod for prod builds).
// Structure MUST match scripts/profile.ps1 Write-ExtensionProfile output.
export const PROFILE = {
  NM_HOST_NAME: "com.browser.mcp",
  MCP_PORT: 58080,
  HEALTH_URL: "http://127.0.0.1:58080/health",
  EXT_INFO_URL: "http://127.0.0.1:58080/api/extension-info",
  MCP_SERVER_NAME: "browser-mcp",
  TAB_GROUP_TITLE: "🤖 Browser MCP",
} as const;
export type Profile = typeof PROFILE;
